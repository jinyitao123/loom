package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// maxCapture bounds the output a command may produce before older output is
// dropped from memory; the dropped part still reaches the full-output file.
const maxCapture = 8 << 20

func init() {
	_, _, name := shellCommand()
	register("bash",
		fmt.Sprintf("Execute a %s command in the current working directory. Returns stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.", name, MaxLines, MaxBytes/1024),
		false,
		`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (optional, no default timeout)"}},"required":["command"]}`,
		runBash)
}

// shellCommand picks the shell: bash when available, otherwise the platform shell.
func shellCommand() (path string, args []string, name string) {
	for _, candidate := range bashCandidates() {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, []string{"-c"}, "bash"
		}
	}
	return fallbackShell()
}

// output collects combined stdout and stderr, keeping the newest maxCapture
// bytes in memory and every byte in a spill file once output grows large.
type output struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	total int
	spill *os.File
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += len(p)
	if o.spill == nil && o.total > MaxBytes {
		if f, err := os.CreateTemp("", "loom-bash-*.log"); err == nil {
			o.spill = f
			_, _ = f.Write(o.buf.Bytes())
		}
	}
	if o.spill != nil {
		_, _ = o.spill.Write(p)
	}
	o.buf.Write(p)
	if o.buf.Len() > maxCapture {
		o.buf.Next(o.buf.Len() - maxCapture)
	}
	return len(p), nil
}

func runBash(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Command == "" {
		return "", errors.New("command is required")
	}
	if args.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(args.Timeout*float64(time.Second)))
		defer cancel()
	}
	shell, shellArgs, _ := shellCommand()
	cmd := exec.Command(shell, append(shellArgs, args.Command)...)
	cmd.Dir = t.cwd
	out := &output{}
	cmd.Stdout, cmd.Stderr = out, out
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("Could not start the command: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		killProcess(cmd)
		waitErr = <-done
	}

	out.mu.Lock()
	text, truncated := truncateTail(out.buf.String())
	spill := out.spill
	out.mu.Unlock()
	if spill != nil {
		_ = spill.Close()
		truncated = true
	}
	if truncated && spill != nil {
		text += fmt.Sprintf("\n\n[Output truncated. Full output: %s]", spill.Name())
	}
	if text == "" {
		text = "(no output)"
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("%s\n\nCommand timed out after %g seconds", text, args.Timeout)
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s\n\nCommand aborted", text)
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return "", fmt.Errorf("%s\n\nCommand exited with code %d", text, exitErr.ExitCode())
	}
	if waitErr != nil {
		return "", fmt.Errorf("%s\n\n%v", text, waitErr)
	}
	return text, nil
}
