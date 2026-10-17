//go:build !windows

package localtools

import (
	"os/exec"
	"syscall"
)

func bashCandidates() []string { return []string{"/bin/bash", "bash"} }

func fallbackShell() (string, []string, string) { return "/bin/sh", []string{"-c"}, "sh" }

// prepareProcess starts the command in its own process group so that killing
// it also stops the processes it started.
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
