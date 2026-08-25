//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

func processPeakRSSBytes(state *os.ProcessState) (uint64, error) {
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0, fmt.Errorf("process usage has type %T, want *syscall.Rusage", state.SysUsage())
	}
	if usage.Maxrss < 0 {
		return 0, fmt.Errorf("process peak RSS is negative: %d", usage.Maxrss)
	}
	// Linux reports ru_maxrss in KiB; the benchmark's public metric is bytes on every OS.
	return uint64(usage.Maxrss) * 1024, nil
}
