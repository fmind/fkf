//go:build !unix

package core

import "os/exec"

// setProcessGroup is a no-op where process groups are not the cancellation primitive. fkf ships
// for Linux and macOS; this keeps the package building elsewhere without pretending the
// pipeline-reaping guarantee holds there. cmd.WaitDelay still bounds the wait.
func setProcessGroup(*exec.Cmd) {}
