//go:build windows

package localtools

import (
	"os/exec"
	"strconv"
)

func bashCandidates() []string { return []string{"bash.exe"} }

func fallbackShell() (string, []string, string) { return "cmd.exe", []string{"/C"}, "cmd" }

func prepareProcess(*exec.Cmd) {}

// killProcess stops the command and every process it started.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
