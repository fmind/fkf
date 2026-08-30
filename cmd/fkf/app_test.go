package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestExitCodeDistinguishesCancellationFromAnOperationalTimeout(t *testing.T) {
	if got := exitCodeFor(context.Canceled); got != ExitCanceled {
		t.Fatalf("cancellation exit = %d, want %d", got, ExitCanceled)
	}
	if got := exitCodeFor(context.DeadlineExceeded); got != ExitPartial {
		t.Fatalf("internal timeout exit = %d, want operational failure %d", got, ExitPartial)
	}
	if got := exitCodeFor(services.ErrContextBudgetTooSmall); got != ExitInvalidUsage {
		t.Fatalf("undersized context budget exit = %d, want usage failure %d", got, ExitInvalidUsage)
	}
}

// The CLI tests drive the real binary's command table against a real temporary base, so they
// cover the wiring — flags, exit codes, output formats — that the service tests deliberately do
// not. They are hermetic: HOME and XDG_STATE_HOME are redirected and FKF_BASE is unset.

func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("FKF_BASE", "")
}

const cliTestContract = `fkf: 1
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
  url: {description: Provider URI., cardinality: optional, relation: true}
  repo: {description: Provider repository argument., cardinality: optional}
  participant: {description: Canonical participant URIs., cardinality: many, relation: true}
`

func withCLITestContract(config string) string {
	if strings.HasPrefix(strings.TrimSpace(config), "fkf:") {
		return config
	}
	return cliTestContract + config
}

type result struct {
	code   int
	stdout string
	stderr string
}

func invoke(t *testing.T, args ...string) result {
	t.Helper()
	return invokeContext(t, t.Context(), args...)
}

func invokeContext(t *testing.T, ctx context.Context, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(ctx, append([]string{"fkf"}, args...), &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestContextWriterStopsEveryOutputFormatAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var output bytes.Buffer
	writer := contextWriter{ctx: ctx, writer: &output}
	if _, err := writer.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := writer.Write([]byte("after")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write after cancellation = %v, want context.Canceled", err)
	}
	if output.String() != "before" {
		t.Fatalf("output after cancellation = %q, want only the completed write", output.String())
	}
}

// TestCLIErrorOutputNeverRendersProviderStderr pins the last propagation boundary. The core
// command failure deliberately retains stderr for retry matching, but the only representation
// the CLI may receive is its safe status diagnostic.
func TestCLIErrorOutputNeverRendersProviderStderr(t *testing.T) {
	const privateStderr = "synthetic-private-provider-cli-response"
	failure := core.NewCommandFailure(errors.New("synthetic provider failure"), privateStderr)
	var stderr bytes.Buffer
	writeCLIError(&stderr, failure)

	diagnostic := stderr.String()
	if strings.Contains(diagnostic, privateStderr) {
		t.Fatalf("CLI leaked provider stderr: %q", diagnostic)
	}
	if !strings.Contains(diagnostic, "fkf: command execution failed") {
		t.Fatalf("CLI diagnostic = %q, want the safe command-failure class", diagnostic)
	}
}

func TestSourceTestCommandSelectsHooksTimesOutAndKeepsStderrPrivate(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	tests := filepath.Join(root, core.BaseTestsDir)
	if err := os.MkdirAll(tests, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"active-check.sh":  "#!/bin/sh\nset -eu\ntest \"$1\" = \"" + root + "\"\n",
		"dormant-check.sh": "#!/bin/sh\nset -eu\necho private-provider-response >&2\nexit 7\n",
		"slow-check.sh":    "#!/bin/sh\nset -eu\nsleep 2\n",
	} {
		if err := os.WriteFile(filepath.Join(tests, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := cliTestContract + `name: source-test-cli
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sync: {timeout: 1s}
sources:
  active:
    enabled: true
    layer: index
    run: [printf, "[]"]
    test: [active-check.sh, "{{base}}"]
    fields: {id: .id}
  dormant:
    enabled: false
    layer: index
    run: [printf, "[]"]
    test: [dormant-check.sh]
    fields: {id: .id}
  slow:
    enabled: false
    layer: index
    run: [printf, "[]"]
    test: [slow-check.sh]
    fields: {id: .id}
`
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "trust"); got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}

	defaultRun := invoke(t, "--format", "json", "--base", root, "test")
	if defaultRun.code != ExitSuccess {
		t.Fatalf("default test exited %d: %s%s", defaultRun.code, defaultRun.stdout, defaultRun.stderr)
	}
	var report services.SourceTestReport
	if err := json.Unmarshal([]byte(defaultRun.stdout), &report); err != nil {
		t.Fatalf("decode test report: %v\n%s", err, defaultRun.stdout)
	}
	if report.Passed != 1 || report.Failed != 0 || len(report.Sources) != 1 || report.Sources[0].Source != "active" {
		t.Fatalf("default report = %+v, want only the enabled hook", report)
	}
	jsonl := invoke(t, "--format", "jsonl", "--base", root, "test")
	if jsonl.code != ExitSuccess || strings.Count(strings.TrimSpace(jsonl.stdout), "\n") != 0 ||
		!strings.Contains(jsonl.stdout, `"source":"active"`) {
		t.Fatalf("JSONL source tests = exit %d stdout %q stderr %q, want one result line", jsonl.code, jsonl.stdout, jsonl.stderr)
	}

	all := invoke(t, "--format", "json", "--base", root, "test", "--all")
	if all.code != ExitPartial {
		t.Fatalf("test --all exited %d, want %d: %s%s", all.code, ExitPartial, all.stdout, all.stderr)
	}
	if strings.Contains(all.stdout+all.stderr, "private-provider-response") {
		t.Fatalf("source test leaked provider stderr: %s%s", all.stdout, all.stderr)
	}
	for _, want := range []string{"dormant-check.sh", "command exited with status 7", "slow-check.sh", "command timed out"} {
		if !strings.Contains(all.stdout+all.stderr, want) {
			t.Fatalf("test --all output omits %q: %s%s", want, all.stdout, all.stderr)
		}
	}

	invalid := invoke(t, "--base", root, "test", "--all", "active")
	if invalid.code != ExitInvalidUsage {
		t.Fatalf("test --all active exited %d, want %d: %s%s", invalid.code, ExitInvalidUsage, invalid.stdout, invalid.stderr)
	}
}

// demoBase builds a small deterministic base through the CLI itself, which is also the check
// that `init --demo` works end to end.
func demoBase(t *testing.T) string {
	t.Helper()
	isolate(t)
	root := filepath.Join(t.TempDir(), "demo")
	if got := invoke(t, "init", root, "--demo", "4"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	return root
}

func TestCLIWriterLockExcludesMutationsButNeverReaders(t *testing.T) {
	root := demoBase(t)
	configPath := filepath.Join(root, core.ConfigFileName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = []byte(strings.Replace(string(config), "sources: {}", `sources:
  synthetic-cli:
    enabled: true
    layer: events
    run: [printf, "[]"]
    fields: {id: .id, time: .time}
`, 1))
	if err := os.WriteFile(configPath, config, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "trust"); got.code != ExitSuccess {
		t.Fatalf("trust before lock exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "build"); got.code != ExitSuccess {
		t.Fatalf("build before lock exited %d: %s%s", got.code, got.stdout, got.stderr)
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

	readers := [][]string{
		{"--base", root, "list", "wiki"},
		{"--base", root, "read", "wiki/index.md"},
		{"--base", root, "find", "demo", "--limit", "1"},
		{"--base", root, "graph"},
		{"--base", root, "status"},
		{"--base", root, "trust", "--check"},
		{"--base", root, "build", "wiki", "--check"},
		{"--base", root, "sync", "synthetic-cli", "--dry-run", "--days", "1"},
		{"--base", root, "sync", "synthetic-cli", "--preview"},
		{"--base", root, "config", "helpers"},
	}
	for _, args := range readers {
		if got := invoke(t, args...); got.code != ExitSuccess {
			t.Errorf("reader %v exited %d while writer lock was held: %s%s", args, got.code, got.stdout, got.stderr)
		}
	}

	writers := [][]string{
		{"init", root},
		{"--base", root, "trust"},
		{"--base", root, "sync", "synthetic-cli", "--days", "1"},
		{"--base", root, "build"},
		{"--base", root, "new", "task", "locked-task"},
		{"--base", root, "config", "helpers", "--refresh"},
	}
	for _, args := range writers {
		got := invoke(t, args...)
		if got.code != ExitPartial || !strings.Contains(got.stderr, core.ErrBaseBusy.Error()) {
			t.Errorf("writer %v = exit %d, stdout %q, stderr %q; want fail-fast busy exit", args, got.code, got.stdout, got.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(root, string(core.LayerTasks), time.Now().Format(time.DateOnly), "locked-task", core.TaskTraceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy `new task` mutated the base: %v", err)
	}
}

func TestInitRejectsInvalidDemoDaysAsUsageWithoutCreatingTheTarget(t *testing.T) {
	isolate(t)
	for _, days := range []string{"-1", "367"} {
		t.Run(days, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			got := invoke(t, "init", root, "--demo", days)
			if got.code != ExitInvalidUsage {
				t.Fatalf("init --demo %s exit = %d, want %d: %s", days, got.code, ExitInvalidUsage, got.stderr)
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid --demo created target %s: %v", root, err)
			}
		})
	}
}

func TestInitRejectsDemoWithAnExplicitRealPreset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mixed")
	got := invoke(t, "init", root, "--preset", "personal", "--demo", "1")
	if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "--demo uses the minimal configuration") {
		t.Fatalf("init personal --demo exit = %d stderr=%q, want an invalid-usage refusal", got.code, got.stderr)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused mixed demo created %s: %v", root, err)
	}
}

func TestCLIRejectsNumericFlagsOutsideTheirPublishedDomains(t *testing.T) {
	root := demoBase(t)
	tooOld := strconv.Itoa(core.MaxFreshnessAgeHours + 1)
	tests := []struct {
		name string
		args []string
		flag string
	}{
		{name: "context budget", args: []string{"context", "retrieval", "--budget", "-1"}, flag: "--budget"},
		{name: "find limit", args: []string{"find", "--limit", "-1"}, flag: "--limit"},
		{name: "events limit", args: []string{"list", "events", "--limit", "-1"}, flag: "--limit"},
		{name: "tasks limit", args: []string{"list", "tasks", "--limit", "-1"}, flag: "--limit"},
		{name: "projects limit", args: []string{"list", "projects", "--limit", "-1"}, flag: "--limit"},
		{name: "wiki limit", args: []string{"list", "wiki", "--limit", "-1"}, flag: "--limit"},
		{name: "read limit", args: []string{"read", "wiki/index.md", "--limit", "-1"}, flag: "--limit"},
		{name: "graph negative depth", args: []string{"graph", "wiki/index.md", "--depth", "-1"}, flag: "--depth"},
		{name: "graph zero depth", args: []string{"graph", "wiki/index.md", "--depth", "0"}, flag: "--depth"},
		{name: "graph excessive depth", args: []string{"graph", "wiki/index.md", "--depth", "4"}, flag: "--depth"},
		{name: "graph edge limit", args: []string{"graph", "wiki/index.md", "--limit", "-1"}, flag: "--limit"},
		{name: "graph node limit", args: []string{"graph", "nodes", "--limit", "-1"}, flag: "--limit"},
		{name: "graph entity limit", args: []string{"graph", "topic:retrieval", "--limit", "-1"}, flag: "--limit"},
		{name: "status negative age", args: []string{"status", "--max-age-hours", "-1"}, flag: "--max-age-hours"},
		{name: "status excessive age", args: []string{"status", "--max-age-hours", tooOld}, flag: "--max-age-hours"},
		{name: "sync negative days", args: []string{"sync", "--days", "-1"}, flag: "--days"},
		{name: "sync excessive days", args: []string{"sync", "--days", "367"}, flag: "--days"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--base", root}, test.args...)
			got := invoke(t, args...)
			if got.code != ExitInvalidUsage {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", got.code, ExitInvalidUsage, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Fatalf("invalid %s wrote stdout: %q", test.flag, got.stdout)
			}
			if !strings.Contains(got.stderr, test.flag) {
				t.Fatalf("stderr = %q, want the invalid flag %s", got.stderr, test.flag)
			}
		})
	}
}

func TestCancellationStopsEveryLongOfflineCLISurfaceWithExit130(t *testing.T) {
	root := demoBase(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "context", args: []string{"--base", root, "context", "retrieval"}},
		{name: "find", args: []string{"--base", root, "find", "retrieval"}},
		{name: "graph summary", args: []string{"--base", root, "graph"}},
		{name: "graph nodes", args: []string{"--base", root, "graph", "nodes"}},
		{name: "layer listing", args: []string{"--base", root, "list", "events"}},
		{name: "trust digest", args: []string{"--base", root, "trust", "--check"}},
		{name: "mcp instructions", args: []string{"--base", root, "mcp", "instructions"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := invokeContext(t, ctx, testCase.args...)
			if got.code != ExitCanceled {
				t.Fatalf("cancelled command exited %d, want %d; stdout=%q stderr=%q", got.code, ExitCanceled, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Fatalf("cancelled command wrote stdout: %q", got.stdout)
			}
		})
	}
}

// TestRelativeBasePathsWorkAtEveryCLIEntryPoint exercises the user-visible regression: a
// relative init path used to put `brain/bin` on PATH and then run commands from Dir=brain, so
// the shell looked in brain/brain/bin and every shipped helper disappeared.
func TestRelativeBasePathsWorkAtEveryCLIEntryPoint(t *testing.T) {
	isolate(t)
	parent := t.TempDir()
	t.Chdir(parent)
	want := filepath.Join(parent, "brain")

	created := invoke(t, "--format", "json", "init", "brain", "--preset", "minimal")
	if created.code != ExitSuccess {
		t.Fatalf("relative init exited %d: %s%s", created.code, created.stdout, created.stderr)
	}
	var report services.InitReport
	if err := json.Unmarshal([]byte(created.stdout), &report); err != nil {
		t.Fatalf("decode init report: %v\n%s", err, created.stdout)
	}
	if report.Base != want {
		t.Fatalf("init base = %q, want absolute %q", report.Base, want)
	}

	// Keep this CLI wiring test independent of every platform-specific preset helper. The tiny
	// fake proves that a command installed under a relative base's bin/ is resolved from PATH,
	// while emitting one deterministic record at the exact start of fkf's requested window.
	config := []byte(withCLITestContract(`name: brain
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  relative-path-probe:
    enabled: true
    layer: events
    run: [fkf-test-source, "{{start}}", "{{end}}"]
    fields:
      id: .id
      time: .time
`))
	if err := core.WriteFileAtomicMode(filepath.Join(want, core.ConfigFileName), config, core.BaseFileMode); err != nil {
		t.Fatalf("write synthetic config: %v", err)
	}
	helper := []byte("#!/bin/sh\nset -eu\nprintf '[{\"id\":\"synthetic\",\"time\":\"%s\"}]\\n' \"$1\"\n")
	if err := core.WriteFileAtomicMode(filepath.Join(want, core.BaseBinDir, "fkf-test-source"), helper, 0o755); err != nil {
		t.Fatalf("write synthetic helper: %v", err)
	}
	trusted := invoke(t, "--base", "brain", "trust", "--all")
	if trusted.code != ExitSuccess {
		t.Fatalf("trust synthetic relative base exited %d: %s%s", trusted.code, trusted.stdout, trusted.stderr)
	}

	byFlag := invoke(t, "--base", "brain", "sync", "--days", "1", "relative-path-probe")
	if byFlag.code != ExitSuccess {
		t.Fatalf("relative --base sync exited %d: %s%s", byFlag.code, byFlag.stdout, byFlag.stderr)
	}
	t.Setenv(core.BaseEnvVar, "brain")
	byEnvironment := invoke(t, "config")
	if byEnvironment.code != ExitSuccess {
		t.Fatalf("relative %s config exited %d: %s%s", core.BaseEnvVar,
			byEnvironment.code, byEnvironment.stdout, byEnvironment.stderr)
	}
}

func TestCommandTableIsTheDocumentedSurface(t *testing.T) {
	app := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	names := make([]string, 0, len(app.Commands))
	var aliases []string
	for _, command := range app.Commands {
		names = append(names, command.Name)
		aliases = append(aliases, command.Aliases...)
		if command.Usage == "" {
			t.Fatalf("command %s has no usage line", command.Name)
		}
		if command.Category == "" {
			t.Fatalf("command %s has no category; the help groups are what makes it findable", command.Name)
		}
	}
	want := []string{
		"context", "find", "read", "graph", "list", "validate", "tags",
		"init", "trust", "test", "sync", "status", "build", "new", "config", "mcp", "upgrade",
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("commands = %v, want %v", names, want)
	}
	// Single-letter aliases must stay unique, or one of them silently stops working.
	seen := map[string]string{}
	for _, alias := range aliases {
		if previous, clash := seen[alias]; clash {
			t.Fatalf("alias %q is claimed by both %s and something else", alias, previous)
		}
		seen[alias] = alias
	}
}

func TestEveryWindowFlagAdvertisesTheSharedGrammar(t *testing.T) {
	app := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	seen := 0
	var visit func(commands []*cli.Command)
	visit = func(commands []*cli.Command) {
		for _, command := range commands {
			for _, flag := range command.Flags {
				window, ok := flag.(*cli.StringFlag)
				if !ok || (window.Name != "since" && window.Name != "until") {
					continue
				}
				seen++
				for _, form := range []string{"YYYY-MM-DD", "today", "yesterday", "7d"} {
					if !strings.Contains(window.Usage, form) {
						t.Fatalf("%s --%s help = %q, want shared window form %q", command.Name, window.Name, window.Usage, form)
					}
				}
			}
			visit(command.Commands)
		}
	}
	visit(app.Commands)
	if seen == 0 {
		t.Fatal("command surface has no window flags")
	}
}

// The alias convention is stated in the root help, so it has to hold: one letter, the command's
// own first letter, and the five that lost a collision are typed in full.
func TestAliasesAreTheFirstLetterOrNothing(t *testing.T) {
	app := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	spelled := []string{"init", "trust", "test", "status", "config"}
	for _, command := range app.Commands {
		if slices.Contains(spelled, command.Name) {
			if len(command.Aliases) != 0 {
				t.Fatalf("%s lost its letter to another command and must be typed in full", command.Name)
			}
			continue
		}
		if len(command.Aliases) != 1 || command.Aliases[0] != command.Name[:1] {
			t.Fatalf("%s has aliases %v, want exactly its own first letter", command.Name, command.Aliases)
		}
	}
}

// Every layer command IS its own listing, and every subcommand name is drawn from one shared
// vocabulary, so learning one layer teaches the rest.
func TestLayerCommandsShareOneVocabulary(t *testing.T) {
	app := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	vocabulary := map[string]string{
		"events": "e", "index": "i", "tasks": "t", "projects": "p", "wiki": "w",
		"read": "r", "search": "s", "tags": "t", "validate": "v",
		"nodes": "n", "build": "b", "serve": "s", "instructions": "i", "schema": "s",
		"helpers": "h", "learned": "l", "task": "t", "project": "p", "graph": "g",
		"helper": "h",
	}
	for _, command := range app.Commands {
		if command.Action == nil && command.Name != "mcp" && command.Name != "new" && command.Name != "list" {
			t.Fatalf("%s has no action of its own; the bare command must do the obvious thing", command.Name)
		}
		for _, sub := range command.Commands {
			letter, known := vocabulary[sub.Name]
			if !known {
				t.Fatalf("%s %s is not in the shared subcommand vocabulary", command.Name, sub.Name)
			}
			if len(sub.Aliases) != 1 || sub.Aliases[0] != letter {
				t.Fatalf("%s %s has aliases %v, want [%s]", command.Name, sub.Name, sub.Aliases, letter)
			}
		}
	}
}

func TestExitCodesAreStable(t *testing.T) {
	root := demoBase(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"a successful read", []string{"--base", root, "find", "--count"}, ExitSuccess},
		{"an unknown command", []string{"frobnicate"}, ExitInvalidUsage},
		{"an unknown flag", []string{"--base", root, "find", "--nope"}, ExitInvalidUsage},
		{"an invalid format", []string{"--format", "yaml", "--base", root, "find"}, ExitInvalidUsage},
		{"a base that does not exist", []string{"--base", "/nonexistent/base", "find"}, ExitInvalidUsage},
		{"a slug where a listing takes none", []string{"--base", root, "list", "wiki", "retrieval-boundary"}, ExitInvalidUsage},
		{"a context with no terms", []string{"--base", root, "context"}, ExitInvalidUsage},
		{"a URI that escapes the base", []string{"--base", root, "read", "../../etc/passwd"}, ExitInvalidUsage},
		{"a missing argument", []string{"--base", root, "read"}, ExitInvalidUsage},
		{"a file that is not there", []string{"--base", root, "read", "events/2020-01-01/x.json"}, ExitPartial},
		// A base with nothing enabled is a configuration problem, reported before the trust
		// gate is even consulted. The untrusted case has its own test below.
		{"a base with no enabled source", []string{"--base", root, "sync"}, ExitInvalidUsage},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := invoke(t, test.args...); got.code != test.want {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", got.code, test.want, got.stdout, got.stderr)
			}
		})
	}
}

// TestNoBaseNamesAllThreeRules is what a first-time user hits, so the message has to be the
// whole answer rather than a refusal.
func TestNoBaseNamesAllThreeRules(t *testing.T) {
	isolate(t)
	t.Chdir(t.TempDir())
	got := invoke(t, "find")
	if got.code != ExitInvalidUsage {
		t.Fatalf("exit = %d, want %d", got.code, ExitInvalidUsage)
	}
	for _, want := range []string{"--base", "FKF_BASE", "fkf.yaml"} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", got.stderr, want)
		}
	}
}

func TestOutputFormats(t *testing.T) {
	root := demoBase(t)
	t.Run("json is the default away from a terminal", func(t *testing.T) {
		got := invoke(t, "--base", root, "find", "--limit", "3")
		if got.code != ExitSuccess {
			t.Fatalf("exit = %d: %s", got.code, got.stderr)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &decoded); err != nil {
			t.Fatalf("stdout is not JSON: %v", err)
		}
	})
	t.Run("jsonl streams the records, not the envelope", func(t *testing.T) {
		got := invoke(t, "--format", "jsonl", "--base", root, "find", "--limit", "3")
		lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
		if len(lines) != 3 {
			t.Fatalf("got %d line(s), want one per record:\n%s", len(lines), got.stdout)
		}
		for _, line := range lines {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("line is not JSON: %v", err)
			}
			if record["uri"] == nil {
				t.Fatalf("record = %v, want every line stamped with its uri", record)
			}
		}
	})
	t.Run("text renders, and says so when it cannot", func(t *testing.T) {
		got := invoke(t, "--format", "text", "--base", root, "find", "--count")
		if got.code != ExitSuccess || !strings.Contains(got.stdout, "day(s)") {
			t.Fatalf("text output = %q (%d)", got.stdout, got.code)
		}
	})
}

func TestConfigSchemaNeedsNoBase(t *testing.T) {
	isolate(t)
	t.Chdir(t.TempDir())
	got := invoke(t, "config", "schema")
	if got.code != ExitSuccess {
		t.Fatalf("exit = %d: %s", got.code, got.stderr)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &schema); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if schema["$id"] == nil {
		t.Fatalf("schema = %v, want a published $id", schema)
	}
}

// TestValidateExitsNonZeroOnAnError is what lets a skill's final step and a pre-commit hook
// depend on the code rather than on parsing the output.
func TestValidateExitsNonZeroOnAnError(t *testing.T) {
	root := demoBase(t)
	if got := invoke(t, "--base", root, "validate", "wiki"); got.code != ExitSuccess {
		t.Fatalf("the demo wiki must validate cleanly, exit = %d: %s", got.code, got.stdout)
	}
	broken := filepath.Join(root, "wiki", "broken.md")
	if err := os.WriteFile(broken, []byte("---\ntype: decision\ntitle: B\ntags: [x]\n---\n\n# B\n\n[out](../../../etc/passwd)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "validate", "wiki"); got.code != ExitPartial {
		t.Fatalf("exit = %d, want a non-zero code for a page that escapes the base", got.code)
	}
	// --strict promotes the warnings too: an untagged concept is unreachable in a flat layer.
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	untagged := filepath.Join(root, "wiki", "untagged.md")
	if err := os.WriteFile(untagged, []byte("---\ntype: insight\ntitle: U\n---\n\n# U\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "--base", root, "validate", "wiki"); got.code != ExitSuccess {
		t.Fatalf("exit = %d, want an untagged page to be a warning by default", got.code)
	}
	if got := invoke(t, "--base", root, "validate", "wiki", "--strict"); got.code != ExitPartial {
		t.Fatalf("exit = %d, want --strict to promote the warning to an error", got.code)
	}
}

func TestSyncDryRunNeedsNoTrustAndRunsNothing(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if got := invoke(t, "init", root, "--preset", "minimal"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s", got.code, got.stderr)
	}
	config := filepath.Join(root, "fkf.yaml")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	source := "sources:\n  synthetic:\n    enabled: true\n    layer: events\n" +
		"    run: [printf, \"[]\"]\n    fields:\n      id: .id\n      time: .t\n"
	if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sources: {}", source, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Editing the file re-arms the trust gate, and --dry-run still works because it executes
	// nothing at all.
	got := invoke(t, "--format", "text", "--base", root, "sync", "--days", "2", "--dry-run")
	if got.code != ExitSuccess {
		t.Fatalf("exit = %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if !strings.Contains(got.stdout, "planned") || !strings.Contains(got.stdout, "printf '[]'") {
		t.Fatalf("stdout = %q, want the substituted command listed as planned", got.stdout)
	}
	// Without --dry-run the same command is refused, and the refusal names the remedy.
	refused := invoke(t, "--base", root, "sync", "--days", "2")
	if refused.code != ExitUntrusted || !strings.Contains(refused.stderr, "fkf trust") {
		t.Fatalf("exit = %d, stderr = %q", refused.code, refused.stderr)
	}
	// Trusting prints the commands, then records them.
	trusted := invoke(t, "--format", "text", "--base", root, "trust")
	if trusted.code != ExitSuccess || !strings.Contains(trusted.stdout, `run:  ["printf", "[]"]`) {
		t.Fatalf("trust = %+v", trusted)
	}
	if after := invoke(t, "--base", root, "sync", "--days", "2"); after.code != ExitSuccess {
		t.Fatalf("exit = %d after trusting: %s", after.code, after.stderr)
	}
}

func TestMCPServeRequiresItsOwnBaseFlag(t *testing.T) {
	root := demoBase(t)
	// The flag is required on purpose: an MCP launch line must always say which base it exposes,
	// so it cannot silently follow FKF_BASE or the working directory.
	t.Setenv("FKF_BASE", root)
	if got := invoke(t, "mcp", "serve"); got.code != ExitInvalidUsage {
		t.Fatalf("exit = %d, want --base demanded even with the environment set", got.code)
	}
	command := newApp(&bytes.Buffer{}, &bytes.Buffer{}).Command("mcp").Command("serve")
	found := false
	for _, flag := range command.Flags {
		if base, ok := flag.(*cli.StringFlag); ok && base.Name == "base" {
			found = base.Required && base.Local
		}
	}
	if !found {
		t.Fatal("mcp serve must declare its own required, local --base flag")
	}
}

func TestVersionAndHelp(t *testing.T) {
	isolate(t)
	if got := invoke(t, "--version"); got.code != ExitSuccess || !strings.Contains(got.stdout, "fkf version") {
		t.Fatalf("--version = %+v", got)
	}
	got := invoke(t, "--help")
	if got.code != ExitSuccess {
		t.Fatalf("--help exited %d", got.code)
	}
	for _, group := range []string{groupAsk, groupInspect, groupRun} {
		if !strings.Contains(got.stdout, group) {
			t.Fatalf("--help omits the %q group; the grouping is what makes the surface readable", group)
		}
	}
	for _, guidance := range []string{
		"fkf init ~/brain --demo 30", "fkf status", "fkf sync --days 7",
		"fkf context", "fkf read", "fkf <command> --help",
	} {
		if !strings.Contains(got.stdout, guidance) {
			t.Fatalf("--help omits first-use guidance %q", guidance)
		}
	}
	// Grouping by purpose only works if the commands that write or execute still say so.
	for _, mark := range []string{markWrite, markRun} {
		if !strings.Contains(got.stdout, strings.TrimSpace(mark)) {
			t.Fatalf("--help omits %q; the trust boundary moved to the leaf, it did not disappear", mark)
		}
	}
	initHelp := invoke(t, "init", "--help")
	for _, contract := range []string{"records trust only", "no execution input predated init", "agent bridges", "preserves"} {
		if !strings.Contains(initHelp.stdout, contract) {
			t.Fatalf("init help omits %q from its refresh/trust contract: %s", contract, initHelp.stdout)
		}
	}
}

func TestExecutionBoundaryHelpIsComplete(t *testing.T) {
	app := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	syncCommand := app.Command("sync")
	for _, marker := range []string{markWrite, markRun} {
		if !strings.Contains(syncCommand.Usage, marker) {
			t.Fatalf("sync usage = %q, want boundary marker %q", syncCommand.Usage, marker)
		}
	}

	trustCommand := app.Command("trust")
	for _, term := range []string{"run:", "test:", "body:", "policy", "bin:", "helper"} {
		if !strings.Contains(trustCommand.Usage, term) {
			t.Fatalf("trust usage = %q, want complete disclosure term %q", trustCommand.Usage, term)
		}
	}
	for _, flag := range trustCommand.Flags {
		all, ok := flag.(*cli.BoolFlag)
		if !ok || all.Name != "all" {
			continue
		}
		for _, term := range []string{"complete", "bin:", "command", "policy", "helper"} {
			if !strings.Contains(all.Usage, term) {
				t.Fatalf("trust --all usage = %q, want complete disclosure term %q", all.Usage, term)
			}
		}
		return
	}
	t.Fatal("trust command has no --all flag")
}

func TestReadResolvesFromInsideTheBase(t *testing.T) {
	root := demoBase(t)
	// Walking up from a subdirectory is the third discovery rule, and the one a user relies on.
	t.Chdir(filepath.Join(root, "wiki"))
	t.Setenv("FKF_BASE", "")
	got := invoke(t, "--format", "text", "read", "wiki/index.md")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, "# Wiki") {
		t.Fatalf("read from inside the base = %+v", got)
	}
}

// TestFindLimitZeroMeansNoLimit pins the semantic the help promises. urfave/cli gives an unset
// IntFlag the same zero an explicit `--limit 0` has, so the service cannot distinguish an
// explicit escape hatch from an omitted flag. IsSet is the only place that distinction exists.
func TestFindLimitZeroMeansNoLimit(t *testing.T) {
	root := demoBase(t)
	read := func(args ...string) (int, bool) {
		t.Helper()
		got := invoke(t, append([]string{"find", "--base", root, "--since", "30d", "--format", "json"}, args...)...)
		if got.code != ExitSuccess {
			t.Fatalf("find exited %d: %s%s", got.code, got.stdout, got.stderr)
		}
		var payload struct {
			Records   []struct{ URI string } `json:"records"`
			Truncated bool                   `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(payload.Records), payload.Truncated
	}
	all, truncated := read("--limit", "0")
	if truncated {
		t.Error("--limit 0 reported a truncated result; the help calls it no limit")
	}
	capped, _ := read("--limit", "3")
	if capped != 3 {
		t.Fatalf("--limit 3 returned %d record(s), want 3", capped)
	}
	if all <= capped {
		t.Fatalf("--limit 0 returned %d record(s), no more than --limit 3 returned (%d)", all, capped)
	}
}

// TestTrustListsWhatAppliesToEveryCommand pins the half of the gate a reviewer could not see.
// An extra PATH directory applies to every declared command without appearing in any run line,
// so trust must disclose it explicitly.
func TestTrustListsWhatAppliesToEveryCommand(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "base")
	toolDir := t.TempDir()
	if got := invoke(t, "init", root, "--preset", "minimal"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	config := "name: t\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\n" +
		"sync: {days: 7, index_max_age_hours: 24, timeout: 1s, concurrency: 2}\n" +
		"bin:\n  - " + toolDir + "\n" +
		"sources:\n  s:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n    fields:\n      id: .id\n      time: .time\n" +
		"  dormant:\n    enabled: false\n    layer: events\n    run: [dormant, --json]\n    fields:\n      id: .id\n      time: .time\n"
	if err := os.WriteFile(filepath.Join(root, "fkf.yaml"), []byte(withCLITestContract(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := invoke(t, "trust", "--base", root, "--format", "text")
	if got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if !strings.Contains(got.stdout, toolDir) {
		t.Errorf("trust listing hides the extra PATH directory, so approving it approves the unseen:\n%s", got.stdout)
	}
	for _, disclosure := range []string{
		"7 day(s)", "index stale after 24h", "timeout 1s", "concurrency 2",
		"dormant", "enabled: false", `run:  ["dormant", "--json"]`,
	} {
		if !strings.Contains(got.stdout, disclosure) {
			t.Errorf("trust listing hides disabled-source disclosure %q:\n%s", disclosure, got.stdout)
		}
	}
}

// TestGraphTakesTheURIItself pins the shape the surface settled on: the question asked daily
// is "what is connected to this", so the URI is the argument of `graph` rather than of an
// `edges` subcommand that existed only to hold it. The bare command still answers the other
// question — the shape of the whole edge list — instead of correcting the caller.
func TestGraphTakesTheURIItself(t *testing.T) {
	root := demoBase(t)
	walk := invoke(t, "--base", root, "--format", "json", "graph", "ticket:FK-412", "--in", "--limit", "3")
	if walk.code != ExitSuccess || !strings.Contains(walk.stdout, "edges") {
		t.Fatalf("graph <uri> exited %d: %s%s", walk.code, walk.stdout, walk.stderr)
	}
	summary := invoke(t, "--base", root, "--format", "json", "graph")
	if summary.code != ExitSuccess || !strings.Contains(summary.stdout, core.GraphFile) {
		t.Fatalf("bare graph exited %d: %s%s", summary.code, summary.stdout, summary.stderr)
	}
	// The subcommands must still win over a URI, or `nodes` would be read as a node name.
	nodes := invoke(t, "--base", root, "--format", "json", "graph", "nodes", "--limit", "1")
	if nodes.code != ExitSuccess || !strings.Contains(nodes.stdout, "nodes") {
		t.Fatalf("graph nodes exited %d: %s%s", nodes.code, nodes.stdout, nodes.stderr)
	}
	if got := invoke(t, "--base", root, "graph", "one", "two"); got.code != ExitInvalidUsage {
		t.Fatalf("graph with two arguments exited %d, want %d", got.code, ExitInvalidUsage)
	}
	for _, flags := range [][]string{{"--in", "--out"}, {"--in", "--both"}, {"--out", "--both"}} {
		args := append([]string{"--base", root, "graph", "ticket:FK-412"}, flags...)
		got := invoke(t, args...)
		if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "choose one of --in, --out, or --both") {
			t.Fatalf("graph direction flags %v exited %d: %s%s", flags, got.code, got.stdout, got.stderr)
		}
	}
}

// TestBareContextExplainsItself records why a bare invocation prints help rather than a usage
// line: somebody typing `fkf context` is asking what the command is for, and answering with
// its grammar tells them nothing they did not already fail to guess.
func TestBareContextExplainsItself(t *testing.T) {
	got := invoke(t, "context")
	if got.code != ExitInvalidUsage {
		t.Fatalf("exit = %d, want %d", got.code, ExitInvalidUsage)
	}
	for _, want := range []string{"DESCRIPTION", "--budget", "fkf find", "--pin", "projects/release-checklist.md"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", got.stdout, want)
		}
	}
}

// TestFindReachesEveryLayerThroughTheCLI is the wiring check for the decision that `find`
// searches the whole base: a term that lives only in a wiki page has to come back from the
// command a reader reaches for when they do not know which layer holds the answer.
func TestFindReachesEveryLayerThroughTheCLI(t *testing.T) {
	root := demoBase(t)
	got := invoke(t, "--base", root, "--format", "json", "find", "retrieval")
	if got.code != ExitSuccess {
		t.Fatalf("exit = %d: %s%s", got.code, got.stdout, got.stderr)
	}
	var result struct {
		Pages []struct {
			URI   string `json:"uri"`
			Layer string `json:"layer"`
		} `json:"pages"`
		Records []struct{} `json:"records"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) == 0 || len(result.Records) == 0 {
		t.Fatalf("result = %+v, want both halves of the base", result)
	}
	for _, page := range result.Pages {
		if page.Layer == "" {
			t.Fatalf("page %s carries no layer; a mixed listing has to say which layer each hit is from", page.URI)
		}
	}
	// --layer is one filter over the whole base, not a page-only switch.
	scoped := invoke(t, "--base", root, "--format", "json", "find", "retrieval", "--layer", "wiki")
	if scoped.code != ExitSuccess || strings.Contains(scoped.stdout, `"records"`) {
		t.Fatalf("--layer wiki exited %d and returned records: %s%s", scoped.code, scoped.stdout, scoped.stderr)
	}
	if bad := invoke(t, "--base", root, "find", "x", "--layer", "nope"); bad.code != ExitInvalidUsage {
		t.Fatalf("an unknown layer exited %d, want %d", bad.code, ExitInvalidUsage)
	}
}

func TestFindHelpDefinesScalarLeafGrep(t *testing.T) {
	got := invoke(t, "find", "--help")
	if got.code != ExitSuccess {
		t.Fatalf("find --help exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	for _, want := range []string{"numeric", "boolean", "compounds"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("find --help = %q, want %q", got.stdout, want)
		}
	}
}

// TestEveryArgumentlessCommandRefusesAStrayArgument is the CLI half of the trust boundary.
// urfave/cli discards extra arguments silently, so before applyArityGuard fourteen of fifteen
// argument-less commands accepted one: `fkf trust check` recorded this machine's trust in every
// declared command and exited 0. Optional singular arguments such as build and init have their
// own mutation-aware regression below. Walking the real command tree is the point — a test listing the commands by hand
// would pass for the fifteenth command that forgot the guard, which is the failure itself.
func TestEveryArgumentlessCommandRefusesAStrayArgument(t *testing.T) {
	root := demoBase(t)
	var walk func(path []string, commands []*cli.Command)
	checked := 0
	walk = func(path []string, commands []*cli.Command) {
		for _, command := range commands {
			here := append(append([]string{}, path...), command.Name)
			walk(here, command.Commands)
			// Commands that declare arguments have command-specific arity tests: some are
			// variadic while init/build are optional singular values.
			if command.ArgsUsage != "" || command.Action == nil || command.Name == "init" {
				continue
			}
			checked++
			got := invoke(t, append(append([]string{"--base", root}, here...), "STRAYARG")...)
			if got.code != ExitInvalidUsage {
				t.Errorf("`fkf %s STRAYARG` exited %d, want %d (a stray argument is a usage error, "+
					"and on trust, build graph, and build wiki it is one that acts): %s%s",
					strings.Join(here, " "), got.code, ExitInvalidUsage, got.stdout, got.stderr)
			}
		}
	}
	walk(nil, newApp(&bytes.Buffer{}, &bytes.Buffer{}).Commands)
	if checked < 14 {
		t.Fatalf("only %d argument-less commands were checked; the walk is not finding the tree", checked)
	}
}

func TestSingularCommandsRefuseExtraArgumentsBeforeWriting(t *testing.T) {
	root := demoBase(t)
	graphPath := filepath.Join(root, core.GraphFile)
	before, err := os.Stat(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	got := invoke(t, "--base", root, "build", "graph", "STRAYARG")
	if got.code != ExitInvalidUsage {
		t.Fatalf("build graph STRAYARG exited %d, want %d: %s%s",
			got.code, ExitInvalidUsage, got.stdout, got.stderr)
	}
	after, err := os.Stat(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("build graph with an extra argument replaced the graph before refusing the usage")
	}

	target := filepath.Join(t.TempDir(), "intended")
	got = invoke(t, "init", target, "STRAYARG")
	if got.code != ExitInvalidUsage {
		t.Fatalf("init <path> STRAYARG exited %d, want %d: %s%s",
			got.code, ExitInvalidUsage, got.stdout, got.stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("init with an extra argument touched %s: %v", target, err)
	}
}

func TestBuildCheckRefusesTargetsThatCannotBeCheckedWithoutWriting(t *testing.T) {
	root := demoBase(t)
	paths := []string{
		filepath.Join(root, core.GraphFile),
		filepath.Join(root, core.GraphMetaFile),
	}
	for _, args := range [][]string{
		{"build", "--check"},
		{"build", "graph", "--check"},
		{"build", "all", "--check"},
	} {
		before := make([]os.FileInfo, len(paths))
		for index, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			before[index] = info
		}
		got := invoke(t, append([]string{"--base", root}, args...)...)
		if got.code != ExitInvalidUsage {
			t.Errorf("fkf %s exited %d, want %d: %s%s",
				strings.Join(args, " "), got.code, ExitInvalidUsage, got.stdout, got.stderr)
		}
		for index, path := range paths {
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before[index], after) {
				t.Errorf("fkf %s replaced %s despite --check", strings.Join(args, " "), path)
			}
		}
	}
}

// And the guard leaves the layer listings' suggestion intact: it is derived from the command's
// own `read` subcommand rather than restated beside it, so the two cannot drift apart.
func TestAStrayArgumentOnAListingNamesTheRead(t *testing.T) {
	root := demoBase(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "events", "some-slug"}, "fkf read events/<date>/<source>.json"},
		{[]string{"list", "index", "some-slug"}, "fkf read index/<name>.json"},
		{[]string{"list", "tasks", "some-slug"}, "fkf read tasks/<date>/<slug>/TASKS.md"},
		{[]string{"list", "wiki", "some-slug"}, "fkf read wiki/<slug>.md"},
		{[]string{"list", "projects", "some-slug"}, "fkf read projects/<slug>.md"},
	} {
		got := invoke(t, append([]string{"--base", root}, tc.args...)...)
		if got.code != ExitInvalidUsage || !strings.Contains(got.stderr+got.stdout, tc.want) {
			t.Errorf("`fkf %s` exited %d saying %q, want %d naming %q",
				strings.Join(tc.args, " "), got.code, got.stderr, ExitInvalidUsage, tc.want)
		}
	}
}

// TestAnUnknownSubcommandIsAUsageErrorNotAnUntrustedBase pins the one exit code fkf does not
// own. A command that is only a container for subcommands had no Action, so urfave/cli fell
// through to its help printer, which answers an unknown subcommand with an error carrying its
// OWN hardcoded exit code 3 — the code fkf documents as "an untrusted base". `fkf mcp bogus`
// therefore told a script the base had not been trusted when the only thing wrong was a typo,
// and exitCodeFor could not tell the two apart: by then both are an ExitCoder holding 3.
func TestAnUnknownSubcommandIsAUsageErrorNotAnUntrustedBase(t *testing.T) {
	root := demoBase(t)
	for _, args := range [][]string{
		{"mcp", "bogus"},
		{"config", "bogus"},
		{"graph", "nodes", "bogus"},
		{"list", "bogus"},
		{"validate", "bogus"},
		{"tags", "bogus"},
		{"build", "bogus"},
		{"new", "bogus"},
		{"bogus"},
	} {
		got := invoke(t, append([]string{"--base", root}, args...)...)
		if got.code != ExitInvalidUsage {
			t.Errorf("`fkf %s` exited %d, want %d; %d is reserved for an untrusted base and a "+
				"script cannot tell a typo from one: %s%s",
				strings.Join(args, " "), got.code, ExitInvalidUsage, ExitUntrusted, got.stdout, got.stderr)
		}
	}
	// And a bare container answers with its own help rather than a usage line, the way a bare
	// `fkf context` does: a bare invocation is somebody asking what the command is for.
	bare := invoke(t, "--base", root, "mcp")
	if bare.code != ExitInvalidUsage || !strings.Contains(bare.stdout, "serve") {
		t.Fatalf("`fkf mcp` exited %d printing %q, want its help and %d", bare.code, bare.stdout, ExitInvalidUsage)
	}
}

// TestReTrustShowsTheSmallestCompleteExecutionDiff pins the canonical review boundary. A
// source-only edit stays concise; a base-wide policy edit still prints the complete disclosure
// because it changes every command without belonging to any one source.
func TestReTrustShowsTheSmallestCompleteExecutionDiff(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "base")
	firstTools := t.TempDir()
	secondTools := t.TempDir()
	if got := invoke(t, "init", root, "--preset", "minimal"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	header := "name: t\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\n"
	quiet := "sources:\n  quiet:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n    fields:\n      id: .id\n      time: .time\n    timeout: 30s\n"
	loud := func(run string) string {
		return "  loud:\n    enabled: true\n    layer: events\n    run: [" + run + "]\n    fields:\n      id: .id\n      time: .time\n"
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(root, "fkf.yaml"), []byte(withCLITestContract(body)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(header + quiet + loud(`echo, "[]"`))
	if got := invoke(t, "trust", "--base", root); got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}

	write(header + quiet + loud("curl, http://evil.test"))
	changed := invoke(t, "trust", "--base", root, "--check", "--format", "text")
	for _, want := range []string{"modified source loud", `"curl"`, `"http://evil.test"`} {
		if !strings.Contains(changed.stdout, want) {
			t.Errorf("source-only review hid %q:\n%s", want, changed.stdout)
		}
	}
	if strings.Contains(changed.stdout, "quiet") {
		t.Errorf("source-only review repeated an untouched source:\n%s", changed.stdout)
	}

	// A base-level PATH directory changes and nothing else does: no source or script item models
	// it, so summarising would make "modified fkf.yaml" the entire execution-plan review.
	write(header + "bin:\n  - " + firstTools + "\n" + quiet)
	if got := invoke(t, "trust", "--base", root); got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	write(header + "bin:\n  - " + secondTools + "\n" + quiet)
	policy := invoke(t, "trust", "--base", root, "--check", "--format", "text")
	if !strings.Contains(policy.stdout, "bin:") || !strings.Contains(policy.stdout, secondTools) {
		t.Errorf("a change only the whole listing can explain was summarised away:\n%s", policy.stdout)
	}
}

func TestReTrustKeepsAScriptOnlyChangeConcise(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "base")
	if got := invoke(t, "init", root, "--preset", "minimal"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	config := "name: t\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\n" +
		"sources:\n  quiet:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n    fields:\n      id: .id\n      time: .time\n"
	if err := os.WriteFile(filepath.Join(root, "fkf.yaml"), []byte(withCLITestContract(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "trust", "--base", root); got.code != ExitSuccess {
		t.Fatalf("trust config exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	help := filepath.Join(root, core.BaseBinDir, "review-helper")
	if err := os.WriteFile(help, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "trust", "--base", root); got.code != ExitSuccess {
		t.Fatalf("trust script exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if err := os.WriteFile(help, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	changed := invoke(t, "trust", "--base", root, "--check", "--format", "text")
	if !strings.Contains(changed.stdout, "modified script review-helper") ||
		!strings.Contains(changed.stdout, "bin/review-helper") {
		t.Fatalf("script-only review did not show the changed script:\n%s", changed.stdout)
	}
	if strings.Contains(changed.stdout, "quiet") || strings.Contains(changed.stdout, "echo '[]'") {
		t.Fatalf("script-only review expanded to the full source listing:\n%s", changed.stdout)
	}
}

func TestTrustDigestIsStableWhenTestsTreeIsAbsent(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "base")
	if got := invoke(t, "init", root, "--preset", "minimal", "--name", "compat"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if err := os.Remove(filepath.Join(root, core.BaseBinDir, "fkf-hook.sh")); err != nil {
		t.Fatal(err)
	}
	got := invoke(t, "--format", "json", "trust", "--base", root)
	if got.code != ExitSuccess {
		t.Fatalf("trust exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	var report services.TrustReport
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("decode trust report: %v\n%s", err, got.stdout)
	}
	const establishedDigestWithoutScripts = "7f97c8781a3d7697f7e96674d8b38cd768871b69aae907d79ceb9de7485b9716"
	if report.State.Digest != establishedDigestWithoutScripts {
		t.Fatalf("digest = %s, want the established digest %s for an unchanged base without tests/", report.State.Digest, establishedDigestWithoutScripts)
	}
}

func TestReTrustKeepsASourceTestOnlyChangeConcise(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "base")
	if got := invoke(t, "init", root, "--preset", "minimal"); got.code != ExitSuccess {
		t.Fatalf("init exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	config := "name: t\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\n" +
		"sources:\n  quiet:\n    enabled: true\n    layer: events\n    run: [echo, \"[]\"]\n" +
		"    test: [source-check.sh]\n    fields:\n      id: .id\n      time: .time\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(withCLITestContract(config)), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	tests := filepath.Join(root, core.BaseTestsDir)
	if err := os.MkdirAll(tests, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(tests, "source-check.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := invoke(t, "trust", "--base", root); got.code != ExitSuccess {
		t.Fatalf("trust test hook exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	current := invoke(t, "trust", "--base", root, "--check", "--format", "json")
	var report services.TrustReport
	if current.code != ExitSuccess {
		t.Fatalf("trust JSON exited %d: %s%s", current.code, current.stdout, current.stderr)
	}
	if err := json.Unmarshal([]byte(current.stdout), &report); err != nil {
		t.Fatalf("decode trust JSON: %v\n%s", err, current.stdout)
	}
	if len(report.Scripts) == 0 || len(report.Tests) != 1 || report.Tests[0].Name != "source-check.sh" {
		t.Fatalf("trust JSON scripts/tests = %+v / %+v, want additive separate trees", report.Scripts, report.Tests)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	changed := invoke(t, "trust", "--base", root, "--check", "--format", "text")
	if !strings.Contains(changed.stdout, "modified test source-check.sh") ||
		!strings.Contains(changed.stdout, "tests/source-check.sh") {
		t.Fatalf("test-only review did not show the changed hook:\n%s", changed.stdout)
	}
	if strings.Contains(changed.stdout, "quiet") || strings.Contains(changed.stdout, "echo '[]'") {
		t.Fatalf("test-only review expanded to the full source listing:\n%s", changed.stdout)
	}
}

func TestSourceTestCommandKeepsAnEmptySelectionSuccessful(t *testing.T) {
	root := demoBase(t)
	got := invoke(t, "--format", "text", "--base", root, "test")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, "0 passed") || !strings.Contains(got.stdout, "0 failed") {
		t.Fatalf("empty source test result = exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
}

func TestBuildAndNewCommands(t *testing.T) {
	root := demoBase(t)

	// Test new subcommands
	if got := invoke(t, "--base", root, "new", "task", "feature-x", "--title", "Feature X"); got.code != ExitSuccess {
		t.Fatalf("new task exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "new", "project", "proj-y", "--title", "Project Y", "--tag", "core"); got.code != ExitSuccess {
		t.Fatalf("new project exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "new", "wiki", "concept-z", "--type", "decision", "--title", "Concept Z", "--tag", "arch"); got.code != ExitSuccess {
		t.Fatalf("new wiki exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	got := invoke(t, "--format", "text", "--base", root, "new", "helper", "collect-prs.sh")
	if got.code != ExitSuccess {
		t.Fatalf("new helper exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	helper := filepath.Join(root, core.BaseBinDir, "collect-prs.sh")
	data, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#!/bin/sh\nset -eu\n") {
		t.Fatalf("helper = %q, want a portable fail-closed /bin/sh template", data)
	}
	info, err := os.Stat(helper)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("helper mode = %v, %v; want %04o", info.Mode().Perm(), err, 0o700)
	}
	if !strings.Contains(got.stdout, `run: ["collect-prs.sh", "{{start}}", "{{end}}"]`) ||
		!strings.Contains(got.stdout, `requires: ["collect-prs.sh"]`) {
		t.Fatalf("new helper output lacks the config snippet:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, "()") {
		t.Fatalf("new helper output reports an unavailable URI:\n%s", got.stdout)
	}
	unsupported := invoke(t, "--base", root, "new", "helper", "other", "--interpreter", "fish")
	if unsupported.code == ExitSuccess || !strings.Contains(unsupported.stderr, "flag provided but not defined") {
		t.Fatalf("interpreter option = (%d) %s%s, want a closed CLI vocabulary", unsupported.code, unsupported.stdout, unsupported.stderr)
	}

	// Test build command
	if got := invoke(t, "--base", root, "build"); got.code != ExitSuccess {
		t.Fatalf("build exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "build", "graph"); got.code != ExitSuccess {
		t.Fatalf("build graph exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "build", "wiki"); got.code != ExitSuccess {
		t.Fatalf("build wiki exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if got := invoke(t, "--base", root, "build", "wiki", "--check"); got.code != ExitSuccess {
		t.Fatalf("build wiki --check exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
}

func TestNewPageHelpNamesTheRequiredTag(t *testing.T) {
	for _, kind := range []string{"project", "wiki"} {
		t.Run(kind, func(t *testing.T) {
			got := invoke(t, "new", kind, "--help")
			if got.code != ExitSuccess {
				t.Fatalf("new %s --help exited %d: %s", kind, got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, "requires at least one --tag") {
				t.Fatalf("new %s help presents its required tag as optional:\n%s", kind, got.stdout)
			}
		})
	}
}
