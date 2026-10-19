//go:build windows

package localtools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// detectShell picks the shell on Windows: the LOOM_SHELL override ("bash",
// "powershell", "cmd" or an executable path), else Git Bash, PowerShell 7,
// Windows PowerShell and finally cmd. The bash.exe in System32 and the
// WindowsApps alias start WSL, whose working directory and paths differ, so
// they are not used.
func detectShell(override string) Shell {
	gitBash := func() string {
		var candidates []string
		for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
			if dir := os.Getenv(env); dir != "" {
				candidates = append(candidates, filepath.Join(dir, "Git", "bin", "bash.exe"))
			}
		}
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "Programs", "Git", "bin", "bash.exe"))
		}
		return lookShell(append([]string{"bash.exe"}, candidates...), notWSL)
	}
	powerShell := func() string {
		var candidates []string
		if dir := os.Getenv("ProgramFiles"); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "PowerShell", "7", "pwsh.exe"))
		}
		candidates = append(candidates, "pwsh.exe", "powershell.exe")
		if root := os.Getenv("SystemRoot"); root != "" {
			candidates = append(candidates, filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
		}
		return lookShell(candidates, nil)
	}
	cmd := func() string {
		if path := lookShell([]string{"cmd.exe"}, nil); path != "" {
			return path
		}
		return "cmd.exe"
	}

	switch strings.ToLower(strings.TrimSpace(override)) {
	case "":
	case "bash":
		if path := gitBash(); path != "" {
			return Shell{Name: "bash", Path: path}
		}
	case "powershell", "pwsh":
		if path := powerShell(); path != "" {
			return Shell{Name: "PowerShell", Path: path}
		}
	case "cmd":
		return Shell{Name: "cmd", Path: cmd()}
	default:
		if path := lookShell([]string{override}, nil); path != "" {
			return shellFor(path)
		}
	}
	if path := gitBash(); path != "" {
		return Shell{Name: "bash", Path: path}
	}
	if path := powerShell(); path != "" {
		return Shell{Name: "PowerShell", Path: path}
	}
	return Shell{Name: "cmd", Path: cmd()}
}

// notWSL rejects bash executables that start WSL.
func notWSL(path string) bool {
	lower := strings.ToLower(path)
	return !strings.Contains(lower, `\windows\system32\`) && !strings.Contains(lower, `\windowsapps\`)
}

// createNoWindow keeps a console window from opening for each command when
// the agent runs under a desktop application.
const createNoWindow = 0x08000000

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// killProcess stops the command and every process it started.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
