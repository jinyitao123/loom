package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// maxMessage bounds one JSON-RPC message read from a server.
const maxMessage = 64 << 20

// stdioTransport runs a server as a child process speaking newline-delimited
// JSON-RPC on stdin and stdout; stderr is discarded.
type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex
	done  chan struct{}
}

func openStdio(command string, args []string, env map[string]string, dir string, s *session) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		reader := bufio.NewScanner(stdout)
		reader.Buffer(make([]byte, 0, 64<<10), maxMessage)
		for reader.Scan() {
			if line := bytes.TrimSpace(reader.Bytes()); len(line) > 0 {
				s.receive(append([]byte(nil), line...))
			}
		}
		err := cmd.Wait()
		if err == nil {
			err = errors.New("server exited")
		}
		s.fail(fmt.Errorf("MCP server %s stopped: %v", s.name, err))
		close(t.done)
	}()
	return t, nil
}

func (t *stdioTransport) send(_ context.Context, message []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.stdin.Write(append(message, '\n'))
	return err
}

func (t *stdioTransport) close() error {
	_ = t.stdin.Close()
	select {
	case <-t.done:
	default:
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		<-t.done
	}
	return nil
}

// httpTransport speaks streamable HTTP: every message is a POST whose answer
// is either one JSON message or an event stream of messages.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	session *session

	mu        sync.Mutex
	sessionID string
	protocol  string
}

func openHTTP(url string, headers map[string]string, s *session) *httpTransport {
	return &httpTransport{url: url, headers: headers, client: &http.Client{}, session: s}
}

func (t *httpTransport) send(ctx context.Context, message []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	if t.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocol)
	}
	t.mu.Unlock()
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.protocol = ProtocolVersion
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return fmt.Errorf("MCP server %s answered %s: %s", t.session.name, resp.Status, strings.TrimSpace(string(body)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The stream may carry server requests that wait for our answer, so
		// it is read in the background while send returns.
		go func() {
			defer resp.Body.Close()
			readEvents(resp.Body, t.session.receive)
		}()
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return err
	}
	if body = bytes.TrimSpace(body); len(body) > 0 {
		if body[0] == '[' {
			var batch []json.RawMessage
			if json.Unmarshal(body, &batch) == nil {
				for _, m := range batch {
					t.session.receive(m)
				}
				return nil
			}
		}
		t.session.receive(body)
	}
	return nil
}

// close ends the HTTP session when the server issued one.
func (t *httpTransport) close() error {
	t.mu.Lock()
	id := t.sessionID
	t.mu.Unlock()
	if id == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Mcp-Session-Id", id)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

// readEvents parses a server-sent event stream, passing each data payload on.
func readEvents(r io.Reader, receive func([]byte)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessage)
	var data []string
	flush := func() {
		if len(data) > 0 {
			receive([]byte(strings.Join(data, "\n")))
			data = nil
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
}
