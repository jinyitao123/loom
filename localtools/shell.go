package localtools

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Shell is the program the bash tool runs commands with.
type Shell struct {
	Name string // shown to the model: "bash", "sh", "PowerShell" or "cmd"
	Path string // executable
}

// systemShell is the shell found when the package loaded.
var systemShell = detectShell(os.Getenv("LOOM_SHELL"))

// ShellName names the shell commands run in, for the system prompt.
func ShellName() string { return systemShell.Name }

// shellFor builds a shell from an executable path, naming it by its kind.
func shellFor(path string) Shell {
	base := strings.ToLower(path[strings.LastIndexAny(path, `/\`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "pwsh", "powershell":
		return Shell{Name: "PowerShell", Path: path}
	case "cmd":
		return Shell{Name: "cmd", Path: path}
	case "sh", "dash":
		return Shell{Name: "sh", Path: path}
	default:
		return Shell{Name: "bash", Path: path}
	}
}

// command prepares the process that runs a command line, with a cleanup to
// call when it has finished.
func (s Shell) command(line string) (*exec.Cmd, func(), error) {
	switch s.Name {
	case "PowerShell":
		return powerShellCommand(s.Path, line)
	case "cmd":
		return exec.Command(s.Path, "/D", "/S", "/C", line), func() {}, nil
	default:
		return exec.Command(s.Path, "-c", line), func() {}, nil
	}
}

// powerShellPrelude makes output UTF-8 without a byte-order mark and hides
// progress bars, which would otherwise garble captured output.
const powerShellPrelude = "$ProgressPreference = 'SilentlyContinue'\n" +
	"$__loomUtf8 = New-Object System.Text.UTF8Encoding $false\n" +
	"[Console]::OutputEncoding = $__loomUtf8\n" +
	"$OutputEncoding = $__loomUtf8\n"

// utf8BOM is the UTF-8 byte-order mark.
const utf8BOM = "\xEF\xBB\xBF"

// maxEncodedCommand keeps the command line under Windows' 32,767-character
// limit; longer scripts run from a temporary file.
const maxEncodedCommand = 30000

// powerShellCommand passes the script as -EncodedCommand (UTF-16LE base64),
// so no quoting is involved. A failing last statement exits with 1; "exit N"
// exits with N.
func powerShellCommand(path, line string) (*exec.Cmd, func(), error) {
	script := powerShellPrelude + line
	args := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}
	encoded := encodePowerShell(script)
	if len(encoded) <= maxEncodedCommand {
		return exec.Command(path, append(args, "-EncodedCommand", encoded)...), func() {}, nil
	}
	f, err := os.CreateTemp("", "loom-command-*.ps1")
	if err != nil {
		return nil, nil, err
	}
	// Windows PowerShell reads a script without a byte-order mark as ANSI.
	_, werr := f.WriteString(utf8BOM + script)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		if werr == nil {
			werr = cerr
		}
		return nil, nil, werr
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	return exec.Command(path, append(args, "-File", f.Name())...), cleanup, nil
}

func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// lookShell finds the first candidate that exists, by path or on PATH.
func lookShell(candidates []string, accept func(string) bool) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		path := c
		if !filepath.IsAbs(c) {
			found, err := exec.LookPath(c)
			if err != nil {
				continue
			}
			path = found
		} else if info, err := os.Stat(c); err != nil || info.IsDir() {
			continue
		}
		if accept == nil || accept(path) {
			return path
		}
	}
	return ""
}
