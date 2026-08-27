package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// A day is collected or absent, never partial. `sync` plans every (source, day) pair the
// window is missing, runs them concurrently under the base's own timeout, and files only the
// documents that came back complete. Today's local day is never collected, because a day that
// is still happening cannot be complete.

// SyncOutcome is what happened to one planned unit.
type SyncOutcome string

const (
	// OutcomeWritten means a complete document was filed.
	OutcomeWritten SyncOutcome = "written"
	// OutcomeSkipped means the document already existed and --force was not given.
	OutcomeSkipped SyncOutcome = "skipped-existing"
	// OutcomeFresh means an index document is younger than index_max_age_hours.
	OutcomeFresh SyncOutcome = "skipped-fresh"
	// OutcomeFailed means the source did not produce a complete document for that unit.
	OutcomeFailed SyncOutcome = "failed"
	// OutcomePlanned is what --dry-run reports: this is what would have run.
	OutcomePlanned SyncOutcome = "planned"
)

// SyncUnit is one (source, day) pair and its result.
type SyncUnit struct {
	Source  string      `json:"source"`
	Kind    core.Layer  `json:"kind"`
	Date    string      `json:"date,omitempty"`
	URI     string      `json:"uri"`
	Outcome SyncOutcome `json:"outcome"`
	Count   int         `json:"count,omitempty"`
	Command string      `json:"command,omitempty"`
	Error   string      `json:"error,omitempty"`
	Elapsed string      `json:"elapsed,omitempty"`
	// Attempts is set only when the declared retry policy actually ran the command more than
	// once. A retried failure must never be quieter than a first-try one.
	Attempts int `json:"attempts,omitempty"`
}

// SyncReport is what `fkf sync` returns. A caller reads its exit code; a timer unit is six
// documented lines rather than a command fkf has to own.
type SyncReport struct {
	Base     string       `json:"base"`
	DryRun   bool         `json:"dry_run,omitempty"`
	Preview  *SyncPreview `json:"preview,omitempty"`
	Window   Window       `json:"window"`
	Units    []SyncUnit   `json:"units"`
	Written  int          `json:"written"`
	Skipped  int          `json:"skipped"`
	Failed   int          `json:"failed"`
	Records  int          `json:"records"`
	Graph    *GraphBuild  `json:"graph,omitempty"`
	Elapsed  string       `json:"elapsed"`
	Complete bool         `json:"complete"`
}

// SyncPreview is a validated, non-persistent sample from exactly one source.
type SyncPreview struct {
	Source string       `json:"source"`
	Kind   core.Layer   `json:"kind"`
	Date   string       `json:"date,omitempty"`
	Count  int          `json:"count"`
	Sample []FindRecord `json:"sample"`
}

// SyncRequest is one collection run.
type SyncRequest struct {
	Targets []string
	Days    int
	Date    string
	Force   bool
	DryRun  bool
	NoGraph bool
	Preview bool
}

// Sync collects every unit the window is missing.
func Sync(ctx context.Context, base *Base, request SyncRequest) (*SyncReport, error) {
	if request.Preview {
		return previewSync(ctx, base, request)
	}
	targets, err := resolveTargets(base, request.Targets)
	if err != nil {
		return nil, err
	}
	days, window, err := planDays(base, request)
	if err != nil {
		return nil, err
	}
	if !request.DryRun {
		if err := base.RequireTrust(ctx); err != nil {
			return nil, err
		}
		if err := sources.EnsureBinDir(base.Root()); err != nil {
			return nil, err
		}
	}
	started := base.Now()
	report := &SyncReport{Base: base.Root(), DryRun: request.DryRun, Window: window, Units: []SyncUnit{}}
	work := planUnits(targets, days)
	if err := runUnits(ctx, base, report, work, request); err != nil {
		return nil, err
	}
	sort.SliceStable(report.Units, func(i, j int) bool {
		if report.Units[i].Date != report.Units[j].Date {
			return report.Units[i].Date < report.Units[j].Date
		}
		return report.Units[i].Source < report.Units[j].Source
	})
	for _, unit := range report.Units {
		switch unit.Outcome {
		case OutcomeWritten:
			report.Written++
			report.Records += unit.Count
		case OutcomeSkipped, OutcomeFresh:
			report.Skipped++
		case OutcomeFailed:
			report.Failed++
		}
	}
	report.Complete = report.Failed == 0
	// A derived-stage failure happens after complete source documents are atomically filed.
	// The ordinary retry therefore sees those units as existing; retry derived work for both
	// newly written and already-collected inputs so that failure cannot become sticky.
	if !request.DryRun && (report.Written > 0 || report.Skipped > 0) {
		if err := rebuildDerived(ctx, base, report, request); err != nil {
			report.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
			return report, fmt.Errorf("source documents are complete but derived rebuild failed; rerun the same sync to retry it: %w", err)
		}
	}
	report.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
	return report, nil
}

func previewSync(ctx context.Context, base *Base, request SyncRequest) (*SyncReport, error) {
	if len(request.Targets) != 1 {
		return nil, fmt.Errorf("%w: --preview requires exactly one source", core.ErrConfig)
	}
	if request.Days != 0 || request.Force || request.DryRun || request.NoGraph {
		return nil, fmt.Errorf("%w: --preview may be combined only with --date", core.ErrConfig)
	}
	targets, err := resolveTargets(base, request.Targets)
	if err != nil {
		return nil, err
	}
	source := targets[0]
	started := base.Now()
	window := Window{}
	collectionWindow := sources.Window{}
	date := ""
	if source.Layer == core.LayerEvents {
		date = request.Date
		if date == "" {
			completed, err := previousCompletedDays(base.Now(), 1)
			if err != nil {
				return nil, err
			}
			date = completed[0].Format(time.DateOnly)
		}
		days, planned, err := planDays(base, SyncRequest{Date: date})
		if err != nil {
			return nil, err
		}
		window = planned
		collectionWindow = sources.DayWindow(days[0])
	} else if request.Date != "" {
		return nil, fmt.Errorf("%w: --date applies only to an events source", core.ErrConfig)
	}
	if err := base.RequireTrust(ctx); err != nil {
		return nil, err
	}
	pacer := sources.NewPacer(base.Now)
	runner := sources.NewPolicyRunner(sources.NewPacingRunner(base.Runner, pacer, source), source)
	document, err := sources.Collect(ctx, runner, source, base.Env, collectionWindow,
		base.Config.Sync.Timeout, base.Now())
	if err != nil {
		return nil, fmt.Errorf("preview source %s: %w", source.Name, err)
	}
	sample := make([]FindRecord, 0, min(3, len(document.Records)))
	for _, record := range document.Records[:min(3, len(document.Records))] {
		projected := project(document, record)
		projected.Record = nil
		sample = append(sample, projected)
	}
	report := &SyncReport{
		Base: base.Root(), Window: window, Units: []SyncUnit{}, Complete: true,
		Records: document.Count, Elapsed: base.Now().Sub(started).Round(time.Millisecond).String(),
		Preview: &SyncPreview{Source: source.Name, Kind: source.Layer, Date: date, Count: document.Count, Sample: sample},
	}
	return report, nil
}

func resolveTargets(base *Base, names []string) ([]*core.Source, error) {
	if len(names) == 0 {
		enabled := base.Config.EnabledSources()
		if len(enabled) == 0 {
			return nil, fmt.Errorf("%w: no source is enabled in %s; set `enabled: true` on the sources you want",
				core.ErrConfig, base.Config.Path)
		}
		return enabled, nil
	}
	targets := make([]*core.Source, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate source %q; name each sync target once", core.ErrConfig, name)
		}
		seen[name] = struct{}{}
		source, err := base.Source(name)
		if err != nil {
			return nil, err
		}
		if !source.Enabled {
			return nil, fmt.Errorf("%w: source %s is disabled; set `sources.%s.enabled: true` in %s to collect it",
				core.ErrConfig, name, name, base.Config.Path)
		}
		targets = append(targets, source)
	}
	return targets, nil
}

// planDays returns the completed local days to collect. Today is excluded on purpose.
func planDays(base *Base, request SyncRequest) ([]time.Time, Window, error) {
	now := base.Now()
	if request.Date != "" {
		day, err := sources.ParseDay(request.Date)
		if err != nil {
			return nil, Window{}, err
		}
		// Compare the DAY LABELS rather than the instants: a local midnight and a UTC clock are
		// different instants for the same calendar day, and comparing instants let "today"
		// through in every zone east of UTC.
		if day.Format(time.DateOnly) >= now.Format(time.DateOnly) {
			return nil, Window{}, fmt.Errorf("%s is today or later; fkf collects completed local days only", request.Date)
		}
		return []time.Time{day}, Window{Since: request.Date, Until: request.Date}, nil
	}
	count := request.Days
	if count <= 0 {
		count = base.Config.Sync.Days
	}
	if count < 1 || count > 366 {
		return nil, Window{}, fmt.Errorf("--days is %d; expected 1..366", count)
	}
	days, err := previousCompletedDays(now, count)
	if err != nil {
		return nil, Window{}, err
	}
	window := Window{
		Since: days[0].Format(time.DateOnly),
		Until: days[len(days)-1].Format(time.DateOnly),
	}
	return days, window, nil
}

// previousCompletedDays enumerates calendar labels rather than subtracting local durations.
// A timezone may skip one label entirely (Pacific/Apia skipped 2011-12-30); AddDate silently
// normalizes that request to another day and could otherwise plan today twice. Missing labels
// are not days, so they do not consume the caller's requested count.
func previousCompletedDays(now time.Time, count int) ([]time.Time, error) {
	anchor, err := time.Parse(time.DateOnly, now.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("anchor completed-day planning: %w", err)
	}
	days := make([]time.Time, 0, count)
	for cursor := anchor.AddDate(0, 0, -1); len(days) < count; cursor = cursor.AddDate(0, 0, -1) {
		day, err := sources.ParseDay(cursor.Format(time.DateOnly))
		if errors.Is(err, sources.ErrCivilDateDoesNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	slices.Reverse(days)
	return days, nil
}

// syncWork is one item under the concurrency guard. A `window: true` source's whole requested
// range is ONE item — one process, one slot — that produces MANY report units when it runs;
// every other source keeps the existing one-item-per-(source,day) shape. Grouping happens at
// planning time rather than by post-processing the day list, so the two paths never have to
// agree on how to undo each other's grouping.
type syncWork struct {
	source *core.Source
	unit   SyncUnit // the single unit to collect; zero when dates is set
	dates  []string // the whole range to collect in one command; nil for a single unit
}

func planUnits(targets []*core.Source, days []time.Time) []syncWork {
	dates := make([]string, len(days))
	for index, day := range days {
		dates[index] = day.Format(time.DateOnly)
	}
	work := make([]syncWork, 0, len(targets)*max(1, len(days)))
	for _, source := range targets {
		switch {
		case source.Layer == core.LayerIndex:
			work = append(work, syncWork{source: source, unit: SyncUnit{
				Source: source.Name, Kind: source.Layer, URI: sources.IndexDocumentURI(source.Name),
			}})
		case source.Window && len(dates) > 0:
			work = append(work, syncWork{source: source, dates: dates})
		default:
			for _, date := range dates {
				work = append(work, syncWork{source: source, unit: SyncUnit{
					Source: source.Name, Kind: source.Layer, Date: date,
					URI: sources.EventDocumentURI(date, source.Name),
				}})
			}
		}
	}
	return work
}

func runUnits(ctx context.Context, base *Base, report *SyncReport, work []syncWork, request SyncRequest) error {
	workerCount := min(len(work), max(1, base.Config.Sync.Concurrency))
	// One pacer per run, not per process: two syncs in one binary — a test suite — must never
	// pace each other, and a source's `min_interval:` is about this run's calls to a provider.
	pacer := sources.NewPacer(base.Now)
	var (
		next        int
		workMutex   sync.Mutex
		reportMutex sync.Mutex
		waiting     sync.WaitGroup
	)
	takeWork := func() (syncWork, bool) {
		workMutex.Lock()
		defer workMutex.Unlock()
		if next >= len(work) {
			return syncWork{}, false
		}
		item := work[next]
		next++
		return item, true
	}
	worker := func() {
		defer waiting.Done()
		for ctx.Err() == nil {
			item, available := takeWork()
			if !available {
				return
			}
			var results []SyncUnit
			if item.dates != nil {
				results = collectRangeGroup(ctx, base, item.source, item.dates, request, pacer)
			} else {
				results = []SyncUnit{collectUnit(ctx, base, item.source, item.unit, request, pacer)}
			}
			reportMutex.Lock()
			report.Units = append(report.Units, results...)
			reportMutex.Unlock()
		}
	}
	// Workers, not work items, own goroutines. A large source-by-day plan therefore keeps its
	// allocation proportional to sync.concurrency and cancellation never has to drain a queue
	// of goroutines that were blocked before doing any work.
	waiting.Add(workerCount)
	for range workerCount {
		go worker()
	}
	waiting.Wait()
	return ctx.Err()
}

func collectUnit(
	ctx context.Context, base *Base, source *core.Source,
	unit SyncUnit, request SyncRequest, pacer *sources.Pacer,
) SyncUnit {
	started := base.Now()
	window := sources.Window{}
	if unit.Date != "" {
		day, err := sources.ParseDay(unit.Date)
		if err != nil {
			unit.Outcome, unit.Error = OutcomeFailed, err.Error()
			return unit
		}
		window = sources.DayWindow(day)
	}
	command := sources.BuildRunCommand(source, base.Env, window, base.Config.Sync.Timeout)
	unit.Command = command.Display()
	skip, outcome, err := shouldSkip(ctx, base, source, unit, request)
	if err != nil {
		unit.Outcome, unit.Error = OutcomeFailed, err.Error()
		unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
		return unit
	}
	if skip {
		unit.Outcome = outcome
		return unit
	}
	if request.DryRun {
		unit.Outcome = OutcomePlanned
		return unit
	}
	// The declared invocation policy wraps the runner rather than replacing it, so the fake
	// runner the tests inject is paced and retried exactly the way the real one is.
	runner := sources.NewPolicyRunner(sources.NewPacingRunner(base.Runner, pacer, source), source)
	document, err := sources.Collect(ctx, runner, source, base.Env, window, base.Config.Sync.Timeout, base.Now())
	// Reported whether the run succeeded or failed: a retried failure must never be quieter
	// than a first-try one, and a source that only ever succeeds on its third attempt is a
	// source whose provider is telling the operator something.
	if attempts := runner.Attempts(); attempts > 1 {
		unit.Attempts = attempts
	}
	if err != nil {
		unit.Outcome, unit.Error = OutcomeFailed, err.Error()
		unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
		return unit
	}
	if err := base.WriteDocument(document); err != nil {
		unit.Outcome, unit.Error = OutcomeFailed, err.Error()
		unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
		return unit
	}
	unit.Outcome, unit.Count = OutcomeWritten, document.Count
	unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
	return unit
}

// collectRangeGroup runs a `window: true` source's command ONCE for the whole requested range
// and turns what comes back into the same per-day SyncUnit shape every other source produces —
// the grouping is invisible to everything that reads report.Units, including the CLI text
// renderer and `fkf status`.
//
// Existing days split the missing dates into contiguous spans. A collector therefore never
// re-fetches an already-complete hole merely because missing days surround it, while each span
// still amortizes the source's fixed process or pagination cost.
func collectRangeGroup(
	ctx context.Context, base *Base, source *core.Source,
	dates []string, request SyncRequest, pacer *sources.Pacer,
) []SyncUnit {
	started := base.Now()
	units := make([]SyncUnit, 0, len(dates))
	needed := make([]string, 0, len(dates))
	for _, date := range dates {
		unit := SyncUnit{Source: source.Name, Kind: source.Layer, Date: date, URI: sources.EventDocumentURI(date, source.Name)}
		skip, outcome, err := shouldSkip(ctx, base, source, unit, request)
		if err != nil {
			unit.Outcome, unit.Error = OutcomeFailed, err.Error()
			unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
			units = append(units, unit)
			continue
		}
		if skip {
			unit.Outcome = outcome
			units = append(units, unit)
			continue
		}
		needed = append(needed, date)
	}
	if len(needed) == 0 {
		return units
	}
	spans, err := contiguousDaySpans(needed)
	if err != nil {
		for _, date := range needed {
			units = append(units, failedRangeUnit(source, date, "", err, started, base.Now()))
		}
		return units
	}
	for _, span := range spans {
		units = append(units, collectRangeSpan(ctx, base, source, span, request, pacer, started)...)
	}
	return units
}

func collectRangeSpan(
	ctx context.Context, base *Base, source *core.Source, dates []string,
	request SyncRequest, pacer *sources.Pacer, started time.Time,
) []SyncUnit {
	units := make([]SyncUnit, 0, len(dates))
	rangeWindow, err := windowSpanning(dates)
	if err != nil {
		for _, date := range dates {
			units = append(units, failedRangeUnit(source, date, "", err, started, base.Now()))
		}
		return units
	}
	command := sources.BuildRunCommand(source, base.Env, rangeWindow, base.Config.Sync.Timeout)
	if request.DryRun {
		for _, date := range dates {
			units = append(units, SyncUnit{
				Source: source.Name, Kind: source.Layer, Date: date,
				URI:     sources.EventDocumentURI(date, source.Name),
				Outcome: OutcomePlanned, Command: command.Display(),
			})
		}
		return units
	}
	runner := sources.NewPolicyRunner(sources.NewPacingRunner(base.Runner, pacer, source), source)
	documents, err := sources.CollectWindow(ctx, runner, source, base.Env, rangeWindow, dates, base.Config.Sync.Timeout, base.Now())
	attempts := runner.Attempts()
	if err != nil {
		for _, date := range dates {
			unit := failedRangeUnit(source, date, command.Display(), err, started, base.Now())
			if attempts > 1 {
				unit.Attempts = attempts
			}
			units = append(units, unit)
		}
		return units
	}
	for _, date := range dates {
		document := documents[date]
		unit := SyncUnit{
			Source: source.Name, Kind: source.Layer, Date: date, URI: document.URI(), Command: command.Display(),
		}
		if attempts > 1 {
			unit.Attempts = attempts
		}
		if err := base.WriteDocument(document); err != nil {
			unit.Outcome, unit.Error = OutcomeFailed, err.Error()
			unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
			units = append(units, unit)
			continue
		}
		unit.Outcome, unit.Count = OutcomeWritten, document.Count
		unit.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
		units = append(units, unit)
	}
	return units
}

func contiguousDaySpans(dates []string) ([][]string, error) {
	if len(dates) == 0 {
		return nil, nil
	}
	previous, err := sources.ParseDay(dates[0])
	if err != nil {
		return nil, err
	}
	spans := [][]string{{dates[0]}}
	for _, date := range dates[1:] {
		current, err := sources.ParseDay(date)
		if err != nil {
			return nil, err
		}
		if previous.AddDate(0, 0, 1).Format(time.DateOnly) != date {
			spans = append(spans, []string{})
		}
		last := len(spans) - 1
		spans[last] = append(spans[last], date)
		previous = current
	}
	return spans, nil
}

// windowSpanning builds the {{start}}/{{end}} boundary for a whole requested range, from the
// first and last requested day's own DST-safe boundary — never from AddDate on either end,
// which is exactly the arithmetic sources.DayWindow was fixed to avoid.
func windowSpanning(dates []string) (sources.Window, error) {
	first, err := sources.ParseDay(dates[0])
	if err != nil {
		return sources.Window{}, err
	}
	last, err := sources.ParseDay(dates[len(dates)-1])
	if err != nil {
		return sources.Window{}, err
	}
	firstWindow, lastWindow := sources.DayWindow(first), sources.DayWindow(last)
	return sources.Window{Date: firstWindow.Date, Next: lastWindow.Next, Start: firstWindow.Start, End: lastWindow.End}, nil
}

func failedRangeUnit(source *core.Source, date, command string, err error, started, now time.Time) SyncUnit {
	return SyncUnit{
		Source: source.Name, Kind: source.Layer, Date: date, URI: sources.EventDocumentURI(date, source.Name),
		Command: command, Outcome: OutcomeFailed, Error: err.Error(),
		Elapsed: now.Sub(started).Round(time.Millisecond).String(),
	}
}

func shouldSkip(ctx context.Context, base *Base, source *core.Source, unit SyncUnit, request SyncRequest) (bool, SyncOutcome, error) {
	absolute, err := base.Store.Resolve(unit.URI)
	if err != nil {
		return false, "", fmt.Errorf("resolve collection destination %s: %w", unit.URI, err)
	}
	_, err = os.Stat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("inspect collection destination %s: %w", unit.URI, err)
	}
	if request.Force {
		return false, "", nil
	}
	if source.Layer == core.LayerEvents {
		return true, OutcomeSkipped, nil
	}
	_, collectedAt, err := readValidatedIndexDocumentContext(ctx, base, unit.URI)
	if err != nil {
		return false, "", fmt.Errorf("inspect existing index snapshot %s: %w; use --force to replace it", unit.URI, err)
	}
	maxAge := time.Duration(base.Config.Sync.IndexMaxAgeHours) * time.Hour
	age := base.Now().Sub(collectedAt)
	if age >= 0 && age < maxAge {
		return true, OutcomeFresh, nil
	}
	return false, "", nil
}

// rebuildDerived regenerates the graph cache from stored input. A failure is reported without
// rolling back complete source documents; an ordinary retry resumes this stage.
func rebuildDerived(ctx context.Context, base *Base, report *SyncReport, request SyncRequest) error {
	if request.NoGraph {
		return nil
	}
	build, err := BuildGraph(ctx, base)
	if err != nil {
		return err
	}
	report.Graph = build
	return nil
}

// ErrPartial reports a run in which at least one source failed. It is the class the CLI maps
// to exit code 1: some work happened, and something did not.
var ErrPartial = errors.New("collection was incomplete")

// FailureSummary renders the failures for a diagnostic, one per line.
func (r *SyncReport) FailureSummary() string {
	var lines []string
	for _, unit := range r.Units {
		if unit.Outcome != OutcomeFailed {
			continue
		}
		label := unit.Source
		if unit.Date != "" {
			label += " " + unit.Date
		}
		line := "  " + label + ": " + unit.Error
		if unit.Command != "" {
			line += "\n    command: " + unit.Command
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
