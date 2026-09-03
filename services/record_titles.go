package services

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

// RecordTitleIssue identifies a source whose subject line does not distinguish most of its
// records. It is advisory because historical evidence is permanent and may predate titles.
type RecordTitleIssue struct {
	Source   string   `json:"source"`
	Title    string   `json:"title"`
	Count    int      `json:"count"`
	Records  int      `json:"records"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// RecordTitleReport is the collected-record half of `fkf validate`.
type RecordTitleReport struct {
	Sources   int                `json:"sources"`
	Documents int                `json:"documents"`
	Records   int                `json:"records"`
	Strict    bool               `json:"strict"`
	Errors    int                `json:"errors"`
	Warnings  int                `json:"warnings"`
	Issues    []RecordTitleIssue `json:"issues"`
	OK        bool               `json:"ok"`
}

type sourceTitleCounts struct {
	documents int
	records   int
	titles    map[string]int
	display   map[string]string
}

// ValidateRecordTitles warns when one title occurs on more than half of a source's records.
// It reads only stored documents and tolerates historical envelopes without a title projection.
func ValidateRecordTitles(ctx context.Context, base *Base, strict bool) (*RecordTitleReport, error) {
	bySource, err := collectTitleCounts(ctx, base)
	if err != nil {
		return nil, err
	}
	report := summarizeTitleCounts(bySource, strict)
	return report, nil
}

func collectTitleCounts(ctx context.Context, base *Base) (map[string]*sourceTitleCounts, error) {
	uris, err := documentURIs(ctx, base)
	if err != nil {
		return nil, err
	}
	bySource := make(map[string]*sourceTitleCounts)
	for _, uri := range uris {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		document, err := base.ReadDocumentContext(ctx, uri)
		if err != nil {
			return nil, err
		}
		counts := bySource[document.Source]
		if counts == nil {
			counts = &sourceTitleCounts{titles: map[string]int{}, display: map[string]string{}}
			bySource[document.Source] = counts
		}
		counts.documents++
		current := base.Config.Sources[document.Source]
		if current == nil || !reflect.DeepEqual(
			document.Fields.Paths(core.FieldTitle), current.Fields.Paths(core.FieldTitle),
		) {
			// Evidence envelopes are permanent. A source may adopt a better title projection
			// without forcing historical recollection, so only its current projection is linted.
			continue
		}
		counts.records += len(document.Records)
		for _, record := range document.Records {
			title, ok := document.Fields.EvalString(core.FieldTitle, map[string]any(record))
			normalized := strings.Join(strings.Fields(title), " ")
			if !ok || normalized == "" {
				continue
			}
			key := strings.ToLower(normalized)
			counts.titles[key]++
			if _, exists := counts.display[key]; !exists {
				counts.display[key] = normalized
			}
		}
	}
	return bySource, nil
}

func summarizeTitleCounts(bySource map[string]*sourceTitleCounts, strict bool) *RecordTitleReport {
	report := &RecordTitleReport{Strict: strict, Issues: []RecordTitleIssue{}, OK: true}
	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		counts := bySource[source]
		report.Sources++
		report.Documents += counts.documents
		report.Records += counts.records
		if counts.records < 2 {
			continue
		}
		keys := make([]string, 0, len(counts.titles))
		for key := range counts.titles {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendTitleIssue(report, source, key, counts, strict)
		}
	}
	if strict {
		report.Errors = len(report.Issues)
		report.OK = report.Errors == 0
	} else {
		report.Warnings = len(report.Issues)
	}
	return report
}

func appendTitleIssue(
	report *RecordTitleReport, source, key string, counts *sourceTitleCounts, strict bool,
) {
	count := counts.titles[key]
	if count*2 <= counts.records {
		return
	}
	severity := SeverityWarning
	if strict {
		severity = SeverityError
	}
	report.Issues = append(report.Issues, RecordTitleIssue{
		Source: source, Title: counts.display[key], Count: count, Records: counts.records,
		Severity: severity,
		Message:  fmt.Sprintf("title is shared by %d of %d records; derive a meaningful subject line", count, counts.records),
	})
}
