package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf status` is the unified inspection command for a base: it summarizes the base layout,
// audits repository health and document integrity, and reports per-source readiness and freshness.

const (
	quietWindow       = 14
	quietArmingDays   = 7
	quietRatioPercent = 20
	gitTimeout        = 15 * time.Second
)

// Finding is one health or integrity issue worth acting on.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Paths    []string `json:"paths,omitempty"`
	Fix      string   `json:"fix,omitempty"`
}

// LayerOverview is one layer's line in the overview.
type LayerOverview struct {
	Layer   core.Layer `json:"layer"`
	Enabled bool       `json:"enabled"`
	URI     string     `json:"uri"`
	Count   int        `json:"count"`
	Unit    string     `json:"unit"`
	Since   string     `json:"since,omitempty"`
	Until   string     `json:"until,omitempty"`
	Note    string     `json:"note,omitempty"`
}

// RequirementStatus is one executable a source explicitly asks status to check.
type RequirementStatus struct {
	Name   string `json:"name"`
	OnPath bool   `json:"on_path"`
}

// SourceStatus is one source's readiness and recent volume.
type SourceStatus struct {
	Name            string              `json:"name"`
	Enabled         bool                `json:"enabled"`
	Kind            core.Layer          `json:"kind"`
	Requires        []RequirementStatus `json:"requires,omitempty"`
	Install         string              `json:"install,omitempty"`
	Body            bool                `json:"body"`
	Undeclared      bool                `json:"undeclared,omitempty"`
	LastDate        string              `json:"last_date,omitempty"`
	LastCollectedAt string              `json:"last_collected_at,omitempty"`
	LagHours        int                 `json:"lag_hours,omitempty"`
	Stale           bool                `json:"stale,omitempty"`
	LastCount       int                 `json:"last_count,omitempty"`
	Median          int                 `json:"median,omitempty"`
	Days            int                 `json:"days,omitempty"`
	Quiet           bool                `json:"quiet,omitempty"`
	QuietReason     string              `json:"quiet_reason,omitempty"`
	// lastCollected retains the exact evidence boundary used for hour-level freshness.
	// LastDate remains the stable public summary.
	lastCollected time.Time
}

// Status is what `fkf status` returns.
type Status struct {
	Base           string          `json:"base"`
	Name           string          `json:"name"`
	Origin         core.BaseOrigin `json:"base_origin"`
	Trust          core.TrustState `json:"trust"`
	Versioned      bool            `json:"versioned"`
	TrackCollected bool            `json:"track_collected"`
	Layers         []LayerOverview `json:"layers"`
	Sources        []SourceStatus  `json:"sources"`
	Findings       []Finding       `json:"findings"`
	Graph          *GraphSummary   `json:"graph,omitempty"`
	Unharvested    int             `json:"unharvested,omitempty"`
	Enabled        int             `json:"enabled"`
	Missing        int             `json:"missing_requirements"`
	Quiet          int             `json:"quiet"`
	Errors         int             `json:"errors"`
	Warnings       int             `json:"warnings"`
	OK             bool            `json:"ok"`
	Stale          bool            `json:"stale"`
	LastSync       string          `json:"last_sync,omitempty"`
	StaleDays      int             `json:"stale_days,omitempty"`
	MaxAge         int             `json:"max_age_hours,omitempty"`
	Next           []string        `json:"next"`
}

func (s *Status) addFinding(check string, severity Severity, message, fix string, paths ...string) {
	s.Findings = append(s.Findings, Finding{
		Check: check, Severity: severity, Message: message, Fix: fix, Paths: paths,
	})
}

// StatusRequest tunes the status report.
type StatusRequest struct {
	MaxAgeHours int
	All         bool
	// SkipGitAudit keeps MCP's status resource subprocess-free. The CLI leaves it false and
	// runs the fixed, sanitized tracked-file audit.
	SkipGitAudit bool
}

// Report compiles the complete base status.
func Report(ctx context.Context, base *Base, request StatusRequest) (*Status, error) {
	trust, err := core.ReadTrustConfig(ctx, base.Config)
	if err != nil {
		return nil, err
	}
	trackCollected, err := TracksCollected(base.Root())
	if err != nil {
		return nil, err
	}

	status := &Status{
		Base:           base.Root(),
		Name:           base.Config.Name,
		Origin:         base.Origin,
		Trust:          trust,
		Versioned:      base.Store.Versioned(),
		TrackCollected: trackCollected,
		Layers:         make([]LayerOverview, 0, len(core.Layers)),
		Sources:        []SourceStatus{},
		Findings:       []Finding{},
		MaxAge:         request.MaxAgeHours,
		Next:           []string{},
	}

	if err := populateLayerOverviews(ctx, base, status); err != nil {
		return nil, err
	}
	if err := populateSourceStatuses(ctx, base, status, request); err != nil {
		return nil, err
	}
	if err := auditHealth(ctx, base, status, request, trackCollected); err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	status.Next = suggestNext(status)
	return status, nil
}

func populateLayerOverviews(ctx context.Context, base *Base, status *Status) error {
	for _, layer := range core.Layers {
		if err := checkContext(ctx); err != nil {
			return err
		}
		summary, err := summarizeLayer(ctx, base, layer)
		if err != nil {
			return err
		}
		status.Layers = append(status.Layers, summary)
	}

	if _, staleDays := collectionFreshness(base, base.Now()); staleDays > 0 {
		status.StaleDays = staleDays
	}
	graph, err := SummarizeGraph(ctx, base)
	switch {
	case err == nil:
		status.Graph = graph
	case errors.Is(err, ErrDerivedMissing):
	default:
		status.Graph = graph
		status.addFinding("derived", SeverityError,
			fmt.Sprintf("the graph cache is invalid: %v", err), baseCommand(base.Root(), "build graph"),
			core.GraphFile, core.GraphMetaFile)
	}

	if base.Store.Enabled(core.LayerTasks) {
		learned, err := ListLearned(ctx, base, Window{}, true)
		if err != nil {
			return err
		}
		status.Unharvested = learned.Unharvested
	}
	return nil
}

func populateSourceStatuses(ctx context.Context, base *Base, status *Status, request StatusRequest) error {
	history, err := volumeHistory(ctx, base)
	if err != nil {
		return err
	}
	undeclared := map[string][]dayVolume{}
	for name, days := range history {
		if _, declared := base.Config.Sources[name]; !declared {
			undeclared[name] = days
		}
	}
	missing := map[string]struct{}{}
	if err := populateDeclaredSourceStatuses(
		ctx, base, status, history, undeclared, missing, request,
	); err != nil {
		return err
	}
	if err := populateUndeclaredSourceStatuses(ctx, base, status, undeclared); err != nil {
		return err
	}
	status.Missing = len(missing)
	return nil
}

func populateDeclaredSourceStatuses(
	ctx context.Context,
	base *Base,
	status *Status,
	history, undeclared map[string][]dayVolume,
	missing map[string]struct{},
	request StatusRequest,
) error {
	for _, name := range base.Config.SourceNames() {
		if err := checkContext(ctx); err != nil {
			return err
		}
		source := base.Config.Sources[name]
		delete(undeclared, name)
		entry := sourceStatusOf(ctx, base, source, history[name])
		if err := checkContext(ctx); err != nil {
			return err
		}
		if source.Enabled {
			status.Enabled++
			for _, requirement := range entry.Requires {
				if !requirement.OnPath {
					missing[requirement.Name] = struct{}{}
				}
			}
		}
		if entry.Quiet {
			status.Quiet++
		}
		observeSourceFreshness(status, &entry, base.Now(), request.MaxAgeHours)
		status.Sources = append(status.Sources, entry)
	}
	return nil
}

func populateUndeclaredSourceStatuses(
	ctx context.Context,
	base *Base,
	status *Status,
	undeclared map[string][]dayVolume,
) error {
	for _, name := range sortedNames(undeclared) {
		if err := checkContext(ctx); err != nil {
			return err
		}
		entry := SourceStatus{Name: name, Kind: core.LayerEvents, Undeclared: true}
		applyVolume(&entry, undeclared[name])
		observeSourceFreshness(status, &entry, base.Now(), 0)
		status.Sources = append(status.Sources, entry)
	}
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if _, declared := base.Config.Sources[name]; declared {
				continue
			}
			entry := SourceStatus{Name: name, Kind: core.LayerIndex, Undeclared: true}
			applyIndexDocument(ctx, base, &entry)
			observeSourceFreshness(status, &entry, base.Now(), 0)
			status.Sources = append(status.Sources, entry)
		}
	}
	return nil
}

func observeSourceFreshness(
	status *Status,
	entry *SourceStatus,
	now time.Time,
	maxAgeHours int,
) {
	if entry.LastDate > status.LastSync {
		status.LastSync = entry.LastDate
	}
	if !entry.lastCollected.IsZero() {
		entry.LastCollectedAt = entry.lastCollected.UTC().Format(time.RFC3339)
		if age := now.Sub(entry.lastCollected); age > 0 {
			entry.LagHours = int(age / time.Hour)
		}
	}
	age := now.Sub(entry.lastCollected)
	entry.Stale = entry.Enabled && maxAgeHours > 0 &&
		(entry.lastCollected.IsZero() || age < 0 || age > time.Duration(maxAgeHours)*time.Hour)
	status.Stale = status.Stale || entry.Stale
}

func auditHealth(ctx context.Context, base *Base, status *Status, request StatusRequest, trackCollected bool) error {
	if !status.Trust.Trusted {
		status.addFinding("trust", SeverityWarning,
			"this base's configuration is not trusted on this machine, so `fkf sync` will refuse to run its commands",
			baseCommand(base.Root(), "trust"))
	}
	if trackCollected {
		status.addFinding("history", SeverityWarning,
			"this base commits events/ and index/; git history is append-only, so anything collected is permanent",
			"start a new base if that was not intended")
	}
	if !request.SkipGitAudit {
		if err := checkGit(ctx, base, status); err != nil {
			return err
		}
	}
	if err := checkSkills(ctx, base, status); err != nil {
		return err
	}
	if err := checkConflictMarkers(ctx, base, status); err != nil {
		return err
	}
	if err := checkPermissions(ctx, base, status); err != nil {
		return err
	}
	checkDerived(base, status)
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := checkLearnedBacklog(ctx, base, status); err != nil {
		return err
	}
	if err := checkDocuments(ctx, base, status); err != nil {
		return err
	}

	sort.SliceStable(status.Findings, func(i, j int) bool { return status.Findings[i].Check < status.Findings[j].Check })
	for _, finding := range status.Findings {
		if finding.Severity == SeverityError {
			status.Errors++
		} else {
			status.Warnings++
		}
	}
	status.OK = status.Errors == 0
	return nil
}

func summarizeLayer(ctx context.Context, base *Base, layer core.Layer) (LayerOverview, error) {
	summary := LayerOverview{Layer: layer, Enabled: base.Store.Enabled(layer), URI: string(layer) + "/"}
	if !summary.Enabled {
		return summary, nil
	}
	switch layer {
	case core.LayerEvents:
		dates, err := base.EventDates()
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(dates), "day"
		if len(dates) > 0 {
			summary.Since, summary.Until = dates[0], dates[len(dates)-1]
		}
	case core.LayerIndex:
		listing, err := ListIndex(ctx, base, 0)
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(listing.Entries), "document"
		summary.Note = oldestIndexNote(listing)
	case core.LayerTasks:
		listing, err := ListTasks(ctx, base, Window{}, 0)
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(listing.Traces), "trace"
		if len(listing.Traces) > 0 {
			summary.Until = listing.Traces[0].Date
		}
	case core.LayerProjects, core.LayerWiki:
		listing, err := ListPages(ctx, base, layer, PageFilter{})
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = listing.Total, "page"
		summary.Note = pageNote(ctx, base, layer, listing)
	}
	return summary, nil
}

func oldestIndexNote(listing *IndexListing) string {
	oldest := 0
	for _, entry := range listing.Entries {
		oldest = max(oldest, entry.AgeHours)
	}
	if oldest == 0 {
		return ""
	}
	return fmt.Sprintf("oldest refreshed %dh ago", oldest)
}

func pageNote(ctx context.Context, base *Base, layer core.Layer, listing *PageListing) string {
	if layer == core.LayerProjects {
		byStatus := map[string]int{}
		for _, page := range listing.Pages {
			if page.Status != "" {
				byStatus[page.Status]++
			}
		}
		return joinCounts(byStatus)
	}
	if listing.Total == 0 {
		return ""
	}
	vocabulary, err := BuildTagVocabulary(ctx, base, layer)
	if err != nil {
		return ""
	}
	note := fmt.Sprintf("%d tags", len(vocabulary.Tags))
	if untagged := len(vocabulary.Untagged); untagged > 0 {
		note += fmt.Sprintf(", %d untagged", untagged)
	}
	return note
}

func joinCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
	}
	return strings.Join(parts, ", ")
}

func suggestNext(status *Status) []string {
	next := make([]string, 0, 3)
	if !status.Trust.Trusted {
		next = append(next, baseCommand(status.Base, "trust")+"  read the commands this base declares, then record them")
	}
	events := layerOverview(status, core.LayerEvents)
	switch {
	case events != nil && events.Count == 0:
		next = append(next, baseCommand(status.Base, "sync --days 7")+"  collect the last seven completed days")
	case status.StaleDays > 1:
		next = append(next, fmt.Sprintf(
			"%s  the newest day here is %d day(s) old",
			baseCommand(status.Base, fmt.Sprintf("sync --days %d", min(status.StaleDays, 30))), status.StaleDays))
	}
	if status.Graph == nil {
		next = append(next, baseCommand(status.Base, "build graph")+"  derive the edge list the graph and --expand read")
	}
	if status.Unharvested > 0 {
		next = append(next, fmt.Sprintf(
			"%s  %d bullet(s) no page has promoted yet",
			baseCommand(status.Base, "list tasks learned --unharvested"), status.Unharvested))
	}
	return append(next,
		baseCommand(status.Base, `context "<terms>"`)+"  the evidence pack you hand an agent",
		baseCommand(status.Base, "find <term>")+"  every match, in every layer",
		baseCommand(status.Base, "graph <uri>")+"  what is connected to one thing",
	)
}

func baseCommand(root, arguments string) string {
	return "fkf --base " + shellArg(root) + " " + arguments
}

func layerOverview(status *Status, layer core.Layer) *LayerOverview {
	for index := range status.Layers {
		if status.Layers[index].Layer == layer {
			return &status.Layers[index]
		}
	}
	return nil
}

func sourceStatusOf(ctx context.Context, base *Base, source *core.Source, days []dayVolume) SourceStatus {
	entry := SourceStatus{
		Name: source.Name, Enabled: source.Enabled, Kind: source.Layer,
		Install: source.Install, Body: source.HasBody(), Requires: []RequirementStatus{},
	}
	for _, name := range source.Requires {
		_, found := base.Env.LookPath(name)
		entry.Requires = append(entry.Requires, RequirementStatus{Name: name, OnPath: found})
	}
	if source.Layer == core.LayerIndex {
		applyIndexDocument(ctx, base, &entry)
	} else {
		applyVolume(&entry, days)
	}
	return entry
}

func sortedNames(values map[string][]dayVolume) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type dayVolume struct {
	date          string
	count         int
	lastCollected time.Time
}

func volumeHistory(ctx context.Context, base *Base) (map[string][]dayVolume, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	history := map[string][]dayVolume{}
	if !base.Store.Enabled(core.LayerEvents) {
		return history, nil
	}
	dates, err := base.EventDates()
	if err != nil {
		return nil, err
	}
	for _, date := range dates {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		names, err := base.DayDocuments(date)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			document, err := base.ReadDocumentContext(ctx, sources.EventDocumentURI(date, name))
			if err != nil {
				if ctxErr := checkContext(ctx); ctxErr != nil {
					return nil, ctxErr
				}
				continue
			}
			lastCollected, err := eventCollectionBoundary(document)
			if err != nil {
				return nil, fmt.Errorf("read collection boundary for %s: %w", document.URI(), err)
			}
			history[name] = append(history[name], dayVolume{
				date: date, count: document.Count, lastCollected: lastCollected,
			})
		}
	}
	for name := range history {
		sort.Slice(history[name], func(i, j int) bool { return history[name][i].date < history[name][j].date })
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return history, nil
}

func eventCollectionBoundary(document *sources.Document) (time.Time, error) {
	if document.WindowEnd != "" {
		return time.Parse(time.RFC3339, document.WindowEnd)
	}
	day, err := time.ParseInLocation(time.DateOnly, document.Date, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return day.AddDate(0, 0, 1), nil
}

func applyIndexDocument(ctx context.Context, base *Base, entry *SourceStatus) {
	document, collected, err := readValidatedIndexDocumentContext(ctx, base, sources.IndexDocumentURI(entry.Name))
	if err != nil {
		return
	}
	entry.LastCount, entry.Days = document.Count, 1
	entry.LastDate = collected.Local().Format(time.DateOnly)
	entry.lastCollected = collected
}

func applyVolume(entry *SourceStatus, days []dayVolume) {
	if len(days) == 0 {
		return
	}
	latest := days[len(days)-1]
	entry.LastDate, entry.LastCount, entry.Days = latest.date, latest.count, len(days)
	entry.lastCollected = latest.lastCollected
	if len(days) < quietArmingDays {
		return
	}
	nonEmpty := make([]int, 0, quietWindow)
	for index := len(days) - 2; index >= 0 && len(nonEmpty) < quietWindow; index-- {
		if days[index].count > 0 {
			nonEmpty = append(nonEmpty, days[index].count)
		}
	}
	if len(nonEmpty) < quietArmingDays {
		return
	}
	sort.Ints(nonEmpty)
	entry.Median = nonEmpty[len(nonEmpty)/2]
	switch {
	case latest.count == 0:
		entry.Quiet = true
		entry.QuietReason = fmt.Sprintf("%s returned nothing while its recent median is %d", latest.date, entry.Median)
	case latest.count*100 < entry.Median*quietRatioPercent:
		entry.Quiet = true
		entry.QuietReason = fmt.Sprintf("%s returned %d, under %d%% of its recent median %d",
			latest.date, latest.count, quietRatioPercent, entry.Median)
	}
}

// --- Health / Audit checks ---

func checkGit(ctx context.Context, base *Base, status *Status) error {
	if !base.Store.Versioned() {
		status.addFinding("git", SeverityWarning,
			"this base is not a git working tree, so nothing versions the wiki, the projects, or the task traces",
			"git init "+shellArg(base.Root()))
		return nil
	}
	tracked, err := trackedPaths(ctx, base.Root())
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		status.addFinding("uncommitted", SeverityWarning,
			"this base is a git tree with no commit, so nothing here is versioned and every audit "+
				"of what git tracks passes by having nothing to look at",
			"git -C "+shellArg(base.Root())+" add -A && git -C "+shellArg(base.Root())+
				" commit -m 'chore: first snapshot'")
		return nil
	}
	var credentials, collected []string
	for _, entry := range tracked {
		if matchesAnyPattern(entry, credentialPatterns) {
			credentials = append(credentials, entry)
		}
		if !status.TrackCollected && matchesAnyLayer(entry, collectedLayers) {
			collected = append(collected, entry)
		}
	}
	if len(credentials) > 0 {
		status.addFinding("tracked-credentials", SeverityError,
			"git tracks files whose whole purpose is to hold a secret; adding a pattern to .gitignore does not untrack them",
			"git rm --cached <path> and rotate the credential", credentials...)
	}
	if len(collected) > 0 {
		status.addFinding("tracked-collected", SeverityError,
			"collected content is ignored by the managed block but is still tracked, so it keeps entering history",
			"git rm -r --cached events index", collected...)
	}
	return nil
}

func trackedPaths(ctx context.Context, root string) ([]string, error) {
	// A repository controls .git/config. Disable the one ls-files extension that executes a
	// configured command, and keep this audit read-only even when another Git process is active.
	output, err := runGit(ctx, root, gitTimeout,
		"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "ls-files")
	if err != nil {
		return nil, fmt.Errorf("ask git what %s tracks: %w", root, err)
	}
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	return paths, nil
}

func matchesAnyPattern(entry string, patterns []string) bool {
	name := filepath.Base(entry)
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(entry, pattern) || strings.Contains(entry, "/"+pattern) {
				return true
			}
			continue
		}
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func matchesAnyLayer(entry string, layers []string) bool {
	for _, layer := range layers {
		if strings.HasPrefix(entry, layer) {
			return true
		}
	}
	return false
}

func checkSkills(ctx context.Context, base *Base, status *Status) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	states, err := SkillDrift(base.Root())
	if err != nil {
		return err
	}
	var drifted, missing []string
	for _, state := range states {
		if err := checkContext(ctx); err != nil {
			return err
		}
		switch {
		case !state.Present:
			missing = append(missing, state.URI)
		case !state.Current:
			drifted = append(drifted, state.URI)
		}
	}
	if len(missing) > 0 {
		status.addFinding("skills", SeverityWarning, "fkf-owned skills are missing from this base",
			"fkf init "+shellArg(base.Root()), missing...)
	}
	if len(drifted) > 0 {
		status.addFinding("skills", SeverityWarning,
			"fkf-owned skills differ from this binary's copy; they are rewritten by init, so local edits are lost",
			"fkf init "+shellArg(base.Root()), drifted...)
	}
	return checkHelpers(ctx, base, status)
}

func checkHelpers(ctx context.Context, base *Base, status *Status) error {
	report, err := InspectHelpers(ctx, base, false)
	if err != nil {
		return err
	}
	var missing, drifted []string
	for _, helper := range report.Helpers {
		switch helper.State {
		case HelperMissing:
			missing = append(missing, helper.Path)
		case HelperDrifted:
			drifted = append(drifted, helper.Path)
		}
	}
	if len(missing) > 0 {
		status.addFinding("helpers", SeverityWarning, "official helpers required by this base are missing",
			baseCommand(base.Root(), "config helpers --refresh"), missing...)
	}
	if len(drifted) > 0 {
		status.addFinding("helpers", SeverityWarning, "official helpers differ from this binary's copy",
			baseCommand(base.Root(), "config helpers --refresh"), drifted...)
	}
	return nil
}

var conflictMarkerBytes = [][]byte{
	[]byte("<<<<<<< "),
	[]byte("=======\n"),
	[]byte(">>>>>>> "),
}

func checkConflictMarkers(ctx context.Context, base *Base, status *Status) error {
	var conflicted []string
	for _, layer := range []core.Layer{core.LayerEvents, core.LayerIndex} {
		found, err := conflictMarkersInLayer(ctx, base, layer)
		if err != nil {
			return err
		}
		conflicted = append(conflicted, found...)
	}
	if len(conflicted) > 0 {
		status.addFinding("conflict-markers", SeverityError,
			"collected JSON documents contain unresolved git merge conflicts",
			"resolve the conflict or re-collect the day with `"+baseCommand(base.Root(), "sync --force")+"`", conflicted...)
	}
	return nil
}

func conflictMarkersInLayer(ctx context.Context, base *Base, layer core.Layer) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if !base.Store.Enabled(layer) {
		return nil, nil
	}
	dir, err := base.Store.Dir(layer)
	if err != nil {
		return nil, nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var conflicted []string
	err = filepath.WalkDir(dir, func(current string, entry fs.DirEntry, walkErr error) error {
		relative, conflict, err := inspectConflictDocument(ctx, base, current, entry, walkErr)
		if err != nil {
			return err
		}
		if conflict {
			conflicted = append(conflicted, relative)
		}
		return nil
	})
	return conflicted, err
}

func inspectConflictDocument(
	ctx context.Context, base *Base, current string, entry fs.DirEntry, walkErr error,
) (string, bool, error) {
	if err := checkContext(ctx); err != nil {
		return "", false, err
	}
	if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
		return "", false, walkErr
	}
	relative, err := base.Store.Relative(current)
	if err != nil {
		return "", false, nil
	}
	// A filesystem walk discovers names, but it grants no authority to follow them. Resolve
	// the URI again and apply the collection-document bound before observing bytes.
	data, err := base.ReadFileContext(ctx, relative, core.MaxSourceDocumentBytes)
	if err != nil {
		if ctxErr := checkContext(ctx); ctxErr != nil {
			return "", false, ctxErr
		}
		// The document audit reports unsafe, unreadable, and oversized entries with the
		// actionable source name. This audit must only answer whether safe bytes conflict.
		return "", false, nil
	}
	return relative, containsConflictMarker(data), nil
}

func containsConflictMarker(data []byte) bool {
	for _, marker := range conflictMarkerBytes {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

func checkPermissions(ctx context.Context, base *Base, status *Status) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	// WalkDir does not follow a symlink when the root argument itself is one. Bases may
	// deliberately be opened through an alias (`~/brain -> /data/brain`), so resolve only the
	// walk root while keeping the chosen spelling for trust identity, commands, and reports.
	walkRoot, err := filepath.EvalSymlinks(base.Root())
	if err != nil {
		return fmt.Errorf("resolve base for permission audit: %w", err)
	}
	binDir := filepath.Join(walkRoot, core.BaseBinDir)
	var wrongMode []string
	err = filepath.WalkDir(walkRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		want := wantFileMode(current, binDir, entry.IsDir(), info.Mode())
		if info.Mode().Perm() != want {
			relative, err := filepath.Rel(walkRoot, current)
			if err != nil {
				return err
			}
			wrongMode = append(wrongMode, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(wrongMode) > 0 {
		// `find <symlink>` does not traverse its target by default, so the external remedy uses
		// the same resolved root the audit just proved.
		fix := permissionRepairCommand(walkRoot)
		status.addFinding("permissions", SeverityWarning,
			"files or directories are not owner-only; a base can hold mail and shell-activity metadata",
			fix, wrongMode...)
	}
	return nil
}

func permissionRepairCommand(root string) string {
	quotedRoot := shellArg(root)
	quotedGit := shellArg(filepath.Join(root, ".git"))
	quotedBin := shellArg(filepath.Join(root, core.BaseBinDir))
	return "chmod 700 " + quotedRoot +
		" && find " + quotedRoot + " -path " + quotedGit + " -prune -o -type d -exec chmod 700 {} +" +
		" && find " + quotedRoot + " -path " + quotedGit + " -prune -o -path " + quotedBin +
		" -prune -o -type f -exec chmod 600 {} +" +
		" && if [ -d " + quotedBin + " ]; then find " + quotedBin +
		` -type f -exec sh -c 'for file do if [ -x "$file" ]; then chmod 700 "$file"; ` +
		`else chmod 600 "$file"; fi; done' sh {} +; fi`
}

func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func wantFileMode(current, binDir string, isDir bool, currentMode fs.FileMode) fs.FileMode {
	if isDir {
		return core.BaseDirMode
	}
	// bin/ may hold both invoked helpers and non-executable sourced/data files at any depth.
	// Keep that intent while removing every group/other permission; trust separately records the
	// executable bit, so changing it here would also change what the owner approved.
	if pathBelow(current, binDir) && currentMode.Perm()&0o111 != 0 {
		return 0o700
	}
	return core.BaseFileMode
}

func pathBelow(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func checkDerived(base *Base, status *Status) {
	graphURI := core.GraphFile
	if !base.Exists(graphURI) {
		status.addFinding("derived", SeverityWarning,
			"the graph cache is absent, so `graph <uri>` and `context --expand` have nothing to read",
			baseCommand(base.Root(), "build graph"), graphURI)
	}
}

func checkLearnedBacklog(ctx context.Context, base *Base, status *Status) error {
	if !base.Store.Enabled(core.LayerTasks) {
		return nil
	}
	learned, err := ListLearned(ctx, base, Window{}, true)
	if err != nil {
		return err
	}
	if learned.Unharvested > 0 {
		status.addFinding("learned", SeverityWarning,
			fmt.Sprintf("%d \"## Learned\" bullet(s) across your task traces have not been promoted "+
				"into a wiki or projects page yet", learned.Unharvested),
			baseCommand(base.Root(), "list tasks learned --unharvested"))
	}
	return nil
}

func checkDocuments(ctx context.Context, base *Base, status *Status) error {
	verifyReport, err := Verify(ctx, base)
	if err != nil {
		return err
	}
	for _, finding := range verifyReport.Findings {
		status.addFinding("documents", SeverityError,
			fmt.Sprintf("%s: %s", finding.URI, finding.Problem),
			"re-collect the day or fix the document JSON", finding.URI)
	}
	return nil
}
