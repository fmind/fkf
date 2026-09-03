package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestCLISyncIfDueSkipsTheWriterLockWhenNothingIsDue(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	configPath := filepath.Join(root, core.ConfigFileName)
	config := []byte(cliTestContract + `name: scheduled-sync
layers: {events: true}
sources:
  scheduled:
    enabled: true
    layer: events
    run: [printf, "[]"]
    fields: {id: .id, time: .time, title: .id}
`)
	if err := os.WriteFile(configPath, config, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "trust"); got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "sync", "scheduled", "--days", "1", "--no-graph"); got.code != ExitSuccess {
		t.Fatalf("initial sync exited %d: %s%s", got.code, got.stdout, got.stderr)
	}

	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()

	got := invoke(t, "--format", "json", "--base", root, "sync", "scheduled", "--days", "1", "--if-due", "--no-graph")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, `"nothing_due": true`) {
		t.Fatalf("sync --if-due under held lock = exit %d, stdout %q, stderr %q; want lock-free no-op", got.code, got.stdout, got.stderr)
	}
}

func TestCLISyncIfDueRejectsExecutionModeCombinations(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	for _, flag := range []string{"--force", "--dry-run", "--preview"} {
		got := invoke(t, "--base", root, "sync", "--if-due", flag)
		if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "--if-due cannot be combined") {
			t.Fatalf("sync --if-due %s = exit %d, stderr %q; want invalid usage", flag, got.code, got.stderr)
		}
	}
}
