//go:build unix

package core

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts a declared command in its own process group and cancels by signalling
// the group rather than the single child. Every `run:` line is `bash -c <script>`, so the thing
// worth killing is the pipeline, not the shell that forked it: signalling only the shell left
// `gh`, `jq`, and `xargs` running, still holding the stdout pipe, and `sync` waiting on them
// long past the timeout it reported.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid means "the group whose leader is this pid". Setpgid above made the
		// child that leader, so this reaches every stage of the pipeline it started.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// The group is already gone between the deadline firing and the signal; fall
			// back to the single process so cancellation still reports honestly.
			return cmd.Process.Kill()
		}
		return nil
	}
}
