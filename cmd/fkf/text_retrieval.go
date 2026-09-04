package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// writeFindText prints the two halves in the order they are worth reading: the durable pages,
// which are few and answer the question outright, then the records, which are the long tail.
// Each half leads with the key you would sort it by — the layer for a page, the timestamp for
// a record — and then the URI `fkf read` takes.
func writeFindText(w *textWriter, result *services.FindResult) {
	if len(result.Volumes) > 0 {
		writeVolumesText(w, result)
		writeLexicalIndexUseText(w, result.Index)
		return
	}
	for _, hit := range result.Pages {
		w.printf("%-9s %s\n", hit.Layer, hit.URI)
		if summary := pageSummary(hit); summary != "" {
			w.printf("          %s\n", inline(summary))
		}
	}
	if len(result.Pages) > 0 && len(result.Records) > 0 {
		w.line("")
	}
	for _, record := range result.Records {
		w.printf("%s  %s\n", orDash(record.Time), record.URI)
		if record.Title != "" {
			w.printf("    %s\n", inline(record.Title))
		}
	}
	w.line("")
	parts := make([]string, 0, 2)
	if len(result.Pages) > 0 {
		parts = append(parts, fmt.Sprintf("%d page(s)", len(result.Pages)))
	}
	// The record clause is dropped when no record layer was scanned, because "0 of 0 record(s)"
	// reads as an empty answer to a question `--layer wiki` never asked.
	if result.Scanned > 0 || len(result.Pages) == 0 {
		clause := fmt.Sprintf("%d of %d record(s) scanned", result.Matched, result.Scanned)
		if result.Truncated {
			clause += " (truncated; raise --limit)"
		}
		parts = append(parts, clause)
	}
	w.line(strings.Join(parts, ", "))
	writeLexicalIndexUseText(w, result.Index)
}

func lexicalIndexUseText(index *services.LexicalIndexUse) string {
	if index == nil || index.Path == "" {
		return ""
	}
	if index.Used {
		return index.Path + " used"
	}
	return index.Path + " fallback=" + orDash(index.Reason)
}

func writeLexicalIndexUseText(w *textWriter, index *services.LexicalIndexUse) {
	if state := lexicalIndexUseText(index); state != "" {
		w.printf("index %s\n", state)
	}
}

// pageSummary is the one line that says what a hit is about: its title, or the excerpt around
// the match when the page has no title to offer.
func pageSummary(hit services.SearchHit) string {
	if hit.Title != "" {
		return hit.Title
	}
	return hit.Excerpt
}

// writeVolumesText totals each source over the window rather than printing the per-day matrix
// the JSON carries: a week of twenty sources is a hundred and forty lines nothing could read,
// the same reason topSources names only the busiest few. The header states the window those
// totals cover.
//
// Ordering is by volume, then name. Accumulating in first-seen order sorted the sources of the
// first day and then appended whichever ones only appeared later, so the list read as sorted
// until it visibly restarted, and the same base printed a different order for every window.
func writeVolumesText(w *textWriter, result *services.FindResult) {
	if len(result.Days) > 0 {
		w.printf("%s .. %s  %d day(s)\n\n", result.Days[0], result.Days[len(result.Days)-1], len(result.Days))
	}
	totals := map[string]int{}
	for _, day := range result.Volumes {
		for _, entry := range day.Sources {
			totals[entry.Source] += entry.Count
		}
	}
	order := slices.SortedFunc(maps.Keys(totals), func(a, b string) int {
		if totals[a] != totals[b] {
			return totals[b] - totals[a]
		}
		return strings.Compare(a, b)
	})
	width := uriWidth(len(order), func(i int) string { return order[i] })
	for _, source := range order {
		w.printf("%-*s %6d\n", width, source, totals[source])
	}
	w.printf("\n%d record(s) across %d day(s)\n", result.Matched, len(result.Volumes))
}

// writeContextText is the compact delivery form used by terminals and session hooks. One item
// is one line; the three receipt lines keep the answer auditable without spending another copy
// of the evidence on prose and indentation.
func writeContextText(w *textWriter, pack *services.ContextPack) {
	w.printf("%s", services.RenderContextText(pack))
}

// The four layer listings lead with the URI `fkf read` takes and end with what the item is
// about. They used to print a slug and a count, which meant the one thing a reader needed next
// — the address — had to be reconstructed from a layer name they were expected to know.

func writeEventsText(w *textWriter, listing *services.EventListing) {
	width := uriWidth(len(listing.Days), func(i int) string { return listing.Days[i].URI })
	for _, day := range listing.Days {
		w.printf("%-*s %5d  %s\n", width, day.URI, day.Total, topSources(day.Sources))
	}
	w.printf("\n%d day(s), %d record(s)\n", len(listing.Days), listing.Total)
}

// topSources names the busiest few rather than every source, because a base with twenty of them
// turned one day into a line nothing could read. The count above it is already the full total.
func topSources(counts []services.DayCount) string {
	const shown = 3
	ordered := slices.SortedStableFunc(slices.Values(counts), func(a, b services.DayCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Source, b.Source)
	})
	parts := make([]string, 0, shown+1)
	for _, entry := range ordered[:min(shown, len(ordered))] {
		parts = append(parts, fmt.Sprintf("%s %d", entry.Source, entry.Count))
	}
	if extra := len(ordered) - shown; extra > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", extra))
	}
	return strings.Join(parts, " · ")
}

func writeIndexText(w *textWriter, listing *services.IndexListing) {
	width := uriWidth(len(listing.Entries), func(i int) string { return listing.Entries[i].URI })
	for _, entry := range listing.Entries {
		stale := ""
		if entry.Stale {
			stale = "  stale"
		}
		w.printf("%-*s %6d record(s)  %5dh old  %7d bytes%s\n",
			width, entry.URI, entry.Count, entry.AgeHours, entry.Bytes, stale)
	}
	// Every other listing closes with its own total, so an empty index says so rather than
	// printing nothing and leaving a reader to wonder whether the command ran.
	w.printf("\n%d document(s)\n", len(listing.Entries))
}

func writeTasksText(w *textWriter, listing *services.TaskListing) {
	width := uriWidth(len(listing.Traces), func(i int) string { return listing.Traces[i].URI })
	for _, trace := range listing.Traces {
		w.printf("%-*s  %s\n", width, trace.URI, inline(trace.Title))
	}
	w.printf("\n%d trace(s)\n", len(listing.Traces))
}

// writeLearnedText marks each bullet the same way `fkf status` marks a source: a state word
// first, so the backlog is scannable without reading every line. A bullet is grouped under its
// trace because two bullets from the same session usually belong to the same candidate.
func writeLearnedText(w *textWriter, listing *services.LearnedListing) {
	current := ""
	for _, bullet := range listing.Bullets {
		if bullet.Trace != current {
			w.printf("%s\n", bullet.Trace)
			current = bullet.Trace
		}
		state := "unharvested"
		if bullet.Harvested {
			state = "harvested  "
		}
		w.printf("  %s  %s\n", state, inline(bullet.Text))
	}
	w.printf("\n%d harvested, %d unharvested\n", listing.Harvested, listing.Unharvested)
}

// uriWidth aligns a listing on its own widest URI, capped so one deeply nested path cannot
// push every title off the right of an eighty-column terminal.
func uriWidth(count int, at func(int) string) int {
	const cap = 44
	width := 0
	for index := range count {
		width = max(width, len(at(index)))
	}
	return min(width, cap)
}

func writePagesText(w *textWriter, listing *services.PageListing) {
	width := uriWidth(len(listing.Pages), func(i int) string { return listing.Pages[i].URI })
	for _, page := range listing.Pages {
		// One classifier column, and which one depends on the layer: a project's type is
		// always "project" and its status is the whole point, while a wiki page is the
		// reverse. Printing both would spend a column on a constant.
		classifier := page.Type
		if listing.Layer == core.LayerProjects {
			classifier = page.Status
		}
		w.printf("%-*s  %-9s  %s\n", width, page.URI, orDash(classifier), inline(page.Title))
		if len(page.Tags) > 0 {
			w.printf("%-*s  %-9s  %s\n", width, "", "", strings.Join(page.Tags, " "))
		}
	}
	w.printf("\n%d page(s) in %s/\n", listing.Total, listing.Layer)
}

// overviewDetail is the half of a layer line that differs per layer: a date range for the
// dated ones, and the note the service computed for the rest.
func overviewDetail(layer services.LayerOverview) string {
	switch {
	case layer.Since != "" && layer.Until != "":
		return fmt.Sprintf("%s .. %s", layer.Since, layer.Until)
	case layer.Until != "":
		return "newest " + layer.Until
	default:
		return layer.Note
	}
}

func plural(unit string, count int) string {
	if count == 1 {
		return unit
	}
	return unit + "s"
}

func writeTagsText(w *textWriter, index *services.TagVocabulary) {
	for _, tag := range index.Tags {
		w.printf("%4d  %-24s %s\n", tag.Count, tag.Tag, strings.Join(tag.Pages, " "))
	}
	if len(index.Untagged) > 0 {
		w.printf("\nuntagged: %s\n", strings.Join(index.Untagged, " "))
	}
}

func writeSearchText(w *textWriter, result *services.SearchResult) {
	for _, hit := range result.Hits {
		w.printf("%4d  %s  [%s]\n", hit.Score, hit.URI, strings.Join(hit.Matched, " "))
		if hit.Excerpt != "" {
			w.printf("      %s\n", hit.Excerpt)
		}
	}
	w.printf("\n%d hit(s)\n", len(result.Hits))
}

func writeEdgesText(w *textWriter, neighbourhood *services.Neighbourhood) {
	for _, edge := range neighbourhood.Edges {
		w.printf("%d  %-10s %s -> %s  (%s)\n", edge.Hop, edge.Kind, edge.Src, edge.Dst, edge.Via)
	}
	w.printf("\n%d edge(s), %d node(s), %d row(s) scanned\n",
		len(neighbourhood.Edges), len(neighbourhood.Nodes), neighbourhood.Stats.Lines)
}

// writeGraphSummaryText answers "what is in this graph" before a reader picks a node to walk
// from: the totals, the mix, and when the cache was last rebuilt.
func writeGraphSummaryText(w *textWriter, summary *services.GraphSummary) {
	w.printf("%s  %d edge(s), %d node(s)", summary.URI, summary.Edges, summary.Nodes)
	if summary.GeneratedAt != "" {
		w.printf("  built %s", summary.GeneratedAt)
	}
	w.printf("\n")
	writeKindCounts(w, "edges", summary.EdgeKinds)
	writeKindCounts(w, "nodes", summary.NodeKinds)
	if len(summary.Extractors) > 0 {
		w.printf("%-6s  %s\n", "from", strings.Join(summary.Extractors, " "))
	}
}

func writeKindCounts(w *textWriter, label string, counts []services.KindCount) {
	if len(counts) == 0 {
		return
	}
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", count.Kind, count.Count))
	}
	w.printf("%-6s  %s\n", label, strings.Join(parts, "  "))
}

func writeNodesText(w *textWriter, listing *services.NodeListing) {
	for _, node := range listing.Nodes {
		w.printf("%5d  %-8s %s  (in %d, out %d)\n", node.Total, node.Kind, node.URI, node.In, node.Out)
	}
	w.printf("\n%d node(s)\n", listing.Total)
}
