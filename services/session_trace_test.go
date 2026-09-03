package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

const sessionTraceConfig = `name: trace-test
layers: {tasks: true}
sources:
  agent-session-traces:
    enabled: true
    layer: tasks
    run: [agent-session-trace.sh, "{{start}}", "{{end}}"]
    requires: [agent-session-trace.sh, find, git, jq]
    window: true
`

func testSessionTraceInput() services.SessionTraceInput {
	return services.SessionTraceInput{
		ID: "codex:abc-123", Harness: "codex", SID: "abc-123",
		FirstAt: "2026-05-09T08:00:00Z", LastAt: "2026-05-09T10:00:00Z",
		CWD: "/workspace/kagglathon", Repo: "fmind/kagglathon", Model: "gpt-test",
		Requests:      []string{"Implement the session trace.\n[untrusted link](https://evil.example.test)"},
		Files:         []string{" M services/session_trace.go"},
		Verification:  []string{"go test ./services -run TestSessionTrace"},
		LastAssistant: "Implemented the bounded trace.\nAll focused tests pass.",
	}
}

func encodeSessionTraceInputs(t *testing.T, inputs ...services.SessionTraceInput) string {
	t.Helper()
	encoded, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestImportSessionTracesWritesOneBoundedIdempotentSkeleton(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	window := sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"}
	result, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, testSessionTraceInput()), window)
	if err != nil {
		t.Fatal(err)
	}
	wantURI := "tasks/2026-05-09/kagglathon-codex-abc-123/TASKS.md"
	if result.Written != 1 || result.Existing != 0 || len(result.Paths) != 1 || result.Paths[0] != wantURI {
		t.Fatalf("ImportSessionTraces() = %+v, want one stable repository/session path", result)
	}
	read, err := services.Read(t.Context(), base, wantURI, services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Page == nil || len(read.Page.Links) != 0 {
		t.Fatalf("generated trace page = %+v, want untrusted request Markdown kept inside a code block", read.Page)
	}
	for _, marker := range []string{
		"## 1. Implement the session trace", "Files changed from git at collection time",
		"Verification commands seen", "Last assistant message", "## Learned",
	} {
		if !strings.Contains(read.Text, marker) {
			t.Errorf("trace omits %q:\n%s", marker, read.Text)
		}
	}

	absolute := mustResolve(t, base, wantURI)
	authored := read.Text + "\n- Owner-reviewed durable lesson.\n"
	if err := os.WriteFile(absolute, []byte(authored), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	second, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, testSessionTraceInput()), window)
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Existing != 1 {
		t.Fatalf("second import = %+v, want the owner-edited trace preserved", second)
	}
	got, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != authored {
		t.Fatal("a later session-store generation overwrote owner-authored learning")
	}
}

func TestImportSessionTracesUsesSafeFencesForCapturedMarkdown(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	input := testSessionTraceInput()
	input.Requests = []string{"Inspect the trace.\n\n````\n[untrusted link](https://evil.example.test)\n   \nFinish."}
	input.LastAssistant = " Implemented with meaningful leading whitespace.  "
	result, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, input),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(mustResolve(t, base, result.Paths[0]))
	if err != nil {
		t.Fatal(err)
	}
	requestBlock := "`````text\n" + input.Requests[0] + "\n`````\n"
	filesBlock := "```text\n" + strings.Join(input.Files, "\n") + "\n```\n"
	assistantBlock := "```text\n" + input.LastAssistant + "\n```\n"
	if !strings.Contains(string(content), requestBlock) || !strings.Contains(string(content), filesBlock) ||
		!strings.Contains(string(content), assistantBlock) {
		t.Fatal("generated trace did not preserve captured text inside collision-safe fences")
	}
	read, err := services.Read(t.Context(), base, result.Paths[0], services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Page == nil || len(read.Page.Links) != 0 {
		t.Fatalf("generated trace page = %+v, want untrusted Markdown kept inert", read.Page)
	}
}

func TestImportSessionTracesFilesCrossMidnightSessionByCompletionDay(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	input := testSessionTraceInput()
	input.FirstAt = "2026-05-08T23:50:00Z"
	input.LastAt = "2026-05-09T00:10:00Z"
	result, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, input),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	want := "tasks/2026-05-09/kagglathon-codex-abc-123/TASKS.md"
	if !slices.Equal(result.Paths, []string{want}) {
		t.Fatalf("cross-midnight paths = %v, want completion-day path %s", result.Paths, want)
	}
}

func TestImportSessionTracesBindsTheHarnessIntoTheTaskPath(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	codex := testSessionTraceInput()
	gemini := testSessionTraceInput()
	gemini.ID, gemini.Harness = "gemini:abc-123", "gemini"
	result, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, codex, gemini),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tasks/2026-05-09/kagglathon-codex-abc-123/TASKS.md",
		"tasks/2026-05-09/kagglathon-gemini-abc-123/TASKS.md",
	}
	if result.Written != 2 || !slices.Equal(result.Paths, want) {
		t.Fatalf("cross-harness trace paths = %+v, want %v", result, want)
	}
}

func TestImportSessionTracesKeepsCaseDistinctSessionIDsDistinct(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	upper := testSessionTraceInput()
	upper.ID, upper.SID = "codex:ABC", "ABC"
	lower := testSessionTraceInput()
	lower.ID, lower.SID = "codex:abc", "abc"
	result, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, upper, lower),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Written != 2 || len(result.Paths) != 2 || result.Paths[0] == result.Paths[1] {
		t.Fatalf("case-distinct session paths = %+v, want two distinct traces", result)
	}
}

func TestImportSessionTracesKeepsNormalizedAndLiteralSessionIDsDistinct(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	window := sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"}
	normalized := testSessionTraceInput()
	normalized.ID, normalized.SID = "codex:ABC", "ABC"
	first, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, normalized), window)
	if err != nil {
		t.Fatal(err)
	}
	literal := testSessionTraceInput()
	literal.ID, literal.SID = "codex:abc-084d7f85a9ec", "abc-084d7f85a9ec"
	second, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, literal), window)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || second.Written != 1 || len(first.Paths) != 1 || len(second.Paths) != 1 ||
		first.Paths[0] == second.Paths[0] {
		t.Fatalf("normalized path = %+v, literal path = %+v; want two distinct traces", first, second)
	}
}

func TestImportSessionTracesFailsBeforeWritingAnyInvalidBatch(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	valid := testSessionTraceInput()
	invalid := testSessionTraceInput()
	invalid.ID = "codex:different"
	_, err = services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, valid, invalid),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "does not match harness and sid") {
		t.Fatalf("ImportSessionTraces() error = %v, want the closed identity contract", err)
	}
	directory := filepath.Join(base.Root(), string(core.LayerTasks))
	entries, readErr := os.ReadDir(directory)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid batch wrote task directories: %v", entries)
	}
}

func TestSyncDoesNotMarkInvalidSessionTraceOutput(t *testing.T) {
	valid := encodeSessionTraceInputs(t, testSessionTraceInput())
	invalidUTF8 := strings.Replace(valid, "Implement", "Imple"+string([]byte{0xff})+"ment", 1)
	for name, test := range map[string]struct {
		stdout string
		want   string
	}{
		"null":          {stdout: "null", want: "JSON array"},
		"invalid UTF-8": {stdout: invalidUTF8, want: "valid UTF-8"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{"": test.stdout}}
			base := newBase(t, sessionTraceConfig, runner)
			trust(t, base)
			report, err := services.Sync(t.Context(), base, services.SyncRequest{
				Date: "2026-05-09", NoGraph: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Failed != 1 || len(report.Units) != 1 ||
				!strings.Contains(report.Units[0].Error, test.want) {
				t.Fatalf("invalid session output report = %+v, want one failure containing %q", report, test.want)
			}
			marker := filepath.Join(
				base.Root(), ".agents", "tmp", "sync", "tasks", "agent-session-traces", "2026-05-09.done",
			)
			if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid session output left a completion marker: %v", statErr)
			}
		})
	}
}

func TestSyncDoesNotMarkACanceledEmptySessionTraceImport(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	trust(t, base)
	ctx, cancel := context.WithCancel(t.Context())
	base.Runner = sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		cancel()
		return "[]", nil
	})
	_, err := services.Sync(ctx, base, services.SyncRequest{Date: "2026-05-09", NoGraph: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled task sync error = %v, want context.Canceled", err)
	}
	marker := filepath.Join(
		base.Root(), ".agents", "tmp", "sync", "tasks", "agent-session-traces", "2026-05-09.done",
	)
	if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled empty import left a completion marker: %v", statErr)
	}
}

func TestTaskTraceMarkerBindsTheCanonicalLocalDayWindow(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "[]"}}
	base := newBase(t, sessionTraceConfig, runner)
	base.Now = func() time.Time {
		return time.Date(2026, 5, 10, 12, 0, 0, 0, time.FixedZone("UTC+02", 2*60*60))
	}
	trust(t, base)
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", NoGraph: true,
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(
		base.Root(), ".agents", "tmp", "sync", "tasks", "agent-session-traces", "2026-05-09.done",
	)
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"source\":\"agent-session-traces\",\"date\":\"2026-05-09\"," +
		"\"start\":\"2026-05-08T22:00:00Z\",\"end\":\"2026-05-09T22:00:00Z\"}\n"
	if string(content) != want {
		t.Fatalf("task marker = %q, want canonical source/day/window %q", content, want)
	}

	base.Now = func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.Due {
		t.Fatalf("timezone-mismatched task marker was accepted: %+v", preflight)
	}
}

func TestStatusUsesSuccessfulEmptyTaskImportAsFreshnessEvidence(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "[]"}}
	base := newBase(t, sessionTraceConfig, runner)
	trust(t, base)
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", NoGraph: true,
	}); err != nil {
		t.Fatal(err)
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	var traceSource *services.SourceStatus
	for index := range status.Sources {
		if status.Sources[index].Name == "agent-session-traces" {
			traceSource = &status.Sources[index]
			break
		}
	}
	if traceSource == nil {
		t.Fatal("status omitted agent-session-traces")
	}
	if traceSource.Stale || traceSource.LastCollectedAt != "2026-05-10T00:00:00Z" || traceSource.LagHours != 12 {
		t.Fatalf("task source freshness = %+v, want the completed local-day boundary", traceSource)
	}
}

func TestSyncTreatsACorruptTaskTraceMarkerAsDueAndRepairsIt(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": "[]"}}
	base := newBase(t, sessionTraceConfig, runner)
	trust(t, base)
	marker := filepath.Join(
		base.Root(), ".agents", "tmp", "sync", "tasks", "agent-session-traces", "2026-05-09.done",
	)
	if err := os.MkdirAll(filepath.Dir(marker), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("not a completion marker\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.Due {
		t.Fatalf("corrupt task marker was accepted: %+v", preflight)
	}
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", NoGraph: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want corrupt marker to make one task import due", len(runner.calls))
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "not a completion marker") {
		t.Fatalf("task sync did not repair corrupt marker: %q", content)
	}
}

func TestSyncRunsATasksSourceOncePerDayAndNeverOverwrites(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"2026-05-08T00:00:00Z 2026-05-09T00:00:00Z": "[]",
		"2026-05-09T00:00:00Z 2026-05-10T00:00:00Z": encodeSessionTraceInputs(t, testSessionTraceInput()),
	}}
	base := newBase(t, sessionTraceConfig, runner)
	trust(t, base)
	request := services.SyncRequest{Days: 2, NoGraph: true}
	first, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || first.Skipped != 1 || first.Records != 1 || len(first.Units) != 2 {
		t.Fatalf("first sync = %+v, want one imported trace across two daily units", first)
	}
	wantDates := []string{"2026-05-08", "2026-05-09"}
	if got := []string{first.Units[0].Date, first.Units[1].Date}; !slices.Equal(got, wantDates) {
		t.Fatalf("task unit dates = %v, want %v", got, wantDates)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d, want one bounded session import per completed day", len(runner.calls))
	}
	for _, call := range runner.calls {
		start, startErr := time.Parse(time.RFC3339, call.Window.Start)
		end, endErr := time.Parse(time.RFC3339, call.Window.End)
		if startErr != nil || endErr != nil || end.Sub(start) != 24*time.Hour {
			t.Fatalf("task import window = %+v, want one UTC day", call.Window)
		}
	}

	second, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Skipped != 2 || len(runner.calls) != 2 {
		t.Fatalf("second sync = %+v, calls=%d; want the completed-range marker to avoid another helper run", second, len(runner.calls))
	}
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{Days: 2, IfDue: true})
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Due {
		t.Fatalf("completed tasks range is still due: %+v", preflight)
	}

	before := len(runner.calls)
	dry, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 2, DryRun: true, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before || len(dry.Units) != 2 ||
		dry.Units[0].Outcome != services.OutcomePlanned || dry.Units[1].Outcome != services.OutcomePlanned {
		t.Fatalf("dry run = %+v, calls=%d; want command disclosure without execution", dry, len(runner.calls))
	}
	if got := []string{dry.Units[0].Date, dry.Units[1].Date}; !slices.Equal(got, wantDates) {
		t.Fatalf("dry-run task unit dates = %v, want %v", got, wantDates)
	}
}

func TestSyncRollsBackNewTaskTracesWhenRangeMarkerFails(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"": encodeSessionTraceInputs(t, testSessionTraceInput()),
	}}
	base := newBase(t, sessionTraceConfig, runner)
	trust(t, base)
	marker := filepath.Join(
		base.Root(), ".agents", "tmp", "sync", "tasks", "agent-session-traces", "2026-05-09.done",
	)
	if err := os.MkdirAll(marker, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	report, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", Force: true, NoGraph: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 1 || report.Written != 0 || report.Complete {
		t.Fatalf("marker-failed sync = %+v, want one failed all-or-nothing unit", report)
	}
	target := filepath.Join(
		base.Root(), "tasks", "2026-05-09", "kagglathon-codex-abc-123", core.TaskTraceFile,
	)
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed marker publication left a task trace: %v", statErr)
	}
	if _, statErr := os.Lstat(mustResolve(t, base, core.GraphFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed task unit changed the graph cache: %v", statErr)
	}
}
