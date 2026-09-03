package services

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestMoveValidatedLearnProposalRefusesDifferentLiveBytes(t *testing.T) {
	directory := t.TempDir()
	approved := []byte("approved proposal\n")
	id := learnProposalDigest(approved)
	source := filepath.Join(directory, id+".diff")
	destination := filepath.Join(directory, "archived.diff")
	if err := os.WriteFile(source, []byte("replacement proposal\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	err := moveValidatedLearnProposal(source, destination, id, approved)
	if err == nil || !strings.Contains(err.Error(), "does not match its SHA-256 digest") {
		t.Fatalf("moveValidatedLearnProposal() error = %v, want live-byte refusal", err)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("refused proposal was not left active: %v", statErr)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("replacement bytes reached the archive: %v", statErr)
	}
}

func TestWriteLearnUpdatesRefusesAChangedTargetBeforeWriting(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.md")
	second := filepath.Join(directory, "second.md")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("approved\n"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := map[string]learnSnapshot{
		"wiki/first.md": {
			absolute: first, exists: true, data: []byte("approved\n"), mode: core.BaseFileMode,
			limit: core.MaxNarrativeBytes,
		},
		"wiki/second.md": {
			absolute: second, exists: true, data: []byte("approved\n"), mode: core.BaseFileMode,
			limit: core.MaxNarrativeBytes,
		},
	}
	updates := []learnUpdate{
		{uri: "wiki/first.md", absolute: first, data: []byte("updated first\n"), mode: core.BaseFileMode},
		{uri: "wiki/second.md", absolute: second, data: []byte("updated second\n"), mode: core.BaseFileMode},
	}
	if err := os.WriteFile(second, []byte("owner edit\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	err := writeLearnUpdates(updates, snapshots, maps.Clone(snapshots))
	if err == nil || !strings.Contains(err.Error(), "target wiki/second.md changed") {
		t.Fatalf("writeLearnUpdates() error = %v, want changed-target refusal", err)
	}
	for path, want := range map[string]string{first: "approved\n", second: "owner edit\n"} {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestWriteLearnUpdatesRechecksEachTargetAndRollsBackPublishedFiles(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.md")
	second := filepath.Join(directory, "second.md")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("approved\n"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := map[string]learnSnapshot{
		"wiki/first.md": {
			absolute: first, exists: true, data: []byte("approved\n"), mode: core.BaseFileMode,
			limit: core.MaxNarrativeBytes,
		},
		"wiki/second.md": {
			absolute: second, exists: true, data: []byte("approved\n"), mode: core.BaseFileMode,
			limit: core.MaxNarrativeBytes,
		},
	}
	published := maps.Clone(snapshots)
	updates := []learnUpdate{
		{
			uri: "wiki/first.md", absolute: first, data: []byte("updated first\n"), mode: core.BaseFileMode,
		},
		{uri: "wiki/second.md", absolute: second, data: []byte("updated second\n"), mode: core.BaseFileMode},
	}
	applyErr := writeLearnUpdatesWithObserver(updates, snapshots, published, func(index int) {
		if index != 0 {
			return
		}
		if err := os.WriteFile(second, []byte("owner edit\n"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
	})
	if applyErr == nil || !strings.Contains(applyErr.Error(), "target wiki/second.md changed") {
		t.Fatalf("writeLearnUpdates() error = %v, want per-target changed refusal", applyErr)
	}
	rollbackErr := restoreLearnSnapshots(snapshots, published)
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "refuse to restore changed file wiki/second.md") {
		t.Fatalf("restoreLearnSnapshots() error = %v, want concurrent editor save preserved", rollbackErr)
	}
	for path, want := range map[string]string{first: "approved\n", second: "owner edit\n"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s after rollback = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestLearnRollbackPreservesAConcurrentEditorSave(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "page.md")
	if err := os.WriteFile(target, []byte("owner edit\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	original := learnSnapshot{
		absolute: target, exists: true, data: []byte("before\n"), mode: core.BaseFileMode,
		limit: core.MaxNarrativeBytes,
	}
	published := learnSnapshot{
		absolute: target, exists: true, data: []byte("proposal output\n"), mode: core.BaseFileMode,
		limit: core.MaxNarrativeBytes,
	}
	err := restoreLearnSnapshots(
		map[string]learnSnapshot{"wiki/page.md": original},
		map[string]learnSnapshot{"wiki/page.md": published},
	)
	if err == nil || !strings.Contains(err.Error(), "refuse to restore changed file wiki/page.md") {
		t.Fatalf("restoreLearnSnapshots() error = %v, want concurrent-save refusal", err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "owner edit\n" {
		t.Fatalf("rollback replaced the concurrent editor save with %q", data)
	}
}
