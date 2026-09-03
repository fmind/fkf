//go:build unix

package core

import (
	"os/exec"
	"syscall"
)

// setProcessGroup cancels a declared command and its descendants together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid addresses the process group created above.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// The group may disappear between the deadline and signal delivery.
			return cmd.Process.Kill()
		}
		return nil
	}
}
