package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesOneOpenFieldMap(t *testing.T) {
	configText := `fkf: 1
name: brain
schema:
  id:
    description: Stable provider identity.
    cardinality: one
  time:
    description: Event time.
    cardinality: one
  project:
    description: Project containing the item.
    cardinality: optional
  author:
    description: Accounts that authored or reviewed the item.
    cardinality: many
    relation: true
    examples: ["actor:github.com/fmind"]
layers: {events: true}
sources:
  work-items:
    enabled: true
    run: [cli, list, --json]
    fields:
      id: .id
      time: .updatedAt
      project: .project.key
      author: [".author_uri", ".reviewer_uris[]"]
    body: [cli, view, "{{id}}", --project, "{{project}}"]
`
	config, err := LoadConfig(writeBase(t, configText, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want one open fields map", err)
	}
	encoded, err := json.Marshal(config.Sources["work-items"])
	if err != nil {
		t.Fatal(err)
	}
	want := `"fields":{"author":[".author_uri",".reviewer_uris[]"],"id":".id","project":".project.key","time":".updatedAt"}`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("source JSON = %s, want canonical scalar-or-list field paths %s", encoded, want)
	}
	definition := config.Schema["author"]
	if definition.Description != "Accounts that authored or reviewed the item." ||
		definition.Cardinality != CardinalityMany || !definition.Relation ||
		len(definition.Examples) != 1 || definition.Examples[0] != "actor:github.com/fmind" {
		t.Fatalf("schema.author = %+v, want the declared semantic contract", definition)
	}

	removedSpelling := strings.Replace(configText,
		"    fields:\n      id: .id\n      time: .updatedAt\n      project: .project.key\n      author: [\".author_uri\", \".reviewer_uris[]\"]\n",
		"    id: .id\n    time: .updatedAt\n", 1)
	if _, err := LoadConfig(writeBase(t, removedSpelling, nil)); err == nil || !strings.Contains(err.Error(), "field id not found") {
		t.Fatalf("removed top-level fields error = %v, want the old spelling refused", err)
	}
}

func TestLoadConfigRejectsTheRemovedLookupSurface(t *testing.T) {
	config := `name: brain
layers: {events: true}
sources:
  directory:
    enabled: false
    lookup:
      person: [gws, people, get, "{{identity}}"]
`
	_, err := LoadConfig(writeBase(t, config, nil))
	if err == nil || !strings.Contains(err.Error(), "field lookup not found") {
		t.Fatalf("LoadConfig() error = %v, want lookup rejected as an unknown v1 key", err)
	}
}

func TestLoadConfigKeepsRequirementsExplicit(t *testing.T) {
	config := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
layers: {index: true}
sources:
  repositories:
    enabled: true
    layer: index
    requires: [gh, jq]
    run: [gh, repo, list, --json, name]
    fields: {id: .name}
`
	loaded, err := LoadConfig(writeBase(t, config, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := strings.Join(loaded.Sources["repositories"].Requires, ","); got != "gh,jq" {
		t.Fatalf("requires = %q, want gh,jq", got)
	}
	for _, replacement := range []string{"requires: [gh, gh]", "requires: [./gh]", "requires: ['']"} {
		_, err := LoadConfig(writeBase(t, strings.Replace(config, "requires: [gh, jq]", replacement, 1), nil))
		if err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("LoadConfig(%q) error = %v, want an explicit requirement failure", replacement, err)
		}
	}
}

func TestLoadConfigAcceptsRunAsDirectArgv(t *testing.T) {
	config := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
layers: {events: true}
sources:
  pull-requests:
    enabled: true
    run: [github-search-json, prs, author, "{{start}}", "{{end}}", "{{base}}/with spaces"]
    fields: {id: .id, time: .time}
`
	loaded, err := LoadConfig(writeBase(t, config, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want direct argv accepted", err)
	}
	want := []string{"github-search-json", "prs", "author", "{{start}}", "{{end}}", "{{base}}/with spaces"}
	if got := loaded.Sources["pull-requests"].Run; !slices.Equal(got, want) {
		t.Fatalf("run = %#v, want %#v", got, want)
	}
}

func TestLoadConfigRejectsAShellCommandAsRunExecutable(t *testing.T) {
	config := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
layers: {index: true}
sources:
  repositories:
    enabled: true
    layer: index
    run: ["gh repo list", --json, name]
    fields: {id: .name}
`
	_, err := LoadConfig(writeBase(t, config, nil))
	if err == nil || !strings.Contains(err.Error(), "run[0]") {
		t.Fatalf("LoadConfig() error = %v, want a shell command rejected as the executable", err)
	}
}

func TestLoadConfigRequiresTheV1SemanticContract(t *testing.T) {
	valid := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  author:
    description: Canonical author identities.
    cardinality: many
    relation: true
    examples: ["person:email/marc@example.test", "actor:github.com/fmind"]
layers: {events: true}
sources:
  work:
    enabled: true
    run: [cli]
    fields: {id: .id, time: .time, author: ".author_uris[]"}
`
	if _, err := LoadConfig(writeBase(t, valid, nil)); err != nil {
		t.Fatalf("LoadConfig() rejected the v1 semantic contract: %v", err)
	}

	cases := []struct {
		name, replace, with, want string
	}{
		{"missing marker", "fkf: 1\n", "", "fkf must be 1"},
		{"future marker", "fkf: 1", "fkf: 2", "fkf must be 1"},
		{"undeclared source field", "author: \".author_uris[]\"", "reviewer: \".author_uris[]\"", "fields.reviewer is not declared in schema"},
		{"unknown cardinality", "cardinality: many", "cardinality: some", "cardinality"},
		{"empty description", "description: Canonical author identities.", "description: '   '", "description is required"},
		{"invalid relation example", "actor:github.com/fmind", "not a URI", "examples[1]"},
		{"identity is not a relation", "id: {description: Stable identity., cardinality: one}", "id: {description: Stable identity., cardinality: one, relation: true}", "schema.id must not be a relation"},
		{"event time is not a relation", "time: {description: Event time., cardinality: one}", "time: {description: Event time., cardinality: one, relation: true}", "schema.time must not be a relation"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			text := strings.Replace(valid, test.replace, test.with, 1)
			_, err := LoadConfig(writeBase(t, text, nil))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

// writeBase lays down a minimal base and returns its root. Every configuration test starts
// from a real directory because discovery, the local overlay, and the trust digest all read
// the filesystem, and faking that would test the fake.
func writeBase(t *testing.T, config string, extra map[string]string) string {
	t.Helper()
	config = withTestContract(config)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range extra {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func withTestContract(config string) string {
	if strings.Contains(config, "\nschema:") {
		return config
	}
	return genericTestContract + config
}

const genericTestContract = `fkf: 1
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Display title., cardinality: optional}
  url: {description: Provider URL., cardinality: optional}
  repo: {description: Provider repository value., cardinality: optional}
  people: {description: Provider identity values., cardinality: many}
  participant: {description: Canonical participant URIs., cardinality: many, relation: true}
  project: {description: Provider project value., cardinality: optional}
  author: {description: Provider author values., cardinality: many}
  topic: {description: Searchable topic., cardinality: optional}
`

const minimalConfig = `name: brain
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources:
  github-pull-requests:
    enabled: true
    layer: events
    run: [gh, search, prs, "--updated={{date}}", --json, number]
    fields:
      id: .number
      time: .updatedAt
      repo: .repository.nameWithOwner
      people: [.author.login]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}"]
`

func TestLoadConfigReadsTheBase(t *testing.T) {
	root := writeBase(t, minimalConfig, nil)
	config, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Name != "brain" {
		t.Fatalf("name = %q, want brain", config.Name)
	}
	if got := config.Sync; got != DefaultSync() {
		t.Fatalf("an omitted sync block must mean the visible defaults, got %+v", got)
	}
	source := config.Sources["github-pull-requests"]
	if source == nil || !source.Enabled || source.Layer != LayerEvents || source.Format != FormatJSON {
		t.Fatalf("source = %+v, want an enabled events source decoded as json", source)
	}
	if !source.HasBody() {
		t.Fatal("source body argv was not loaded")
	}
	if len(config.EnabledSources()) != 1 {
		t.Fatalf("enabled sources = %d, want 1", len(config.EnabledSources()))
	}
}

func TestLoadConfigRejectsAmbiguousExecutionText(t *testing.T) {
	for _, testCase := range []struct {
		name, config, want string
	}{
		{
			name: "NUL in run",
			config: strings.Replace(minimalConfig,
				`    run: [gh, search, prs, "--updated={{date}}", --json, number]`,
				`    run: ["gh\0", search, prs, "--updated={{date}}"]`, 1),
			want: "run[0] contains control character U+0000",
		},
		{
			name: "NUL in body argv",
			config: strings.Replace(minimalConfig,
				`    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}"]`,
				`    body: ["gh\0evil", view, "{{id}}"]`, 1),
			want: "body[0] contains control character U+0000",
		},
		{
			name: "invisible retry matcher",
			config: strings.Replace(minimalConfig,
				"    fields:\n",
				"    retry: {attempts: 2, on: [\"temporary\u202Efailure\"]}\n    fields:\n", 1),
			want: "retry.on",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := LoadConfig(writeBase(t, testCase.config, nil))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("LoadConfig() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

// TestLoadConfigRejects is the whole strict-decoding contract in one table. Each case names
// the key it must complain about, because an error the reader cannot act on is just an outage.
func TestLoadConfigRejects(t *testing.T) {
	cases := []struct {
		name, config, wantMessage string
	}{
		{
			name:        "an unknown key",
			config:      "name: brain\nlayers: {events: true}\nnonsense: 1\n",
			wantMessage: "nonsense",
		},
		{
			name: "malformed trailing YAML after a valid document",
			config: "name: brain\nlayers: {events: true}\nsources: {}\n---\n" +
				"broken: [\n",
			wantMessage: "trailing YAML",
		},
		{
			// An unknown key is refused rather than ignored: a misspelled key in the file that
			// declares what runs is a command that silently does not run.
			name:        "an unknown source key",
			config:      "name: brain\nlayers: {events: true}\nsources:\n  s:\n    kind: log\n",
			wantMessage: "field kind not found",
		},
		{
			name:        "an unknown layer",
			config:      "name: brain\nlayers: {logs: true}\n",
			wantMessage: "unknown layer \"logs\"",
		},
		{
			name:        "removed base environment configuration",
			config:      "name: brain\nlayers: {events: true}\nenv: {GH_CONFIG_DIR: /tmp/gh}\n",
			wantMessage: "field env not found",
		},
		{
			name:        "relative command bin directory",
			config:      "name: brain\nlayers: {events: true}\nbin: [tools]\n",
			wantMessage: "bin[0] must be an absolute or ~-relative directory outside the base",
		},
		{
			name:        "a missing name",
			config:      "layers: {events: true}\n",
			wantMessage: "name is required",
		},
		{
			name:        "an MCP authority name beyond its bound",
			config:      "name: " + strings.Repeat("x", MaxBaseNameLength+1) + "\nlayers: {events: true}\n",
			wantMessage: "expected at most 63",
		},
		{
			name: "a typo in a DISABLED source, because the file is documentation",
			config: "name: brain\nlayers: {events: true}\nsources:\n  off-source:\n" +
				"    enabled: false\n    run: [cli]\n    fields:\n      id: not-a-path\n",
			wantMessage: "must start with `.`",
		},
		{
			name: "a source with no run",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: []\n    fields:\n      id: .id\n",
			wantMessage: "run is required",
		},
		{
			name: "removed source environment configuration",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields: {id: .id, time: .t}\n    env: {GH_CONFIG_DIR: /tmp/gh}\n",
			wantMessage: "field env not found",
		},
		{
			name:        "collection concurrency above the safe maximum",
			config:      "name: brain\nlayers: {events: true}\nsync: {concurrency: 5}\n",
			wantMessage: "expected 1..4",
		},
		{
			name: "a source with no id",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n",
			wantMessage: "fields.id is required",
		},
		{
			name: "an events source with no time",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    layer: events\n    run: [cli]\n    fields:\n      id: .id\n",
			wantMessage: "fields.time is required for an events source",
		},
		{
			name: "an unknown placeholder",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    layer: index\n    run: [cli, --at, \"{{yesterday}}\"]\n    fields:\n      id: .id\n",
			wantMessage: "unknown placeholder {{yesterday}}",
		},
		{
			name: "a placeholder in the executable",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [\"{{home}}/cli\"]\n    fields:\n      id: .id\n      time: .t\n",
			wantMessage: "literal executable",
		},
		{
			name: "a run command with stray closing placeholder braces",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli, --at, \"date}}\"]\n    fields:\n      id: .id\n      time: .t\n",
			wantMessage: "malformed placeholder",
		},
		{
			name: "a date placeholder in an index source",
			config: "name: brain\nlayers: {index: true}\nsources:\n  s:\n" +
				"    enabled: true\n    layer: index\n    run: [cli, --at, \"{{date}}\"]\n    fields:\n      id: .id\n",
			wantMessage: "an index source collects a point in time",
		},
		{
			name: "a body without {{id}}",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [cli, view]\n",
			wantMessage: "body must name {{id}}",
		},
		{
			name: "a body with no argument after its executable",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [\"cli{{id}}\"]\n",
			wantMessage: "executable and at least one argument",
		},
		{
			name: "a body whose executable comes from collected data",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [\"{{id}}\", view]\n",
			wantMessage: "body[0] must not use collected placeholder {{id}}",
		},
		{
			name: "a body with a base-relative executable",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [./wiki/helper, \"{{id}}\"]\n",
			wantMessage: "body[0] must be a bare executable resolved on PATH or an absolute machine-local path outside the base",
		},
		{
			name: "a body executable built from the base path",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [\"{{base}}/wiki/helper\", \"{{id}}\"]\n",
			wantMessage: "body[0] must be a literal executable",
		},
		{
			name: "a body using {{repo}} with no repo path",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [cli, \"{{id}}\", \"{{repo}}\"]\n",
			wantMessage: "declare fields.repo",
		},
		{
			name: "a body command with stray closing placeholder braces",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [cli, \"{{id}}\", \"date}}\"]\n",
			wantMessage: "malformed placeholder",
		},
		{
			name: "an enabled source in a disabled layer, naming both",
			config: "name: brain\nlayers: {events: false, index: true}\nsources:\n  s:\n" +
				"    enabled: true\n    layer: events\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n",
			wantMessage: "layers.events is false",
		},
		{
			name:        "a graph mode even when it asks for a full rebuild",
			config:      "name: brain\nlayers: {events: true}\ngraph: {rebuild: full}\n",
			wantMessage: "field graph not found",
		},
		{
			name:        "a sync window outside its range",
			config:      "name: brain\nlayers: {events: true}\nsync: {days: 400}\n",
			wantMessage: "expected 1..366",
		},
		{
			name: "a source name that could not be a filename",
			config: "name: brain\nlayers: {events: true}\nsources:\n  \"Bad Name\":\n" +
				"    enabled: false\n    run: [cli]\n    fields:\n      id: .id\n",
			wantMessage: "lowercase letters, digits, and hyphens",
		},
		{
			name: "a source name whose JSON filename exceeds the portable component bound",
			config: "name: brain\nlayers: {events: true}\nsources:\n  " + strings.Repeat("a", MaxSourceNameLength+1) + ":\n" +
				"    enabled: false\n    run: [cli]\n    fields:\n      id: .id\n",
			wantMessage: fmt.Sprintf("at most %d bytes", MaxSourceNameLength),
		},
		{
			name: "a source without run",
			config: "name: brain\nlayers: {events: true}\nsources:\n  empty:\n" +
				"    enabled: false\n    fields:\n      id: .id\n",
			wantMessage: "run is required",
		},
		{
			name: "a retry with more than one attempt and no on:",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    retry: {attempts: 3}\n",
			wantMessage: "retry.on is empty",
		},
		{
			name: "a retry naming backoff but no attempts",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    retry: {backoff: 30s}\n",
			wantMessage: "no attempts",
		},
		{
			name: "a retry attempts count beyond the ceiling",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n" +
				"    retry: {attempts: 50, on: [\"exit:1\"]}\n",
			wantMessage: "expected 1..5",
		},
		{
			name: "a retry on: with an empty condition",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n" +
				"    retry: {attempts: 2, on: [\"\"]}\n",
			wantMessage: "empty condition",
		},
		{
			name: "a retry exit code that does not parse",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n" +
				"    retry: {attempts: 2, on: [\"exit:nope\"]}\n",
			wantMessage: "retry.on \"exit:nope\"",
		},
		{
			name: "a min_interval that is not a duration",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    min_interval: soon\n",
			wantMessage: "min_interval",
		},
		{
			name: "a min_interval beyond its ceiling",
			config: "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
				"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    min_interval: 24h\n",
			wantMessage: "expected a positive duration up to 10m0s",
		},
		{
			name: "window: true on an index source",
			config: "name: brain\nlayers: {index: true}\nsources:\n  s:\n" +
				"    enabled: true\n    layer: index\n    run: [cli]\n    fields:\n      id: .id\n    window: true\n",
			wantMessage: "a whole-range collection buckets by day",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfig(writeBase(t, test.config, nil))
			if err == nil {
				t.Fatal("LoadConfig() succeeded, want a rejection")
			}
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("error = %v, want it to wrap ErrConfig so the CLI can exit 2", err)
			}
			if !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantMessage)
			}
		})
	}
}

func TestLoadConfigRejectsIndexFreshnessThatCannotBeRepresentedSafely(t *testing.T) {
	root := t.TempDir()
	config := fmt.Sprintf(
		"name: brain\nlayers: {index: true}\nsync: {index_max_age_hours: %d}\n",
		MaxFreshnessAgeHours+1,
	)
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract(config)), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(root)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("1..%d", MaxFreshnessAgeHours)) {
		t.Fatalf("LoadConfig() error = %v, want the bounded index freshness range", err)
	}
}

func TestLoadConfigRejectsCommandBinInsideTheBase(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "tools")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "name: brain\nlayers: {events: true}\nbin: [" + inside + "]\n"
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(root)
	if err == nil || !strings.Contains(err.Error(), "resolves inside the base") {
		t.Fatalf("LoadConfig() error = %v, want the base-controlled PATH directory rejected", err)
	}
}

func TestPlaceholderNamesRequiresCanonicalSpelling(t *testing.T) {
	for _, value := range []string{
		"cli {{ date }}",
		"cli {{date }}",
		"cli {{ date}}",
		"cli date}}",
		"cli }} {{date}}",
	} {
		if _, err := placeholderNames(value); err == nil || !strings.Contains(err.Error(), "use {{name}} in lowercase") {
			t.Errorf("placeholderNames(%q) error = %v, want the non-canonical spelling rejected", value, err)
		}
	}
}

func TestLoadConfigRejectsCommandBinSymlinkedInsideTheBase(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "tools")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "tools")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	config := "name: brain\nlayers: {events: true}\nbin: [" + link + "]\n"
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract(config)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(root)
	if err == nil || !strings.Contains(err.Error(), "resolves inside the base") {
		t.Fatalf("LoadConfig() error = %v, want the symlinked base-controlled PATH directory rejected", err)
	}
}

func TestLoadConfigRejectsArgvExecutableInsideTheBase(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "wiki", "helper")
	if err := os.MkdirAll(filepath.Dir(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "helper")
	if err := os.Symlink(helper, link); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{helper, link} {
		config := "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
			"    enabled: true\n    run: [cli]\n    fields:\n      id: .id\n      time: .t\n    body: [" + executable + ", \"{{id}}\"]\n"
		if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(withTestContract(config)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(root)
		if err == nil || !strings.Contains(err.Error(), "put base-controlled helpers in bin/") {
			t.Fatalf("LoadConfig() with body executable %q error = %v, want base-controlled argv executable rejected", executable, err)
		}
	}
}

func TestLoadConfigAppliesTheLocalOverlay(t *testing.T) {
	local := `bin: [~/tools]
sources:
  github-pull-requests:
    enabled: false
    timeout: 30s
`
	root := writeBase(t, minimalConfig, map[string]string{LocalConfigName: local})
	config, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Sources["github-pull-requests"].Enabled {
		t.Fatal("the overlay must be able to turn a source off on one laptop")
	}
	if config.Sources["github-pull-requests"].Timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want the overlay's 30s", config.Sources["github-pull-requests"].Timeout)
	}
	if len(config.Bin) != 1 || config.Bin[0] != "~/tools" {
		t.Fatalf("bin = %v, want the overlay's directory", config.Bin)
	}
	// Provenance is the point of the overlay: `fkf config` has to be able to say which file
	// each value came from, or a surprising result is unattributable.
	for _, key := range []string{"bin", "sources.github-pull-requests.enabled", "sources.github-pull-requests.timeout"} {
		if got := config.Origins[key]; !strings.HasSuffix(got, LocalConfigName) {
			t.Fatalf("origin[%s] = %q, want it to name %s", key, got, LocalConfigName)
		}
	}
}

func TestLocalOverlayRejectsAnEmptyRun(t *testing.T) {
	root := writeBase(t, minimalConfig, map[string]string{
		LocalConfigName: "sources:\n  github-pull-requests:\n    run: []\n",
	})
	_, err := LoadConfig(root)
	if err == nil || !strings.Contains(err.Error(), "run is required") {
		t.Fatalf("LoadConfig() error = %v, want the overlay's empty command rejected", err)
	}
}

func TestLocalOverlayRefusesAnUndeclaredSource(t *testing.T) {
	root := writeBase(t, minimalConfig, map[string]string{
		LocalConfigName: "sources:\n  invented:\n    enabled: true\n",
	})
	_, err := LoadConfig(root)
	if err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("LoadConfig() error = %v, want the overlay to refuse an undeclared source", err)
	}
}

func TestLocalOverlayRejectsMalformedTrailingYAML(t *testing.T) {
	root := writeBase(t, minimalConfig, map[string]string{
		LocalConfigName: "bin: [~/tools]\n---\nbroken: [\n",
	})
	_, err := LoadConfig(root)
	if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "trailing YAML") {
		t.Fatalf("LoadConfig() error = %v, want malformed trailing local YAML rejected", err)
	}
}

func TestLocalOverlayRejectsRemovedEnvironmentConfiguration(t *testing.T) {
	for _, test := range []struct {
		name  string
		local string
	}{
		{name: "base", local: "env: {GH_CONFIG_DIR: /tmp/gh}\n"},
		{name: "source", local: "sources:\n  github-pull-requests:\n    env: {GH_CONFIG_DIR: /tmp/gh}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := writeBase(t, minimalConfig, map[string]string{LocalConfigName: test.local})
			_, err := LoadConfig(root)
			if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "field env not found") {
				t.Fatalf("LoadConfig() error = %v, want removed local env rejected", err)
			}
		})
	}
}

func TestLoadConfigNamesTheMissingFile(t *testing.T) {
	_, err := LoadConfig(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "fkf init") {
		t.Fatalf("LoadConfig() error = %v, want it to name `fkf init` as the remedy", err)
	}
}

func TestDiscoverBaseOrder(t *testing.T) {
	root := writeBase(t, minimalConfig, nil)
	nested := filepath.Join(root, "events", "2026-08-22")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("an explicit path wins", func(t *testing.T) {
		t.Setenv(BaseEnvVar, "/elsewhere")
		got, origin, err := DiscoverBase(root)
		if err != nil || got != root || origin != BaseFromFlag {
			t.Fatalf("DiscoverBase(%q) = %q, %q, %v", root, got, origin, err)
		}
	})
	t.Run("then the environment", func(t *testing.T) {
		t.Setenv(BaseEnvVar, root)
		got, origin, err := DiscoverBase("")
		if err != nil || got != root || origin != BaseFromEnvironment {
			t.Fatalf("DiscoverBase(\"\") = %q, %q, %v", got, origin, err)
		}
	})
	t.Run("then walking up, like git", func(t *testing.T) {
		t.Setenv(BaseEnvVar, "")
		t.Chdir(nested)
		got, origin, err := DiscoverBase("")
		if err != nil || origin != BaseFromDiscovery {
			t.Fatalf("DiscoverBase(\"\") = %q, %q, %v", got, origin, err)
		}
		// macOS resolves TMPDIR through a symlinked /var, so compare the resolved paths.
		wantResolved, _ := filepath.EvalSymlinks(root)
		gotResolved, _ := filepath.EvalSymlinks(got)
		if gotResolved != wantResolved {
			t.Fatalf("walked up to %q, want %q", gotResolved, wantResolved)
		}
	})
	t.Run("a miss names all three rules rather than guessing", func(t *testing.T) {
		t.Setenv(BaseEnvVar, "")
		t.Chdir(t.TempDir())
		_, _, err := DiscoverBase("")
		if !errors.Is(err, ErrNoBase) {
			t.Fatalf("DiscoverBase(\"\") error = %v, want ErrNoBase", err)
		}
		for _, want := range []string{"--base", BaseEnvVar, ConfigFileName} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to mention %q", err, want)
			}
		}
	})
}

func TestDiscoverBaseMakesRelativeSelectionsAbsolute(t *testing.T) {
	root := writeBase(t, minimalConfig, nil)
	parent := filepath.Dir(root)
	relative := filepath.Base(root)
	t.Chdir(parent)

	got, origin, err := DiscoverBase(relative)
	if err != nil || got != root || origin != BaseFromFlag {
		t.Fatalf("DiscoverBase(%q) = %q, %q, %v; want absolute %q", relative, got, origin, err, root)
	}
	t.Setenv(BaseEnvVar, relative)
	got, origin, err = DiscoverBase("")
	if err != nil || got != root || origin != BaseFromEnvironment {
		t.Fatalf("DiscoverBase from %s=%q = %q, %q, %v; want absolute %q",
			BaseEnvVar, relative, got, origin, err, root)
	}
}

func TestResolveAbsolutePathPreservesTheChosenSymlinkSpelling(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	alias := filepath.Join(parent, "alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	got, err := ResolveAbsolutePath("alias")
	if err != nil || got != alias {
		t.Fatalf("ResolveAbsolutePath(alias) = %q, %v; want chosen spelling %q", got, err, alias)
	}
}

// TestNoGlobalConfigurationIsRead is the invariant, not an implementation detail: a file in
// ~/.config must never change what a base does.
func TestNoGlobalConfigurationIsRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "fkf.yaml"), []byte("name: intruder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(writeBase(t, minimalConfig, nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.Name != "brain" {
		t.Fatalf("name = %q, want the base's own value; no global file may be read", config.Name)
	}
}

func TestValidBodyValueAcceptsOpaqueUnicodeArgvValues(t *testing.T) {
	for _, value := range []string{"42", "fmind/fkf", "a.b_c:d@e+f-g", "révision n° 42", "a; $(not-a-shell) | 👍"} {
		if !ValidBodyValue(value) {
			t.Fatalf("ValidBodyValue(%q) = false, want an opaque argv value accepted", value)
		}
	}
	for _, value := range []string{
		"--help", "-Rattacker/repo", "a\nb", "a\tb", "", "   ", "a\u200bb",
		string([]byte{0xff}), strings.Repeat("a", 257),
	} {
		if ValidBodyValue(value) {
			t.Fatalf("ValidBodyValue(%q) = true, want it refused before exec", value)
		}
	}
}

// TestLoadConfigAcceptsADeclaredRetryPolicy is the acceptance half of the rejection table: a
// well-formed policy decodes into exactly the values the runner reads, and RetryAttempts
// floors at one so a source declaring no policy at all behaves as "run it once".
func TestLoadConfigAcceptsADeclaredRetryPolicy(t *testing.T) {
	config := "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
		"    enabled: true\n    run: [gh, search, prs]\n    fields:\n      id: .id\n      time: .t\n" +
		"    retry: {attempts: 3, backoff: 30s, on: [\"exit:1\", rate limit]}\n" +
		"    min_interval: 5s\n"
	loaded, err := LoadConfig(writeBase(t, config, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	source := loaded.Sources["s"]
	if source.RetryAttempts() != 3 {
		t.Fatalf("RetryAttempts() = %d, want 3", source.RetryAttempts())
	}
	if source.Retry.Backoff != 30*time.Second {
		t.Fatalf("Retry.Backoff = %v, want 30s", source.Retry.Backoff)
	}
	if len(source.Retry.On) != 2 || source.Retry.On[0] != "exit:1" || source.Retry.On[1] != "rate limit" {
		t.Fatalf("Retry.On = %v, want [exit:1, rate limit]", source.Retry.On)
	}
	if source.MinInterval != 5*time.Second {
		t.Fatalf("MinInterval = %v, want 5s", source.MinInterval)
	}
	// A source declaring nothing gets the "run it once" default, unaffected by a neighbour's
	// policy — retry is per-source, not a base-wide switch.
	quiet := &Source{}
	if quiet.RetryAttempts() != 1 {
		t.Fatalf("RetryAttempts() on an undeclared policy = %d, want 1", quiet.RetryAttempts())
	}
}

// TestLoadConfigAcceptsWindowOnAnEventsSource is the acceptance half: a well-formed window:
// true decodes onto an events source and does not touch its neighbours.
func TestLoadConfigAcceptsWindowOnAnEventsSource(t *testing.T) {
	config := "name: brain\nlayers: {events: true}\nsources:\n  s:\n" +
		"    enabled: true\n    layer: events\n    run: [cli, \"{{start}}\", \"{{end}}\"]\n    fields:\n      id: .id\n      time: .t\n    window: true\n"
	loaded, err := LoadConfig(writeBase(t, config, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !loaded.Sources["s"].Window {
		t.Fatal("Window = false, want true")
	}
}
