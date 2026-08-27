package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

// --format text is for a human at a terminal. Every renderer prints the URI, because the URI is
// what the reader will paste into the next command.
//
// Renderers write through a textWriter rather than returning an error each: a broken pipe has to
// be reported once, at the end, and threading `if err != nil` through forty Fprintf calls buys
// nothing but noise.

type textWriter struct {
	out io.Writer
	err error
}

func (w *textWriter) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	// Text output is normally a terminal. Sanitize the completed rendering so every dynamic
	// value receives the same protection, including provider bodies and future renderers.
	_, w.err = fmt.Fprint(w.out, block(fmt.Sprintf(format, args...)))
}

func (w *textWriter) line(text string) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintln(w.out, block(text))
}

// writeText renders a result, reporting whether a renderer existed for it. A --format text that
// silently fell back to JSON looked like it worked, and a flag that appears to work is harder to
// diagnose than one that says it did not.
func writeText(out io.Writer, result any) (error, bool) {
	writer := &textWriter{out: out}
	if renderReadSurface(writer, result) || renderOperations(writer, result) {
		return writer.err, true
	}
	return nil, false
}

func renderReadSurface(w *textWriter, result any) bool {
	switch typed := result.(type) {
	case *services.FindResult:
		writeFindText(w, typed)
	case *services.ContextPack:
		writeContextText(w, typed)
	case *services.ReadResult:
		writeReadText(w, typed)
	case services.Page:
		writePageText(w, typed)
	case *sources.Document:
		writeDocumentText(w, typed)
	case *services.Neighbourhood:
		writeEdgesText(w, typed)
	case *services.NodeListing:
		writeNodesText(w, typed)
	case *services.GraphSummary:
		writeGraphSummaryText(w, typed)
	default:
		return false
	}
	return true
}

func renderOperations(w *textWriter, result any) bool {
	switch typed := result.(type) {
	case *services.EventListing:
		writeEventsText(w, typed)
	case *services.IndexListing:
		writeIndexText(w, typed)
	case *services.TaskListing:
		writeTasksText(w, typed)
	case *services.LearnedListing:
		writeLearnedText(w, typed)
	case *services.PageListing:
		writePagesText(w, typed)
	case *services.TagVocabulary:
		writeTagsText(w, typed)
	case *services.SearchResult:
		writeSearchText(w, typed)
	case *core.Config:
		writeConfigText(w, typed)
	case *services.Status:
		writeStatusText(w, typed)
	case *services.BuildReport:
		writeBuildText(w, typed)
	case *services.NewResult:
		writeNewText(w, typed)
	case *services.SyncReport:
		writeSyncText(w, typed)
	case *services.InitReport:
		writeInitText(w, typed)
	case *services.TrustReport:
		writeTrustText(w, typed)
	case *services.HelperReport:
		writeHelpersText(w, typed)
	case *services.ValidationReport:
		writeValidationText(w, typed)
	case *services.WikiIndexReport:
		writeWikiIndexText(w, typed)
	case *services.GraphBuild:
		writeGraphBuildText(w, typed)
	case *services.UpgradeReport:
		writeUpgradeText(w, typed)
	default:
		return false
	}
	return true
}

func writeUpgradeText(w *textWriter, report *services.UpgradeReport) {
	if report.Updated {
		w.printf("updated: %s -> %s\n", report.Previous, report.Current)
	} else {
		w.printf("up to date: %s\n", report.Current)
	}
	w.printf("path: %s\n", report.Path)
}

// writeFindText prints the two halves in the order they are worth reading: the durable pages,
// which are few and answer the question outright, then the records, which are the long tail.
// Each half leads with the key you would sort it by — the layer for a page, the timestamp for
// a record — and then the URI `fkf read` takes.
func writeFindText(w *textWriter, result *services.FindResult) {
	if len(result.Volumes) > 0 {
		writeVolumesText(w, result)
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
}

// pageSummary is the one line that says what a hit is about: its title, or the excerpt around
// the match when the page has no title to offer.
func pageSummary(hit services.SearchHit) string {
	if hit.Title != "" {
		return hit.Title
	}
	return hit.Excerpt
}

func writeVolumesText(w *textWriter, result *services.FindResult) {
	if len(result.Days) > 0 {
		w.printf("%s .. %s  %d day(s)\n\n", result.Days[0], result.Days[len(result.Days)-1], len(result.Days))
	}
	totals, order := map[string]int{}, []string{}
	for _, day := range result.Volumes {
		for _, entry := range day.Sources {
			if _, known := totals[entry.Source]; !known {
				order = append(order, entry.Source)
			}
			totals[entry.Source] += entry.Count
		}
	}
	for _, source := range order {
		w.printf("%-22s %6d\n", source, totals[source])
	}
	w.printf("\n%d record(s) across %d day(s)\n", result.Matched, len(result.Volumes))
}

// writeContextText leads with what the pack IS — the query, how much of the base it looked at,
// and how much budget it spent — because that is the line that tells a first-time reader what
// the command produced. It used to sit under the items, which on a full pack is a screen and a
// half below the point where the reader decided they did not understand the output.
func writeContextText(w *textWriter, pack *services.ContextPack) {
	receipt := pack.Receipt
	w.printf("pack for %q\n", pack.Query)
	w.printf("%d of %d candidate(s) · %d item tokens · %d/%d encoded tokens",
		receipt.Selected, receipt.Candidates, receipt.UsedTokens, receipt.EncodedTokens, receipt.Budget)
	// A pack over "the last 30 days" reads the same whether collection ran this morning or
	// stopped in May, so the age of the newest collected day is printed, not just the window.
	if receipt.NewestEventDay != "" {
		w.printf(" · newest event %s (%d day(s) ago)", receipt.NewestEventDay, receipt.StaleDays)
	}
	w.line("")
	// Printed here, not only once by `fkf mcp serve` at connection time: this is the ONLY
	// framing a session driven by `fkf-hook` ever sees, since the hook calls --format text
	// directly and never goes through MCP's Instructions at all.
	w.line(receipt.Notice)
	if len(pack.Items) == 0 {
		w.line("")
		w.line(receipt.Warning)
	}
	w.line("")
	for _, item := range pack.Items {
		pin := ""
		if item.Pinned {
			pin = " [pinned]"
		}
		w.printf("%4d  %-8s %s%s\n", item.Score, item.Kind, item.URI, pin)
		if item.Title != "" {
			w.printf("      %s\n", inline(item.Title))
		}
		for _, reason := range item.Reasons {
			w.printf("      %+5d %s %s\n", reason.Points, reason.Reason, inline(reason.Detail))
		}
	}
	w.printf("\nscore floor %d", receipt.Floor)
	w.line("")
	w.printf("digest %s  as of %s  ranking v%d  fkf %s\n",
		receipt.InputDigest, receipt.AsOf, receipt.RankingVersion, receipt.ToolVersion)
	if receipt.UnharvestedBullets > 0 {
		w.printf("%d `## Learned` bullet(s) not yet promoted — see `fkf list tasks learned --unharvested`\n",
			receipt.UnharvestedBullets)
	}
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

// writeConfigText answers "what does this base collect, and what did I actually get" — the
// resolved values plus the file each one came from, which is the whole reason to ask.
func writeConfigText(w *textWriter, config *core.Config) {
	w.printf("%s  %s\n", config.Name, config.Path)
	if config.LocalPath != "" {
		w.printf("overlay: %s\n", config.LocalPath)
	}
	layers := make([]string, 0, len(config.Layers))
	for _, layer := range slices.Sorted(maps.Keys(config.Layers)) {
		if config.Layers[layer] {
			layers = append(layers, string(layer))
		}
	}
	w.printf("layers:  %s\n", strings.Join(layers, " "))
	w.printf("sync:    %d day(s), timeout %s, concurrency %d, index stale after %dh\n",
		config.Sync.Days, config.Sync.Timeout, config.Sync.Concurrency, config.Sync.IndexMaxAgeHours)
	if len(config.Bin) > 0 {
		w.printf("bin:     %s\n", strings.Join(config.Bin, " "))
	}
	names := config.SourceNames()
	w.printf("\n%d source(s), %d enabled\n", len(names), len(config.EnabledSources()))
	for _, name := range names {
		source := config.Sources[name]
		state := "off"
		if source.Enabled {
			state = "on"
		}
		w.printf("%-24s %-4s %-8s %s\n", name, state, source.Layer, strings.Join(source.Requires, ", "))
	}
	if len(config.Origins) > 0 {
		w.line("")
		for _, key := range slices.Sorted(maps.Keys(config.Origins)) {
			w.printf("%-24s from %s\n", key, config.Origins[key])
		}
	}
}

func writeStatusText(w *textWriter, status *services.Status) {
	trust := "untrusted"
	if status.Trust.Trusted {
		trust = "trusted"
	}
	w.printf("%s  %s  (%s, %s)\n\n", status.Name, status.Base, status.Origin, trust)

	// 1. Layers overview
	for _, layer := range status.Layers {
		if !layer.Enabled {
			w.printf("%-10s %s\n", layer.Layer, "off")
			continue
		}
		w.printf("%s\n", strings.TrimRight(fmt.Sprintf("%-10s %6d %-9s %s",
			layer.Layer, layer.Count, plural(layer.Unit, layer.Count), overviewDetail(layer)), " "))
	}
	if status.Graph != nil {
		w.printf("%-10s %6d %-9s over %d nodes, built %s\n",
			"graph", status.Graph.Edges, plural("edge", status.Graph.Edges),
			status.Graph.Nodes, status.Graph.GeneratedAt)
	} else {
		w.printf("%-10s %s\n", "graph", "not built")
	}

	// 2. Sources table
	if len(status.Sources) > 0 {
		w.printf("\nsources\n")
		for _, source := range status.Sources {
			writeSourceStatusText(w, source)
		}
		w.printf("\n%d enabled, %d missing requirement(s), %d quiet\n", status.Enabled, status.Missing, status.Quiet)
	}

	// 3. Health & Integrity Findings
	if len(status.Findings) > 0 {
		w.printf("\nfindings\n")
		for _, finding := range status.Findings {
			w.printf("  [%s] %-20s %s\n", finding.Severity, finding.Check, finding.Message)
			for _, path := range finding.Paths {
				w.printf("         %s\n", path)
			}
			if finding.Fix != "" {
				w.printf("         fix: %s\n", finding.Fix)
			}
		}
		w.printf("\n%d error(s), %d warning(s)\n", status.Errors, status.Warnings)
	}

	// 4. Next recommendations
	if len(status.Next) > 0 {
		w.printf("\nnext\n")
		for _, line := range status.Next {
			w.printf("  %s\n", line)
		}
	}
}

func writeSourceStatusText(w *textWriter, source services.SourceStatus) {
	state := "off"
	switch {
	case source.Undeclared:
		state = "gone"
	case source.Enabled:
		state = "on"
	}
	requirements := make([]string, 0, len(source.Requires))
	missing := false
	for _, requirement := range source.Requires {
		name := requirement.Name
		if !requirement.OnPath {
			name += " (missing)"
			missing = true
		}
		requirements = append(requirements, name)
	}
	w.printf("%-24s %-4s %-22s %-12s %6d", source.Name, state,
		orDash(strings.Join(requirements, ", ")), orDash(source.LastDate), source.LastCount)
	if source.Quiet {
		w.printf("  quiet: %s", source.QuietReason)
	}
	w.line("")
	if missing && source.Enabled && source.Install != "" {
		w.printf("%26s install: %s\n", "", source.Install)
	}
}

func writeSyncText(w *textWriter, report *services.SyncReport) {
	if report.Preview != nil {
		preview := report.Preview
		when := preview.Date
		if when == "" {
			when = "snapshot"
		}
		w.printf("preview %s (%s, %s): %d record(s)\n", preview.Source, preview.Kind, when, preview.Count)
		for _, record := range preview.Sample {
			w.printf("  %s\n", record.URI)
			if record.Title != "" {
				w.printf("      %s\n", inline(record.Title))
			}
		}
		w.printf("\nvalidated in %s; nothing written\n", report.Elapsed)
		return
	}
	for _, unit := range report.Units {
		label := unit.Source
		if unit.Date != "" {
			label = unit.Date + " " + unit.Source
		}
		w.printf("%-36s %-16s", label, unit.Outcome)
		switch {
		case unit.Outcome == services.OutcomeWritten:
			w.printf(" %5d record(s)  %s", unit.Count, unit.Elapsed)
		case unit.Outcome == services.OutcomePlanned:
			w.printf(" %s", unit.Command)
		case unit.Error != "":
			w.printf(" %s", unit.Error)
		}
		w.line("")
	}
	w.printf("\n%d written, %d skipped, %d failed, %d record(s) in %s\n",
		report.Written, report.Skipped, report.Failed, report.Records, report.Elapsed)
	if report.Graph != nil {
		w.printf("graph: %d edge(s) (%s)\n", report.Graph.Edges, report.Graph.Mode)
	}
}

func writeInitText(w *textWriter, report *services.InitReport) {
	verb := "refreshed"
	if report.Created {
		verb = "created"
	}
	w.printf("%s %s\n", verb, report.Base)
	for _, step := range report.Steps {
		marker := " "
		if step.Changed {
			marker = "+"
		}
		w.printf(" %s %-18s %s\n", marker, step.Item, step.Detail)
	}
	if len(report.Next) > 0 {
		w.line("\nnext")
		for index, step := range report.Next {
			w.printf("  %d. %s\n", index+1, step)
		}
	}
}

func writeTrustText(w *textWriter, report *services.TrustReport) {
	// A re-trust leads with what moved. The full listing is the right answer the first time a
	// base is trusted, and the wrong one every time after: on a base with 46 sources and 15
	// scripts it is a 322-line read over 1,396 lines of shell, so the review that the gate
	// exists to force is the review nobody performs. `--all` is how you ask for it anyway.
	if !report.All && summarizesEverythingThatChanged(report.State.Changes) {
		writeTrustChangesText(w, report)
		return
	}
	w.printf("collection policy\n")
	for _, layer := range core.Layers {
		w.printf("  layer: %s=%t\n", layer, report.Policy.Layers[layer])
	}
	w.printf("  sync:  %d day(s), index stale after %dh, timeout %s, concurrency %d\n\n",
		report.Policy.Days, report.Policy.IndexMaxAgeHours, report.Policy.Timeout, report.Policy.Concurrency)
	w.printf("execution\n  direct argv, cwd %s, %s\n\n",
		report.Policy.WorkingDirectory, report.Policy.Environment)
	// Extra PATH directories apply to every command below without appearing in the run lines.
	if len(report.Bin) > 0 {
		w.printf("applies to every command below\n")
		for _, directory := range report.Bin {
			w.printf("  bin:  %s (on PATH)\n", directory)
		}
		w.line("")
	}
	if len(report.Commands) == 0 {
		w.printf("%s enables no source, so nothing would run.\n", report.Base)
	}
	for _, source := range report.Commands {
		w.printf("%s\n", source.Name)
		writeTrustedSourceText(w, source)
	}
	// The scripts are part of what is being approved: bin/ is prepended to PATH, so a bin/git
	// here is what `run: git log …` above would actually execute.
	if len(report.Scripts) > 0 {
		w.printf("\nbin/ (first on PATH for every command above)\n")
		for _, script := range report.Scripts {
			switch {
			case script.Kind == "symlink":
				w.printf("  %-24s symlink -> %s\n", script.Name, script.Target)
			case !script.Executable:
				w.printf("  %-24s %s (not executable)\n", script.Name, short(script.Digest))
			default:
				w.printf("  %-24s %s\n", script.Name, short(script.Digest))
			}
		}
	}
	if report.Recorded {
		w.printf("\ntrusted %s (digest %s)\n", report.Base, short(report.State.Digest))
		return
	}
	state := "NOT trusted"
	if report.State.Trusted {
		state = "trusted since " + report.State.Since
	}
	w.printf("\n%s: %s\n", report.Base, state)
}

func writeHelpersText(w *textWriter, report *services.HelperReport) {
	for _, helper := range report.Helpers {
		detail := string(helper.State)
		if helper.Required {
			detail += ", required"
		}
		if helper.Refreshed {
			detail += ", refreshed"
		}
		w.printf("%-28s %s\n", helper.Path, detail)
		if helper.State != services.HelperCurrent {
			w.printf("  current: %s\n  shipped: %s\n", orDash(short(helper.CurrentSHA256)), short(helper.ShippedSHA256))
		}
	}
	w.printf("\n%d current, %d drifted, %d missing, %d refreshed\n",
		report.Current, report.Drifted, report.Missing, report.Refreshed)
}

func writeBuildText(w *textWriter, report *services.BuildReport) {
	if report.Graph != nil {
		writeGraphBuildText(w, report.Graph)
	}
	if report.Wiki != nil {
		writeWikiIndexText(w, report.Wiki)
	}
}

func writeGraphBuildText(w *textWriter, typed *services.GraphBuild) {
	w.printf("%s  %d edges from %d document(s) and %d page(s) in %s (%s)\n",
		typed.URI, typed.Edges, typed.Documents, typed.Pages, typed.Elapsed, typed.Mode)
}

func writeNewText(w *textWriter, result *services.NewResult) {
	if result.URI == "" {
		w.line(result.Message)
	} else {
		w.printf("%s (%s)\n", result.Message, result.URI)
	}
	if len(result.Run) > 0 {
		w.printf("run: [%s]\n", yamlFlowStrings(result.Run))
	}
	if len(result.Requires) > 0 {
		w.printf("requires: [%s]\n", yamlFlowStrings(result.Requires))
	}
}

func yamlFlowStrings(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		// JSON string literals are valid YAML and keep placeholders from becoming flow mappings.
		quoted[index] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}

func writeValidationText(w *textWriter, report *services.ValidationReport) {
	for _, issue := range report.Issues {
		location := issue.URI
		if issue.Line > 0 {
			location = fmt.Sprintf("%s:%d", issue.URI, issue.Line)
		}
		w.printf("%-7s %s  %s\n", issue.Severity, location, issue.Message)
	}
	w.printf("\n%d page(s): %d error(s), %d warning(s)\n", report.Pages, report.Errors, report.Warnings)
}

func writeReadText(w *textWriter, result *services.ReadResult) {
	w.printf("%s  [%s]\n\n", result.URI, result.Kind)
	switch {
	case result.Text != "":
		w.line(result.Text)
	case len(result.Entries) > 0:
		for _, entry := range result.Entries {
			w.line(entry)
		}
	case result.Selection != nil:
		w.line(string(result.Selection))
	case result.Entity != nil:
		writeEntityText(w, result.Entity)
	case result.Record != nil:
		writeRecordText(w, result.Record)
	case result.Document != nil:
		writeDocumentText(w, result.Document)
	}
	if result.Body != "" {
		w.printf("\n--- body (%s, not stored) ---\n%s\n", result.BodyState, result.Body)
	}
}

func writeEntityText(w *textWriter, entity *services.EntityView) {
	for _, edge := range entity.Neighbours {
		w.printf("%-10s %s -> %s\n", edge.Kind, edge.Src, edge.Dst)
	}
	w.printf("\n%d edge(s)", len(entity.Neighbours))
	if entity.NeighbourCap {
		w.printf(" (truncated; raise --limit)")
	}
	w.line("")
}

func writePageText(w *textWriter, page services.Page) {
	w.printf("%s  %s  [%s]\n", page.URI, inline(page.Title), orDash(page.Type))
	if len(page.Tags) > 0 {
		w.printf("tags: %s\n", strings.Join(page.Tags, ", "))
	}
	w.printf("\n%s\n", page.Body)
}

func writeDocumentText(w *textWriter, document *sources.Document) {
	w.printf("%s  %s  %d record(s)  collected %s\n",
		document.URI(), document.Source, document.Count, document.CollectedAt)
	w.line("")
	for _, record := range document.Records {
		uri, _ := document.RecordURI(record)
		title, _ := document.Fields.EvalString(core.FieldTitle, map[string]any(record))
		w.printf("%s\n", uri)
		if title != "" {
			w.printf("    %s\n", inline(title))
		}
	}
}

// inline flattens collected or authored free text onto the one line this renderer allocated
// for it. A record's title is unmodified provider data — a mail subject, a PR title, a page
// title — so a newline in it would otherwise emit lines indistinguishable from fkf's own, and
// `fkf context --format text` is exactly the string bin/fkf-hook feeds an agent unattended at
// every session start. Escaping belongs here rather than in services/: JSON already escapes,
// and rendering must not mutate the stored record.
func inline(value string) string {
	if !strings.ContainsFunc(value, isBreaking) {
		return value
	}
	return strings.Map(func(r rune) rune {
		if isBreaking(r) {
			return ' '
		}
		return r
	}, value)
}

func isBreaking(r rune) bool {
	// Tab is included because the text renderer aligns with spaces and a tab shifts a column;
	// the other terminal-active runes because they can act on or visually reorder output.
	return r == '\n' || r == '\t' || terminalActive(r)
}

// block preserves the line and tab layout of Markdown and fetched bodies while neutralising
// terminal controls and invisible directionality characters. The stored bytes remain verbatim;
// this is only a safety boundary for the human text renderer.
func block(value string) string {
	if !strings.ContainsFunc(value, terminalActive) {
		return value
	}
	return strings.Map(func(r rune) rune {
		if terminalActive(r) {
			return ' '
		}
		return r
	}, value)
}

func terminalActive(r rune) bool {
	if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	_, _, invisible := services.FindInvisible(string(r))
	return invisible
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func writeWikiIndexText(w *textWriter, report *services.WikiIndexReport) {
	state := "unchanged"
	switch {
	case report.Stale:
		state = "STALE — run `fkf build wiki`"
	case report.Created:
		state = "created"
	case report.Changed:
		state = "rewritten"
	}
	w.printf("%s  %s\n%d page(s) across %d type(s), %d tag(s)\n",
		report.URI, state, report.Pages, report.Types, report.Tags)
}

// writeRecordText renders one stored record. Without it `fkf read <uri>#<id>` printed its own
// URI and nothing else at a terminal, while --format json returned the whole record: text is
// the default at a terminal, so the one command that opens exactly one thing showed nothing.
//
// Keys are sorted because a record is a map and a reader comparing two reads must not see the
// same record twice in two orders. Values go through the same `inline` escape every other
// collected string does — a record is untrusted provider data, and a newline in it would
// otherwise emit lines indistinguishable from fkf's own.
func writeRecordText(w *textWriter, record sources.Record) {
	keys := slices.Sorted(maps.Keys(record))
	width := 0
	for _, key := range keys {
		if scalarText(record[key]) != "" {
			width = max(width, len(key))
		}
	}
	for _, key := range keys {
		if scalar := scalarText(record[key]); scalar != "" {
			w.printf("%-*s  %s\n", width, key, inline(scalar))
			continue
		}
		// A nested object or array on one line is a four-kilobyte wall — a GitHub notification
		// carries a whole repository object. Indenting it puts the structure back and marks it
		// as fkf's own rendering rather than provider text; json.Marshal has already escaped
		// every control character inside the strings, so the indentation is the only newline.
		w.printf("%s\n", key)
		for _, line := range strings.Split(indentedText(record[key]), "\n") {
			w.printf("  %s\n", line)
		}
	}
}

// scalarText renders a record value that fits on one line, or "" when it does not. Numbers go
// through strconv rather than %v so a large integer is not rendered in scientific notation.
func scalarText(value any) string {
	switch typed := value.(type) {
	case nil:
		return "-"
	case string:
		if typed == "" {
			return "-"
		}
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	}
	return ""
}

// indentedText re-encodes a nested value as indented JSON, so what the reader sees is the same
// structure `--format json` and `?jq=` would give them.
func indentedText(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// summarizesEverythingThatChanged decides whether the diff may stand in for the listing. It
// may only when every change is a source or a script, because those are the items whose whole
// reviewable text the diff prints back.
//
// A configuration file that moved with no source or script moving with it is the case that
// must NOT be summarised: `bin:`, `layers:` and every disabled source live in that file and in
// no item, so "modified fkf.yaml" would be the entire review. A base-level PATH-directory
// change is exactly that shape — it can replace every command without appearing in any run line.
func summarizesEverythingThatChanged(changes []core.TrustChange) bool {
	if len(changes) == 0 {
		return false
	}
	for _, change := range changes {
		switch change.Item {
		case core.TrustItemSource, core.TrustItemScript:
			// The compact renderer prints the complete current disclosure for these items.
		default:
			return false
		}
	}
	return true
}

// writeTrustChangesText is the re-trust review: what differs from what was approved, printed
// with the text that differs. Anything that applies to every command is printed too, however
// small the diff — a reviewer cannot reconstruct it from a filename.
func writeTrustChangesText(w *textWriter, report *services.TrustReport) {
	w.printf("%s changed since it was trusted on %s\n\n", report.Base, report.State.Since)
	if len(report.Bin) > 0 {
		w.printf("applies to every command below\n")
		for _, directory := range report.Bin {
			w.printf("  bin:  %s (on PATH)\n", directory)
		}
		w.line("")
	}
	sources := map[string]services.TrustedSource{}
	for _, source := range report.Commands {
		sources[source.Name] = source
	}
	scripts := map[string]core.BinScript{}
	for _, script := range report.Scripts {
		scripts[script.Name] = script
	}
	for _, change := range report.State.Changes {
		if change.Item == core.TrustItemConfig {
			continue // carried by the source and script lines below it
		}
		w.printf("%s %s %s%s\n", change.Kind, change.Item, change.Name, trustChangeNote(change))
		if source, ok := sources[change.Name]; ok && change.Item == core.TrustItemSource {
			writeTrustedSourceText(w, source)
		}
		if script, ok := scripts[change.Name]; ok && change.Item == core.TrustItemScript {
			w.printf("  bin/%s  %s\n", script.Name, short(script.Digest))
		}
	}
	if report.Recorded {
		w.printf("\ntrusted %s (digest %s)\n", report.Base, short(report.State.Digest))
		return
	}
	w.printf("\n%d change(s). `fkf trust --all` prints everything; `fkf trust` records.\n",
		len(report.State.Changes))
}

// trustChangeNote spells out the one change whose consequence is not in its own name. A script
// whose contents did not move but which gained the executable bit is a one-line mode diff in a
// pull, and it is exactly what decides whether PATH lookup runs it.
func trustChangeNote(change core.TrustChange) string {
	switch change.Kind {
	case core.TrustArmed:
		return "  (unchanged contents, now executable — this is what PATH picks up)"
	case core.TrustDisarmed:
		return "  (unchanged contents, no longer executable)"
	default:
		return ""
	}
}

// writeTrustedSourceText prints everything about one source that a reviewer is approving. The
// full listing and the change diff both call it, so the two can never show different things
// about the same source.
func writeTrustedSourceText(w *textWriter, source services.TrustedSource) {
	w.printf("  enabled: %t\n", source.Enabled)
	w.printf("  layer:   %s\n", source.Layer)
	if len(source.Run) > 0 {
		arguments := make([]string, 0, len(source.Run))
		for _, argument := range source.Run {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  run:  [%s]\n", strings.Join(arguments, ", "))
	}
	if len(source.Body) > 0 {
		arguments := make([]string, 0, len(source.Body))
		for _, argument := range source.Body {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  body: [%s]\n", strings.Join(arguments, ", "))
	}
	for _, name := range source.BodyFields.Names() {
		w.printf("  body field %s: %s\n", name, fieldPathsText(source.BodyFields.Paths(name)))
	}
	if source.Policy != "" {
		w.printf("  how:  %s\n", source.Policy)
	}
}

func fieldPathsText(paths core.FieldPaths) string {
	values := make([]string, 0, len(paths))
	for _, fieldPath := range paths {
		values = append(values, fieldPath.String())
	}
	if len(values) == 1 {
		return values[0]
	}
	return "[" + strings.Join(values, ", ") + "]"
}
