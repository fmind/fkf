package services

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf find` is window-first: it lists the dates under events/ once, filters them, and opens
// only the days that survive. That is what keeps a read cheap on a base with years of history,
// and it is why a corrupt document outside the window is never even parsed.
//
// Filters read each document's OWN field map rather than the live fkf.yaml, so editing a path
// today never changes what yesterday's records mean.
//
// A searching find covers every enabled layer, not just the collected records. A command
// called `find` that could not reach the wiki left three lexical searches on the surface —
// `find`, `wiki search`, `projects search` — split by which layer the answer happened to be
// in, which is the one thing the asker does not know yet. The layer commands keep their
// `search` verb as scoped sugar over the same scorer.

// FindFilter is the whole filter surface.
type FindFilter struct {
	Sources []string
	Layers  []core.Layer
	Window  Window
	Grep    []string
	Where   []WhereClause
	Limit   int
}

// selects reports whether the filter actually narrows anything. A bare `fkf find` is "what
// arrived lately" and answers from the dated window alone; the moment a term or a filter is
// given the question changes to "where does this appear", and the answer is wrong if it
// silently omits the index — which on a real base is more records than events holds.
func (f FindFilter) selects() bool {
	return len(f.Sources) > 0 || len(f.Grep) > 0 || len(f.Where) > 0
}

// resultLimit keeps the safe discovery default out of actual searches. A zero-value service
// request means the caller did not choose a limit: a bare recent-record discovery is bounded,
// while a lexical, entity, or field query must reach every match and every admitted layer.
// Positive limits and the explicit NoFindLimit sentinel pass through unchanged.
func (f FindFilter) resultLimit() int {
	if f.Limit != 0 {
		return f.Limit
	}
	if f.selects() {
		return NoFindLimit
	}
	return DefaultFindLimit
}

// wants reports whether --layer admits a layer; no --layer means every enabled one.
func (f FindFilter) wants(layer core.Layer) bool {
	return len(f.Layers) == 0 || slices.Contains(f.Layers, layer)
}

// scansEvents and scansIndex are "admitted by --layer AND enabled in this base". A layer the
// base does not enable is skipped when find covers the whole base; validateFindRequest refuses
// it consistently when --layer names it directly.
func (f FindFilter) scansEvents(base *Base) bool {
	return f.wants(core.LayerEvents) && base.Store.Enabled(core.LayerEvents)
}

func (f FindFilter) scansIndex(base *Base) bool {
	return f.wants(core.LayerIndex) && base.Store.Enabled(core.LayerIndex)
}

// recordOnly reports a filter no Markdown page could ever satisfy. --source names a declared
// collector and --where addresses a field inside a stored record; scanning pages for either
// would report "0 pages" as if the question had been asked of them, which it had not.
func (f FindFilter) recordOnly() bool {
	return len(f.Sources) > 0 || len(f.Where) > 0
}

// pageTerms is what a page is matched on.
func (f FindFilter) pageTerms() []string {
	return normalizeTerms(slices.Clone(f.Grep))
}

// WhereClause is one `path=value` equality over a record, using the same jq subset the
// configuration uses so a filter is pasteable into jq.
type WhereClause struct {
	Path  core.FieldPath
	Value string
}

// ParseWhere reads a `--where <path>=<value>` argument.
func ParseWhere(argument string) (WhereClause, error) {
	raw, value, found := strings.Cut(argument, "=")
	if !found {
		return WhereClause{}, fmt.Errorf("--where takes <path>=<value>, for example --where .state=MERGED (got %q)", argument)
	}
	parsed, err := core.ParseFieldPath(strings.TrimSpace(raw))
	if err != nil {
		return WhereClause{}, fmt.Errorf("--where: %w", err)
	}
	return WhereClause{Path: parsed, Value: strings.TrimSpace(value)}, nil
}

// FindRecord is one matching record, stamped with everything needed to cite it.
type FindRecord struct {
	URI    string              `json:"uri"`
	Source string              `json:"source"`
	Date   string              `json:"date,omitempty"`
	Time   string              `json:"time,omitempty"`
	Title  string              `json:"title,omitempty"`
	URL    string              `json:"url,omitempty"`
	Fields map[string][]string `json:"fields,omitempty"`
	Body   bool                `json:"body,omitempty"`
	Record sources.Record      `json:"record,omitempty"`
}

// SourceCount is one source's volume within one day.
type SourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

// DayVolume is one day's totals, which is what `--count` prints.
type DayVolume struct {
	Date    string        `json:"date"`
	Total   int           `json:"total"`
	Sources []SourceCount `json:"sources"`
}

// FindResult is what `fkf find` returns: the matching Markdown pages, then the matching
// collected records. Pages come first and are never dropped by --limit, because there are a
// few hundred of them against years of records and a truncated record list must not be able
// to hide the durable page that answered the question.
type FindResult struct {
	Window  Window       `json:"window"`
	Days    []string     `json:"days,omitempty"`
	Pages   []SearchHit  `json:"pages,omitempty"`
	Records []FindRecord `json:"records,omitempty"`
	Volumes []DayVolume  `json:"volumes,omitempty"`
	// Scanned and Matched count RECORDS only, before --limit. Pages have their own count in
	// len(Pages) because a few hundred scanned pages folded into a record total would make
	// "27 of 366 scanned" mean nothing at all.
	Scanned   int  `json:"scanned"`
	Matched   int  `json:"matched"`
	Truncated bool `json:"truncated,omitempty"`
}

// DefaultFindDays is what a query with neither a window nor a filter falls back to: the last
// seven days that actually hold records, not the last seven calendar days, so a quiet week
// still answers with something.
const DefaultFindDays = 7

// DefaultFindLimit bounds a record listing. It exists so a bare `fkf find` cannot dump a
// year into an agent's context window.
const DefaultFindLimit = 200

// NoFindLimit asks for every record in the window. It exists so an internal caller can say
// "the window is the bound" explicitly: a zero Limit means "apply the service default", and
// `fkf context` silently inherited that 200-record cap while reporting the full window.
const NoFindLimit = -1

// resolveEventDates is the dated half of Find, resolved only when it is going to be scanned.
// Asking the store for the event dates unconditionally made `fkf find retrieval` fail with
// "layer events is disabled" on a base that keeps only wiki and projects — including with an
// explicit `--layer wiki` — which is the opposite of what a command that covers every enabled
// layer should do. An explicit `--layer events` still gets the refusal, because "you turned it
// off" and "it is empty" are different answers and only the direct request deserves the first.
func resolveEventDates(base *Base, filter FindFilter) ([]string, error) {
	if filter.scansEvents(base) {
		dates, err := base.EventDates()
		if err != nil {
			return nil, err
		}
		return selectDates(dates, filter), nil
	}
	return nil, nil
}

// Find scans the base under the filter.
func Find(ctx context.Context, base *Base, filter FindFilter, counting bool) (*FindResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := validateFindRequest(base, filter); err != nil {
		return nil, err
	}
	filter.Grep = normalizeTerms(filter.Grep)
	// Validated against every DECLARED source, not only the enabled ones: a document already
	// on disk from a source since disabled is still real evidence, and `--source` has to keep
	// finding it. What it must never do is silently return "0 of 0 record(s)" for a name that
	// was simply mistyped — `github-prs` for `github-pull-requests` looked exactly like an
	// empty base, which for an agent is a false claim about the user's own history.
	selected, err := resolveEventDates(base, filter)
	if err != nil {
		return nil, err
	}
	result := &FindResult{Window: filter.Window, Days: selected}
	limit := filter.resultLimit()
	if counting {
		// A zero limit is the CLI count default and remains exhaustive. MCP supplies an
		// explicit positive cap so only the returned DayVolume slice is bounded; the scan
		// must continue to keep Scanned and Matched truthful for the requested window.
		limit = filter.Limit
	}
	// The Markdown layers are scanned before the records so the record limit cannot truncate
	// them away, and only when the filter selects: a bare `fkf find` is "what arrived lately",
	// which is a question about dated records alone.
	if filter.selects() && !counting {
		if err := scanPages(ctx, base, filter, result); err != nil {
			return nil, err
		}
	}
	if err := scanFindDays(ctx, base, selected, filter, counting, limit, result); err != nil {
		return nil, err
	}
	// The index has no date, so no window bounds it: an index document is the state
	// of things now. It is scanned only when the filter selects, and after the dated days, so
	// a bare listing keeps its recency order and a search reaches the whole base.
	// !result.Truncated is deliberate and is NOT a way to skip the layer: once the record limit
	// is full there is nowhere to put an index match, and scanning anyway would read every index
	// document to discard the result. An explicit `--layer index` never reaches here truncated,
	// because no event day was scanned to fill the limit. Count mode is different: it keeps
	// scanning after its output slice fills so its complete Scanned and Matched totals stay true.
	if err := scanFindIndex(ctx, base, filter, counting, limit, result); err != nil {
		return nil, err
	}
	finishFindResult(result, counting)
	return result, nil
}

func validateFindRequest(base *Base, filter FindFilter) error {
	for _, term := range filter.Grep {
		if strings.TrimSpace(term) == "" {
			return fmt.Errorf("%w: --grep terms must contain non-whitespace text", core.ErrConfig)
		}
	}
	for _, layer := range filter.Layers {
		if !base.Store.Enabled(layer) {
			return core.ErrLayerDisabled{Layer: layer}
		}
	}
	if len(filter.Layers) > 0 && !filter.selects() {
		windowedEvents := (filter.Window.Since != "" || filter.Window.Until != "") &&
			slices.Contains(filter.Layers, core.LayerEvents)
		if !windowedEvents {
			return fmt.Errorf("%w: --layer narrows a find question but none was given; add terms or a record predicate, or use `fkf list %s` to browse the layer",
				core.ErrConfig, filter.Layers[0])
		}
	}
	return requireKnown("source", filter.Sources, base.Config.SourceNames())
}

func scanFindDays(
	ctx context.Context, base *Base, selected []string, filter FindFilter, counting bool, limit int, result *FindResult,
) error {
	for index := len(selected) - 1; index >= 0; index-- {
		if err := checkContext(ctx); err != nil {
			return err
		}
		volume, err := scanDay(ctx, base, selected[index], filter, counting, limit, result)
		if err != nil {
			return err
		}
		if counting && volume.Total > 0 {
			appendCountVolume(result, volume, limit)
		}
		if result.Truncated && !counting {
			break
		}
	}
	return nil
}

func scanFindIndex(ctx context.Context, base *Base, filter FindFilter, counting bool, limit int, result *FindResult) error {
	if filter.selects() && (!result.Truncated || counting) && filter.scansIndex(base) {
		return scanIndex(ctx, base, filter, counting, limit, result)
	}
	return nil
}

// appendCountVolume bounds only the materialized count rows. Reaching the limit exactly is
// not truncation; the flag becomes true only when one more non-empty volume exists. Callers
// keep scanning after that point so the aggregate counters still cover the complete window.
func appendCountVolume(result *FindResult, volume DayVolume, limit int) {
	if volume.Total == 0 {
		return
	}
	if limit > 0 && len(result.Volumes) >= limit {
		result.Truncated = true
		return
	}
	result.Volumes = append(result.Volumes, volume)
}

func finishFindResult(result *FindResult, counting bool) {
	if counting && result.Volumes == nil {
		result.Volumes = []DayVolume{}
	}
	if !counting && result.Records == nil {
		result.Records = []FindRecord{}
	}
	// Ordering belongs to the service, not to one caller. While `fkf find` sorted its own copy
	// the MCP `find` tool returned the raw scan order, so the same query answered two clients
	// two ways — and "same query, same base, same binary, same answer" is the property the whole
	// read path is built on.
	SortFindRecords(result.Records)
}

// scanPages folds wiki, projects, and tasks into the same result using the shared page scorer.
// The window bounds task pages; a wiki concept or project
// is true today whatever --since said, and excluding it because it has no date would answer a
// different question than the one asked.
func scanPages(ctx context.Context, base *Base, filter FindFilter, result *FindResult) error {
	terms := filter.pageTerms()
	if len(terms) == 0 || filter.recordOnly() {
		return nil
	}
	if err := scanUndatedPages(ctx, base, filter, terms, result); err != nil {
		return err
	}
	if err := scanTaskPages(ctx, base, filter, terms, result); err != nil {
		return err
	}
	SortSearchHits(result.Pages)
	// --limit bounds each half on its own. Sharing one budget would mean a base with a year of
	// records could never show a page, which is the failure this whole scan exists to remove.
	limit := filter.resultLimit()
	if limit > 0 && len(result.Pages) > limit {
		result.Pages = result.Pages[:limit]
	}
	return nil
}

func scanUndatedPages(ctx context.Context, base *Base, filter FindFilter, terms []string, result *FindResult) error {
	// Each layer is scanned whole and the bound is applied once, over the merged list: a
	// per-layer cap would let a low-scoring wiki page survive while a better project page,
	// ranked below its own layer's cap, never reached the comparison.
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !filter.wants(layer) || !base.Store.Enabled(layer) {
			continue
		}
		hits, err := SearchPages(ctx, base, layer, terms, PageFilter{})
		if err != nil {
			return err
		}
		result.Pages = append(result.Pages, hits.Hits...)
	}
	return nil
}

func scanTaskPages(ctx context.Context, base *Base, filter FindFilter, terms []string, result *FindResult) error {
	// Tasks are gathered inside the same function rather than returned from early, because the
	// sort and the bound below apply to the MERGED list. Returning here when the tasks layer was
	// off left the wiki and project hits in layer order and unbounded — a base without tasks got
	// a different ranking, and a different length, from the same query.
	if filter.wants(core.LayerTasks) && base.Store.Enabled(core.LayerTasks) {
		traces, err := ListTasks(ctx, base, filter.Window, 0)
		if err != nil {
			return err
		}
		for _, trace := range traces.Traces {
			if err := checkContext(ctx); err != nil {
				return err
			}
			hit, matched := scorePage(trace.page, terms)
			if !matched {
				continue
			}
			hit.Layer, hit.Date, hit.Slug = core.LayerTasks, trace.Date, trace.Slug
			result.Pages = append(result.Pages, hit)
		}
	}
	return nil
}

// SortSearchHits orders hits by score, then by URI, so the same base and the same terms
// always produce the same list. Retrieval is reproducible or it is not evidence.
func SortSearchHits(hits []SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].URI < hits[j].URI
	})
}

// scanIndex folds the point-in-time documents into the same result. They carry no date, so
// they are reported under an empty DayVolume date rather than pretending to belong to a day.
func scanIndex(ctx context.Context, base *Base, filter FindFilter, counting bool, limit int, result *FindResult) error {
	if !base.Store.Enabled(core.LayerIndex) {
		return nil
	}
	names, err := base.IndexDocuments()
	if err != nil {
		return err
	}
	volume := DayVolume{Sources: []SourceCount{}}
	for _, name := range names {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if len(filter.Sources) > 0 && !contains(filter.Sources, name) {
			continue
		}
		document, err := base.ReadDocumentContext(ctx, sources.IndexDocumentURI(name))
		if err != nil {
			return err
		}
		matched, err := matchDocument(ctx, document, filter)
		if err != nil {
			return err
		}
		result.Scanned += document.Count
		result.Matched += len(matched)
		if len(matched) > 0 {
			volume.Total += len(matched)
			volume.Sources = append(volume.Sources, SourceCount{Source: name, Count: len(matched)})
		}
		if counting {
			continue
		}
		for _, record := range matched {
			if limit > 0 && len(result.Records) >= limit {
				result.Truncated = true
				return nil
			}
			result.Records = append(result.Records, record)
		}
	}
	if counting && volume.Total > 0 {
		appendCountVolume(result, volume, limit)
	}
	return nil
}

// scanDay opens one day's documents and folds their matches into the result. It is separate
// from Find because the window-first rule is the interesting half and it stays readable only
// while the per-day work lives somewhere else.
func scanDay(ctx context.Context, base *Base, date string, filter FindFilter, counting bool, limit int, result *FindResult) (DayVolume, error) {
	volume := DayVolume{Date: date, Sources: []SourceCount{}}
	names, err := base.DayDocuments(date)
	if err != nil {
		return volume, err
	}
	for _, name := range names {
		if err := checkContext(ctx); err != nil {
			return volume, err
		}
		if len(filter.Sources) > 0 && !contains(filter.Sources, name) {
			continue
		}
		document, err := base.ReadDocumentContext(ctx, sources.EventDocumentURI(date, name))
		if err != nil {
			return volume, err
		}
		matched, err := matchDocument(ctx, document, filter)
		if err != nil {
			return volume, err
		}
		result.Scanned += document.Count
		result.Matched += len(matched)
		if len(matched) > 0 {
			volume.Total += len(matched)
			volume.Sources = append(volume.Sources, SourceCount{Source: name, Count: len(matched)})
		}
		if counting {
			continue
		}
		for _, record := range matched {
			if limit > 0 && len(result.Records) >= limit {
				result.Truncated = true
				return volume, nil
			}
			result.Records = append(result.Records, record)
		}
	}
	return volume, nil
}

func selectDates(dates []string, filter FindFilter) []string {
	selected := make([]string, 0, len(dates))
	for _, date := range dates {
		if filter.Window.Contains(date) {
			selected = append(selected, date)
		}
	}
	if filter.Window.Since == "" && filter.Window.Until == "" && !filter.hasPredicate() && len(selected) > DefaultFindDays {
		selected = selected[len(selected)-DefaultFindDays:]
	}
	return selected
}

func (f FindFilter) hasPredicate() bool {
	return f.selects()
}

func matchDocument(ctx context.Context, document *sources.Document, filter FindFilter) ([]FindRecord, error) {
	matched := make([]FindRecord, 0, len(document.Records))
	for _, record := range document.Records {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		values := map[string]any(record)
		projected := project(document, record)
		if !matchesRecord(values, filter) {
			continue
		}
		matched = append(matched, projected)
	}
	return matched, nil
}

func project(document *sources.Document, record sources.Record) FindRecord {
	values := map[string]any(record)
	projected := FindRecord{Source: document.Source, Date: document.Date, Body: document.Body, Record: record}
	projected.URI, _ = document.RecordURI(record)
	if raw, ok := document.Fields.EvalString(core.FieldTime, values); ok {
		if parsed, err := sources.ParseRecordTime(raw); err == nil {
			projected.Time = parsed.Format("2006-01-02T15:04:05Z")
		}
	}
	projected.Title, _ = document.Fields.EvalString(core.FieldTitle, values)
	projected.URL, _ = document.Fields.EvalString(core.FieldURL, values)
	for _, name := range document.Fields.Names() {
		if core.IsWellKnownField(name) {
			continue
		}
		if projectedValues := document.Fields.EvalStrings(name, values); len(projectedValues) > 0 {
			if projected.Fields == nil {
				projected.Fields = map[string][]string{}
			}
			projected.Fields[name] = projectedValues
		}
	}
	return projected
}

func matchesRecord(values map[string]any, filter FindFilter) bool {
	for _, clause := range filter.Where {
		if !matchesWhere(values, clause) {
			return false
		}
	}
	for _, term := range filter.Grep {
		if !grepRecord(values, term) {
			return false
		}
	}
	return true
}

func matchesWhere(values map[string]any, clause WhereClause) bool {
	for _, selected := range clause.Path.EvalStrings(values) {
		if strings.EqualFold(selected, clause.Value) {
			return true
		}
	}
	return false
}

// grepRecord matches scalar leaf values only, never keys or stringified compounds. Matching
// keys made `--grep author` return every record from every source that happens to have an
// author field; stringifying objects would make formatting syntax part of the search surface.
func grepRecord(value any, term string) bool {
	needle := strings.ToLower(term)
	found := false
	walkScalarLeaves(value, func(text string) bool {
		if strings.Contains(strings.ToLower(text), needle) {
			found = true
			return false
		}
		return true
	})
	return found
}

func walkScalarLeaves(value any, visit func(string) bool) bool {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(typed) {
			if !walkScalarLeaves(typed[key], visit) {
				return false
			}
		}
	case []any:
		for _, item := range typed {
			if !walkScalarLeaves(item, visit) {
				return false
			}
		}
	default:
		if text, scalar := core.ScalarString(typed); scalar {
			return visit(text)
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// SortFindRecords orders a result newest first, then by URI so the output is stable. Find calls
// it before returning; it stays exported because a caller that merges two results re-sorts.
func SortFindRecords(records []FindRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Time != records[j].Time {
			return records[i].Time > records[j].Time
		}
		return records[i].URI < records[j].URI
	})
}
