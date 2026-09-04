package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrustStateFailsClosedWithoutHomeOrXDGState(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}

	if got := StateDir(); got != "" {
		t.Fatalf("StateDir() = %q, want no shared temporary fallback", got)
	}
	if _, err := ReadTrust(t.Context(), trustTestConfig(t, root)); err == nil || !strings.Contains(err.Error(), "HOME or XDG_STATE_HOME") {
		t.Fatalf("ReadTrust() error = %v, want a missing state-root refusal", err)
	}
	if _, err := WriteTrust(t.Context(), trustTestConfig(t, root), time.Now()); err == nil || !strings.Contains(err.Error(), "HOME or XDG_STATE_HOME") {
		t.Fatalf("WriteTrust() error = %v, want a missing state-root refusal", err)
	}
	if _, err := os.Stat(filepath.Join(temporary, "fkf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trust operation created the shared temporary fallback: %v", err)
	}
}

func TestTrustBoundariesRefuseAPreCancelledContextWithoutWritingState(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateRoot)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "config digest", run: func() error { _, err := ConfigDigest(ctx, trustTestConfig(t, root)); return err }},
		{name: "bin scripts", run: func() error { _, err := BinScripts(ctx, root); return err }},
		{name: "trust items", run: func() error { _, err := TrustItems(ctx, trustTestConfig(t, root)); return err }},
		{name: "read trust", run: func() error { _, err := ReadTrust(ctx, trustTestConfig(t, root)); return err }},
		{name: "write trust", run: func() error { _, err := WriteTrust(ctx, trustTestConfig(t, root), time.Now()); return err }},
		{name: "require trust", run: func() error { return RequireTrust(ctx, trustTestConfig(t, root)) }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled trust operation error = %v, want context.Canceled", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "fkf", "trust")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-cancelled trust write created state: %v", err)
	}
}

// TestConfigDigestCoversTheBinScripts is the regression test for a trust gate that reviewed the
// wrong thing. <base>/bin is committed with the base and prepended to PATH, so approving
// `run: git log …` meant nothing while a bin/git the reviewer never saw is what actually ran —
// and, because only the YAML was hashed, a later `git pull` could swap that script with the
// digest still matching.
func TestConfigDigestCoversTheBinScripts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, BaseBinDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, BaseBinDir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	added, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if added == before {
		t.Fatal("adding bin/git left the digest unchanged; the script would run unreviewed")
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	edited, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if edited == added {
		t.Fatal("editing bin/git after trust left the digest unchanged; this is the `git pull` case")
	}
}

// Source verification hooks are repository-controlled code just like collection helpers. Keeping
// them in a dedicated tests/ tree is safe only when every byte and executable-bit change re-arms
// trust before `fkf test` may execute the new definition.
func TestConfigDigestCoversTheSourceTestScripts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	testsDir := filepath.Join(root, BaseTestsDir, "fixtures")
	if err := os.MkdirAll(testsDir, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(testsDir, "source-check.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	added, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if added == before {
		t.Fatal("adding tests/fixtures/source-check.sh left the digest unchanged; fkf test would run unreviewed bytes")
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	edited, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if edited == added {
		t.Fatal("editing tests/source-check.sh left the digest unchanged")
	}
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	armed, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if armed == edited {
		t.Fatal("making tests/fixtures/source-check.sh executable left the digest unchanged")
	}
	items, err := TrustItems(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == TrustItemTest && item.Name == "fixtures/source-check.sh" && item.Executable {
			return
		}
	}
	t.Fatalf("trust items = %+v, want an executable test/fixtures/source-check.sh item", items)
}

func TestTestScriptsRefusesSymlinksAtEveryDepth(t *testing.T) {
	for _, testCase := range []struct {
		name string
		link func(root, outside string) string
	}{
		{
			name: "root",
			link: func(root, outside string) string {
				return filepath.Join(root, BaseTestsDir)
			},
		},
		{
			name: "nested file",
			link: func(root, outside string) string {
				directory := filepath.Join(root, BaseTestsDir, "fixtures")
				if err := os.MkdirAll(directory, BaseDirMode); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(directory, "source-check.sh")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "source-check.sh")
			if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			link := testCase.link(root, outside)
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks are unavailable: %v", err)
			}
			if _, err := TestScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("TestScripts() error = %v, want the symlink refused", err)
			}
			if _, err := ConfigDigest(t.Context(), trustTestConfig(t, root)); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("ConfigDigest() error = %v, want the tests/ refusal to reach trust", err)
			}
			if err := RequireTrust(t.Context(), trustTestConfig(t, root)); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("RequireTrust() error = %v, want the tests/ refusal to reach execution", err)
			}
		})
	}
}

func TestTestScriptsRefusesASymlinkedNestedDirectory(t *testing.T) {
	root := t.TempDir()
	tests := filepath.Join(root, BaseTestsDir)
	if err := os.MkdirAll(tests, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "fixture.json"), []byte("{}\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tests, "fixtures")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := TestScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("TestScripts() error = %v, want the nested directory symlink refused", err)
	}
}

func TestTestScriptsRefusesATestsRootThatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, BaseTestsDir), []byte("not a directory\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := TestScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("TestScripts() error = %v, want a non-directory tests root refused", err)
	}
}

func TestTrustDiffKeepsBinAndTestsDistinctAcrossTestHookChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, BaseBinDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BaseBinDir, "shared.sh"), []byte("echo one\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTrust(t.Context(), trustTestConfig(t, root), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, BaseTestsDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, BaseTestsDir, "shared.sh")
	assertChange := func(want TrustChangeKind, mutate func() error) {
		t.Helper()
		if err := mutate(); err != nil {
			t.Fatal(err)
		}
		state, err := ReadTrust(t.Context(), trustTestConfig(t, root))
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Changes) != 1 || state.Changes[0] != (TrustChange{
			Kind: want, Item: TrustItemTest, Name: "shared.sh",
		}) {
			t.Fatalf("changes = %+v, want one %s test/shared.sh change", state.Changes, want)
		}
		if _, err := WriteTrust(t.Context(), trustTestConfig(t, root), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	assertChange(TrustAdded, func() error {
		return os.WriteFile(hook, []byte("echo one\n"), BaseFileMode)
	})
	assertChange(TrustModified, func() error {
		return os.WriteFile(hook, []byte("echo two\n"), BaseFileMode)
	})
	assertChange(TrustArmed, func() error { return os.Chmod(hook, 0o700) })
	assertChange(TrustDisarmed, func() error { return os.Chmod(hook, BaseFileMode) })
	assertChange(TrustRemoved, func() error { return os.Remove(hook) })
}

// TestBinScriptsRefusesASymlinkedFile closes the gap left by hashing only a link target: the
// target text stays fixed while the file outside the base can change after trust.
func TestBinScriptsRefusesASymlinkedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, BaseBinDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\necho one\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, BaseBinDir, "helper")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	if _, err := BinScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("BinScripts(t.Context(), ) error = %v, want the symlink refused", err)
	}
	if _, err := ConfigDigest(t.Context(), trustTestConfig(t, root)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ConfigDigest(t.Context(), ) error = %v, want the refusal to reach the trust gate", err)
	}
}

// TestConfigDigestCoversTheExecutableBit pins the half of the gate that a content-only hash
// missed. Git tracks the executable bit and nothing else about a mode, so `old mode 100644 /
// new mode 100755` is a one-line diff a `git pull` can deliver — and it is exactly what decides
// whether PATH lookup picks a shadow `git` up.
func TestConfigDigestCoversTheExecutableBit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, BaseBinDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, BaseBinDir, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho shadow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("making bin/git executable left the digest unchanged; it would now run unreviewed")
	}
}

// TestConfigDigestCoversNestedBinScripts pins the depth of the walk. A one-level listing left
// everything under bin/<subdir>/ outside both the digest and the `fkf trust` listing, so a
// reviewer approved bin/helper once and every later edit to the file it sources was trusted
// silently.
func TestConfigDigestCoversNestedBinScripts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, BaseBinDir, "lib")
	if err := os.MkdirAll(nested, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	sourced := filepath.Join(nested, "impl.sh")
	if err := os.WriteFile(sourced, []byte("echo v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourced, []byte("echo pwned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("rewriting bin/lib/impl.sh left the digest unchanged; a sourced helper is executable code")
	}
	scripts, err := BinScripts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, script := range scripts {
		if script.Name == "lib/impl.sh" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("bin/lib/impl.sh is not in the trust listing, so a reviewer never sees it: %+v", scripts)
	}
}

// TestConfigDigestTreatsKnowledgeAsDataNotAnExecutionDefinition pins the honest boundary of
// the trust gate. Authored and collected content changes routinely, so hashing every base file
// would re-arm trust after every note or sync. A reviewed executable plan is not sandboxed and
// must not source such mutable data; every base-controlled helper belongs under the hashed
// bin/ tree instead.
func TestConfigDigestTreatsKnowledgeAsDataNotAnExecutionDefinition(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	wiki := filepath.Join(root, string(LayerWiki))
	if err := os.Mkdir(wiki, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(wiki, "knowledge.md")
	if err := os.WriteFile(page, []byte("# One\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("# Two\n"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	after, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("editing knowledge re-armed command trust; authored data is not an execution definition")
	}
}

// TestBinScriptsRefusesASymlinkedBinDirectory closes the hole the sibling tests above leave
// open: they all check what happens INSIDE bin/, and every one of them passes when bin/ is
// itself a link. WalkDir stops at a symlinked root and the walk skips the root entry, so every
// script behind the link stayed out of the digest and out of the `fkf trust` listing while the
// directory it points at was still first on the PATH of every declared command. A shadow `git`
// there ran and its output was stored as a record, and swapping that script afterwards left the
// digest byte-identical — the exact failure the bin/ hashing was added to prevent, reachable by
// replacing one directory with one link.
func TestBinScriptsRefusesASymlinkedBinDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract("name: t\n")), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "git"), []byte("#!/bin/sh\necho shadow\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, BaseBinDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := BinScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("BinScripts(t.Context(), ) error = %v, want it refused as an unsafe path", err)
	}
	// And the refusal reaches the gate, so nothing runs against such a base.
	if _, err := ConfigDigest(t.Context(), trustTestConfig(t, root)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ConfigDigest(t.Context(), ) error = %v, want the refusal to reach the trust gate", err)
	}
	if err := RequireTrust(t.Context(), trustTestConfig(t, root)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("RequireTrust(t.Context(), ) error = %v, want the refusal to reach every command that executes", err)
	}
}

// A bin/ that is a regular file rather than a directory is refused for the same reason: it is
// not a tree the walk can enumerate, so "nothing to review" would be the wrong answer.
func TestBinScriptsRefusesABinThatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, BaseBinDir), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BinScripts(t.Context(), root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("BinScripts(t.Context(), ) error = %v, want it refused as an unsafe path", err)
	}
}

// TestTrustRecordsPerItemDigestsAndDiffsThem is what makes a re-trust reviewable. One aggregate
// hash can only ever say "something changed", so the second time trust is asked for — after a
// `git pull` on a shared base, the moment the gate exists for — re-approval meant re-reading
// every source and every script to find the line that moved, and a review nobody re-reads is a
// review nobody performs.
func TestTrustRecordsPerItemDigestsAndDiffsThem(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	config := "name: t\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\n" +
		"sources:\n" +
		"  kept:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n    fields:\n      id: .id\n      time: .t\n      title: .title\n" +
		"  moved:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n    fields:\n      id: .id\n      time: .t\n      title: .title\n"
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract(body)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(config)
	if err := os.MkdirAll(filepath.Join(root, BaseBinDir), BaseDirMode); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, BaseBinDir, "helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTrust(t.Context(), trustTestConfig(t, root), time.Now()); err != nil {
		t.Fatal(err)
	}

	// Three edits a reviewer must be able to tell apart: one command's text moved, one script
	// gained the bit that decides whether PATH picks it up, and one source appeared.
	write(config +
		"  added:\n    enabled: true\n    layer: events\n    run: [curl, http://evil.test]\n    fields:\n      id: .id\n      time: .t\n      title: .title\n")
	moved := strings.Replace(config, "  moved:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]",
		"  moved:\n    enabled: true\n    layer: events\n    run: [echo, \"[1]\"]", 1)
	write(moved +
		"  added:\n    enabled: true\n    layer: events\n    run: [curl, http://evil.test]\n    fields:\n      id: .id\n      time: .t\n      title: .title\n")
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}

	state, err := ReadTrust(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if state.Trusted {
		t.Fatal("the base changed and is still reported trusted")
	}
	got := map[string]TrustChangeKind{}
	for _, change := range state.Changes {
		got[string(change.Item)+"/"+change.Name] = change.Kind
	}
	for name, want := range map[string]TrustChangeKind{
		"source/added":  TrustAdded,
		"source/moved":  TrustModified,
		"script/helper": TrustArmed,
	} {
		if got[name] != want {
			t.Errorf("change for %s = %q, want %q (all changes: %+v)", name, got[name], want, state.Changes)
		}
	}
	// A source nobody touched must not appear, or the diff is the listing again.
	if _, noisy := got["source/kept"]; noisy {
		t.Errorf("an untouched source appears in the diff: %+v", state.Changes)
	}
}

func TestTrustSourceDigestCoversDisplayedExecutionPolicy(t *testing.T) {
	const baseConfig = `name: t
layers: {events: true}
sources:
  source:
    enabled: true
    layer: events
    run: [echo, "[]"]
    fields:
      id: .id
      time: .t
      title: .title
`
	policies := map[string]string{
		"timeout":      "    timeout: 5s\n",
		"retry":        "    retry:\n      attempts: 2\n      backoff: 1s\n      on: [exit:7]\n",
		"min_interval": "    min_interval: 1s\n",
		"window":       "    window: true\n",
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ConfigFileName)
			if err := os.WriteFile(path, []byte(withTestContract(baseConfig)), BaseFileMode); err != nil {
				t.Fatal(err)
			}
			before := trustSourceDigest(t, root, "source")
			if err := os.WriteFile(path, []byte(withTestContract(baseConfig+policy)), BaseFileMode); err != nil {
				t.Fatal(err)
			}
			after := trustSourceDigest(t, root, "source")
			if after == before {
				t.Fatalf("adding displayed %s policy left the per-source trust digest unchanged", name)
			}
		})
	}
}

func TestTrustSourceDigestCoversSourceTestHook(t *testing.T) {
	without := &Source{Name: "source", Run: []string{"collect.sh"}}
	with := &Source{Name: "source", Run: []string{"collect.sh"}, Test: []string{"collect.sh", "--test"}}
	if sourceDigest(without) == sourceDigest(with) {
		t.Fatal("adding a source test hook left the execution trust digest unchanged")
	}
	withAuth := &Source{Name: "source", Run: []string{"collect.sh"}, Auth: []string{"provider", "auth", "status"}}
	if sourceDigest(without) == sourceDigest(withAuth) {
		t.Fatal("adding a source auth probe left the execution trust digest unchanged")
	}
}

func TestTrustSourceDigestCoversBodyFieldPaths(t *testing.T) {
	const config = `name: t
layers: {events: true}
sources:
  source:
    enabled: true
    run: [echo, "[]"]
    fields:
      id: .id
      time: .t
      title: .title
      project: [.project, .fallback]
      topic: .topic
    body: [cli, view, "{{id}}", --project, "{{project}}"]
`
	root := t.TempDir()
	path := filepath.Join(root, ConfigFileName)
	if err := os.WriteFile(path, []byte(withTestContract(config)), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	before := trustSourceDigest(t, root, "source")
	changedBodyField := strings.Replace(config, "project: [.project, .fallback]", "project: [.space, .fallback]", 1)
	if err := os.WriteFile(path, []byte(withTestContract(changedBodyField)), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if after := trustSourceDigest(t, root, "source"); after == before {
		t.Fatal("changing a field projected into body argv left the per-source trust digest unchanged")
	}
	changedRetrievalField := strings.Replace(config, "topic: .topic", "topic: .category", 1)
	if err := os.WriteFile(path, []byte(withTestContract(changedRetrievalField)), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if after := trustSourceDigest(t, root, "source"); after != before {
		t.Fatal("changing a retrieval-only field altered the execution-specific source digest")
	}
}

func TestTrustSourceDigestFramesVariableLengthValues(t *testing.T) {
	// The old NUL-delimited encoding gave these distinct argv sequences identical bytes:
	// the embedded delimiters in the first argument shifted the following field boundaries.
	// Keep the regression at the digest seam even though configuration validation may reject
	// particular control bytes independently; the trust encoding must remain injective itself.
	joined := &Source{Body: []string{"safe\x00body\x00evil", "{{id}}"}}
	split := &Source{Body: []string{"safe", "evil", "{{id}}"}}
	if sourceDigest(joined) == sourceDigest(split) {
		t.Fatal("distinct body argv sequences share a trust digest")
	}

	runAndRetry := &Source{Run: []string{"safe\x00retry-on\x00exit:7"}}
	separateRetry := &Source{Run: []string{"safe"}, Retry: RetryPolicy{On: []string{"exit:7"}}}
	if sourceDigest(runAndRetry) == sourceDigest(separateRetry) {
		t.Fatal("run text and retry policy boundaries share a trust digest")
	}
}

func TestTrustItemsIncludeDisabledSourcesAndTheirEnabledState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ConfigFileName)
	config := `name: t
layers: {events: true}
sources:
  dormant:
    enabled: false
    layer: events
    run: [dormant, --json]
    fields:
      id: .id
      time: .t
      title: .title
`
	if err := os.WriteFile(path, []byte(withTestContract(config)), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	disabled := trustSourceDigest(t, root, "dormant")
	if err := os.WriteFile(path, []byte(withTestContract(strings.Replace(config, "enabled: false", "enabled: true", 1))), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if enabled := trustSourceDigest(t, root, "dormant"); enabled == disabled {
		t.Fatal("enabling a declared source left its per-source trust digest unchanged")
	}
}

func TestConfigDigestUsesTheCanonicalExecutionPlan(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ConfigFileName)
	config := withTestContract(`name: t
layers: {events: true}
sources:
  source:
    enabled: true
    layer: events
    run: [provider, list, --json]
    fields:
      id: .id
      time: .time
      title: .title
      topic: .topic
`)
	if err := os.WriteFile(path, []byte(config), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigDigest(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}

	semanticOnly := strings.Replace(config, "Searchable topic.", "Terms used only for retrieval.", 1)
	semanticOnly = strings.Replace(semanticOnly, "topic: .topic", "topic: .category", 1)
	semanticOnly = "# Human documentation does not arm execution.\n" + semanticOnly
	if err := os.WriteFile(path, []byte(semanticOnly), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if after, err := ConfigDigest(t.Context(), trustTestConfig(t, root)); err != nil || after != before {
		t.Fatalf("semantic-only configuration edit digest = %q, %v, want unchanged %q", after, err, before)
	}

	executionChange := strings.Replace(semanticOnly, "provider, list, --json", "provider, list, --json, --all", 1)
	if err := os.WriteFile(path, []byte(executionChange), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if after, err := ConfigDigest(t.Context(), trustTestConfig(t, root)); err != nil || after == before {
		t.Fatalf("execution edit digest = %q, %v, want a new digest", after, err)
	}
}

func trustSourceDigest(t *testing.T, root, name string) string {
	t.Helper()
	items, err := TrustItems(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == TrustItemSource && item.Name == name {
			return item.Digest
		}
	}
	t.Fatalf("source trust item %q not found in %+v", name, items)
	return ""
}

// TestTrustDiffIsAbsentUntilItCanBeHonest keeps the fallback safe. A record written before
// per-item digests existed, and a base that has never been trusted, both carry nothing to diff
// against — and the caller must then print the whole listing rather than an empty change set
// that reads as "nothing changed".
func TestTrustDiffIsAbsentUntilItCanBeHonest(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName),
		[]byte(withTestContract("name: t\nlayers: {events: true}\nsources: {}\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	never, err := ReadTrust(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(never.Changes) != 0 {
		t.Fatalf("changes = %+v, want none for a base that was never trusted", never.Changes)
	}
	// A record from an older build: the aggregate digest only.
	state, err := WriteTrust(t.Context(), trustTestConfig(t, root), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	withoutItems, err := json.Marshal(TrustRecord{Base: state.Base, Digest: "stale", TrustedAt: state.Since})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Path, withoutItems, BaseFileMode); err != nil {
		t.Fatal(err)
	}
	old, err := ReadTrust(t.Context(), trustTestConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if old.Trusted {
		t.Fatal("a stale digest must not be trusted")
	}
	if len(old.Changes) != 0 {
		t.Fatalf("changes = %+v, want none when the stored record predates per-item digests", old.Changes)
	}
}

func trustTestConfig(t *testing.T, root string) *Config {
	t.Helper()
	config, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// TestBinScriptsFailsClosedWhenAnEntryVanishesMidWalk pins the direction the gate must fail in.
// "Nothing to review" is an answer only an absent tree root may give. Reading it out of the
// walk's error instead let a single ENOENT raised anywhere below the root — an editor's atomic
// save, a helper's temp file, a concurrent checkout — return an empty listing with no error.
// That empty listing is exactly the digest a base recorded when `fkf trust` ran before any
// helper existed, so a bin/ full of unreviewed executables would match the stored record and
// run, with bin/ prepended first on the child PATH.
func TestBinScriptsFailsClosedWhenAnEntryVanishesMidWalk(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, BaseBinDir)
	if err := os.MkdirAll(bin, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	const stable = 64
	for i := range stable {
		script := filepath.Join(bin, fmt.Sprintf("helper-%02d.sh", i))
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho helper\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// One file appearing and disappearing under the tree, so whichever syscall loses the race —
	// the walk's readdir, the entry lstat, or the script read — reports ENOENT.
	flapping := filepath.Join(bin, "zz-flapping.sh")
	stop := make(chan struct{})
	var flapper sync.WaitGroup
	flapper.Add(1)
	go func() {
		defer flapper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(flapping, []byte("#!/bin/sh\necho flapping\n"), 0o700)
			_ = os.Remove(flapping)
		}
	}()
	defer func() {
		close(stop)
		flapper.Wait()
	}()
	for range 200 {
		scripts, err := BinScripts(t.Context(), root)
		if err != nil {
			// Refusing the whole tree is the correct answer to a vanished entry: the caller
			// re-runs the check rather than trusting a listing it knows is incomplete.
			continue
		}
		if len(scripts) < stable {
			t.Fatalf(
				"BinScripts(t.Context(), ) = %d scripts with a nil error, want the %d stable scripts or a refusal",
				len(scripts), stable,
			)
		}
	}
}

// Only a missing tree root means "nothing to review". Every other failure to enumerate the tree
// leaves the reviewed set unknown, and an unknown set may not be hashed as the empty one.
func TestBinScriptsTreatsOnlyAnAbsentTreeAsNothingToReview(t *testing.T) {
	root := t.TempDir()
	scripts, err := BinScripts(t.Context(), root)
	if err != nil || scripts != nil {
		t.Fatalf("BinScripts(t.Context(), ) = %v, %v, want no scripts and no error for an absent bin/", scripts, err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions the rest of this test needs")
	}
	nested := filepath.Join(root, BaseBinDir, "lib")
	if err := os.MkdirAll(nested, BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "impl.sh"), []byte("#!/bin/sh\necho impl\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Searchable but not listable, so the walk's readdir fails below the root.
	if err := os.Chmod(nested, 0o100); err != nil {
		t.Fatal(err)
	}
	// Restored before t.TempDir removes the tree, which needs the directory readable again.
	t.Cleanup(func() { _ = os.Chmod(nested, BaseDirMode) })
	if _, err := BinScripts(t.Context(), root); err == nil {
		t.Fatal("BinScripts(t.Context(), ) error = nil, want the unreadable subdirectory refused")
	}
}
