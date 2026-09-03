package services

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
)

func TestPruneContextSnapshotsRetainsTheKeepFileAndNewestGeneration(t *testing.T) {
	directory := t.TempDir()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for index := range contextSnapshotRetention + 2 {
		name := filepath.Join(directory, "snapshot-"+string(rune('a'+index))+".json.gz")
		if err := os.WriteFile(name, []byte("snapshot"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
		stamp := start.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(name, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	keep := "snapshot-a.json.gz"
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("keep"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "directory.json.gz"), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("snapshot-a.json.gz", filepath.Join(directory, "link.json.gz")); err != nil {
		t.Fatal(err)
	}

	if err := pruneContextSnapshots(directory, keep); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	regularSnapshots := 0
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json.gz") {
			regularSnapshots++
		}
	}
	if regularSnapshots != contextSnapshotRetention {
		t.Fatalf("retained %d regular snapshots, want %d", regularSnapshots, contextSnapshotRetention)
	}
	for _, retained := range []string{keep, "notes.txt", "directory.json.gz", "link.json.gz"} {
		if _, err := os.Lstat(filepath.Join(directory, retained)); err != nil {
			t.Errorf("pruning removed %s: %v", retained, err)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "snapshot-b.json.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest removable snapshot survived: %v", err)
	}

	if err := pruneContextSnapshots(t.TempDir(), ""); err != nil {
		t.Fatalf("prune below retention limit: %v", err)
	}
	if err := pruneContextSnapshots(filepath.Join(t.TempDir(), "missing"), ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prune missing directory error = %v, want os.ErrNotExist", err)
	}
}

func TestHarnessOwnershipDetectionIsRecursiveAndRelinksOnlyManagedSkills(t *testing.T) {
	hook := map[string]any{
		"outer": []any{map[string]any{"command": "'/base/bin/fkf-hook.sh' claude"}},
	}
	if !findHarnessHookString(hook, "claude") || findHarnessHookString(hook, "codex") ||
		findHarnessHookString(map[string]any{"command": "other"}, "") || findHarnessHookString(42, "") {
		t.Fatalf("recursive hook ownership detection accepted the wrong value")
	}
	if !isManagedHarnessFile([]byte("# Managed by fkf harness install: cline\n"), "cline") ||
		isManagedHarnessFile([]byte("#!/bin/sh\n"), "cline") {
		t.Fatal("managed file marker detection is not exact")
	}
	if !isManagedSkillTarget("/old/base/.agents/skills/fkf-use") ||
		!isManagedSkillTarget("/old/base/.agents/skills/daily-brief") ||
		isManagedSkillTarget("/old/base/skills/fkf-use") {
		t.Fatal("managed skill target detection is not bounded to bundled base skills")
	}

	firstBase := makeHarnessBase(t)
	secondBase := makeHarnessBase(t)
	home := t.TempDir()
	if _, err := InstallHarnesses(t.Context(), firstBase, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}); err != nil {
		t.Fatal(err)
	}
	relinked, err := InstallHarnesses(t.Context(), secondBase, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !relinked.Complete {
		t.Fatalf("relink report = %#v", relinked)
	}
	link := filepath.Join(home, ".claude", "skills", "fkf-use")
	if got, err := os.Readlink(link); err != nil || got != filepath.Join(secondBase, ".agents", "skills", "fkf-use") {
		t.Fatalf("relinked skill = %q, error %v", got, err)
	}
	if got, err := os.Readlink(link + harnessBackupSuffix); err != nil || got != filepath.Join(firstBase, ".agents", "skills", "fkf-use") {
		t.Fatalf("skill backup = %q, error %v", got, err)
	}

	conflictHome := t.TempDir()
	conflict := filepath.Join(conflictHome, ".claude", "skills", "fkf-use")
	if err := os.MkdirAll(filepath.Dir(conflict), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/unmanaged/skills/fkf-use", conflict); err != nil {
		t.Fatal(err)
	}
	_, err = InstallHarnesses(t.Context(), firstBase, HarnessInstallRequest{
		Names: []string{"claude"}, Home: conflictHome, Executable: testHarnessExecutable,
	})
	if !errors.Is(err, ErrHarnessConflict) {
		t.Fatalf("unmanaged skill link error = %v, want ErrHarnessConflict", err)
	}
	if _, err := os.Stat(filepath.Join(conflictHome, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight conflict wrote another harness file: %v", err)
	}
}

func TestLearnDiffParserRejectsAmbiguousOrUnsafePatches(t *testing.T) {
	valid := "--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n-old\n+new\n"
	cases := []struct {
		name string
		diff []byte
		want string
	}{
		{"empty", nil, "diff is empty"},
		{"oversized", bytes.Repeat([]byte{'x'}, maxLearnProposalBytes+1), "limit is"},
		{"invalid UTF-8", []byte{0xff, '\n'}, "not valid UTF-8"},
		{"NUL", []byte("--- a/wiki/a.md\x00\n"), "without NUL"},
		{"CRLF", []byte("--- a/wiki/a.md\r\n"), "LF line endings"},
		{"missing newline", []byte(strings.TrimSuffix(valid, "\n")), "must end with a newline"},
		{"old header", []byte("+++ b/wiki/a.md\n"), "expected an old-file header"},
		{"empty old path", []byte("--- \n+++ b/wiki/a.md\n@@ -0,0 +1 @@\n+x\n"), "file header has no path"},
		{"new header", []byte("--- a/wiki/a.md\n@@ -1 +1 @@\n-old\n+new\n"), "expected a new-file header"},
		{"empty new path", []byte("--- a/wiki/a.md\n+++ \n@@ -1 +1 @@\n-old\n+new\n"), "file header has no path"},
		{"deletion", []byte("--- a/wiki/a.md\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-old\n"), "deletion is not supported"},
		{"new prefix", []byte("--- a/wiki/a.md\n+++ wiki/a.md\n@@ -1 +1 @@\n-old\n+new\n"), "must begin b/"},
		{"old prefix", []byte("--- wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n-old\n+new\n"), "must begin a/"},
		{"rename", []byte("--- a/wiki/a.md\n+++ b/wiki/b.md\n@@ -1 +1 @@\n-old\n+new\n"), "renames are not supported"},
		{"nested target", []byte("--- /dev/null\n+++ b/wiki/nested/a.md\n@@ -0,0 +1 @@\n+x\n"), "one flat"},
		{"no hunks", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n"), "has no hunks"},
		{"malformed hunk", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ malformed\n"), "malformed hunk header"},
		{"empty hunk line", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n\n"), "has no prefix"},
		{"invalid hunk line", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1 +1 @@\n?bad\n"), "must begin space, +, or -"},
		{"excess hunk count", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -0,0 +1,1 @@\n context\n"), "exceeds its declared counts"},
		{"short hunk", []byte("--- a/wiki/a.md\n+++ b/wiki/a.md\n@@ -1,2 +1,2 @@\n one\n"), "hunk declares"},
		{"repeated target", []byte(valid + valid), "repeats target"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseLearnDiff(test.diff)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseLearnDiff() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLearnDiffRoundTripAndPatchMismatchDiagnostics(t *testing.T) {
	oldData := []byte("old\n")
	newData := []byte("new\n")
	diff, err := renderLearnDiff("wiki/a.md", oldData, newData)
	if err != nil {
		t.Fatal(err)
	}
	patches, err := parseLearnDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyLearnFilePatch(oldData, patches[0])
	if err != nil || !bytes.Equal(got, newData) {
		t.Fatalf("round trip = %q, error %v", got, err)
	}
	if unchanged, err := renderLearnDiff("wiki/a.md", oldData, oldData); err != nil || unchanged != nil {
		t.Fatalf("unchanged diff = %q, error %v", unchanged, err)
	}
	for _, invalid := range [][]byte{{0xff}, []byte("no final newline")} {
		if _, err := renderLearnDiff("wiki/a.md", invalid, newData); err == nil {
			t.Fatalf("renderLearnDiff accepted invalid old page %q", invalid)
		}
		if _, err := renderLearnDiff("wiki/a.md", newData, invalid); err == nil {
			t.Fatalf("renderLearnDiff accepted invalid new page %q", invalid)
		}
	}

	mismatches := []struct {
		name  string
		patch learnFilePatch
		want  string
	}{
		{"outside", learnFilePatch{URI: "wiki/a.md", Hunks: []learnPatchHunk{{oldStart: 3, newStart: 3}}}, "outside or overlaps"},
		{"new position", learnFilePatch{URI: "wiki/a.md", Hunks: []learnPatchHunk{{oldStart: 1, oldCount: 1, newStart: 2, newCount: 1, lines: []learnPatchLine{{kind: '-', text: "old"}, {kind: '+', text: "new"}}}}}, "does not follow"},
		{"context", learnFilePatch{URI: "wiki/a.md", Hunks: []learnPatchHunk{{oldStart: 1, oldCount: 1, newStart: 1, newCount: 1, lines: []learnPatchLine{{kind: ' ', text: "other"}}}}}, "context does not match"},
		{"removal", learnFilePatch{URI: "wiki/a.md", Hunks: []learnPatchHunk{{oldStart: 1, oldCount: 1, newStart: 0, newCount: 0, lines: []learnPatchLine{{kind: '-', text: "other"}}}}}, "removal does not match"},
	}
	for _, test := range mismatches {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyLearnFilePatch(oldData, test.patch)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("applyLearnFilePatch() error = %v, want %q", err, test.want)
			}
		})
	}

	removed, err := applyLearnFilePatch(oldData, learnFilePatch{URI: "wiki/a.md", Hunks: []learnPatchHunk{{
		oldStart: 1, oldCount: 1, newStart: 0, newCount: 0,
		lines: []learnPatchLine{{kind: '-', text: "old"}},
	}}})
	if err != nil || len(removed) != 0 {
		t.Fatalf("remove all lines = %q, error %v", removed, err)
	}
	if _, err := applyLearnFilePatch([]byte{0xff, '\n'}, patches[0]); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid source page error = %v", err)
	}
	if _, err := applyLearnFilePatch([]byte("old"), patches[0]); err == nil || !strings.Contains(err.Error(), "must end with a newline") {
		t.Fatalf("unterminated source page error = %v", err)
	}

	tooLarge := bytes.Repeat([]byte("x"), maxLearnProposalBytes)
	tooLarge = append(tooLarge, '\n')
	if utf8.Valid(tooLarge) {
		if _, err := renderLearnDiff("wiki/a.md", nil, tooLarge); err == nil || !strings.Contains(err.Error(), "generated diff") {
			t.Fatalf("oversized rendered diff error = %v", err)
		}
	}
}

func TestIdentityResolverSubstringMatchingAndCanonicalMembership(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime, actor:github.com/maxime]
    kind: person
  maxine:
    canonical: person:email/maxine@example.com
    aliases: [maxine]
    kind: person
`)
	resolver, err := LoadIdentityResolver(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if exact := resolver.Match("maxime"); len(exact) != 1 || exact[0].Canonical != "person:email/maxime@example.com" {
		t.Fatalf("exact match = %#v", exact)
	}
	partial := resolver.Match("maxi")
	if len(partial) != 2 || partial[0].Canonical != "person:email/maxime@example.com" ||
		partial[1].Canonical != "person:email/maxine@example.com" {
		t.Fatalf("substring matches = %#v", partial)
	}
	if matches := resolver.Match("  "); matches != nil {
		t.Fatalf("blank match = %#v, want nil", matches)
	}
	if !resolver.IsCanonical("person:email/maxime@example.com") || resolver.IsCanonical("maxime") {
		t.Fatal("canonical membership accepted an alias or rejected the canonical node")
	}
}
