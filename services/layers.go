package services

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	gast "github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// Every enabled layer exposes the same two verbs. Each `read` is sugar over `fkf read <uri>`;
// the per-layer form exists because it is what a human types and what an MCP `list` returns.

// Window bounds a listing or a query by date. An empty bound is open.
type Window struct {
	Since       string `json:"since,omitempty"`
	Until       string `json:"until,omitempty"`
	DerivedFrom string `json:"derived_from,omitempty"`
}

// dayKeywords name the two days an agent asks for by name. They resolve to one absolute date
// on either bound, unlike a relative window, whose direction depends on which bound it is on:
// `--since yesterday --until yesterday` is one day, while `--since 1d --until 1d` is three.
var dayKeywords = map[string]int{"today": 0, "yesterday": -1}

// ParseWindow reads the two bounds, accepting an absolute YYYY-MM-DD, the day keywords `today`
// and `yesterday`, or a relative `7d`/`6w`/`3m` offset from today. Relative windows are what
// makes a scheduled command correct: an absolute date in a timer unit silently stops moving.
func ParseWindow(since, until string, now time.Time) (Window, error) {
	resolved := Window{}
	for _, bound := range []struct {
		raw   string
		into  *string
		flag  string
		shift int
	}{{since, &resolved.Since, "--since", -1}, {until, &resolved.Until, "--until", 1}} {
		value := strings.TrimSpace(bound.raw)
		if value == "" {
			continue
		}
		if offset, named := dayKeywords[strings.ToLower(value)]; named {
			*bound.into = now.AddDate(0, 0, offset).Format(time.DateOnly)
			continue
		}
		if isoDatePattern.MatchString(value) {
			if _, err := time.Parse(time.DateOnly, value); err != nil {
				return Window{}, fmt.Errorf("%s must be YYYY-MM-DD, today, yesterday, or a window like 7d: %w", bound.flag, err)
			}
			*bound.into = value
			continue
		}
		days, err := parseRelativeWindow(value)
		if err != nil {
			return Window{}, fmt.Errorf("%s: %w", bound.flag, err)
		}
		resolvedDate := now.AddDate(0, 0, bound.shift*days)
		if resolvedDate.Year() < 0 || resolvedDate.Year() > 9999 {
			return Window{}, fmt.Errorf("%s window %q resolves outside the supported YYYY-MM-DD range", bound.flag, value)
		}
		*bound.into = resolvedDate.Format(time.DateOnly)
	}
	if resolved.Since != "" && resolved.Until != "" && resolved.Since > resolved.Until {
		return Window{}, fmt.Errorf("--since %s is after --until %s", resolved.Since, resolved.Until)
	}
	return resolved, nil
}

func parseRelativeWindow(value string) (int, error) {
	invalid := func() (int, error) {
		return 0, fmt.Errorf("%q is not a window; use today, yesterday, 7d, 6w, 3m, or YYYY-MM-DD", value)
	}
	if len(value) < 2 {
		return invalid()
	}
	digits, unit := value[:len(value)-1], value[len(value)-1]
	if digits[0] == '0' {
		return invalid()
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return invalid()
		}
	}
	factor := 0
	switch unit {
	case 'd':
		factor = 1
	case 'w':
		factor = 7
	case 'm':
		factor = 30
	case 'y':
		factor = 365
	default:
		return invalid()
	}
	count, err := strconv.Atoi(digits)
	if err != nil {
		return invalid()
	}
	// A YYYY-MM-DD bound can address only four-digit years. This cap is wider than that
	// calendar span, so the direction-specific year check in ParseWindow makes the final call;
	// its purpose here is to reject multiplication and AddDate overflow before either occurs.
	const maxRelativeWindowDays = 10_000 * 366
	if count > maxRelativeWindowDays/factor {
		return invalid()
	}
	return count * factor, nil
}

// Contains reports whether a date falls inside the window.
func (w Window) Contains(date string) bool {
	return (w.Since == "" || date >= w.Since) && (w.Until == "" || date <= w.Until)
}

// --- events --------------------------------------------------------------------------------

// DayCount is one source's contribution to one day.
type DayCount struct {
	Source string `json:"source"`
	URI    string `json:"uri"`
	Count  int    `json:"count"`
	Body   bool   `json:"body"`
}

// EventDay is one collected day.
type EventDay struct {
	Date    string     `json:"date"`
	URI     string     `json:"uri"`
	Total   int        `json:"total"`
	Sources []DayCount `json:"sources"`
}

// EventListing is what `fkf list events` returns.
type EventListing struct {
	Window Window     `json:"window"`
	Days   []EventDay `json:"days"`
	Total  int        `json:"total"`
}

// ListEvents walks the dates first and opens only the documents that survive the filters —
// the window-first rule that keeps a read cheap on a base with years of history.
func ListEvents(ctx context.Context, base *Base, window Window, source string, limit int) (*EventListing, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	// A typo must not look like an authoritative empty history. Disabled sources remain in
	// the declared vocabulary, so their already-collected documents are still listable.
	if source != "" {
		if err := requireKnown("source", []string{source}, base.Config.SourceNames()); err != nil {
			return nil, err
		}
	}
	dates, err := base.EventDates()
	if err != nil {
		return nil, err
	}
	listing := &EventListing{Window: window, Days: []EventDay{}}
	for index := len(dates) - 1; index >= 0; index-- {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		date := dates[index]
		if !window.Contains(date) {
			continue
		}
		day, err := readEventDay(ctx, base, date, source)
		if err != nil {
			return nil, err
		}
		if len(day.Sources) == 0 {
			continue
		}
		listing.Days = append(listing.Days, day)
		listing.Total += day.Total
		if limit > 0 && len(listing.Days) >= limit {
			break
		}
	}
	return listing, nil
}

func readEventDay(ctx context.Context, base *Base, date, source string) (EventDay, error) {
	names, err := base.DayDocuments(date)
	if err != nil {
		return EventDay{}, err
	}
	day := EventDay{Date: date, URI: path.Join(string(core.LayerEvents), date) + "/", Sources: []DayCount{}}
	for _, name := range names {
		if err := checkContext(ctx); err != nil {
			return EventDay{}, err
		}
		if source != "" && name != source {
			continue
		}
		document, err := base.ReadDocumentContext(ctx, sources.EventDocumentURI(date, name))
		if err != nil {
			return EventDay{}, err
		}
		day.Sources = append(day.Sources, DayCount{
			Source: name, URI: document.URI(), Count: document.Count, Body: document.Body,
		})
		day.Total += document.Count
	}
	return day, nil
}

// ValidateDate is the shared YYYY-MM-DD check for every command that takes a date.
func ValidateDate(value string) error { return core.ValidateDate(value) }

// --- index ---------------------------------------------------------------------------------

// IndexEntry is one point-in-time document under index/.
type IndexEntry struct {
	Name        string `json:"name"`
	URI         string `json:"uri"`
	Count       int    `json:"count,omitempty"`
	Bytes       int64  `json:"bytes"`
	CollectedAt string `json:"collected_at"`
	AgeHours    int    `json:"age_hours"`
	Stale       bool   `json:"stale,omitempty"`
}

// IndexListing is what `fkf list index` returns.
type IndexListing struct {
	Entries []IndexEntry `json:"entries"`
	Total   int          `json:"total"`
}

// ListIndex reports the collected point-in-time documents only. What fkf DERIVES lives under
// the base root and is read through `fkf graph`, so nothing in this listing needs flagging.
func ListIndex(ctx context.Context, base *Base, limit int) (*IndexListing, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	names, err := base.IndexDocuments()
	if err != nil {
		return nil, err
	}
	listing := &IndexListing{Entries: []IndexEntry{}, Total: len(names)}
	now := base.Now()
	maxAge := time.Duration(base.Config.Sync.IndexMaxAgeHours) * time.Hour
	for index, name := range names {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if limit > 0 && index >= limit {
			break
		}
		uri := sources.IndexDocumentURI(name)
		entry := IndexEntry{Name: name, URI: uri}
		document, collectedAt, err := readValidatedIndexDocumentContext(ctx, base, uri)
		if err != nil {
			return nil, err
		}
		if err := describeIndexDocument(base, uri, document, collectedAt, &entry, now, maxAge); err != nil {
			return nil, err
		}
		listing.Entries = append(listing.Entries, entry)
	}
	return listing, nil
}

func describeIndexDocument(
	base *Base, uri string, document *sources.Document, collectedAt time.Time,
	entry *IndexEntry, now time.Time, maxAge time.Duration,
) error {
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", uri, err)
	}
	age := now.Sub(collectedAt)
	entry.Bytes, entry.Count = info.Size(), document.Count
	entry.CollectedAt = document.CollectedAt
	entry.AgeHours = max(0, int(age/time.Hour))
	entry.Stale = age < 0 || age >= maxAge
	return nil
}

// readValidatedIndexDocumentContext is the one freshness clock for point-in-time snapshots. File
// mtimes describe copies, checkouts, and restores; collected_at describes the provider read.
func readValidatedIndexDocumentContext(ctx context.Context, base *Base, uri string) (*sources.Document, time.Time, error) {
	document, err := base.ReadDocumentContext(ctx, uri)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err := sources.VerifyRecords(document); err != nil {
		return nil, time.Time{}, fmt.Errorf("validate %s: %w", uri, err)
	}
	collectedAt, err := time.Parse(time.RFC3339, document.CollectedAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse %s collected_at: %w", uri, err)
	}
	return document, collectedAt, nil
}

// --- tasks ---------------------------------------------------------------------------------

// TaskTrace is one execution trace.
type TaskTrace struct {
	Date  string `json:"date"`
	Slug  string `json:"slug"`
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
	Bytes int    `json:"bytes"`

	// page is the parsed trace, kept so a context pack can rank it without a second read.
	page Page
}

// TaskListing is what `fkf list tasks` returns.
type TaskListing struct {
	Window Window      `json:"window"`
	Traces []TaskTrace `json:"traces"`
}

// ListTasks reports the traces in the window, newest first.
func ListTasks(ctx context.Context, base *Base, window Window, limit int) (*TaskListing, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := base.RequireLayer(core.LayerTasks); err != nil {
		return nil, err
	}
	directory, err := base.Store.Dir(core.LayerTasks)
	if err != nil {
		return nil, err
	}
	dates, err := readDateDirectories(directory)
	if err != nil {
		return nil, err
	}
	listing := &TaskListing{Window: window, Traces: []TaskTrace{}}
	for index := len(dates) - 1; index >= 0; index-- {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		date := dates[index]
		if !window.Contains(date) {
			continue
		}
		slugs, err := readSubdirectories(path.Join(directory, date))
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			uri := path.Join(string(core.LayerTasks), date, slug, core.TaskTraceFile)
			if !base.Exists(uri) {
				continue
			}
			page, err := ReadPageContext(ctx, base, uri)
			if err != nil {
				return nil, err
			}
			listing.Traces = append(listing.Traces, TaskTrace{
				Date: date, Slug: slug, URI: uri, Title: page.Title, Bytes: page.Bytes, page: page,
			})
			if limit > 0 && len(listing.Traces) >= limit {
				return listing, nil
			}
		}
	}
	return listing, nil
}

// LearnedBullet is one "## Learned" line from a task trace, with whether some wiki or
// projects page has already cited the trace it came from.
type LearnedBullet struct {
	Trace     string `json:"trace"`
	Text      string `json:"text"`
	Harvested bool   `json:"harvested"`
}

// LearnedListing is what `fkf list tasks learned` returns.
type LearnedListing struct {
	Window      Window          `json:"window"`
	Bullets     []LearnedBullet `json:"bullets"`
	Harvested   int             `json:"harvested"`
	Unharvested int             `json:"unharvested"`
}

// ListLearned scans every task trace in the window for its "## Learned" bullets, and marks a
// bullet harvested when some wiki or projects page's `sources:` frontmatter already cites the
// trace it came from.
//
// It exists because a backlog nobody can enumerate is a backlog nobody works: task traces on a
// real base carried dozens of "## Learned" bullets and zero of them had become a wiki page, and
// nothing said so. This is a deterministic lexical scan over Markdown fkf already parses —
// nothing is inferred and nothing is written — so it costs nothing to run on every session.
func ListLearned(ctx context.Context, base *Base, window Window, onlyUnharvested bool) (*LearnedListing, error) {
	traces, err := ListTasks(ctx, base, window, 0)
	if err != nil {
		return nil, err
	}
	cited, err := citedTraces(ctx, base)
	if err != nil {
		return nil, err
	}
	listing := &LearnedListing{Window: window, Bullets: []LearnedBullet{}}
	for _, trace := range traces.Traces {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		harvested := cited[trace.URI]
		for _, text := range learnedBullets(trace.page) {
			if harvested {
				listing.Harvested++
			} else {
				listing.Unharvested++
			}
			if onlyUnharvested && harvested {
				continue
			}
			listing.Bullets = append(listing.Bullets, LearnedBullet{Trace: trace.URI, Text: text, Harvested: harvested})
		}
	}
	return listing, nil
}

// learnedBullets extracts every list item under every heading literally named "Learned", at
// any level, stopping each section at the next heading of the same level or shallower. The
// heading text is matched exactly against the template AGENTS.md itself writes into every base
// ("## Learned"), so a differently named section is left alone rather than guessed at.
func learnedBullets(page Page) []string {
	source := []byte(page.Body)
	document := markdownParser.Parse(gmtext.NewReader(source))
	var bullets []string
	activeLevel := 0
	for block := document.FirstChild(); block != nil; block = block.NextSibling() {
		if heading, ok := block.(*gast.Heading); ok {
			text := strings.TrimSpace(renderedMarkdownText(heading, source))
			if text == "Learned" {
				activeLevel = heading.Level
			} else if activeLevel > 0 && heading.Level <= activeLevel {
				activeLevel = 0
			}
			continue
		}
		if activeLevel == 0 {
			continue
		}
		_ = gast.Walk(block, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
			item, ok := node.(*gast.ListItem)
			if !entering || !ok {
				return gast.WalkContinue, nil
			}
			list, ok := item.Parent().(*gast.List)
			if !ok || list.IsOrdered() {
				return gast.WalkContinue, nil
			}
			if text := learnedListItemText(item, source); text != "" {
				bullets = append(bullets, text)
			}
			return gast.WalkContinue, nil
		})
	}
	return bullets
}

func learnedListItemText(item *gast.ListItem, source []byte) string {
	var parts []string
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		if _, nested := child.(*gast.List); nested {
			continue
		}
		if text := strings.Join(strings.Fields(renderedMarkdownText(child, source)), " "); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// citedTraces returns the task-trace URIs some wiki or projects page's `sources:` frontmatter
// already names — the same list value pageEdges already reads for the graph, resolved the same
// way, so "cited" here means exactly what the graph would draw an edge for.
func citedTraces(ctx context.Context, base *Base) (map[string]bool, error) {
	cited := map[string]bool{}
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if !base.Store.Enabled(layer) {
			continue
		}
		listing, err := ListPages(ctx, base, layer, PageFilter{})
		if err != nil {
			return nil, err
		}
		for _, page := range listing.Pages {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			list, isList := page.Frontmatter["sources"].([]any)
			if !isList {
				continue
			}
			for _, item := range list {
				candidate, ok := core.ScalarString(item)
				if !ok {
					continue
				}
				resolved, err := ResolveLink(page.URI, candidate)
				if err != nil {
					continue
				}
				target, _, _ := strings.Cut(resolved.NodeURI(), "#")
				if strings.HasSuffix(target, "/"+core.TaskTraceFile) {
					cited[target] = true
				}
			}
		}
	}
	return cited, nil
}

func readDateDirectories(directory string) ([]string, error) {
	names, err := readSubdirectories(directory)
	if err != nil {
		return nil, err
	}
	dates := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := time.Parse(time.DateOnly, name); err == nil {
			dates = append(dates, name)
		}
	}
	sort.Strings(dates)
	return dates, nil
}

func readSubdirectories(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
