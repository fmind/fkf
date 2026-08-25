package services_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestReadResolvesEveryShape(t *testing.T) {
	base := contextBase(t)
	cases := []struct {
		name, uri, wantKind string
		check               func(*testing.T, *services.ReadResult)
	}{
		{
			"a directory lists", "events/2026-05-04/", "directory",
			func(t *testing.T, result *services.ReadResult) {
				if len(result.Entries) != 1 || result.Entries[0] != "events/2026-05-04/synthetic.json" {
					t.Fatalf("entries = %v", result.Entries)
				}
			},
		},
		{
			"a document", "events/2026-05-04/synthetic.json", "document",
			func(t *testing.T, result *services.ReadResult) {
				if result.Document == nil || result.Document.Count != 2 {
					t.Fatalf("document = %+v", result.Document)
				}
			},
		},
		{
			"one record by its declared id", "events/2026-05-04/synthetic.json#a1", "record",
			func(t *testing.T, result *services.ReadResult) {
				if result.Record["id"] != "a1" {
					t.Fatalf("record = %v", result.Record)
				}
			},
		},
		{
			"a page", "wiki/retrieval-boundary.md", "page",
			func(t *testing.T, result *services.ReadResult) {
				if result.Page == nil || result.Page.Type != "decision" {
					t.Fatalf("page = %+v", result.Page)
				}
			},
		},
		{
			"a heading in a page", "wiki/retrieval-boundary.md#retrieval-boundary", "section",
			func(t *testing.T, result *services.ReadResult) {
				if !strings.HasPrefix(result.Text, "# Retrieval boundary") {
					t.Fatalf("section = %q, want the heading and its body", result.Text)
				}
			},
		},
		{
			"an entity", "person:email/marc@example.test", "entity",
			func(t *testing.T, result *services.ReadResult) {
				if result.Entity == nil {
					t.Fatalf("entity = %+v", result.Entity)
				}
				if len(result.SnapshotSHA256) != 64 {
					t.Fatalf("entity snapshot = %q, want the validated graph SHA-256", result.SnapshotSHA256)
				}
				if len(result.Entity.Neighbours) == 0 {
					t.Fatal("an entity read carries its graph neighbourhood")
				}
			},
		},
		{
			"a tag", "tag:decision", "entity",
			func(t *testing.T, result *services.ReadResult) {
				if len(result.Entity.Neighbours) == 0 {
					t.Fatalf("entity = %+v, want the authored tag edge", result.Entity)
				}
			},
		},
		{
			"the base's own configuration", "fkf.yaml", "file",
			func(t *testing.T, result *services.ReadResult) {
				if !strings.Contains(result.Text, "name: brain") {
					t.Fatalf("text = %q", result.Text)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := readURI(t, base, test.uri)
			if result.Kind != test.wantKind {
				t.Fatalf("Read(%q).Kind = %q, want %q", test.uri, result.Kind, test.wantKind)
			}
			if result.URI == "" {
				t.Fatal("every result carries the uri it resolved, so an agent can cite it")
			}
			test.check(t, result)
		})
	}
}

func TestReadResolvesExternalAsAnOfflineGraphNode(t *testing.T) {
	base := contextBase(t)
	result := readURI(t, base, "https://example.test/a1")
	if result.Kind != "entity" || result.Entity == nil || result.Entity.Scheme != services.SchemeExternal {
		t.Fatalf("external node = %+v", result)
	}
	if len(result.Entity.Neighbours) == 0 {
		t.Fatalf("external node = %+v, want only its offline graph neighbourhood", result.Entity)
	}
}

func TestReadDirectoryListsOnlyAddressableChildren(t *testing.T) {
	base := contextBase(t)
	projects := filepath.Join(base.Root(), string(core.LayerProjects))
	for _, name := range []string{"backup.key", ".env"} {
		if err := os.WriteFile(filepath.Join(projects, name), []byte("private neighbour"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(projects, "private"), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	result, err := services.Read(t.Context(), base, "projects/", services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		if entry == "projects/backup.key" || entry == "projects/private/" || strings.Contains(entry, ".env") {
			t.Fatalf("directory listing leaked a non-addressable neighbour: %v", result.Entries)
		}
		if _, err := base.Store.Resolve(strings.TrimSuffix(entry, "/")); err != nil {
			t.Fatalf("listing advertised %q, which read refuses: %v", entry, err)
		}
	}
}

func readURI(t *testing.T, base *services.Base, uri string) *services.ReadResult {
	t.Helper()
	result, err := services.Read(t.Context(), base, uri, services.ReadOptions{})
	if err != nil {
		t.Fatalf("Read(%q) error = %v", uri, err)
	}
	return result
}

// TestReadJQGoesToTheRealJQ is the point of `?jq=`: fkf implements no expression language, so
// what a user debugs in a terminal is what fkf ran — one argv element, no shell.
// TestReadJQEvaluatesInProcessAndExecutesNothing pins the fix for a credential oracle: `?jq=`
// used to shell out to the jq on PATH with fkf's inherited environment, so `?jq=$ENV` returned
// every exported token — through the MCP `read` tool, which is ungated and agent-driven. The
// runner t.Fatal-ing on any call is the assertion that matters: the read path executes nothing.
func TestReadJQEvaluatesInProcessAndExecutesNothing(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "unused"}}
	base := graphBase(t)
	base.Runner = runner

	result, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json?jq=.records[].id", services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "selection" {
		t.Fatalf("kind = %q, want a selection", result.Kind)
	}
	// A multi-value result is wrapped into one array so the envelope stays valid JSON.
	var selection []string
	if err := json.Unmarshal(result.Selection, &selection); err != nil {
		t.Fatalf("selection %s is not valid JSON: %v", result.Selection, err)
	}
	if len(selection) == 0 {
		t.Fatalf("selection = %s, want the ids the document declares", result.Selection)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("the runner was called %d time(s); a `?jq=` read must execute nothing", len(runner.calls))
	}
}

// TestReadJQCannotReachTheEnvironmentOrTheFilesystem is the regression test for the oracle.
// gojq compiled with no options resolves `env`/`$ENV` to an empty object and refuses to compile
// `input`, `inputs`, `include`, and `import`, so the closure is structural rather than a
// denylist that the next jq builtin walks around.
func TestReadJQCannotReachTheEnvironmentOrTheFilesystem(t *testing.T) {
	t.Setenv("FKF_TEST_SECRET", "ghp_must_not_leak")
	base := graphBase(t)
	uri := "events/2026-05-04/synthetic.json?jq="

	for _, expression := range []string{"$ENV.FKF_TEST_SECRET", "env.FKF_TEST_SECRET"} {
		result, err := services.Read(t.Context(), base, uri+expression, services.ReadOptions{})
		if err != nil {
			t.Fatalf("Read(%s) error = %v", expression, err)
		}
		if string(result.Selection) != "null" {
			t.Fatalf("%s returned %s, want null: the environment must be unreachable", expression, result.Selection)
		}
	}
	for _, expression := range []string{"env|keys|length", "$ENV|length"} {
		result, err := services.Read(t.Context(), base, uri+expression, services.ReadOptions{})
		if err != nil {
			t.Fatalf("Read(%s) error = %v", expression, err)
		}
		if string(result.Selection) != "0" {
			t.Fatalf("%s returned %s, want 0: the environment must be empty", expression, result.Selection)
		}
	}
	for _, expression := range []string{"input", "[inputs]", `include "x"; .`} {
		_, err := services.Read(t.Context(), base, uri+expression, services.ReadOptions{})
		if !errors.Is(err, services.ErrJQExpression) {
			t.Fatalf("Read(%s) error = %v, want it refused as an invalid expression", expression, err)
		}
	}
}

// TestReadJQKeepsLargeIntegerPrecision guards the reason gojq.Marshal is used rather than
// encoding/json: a 19-digit record id decoded into a float64 comes back rounded, and a rounded
// id addresses a different record.
func TestReadJQKeepsLargeIntegerPrecision(t *testing.T) {
	base := graphBase(t)
	result, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json?jq=9007199254740993123", services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Selection) != "9007199254740993123" {
		t.Fatalf("selection = %s, want the integer intact", result.Selection)
	}
}

func TestReadJQRefusesANonJSONTarget(t *testing.T) {
	base := contextBase(t)
	_, err := services.Read(t.Context(), base, "wiki/retrieval-boundary.md?jq=.title", services.ReadOptions{})
	if err == nil || !strings.Contains(err.Error(), "applies to a JSON document") {
		t.Fatalf("Read() error = %v, want ?jq= refused on a page", err)
	}
}

func TestReadRefusesFragmentsOnFilesWithoutAddressableChildren(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"fkf.yaml#anything", "graph.tsv#anything"} {
		if _, err := services.Read(t.Context(), base, uri, services.ReadOptions{}); err == nil ||
			!strings.Contains(err.Error(), "does not support fragments") {
			t.Errorf("Read(%q) error = %v, want the unsupported fragment refused", uri, err)
		}
	}
}

func TestReadBodyRunsTheDeclaredArgvAndStoresNothing(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "the full body"}}
	base := newBase(t, baseConfig, runner)
	trust(t, base)
	collect(t, base, "2026-05-04", dayOne)

	result, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Body != "the full body" || result.BodyState != "fetched" {
		t.Fatalf("result = %+v", result)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %+v, want one argv execution", runner.calls)
	}
	// Nothing fetched is ever stored: the document on disk is unchanged.
	document, err := base.ReadDocument("events/2026-05-04/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := document.Records[0]["body"]; present {
		t.Fatal("a fetched body was written into the base")
	}
}

func TestReadBodyUsesTheDocumentFieldMapAfterConfigurationDrift(t *testing.T) {
	oldConfig := strings.Replace(baseConfig, "      id: .id\n", "      id: .old_id\n", 1)
	base := newBase(t, oldConfig, nil)
	collect(t, base, "2026-05-04", `[{"old_id":"legacy-77","t":"2026-05-04T09:00:00Z","subject":"Old record"}]`)

	newConfig := strings.Replace(baseConfig, "      id: .id\n", "      id: .new_id\n", 1)
	if err := os.WriteFile(filepath.Join(base.Root(), core.ConfigFileName), []byte(newConfig), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]string{"": "the full body"}}
	base = openBase(t, base.Root(), runner)
	trust(t, base)

	result, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#legacy-77", services.ReadOptions{Body: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.BodyState != "fetched" || len(runner.calls) != 1 {
		t.Fatalf("result = %+v, calls = %+v, want one successful historical body fetch", result, runner.calls)
	}
	if got := runner.calls[0].Argv[len(runner.calls[0].Argv)-1]; got != "legacy-77" {
		t.Fatalf("body id argument = %q, want the value at the document's stored fields.id path", got)
	}
}

func TestReadBodyRefusesANewPlaceholderAbsentFromTheDocumentFieldMap(t *testing.T) {
	oldConfig := strings.Replace(baseConfig, "      topic: .topic\n", "", 1)
	base := newBase(t, oldConfig, nil)
	collect(t, base, "2026-05-04", `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Old record","topic":"unreviewed-raw-value"}]`)

	newConfig := strings.Replace(baseConfig,
		"    body: [cli, view, \"{{id}}\"]\n",
		"    body: [cli, view, \"{{id}}\", --topic, \"{{topic}}\"]\n", 1)
	if err := os.WriteFile(filepath.Join(base.Root(), core.ConfigFileName), []byte(newConfig), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]string{"": "must not run"}}
	base = openBase(t, base.Root(), runner)
	trust(t, base)

	_, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
	if err == nil || !strings.Contains(err.Error(), "fields.topic") || !strings.Contains(err.Error(), "collected document") {
		t.Fatalf("Read(--body) error = %v, want the absent stored field map entry named", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("new placeholder executed with historical raw data: %+v", runner.calls)
	}
}

func TestReadBodyNeverExecutesADisabledHistoricalSource(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	disabled := strings.Replace(baseConfig, "enabled: true", "enabled: false", 1)
	if err := os.WriteFile(filepath.Join(base.Root(), core.ConfigFileName), []byte(disabled), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]string{"": "must not run"}}
	base = openBase(t, base.Root(), runner)
	trust(t, base)
	_, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Read(--body) error = %v, want disabled source refused", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("disabled historical source executed %+v", runner.calls)
	}
}

// TestReadBodyDistinguishesTheThreeAbsences keeps "fetchable", "never", and "the fetch failed"
// apart, which is what the design insists on.
func TestReadBodyDistinguishesTheThreeAbsences(t *testing.T) {
	t.Run("never", func(t *testing.T) {
		config := strings.Replace(baseConfig, "    body: [cli, view, \"{{id}}\"]\n", "", 1)
		base := newBase(t, config, nil)
		trust(t, base)
		collect(t, base, "2026-05-04", dayOne)
		_, err := services.Read(t.Context(), base, "events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
		if err == nil || !strings.Contains(err.Error(), "declares no body:") {
			t.Fatalf("error = %v, want the source to say bodies are not fetchable", err)
		}
	})
	t.Run("failed", func(t *testing.T) {
		runner := &fakeRunner{err: errUnavailable{}}
		base := newBase(t, baseConfig, runner)
		trust(t, base)
		collect(t, base, "2026-05-04", dayOne)
		_, err := services.Read(t.Context(), base, "events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
		if err == nil || !strings.Contains(err.Error(), "fetch body") {
			t.Fatalf("error = %v, want the failure reported rather than swallowed", err)
		}
	})
	t.Run("an untrusted base refuses to fetch", func(t *testing.T) {
		base := newBase(t, baseConfig, &fakeRunner{responses: map[string]string{"": "body"}})
		collect(t, base, "2026-05-04", dayOne)
		_, err := services.Read(t.Context(), base, "events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
		if err == nil || !strings.Contains(err.Error(), "not trusted") {
			t.Fatalf("error = %v, want the trust gate to cover --body", err)
		}
	})
}

type errUnavailable struct{}

func (errUnavailable) Error() string { return "the CLI is not logged in" }

func TestReadRejects(t *testing.T) {
	base := contextBase(t)
	cases := []struct{ uri, wantMessage string }{
		{"events/2026-05-04/synthetic.json#nope", "holds no record with id"},
		{"wiki/retrieval-boundary.md#no-such-heading", "has no heading anchored"},
		{"../outside", "escapes the base"},
	}
	for _, test := range cases {
		_, err := services.Read(t.Context(), base, test.uri, services.ReadOptions{})
		if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
			t.Fatalf("Read(%q) error = %v, want it to mention %q", test.uri, err, test.wantMessage)
		}
	}
	// --body only means something for a collected record.
	for _, uri := range []string{"person:marc@example.test", "events/2026-05-04/", "wiki/retrieval-boundary.md"} {
		if _, err := services.Read(t.Context(), base, uri, services.ReadOptions{Body: true}); err == nil {
			t.Fatalf("Read(%q, --body) succeeded, want it refused", uri)
		}
	}
}

func TestReadGraphArtifactsRequireOneValidatedGeneration(t *testing.T) {
	base := contextBase(t)
	graphURI := core.GraphFile
	metaURI := core.GraphMetaFile
	graph, err := services.Read(t.Context(), base, graphURI, services.ReadOptions{})
	if err != nil || graph.Kind != "file" || graph.Text == "" {
		t.Fatalf("Read(%s) = %+v, %v; want validated graph bytes", graphURI, graph, err)
	}
	meta, err := services.Read(t.Context(), base, metaURI, services.ReadOptions{})
	if err != nil || meta.Kind != "index" || !strings.Contains(string(meta.Selection), `"sha256"`) {
		t.Fatalf("Read(%s) = %+v, %v; want validated matching metadata", metaURI, meta, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *services.Base)
	}{
		{
			name: "metadata trailing JSON",
			mutate: func(t *testing.T, base *services.Base) {
				appendFile(t, base, metaURI, "\n{}\n")
			},
		},
		{
			name: "mixed graph bytes and sidecar",
			mutate: func(t *testing.T, base *services.Base) {
				absolute, err := base.Store.Resolve(graphURI)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(absolute)
				if err != nil {
					t.Fatal(err)
				}
				changed := strings.Replace(string(data), "repo:github.com/fmind/fkf", "repo:github.com/fmind/fkg", 1)
				if changed == string(data) {
					t.Fatal("graph fixture contains no repository edge to change")
				}
				if err := os.WriteFile(absolute, []byte(changed), core.BaseFileMode); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stale authored input",
			mutate: func(t *testing.T, base *services.Base) {
				appendFile(t, base, "wiki/retrieval-boundary.md", "\nChanged after the graph build.\n")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := contextBase(t)
			test.mutate(t, base)
			for _, uri := range []string{graphURI, metaURI} {
				if _, err := services.Read(t.Context(), base, uri, services.ReadOptions{}); err == nil ||
					!strings.Contains(err.Error(), "invalid derived graph cache") {
					t.Errorf("Read(%s) error = %v, want the shared graph-cache validation failure", uri, err)
				}
			}
		})
	}
}

func appendFile(t *testing.T, base *services.Base, relative, suffix string) {
	t.Helper()
	absolute, err := base.Store.Resolve(relative)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(absolute, os.O_APPEND|os.O_WRONLY, core.BaseFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(suffix); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadEntityWithoutBodyRunsNothing is the boundary the whole design rests on: an entity
// is navigated entirely from the stored graph.
func TestReadEntityWithoutBodyRunsNothing(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "never reached"}}
	base := newBase(t, baseConfig, runner)

	if _, err := services.Read(t.Context(), base, "person:marc@example.test", services.ReadOptions{}); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("the runner was called %d time(s); a read without --body executes nothing", len(runner.calls))
	}
}

func TestReadEntityRejectsBodyInsteadOfResolvingThroughAProvider(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "never reached"}}
	base := newBase(t, baseConfig, runner)

	_, err := services.Read(t.Context(), base, "person:marc@example.test", services.ReadOptions{Body: true})
	if err == nil || !strings.Contains(err.Error(), "is an entity") {
		t.Fatalf("Read() error = %v, want entity bodies rejected", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("the runner was called %d time(s); entity reads must remain offline", len(runner.calls))
	}
}

// TestReadEntityPropagatesACorruptIndex is the regression test for a confidently empty answer.
// The neighbourhood error used to be discarded entirely, so `read person:X` against an
// unreadable edge list returned "0 edge(s)" — indistinguishable from "this person appears in
// nothing", which is the one answer a retrieval tool must never invent. An index that has
// simply never been built is the separate, benign case the tests above already cover.
func TestReadEntityPropagatesACorruptIndex(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	// A directory where the edge list belongs: present, so not the benign "not built yet",
	// and unreadable, so an answer built from it would be a guess.
	if err := os.MkdirAll(filepath.Join(base.Root(), core.GraphFile), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := services.Read(t.Context(), base, "ticket:FK-412", services.ReadOptions{})
	if err == nil {
		t.Fatal("Read() answered from an unreadable edge list; an empty neighbourhood would be a guess")
	}
	if !strings.Contains(err.Error(), "neighbourhood") {
		t.Errorf("error = %v, want it to name what could not be read", err)
	}
}

func TestReadEntityRejectsMalformedGraphRows(t *testing.T) {
	base := contextBase(t)
	graph, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(graph, os.O_APPEND|os.O_WRONLY, core.BaseFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("missing\tticket:FK-412\tbroken\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = services.Read(t.Context(), base, "ticket:FK-412", services.ReadOptions{})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Read() error = %v, want the malformed graph row reported", err)
	}
}

func TestReadEntityRejectsARowCompleteTruncatedGraph(t *testing.T) {
	base := contextBase(t)
	truncateGraphCacheByOneRow(t, base)

	_, err := services.Read(t.Context(), base, "ticket:FK-412", services.ReadOptions{})
	if err == nil || !strings.Contains(err.Error(), "metadata edges") ||
		!strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("Read() error = %v, want the row-count mismatch and rebuild remedy", err)
	}
}

// TestReadJQIsBoundedInSpaceNotOnlyInTime is the other half of "reads are bounded". The jq
// timeout caps how LONG a selection runs; nothing capped how much it produced. gojq streams
// values and the reader accumulated every one before returning, so `?jq=range(50000000)` over a
// two-kilobyte document reached 7.2 GB of resident memory in twelve seconds and was still
// growing when the thirty-second timeout would have fired — a machine out of memory long before
// the guard that was supposed to stop it. The same expression is reachable through the MCP
// `read` tool, where the string comes from a connected agent rather than from a person.
func TestReadJQIsBoundedInSpaceNotOnlyInTime(t *testing.T) {
	base := graphBase(t)
	_, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json?jq=range(50000000)", services.ReadOptions{})
	if !errors.Is(err, core.ErrFileTooLarge) {
		t.Fatalf("error = %v, want the selection refused for size before it exhausts memory", err)
	}
	// The bound is on output, not on the expression: a generator that stays small still answers.
	result, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json?jq=range(3)", services.ReadOptions{})
	if err != nil {
		t.Fatalf("a small generator must still evaluate: %v", err)
	}
	if string(result.Selection) != "[0,1,2]" {
		t.Fatalf("selection = %s, want [0,1,2]", result.Selection)
	}
}
