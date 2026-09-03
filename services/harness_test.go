package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

const testHarnessExecutable = "/usr/local/bin/fkf"

func TestHarnessVocabularyAndFragmentsAreComplete(t *testing.T) {
	want := []string{
		"claude", "codex", "gemini", "copilot", "antigravity",
		"opencode", "grok", "cursor", "kiro", "cline",
	}
	if got := HarnessNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("harness names = %v, want %v", got, want)
	}

	base := makeHarnessBase(t)
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			plan, err := HarnessPlanFor(base, name, testHarnessExecutable)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Name != name || plan.Base != base {
				t.Fatalf("plan identity = %#v", plan)
			}
			joined := ""
			for _, fragment := range plan.Fragments {
				joined += fragment.Content + "\n"
			}
			for _, required := range []string{
				testHarnessExecutable, "mcp", "serve", "--base", base, "fkf-hook.sh", name,
				"trust", "--check",
			} {
				if !strings.Contains(joined, required) {
					t.Fatalf("fragments do not contain %q:\n%s", required, joined)
				}
			}
			if !strings.Contains(joined, filepath.Join(base, ".agents", "skills")) {
				t.Fatalf("fragments do not expose the skills bridge:\n%s", joined)
			}
		})
	}

	if _, err := HarnessPlanFor(base, "Devin", testHarnessExecutable); !errors.Is(err, ErrHarnessName) {
		t.Fatalf("unknown harness error = %v", err)
	}
}

func TestHarnessPlansUseCurrentVendorSchemas(t *testing.T) {
	base := makeHarnessBase(t)

	copilot := harnessFragmentFor(t, base, "copilot", "~/.copilot/hooks/fkf.json", "hooks.sessionStart")
	copilotHook, ok := copilot.value.(map[string]any)
	if !ok {
		t.Fatalf("Copilot hook value = %#v", copilot.value)
	}
	if copilotHook["bash"] == nil || copilotHook["timeoutSec"] != json.Number("20") {
		t.Fatalf("Copilot hook = %#v", copilotHook)
	}
	if _, oldKey := copilotHook["command"]; oldKey {
		t.Fatalf("Copilot hook uses the legacy command key: %#v", copilotHook)
	}

	antigravity := harnessFragmentFor(t, base, "antigravity", "~/.gemini/config/hooks.json", "fkf")
	antigravityHook, ok := antigravity.value.(map[string]any)
	if !ok {
		t.Fatalf("Antigravity hook value = %#v", antigravity.value)
	}
	if _, wrapper := antigravityHook["hooks"]; wrapper {
		t.Fatalf("Antigravity hook has a Gemini-style wrapper: %#v", antigravityHook)
	}
	if entries, ok := antigravityHook["PreInvocation"].([]any); !ok || len(entries) != 1 {
		t.Fatalf("Antigravity PreInvocation = %#v", antigravityHook["PreInvocation"])
	}

	kiro := harnessFragmentFor(t, base, "kiro", "~/.kiro/hooks/fkf.json", "hooks")
	kiroHook, ok := kiro.value.(map[string]any)
	if !ok {
		t.Fatalf("Kiro hook value = %#v", kiro.value)
	}
	action, ok := kiroHook["action"].(map[string]any)
	if !ok || action["type"] != "command" || action["command"] == nil {
		t.Fatalf("Kiro hook action = %#v", kiroHook)
	}
	if _, oldKey := kiroHook["command"]; oldKey {
		t.Fatalf("Kiro hook uses the pre-v3 shape: %#v", kiroHook)
	}

	opencode := harnessFragmentFor(t, base, "opencode", "~/.config/opencode/plugins/fkf.js", "")
	if !strings.Contains(opencode.Content, testHarnessExecutable) {
		t.Fatalf("OpenCode hook does not pin the installing FKF executable:\n%s", opencode.Content)
	}

	cline := harnessFragmentFor(t, base, "cline", "~/.cline/data/settings/cline_mcp_settings.json", "mcpServers.fkf")
	if cline.Kind != HarnessFragmentJSON {
		t.Fatalf("Cline MCP fragment = %#v", cline)
	}
	hook := harnessFragmentFor(t, base, "cline", "~/.cline/hooks/TaskStart", "")
	if hook.Kind != HarnessFragmentFile {
		t.Fatalf("Cline hook fragment = %#v", hook)
	}
	if !strings.Contains(hook.Content, testHarnessExecutable) {
		t.Fatalf("Cline hook does not pin the installing FKF executable:\n%s", hook.Content)
	}
}

func harnessFragmentFor(t *testing.T, base, harness, path, selector string) HarnessFragment {
	t.Helper()
	plan, err := HarnessPlanFor(base, harness, testHarnessExecutable)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range plan.Fragments {
		if fragment.Path == path && fragment.Selector == selector {
			return fragment
		}
	}
	t.Fatalf("%s plan has no fragment %s#%s", harness, path, selector)
	return HarnessFragment{}
}

func TestInstallHarnessesPreservesFixturesAndIsIdempotent(t *testing.T) {
	base := makeHarnessBase(t)
	for _, name := range HarnessNames() {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			plan, err := HarnessPlanFor(base, name, testHarnessExecutable)
			if err != nil {
				t.Fatal(err)
			}
			seedHarnessFixture(t, home, plan)

			report, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
				Names: []string{name}, Home: home, Executable: testHarnessExecutable,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !report.Complete || len(report.Changes) == 0 {
				t.Fatalf("first install report = %#v", report)
			}
			assertHarnessFragmentsInstalled(t, home, plan)

			again, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
				Names: []string{name}, Home: home, Executable: testHarnessExecutable,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !again.Complete || len(again.Changes) != 0 {
				t.Fatalf("idempotent install report = %#v", again)
			}
		})
	}
}

func TestInstallHarnessesPreservesSemanticallyCurrentJSONBytes(t *testing.T) {
	base := makeHarnessBase(t)
	home := t.TempDir()
	request := HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}
	if _, err := InstallHarnesses(t.Context(), base, request); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home, ".claude.json")
	installed, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(installed, &config); err != nil {
		t.Fatal(err)
	}
	config["unrelated"] = map[string]any{"keep": true}
	reformatted, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	reformatted = append(reformatted, '\n')
	if err := os.WriteFile(configPath, reformatted, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := InstallHarnesses(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Changes) != 0 {
		t.Fatalf("semantically current install report = %#v", report)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, reformatted) {
		t.Fatalf("semantically current install rewrote host JSON: got=%q err=%v", got, err)
	}
}

func TestInstallHarnessesAllCombinesSharedTargetsAndIsIdempotent(t *testing.T) {
	base := makeHarnessBase(t)
	home := t.TempDir()
	plans := make([]*HarnessPlan, 0, len(HarnessNames()))
	wantPaths := map[string]struct{}{}
	for _, name := range HarnessNames() {
		plan, err := HarnessPlanFor(base, name, testHarnessExecutable)
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
		seedHarnessFixture(t, home, plan)
		for _, fragment := range plan.Fragments {
			wantPaths[expandHarnessTestPath(t, home, fragment.Path)] = struct{}{}
		}
	}

	installed, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		All: true, Home: home, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Complete || !reflect.DeepEqual(installed.Harnesses, HarnessNames()) {
		t.Fatalf("all install report = %#v", installed)
	}
	if len(installed.Changes) != len(wantPaths) {
		t.Fatalf("all install changes = %d, want %d unique targets", len(installed.Changes), len(wantPaths))
	}
	seenChanges := map[string]bool{}
	for _, change := range installed.Changes {
		if _, ok := wantPaths[change.Path]; !ok {
			t.Fatalf("all install changed unexpected path %q", change.Path)
		}
		if seenChanges[change.Path] {
			t.Fatalf("all install changed shared target %q more than once", change.Path)
		}
		seenChanges[change.Path] = true
	}
	for _, plan := range plans {
		assertHarnessFragmentsInstalled(t, home, plan)
	}

	checked, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		All: true, Check: true, Home: home, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !checked.Complete || len(checked.Changes) != 0 {
		t.Fatalf("all check report = %#v", checked)
	}

	again, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		All: true, Home: home, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Complete || len(again.Changes) != 0 {
		t.Fatalf("idempotent all install report = %#v", again)
	}
}

func TestInstallHarnessesDryRunCheckRepairAndBackup(t *testing.T) {
	base := makeHarnessBase(t)
	home := t.TempDir()

	dry, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, DryRun: true, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Complete || len(dry.Changes) == 0 {
		t.Fatalf("dry-run report = %#v", dry)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("dry-run wrote home: entries=%v err=%v", entries, err)
	}

	if _, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".claude.json")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(string(before), base, filepath.Join(t.TempDir(), "other-base"), 1)
	if drifted == string(before) {
		t.Fatal("fixture did not contain base path")
	}
	if err := os.WriteFile(config, []byte(drifted), 0o600); err != nil {
		t.Fatal(err)
	}

	check, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Check: true, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete || len(check.Changes) == 0 {
		t.Fatalf("drift check report = %#v", check)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != drifted {
		t.Fatalf("check changed config: got=%q err=%v", got, err)
	}

	repaired, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.Complete {
		t.Fatalf("repair report = %#v", repaired)
	}
	backup, err := os.ReadFile(config + ".fkf.bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != drifted {
		t.Fatalf("backup = %q, want drifted bytes %q", backup, drifted)
	}
	if got, err := os.ReadFile(config); err != nil || !strings.Contains(string(got), base) {
		t.Fatalf("repair did not restore base: got=%q err=%v", got, err)
	}
}

func TestManagedHarnessJSONTreatsExtraKeysAsDriftAndPreservesSurroundingConfig(t *testing.T) {
	base := makeHarnessBase(t)
	home := t.TempDir()
	if _, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	servers := harnessJSONMap(t, config["mcpServers"], "mcpServers")
	managed := harnessJSONMap(t, servers["fkf"], "mcpServers.fkf")
	managed["disabled"] = true
	harnessJSONMap(t, managed["env"], "mcpServers.fkf.env")["FKF_BEHAVIOR_CHANGE"] = "1"
	servers["keep"] = map[string]any{"command": "other"}
	config["unrelated"] = map[string]any{"keep": true}
	drifted, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	drifted = append(drifted, '\n')
	if err := os.WriteFile(configPath, drifted, 0o600); err != nil {
		t.Fatal(err)
	}

	check, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Check: true, Executable: testHarnessExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete || len(check.Changes) != 1 {
		t.Fatalf("check report = %#v, want one drifted managed JSON file", check)
	}
	registrations, err := InspectHarnesses(t.Context(), base, home, testHarnessExecutable)
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if registration.Name == "claude" && (registration.Registered || registration.Changes != 1) {
			t.Fatalf("Claude status = %#v, want one drifted managed JSON file", registration)
		}
	}
	if current, err := os.ReadFile(configPath); err != nil || string(current) != string(drifted) {
		t.Fatalf("check/status changed config: got=%q err=%v", current, err)
	}

	if _, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = nil
	if err := json.Unmarshal(repaired, &config); err != nil {
		t.Fatal(err)
	}
	servers = harnessJSONMap(t, config["mcpServers"], "mcpServers")
	managed = harnessJSONMap(t, servers["fkf"], "mcpServers.fkf")
	if _, exists := managed["disabled"]; exists {
		t.Fatalf("repair retained behavior-changing managed key: %#v", managed)
	}
	if len(harnessJSONMap(t, managed["env"], "mcpServers.fkf.env")) != 0 {
		t.Fatalf("repair retained behavior-changing managed environment: %#v", managed)
	}
	if harnessJSONMap(t, servers["keep"], "mcpServers.keep")["command"] != "other" ||
		harnessJSONMap(t, config["unrelated"], "unrelated")["keep"] != true {
		t.Fatalf("repair changed permitted surrounding config: %#v", config)
	}
}

func harnessJSONMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON object", name, value)
	}
	return object
}

func TestInstallHarnessesRefusesUnmanagedConflictsBeforeWriting(t *testing.T) {
	base := makeHarnessBase(t)
	home := t.TempDir()
	conflict := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(conflict, []byte(`{"mcpServers":{"fkf":{"command":"not-fkf"}},"keep":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"claude", "codex"}, Home: home, Executable: testHarnessExecutable,
	})
	if !errors.Is(err, ErrHarnessConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(statErr) {
		t.Fatalf("preflight wrote another harness before conflict: %v", statErr)
	}
	if _, statErr := os.Stat(conflict + ".fkf.bak"); !os.IsNotExist(statErr) {
		t.Fatalf("conflict created a backup: %v", statErr)
	}
}

func TestInstallHarnessesValidatesSelectionAndCancellation(t *testing.T) {
	base := makeHarnessBase(t)
	tests := []HarnessInstallRequest{
		{Home: t.TempDir(), Executable: testHarnessExecutable},
		{Names: []string{"claude"}, All: true, Home: t.TempDir(), Executable: testHarnessExecutable},
		{Names: []string{"claude"}, DryRun: true, Check: true, Home: t.TempDir(), Executable: testHarnessExecutable},
		{Names: []string{"unknown"}, Home: t.TempDir(), Executable: testHarnessExecutable},
		{Names: []string{"claude"}, Home: "relative", Executable: testHarnessExecutable},
		{Names: []string{"claude"}, Home: t.TempDir(), Executable: "fkf"},
	}
	for _, request := range tests {
		if _, err := InstallHarnesses(t.Context(), base, request); err == nil {
			t.Fatalf("request %#v unexpectedly succeeded", request)
		}
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InstallHarnesses(canceled, base, HarnessInstallRequest{
		Names: []string{"claude"}, Home: t.TempDir(), Executable: testHarnessExecutable,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled install error = %v", err)
	}
}

func TestInstallHarnessesRefusesASymlinkedBaseHook(t *testing.T) {
	base := makeHarnessBase(t)
	hook := filepath.Join(base, "bin", "fkf-hook.sh")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "fkf-hook.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hook); err != nil {
		t.Fatal(err)
	}
	_, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"codex"}, Home: t.TempDir(), Executable: testHarnessExecutable,
	})
	if err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlinked harness hook error = %v, want explicit refusal", err)
	}
}

func TestInstallHarnessesRefusesASymlinkedBaseSkill(t *testing.T) {
	base := makeHarnessBase(t)
	skill := filepath.Join(base, ".agents", "skills", BundledSkills[0])
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "skill")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, skill); err != nil {
		t.Fatal(err)
	}
	_, err := InstallHarnesses(t.Context(), base, HarnessInstallRequest{
		Names: []string{"codex"}, Home: t.TempDir(), Executable: testHarnessExecutable,
	})
	if !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("symlinked harness skill error = %v, want unsafe-path refusal", err)
	}
}

func TestApplyHarnessFilesRefusesDriftAfterPreflight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte("{\"owner\":true}\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	mutation, err := preflightHarnessFile("claude", path, []HarnessFragment{
		jsonFragment("~/.claude.json", "mcpServers.fkf", map[string]any{
			"command": testHarnessExecutable,
			"args":    []any{"mcp", "serve", "--base", "/base"},
		}, false, "mcp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("{\"owner\":\"concurrent edit\"}\n")
	if err := os.WriteFile(path, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyHarnessFiles(t.Context(), []harnessFileMutation{mutation}); !errors.Is(err, ErrHarnessConflict) {
		t.Fatalf("apply drift error = %v, want harness conflict", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, concurrent) {
		t.Fatalf("config after drift refusal = %q, %v; want %q", got, err, concurrent)
	}
	if _, err := os.Lstat(path + harnessBackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drift created a stale backup: %v", err)
	}
}

func TestApplyHarnessLinksRefusesDriftAfterPreflight(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fkf-use")
	oldTarget := filepath.Join(root, "old", ".agents", "skills", "fkf-use")
	newTarget := filepath.Join(root, "new", ".agents", "skills", "fkf-use")
	if err := os.Symlink(oldTarget, path); err != nil {
		t.Fatal(err)
	}
	mutation := harnessLinkMutation{harness: "codex", path: path, after: newTarget}
	if err := preflightHarnessLink(&mutation); err != nil {
		t.Fatal(err)
	}
	concurrent := filepath.Join(root, "concurrent", ".agents", "skills", "fkf-use")
	if err := replaceSymlink(path, concurrent); err != nil {
		t.Fatal(err)
	}
	if err := applyHarnessLinks(t.Context(), []harnessLinkMutation{mutation}); !errors.Is(err, ErrHarnessConflict) {
		t.Fatalf("apply drift error = %v, want harness conflict", err)
	}
	if got, err := os.Readlink(path); err != nil || got != concurrent {
		t.Fatalf("link after drift refusal = %q, %v; want %q", got, err, concurrent)
	}
	if _, err := os.Lstat(path + harnessBackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drift created a stale link backup: %v", err)
	}
}

func makeHarnessBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base with spaces")
	directories := []string{filepath.Join(base, "bin")}
	for _, skill := range BundledSkills {
		directories = append(directories, filepath.Join(base, ".agents", "skills", skill))
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "bin", "fkf-hook.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return base
}

func seedHarnessFixture(t *testing.T, home string, plan *HarnessPlan) {
	t.Helper()
	seen := map[string]bool{}
	for _, fragment := range plan.Fragments {
		if seen[fragment.Path] {
			continue
		}
		seen[fragment.Path] = true
		var fixture string
		switch fragment.Kind {
		case HarnessFragmentJSON:
			fixture = filepath.Join("testdata", "harnesses", "unrelated.json")
		case HarnessFragmentTOML:
			fixture = filepath.Join("testdata", "harnesses", "unrelated.toml")
		default:
			continue
		}
		body, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		target := expandHarnessTestPath(t, home, fragment.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertHarnessFragmentsInstalled(t *testing.T, home string, plan *HarnessPlan) {
	t.Helper()
	seen := map[string]bool{}
	for _, fragment := range plan.Fragments {
		target := expandHarnessTestPath(t, home, fragment.Path)
		if fragment.Kind == HarnessFragmentLink {
			got, err := os.Readlink(target)
			if err != nil {
				t.Fatalf("read skill link %s: %v", target, err)
			}
			if got != fragment.Content {
				t.Fatalf("skill link %s = %q, want %q", target, got, fragment.Content)
			}
			continue
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read installed fragment %s: %v", target, err)
		}
		switch fragment.Kind {
		case HarnessFragmentJSON:
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatalf("decode installed JSON %s: %v", target, err)
			}
			if _, seeded := value["unrelated"]; seeded {
				if keep, _ := value["unrelated"].(map[string]any)["keep"].(bool); !keep {
					t.Fatalf("unrelated JSON changed in %s", target)
				}
			}
		case HarnessFragmentTOML:
			if !strings.Contains(string(body), `user_setting = "keep"`) {
				t.Fatalf("unrelated TOML changed in %s:\n%s", target, body)
			}
		}
	}
}

func expandHarnessTestPath(t *testing.T, home, path string) string {
	t.Helper()
	if !strings.HasPrefix(path, "~/") {
		t.Fatalf("fragment path is not home-relative: %q", path)
	}
	return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(path, "~/")))
}
