//go:build !windows

package localtools

import (
	"os/exec"
	"strings"
	"syscall"
)

// detectShell picks bash, or /bin/sh when there is none. LOOM_SHELL may name
// another executable.
func detectShell(override string) Shell {
	if override = strings.TrimSpace(override); override != "" {
		if path := lookShell([]string{override}, nil); path != "" {
			return shellFor(path)
		}
	}
	if path := lookShell([]string{"/bin/bash", "bash"}, nil); path != "" {
		return Shell{Name: "bash", Path: path}
	}
	return Shell{Name: "sh", Path: "/bin/sh"}
}

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
