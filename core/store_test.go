package core

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func allLayers() map[Layer]bool {
	enabled := make(map[Layer]bool, len(Layers))
	for _, layer := range Layers {
		enabled[layer] = true
	}
	return enabled
}

func TestStoreActivation(t *testing.T) {
	store := NewStore("/base", map[Layer]bool{LayerEvents: true, LayerWiki: true})
	if got := store.EnabledLayers(); !slices.Equal(got, []Layer{LayerEvents, LayerWiki}) {
		t.Fatalf("EnabledLayers() = %v, want the two activated ones in canonical order", got)
	}
	// An absent entry is disabled, so a hand-written configuration cannot silently enable a
	// layer by omission.
	if store.Enabled(LayerProjects) {
		t.Fatal("an undeclared layer must be disabled")
	}
	if _, err := store.Dir(LayerProjects); !errors.As(err, &ErrLayerDisabled{}) {
		t.Fatalf("Dir(projects) error = %v, want ErrLayerDisabled", err)
	}
	if _, err := store.Dir(LayerEvents); err != nil {
		t.Fatalf("Dir(events) error = %v", err)
	}
}

func TestStoreDirRefusesASymlinkedLayerRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, string(LayerEvents))); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	store := NewStore(root, map[Layer]bool{LayerEvents: true})
	if _, err := store.Dir(LayerEvents); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Dir(events) error = %v, want a symlinked layer root refused", err)
	}
}

// TestStoreResolveConfinesEveryPath is the single enforced rule that replaces a convention:
// a path that leaves the base is rejected, never clamped into something plausible.
func TestStoreResolveConfinesEveryPath(t *testing.T) {
	store := NewStore("/base", allLayers())
	for _, relative := range []string{
		"../etc/passwd", "events/../../etc/passwd", "/etc/passwd", "~/secrets",
		"events\\windows", "events/\x00", "..",
	} {
		if resolved, err := store.Resolve(relative); err == nil {
			t.Fatalf("Resolve(%q) = %q, want it rejected", relative, resolved)
		} else if !errors.Is(err, ErrPathEscapes) {
			t.Fatalf("Resolve(%q) error = %v, want ErrPathEscapes", relative, err)
		}
	}
	got, err := store.Resolve("events/2026-08-22/gmail.json")
	if err != nil || got != filepath.Join("/base", "events", "2026-08-22", "gmail.json") {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
	if _, err := store.Resolve("."); err != nil {
		t.Fatalf("Resolve(\".\") must resolve to the root: %v", err)
	}
}

func TestStoreResolveRefusesADisabledLayer(t *testing.T) {
	store := NewStore("/base", map[Layer]bool{LayerEvents: true})
	if _, err := store.Resolve("wiki/page.md"); !errors.As(err, &ErrLayerDisabled{}) {
		t.Fatalf("Resolve() error = %v, want a disabled layer to be refused, not silently empty", err)
	}
	// A disabled layer names its own remedy: "you turned it off" and "it is empty" are
	// different answers, and a command that conflates them teaches the user to ignore both.
	err := ErrLayerDisabled{Layer: LayerWiki}
	if !strings.Contains(err.Error(), "layers.wiki: true") {
		t.Fatalf("error = %v, want it to name the key to flip", err)
	}
}

func TestStoreRelativeIsTheInverseOfResolve(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, allLayers())
	absolute, err := store.Resolve("wiki/a.md")
	if err != nil {
		t.Fatal(err)
	}
	relative, err := store.Relative(absolute)
	if err != nil || relative != "wiki/a.md" {
		t.Fatalf("Relative(%q) = %q, %v", absolute, relative, err)
	}
	if _, err := store.Relative(filepath.Join(root, "..", "outside")); !errors.Is(err, ErrPathEscapes) {
		t.Fatal("Relative() must refuse a path outside the base")
	}
}

func TestStoreLayerOf(t *testing.T) {
	store := NewStore("/base", allLayers())
	if layer, ok := store.LayerOf("wiki/a.md"); !ok || layer != LayerWiki {
		t.Fatalf("LayerOf() = %q, %v", layer, ok)
	}
	if _, ok := store.LayerOf("fkf.yaml"); ok {
		t.Fatal("a root file belongs to no layer")
	}
}

func TestParseLayer(t *testing.T) {
	if layer, err := ParseLayer("  WIKI "); err != nil || layer != LayerWiki {
		t.Fatalf("ParseLayer() = %q, %v", layer, err)
	}
	if _, err := ParseLayer("logs"); err == nil || !strings.Contains(err.Error(), LayerNames()) {
		t.Fatalf("ParseLayer(\"logs\") error = %v, want it to list the valid layers", err)
	}
}

func TestStoreVersionedDetection(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, allLayers())
	if store.Versioned() || !store.EnforcePermissions() {
		t.Fatal("a plain directory is not versioned, so fkf may repair its modes")
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if store.Versioned() || !store.EnforcePermissions() {
		t.Fatal("an empty .git directory is not repository metadata")
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !store.Versioned() || store.EnforcePermissions() {
		t.Fatal("inside a working tree fkf inspects modes but must not repair them")
	}
}

func TestStoreVersionedDetectionNeverFollowsDotGitSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".git")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	store := NewStore(root, allLayers())
	if store.Versioned() || !store.EnforcePermissions() {
		t.Fatal("a .git symlink is not trusted repository metadata and must not disable permission repair")
	}
}

func TestCleanRelative(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"events/./a.json", "events/a.json"},
		{"events/", "events/"},
		{"a/b/../c", "a/c"},
		{".", "."},
	} {
		got, err := CleanRelative(test.in)
		if err != nil || got != test.want {
			t.Fatalf("CleanRelative(%q) = %q, %v; want %q", test.in, got, err, test.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := ExpandHome("~/brain"); got != filepath.Join(home, "brain") {
		t.Fatalf("ExpandHome(\"~/brain\") = %q, want it under %q", got, home)
	}
	if got := ExpandHome("~notauser/x"); got != "~notauser/x" {
		t.Fatalf("ExpandHome(%q) = %q, want only a leading ~/ expanded", "~notauser/x", got)
	}
}

func TestValidateDate(t *testing.T) {
	if err := ValidateDate("2026-08-22"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDate(""); err != nil {
		t.Fatal("an empty date is an absent bound, not an error")
	}
	if err := ValidateDate("22/08/2026"); err == nil {
		t.Fatal("ValidateDate() must refuse a non-ISO date")
	}
}

// --- trust ------------------------------------------------------------------------------------

func TestTrustGate(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	root := writeBase(t, minimalConfig, nil)

	// A base nobody trusted here refuses, and names the remedy rather than only the refusal.
	err := RequireTrust(t.Context(), root)
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), "fkf trust") {
		t.Fatalf("RequireTrust(t.Context(), ) error = %v, want an untrusted refusal naming `fkf trust`", err)
	}

	recorded, err := WriteTrust(t.Context(), root, time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC))
	if err != nil || !recorded.Trusted {
		t.Fatalf("WriteTrust(t.Context(), ) = %+v, %v", recorded, err)
	}
	if err := RequireTrust(t.Context(), root); err != nil {
		t.Fatalf("RequireTrust(t.Context(), ) after trusting = %v", err)
	}
	// Trust state lives outside the base on purpose: recording it inside would make it
	// clonable, which is the thing the gate exists to prevent.
	if strings.HasPrefix(recorded.Path, root) {
		t.Fatalf("trust record at %q must live outside the base", recorded.Path)
	}

	// Descriptions and retrieval-only projections do not change what executes, so they do
	// not force a second execution review.
	configPath := filepath.Join(root, ConfigFileName)
	semanticOnly := strings.Replace(withTestContract(minimalConfig), "Provider URL.", "Canonical provider URL.", 1)
	if err := os.WriteFile(configPath, []byte(semanticOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireTrust(t.Context(), root); err != nil {
		t.Fatalf("RequireTrust(t.Context(), ) after a semantic-only edit = %v", err)
	}

	// Changing an execution definition re-arms the gate and the message says when it was
	// last trusted.
	executionChange := strings.Replace(semanticOnly, "gh, search, prs", "gh, search, issues", 1)
	if err := os.WriteFile(configPath, []byte(executionChange), 0o600); err != nil {
		t.Fatal(err)
	}
	err = RequireTrust(t.Context(), root)
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), "changed since it was trusted") {
		t.Fatalf("RequireTrust(t.Context(), ) after an edit = %v, want the changed-configuration refusal", err)
	}
}

// TestTrustCoversTheLocalOverlay closes the obvious hole: fkf.local.yaml can carry a run: line
// too, so creating one has to re-arm the gate.
func TestTrustCoversTheLocalOverlay(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := writeBase(t, minimalConfig, nil)
	if _, err := WriteTrust(t.Context(), root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, LocalConfigName), []byte("bin: [/tmp]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireTrust(t.Context(), root); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("RequireTrust(t.Context(), ) = %v, want adding a local overlay to re-arm the gate", err)
	}
}

// TestResolveAdmitsOnlyThePublishedGrammar pins the fix for a read that served credentials. A
// base IS a git repository, so .git/config always sits beside the layers, and a user wiring up
// a source drops a .env there — both were readable through `fkf read`, and therefore through the
// ungated MCP read tool. Confining to the root was never enough on its own.
func TestResolveAdmitsOnlyThePublishedGrammar(t *testing.T) {
	store := NewStore(t.TempDir(), map[Layer]bool{
		LayerEvents: true, LayerIndex: true, LayerTasks: true, LayerProjects: true, LayerWiki: true,
	})
	for _, addressable := range []string{
		"events", "events/2026-05-04", "events/2026-05-04/gmail.json",
		"index", "index/github-repositories.json",
		"tasks", "tasks/2026-05-04", "tasks/2026-05-04/x",
		"tasks/2026-05-04/x/TASKS.md", "projects", "projects/a.md", "wiki",
		"wiki/b.md", GraphFile, GraphMetaFile, ConfigFileName, BaseAgentsFile,
	} {
		if _, err := store.Resolve(addressable); err != nil {
			t.Fatalf("Resolve(%q) = %v, want the published grammar admitted", addressable, err)
		}
	}
	for _, refused := range []string{
		".env", ".git/config", ".netrc", LocalConfigName, "credentials.json",
		".agents/skills/fkf-use/SKILL.md", "bin/git-log-json.sh", "README.md",
		"events/.env", "events/2026-05-04/SUMMARY.md", "events/2026-05-04/private.txt", "events/not-a-day/gmail.json",
		"events/2026-05-04/NESTED.json", "index/.env", "index/backup.key",
		"tasks/.env", "tasks/2026-05-04/x/private.md", "tasks/not-a-day/x/TASKS.md",
		"projects/.env", "projects/backup.key", "projects/nested/page.md",
		"wiki/.env", "wiki/backup.key", "wiki/nested/page.md", "derived",
		"derived/graph.tsv", "derived/graph.meta.json", "derived/private.txt",
	} {
		if _, err := store.Resolve(refused); !errors.Is(err, ErrNotAddressable) {
			t.Fatalf("Resolve(%q) = %v, want ErrNotAddressable", refused, err)
		}
	}
}

// TestResolveRefusesASymlinkInsideTheBase covers the escape a lexical check cannot see. A base
// is a git repository and git carries symlinks, so `wiki/notes -> /home/you/.ssh` arrives with a
// clone and every read through it left the base entirely.
func TestResolveRefusesASymlinkInsideTheBase(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki"), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "loot.md"), filepath.Join(root, "wiki", "escape.md")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	store := NewStore(root, map[Layer]bool{LayerWiki: true})
	if _, err := store.Resolve("wiki/escape.md"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Resolve() = %v, want a symlinked directory refused", err)
	}
	// A real flat page stays addressable, so the guard is not simply refusing everything.
	if _, err := store.Resolve("wiki/real.md"); err != nil {
		t.Fatalf("Resolve() = %v, want a real page path still admitted", err)
	}
}
