package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestHarnessCLIPrintsPlansChangesBackupsAndCurrentState(t *testing.T) {
	root := demoBase(t)

	listed := invoke(t, "--format", "text", "--base", root, "harness", "list")
	if listed.code != ExitSuccess {
		t.Fatalf("harness list = exit %d stdout %q stderr %q", listed.code, listed.stdout, listed.stderr)
	}
	for _, name := range []string{"claude", "codex", "gemini", "copilot", "antigravity", "opencode", "grok", "cursor", "kiro", "cline"} {
		if !strings.Contains(listed.stdout, name+"\n") {
			t.Errorf("harness list omits %q:\n%s", name, listed.stdout)
		}
	}

	printed := invoke(t, "--format", "text", "--base", root, "harness", "print", "claude")
	if printed.code != ExitSuccess {
		t.Fatalf("harness print = exit %d stdout %q stderr %q", printed.code, printed.stdout, printed.stderr)
	}
	for _, want := range []string{"# ~/.claude.json (json: mcpServers.fkf)", "mcp", "serve", root, "# ~/.claude/skills/daily-brief (link)"} {
		if !strings.Contains(printed.stdout, want) {
			t.Errorf("harness print omits %q:\n%s", want, printed.stdout)
		}
	}

	dryRun := invoke(t, "--format", "text", "--base", root, "harness", "install", "claude", "--dry-run")
	if dryRun.code != ExitSuccess || !strings.Contains(dryRun.stdout, "create ") || !strings.Contains(dryRun.stdout, "[claude]") {
		t.Fatalf("harness dry-run = exit %d stdout %q stderr %q", dryRun.code, dryRun.stdout, dryRun.stderr)
	}
	installed := invoke(t, "--format", "text", "--base", root, "harness", "install", "claude")
	if installed.code != ExitSuccess || !strings.Contains(installed.stdout, "[claude]") {
		t.Fatalf("harness install = exit %d stdout %q stderr %q", installed.code, installed.stdout, installed.stderr)
	}
	current := invoke(t, "--format", "text", "--base", root, "harness", "install", "claude", "--check")
	if current.code != ExitSuccess || current.stdout != "harness check: current\n" {
		t.Fatalf("harness check = exit %d stdout %q stderr %q", current.code, current.stdout, current.stderr)
	}

	configPath := filepath.Join(os.Getenv("HOME"), ".claude.json")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	otherBase := filepath.Join(t.TempDir(), "other-base")
	drifted := strings.Replace(string(config), root, otherBase, 1)
	if drifted == string(config) {
		t.Fatal("installed Claude config did not contain the selected base")
	}
	if err := os.WriteFile(configPath, []byte(drifted), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	repaired := invoke(t, "--format", "text", "--base", root, "harness", "install", "claude")
	if repaired.code != ExitSuccess || !strings.Contains(repaired.stdout, "backup ") ||
		!strings.Contains(repaired.stdout, configPath+".fkf.bak") {
		t.Fatalf("harness repair = exit %d stdout %q stderr %q", repaired.code, repaired.stdout, repaired.stderr)
	}
}

func TestScheduleCLITextDistinguishesMissingDryRunCurrentAndDrift(t *testing.T) {
	root := demoBase(t)
	installFakeScheduleManager(t)

	missing := invoke(t, "--format", "text", "--base", root, "schedule", "status")
	if missing.code != ExitSuccess || !strings.Contains(missing.stdout, ": missing\n") {
		t.Fatalf("missing schedule = exit %d stdout %q stderr %q", missing.code, missing.stdout, missing.stderr)
	}
	dryRun := invoke(t, "--format", "text", "--base", root, "schedule", "install", "--dry-run")
	if dryRun.code != ExitSuccess || !strings.Contains(dryRun.stdout, "schedule dry-run "+runtime.GOOS) ||
		!strings.Contains(dryRun.stdout, "missing:") {
		t.Fatalf("schedule dry-run = exit %d stdout %q stderr %q", dryRun.code, dryRun.stdout, dryRun.stderr)
	}
	installed := invoke(t, "--format", "text", "--base", root, "schedule", "install")
	if installed.code != ExitSuccess || !strings.Contains(installed.stdout, "current:") {
		t.Fatalf("schedule install = exit %d stdout %q stderr %q", installed.code, installed.stdout, installed.stderr)
	}
	current := invoke(t, "--format", "text", "--base", root, "schedule", "status")
	if current.code != ExitSuccess || !strings.Contains(current.stdout, ": current\n") {
		t.Fatalf("current schedule = exit %d stdout %q stderr %q", current.code, current.stdout, current.stderr)
	}

	unitDirectory := scheduleTestDirectory()
	entries, err := os.ReadDir(unitDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != scheduleTestFileCount() {
		t.Fatalf("schedule installed %d files, want %d on %s", len(entries), scheduleTestFileCount(), runtime.GOOS)
	}
	if err := os.WriteFile(filepath.Join(unitDirectory, entries[0].Name()), []byte("drifted\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	drifted := invoke(t, "--format", "text", "--base", root, "schedule", "status")
	if drifted.code != ExitSuccess || !strings.Contains(drifted.stdout, ": drifted\n") {
		t.Fatalf("drifted schedule = exit %d stdout %q stderr %q", drifted.code, drifted.stdout, drifted.stderr)
	}
}

func TestLearnCLITextReviewsAndRejectsTheActiveQueue(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	config := cliTestContract + `name: learn-text-cli
layers: {tasks: true, projects: true, wiki: true}
sources: {}
`
	writeLearnCLIFile(t, root, core.ConfigFileName, config)
	writeLearnCLIFile(t, root, "wiki/index.md", "# Wiki\n")
	writeLearnCLIFile(t, root, "wiki/log.md", "# Log\n")
	writeLearnCLIFile(t, root, "tasks/2026-09-02/session/TASKS.md",
		"# Session\n\n## Learned\n\n- Preserve an exact review boundary.\n")

	dryRun := invoke(t, "--format", "text", "--base", root, "learn", "propose", "--dry-run")
	if dryRun.code != ExitSuccess || !strings.Contains(dryRun.stdout, "Preserve an exact review boundary.") ||
		!strings.Contains(dryRun.stdout, "nothing written") {
		t.Fatalf("learn dry-run = exit %d stdout %q stderr %q", dryRun.code, dryRun.stdout, dryRun.stderr)
	}
	staged := invoke(t, "--format", "text", "--base", root, "learn", "propose")
	if staged.code != ExitSuccess || !strings.HasPrefix(staged.stdout, "staged ") {
		t.Fatalf("learn propose = exit %d stdout %q stderr %q", staged.code, staged.stdout, staged.stderr)
	}
	fields := strings.Fields(staged.stdout)
	if len(fields) < 2 {
		t.Fatalf("learn propose output has no proposal id: %q", staged.stdout)
	}
	id := fields[1]

	queue := invoke(t, "--format", "text", "--base", root, "learn", "review")
	if queue.code != ExitSuccess || !strings.Contains(queue.stdout, id) || !strings.Contains(queue.stdout, "wiki/log.md") {
		t.Fatalf("learn review queue = exit %d stdout %q stderr %q", queue.code, queue.stdout, queue.stderr)
	}
	diff := invoke(t, "--format", "text", "--base", root, "learn", "review", id, "--diff")
	if diff.code != ExitSuccess || !strings.Contains(diff.stdout, "+++ b/wiki/log.md") {
		t.Fatalf("learn review diff = exit %d stdout %q stderr %q", diff.code, diff.stdout, diff.stderr)
	}
	rejected := invoke(t, "--format", "text", "--base", root, "learn", "reject", id)
	if rejected.code != ExitSuccess || !strings.Contains(rejected.stdout, "rejected "+id) ||
		!strings.Contains(rejected.stdout, "files: wiki/log.md") {
		t.Fatalf("learn reject = exit %d stdout %q stderr %q", rejected.code, rejected.stdout, rejected.stderr)
	}
	repeated := invoke(t, "--format", "text", "--base", root, "learn", "reject", id)
	if repeated.code != ExitSuccess || !strings.Contains(repeated.stdout, "already-rejected "+id) {
		t.Fatalf("repeated learn reject = exit %d stdout %q stderr %q", repeated.code, repeated.stdout, repeated.stderr)
	}
	empty := invoke(t, "--format", "text", "--base", root, "learn", "review")
	if empty.code != ExitSuccess || empty.stdout != "no active learn proposals\n" {
		t.Fatalf("empty learn review = exit %d stdout %q stderr %q", empty.code, empty.stdout, empty.stderr)
	}
}

func TestWhoCLIResolvesSubstringAndReportsNoMatchOffline(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	config := cliTestContract + `name: who-cli
layers: {events: false, index: false, tasks: false, projects: false, wiki: true}
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime, actor:github.com/maxime]
    kind: person
sources: {}
`
	writeLearnCLIFile(t, root, core.ConfigFileName, config)
	writeLearnCLIFile(t, root, "wiki/index.md", "# Wiki\n\n[Maxime](maxime.md)\n")
	writeLearnCLIFile(t, root, "wiki/maxime.md", `---
type: person
title: Maxime Cordy
aliases: [actor:github.com/maxime]
---

# Maxime Cordy
`)
	if built := invoke(t, "--base", root, "build"); built.code != ExitSuccess {
		t.Fatalf("build identity graph = exit %d stdout %q stderr %q", built.code, built.stdout, built.stderr)
	}

	matched := invoke(t, "--format", "text", "--base", root, "who", "Cordy")
	if matched.code != ExitSuccess {
		t.Fatalf("who Cordy = exit %d stdout %q stderr %q", matched.code, matched.stdout, matched.stderr)
	}
	for _, want := range []string{"person:email/maxime@example.com [person]", "names: Maxime Cordy", "page: wiki/maxime.md", "total: 0 interaction(s)"} {
		if !strings.Contains(matched.stdout, want) {
			t.Errorf("who output omits %q:\n%s", want, matched.stdout)
		}
	}
	missing := invoke(t, "--format", "text", "--base", root, "who", "Nobody")
	if missing.code != ExitSuccess || missing.stdout != "no identity match for \"Nobody\"\n" {
		t.Fatalf("who Nobody = exit %d stdout %q stderr %q", missing.code, missing.stdout, missing.stderr)
	}
	encoded := invoke(t, "--format", "json", "--base", root, "who", "maxime")
	if encoded.code != ExitSuccess {
		t.Fatalf("who JSON = exit %d stdout %q stderr %q", encoded.code, encoded.stdout, encoded.stderr)
	}
	var report struct {
		Matches []json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal([]byte(encoded.stdout), &report); err != nil || len(report.Matches) != 1 {
		t.Fatalf("who JSON = %q, matches=%d error=%v", encoded.stdout, len(report.Matches), err)
	}
}

func TestValidateRecordsCLIReportsRepeatedTitlesInText(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	config := cliTestContract + `name: title-cli
layers: {events: true, index: false, tasks: false, projects: false, wiki: false}
sources:
  repeated:
    enabled: true
    layer: events
    run: [printf, '[{"id":"one","time":"{{start}}","title":"Same title"},{"id":"two","time":"{{start}}","title":"Same title"}]']
    fields: {id: .id, time: .time, title: .title}
`
	writeLearnCLIFile(t, root, core.ConfigFileName, config)
	if trusted := invoke(t, "--base", root, "trust"); trusted.code != ExitSuccess {
		t.Fatalf("trust title source = exit %d stdout %q stderr %q", trusted.code, trusted.stdout, trusted.stderr)
	}
	if synced := invoke(t, "--base", root, "sync", "--days", "1"); synced.code != ExitSuccess {
		t.Fatalf("sync title source = exit %d stdout %q stderr %q", synced.code, synced.stdout, synced.stderr)
	}

	warning := invoke(t, "--format", "text", "--base", root, "validate", "records")
	if warning.code != ExitSuccess || !strings.Contains(warning.stdout, "warning") ||
		!strings.Contains(warning.stdout, "Same title") {
		t.Fatalf("validate records = exit %d stdout %q stderr %q", warning.code, warning.stdout, warning.stderr)
	}
	strict := invoke(t, "--format", "text", "--base", root, "validate", "records", "--strict")
	if strict.code != ExitPartial || !strings.Contains(strict.stdout, "error") ||
		!strings.Contains(strict.stderr, "record-title validation found 1 error(s)") {
		t.Fatalf("validate records --strict = exit %d stdout %q stderr %q", strict.code, strict.stdout, strict.stderr)
	}
}
