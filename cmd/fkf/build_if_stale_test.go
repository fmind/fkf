package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestCLIBuildIfStaleSkipsTheWriterLockWhenCachesAreCurrent(t *testing.T) {
	root := demoBase(t)
	if got := invoke(t, "--base", root, "build"); got.code != ExitSuccess {
		t.Fatalf("initial build exited %d: %s%s", got.code, got.stdout, got.stderr)
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
	got := invoke(t, "--format", "json", "--base", root, "build", "--if-stale")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, `"nothing_stale": true`) {
		t.Fatalf("build --if-stale under held lock = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

func TestCLIBuildIfStaleRejectsCheck(t *testing.T) {
	got := invoke(t, "build", "wiki", "--check", "--if-stale")
	if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "--check cannot be combined") {
		t.Fatalf("build --check --if-stale = exit %d, stderr %q", got.code, got.stderr)
	}
}

func TestCLIBuildBodiesRequiresExplicitPrune(t *testing.T) {
	root := demoBase(t)
	without := invoke(t, "--base", root, "build", "bodies")
	if without.code != ExitInvalidUsage || !strings.Contains(without.stderr, "requires --prune") {
		t.Fatalf("build bodies = exit %d, stderr %q", without.code, without.stderr)
	}
	with := invoke(t, "--format", "json", "--base", root, "build", "bodies", "--prune")
	if with.code != ExitSuccess || !strings.Contains(with.stdout, `"bodies"`) {
		t.Fatalf("build bodies --prune = exit %d, stdout %q, stderr %q", with.code, with.stdout, with.stderr)
	}

	// Flag validation
	if got := invoke(t, "--base", root, "build", "bodies", "--older-than", "30d"); got.code != ExitInvalidUsage {
		t.Fatalf("build bodies --older-than without --prune exited %d, want %d", got.code, ExitInvalidUsage)
	}
	if got := invoke(t, "--base", root, "build", "graph", "--older-than", "30d"); got.code != ExitInvalidUsage {
		t.Fatalf("build graph --older-than exited %d, want %d", got.code, ExitInvalidUsage)
	}
	if got := invoke(t, "--base", root, "build", "graph", "--source", "github"); got.code != ExitInvalidUsage {
		t.Fatalf("build graph --source exited %d, want %d", got.code, ExitInvalidUsage)
	}
	// Every rejected duration says what is wrong with it. `%!` in stderr means a format verb
	// reached the user instead of a diagnostic, which is how a nil-wrapping %w shows up.
	for _, tc := range []struct{ raw, want string }{
		{"bad", `invalid duration days "bad"`},
		{"   ", `invalid duration "   "`},
		{"-5d", `invalid duration days "-5d"`},
		{"999999999999d", `invalid duration days "999999999999d"`},
		{"0h", `invalid duration "0h": must be positive`},
		{"-30m", `invalid duration "-30m": must be positive`},
		{"nope", `invalid duration "nope": `},
	} {
		got := invoke(t, "--base", root, "build", "bodies", "--prune", "--older-than", tc.raw)
		if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, tc.want) || strings.Contains(got.stderr, "%!") {
			t.Errorf("build bodies --prune --older-than %q = exit %d, stderr %q, want %d containing %q",
				tc.raw, got.code, got.stderr, ExitInvalidUsage, tc.want)
		}
	}
	if got := invoke(t, "--base", root, "build", "bodies", "--prune", "--source", "INVALID_SOURCE"); got.code != ExitInvalidUsage {
		t.Fatalf("build bodies --source INVALID_SOURCE exited %d, want %d", got.code, ExitInvalidUsage)
	}
	// A syntactically valid name the base never declares is a typo, not an empty selection.
	unknown := invoke(t, "--base", root, "build", "bodies", "--prune", "--source", "github-prs")
	if unknown.code != ExitInvalidUsage || !strings.Contains(unknown.stderr, `unknown source "github-prs"`) ||
		!strings.Contains(unknown.stderr, "github-pull-requests") {
		t.Fatalf("build bodies --prune --source github-prs = exit %d, stderr %q, want %d naming the declared sources",
			unknown.code, unknown.stderr, ExitInvalidUsage)
	}

	// Execution with selective flags, including the Go-duration spelling the flag help advertises
	selective := invoke(t, "--format", "json", "--base", root,
		"build", "bodies", "--prune", "--older-than", "30d", "--source", "github-pull-requests")
	if selective.code != ExitSuccess || !strings.Contains(selective.stdout, `"bodies"`) {
		t.Fatalf("build bodies --prune --older-than 30d --source github-pull-requests = exit %d, stdout %q, stderr %q",
			selective.code, selective.stdout, selective.stderr)
	}
	hours := invoke(t, "--format", "json", "--base", root, "build", "bodies", "--prune", "--older-than", "720h")
	if hours.code != ExitSuccess || !strings.Contains(hours.stdout, `"bodies"`) {
		t.Fatalf("build bodies --prune --older-than 720h = exit %d, stdout %q, stderr %q",
			hours.code, hours.stdout, hours.stderr)
	}

	// Text formatting does not stutter and keeps the count the JSON envelope reports
	textOut := invoke(t, "--format", "text", "--base", root, "build", "bodies", "--prune")
	if textOut.code != ExitSuccess || !strings.Contains(textOut.stdout, "body cache is empty (0 bytes)") {
		t.Fatalf("build bodies text output = %q, want 'body cache is empty (0 bytes)'", textOut.stdout)
	}
	seedBodyCache(t, root, "github-pull-requests", "record:github-pull-requests/1", "Body.\n")
	noMatch := invoke(t, "--format", "text", "--base", root, "build", "bodies", "--prune", "--source", "jira-issues")
	if noMatch.code != ExitSuccess || !strings.Contains(noMatch.stdout, "nothing matched; body cache unchanged (0 bytes)") {
		t.Fatalf("build bodies --prune --source jira-issues text output = %q, want a no-op report", noMatch.stdout)
	}
	pruned := invoke(t, "--format", "text", "--base", root, "build", "bodies", "--prune")
	if pruned.code != ExitSuccess || !strings.Contains(pruned.stdout, "pruned 1 cached body or bodies (6 bytes)") {
		t.Fatalf("build bodies --prune text output = %q, want the pruned count beside the reclaimed bytes", pruned.stdout)
	}
}

// seedBodyCache writes one manifest-consistent cached body so a prune has something to report.
// The manifest binds the URI to its canonical path and content digest exactly as cacheBody does.
func seedBodyCache(t *testing.T, root, source, uri, body string) {
	t.Helper()
	digest := sha256.Sum256([]byte(uri))
	relative := path.Join("bodies", source, hex.EncodeToString(digest[:])+".txt")
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(body), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	content := sha256.Sum256([]byte(body))
	manifest := fmt.Sprintf(`{
  "schema_version": 1,
  "entries": {
    %[1]q: {
      "uri": %[1]q,
      "source": %[2]q,
      "path": %[3]q,
      "sha256": %[4]q,
      "bytes": %[5]d,
      "fetched_at": "2026-01-01T00:00:00Z"
    }
  }
}
`, uri, source, relative, hex.EncodeToString(content[:]), len(body))
	if err := os.WriteFile(filepath.Join(root, "bodies", "manifest.json"), []byte(manifest), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}
