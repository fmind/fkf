package sources_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// Every test here injects a runner. The suite must never be able to reach a provider, and a
// fake also lets a test assert exactly what WOULD have run — which is what makes --dry-run
// verifiable rather than merely plausible.
type fakeRunner struct {
	stdout string
	err    error
	calls  []sources.Command
}

func (f *fakeRunner) Run(_ context.Context, cmd sources.Command) (string, error) {
	f.calls = append(f.calls, cmd)
	return f.stdout, f.err
}

func mustSource(t *testing.T, yaml string) *core.Source {
	t.Helper()
	root := t.TempDir()
	body := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human label., cardinality: optional}
  url: {description: Provider URL., cardinality: optional}
  repo: {description: Repository value., cardinality: optional}
  people: {description: Identity values., cardinality: many}
  project: {description: Project value., cardinality: optional}
  base: {description: Provider base value., cardinality: optional}
  home: {description: Provider home value., cardinality: optional}
  author: {description: Canonical author URI., cardinality: optional, relation: true}
  participants: {description: Canonical participant URIs., cardinality: many, relation: true}
layers: {events: true, index: true}
sources:
  s:
` + yaml
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatalf("load the test source: %v", err)
	}
	return config.Sources["s"]
}

const logSource = `    enabled: true
    layer: events
    run: [cli, --since, "{{date}}", --until, "{{next_date}}"]
    fields:
      id: .id
      time: .t
      title: .subject
      repo: .repo
      people: [.who]
`

var testDay = time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)

func testEnvironment(t *testing.T) sources.Environment {
	t.Helper()
	return sources.Environment{Root: t.TempDir(), Env: map[string]string{"PATH": "/usr/bin"}}
}

func TestCollectStoresRecordsWholeWithTheirFieldMap(t *testing.T) {
	source := mustSource(t, logSource)
	runner := &fakeRunner{stdout: `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Fix FK-412","repo":"o/r","who":"m@x.test","extra":{"kept":true}}]`}
	document, err := sources.Collect(context.Background(), runner, source, testEnvironment(t),
		sources.DayWindow(testDay), time.Minute, time.Date(2026, 5, 5, 5, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if document.FKF != sources.SchemaVersion || document.Date != "2026-05-04" || document.Count != 1 {
		t.Fatalf("document = %+v", document)
	}
	if document.WindowStart != "2026-05-04T00:00:00Z" || document.WindowEnd != "2026-05-05T00:00:00Z" {
		t.Fatalf("stored window = [%s, %s), want the exact UTC bounds used for collection", document.WindowStart, document.WindowEnd)
	}
	encoded, err := sources.EncodeDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	// A provider command is mutable execution configuration, not reproducible evidence. It can
	// also contain machine-local paths, so the durable document deliberately stores neither it
	// nor the collection planner's windowing detail.
	if strings.Contains(string(encoded), `"command"`) || strings.Contains(string(encoded), `"windowed"`) {
		t.Fatalf("stored document exposes execution metadata:\n%s", encoded)
	}
	// The field map travels too, so a read never depends on the live fkf.yaml.
	if document.Fields.Path(core.FieldID).String() != ".id" ||
		document.Fields.Path("repo").String() != ".repo" {
		t.Fatalf("fields = %+v", document.Fields)
	}
	if definition := document.Schema[core.FieldID]; definition.Cardinality != core.CardinalityOne {
		t.Fatalf("schema.id = %+v, want the semantic definition stored with the field map", definition)
	}
	// Records retain every decoded value: a field fkf knows nothing about survives.
	if nested, ok := document.Records[0]["extra"].(map[string]any); !ok || nested["kept"] != true {
		t.Fatalf("record = %v, want every decoded field and value retained", document.Records[0])
	}
	if uri, ok := document.RecordURI(document.Records[0]); !ok || uri != "events/2026-05-04/s.json#a1" {
		t.Fatalf("RecordURI() = %q, %v", uri, ok)
	}
}

func TestCollectedEventKeepsItsOriginalWindowAcrossTimezoneChanges(t *testing.T) {
	location, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 5, 4, 12, 0, 0, 0, location)

	source := mustSource(t, logSource)
	runner := &fakeRunner{stdout: `[{"id":"a1","t":"2026-05-03T12:30:00Z","subject":"Timezone evidence"}]`}
	document, err := sources.Collect(t.Context(), runner, source, testEnvironment(t),
		sources.DayWindow(day), time.Minute, testDay)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sources.EncodeDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	document, err = sources.DecodeDocument(encoded, document.URI())
	if err != nil {
		t.Fatal(err)
	}

	if err := sources.VerifyRecords(document); err != nil {
		t.Fatalf("VerifyRecords() with stored Pacific/Apia bounds under the process timezone: %v", err)
	}
}

func TestCollectEnforcesDeclaredCardinalityAndRelationURIs(t *testing.T) {
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli]
    fields:
      id: .ids[]
      time: .time
      title: .titles[]
      author: .author
      participants: .participants[]
`)
	cases := []struct {
		name, stdout, want string
	}{
		{
			"one identity",
			`[{"ids":["a","b"],"time":"2026-05-04T09:00:00Z","author":"actor:github.com/fmind"}]`,
			"field id projects 2 values; schema cardinality one",
		},
		{
			"optional title",
			`[{"ids":["a"],"time":"2026-05-04T09:00:00Z","titles":["one","two"],"author":"actor:github.com/fmind"}]`,
			"field title projects 2 values; schema cardinality optional",
		},
		{
			"canonical relation",
			`[{"ids":["a"],"time":"2026-05-04T09:00:00Z","author":"fmind"}]`,
			"field author value \"fmind\" is not a canonical relation URI",
		},
		{
			"relation whitespace is not normalized",
			`[{"ids":["a"],"time":"2026-05-04T09:00:00Z","author":" actor:github.com/fmind "}]`,
			"field author value \" actor:github.com/fmind \" is not a canonical relation URI",
		},
		{
			"file relation spelling is canonical",
			`[{"ids":["a"],"time":"2026-05-04T09:00:00Z","author":"wiki/../projects/fkf.md"}]`,
			"field author value \"wiki/../projects/fkf.md\" is not a canonical relation URI",
		},
		{
			"file relation matches the closed address grammar",
			`[{"ids":["a"],"time":"2026-05-04T09:00:00Z","author":"wiki/nested/page.md"}]`,
			"field author value \"wiki/nested/page.md\" is not a canonical relation URI",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := sources.Collect(t.Context(), &fakeRunner{stdout: test.stdout}, source,
				testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay)
			if !errors.Is(err, sources.ErrIncomplete) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Collect() error = %v, want %q", err, test.want)
			}
		})
	}

	// A record identity may itself be a URL. Only the file-path head participates in scheme
	// classification; the fragment remains an opaque canonical record identity.
	output := `[{"ids":["a"],"time":"2026-05-04T09:00:00Z","titles":["URL-shaped identity"],"author":"events/2026-08-22/rss.json#https://example.test/post"}]`
	if _, err := sources.Collect(t.Context(), &fakeRunner{stdout: output}, source,
		testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay); err != nil {
		t.Fatalf("Collect() rejected a URL-shaped record fragment: %v", err)
	}
}

func TestCollectTreatsEmptyMappedStringsByDeclaredCardinality(t *testing.T) {
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli]
    fields:
      id: .id
      time: .time
      title: .title
      participants: .participants[]
`)
	document, err := sources.Collect(t.Context(), &fakeRunner{stdout: `[
  {"id":"a","time":"2026-05-04T09:00:00Z","title":"Record","participants":["", "actor:github.com/fmind"]}
]`}, source, testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay)
	if err != nil {
		t.Fatalf("Collect() error = %v; optional and many empty strings must be absent", err)
	}
	if document.Count != 1 {
		t.Fatalf("document count = %d, want 1", document.Count)
	}
}

func TestCollectRequiresAControlFreeMeaningfulTitle(t *testing.T) {
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli]
    fields:
      id: .id
      time: .time
      title: .title
`)
	for name, title := range map[string]string{
		"empty": "", "control": "unsafe\nheading",
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal([]map[string]string{{
				"id": "a", "time": "2026-05-04T09:00:00Z", "title": title,
			}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = sources.Collect(t.Context(), &fakeRunner{stdout: string(encoded)}, source,
				testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay)
			if !errors.Is(err, sources.ErrIncomplete) || !strings.Contains(err.Error(), "title") {
				t.Fatalf("Collect() error = %v, want title contract refusal", err)
			}
		})
	}
}

func TestCollectTreatsDateOnlyEventTimeAsACivilDate(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata for America/New_York on this system: %v", err)
	}
	source := mustSource(t, logSource)
	runner := &fakeRunner{stdout: `[{"id":"all-day","t":"2026-05-04","subject":"all-day event"}]`}
	day := time.Date(2026, 5, 4, 12, 0, 0, 0, loc)
	if _, err := sources.Collect(t.Context(), runner, source, testEnvironment(t),
		sources.DayWindow(day), time.Minute, day); err != nil {
		t.Fatalf("Collect() rejected a date-only record belonging to its civil day west of UTC: %v", err)
	}
}

func TestVerifyRecordsRejectsAnEventOutsideItsDocumentDay(t *testing.T) {
	source := mustSource(t, logSource)
	for _, recordTime := range []string{"2026-05-05T00:00:00Z", "2026-05-05"} {
		t.Run(recordTime, func(t *testing.T) {
			document := &sources.Document{
				FKF: sources.SchemaVersion, Source: source.Name, Layer: core.LayerEvents, Date: "2026-05-04",
				CollectedAt: "2026-05-05T08:00:00Z",
				Schema:      sources.SchemaOf(source), Fields: sources.FieldsOf(source),
				Count: 1, Records: []sources.Record{{"id": "outside", "t": recordTime}},
			}
			err := sources.VerifyRecords(document)
			if err == nil || !strings.Contains(err.Error(), "outside the requested window") {
				t.Fatalf("VerifyRecords() error = %v, want the event-day membership failure", err)
			}
		})
	}
}

func TestVerifyRecordsRejectsANonCanonicalStoredSourceName(t *testing.T) {
	source := mustSource(t, `    enabled: true
    layer: index
    run: [cli]
    fields:
      id: .id
      title: .id
`)
	for _, test := range []struct {
		name, source, want string
	}{
		{name: "path traversal that normalizes to the filed source", source: "other/../s", want: "lowercase letters"},
		{name: "overlong source", source: strings.Repeat("a", core.MaxSourceNameLength+1), want: "at most"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := &sources.Document{
				FKF: sources.SchemaVersion, Source: test.source, Layer: core.LayerIndex,
				CollectedAt: "2026-05-05T08:00:00Z",
				Schema:      sources.SchemaOf(source), Fields: sources.FieldsOf(source),
				Count: 1, Records: []sources.Record{{"id": "one"}},
			}
			if test.source == "other/../s" && document.URI() != "index/s.json" {
				t.Fatalf("fixture URI = %q, want path.Join to demonstrate the provenance collision", document.URI())
			}
			err := sources.VerifyRecords(document)
			if err == nil || !strings.Contains(err.Error(), "document source") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyRecords() error = %v, want a stored source-name refusal containing %q", err, test.want)
			}
		})
	}
}

func TestVerifyRecordsValidatesStoredEventWindowBounds(t *testing.T) {
	source := mustSource(t, logSource)
	base := sources.Document{
		FKF: sources.SchemaVersion, Source: source.Name, Layer: core.LayerEvents, Date: "2026-05-04",
		CollectedAt: "2026-05-05T08:00:00Z",
		Schema:      sources.SchemaOf(source), Fields: sources.FieldsOf(source),
		Count: 0, Records: []sources.Record{},
	}
	cases := []struct {
		name, start, end, want string
	}{
		{"missing end", "2026-05-04T00:00:00Z", "", "both window_start and window_end"},
		{"invalid start", "not-a-time", "2026-05-05T00:00:00Z", "window_start"},
		{"invalid end", "2026-05-04T00:00:00Z", "not-a-time", "window_end"},
		{"reversed", "2026-05-05T00:00:00Z", "2026-05-04T00:00:00Z", "empty or reversed"},
		{"noncanonical offset", "2026-05-04T02:00:00+02:00", "2026-05-05T00:00:00Z", "not canonical UTC"},
		{"arbitrary oversized range", "1900-01-01T00:00:00Z", "2100-01-01T00:00:00Z", "civil day must span"},
		{"wrong civil date", "2026-06-04T00:00:00Z", "2026-06-05T00:00:00Z", "not aligned with civil date"},
		{"implausibly short day", "2026-05-04T00:00:00Z", "2026-05-04T01:00:00Z", "civil day must span"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document := base
			document.WindowStart, document.WindowEnd = test.start, test.end
			err := sources.VerifyRecords(&document)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyRecords() error = %v, want %q", err, test.want)
			}
		})
	}

	for _, valid := range []struct{ start, end string }{
		// Fractional-hour DST shifts and historical date-line moves are both real civil days.
		{"2026-05-04T00:00:00Z", "2026-05-04T23:30:00Z"},
		{"2026-05-03T12:00:00Z", "2026-05-05T12:00:00Z"},
	} {
		document := base
		document.WindowStart, document.WindowEnd = valid.start, valid.end
		if err := sources.VerifyRecords(&document); err != nil {
			t.Errorf("VerifyRecords() rejected plausible civil bounds [%s, %s): %v", valid.start, valid.end, err)
		}
	}

	index := base
	index.Layer, index.Date = core.LayerIndex, ""
	index.WindowStart, index.WindowEnd = "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z"
	if err := sources.VerifyRecords(&index); err == nil || !strings.Contains(err.Error(), "index document declares an event collection window") {
		t.Fatalf("VerifyRecords(index) error = %v", err)
	}
}

// TestCollectEmptyOutputRule is the difference the two formats exist for: a CLI printing one
// JSON document prints [] for an empty result, so silence means it was cut short; a paginating
// CLI streaming NDJSON legitimately prints nothing for a day that held nothing.
func TestCollectEmptyOutputRule(t *testing.T) {
	t.Run("json refuses silence", func(t *testing.T) {
		source := mustSource(t, logSource)
		_, err := sources.Collect(context.Background(), &fakeRunner{stdout: "   \n"}, source,
			testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay)
		if !errors.Is(err, sources.ErrIncomplete) || !strings.Contains(err.Error(), "prints []") {
			t.Fatalf("Collect() error = %v, want an incomplete day naming the [] convention", err)
		}
	})
	t.Run("ndjson accepts silence as an empty day", func(t *testing.T) {
		source := mustSource(t, logSource+"    format: ndjson\n")
		document, err := sources.Collect(context.Background(), &fakeRunner{stdout: ""}, source,
			testEnvironment(t), sources.DayWindow(testDay), time.Minute, testDay)
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if document.Count != 0 || document.Records == nil {
			t.Fatalf("document = %+v, want a complete, empty day", document)
		}
	})
}

func TestCollectFailsTheWholeDay(t *testing.T) {
	cases := []struct {
		name, yaml, stdout, wantMessage string
		runErr                          error
	}{
		{
			name: "a non-zero exit", yaml: logSource, runErr: errors.New("exit status 3"),
			wantMessage: "exit status 3",
		},
		{
			name: "undecodable output", yaml: logSource, stdout: "not json",
			wantMessage: "not valid JSON",
		},
		{
			name: "a record with no declared id", yaml: logSource,
			stdout:      `[{"t":"2026-05-04T09:00:00Z"}]`,
			wantMessage: "field id projects 0 values; schema cardinality one",
		},
		{
			name: "a record with an empty declared id", yaml: logSource,
			stdout:      `[{"id":"","t":"2026-05-04T09:00:00Z"}]`,
			wantMessage: "empty identity",
		},
		{
			name: "a log record with no timestamp", yaml: logSource,
			stdout:      `[{"id":"a1"}]`,
			wantMessage: "field time projects 0 values; schema cardinality one",
		},
		{
			name: "a log record with an unreadable timestamp", yaml: logSource,
			stdout:      `[{"id":"a1","t":"last tuesday"}]`,
			wantMessage: "matches no known layout",
		},
		{
			name: "a log record outside the requested day", yaml: logSource,
			stdout:      `[{"id":"a1","t":"2026-05-05T00:00:00Z"}]`,
			wantMessage: "outside the requested window",
		},
		{
			name: "a scalar where a record belongs", yaml: logSource,
			stdout: `["just a string"]`, wantMessage: "must be a JSON object",
		},
		{
			name: "an object where an array belongs", yaml: logSource,
			stdout: `{"items": []}`, wantMessage: "declare `records:`",
		},
		{
			// A URI names a record by its declared identity, so two records sharing one leave
			// the second permanently unreachable and make `read <doc>#<id>` answer with
			// whichever came first. Filing the day anyway would hide a broken `fields.id` path
			// forever: a day is complete or absent, never ambiguous.
			name: "two records sharing one declared id", yaml: logSource,
			stdout:      `[{"id":"a1","t":"2026-05-04T09:00:00Z"},{"id":"a1","t":"2026-05-04T10:00:00Z"}]`,
			wantMessage: `share the id "a1"`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			source := mustSource(t, test.yaml)
			runner := &fakeRunner{stdout: test.stdout, err: test.runErr}
			document, err := sources.Collect(context.Background(), runner, source, testEnvironment(t),
				sources.DayWindow(testDay), time.Minute, testDay)
			if err == nil {
				t.Fatalf("Collect() succeeded with %+v, want the whole day to fail", document)
			}
			if !errors.Is(err, sources.ErrIncomplete) {
				t.Fatalf("error = %v, want ErrIncomplete", err)
			}
			for _, want := range []string{"source s", test.wantMessage} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestDecodeRecordsFromAWrappedPage(t *testing.T) {
	source := mustSource(t, logSource+"    format: ndjson\n    records: .items\n")
	stdout := "{\"items\":[{\"id\":\"a\",\"t\":\"2026-05-04T00:00:00Z\"}],\"next\":\"x\"}\n" +
		"{\"items\":[{\"id\":\"b\",\"t\":\"2026-05-04T01:00:00Z\"}]}\n" +
		"{\"next\":null}\n" // the last page of a quiet day carries the envelope and no items
	records, err := sources.DecodeRecords(source, stdout)
	if err != nil {
		t.Fatalf("DecodeRecords() error = %v", err)
	}
	if len(records) != 2 || records[0]["id"] != "a" || records[1]["id"] != "b" {
		t.Fatalf("records = %v, want the two items across the pages", records)
	}
}

func TestDecodeRecordsRequiresDeclaredPathInJSONDocument(t *testing.T) {
	source := mustSource(t, logSource+"    records: .items\n")
	_, err := sources.DecodeRecords(source, `{"next":null}`)
	if err == nil {
		t.Fatal("DecodeRecords() succeeded when the JSON document omitted its declared records path")
	}
	for _, want := range []string{".items", "selected nothing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestBuildRunCommandSubstitutesOnlyWhatFkfControls(t *testing.T) {
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli, "{{date}}", "{{next_date}}", "{{start}}", "{{end}}", "{{base}}"]
    fields:
      id: .id
      time: .t
      title: .id
`)
	env := sources.Environment{Root: "/base", Env: map[string]string{"PATH": "/usr/bin"}}
	command := mustBuildRunCommand(t, source, env, sources.DayWindow(testDay), time.Minute)
	want := []string{"cli", "2026-05-04", "2026-05-05", "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", "/base"}
	if !slices.Equal(command.Argv, want) {
		t.Fatalf("argv = %v, want direct argv %v", command.Argv, want)
	}
	if command.Display() != strings.Join(want, " ") {
		t.Fatalf("Display() = %q, want the substituted argv", command.Display())
	}
}

func TestBuildRunCommandPassesFilesystemPlaceholdersAsOpaqueArguments(t *testing.T) {
	t.Setenv("HOME", "/tmp/home $(touch nope)")
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli, "{{base}}", "{{home}}/repos"]
    fields:
      id: .id
      time: .t
      title: .id
`)
	env := sources.Environment{
		Root: "/tmp/brain; touch pwned ' quote",
		Env:  map[string]string{"PATH": "/usr/bin"},
	}
	command := mustBuildRunCommand(t, source, env, sources.DayWindow(testDay), time.Minute)
	if command.Argv[1] != env.Root || command.Argv[2] != "/tmp/home $(touch nope)/repos" {
		t.Fatalf("path argv = %q, %q; want hostile paths passed as data", command.Argv[1], command.Argv[2])
	}
}

func TestCollectFailsPlanningAHomePlaceholderWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli, "{{home}}"]
    fields:
      id: .id
      time: .t
      title: .id
`)
	runner := &fakeRunner{stdout: `[{"id":"a","t":"2026-05-04T09:00:00Z"}]`}
	_, err := sources.Collect(t.Context(), runner, source, testEnvironment(t),
		sources.DayWindow(testDay), time.Minute, testDay)
	if err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("Collect() error = %v, want {{home}} planning to fail without HOME", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %d, want planning to fail before execution", len(runner.calls))
	}
}

func TestBuildRunCommandNeverParsesShellSyntax(t *testing.T) {
	source := mustSource(t, logSource)
	command := mustBuildRunCommand(t, source, testEnvironment(t), sources.DayWindow(testDay), time.Minute)
	if command.Argv[0] != "cli" {
		t.Fatalf("command = %+v, want the declared executable invoked directly", command)
	}
}

func TestBuildRunCommandCopiesTheResolvedEnvironment(t *testing.T) {
	source := &core.Source{Run: []string{"git", "log"}}
	env := sources.Environment{Root: "/base", Env: map[string]string{"PATH": "/base/bin:/usr/bin"}}
	command := mustBuildRunCommand(t, source, env, sources.DayWindow(testDay), time.Minute)
	if len(command.Env) != 1 || command.Env["PATH"] != "/base/bin:/usr/bin" {
		t.Fatalf("environment = %v, want only the resolved PATH override", command.Env)
	}
	command.Env["PATH"] = "/changed"
	if env.Env["PATH"] != "/base/bin:/usr/bin" {
		t.Fatal("BuildRunCommand returned the caller's mutable environment map")
	}
}

func TestSourceTimeoutOverridesTheBaseDefault(t *testing.T) {
	source := mustSource(t, logSource+"    timeout: 5s\n")
	command := mustBuildRunCommand(t, source, testEnvironment(t), sources.DayWindow(testDay), time.Minute)
	if command.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want the source's own 5s", command.Timeout)
	}
}

// --- body ---------------------------------------------------------------------------------

const bodySource = `    enabled: true
    layer: events
    run: [cli, --since, "{{date}}"]
    fields:
      id: .id
      time: .t
      title: .id
      repo: .repo
      project: [.project.key, .fallback_project]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --project, "{{project}}"]
`

func TestFetchBodySubstitutesIntoArgvAndNeverAShell(t *testing.T) {
	source := mustSource(t, bodySource)
	runner := &fakeRunner{stdout: "the body"}
	environment := testEnvironment(t)
	record := sources.Record{
		"id": "412", "repo": "fmind/fkf", "fallback_project": "knowledge",
		"t": "2026-05-04T00:00:00Z",
	}
	body, command, err := sources.FetchBody(context.Background(), runner, source, source.Fields, environment, record, time.Minute)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if body != "the body" {
		t.Fatalf("body = %q", body)
	}
	want := []string{"gh", "pr", "view", "412", "--repo", "fmind/fkf", "--project", "knowledge"}
	if strings.Join(command.Argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", command.Argv, want)
	}
	if command.Dir != string(filepath.Separator) || command.ForbiddenRoot != environment.Root {
		t.Fatalf("body command boundary = dir %q, root %q; want neutral cwd and protected base %q",
			command.Dir, command.ForbiddenRoot, environment.Root)
	}
	if command.Source != "" {
		t.Fatalf("body command source = %q; collected argv values must stay out of run diagnostics", command.Source)
	}
}

func TestFetchBodyFailsPlanningAHomePlaceholderWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	source := mustSource(t, `    enabled: true
    layer: events
    run: [cli]
    fields:
      id: .id
      time: .t
      title: .id
    body: [cli, "{{home}}", "{{id}}"]
`)
	runner := &fakeRunner{stdout: "body"}
	_, _, err := sources.FetchBody(t.Context(), runner, source,
		source.Fields, testEnvironment(t),
		sources.Record{"id": "a"}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("FetchBody() error = %v, want {{home}} planning to fail without HOME", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %d, want planning to fail before execution", len(runner.calls))
	}
}

func TestFetchBodyKeepsStaticPlaceholdersAheadOfSameNamedFields(t *testing.T) {
	source := mustSource(t, strings.Replace(bodySource,
		"      project: [.project.key, .fallback_project]\n",
		"      project: [.project.key, .fallback_project]\n      base: .provider_base\n", 1))
	source.Body = append(source.Body, "--base", "{{base}}")
	environment := testEnvironment(t)
	runner := &fakeRunner{stdout: "ok"}
	record := sources.Record{
		"id": "412", "repo": "fmind/fkf", "fallback_project": "knowledge",
		"provider_base": "--help", "t": "2026-05-04T00:00:00Z",
	}
	_, command, err := sources.FetchBody(t.Context(), runner, source, source.Fields, environment, record, time.Minute)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if got := command.Argv[len(command.Argv)-1]; got != environment.Root {
		t.Fatalf("{{base}} = %q, want trusted base %q rather than the same-named record field", got, environment.Root)
	}
}

// TestFetchBodyRefusesAnOptionOrInvisibleIdBeforeExec prevents argument confusion and terminal
// deception while leaving ordinary Unicode and punctuation available to provider identities.
func TestFetchBodyRefusesAnOptionOrInvisibleIdBeforeExec(t *testing.T) {
	source := mustSource(t, bodySource)
	runner := &fakeRunner{stdout: "never reached"}
	for _, id := range []string{"--help", "-Rattacker/repo", "@response-file", "a\tb", "a\nb", "a\u200bb"} {
		record := sources.Record{
			"id": id, "repo": "fmind/fkf", "fallback_project": "knowledge",
			"t": "2026-05-04T00:00:00Z",
		}
		_, _, err := sources.FetchBody(context.Background(), runner, source, source.Fields, testEnvironment(t), record, time.Minute)
		if err == nil {
			t.Fatalf("FetchBody() accepted the id %q", id)
		}
		if !strings.Contains(err.Error(), "safe opaque argv") {
			t.Fatalf("error = %v, want the option or invisible-value refusal", err)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("the runner was called %d time(s); a refused value must never reach exec", len(runner.calls))
	}
}

func TestFetchBodyPassesUnicodeAndPunctuationAsOneArgvValue(t *testing.T) {
	source := mustSource(t, bodySource)
	runner := &fakeRunner{stdout: "body"}
	id := "révision 42; $(not-a-shell) | 👍"
	record := sources.Record{
		"id": id, "repo": "fmind/fkf", "fallback_project": "knowledge",
		"t": "2026-05-04T00:00:00Z",
	}
	_, command, err := sources.FetchBody(t.Context(), runner, source, source.Fields, testEnvironment(t), record, time.Minute)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if command.Argv[3] != id {
		t.Fatalf("command = %+v, want Unicode identity preserved as one non-shell argv value", command)
	}
}

// TestFetchBodyAllowsPathShapedValues records what the charset is FOR. It bounds what may be
// handed to exec, not what may look like a path: `/` and `.` are needed for owner/name and for
// message ids, there is no shell to escape, and the CLI a source names decides what its own
// argument means.
func TestFetchBodyAllowsPathShapedValues(t *testing.T) {
	source := mustSource(t, bodySource)
	runner := &fakeRunner{stdout: "ok"}
	record := sources.Record{
		"id": "../../etc/passwd", "repo": "fmind/fkf", "fallback_project": "knowledge",
		"t": "2026-05-04T00:00:00Z",
	}
	_, command, err := sources.FetchBody(context.Background(), runner, source, source.Fields, testEnvironment(t), record, time.Minute)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if command.Argv[3] != "../../etc/passwd" {
		t.Fatalf("argv[3] = %q, want the value passed through verbatim", command.Argv[3])
	}
}

func TestFetchBodySaysWhenASourceHasNone(t *testing.T) {
	source := mustSource(t, logSource)
	_, _, err := sources.FetchBody(context.Background(), &fakeRunner{}, source, source.Fields, testEnvironment(t),
		sources.Record{"id": "a"}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "declares no body:") {
		t.Fatalf("FetchBody() error = %v, want it to say bodies are not fetchable here", err)
	}
}

// TestNDJSONLinesAreRecords is the shape `gh api --paginate --jq '.[]'` actually emits: one JSON
// OBJECT per line. The array rule belongs to `format: json`, where the command prints one
// document; applying it to ndjson rejected a correct source.
func TestNDJSONLinesAreRecords(t *testing.T) {
	source := mustSource(t, logSource+"    format: ndjson\n")
	records, err := sources.DecodeRecords(source,
		`{"id":"a","t":"2026-05-04T09:00:00Z"}`+"\n"+`{"id":"b","t":"2026-05-04T10:00:00Z"}`+"\n")
	if err != nil {
		t.Fatalf("DecodeRecords() error = %v", err)
	}
	if len(records) != 2 || records[0]["id"] != "a" || records[1]["id"] != "b" {
		t.Fatalf("records = %v, want one per line", records)
	}
	// A line holding a batch is accepted too: a CLI that groups is not wrong, only different.
	batched, err := sources.DecodeRecords(source, `[{"id":"a","t":"2026-05-04T09:00:00Z"}]`+"\n")
	if err != nil || len(batched) != 1 {
		t.Fatalf("DecodeRecords() = %v, %v", batched, err)
	}
	// json still refuses a bare object, because there the whole output is one document.
	asJSON := mustSource(t, logSource)
	if _, err := sources.DecodeRecords(asJSON, `{"id":"a","t":"2026-05-04T09:00:00Z"}`); err == nil {
		t.Fatal("format: json must refuse a top-level object; it is an envelope, not a record")
	}
}

const windowedSource = `    enabled: true
    layer: events
    window: true
    run: [cli, --since, "{{start}}", --until, "{{end}}"]
    fields:
      id: .id
      time: .t
      title: .subject
`

// TestCollectWindowBucketsRecordsByTheirOwnDeclaredDay is the point of `window: true`: one
// command execution, one document per requested day, with each record filed under the LOCAL
// calendar day its own time falls in — never the day the command happened to be invoked with.
//
// Boundary timestamps are derived from sources.DayWindow rather than hand-written UTC strings,
// because "the day boundary" is a property of the LOCAL zone the test happens to run under —
// hard-coding e.g. "23:59:59Z" as "just before the next day" is only true when the local zone
// is UTC, and is wrong by exactly the zone's offset everywhere else.
func TestCollectWindowBucketsRecordsByTheirOwnDeclaredDay(t *testing.T) {
	source := mustSource(t, windowedSource)
	day1, day2, day3 := dayBounds(t, "2026-05-04"), dayBounds(t, "2026-05-05"), dayBounds(t, "2026-05-06")
	stdout := fmt.Sprintf(`[
		{"id":"a1","t":%q,"subject":"day one, mid-morning"},
		{"id":"a2","t":%q,"subject":"day two, its last instant"},
		{"id":"a3","t":%q,"subject":"day three, its first instant"},
		{"id":"a4","t":%q,"subject":"day two, exactly its own midnight"}
	]`,
		day1.start.Add(9*time.Hour).Format(time.RFC3339),
		day2.end.Add(-time.Second).Format(time.RFC3339), // still day two: End is exclusive
		day3.start.Format(time.RFC3339),
		day2.start.Format(time.RFC3339),
	)
	runner := &fakeRunner{stdout: stdout}
	dates := []string{"2026-05-04", "2026-05-05", "2026-05-06"}
	documents, err := sources.CollectWindow(t.Context(), runner, source, testEnvironment(t),
		sources.Window{}, dates, time.Minute, testDay)
	if err != nil {
		t.Fatalf("CollectWindow() error = %v", err)
	}
	if len(documents) != 3 {
		t.Fatalf("documents = %d, want one per requested day", len(documents))
	}
	if len(runner.calls) != 1 {
		t.Fatalf("the runner was called %d time(s); a windowed source must run the command exactly once", len(runner.calls))
	}
	cases := map[string][]string{
		"2026-05-04": {"a1"},
		"2026-05-05": {"a4", "a2"}, // midnight belongs to the day it opens, not the day before
		"2026-05-06": {"a3"},
	}
	for date, wantIDs := range cases {
		doc, ok := documents[date]
		if !ok {
			t.Fatalf("no document for %s", date)
		}
		if doc.Count != len(wantIDs) {
			t.Fatalf("%s: count = %d, want %d (records: %+v)", date, doc.Count, len(wantIDs), doc.Records)
		}
		encoded, err := sources.EncodeDocument(doc)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"windowed"`) || strings.Contains(string(encoded), `"command"`) {
			t.Fatalf("%s: stored window collection exposes planner metadata: %s", date, encoded)
		}
		if doc.Date != date {
			t.Fatalf("%s: Date = %q, want it to match", date, doc.Date)
		}
	}
}

// dayBounds gives a test the absolute Start/End of one local civil day, parsed from
// sources.DayWindow's own output, so a test fixture is correct under whatever local zone the
// suite happens to run in rather than assuming UTC.
func dayBounds(t *testing.T, date string) struct{ start, end time.Time } {
	t.Helper()
	day, err := sources.ParseDayInLocation(date, testDay.Location())
	if err != nil {
		t.Fatal(err)
	}
	window := sources.DayWindow(day)
	start, err := time.Parse(time.RFC3339, window.Start)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339, window.End)
	if err != nil {
		t.Fatal(err)
	}
	return struct{ start, end time.Time }{start, end}
}

// TestCollectWindowFilesAnEmptyDayAsCompleteWithZero is what makes a windowed range behave
// like a day-at-a-time sync for the days a source truly had nothing to say about: absence is
// a complete document with zero records, not a missing one.
func TestCollectWindowFilesAnEmptyDayAsCompleteWithZero(t *testing.T) {
	source := mustSource(t, windowedSource)
	runner := &fakeRunner{stdout: `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"only day one"}]`}
	dates := []string{"2026-05-04", "2026-05-05", "2026-05-06"}
	documents, err := sources.CollectWindow(t.Context(), runner, source, testEnvironment(t),
		sources.Window{}, dates, time.Minute, testDay)
	if err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []string{"2026-05-05", "2026-05-06"} {
		doc := documents[quiet]
		if doc.Count != 0 || len(doc.Records) != 0 {
			t.Fatalf("%s: count = %d, want a complete document with zero records", quiet, doc.Count)
		}
	}
}

func TestCollectWindowBucketsDateOnlyEventTimeByCivilDate(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata for America/New_York on this system: %v", err)
	}
	source := mustSource(t, windowedSource)
	runner := &fakeRunner{stdout: `[{"id":"all-day","t":"2026-05-04","subject":"all-day event"}]`}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, loc)
	documents, err := sources.CollectWindow(t.Context(), runner, source, testEnvironment(t),
		sources.Window{}, []string{"2026-05-04"}, time.Minute, now)
	if err != nil {
		t.Fatalf("CollectWindow() rejected a date-only record belonging to its civil day west of UTC: %v", err)
	}
	if documents["2026-05-04"].Count != 1 {
		t.Fatalf("document = %+v, want the all-day record bucketed by its civil date", documents["2026-05-04"])
	}
}

// TestCollectWindowFailsTheWholeRangeOnAnOutOfWindowRecord is the completeness rule extended
// to a range: a record whose declared time falls outside every requested day means the range
// is not what was asked for, and nothing partial is written — exactly as a missing identity
// fails one day today.
func TestCollectWindowFailsTheWholeRangeOnAnOutOfWindowRecord(t *testing.T) {
	source := mustSource(t, windowedSource)
	runner := &fakeRunner{stdout: `[
		{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"in range"},
		{"id":"a2","t":"2026-06-15T09:00:00Z","subject":"a stale bug in the provider's date filter"}
	]`}
	dates := []string{"2026-05-04", "2026-05-05"}
	_, err := sources.CollectWindow(t.Context(), runner, source, testEnvironment(t),
		sources.Window{}, dates, time.Minute, testDay)
	if err == nil {
		t.Fatal("CollectWindow() succeeded, want the out-of-window record to fail the whole range")
	}
	if !strings.Contains(err.Error(), "outside the requested window") {
		t.Fatalf("error = %v, want it to name the out-of-window failure", err)
	}
}

// TestCollectWindowRunsTheCommandOnceAcrossADSTSpringForwardDay proves the bucketing survives
// exactly the day sources.DayWindow was fixed for: a civil day whose local midnight does not
// exist still receives its own records, filed under the correct calendar date, from one
// command run spanning it.
func TestCollectWindowRunsTheCommandOnceAcrossADSTSpringForwardDay(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata for America/Sao_Paulo on this system: %v", err)
	}
	source := mustSource(t, windowedSource)
	// 2015-10-18 is a spring-forward day in this zone: local midnight does not exist.
	runner := &fakeRunner{stdout: `[
		{"id":"before","t":"2015-10-17T20:00:00Z","subject":"the day before"},
		{"id":"during","t":"2015-10-18T12:00:00Z","subject":"the short day itself"},
		{"id":"after","t":"2015-10-19T20:00:00Z","subject":"the day after"}
	]`}
	noon := parseNoonIn(t, "2015-10-18", loc)
	dates := []string{"2015-10-17", "2015-10-18", "2015-10-19"}
	_ = noon // the dates slice is what CollectWindow buckets against; noon only proves the zone loads
	documents, err := sources.CollectWindow(t.Context(), runner, source, testEnvironment(t),
		sources.Window{}, dates, time.Minute, testDay)
	if err != nil {
		t.Fatalf("CollectWindow() error = %v", err)
	}
	if documents["2015-10-18"].Count != 1 || documents["2015-10-18"].Records[0]["id"] != "during" {
		t.Fatalf("2015-10-18 = %+v, want exactly the record inside the short day", documents["2015-10-18"])
	}
}
