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
	case *services.SourceTestReport:
		writeSourceTestText(w, typed)
	case *services.HelperReport:
		writeHelpersText(w, typed)
	case *validateReport:
		writeValidateReportText(w, typed)
	case *services.ValidationReport:
		writeValidationText(w, typed)
	case *services.RecordTitleReport:
		writeRecordTitleValidationText(w, typed)
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

// writeValidateReportText names each check before delegating to the renderer that already
// knows how to print it. Bare `fkf validate` prints three or four blocks whose summary lines
// are near-identical, so without the heading a reader cannot tell which layer failed — the
// JSON has carried the layer all along. The heading is the same section form `fkf status`
// uses, and its leading blank line is what separates one block from the next.
func writeValidateReportText(w *textWriter, report *validateReport) {
	if report.Wiki != nil {
		w.printf("\n%s\n", report.Wiki.Layer)
		writeValidationText(w, report.Wiki)
	}
	if report.Projects != nil {
		w.printf("\n%s\n", report.Projects.Layer)
		writeValidationText(w, report.Projects)
	}
	if report.Records != nil {
		// RecordTitleReport has no layer of its own: it summarises every collected source.
		w.printf("\nrecords\n")
		writeRecordTitleValidationText(w, report.Records)
	}
	if report.Lint != nil {
		w.printf("\n%s\n", report.Lint.Layer)
		writeValidationText(w, report.Lint)
	}
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
		w.printf("\n--- body (%s) ---\n%s\n", result.BodyState, result.Body)
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
// Text output may be fed to an agent unattended by a session hook. The canonical context
// renderer mirrors this boundary in services because exact selection must measure those bytes;
// rendering never mutates the stored record.
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
