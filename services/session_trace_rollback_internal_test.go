package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestTaskTraceBatchRollbackPreservesAChangedPublishedTrace(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, core.TaskTraceFile)
	temporary := filepath.Join(directory, ".staged.tmp")
	if err := os.WriteFile(target, []byte("owner edit\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("generated\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	staged := []stagedSessionTrace{{
		target: sessionTraceTarget{
			trace:    preparedSessionTrace{URI: "tasks/2026-09-01/session/TASKS.md", Content: []byte("generated\n")},
			absolute: target,
		},
		temporary: temporary,
	}}
	err := rollbackSessionTraceBatch(errors.New("later publish failed"), staged, []string{target})
	if err == nil || !strings.Contains(err.Error(), "refuse to roll back changed task trace") {
		t.Fatalf("rollback error = %v, want changed-trace refusal", err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "owner edit\n" {
		t.Fatalf("changed published trace = %q, want the owner edit preserved", data)
	}
	if _, statErr := os.Lstat(temporary); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rollback left the staged temporary: %v", statErr)
	}
}
