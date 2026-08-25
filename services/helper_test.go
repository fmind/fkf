package services_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

// The suite is hermetic by construction: every base lives in a temporary directory, HOME and
// XDG_STATE_HOME are redirected, FKF_BASE is unset, and every command execution goes through a
// fake runner. No test can reach a provider or discover a real base.

// testClock is fixed so every golden document, receipt digest, and elapsed field is stable.
var testClock = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

type fakeRunner struct {
	// responses maps a substring of the command's display form to the stdout it returns, so a
	// test can drive several sources without caring about their order.
	responses map[string]string
	err       error

	// sync collects units concurrently, so the recorder needs a lock of its own; without one
	// the race detector is right and the call count is wrong.
	mutex sync.Mutex
	calls []sources.Command
}

func (f *fakeRunner) Run(_ context.Context, cmd sources.Command) (string, error) {
	f.mutex.Lock()
	f.calls = append(f.calls, cmd)
	f.mutex.Unlock()
	if f.err != nil {
		return "", f.err
	}
	display := cmd.Display()
	fallback := ""
	for needle, stdout := range f.responses {
		if needle == "" {
			fallback = stdout
			continue
		}
		if contains(display, needle) {
			return stdout, nil
		}
	}
	return fallback, nil
}

func contains(haystack, needle string) bool {
	return len(needle) <= len(haystack) && (needle == "" || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

// isolate redirects everything a base could reach outside its own directory.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(core.BaseEnvVar, "")
}

func installBaseGitSubstitute(t *testing.T, root string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("Git is required for repository policy tests: %v", err)
	}
	bin := filepath.Join(root, core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "BASE_GIT_EXECUTED")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > BASE_GIT_EXECUTED\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// newBase writes a base with the given fkf.yaml and opens it against a fake runner.
func newBase(t *testing.T, config string, runner *fakeRunner) *services.Base {
	t.Helper()
	isolate(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(withServiceTestContract(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	return openBase(t, root, runner)
}

const serviceTestContract = `fkf: 1
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
  url: {description: Provider URL., cardinality: optional}
  author: {description: Provider author values., cardinality: many}
  customer: {description: Provider customer value., cardinality: optional}
  people: {description: Provider identity values., cardinality: many}
  person: {description: Provider identity value., cardinality: optional}
  repo: {description: Provider repository value., cardinality: optional}
  repository: {description: Provider repository value., cardinality: optional}
  ticket: {description: Provider work item value., cardinality: many}
  topic: {description: Searchable topic., cardinality: optional}
`

func withServiceTestContract(config string) string {
	if strings.HasPrefix(strings.TrimSpace(config), "fkf:") {
		return config
	}
	return serviceTestContract + config
}

func openBase(t *testing.T, root string, runner *fakeRunner) *services.Base {
	t.Helper()
	base, err := services.Open(root)
	if err != nil {
		t.Fatalf("open the test base: %v", err)
	}
	if runner == nil {
		runner = &fakeRunner{}
	}
	base.Runner = runner
	base.Now = func() time.Time { return testClock }
	return base
}

// baseConfig enables every layer and declares one source, which is what most tests need.
const baseConfig = `fkf: 1
name: brain
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable label., cardinality: optional}
  url: {description: Provider URL., cardinality: optional, relation: true, examples: ["https://example.test/item"]}
  repository: {description: Repository containing the record., cardinality: optional, relation: true, examples: ["repo:github.com/fmind/fkf"]}
  author: {description: Account that authored the record., cardinality: many, relation: true, examples: ["person:email/marc@example.test"]}
  participant: {description: Actor participating in the record., cardinality: many, relation: true, examples: ["person:email/marc@example.test"]}
  ticket: {description: Work item associated with the record., cardinality: optional, relation: true, examples: ["ticket:FK-412"]}
  customer: {description: Customer concerned by the record., cardinality: optional, relation: true, examples: ["customer:crm.example/123"]}
  related: {description: Related base resources., cardinality: many, relation: true, examples: ["projects/fkf-rebuild.md"]}
  topic: {description: Searchable subject vocabulary., cardinality: optional}
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources:
  synthetic:
    enabled: true
    layer: events
    run: [cli, --since, "{{date}}", --until, "{{next_date}}"]
    fields:
      id: .id
      time: .t
      title: .subject
      url: .link
      repository: .repo_uri
      author: [".author_uris[]"]
      ticket: .ticket_uri
      customer: .customer_uri
      topic: .topic
    body: [cli, view, "{{id}}"]
`

// write puts a file into the base, creating its parents.
func write(t *testing.T, base *services.Base, relative, body string) {
	t.Helper()
	absolute, err := base.Store.Resolve(relative)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(body), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}

func mustResolve(t *testing.T, base *services.Base, relative string) string {
	t.Helper()
	absolute, err := base.Store.Resolve(relative)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// collect files one synthetic day, which is the shortest route to a base with records in it.
func collect(t *testing.T, base *services.Base, date, records string) *sources.Document {
	t.Helper()
	source, err := base.Source("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	day, err := sources.ParseDay(date)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]string{"": records}}
	document, err := sources.Collect(t.Context(), runner, source, base.Env,
		sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatalf("collect %s: %v", date, err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	return document
}

// trust records the digest so a test that exercises execution is not blocked by the gate.
func trust(t *testing.T, base *services.Base) {
	t.Helper()
	if _, err := core.WriteTrust(t.Context(), base.Root(), testClock); err != nil {
		t.Fatal(err)
	}
}

func completeTestDocument(base *services.Base, document *sources.Document) *sources.Document {
	document.Schema = base.Config.Schema.Select(document.Fields)
	return document
}
