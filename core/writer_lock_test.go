package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const writerLockHelperEnv = "FKF_WRITER_LOCK_HELPER"

func TestWriterLockFailsClosedWithoutHomeOrXDGState(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)

	if lock, err := AcquireWriterLock(t.Context(), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "HOME or XDG_STATE_HOME") {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("AcquireWriterLock() error = %v, want a missing state-root refusal", err)
	}
	if _, err := os.Stat(filepath.Join(temporary, "fkf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer lock created the shared temporary fallback: %v", err)
	}
}

func TestWriterLockExcludesAnotherProcessAndReleasesOnExit(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	if os.Getenv(writerLockHelperEnv) == "1" {
		lock, err := AcquireWriterLock(t.Context(), os.Getenv("FKF_WRITER_LOCK_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		fmt.Println("locked")
		// A bare select can make the standalone helper look deadlocked and let the runtime
		// exit before the parent probes the lock, especially on slower hosted runners.
		for {
			time.Sleep(time.Hour)
		}
	}

	// Set the parent before copying its environment so the child receives exactly one
	// XDG_STATE_HOME entry. Appending a duplicate can make libc choose a different lock root.
	t.Setenv("XDG_STATE_HOME", state)
	command := exec.Command(os.Args[0], "-test.run=^TestWriterLockExcludesAnotherProcessAndReleasesOnExit$")
	command.Env = append(os.Environ(),
		writerLockHelperEnv+"=1",
		"FKF_WRITER_LOCK_ROOT="+root,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	if scanner := bufio.NewScanner(stdout); !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatalf("writer-lock helper did not acquire the lock: %q", scanner.Text())
	}

	if _, err := AcquireWriterLock(t.Context(), root); !errors.Is(err, ErrBaseBusy) {
		t.Fatalf("second writer error = %v, want ErrBaseBusy", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper exited successfully")
	}

	lock, err := AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatalf("acquire after writer exit: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release after writer exit: %v", err)
	}
}

func TestWriterLockTreatsSymlinkAliasesAsOneBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "brain")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWriterLock(t.Context(), realRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if _, err := AcquireWriterLock(t.Context(), alias); !errors.Is(err, ErrBaseBusy) {
		t.Fatalf("alias writer error = %v, want ErrBaseBusy", err)
	}
}

func TestWriterLockCanonicalizesMissingTargetsBelowSymlinkedParents(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	realTarget := filepath.Join(realParent, "future", "brain")
	aliasTarget := filepath.Join(aliasParent, "future", "brain")
	lock, err := AcquireWriterLock(t.Context(), realTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if _, err := AcquireWriterLock(t.Context(), aliasTarget); !errors.Is(err, ErrBaseBusy) {
		t.Fatalf("missing alias target error = %v, want ErrBaseBusy", err)
	}
}

func TestWriterLockStateIsOwnerOnlyAndOutsideTheBase(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	directory := filepath.Join(state, "fkf", "locks")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("lock changed base entries from %d to %d", len(before), len(after))
	}

	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != BaseDirMode {
		t.Fatalf("lock directory mode = %o, want %o", got, BaseDirMode)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("lock entries = %d, want one persistent inode", len(entries))
	}
	info, err = entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != BaseFileMode {
		t.Fatalf("lock file mode = %o, want %o", got, BaseFileMode)
	}
}

func TestCanceledWriterLockCreatesNoState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AcquireWriterLock(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(state, "fkf", "locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled lock created state: %v", err)
	}
}
