package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestCLIContextSnapshotPersistenceRemainsLockFree(t *testing.T) {
	root := demoBase(t)
	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	if got := invoke(t, "--base", root, "context", "demo"); got.code != ExitSuccess {
		t.Fatalf("context under writer lock = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if entries, err := os.ReadDir(filepath.Join(core.StateDir(), "receipts")); err != nil || len(entries) == 0 {
		t.Fatalf("lock-free context did not persist its receipt namespace: entries=%v err=%v", entries, err)
	}
}

func TestCLIHarnessMutationsTakeTheBaseWriterLockButChecksDoNot(t *testing.T) {
	root := demoBase(t)
	home := os.Getenv("HOME")
	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	if got := invoke(t, "--base", root, "harness", "install", "claude", "--dry-run"); got.code != ExitSuccess || strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
		t.Fatalf("harness dry-run under writer lock = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "harness", "install", "claude", "--check"); got.code != ExitPartial || strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
		t.Fatalf("harness check under writer lock = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	assertCLIWriterBusy(t, invoke(t, "--base", root, "harness", "install", "claude"))
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy harness install changed user config: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "harness", "install", "claude"); got.code != ExitSuccess {
		t.Fatalf("unlocked harness install = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatalf("unlocked harness install did not write config: %v", err)
	}
}

func TestCLIScheduleInstallTakesTheBaseWriterLockButStatusAndDryRunDoNot(t *testing.T) {
	root := demoBase(t)
	installFakeSystemctl(t)
	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	for _, args := range [][]string{
		{"--base", root, "schedule", "status"},
		{"--base", root, "schedule", "install", "--dry-run"},
	} {
		if got := invoke(t, args...); got.code != ExitSuccess || strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
			t.Fatalf("lock-free schedule path %v = exit %d, stdout %q, stderr %q", args, got.code, got.stdout, got.stderr)
		}
	}
	assertCLIWriterBusy(t, invoke(t, "--base", root, "schedule", "install"))
	directory := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy schedule install changed user config: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "schedule", "install"); got.code != ExitSuccess {
		t.Fatalf("unlocked schedule install = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 2 {
		t.Fatalf("unlocked schedule install files = %v, err %v; want service and timer", entries, err)
	}
}

func TestCLIScheduleRemoveTakesTheBaseWriterLockButStatusAndDryRunDoNot(t *testing.T) {
	root := demoBase(t)
	installFakeSystemctl(t)
	if got := invoke(t, "--base", root, "schedule", "install"); got.code != ExitSuccess {
		t.Fatalf("schedule fixture install = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	directory := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	before, err := os.ReadDir(directory)
	if err != nil || len(before) != 2 {
		t.Fatalf("schedule fixture files = %v, err %v; want service and timer", before, err)
	}

	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	for _, args := range [][]string{
		{"--base", root, "schedule", "status"},
		{"--base", root, "schedule", "remove", "--dry-run"},
	} {
		if got := invoke(t, args...); got.code != ExitSuccess || strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
			t.Fatalf("lock-free schedule path %v = exit %d, stdout %q, stderr %q", args, got.code, got.stdout, got.stderr)
		}
	}
	assertCLIWriterBusy(t, invoke(t, "--base", root, "schedule", "remove"))
	after, err := os.ReadDir(directory)
	if err != nil || len(after) != len(before) {
		t.Fatalf("busy schedule remove changed files from %d to %d, err %v", len(before), len(after), err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "schedule", "remove"); got.code != ExitSuccess {
		t.Fatalf("unlocked schedule remove = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if after, err := os.ReadDir(directory); err != nil || len(after) != 0 {
		t.Fatalf("unlocked schedule remove files = %v, err %v; want none", after, err)
	}
}

func assertCLIWriterBusy(t *testing.T, got result) {
	t.Helper()
	if got.code != ExitPartial || !strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
		t.Fatalf("writer = exit %d, stdout %q, stderr %q; want fail-fast busy exit", got.code, got.stdout, got.stderr)
	}
}

func installFakeSystemctl(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	const script = `#!/bin/sh
state=$HOME/.fkf-test-systemctl-active
case " $* " in
  *" enable --now "*) : >"$state" ;;
  *" disable --now "*) rm -f "$state" ;;
  *" is-enabled "*|*" is-active "*) test -f "$state" || exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
}
