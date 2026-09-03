package services_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func syncBase(t *testing.T, stdout string) (*services.Base, *fakeRunner) {
	t.Helper()
	responses := map[string]string{"": stdout}
	if stdout == oneRecord {
		for _, date := range []string{"2026-05-07", "2026-05-08", "2026-05-09"} {
			responses["--since "+date] = strings.Replace(oneRecord, "2026-05-09", date, 1)
		}
	}
	runner := &fakeRunner{responses: responses}
	base := newBase(t, baseConfig, runner)
	trust(t, base)
	return base, runner
}

const oneRecord = `[{"id":"a1","t":"2026-05-09T09:00:00Z","subject":"s","link":"https://x.test/a1","repo_uri":"repo:o/r","author_uris":["person:email/m@x.test"]}]`

type authSyncRunner struct {
	mutex sync.Mutex
	calls []sources.Command
}

type authExitFailure struct{}

func (authExitFailure) Error() string         { return "private auth error" }
func (authExitFailure) ExitCode() (int, bool) { return 1, true }

func (runner *authSyncRunner) Run(_ context.Context, command sources.Command) (string, error) {
	runner.mutex.Lock()
	runner.calls = append(runner.calls, command)
	runner.mutex.Unlock()
	if len(command.Argv) > 0 && command.Argv[0] == "gws" {
		return "private auth output", authExitFailure{}
	}
	return "[]", nil
}

func TestSyncAuthProbeTreatsTrustDriftAsAUnitFailure(t *testing.T) {
	base := newBase(t, authSyncConfig, nil)
	base.Runner = sources.RunnerFunc(func(_ context.Context, command sources.Command) (string, error) {
		if command.Argv[0] == "gh" || command.Argv[0] == "gws" {
			return "", fmt.Errorf("revalidate declared command trust: %w", core.ErrUntrusted)
		}
		return "[]", nil
	})
	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 2 || len(report.AuthRequired) != 0 || report.Complete {
		t.Fatalf("sync report = %+v, want both auth-gated units failed on trust drift", report)
	}
	for _, unit := range report.Units {
		if unit.Source != "local" && !strings.Contains(unit.Error, core.ErrUntrusted.Error()) {
			t.Fatalf("auth-gated unit = %+v, want trust failure preserved", unit)
		}
	}
}

func (runner *authSyncRunner) countExecutable(name string) int {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	count := 0
	for _, command := range runner.calls {
		if len(command.Argv) > 0 && command.Argv[0] == name {
			count++
		}
	}
	return count
}

const authSyncConfig = `name: auth-sync
layers: {events: true}
sources:
  local:
    enabled: true
    layer: events
    run: [local-cli, "{{date}}"]
    fields: {id: .id, time: .time, title: .id}
  github:
    enabled: true
    layer: events
    auth: [gh, auth, status]
    run: [github-cli, "{{date}}"]
    fields: {id: .id, time: .time, title: .id}
  google:
    enabled: true
    layer: events
    auth: [gws, auth, status]
    run: [google-cli, "{{date}}"]
    fields: {id: .id, time: .time, title: .id}
`

func TestSyncProbesAuthOncePerDueSourceAndKeepsTheRunComplete(t *testing.T) {
	runner := &authSyncRunner{}
	base := newBase(t, authSyncConfig, nil)
	base.Runner = runner
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 2, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Failed != 0 || !slices.Equal(report.AuthRequired, []string{"google"}) {
		t.Fatalf("report = %+v, want a complete run naming only google as auth-required", report)
	}
	if runner.countExecutable("gh") != 1 || runner.countExecutable("gws") != 1 {
		t.Fatalf("auth calls: gh=%d gws=%d, want one per due source",
			runner.countExecutable("gh"), runner.countExecutable("gws"))
	}
	if runner.countExecutable("local-cli") != 2 || runner.countExecutable("github-cli") != 2 ||
		runner.countExecutable("google-cli") != 0 {
		t.Fatalf("collection calls: local=%d github=%d google=%d; want ready sources collected and google skipped",
			runner.countExecutable("local-cli"), runner.countExecutable("github-cli"), runner.countExecutable("google-cli"))
	}
	for _, unit := range report.Units {
		if unit.Source == "google" && (unit.Outcome != services.OutcomeAuthRequired || unit.Error != "") {
			t.Fatalf("google unit = %+v, want a non-failing auth-required outcome with no private diagnostic", unit)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private auth") {
		t.Fatalf("report leaked discarded auth output/error: %s", encoded)
	}
}

func TestSyncDoesNotProbeAuthWithoutDueWorkOrDuringDryRun(t *testing.T) {
	runner := &authSyncRunner{}
	base := newBase(t, authSyncConfig, nil)
	base.Runner = runner
	trust(t, base)

	if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("dry run executed %d command(s), want none", len(runner.calls))
	}

	readyOnly := strings.Replace(authSyncConfig, "    auth: [gws, auth, status]\n", "", 1)
	readyBase := newBase(t, readyOnly, nil)
	readyBase.Runner = runner
	trust(t, readyBase)
	if _, err := services.Sync(t.Context(), readyBase, services.SyncRequest{Days: 1, NoGraph: true}); err != nil {
		t.Fatal(err)
	}
	before := len(runner.calls)
	if _, err := services.Sync(t.Context(), readyBase, services.SyncRequest{Days: 1, NoGraph: true}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before {
		t.Fatalf("no-due sync added %d command(s), want no auth or collection", len(runner.calls)-before)
	}
}

func TestSyncRunsBaseReadingSourcesAfterOrdinaryCollection(t *testing.T) {
	const config = `name: dependent-sync
layers: {events: true}
sync: {days: 1, concurrency: 4}
sources:
  calendar:
    enabled: true
    layer: events
    window: true
    run: [calendar-cli, "{{start}}", "{{end}}"]
    fields: {id: .id, time: .time, title: .title}
  notes:
    enabled: true
    layer: events
    window: true
    run: [notes-cli, "{{base}}", "{{start}}", "{{end}}"]
    fields: {id: .id, time: .time, title: .title}
`
	base := newBase(t, config, nil)
	notesStarted := make(chan struct{})
	base.Runner = sources.RunnerFunc(func(_ context.Context, command sources.Command) (string, error) {
		switch command.Argv[0] {
		case "calendar-cli":
			timer := time.NewTimer(100 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-notesStarted:
			case <-timer.C:
			}
			return `[{"id":"calendar","time":"2026-05-09T09:00:00Z","title":"Calendar"}]`, nil
		case "notes-cli":
			close(notesStarted)
			calendar := filepath.Join(base.Root(), "events", "2026-05-09", "calendar.json")
			if _, err := os.Stat(calendar); err != nil {
				return "", fmt.Errorf("notes observed collection before its durable calendar dependency: %w", err)
			}
			return `[{"id":"notes","time":"2026-05-09T09:05:00Z","title":"Notes"}]`, nil
		default:
			return "", fmt.Errorf("unexpected command %q", command.Argv[0])
		}
	})
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Written != 2 || report.Failed != 0 {
		t.Fatalf("sync report = %+v, want the base reader after the durable calendar write", report)
	}
}

func TestSyncPreviewProbesAuthAndWritesNothing(t *testing.T) {
	runner := &authSyncRunner{}
	base := newBase(t, authSyncConfig, nil)
	base.Runner = runner
	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"google"}, Preview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || !slices.Equal(report.AuthRequired, []string{"google"}) || report.Preview != nil {
		t.Fatalf("preview report = %+v, want a complete auth-required result without a sample", report)
	}
	if runner.countExecutable("gws") != 1 || runner.countExecutable("google-cli") != 0 {
		t.Fatalf("preview calls: gws=%d google=%d, want only the readiness probe",
			runner.countExecutable("gws"), runner.countExecutable("google-cli"))
	}
}

func TestSyncCollectsTheMissingDays(t *testing.T) {
	base, runner := syncBase(t, oneRecord)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 3 || report.Failed != 0 || !report.Complete {
		t.Fatalf("report = %+v, want three complete days", report)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("the runner was called %d time(s), want one per day", len(runner.calls))
	}
	// Today is never collected: a day that is still happening cannot be complete.
	today := testClock.Format(time.DateOnly)
	for _, unit := range report.Units {
		if unit.Date == today {
			t.Fatalf("today (%s) was collected", today)
		}
	}
	// Re-running skips what is already there rather than re-fetching it.
	second, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Skipped != 3 {
		t.Fatalf("second run = %+v, want everything skipped as existing", second)
	}
	forced, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if forced.Written != 3 {
		t.Fatalf("--force = %+v, want the days rewritten", forced)
	}
}

func TestSyncBodyPolicyPrefetchesAndExplicitlyRestoresAWipedCache(t *testing.T) {
	config := strings.Replace(baseConfig,
		"    body: [cli, view, \"{{id}}\"]\n",
		"    body: [cli, view, \"{{id}}\"]\n    bodies: sync\n", 1)
	runner := &fakeRunner{responses: map[string]string{
		"cli --since": dayOne,
		"cli view":    "meeting body text",
	}}
	base := newBase(t, config, runner)
	trust(t, base)
	request := services.SyncRequest{Date: "2026-05-04"}
	first, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || first.BodiesCached != 2 || first.BodyFailed != 0 || !first.Complete ||
		first.Graph == nil || first.Index == nil {
		t.Fatalf("first sync = %+v", first)
	}
	read, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
	if err != nil || read.BodyState != "cached" || read.Body != "meeting body text" {
		t.Fatalf("cached read = %+v, %v", read, err)
	}
	if _, err := services.PruneBodies(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-04", NoGraph: true, IfDue: true,
	})
	if err != nil || !preflight.Due {
		t.Fatalf("preflight after body prune = %+v, %v; the newest sync-policy event should restore once", preflight, err)
	}

	runner = &fakeRunner{responses: map[string]string{"cli view": "updated meeting body text"}}
	base.Runner = runner
	restored, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-04", IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Written != 0 || restored.Skipped != 1 || restored.BodiesCached != 2 ||
		restored.BodyFailed != 0 || restored.Graph != nil || restored.Index == nil || len(runner.calls) != 2 {
		t.Fatalf("ordinary cache restore = %+v, calls = %+v; want only the two body fetches", restored, runner.calls)
	}

	if _, err := services.PruneBodies(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	runner = &fakeRunner{responses: map[string]string{"cli view": "meeting body text"}}
	base.Runner = runner
	fetched, err := services.Read(t.Context(), base,
		"events/2026-05-04/synthetic.json#a1", services.ReadOptions{Body: true})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.BodyState != "fetched-and-cached" || fetched.Body != "meeting body text" || len(runner.calls) != 1 {
		t.Fatalf("explicit body read = %+v, calls = %+v", fetched, runner.calls)
	}

	if _, err := services.PruneBodies(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	runner = &fakeRunner{responses: map[string]string{
		"cli --since": dayOne,
		"cli view":    "meeting body text",
	}}
	base.Runner = runner
	forced, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-04", NoGraph: true, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if forced.Written != 1 || forced.BodiesCached != 2 || forced.BodyFailed != 0 || len(runner.calls) != 3 {
		t.Fatalf("forced recollection = %+v, calls = %+v; want collection plus both body prefetches", forced, runner.calls)
	}
}

func TestSyncBodyPolicyRetriesATransientEventBodyFailureOnce(t *testing.T) {
	config := strings.Replace(baseConfig,
		"    body: [cli, view, \"{{id}}\"]\n",
		"    body: [cli, view, \"{{id}}\"]\n    bodies: sync\n", 1)
	base := newBase(t, config, nil)
	bodyCalls := 0
	base.Runner = sources.RunnerFunc(func(_ context.Context, command sources.Command) (string, error) {
		if len(command.Argv) > 1 && command.Argv[0] == "cli" && command.Argv[1] == "view" {
			bodyCalls++
			return "", errors.New("historical backing file disappeared")
		}
		return dayOne, nil
	})
	trust(t, base)
	request := services.SyncRequest{Date: "2026-05-04", NoGraph: true}
	first, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || first.BodyFailed != 2 || first.Complete || bodyCalls != 2 {
		t.Fatalf("first sync = %+v, body calls = %d; want one durable document and two reported prefetch failures",
			first, bodyCalls)
	}
	document, err := base.ReadDocumentContext(t.Context(), "events/2026-05-04/synthetic.json")
	if err != nil || document.Count != 2 || len(document.Records) != 2 {
		t.Fatalf("stored evidence after body failures = %+v, %v; want the complete atomic document", document, err)
	}

	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-04", NoGraph: true, IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.Due {
		t.Fatalf("preflight = %+v; the failed new document must arm one bounded body retry", preflight)
	}
	second, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Skipped != 1 || second.BodyFailed != 2 || second.Complete || bodyCalls != 4 {
		t.Fatalf("second sync = %+v, body calls = %d; want exactly one retry of the failed document",
			second, bodyCalls)
	}
	preflight, err = services.PreflightSync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-04", NoGraph: true, IfDue: true,
	})
	if err != nil || preflight.Due {
		t.Fatalf("preflight after retry = %+v, %v; the exhausted retry must not run forever", preflight, err)
	}
}

func TestSyncBodyPolicyRestoresOnlyTheNewestEventDocumentAfterPrune(t *testing.T) {
	config := strings.Replace(baseConfig,
		"    body: [cli, view, \"{{id}}\"]\n",
		"    body: [cli, view, \"{{id}}\"]\n    bodies: sync\n", 1)
	runner := &fakeRunner{responses: map[string]string{
		"cli --since 2026-05-04": dayOne,
		"cli --since 2026-05-05": strings.ReplaceAll(dayOne, "2026-05-04", "2026-05-05"),
		"cli view":               "meeting body text",
	}}
	base := newBase(t, config, runner)
	base.Now = func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) }
	trust(t, base)
	request := services.SyncRequest{Days: 2, NoGraph: true}
	first, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 2 || first.BodiesCached != 4 || first.BodyFailed != 0 {
		t.Fatalf("first sync = %+v, want two body-prefetched event documents", first)
	}
	if _, err := services.PruneBodies(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	runner = &fakeRunner{responses: map[string]string{"cli view": "restored body text"}}
	base.Runner = runner
	restored, err := services.Sync(t.Context(), base, services.SyncRequest{
		Days: 2, NoGraph: true, IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Written != 0 || restored.Skipped != 2 || restored.BodiesCached != 2 ||
		restored.BodyFailed != 0 || len(runner.calls) != 2 {
		t.Fatalf("restore sync = %+v, calls = %+v; want newest-document body calls only", restored, runner.calls)
	}
	manifestData, err := os.ReadFile(filepath.Join(base.Root(), "bodies", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest services.BodyManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 2 || !manifest.EventAttempts["synthetic"] {
		t.Fatalf("restored manifest = %+v, want two newest-day entries and one event attempt", manifest)
	}
	for uri := range manifest.Entries {
		if !strings.HasPrefix(uri, "events/2026-05-05/synthetic.json#") {
			t.Fatalf("restored historical body %q, want only the newest selected document", uri)
		}
	}
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Days: 2, NoGraph: true, IfDue: true,
	})
	if err != nil || preflight.Due {
		t.Fatalf("preflight after one restore = %+v, %v; attempt must not repeat", preflight, err)
	}
}

func TestSyncBodyPolicyRepairsTheCurrentIndexSnapshot(t *testing.T) {
	config := strings.Replace(baseConfig, "    layer: events\n", "    layer: index\n", 1)
	config = strings.Replace(config,
		"    body: [cli, view, \"{{id}}\"]\n",
		"    body: [cli, view, \"{{id}}\"]\n    bodies: sync\n", 1)
	runner := &fakeRunner{responses: map[string]string{
		"cli --since": dayOne,
		"cli view":    "current body text",
	}}
	base := newBase(t, config, runner)
	trust(t, base)
	request := services.SyncRequest{Days: 1, NoGraph: true}
	first, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || first.BodiesCached != 2 || first.BodyFailed != 0 {
		t.Fatalf("first index sync = %+v", first)
	}
	if _, err := services.PruneBodies(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	preflight, err := services.PreflightSync(t.Context(), base, services.SyncRequest{
		Days: 1, NoGraph: true, IfDue: true,
	})
	if err != nil || !preflight.Due {
		t.Fatalf("index preflight after body prune = %+v, %v; want current snapshot repair due", preflight, err)
	}

	runner = &fakeRunner{responses: map[string]string{"cli view": "current body text"}}
	base.Runner = runner
	restored, err := services.Sync(t.Context(), base, services.SyncRequest{
		Days: 1, NoGraph: true, IfDue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Written != 0 || restored.Skipped != 1 || restored.BodiesCached != 2 ||
		restored.BodyFailed != 0 || len(runner.calls) != 2 {
		t.Fatalf("restored index sync = %+v, calls = %+v; want two body calls and no provider listing", restored, runner.calls)
	}
}

// TestSyncDryRunExecutesNothing is asserted the only way that proves it: the runner is never
// called, and the report still shows exactly what would have run.
func TestSyncDryRunExecutesNothing(t *testing.T) {
	base, runner := syncBase(t, oneRecord)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 2, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("--dry-run called the runner %d time(s)", len(runner.calls))
	}
	if report.Written != 0 || len(report.Units) != 2 {
		t.Fatalf("report = %+v, want two planned units and nothing written", report)
	}
	for _, unit := range report.Units {
		if unit.Outcome != services.OutcomePlanned || !strings.Contains(unit.Command, "cli --since 2026-05-") {
			t.Fatalf("unit = %+v, want the substituted command", unit)
		}
	}
}

func TestSyncIfDuePreflightIsReadOnlyAndSyncRechecksAfterTheLock(t *testing.T) {
	base, runner := syncBase(t, oneRecord)
	request := services.SyncRequest{Days: 1, NoGraph: true, IfDue: true}
	preflight, err := services.PreflightSync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.Due || !slices.Equal(preflight.DueSources, []string{"synthetic"}) {
		t.Fatalf("empty-base preflight = %+v, want synthetic due", preflight)
	}
	if len(runner.calls) != 0 || base.Exists(core.GraphFile) {
		t.Fatalf("preflight executed %d command(s) or wrote the graph", len(runner.calls))
	}

	// Simulate another process winning the race after the CLI's lock-free preflight.
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, NoGraph: true}); err != nil {
		t.Fatal(err)
	}
	runner.mutex.Lock()
	runner.calls = nil
	runner.mutex.Unlock()
	preflight, err = services.PreflightSync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Due {
		t.Fatalf("filled-base preflight = %+v, want no due work", preflight)
	}
	report, err := services.Sync(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.NothingDue || !report.Complete || len(report.Units) != 0 || report.Graph != nil {
		t.Fatalf("race recheck report = %+v, want a complete no-work result", report)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("race recheck executed %d command(s), want none", len(runner.calls))
	}
}

func TestSyncIfDueRejectsAmbiguousModes(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	for _, request := range []services.SyncRequest{
		{IfDue: true, Force: true},
		{IfDue: true, DryRun: true},
		{IfDue: true, Preview: true, Targets: []string{"synthetic"}},
	} {
		if _, err := services.PreflightSync(t.Context(), base, request); !errors.Is(err, core.ErrConfig) {
			t.Fatalf("PreflightSync(%+v) error = %v, want ErrConfig", request, err)
		}
		if _, err := services.Sync(t.Context(), base, request); !errors.Is(err, core.ErrConfig) {
			t.Fatalf("Sync(%+v) error = %v, want ErrConfig", request, err)
		}
	}
}

func TestSyncRebuildsDerivedContentOnlyAfterWritingADocument(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	first, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil || first.Graph == nil || first.Written != 1 {
		t.Fatalf("first sync = %+v, %v; want one write and a graph rebuild", first, err)
	}
	second, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Skipped != 1 || second.Graph != nil {
		t.Fatalf("second sync = %+v, want an existing document skipped without a derived rebuild", second)
	}
}

func TestSyncTrustsTheExactOpenedExecutionPlan(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": oneRecord}}
	openedConfig := strings.Replace(baseConfig, "run: [cli, --since", "run: [untrusted-cli, --since", 1)
	base := newBase(t, openedConfig, runner)

	trustedConfig := strings.Replace(openedConfig, "run: [untrusted-cli, --since", "run: [trusted-cli, --since", 1)
	if err := os.WriteFile(filepath.Join(base.Root(), core.ConfigFileName), []byte(trustedConfig), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	trusted, err := core.LoadConfig(base.Root())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.WriteTrust(t.Context(), trusted, testClock); err != nil {
		t.Fatal(err)
	}

	_, err = services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"synthetic"}, Date: "2026-05-09", Preview: true,
	})
	if !errors.Is(err, core.ErrUntrusted) {
		t.Fatalf("Sync() error = %v, want the cached execution plan refused as untrusted", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("untrusted cached plan executed %d command(s)", len(runner.calls))
	}
}

func TestSyncSkipsACivilDateThatDidNotExist(t *testing.T) {
	location, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}

	base, runner := syncBase(t, "[]")
	base.Now = func() time.Time { return time.Date(2011, 12, 31, 12, 0, 0, 0, location) }
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("dry run called the provider %d time(s)", len(runner.calls))
	}
	if len(report.Units) != 1 || report.Units[0].Date != "2011-12-29" {
		t.Fatalf("units = %+v, want the last completed civil day before Apia skipped 2011-12-30", report.Units)
	}

	preview, err := services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"synthetic"}, Preview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Preview == nil || preview.Preview.Date != "2011-12-29" {
		t.Fatalf("preview = %+v, want the same last completed civil day", preview.Preview)
	}
}

func TestSyncPreviewValidatesOneSourceWithoutWriting(t *testing.T) {
	date := testClock.AddDate(0, 0, -1).Format(time.DateOnly)
	records := `[
  {"id":"a","t":"` + date + `T09:00:00Z","subject":"A"},
  {"id":"b","t":"` + date + `T10:00:00Z","subject":"B"},
  {"id":"c","t":"` + date + `T11:00:00Z","subject":"C"},
  {"id":"d","t":"` + date + `T12:00:00Z","subject":"D"}
]`
	base, runner := syncBase(t, records)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"synthetic"}, Preview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Preview == nil || report.Preview.Source != "synthetic" || report.Preview.Date != date ||
		report.Preview.Count != 4 || len(report.Preview.Sample) != 3 {
		t.Fatalf("preview = %+v, want one validated source, full count, and three projected samples", report.Preview)
	}
	for _, sample := range report.Preview.Sample {
		if sample.Record != nil || sample.URI == "" {
			t.Fatalf("sample = %+v, want an addressable projection without raw provider JSON", sample)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want exactly one preview execution", len(runner.calls))
	}
	if base.Exists(sources.EventDocumentURI(date, "synthetic")) || base.Exists(core.GraphFile) {
		t.Fatal("preview wrote a source document or derived graph")
	}
}

func TestSyncPreviewRejectsAmbiguousOrWriteFlags(t *testing.T) {
	base, _ := syncBase(t, "[]")
	for _, request := range []services.SyncRequest{
		{Preview: true},
		{Targets: []string{"synthetic", "synthetic"}, Preview: true},
		{Targets: []string{"synthetic"}, Preview: true, Days: 2},
		{Targets: []string{"synthetic"}, Preview: true, Force: true},
		{Targets: []string{"synthetic"}, Preview: true, DryRun: true},
		{Targets: []string{"synthetic"}, Preview: true, NoGraph: true},
	} {
		if _, err := services.Sync(t.Context(), base, request); err == nil {
			t.Fatalf("Sync(%+v) succeeded, want preview ambiguity rejected", request)
		}
	}
}

func TestSyncRefusesAnUntrustedBase(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": oneRecord}}
	base := newBase(t, baseConfig, runner)
	_, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if !errors.Is(err, core.ErrUntrusted) {
		t.Fatalf("Sync() error = %v, want the trust gate to refuse", err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("an untrusted base must not run a single command")
	}
	// --dry-run needs no trust: it executes nothing, so there is nothing to gate.
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, DryRun: true}); err != nil {
		t.Fatalf("--dry-run on an untrusted base = %v, want it allowed", err)
	}
}

func TestSyncReportsAFailedDayWithoutWritingIt(t *testing.T) {
	base, _ := syncBase(t, "not json at all")
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 2 || report.Written != 0 || report.Complete {
		t.Fatalf("report = %+v, want both days failed and nothing written", report)
	}
	if !strings.Contains(report.FailureSummary(), "not valid JSON") {
		t.Fatalf("summary = %q, want the reason named per unit", report.FailureSummary())
	}
	// Nothing partial is written, so a later run still sees the days as missing.
	if listing, err := services.ListEvents(t.Context(), base, services.Window{}, "", 0); err != nil || len(listing.Days) != 0 {
		t.Fatalf("a failed day left something behind: %+v, %v", listing, err)
	}
}

func TestSyncRefusesAnUnsafeDestinationBeforeCallingTheProvider(t *testing.T) {
	base, runner := syncBase(t, oneRecord)
	date := testClock.AddDate(0, 0, -1).Format(time.DateOnly)
	destination := mustResolve(t, base, sources.EventDocumentURI(date, "synthetic"))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, destination); err != nil {
		t.Fatal(err)
	}

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil {
		t.Fatalf("Sync() error = %v, want an ordinary failed unit report", err)
	}
	if report.Failed != 1 || report.Units[0].Outcome != services.OutcomeFailed ||
		!strings.Contains(report.Units[0].Error, "symlink") {
		t.Fatalf("report = %+v, want the unsafe destination named as a failed unit", report)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unsafe destination still called the provider %d time(s)", len(runner.calls))
	}
}

// TestSyncNeverSerializesProviderStderr covers the service boundary above ExecRunner. A failed
// unit is rendered in text and JSON and then repeated in the CLI's failure summary, so copying
// a provider diagnostic here would turn one private response into three durable disclosures.
func TestSyncNeverSerializesProviderStderr(t *testing.T) {
	const privateStderr = "synthetic-private-provider-response"
	base, runner := syncBase(t, oneRecord)
	runner.err = core.NewCommandFailure(errors.New("synthetic provider failure"), privateStderr)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil {
		t.Fatalf("Sync() error = %v, want the failed unit returned in its report", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal SyncReport: %v", err)
	}
	for name, diagnostic := range map[string]string{
		"unit": report.Units[0].Error, "summary": report.FailureSummary(), "JSON": string(encoded),
	} {
		if strings.Contains(diagnostic, privateStderr) {
			t.Fatalf("%s leaked provider stderr: %q", name, diagnostic)
		}
		if !strings.Contains(diagnostic, "command execution failed") {
			t.Fatalf("%s = %q, want the safe failure class", name, diagnostic)
		}
	}
	if summary := report.FailureSummary(); !strings.Contains(summary, "command: cli --since") {
		t.Fatalf("summary = %q, want the failed command and its substituted parameters", summary)
	}
}

func TestSyncRebuildsTheDerivedFiles(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Graph == nil || report.Graph.Edges == 0 {
		t.Fatalf("report = %+v, want graph.tsv rebuilt", report)
	}
	if !base.Exists(core.GraphFile) {
		t.Fatal("the derived graph was reported but not written")
	}
	yesterday := testClock.AddDate(0, 0, -1).Format(time.DateOnly)
	if base.Exists(path.Join(string(core.LayerEvents), yesterday, "SUMMARY.md")) {
		t.Fatal("sync created the removed SUMMARY.md artifact")
	}
	skipped, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, Force: true, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Graph != nil {
		t.Fatalf("--no-graph still rebuilt the edge list: %+v", skipped.Graph)
	}
}

func TestSyncRecollectionReplacesStaleGraphEdges(t *testing.T) {
	oldRecord := strings.Replace(oneRecord, `"repo_uri":"repo:o/r"`, `"repo_uri":"repo:o/a"`, 1)
	runner := &fakeRunner{responses: map[string]string{"": oldRecord}}
	base := newBase(t, baseConfig, runner)
	trust(t, base)
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1}); err != nil {
		t.Fatal(err)
	}
	runner.responses[""] = strings.Replace(oneRecord, `"repo_uri":"repo:o/r"`, `"repo_uri":"repo:o/b"`, 1)
	if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, Force: true}); err != nil {
		t.Fatal(err)
	}
	edges, err := base.ReadFileContext(t.Context(), core.GraphFile, core.MaxSourceDocumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(edges), "repo:o/a") || !strings.Contains(string(edges), "repo:o/b") {
		t.Fatalf("recollected graph = %s, want the new edge and no stale edge", edges)
	}
}

func TestSyncTargetsAreValidated(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	_, err := services.Sync(t.Context(), base, services.SyncRequest{Targets: []string{"invented"}, Days: 1})
	if err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("Sync() error = %v, want an undeclared target refused with the fix named", err)
	}
	disabled := newBase(t, strings.Replace(baseConfig, "    enabled: true", "    enabled: false", 1), nil)
	trust(t, disabled)
	_, err = services.Sync(t.Context(), disabled, services.SyncRequest{Targets: []string{"synthetic"}, Days: 1})
	if err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("Sync() error = %v, want a disabled target refused with the key named", err)
	}
	_, err = services.Sync(t.Context(), disabled, services.SyncRequest{Days: 1})
	if err == nil || !strings.Contains(err.Error(), "no source is enabled") {
		t.Fatalf("Sync() error = %v, want a base with nothing enabled to say so", err)
	}
	for _, request := range []services.SyncRequest{
		{Targets: []string{"synthetic"}, Days: 1, DryRun: true},
		{Days: 1, DryRun: true},
	} {
		report, err := services.Sync(t.Context(), disabled, request)
		if err != nil || len(report.Units) != 1 || report.Units[0].Outcome != services.OutcomePlanned {
			t.Fatalf("disabled dry-run = %+v, %v; want one safe planned command", report, err)
		}
	}
}

func TestSyncRefusesADuplicateTargetBeforePlanningWork(t *testing.T) {
	base, runner := syncBase(t, oneRecord)
	_, err := services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"synthetic", "synthetic"}, Days: 1,
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "duplicate source") {
		t.Fatalf("Sync() error = %v, want the duplicate target refused as configuration", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("duplicate target called the provider %d time(s), want refusal before planning", len(runner.calls))
	}
}

func TestSyncRefusesTodayAndTheFuture(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	for _, date := range []string{testClock.Format(time.DateOnly), "2027-01-01"} {
		_, err := services.Sync(t.Context(), base, services.SyncRequest{Date: date})
		if err == nil || !strings.Contains(err.Error(), "completed local days only") {
			t.Fatalf("Sync(--date %s) error = %v, want it refused", date, err)
		}
	}
}

// TestSyncIndexSourceRefreshesOnAge is what makes an index source different from a log one:
// it collects a point in time, and is re-run on age rather than per day.
func TestSyncIndexSourceRefreshesOnAge(t *testing.T) {
	config := strings.Replace(baseConfig, `  synthetic:
    enabled: true
    layer: events
    run: [cli, --since, "{{date}}", --until, "{{next_date}}"]`, `  repos:
    enabled: true
    layer: index
    run: [cli, repo, list]`, 1)
	config = strings.Replace(config, "      time: .t\n", "", 1)
	runner := &fakeRunner{responses: map[string]string{"": `[{"id":"o/r","subject":"s","link":"https://x.test","repo":"o/r","who":"m@x.test"}]`}}
	base := newBase(t, config, runner)
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 5})
	if err != nil {
		t.Fatal(err)
	}
	// One unit, not five: an index source has no per-day window.
	if len(report.Units) != 1 || report.Units[0].URI != "index/repos.json" {
		t.Fatalf("units = %+v, want exactly one index unit", report.Units)
	}
	again, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 5})
	if err != nil {
		t.Fatal(err)
	}
	if again.Units[0].Outcome != services.OutcomeFresh {
		t.Fatalf("outcome = %q, want it skipped as fresh within index_max_age_hours", again.Units[0].Outcome)
	}
	document, err := base.ReadDocumentContext(t.Context(), "index/repos.json")
	if err != nil {
		t.Fatal(err)
	}
	document.CollectedAt = testClock.Add(time.Hour).Format(time.RFC3339)
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	future, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 5, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if future.Written != 1 || future.Units[0].Outcome != services.OutcomeWritten {
		t.Fatalf("future-dated snapshot = %+v, want it refreshed rather than treated as indefinitely fresh", future)
	}
}

func TestSyncHonoursCancellation(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := services.Sync(ctx, base, services.SyncRequest{Days: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync() error = %v, want cancellation to propagate", err)
	}
}

func TestSyncWorkerAllocationIsBoundedByConcurrency(t *testing.T) {
	const (
		sourceCount = 3
		days        = 366
		concurrency = 4
	)
	var config strings.Builder
	config.WriteString("name: bounded-sync\n" +
		"layers: {events: true, index: false, tasks: false, projects: false, wiki: false}\n" +
		"sources:\n")
	for index := range sourceCount {
		fmt.Fprintf(&config, "  source-%d:\n"+
			"    enabled: true\n"+
			"    layer: events\n"+
			"    run: [cli, --since, \"{{date}}\", --until, \"{{next_date}}\"]\n"+
			"    fields:\n      id: .id\n      title: .id\n"+
			"      time: .time\n", index)
	}
	fmt.Fprintf(&config, "sync: {days: 30, index_max_age_hours: 168, timeout: 1m, concurrency: %d}\n", concurrency)

	base := newBase(t, config.String(), nil)
	trust(t, base)
	started := make(chan struct{}, concurrency)
	base.Runner = sources.RunnerFunc(func(ctx context.Context, _ sources.Command) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := services.Sync(ctx, base, services.SyncRequest{Days: days, NoGraph: true})
		done <- err
	}()
	for range concurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("sync did not start the configured number of workers")
		}
	}

	// The caller running Sync plus the fixed workers are the only goroutines runUnits may own.
	// Sampling until the count settles makes the assertion catch the old one-goroutine-per-unit
	// scheduler without depending on a point-in-time race with its spawn loop.
	if got, wantMax := stableSyncRunGoroutines(t), concurrency+1; got > wantMax {
		cancel()
		t.Fatalf("runUnits owns %d goroutines for %d planned units at concurrency %d; want at most %d",
			got, sourceCount*days, concurrency, wantMax)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Sync() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded sync workers did not stop after cancellation")
	}
}

func stableSyncRunGoroutines(t *testing.T) int {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	last, stable, highest := -1, 0, 0
	for {
		current := syncRunGoroutines()
		highest = max(highest, current)
		if current == last {
			stable++
		} else {
			last, stable = current, 0
		}
		if stable >= 20 {
			return highest
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("runUnits goroutine count did not settle within one second (last %d, highest %d)", last, highest)
			return 0
		}
	}
}

func syncRunGoroutines() int {
	buffer := make([]byte, 1<<20)
	for {
		written := runtime.Stack(buffer, true)
		if written < len(buffer) {
			buffer = buffer[:written]
			break
		}
		buffer = make([]byte, len(buffer)*2)
	}
	count := 0
	for _, stack := range bytes.Split(buffer, []byte("\n\n")) {
		if bytes.Contains(stack, []byte("github.com/fmind/fkf/services.runUnits")) {
			count++
		}
	}
	return count
}

// TestSyncRebuildsTheDerivedFilesWithoutTheIndexLayer is the events-only base `--preset minimal`
// creates. The edge list does not belong to the index layer; it is extracted from events and
// the Markdown layers, so
// gating the rebuild on `layers.index` left such a base with a graph that `fkf sync` never
// wrote, and `fkf graph <uri>` and `context --expand` permanently empty.
func TestSyncRebuildsTheDerivedFilesWithoutTheIndexLayer(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": oneRecord}}
	config := strings.Replace(baseConfig, "  index: true", "  index: false", 1)
	base := newBase(t, config, runner)
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 1 {
		t.Fatalf("report = %+v, want the day collected", report)
	}
	if report.Graph == nil || report.Graph.Edges == 0 {
		t.Fatalf("report.Graph = %+v, want the edge list rebuilt; it is derived from events, not from index/", report.Graph)
	}
	for _, uri := range []string{
		core.GraphFile, core.GraphDstFile, core.GraphOffsetsFile, core.GraphMetaFile, core.GraphGenerationFile,
	} {
		if !base.Exists(uri) {
			t.Fatalf("%s was not written on a base that disables the index layer", uri)
		}
	}
}

const windowedConfig = `name: brain
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources:
  windowed:
    enabled: true
    layer: events
    window: true
    run: [cli, --since, "{{start}}", --until, "{{end}}"]
    fields:
      id: .id
      time: .t
      title: .subject
`

// TestSyncCollectsAWholeWindowInOneCommand is the point of `window: true`: three requested
// days, one command execution, three documents — bucketed by each record's own declared time,
// never by which day the command happened to be invoked with.
func TestSyncCollectsAWholeWindowInOneCommand(t *testing.T) {
	runner := &fakeRunner{}
	base := newBase(t, windowedConfig, runner)
	trust(t, base)

	// Three requested days: the last three days before testClock (Sync excludes today).
	first := testClock.AddDate(0, 0, -3)
	second := testClock.AddDate(0, 0, -2)
	third := testClock.AddDate(0, 0, -1)
	stdout := `[
		{"id":"a1","t":"` + first.Add(9*time.Hour).UTC().Format(time.RFC3339) + `","subject":"day one"},
		{"id":"a2","t":"` + third.Add(9*time.Hour).UTC().Format(time.RFC3339) + `","subject":"day three"}
	]`
	runner.responses = map[string]string{"": stdout}

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("the runner was called %d time(s); a windowed source must run its command exactly "+
			"once for the whole range, not once per day", len(runner.calls))
	}
	if report.Written != 3 {
		t.Fatalf("report.Written = %d, want all three requested days written from the one call "+
			"(units: %+v)", report.Written, report.Units)
	}
	if report.Records != 2 {
		t.Fatalf("report.Records = %d, want the two records bucketed across the three days", report.Records)
	}
	counts := map[string]int{}
	for _, unit := range report.Units {
		counts[unit.Date] = unit.Count
	}
	if counts[first.Format(time.DateOnly)] != 1 || counts[second.Format(time.DateOnly)] != 0 ||
		counts[third.Format(time.DateOnly)] != 1 {
		t.Fatalf("per-day counts = %+v, want [1 0 1] across the three requested days", counts)
	}
	// The empty middle day is still a complete, addressable document.
	if !base.Exists("events/" + second.Format(time.DateOnly) + "/windowed.json") {
		t.Fatal("the quiet middle day was not filed as a complete document")
	}

	// Re-running skips every day as existing WITHOUT calling the command again.
	skipped, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3})
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Written != 0 || skipped.Skipped != 3 {
		t.Fatalf("second run = %+v, want everything skipped as existing", skipped)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("the runner was called again (%d total) on a run where every day already existed",
			len(runner.calls))
	}

	// --force runs the command exactly once more and rewrites every day.
	forced, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if forced.Written != 3 {
		t.Fatalf("--force = %+v, want all three days rewritten", forced)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("the runner was called %d time(s) after --force, want exactly 2 total", len(runner.calls))
	}
}

func TestSyncScalesOneWindowedCommandTimeoutAcrossTheContiguousSpan(t *testing.T) {
	for _, test := range []struct {
		name, sourceTimeout string
		want                time.Duration
	}{
		{name: "base timeout", want: 6 * time.Second},
		{name: "source override before scaling", sourceTimeout: "    timeout: 5s\n", want: 15 * time.Second},
		{name: "one hour cap", sourceTimeout: "    timeout: 30m\n", want: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := strings.Replace(windowedConfig, "    window: true\n", "    window: true\n"+test.sourceTimeout, 1) +
				"sync: {timeout: 2s}\n"
			runner := &fakeRunner{responses: map[string]string{"": "[]"}}
			base := newBase(t, config, runner)
			trust(t, base)

			if _, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 3, NoGraph: true}); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("runner calls = %d, want one contiguous-span invocation", len(runner.calls))
			}
			if got := runner.calls[0].Timeout; got != test.want {
				t.Fatalf("command timeout = %s, want %s for the three-day span", got, test.want)
			}
		})
	}
}

func TestSyncPartitionsWindowedCollectionAroundExistingDays(t *testing.T) {
	leftDay, err := sources.ParseDayInLocation("2026-05-05", testClock.Location())
	if err != nil {
		t.Fatal(err)
	}
	rightDay, err := sources.ParseDayInLocation("2026-05-08", testClock.Location())
	if err != nil {
		t.Fatal(err)
	}
	leftWindow, rightWindow := sources.DayWindow(leftDay), sources.DayWindow(rightDay)
	runner := &fakeRunner{responses: map[string]string{
		"--since " + leftWindow.Start:  `[{"id":"left","t":"2026-05-05T09:00:00Z","subject":"left span"}]`,
		"--since " + rightWindow.Start: `[{"id":"right","t":"2026-05-08T09:00:00Z","subject":"right span"}]`,
	}}
	base := newBase(t, windowedConfig, runner)
	source, err := base.Source("windowed")
	if err != nil {
		t.Fatal(err)
	}
	day, err := sources.ParseDayInLocation("2026-05-07", testClock.Location())
	if err != nil {
		t.Fatal(err)
	}
	existing, err := sources.Collect(t.Context(), &fakeRunner{responses: map[string]string{
		"": `[{"id":"existing","t":"2026-05-07T09:00:00Z","subject":"existing"}]`,
	}}, source, base.Env, sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteDocument(existing); err != nil {
		t.Fatal(err)
	}
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 5})
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 4 || report.Skipped != 1 || len(runner.calls) != 2 {
		t.Fatalf("report = %+v, calls = %d; want two contiguous missing spans around one existing day", report, len(runner.calls))
	}
	displays := []string{runner.calls[0].Display(), runner.calls[1].Display()}
	leftEnd := sources.DayWindow(leftDay.AddDate(0, 0, 1)).End
	rightEnd := sources.DayWindow(rightDay.AddDate(0, 0, 1)).End
	if !strings.Contains(displays[0], "--since "+leftWindow.Start) || !strings.Contains(displays[0], "--until "+leftEnd) ||
		!strings.Contains(displays[1], "--since "+rightWindow.Start) || !strings.Contains(displays[1], "--until "+rightEnd) {
		t.Fatalf("commands = %v, want exact gap-partitioned windows", displays)
	}
}

// TestSyncFailsTheWholeWindowedRangeOnOneBadRecord is the completeness rule at range scope:
// one record whose declared time is outside every requested day fails every day in the range,
// and nothing partial is written — the same rule a missing identity applies to one day today.
func TestSyncFailsTheWholeWindowedRangeOnOneBadRecord(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"": `[{"id":"a1","t":"2020-01-01T00:00:00Z","subject":"years out of range"}]`,
	}}
	base := newBase(t, windowedConfig, runner)
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 2 || report.Written != 0 {
		t.Fatalf("report = %+v, want both requested days failed and nothing written", report)
	}
	for _, unit := range report.Units {
		if unit.Outcome != services.OutcomeFailed || !strings.Contains(unit.Error, "outside the requested window") {
			t.Fatalf("unit = %+v, want it to fail naming the out-of-window record", unit)
		}
	}
}

// TestSyncWindowedSourceHonoursASingleDayRequest is --date on a windowed source: the range is
// exactly the one requested day, so the command still runs once and the result is one document
// indistinguishable from an ordinary day-at-a-time collection.
func TestSyncWindowedSourceHonoursASingleDayRequest(t *testing.T) {
	date := testClock.AddDate(0, 0, -1).Format(time.DateOnly)
	runner := &fakeRunner{responses: map[string]string{
		"": `[{"id":"a1","t":"` + date + `T09:00:00Z","subject":"the one day"}]`,
	}}
	base := newBase(t, windowedConfig, runner)
	trust(t, base)

	report, err := services.Sync(t.Context(), base, services.SyncRequest{Date: date})
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 1 || len(runner.calls) != 1 {
		t.Fatalf("report = %+v, calls = %d, want one document from one call", report, len(runner.calls))
	}
}
