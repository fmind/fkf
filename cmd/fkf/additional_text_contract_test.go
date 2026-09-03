package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestTextRenderersCoverSearchPagesHelpersAndOperationalFindings(t *testing.T) {
	tests := []struct {
		name   string
		result any
		want   []string
	}{
		{
			name: "search",
			result: &services.SearchResult{Layer: core.LayerWiki, Hits: []services.SearchHit{
				{URI: "wiki/a.md", Score: 90, Matched: []string{"alpha", "beta"}, Excerpt: "matching excerpt"},
				{URI: "wiki/b.md", Score: 40, Matched: []string{"alpha"}},
			}},
			want: []string{"90  wiki/a.md  [alpha beta]", "matching excerpt", "2 hit(s)"},
		},
		{
			name: "page",
			result: services.Page{
				URI: "wiki/page.md", Title: "Page title", Type: "decision", Tags: []string{"one", "two"}, Body: "# Page title\n\nBody.",
			},
			want: []string{"wiki/page.md  Page title  [decision]", "tags: one, two", "# Page title"},
		},
		{
			name: "helpers",
			result: &services.HelperReport{Helpers: []services.HelperStatus{
				{Path: "bin/current.sh", State: services.HelperCurrent, Required: true, ShippedSHA256: strings.Repeat("a", 64)},
				{Path: "bin/drifted.sh", State: services.HelperDrifted, CurrentSHA256: strings.Repeat("b", 64), ShippedSHA256: strings.Repeat("c", 64), Refreshed: true},
				{Path: "bin/missing.sh", State: services.HelperMissing, ShippedSHA256: strings.Repeat("d", 64)},
			}, Current: 1, Drifted: 1, Missing: 1, Refreshed: 1},
			want: []string{"bin/current.sh", "current, required", "drifted, refreshed", "current: bbbbbbbbbbbb", "shipped: cccccccccccc", "1 current, 1 drifted, 1 missing, 1 refreshed"},
		},
		{
			name: "index",
			result: &services.IndexListing{Entries: []services.IndexEntry{
				{URI: "index/fresh.json", Count: 2, AgeHours: 1, Bytes: 120},
				{URI: "index/stale.json", Count: 3, AgeHours: 200, Bytes: 240, Stale: true},
			}},
			want: []string{"index/fresh.json", "index/stale.json", "stale", "2 document(s)"},
		},
		{
			name: "source tests",
			result: &services.SourceTestReport{Sources: []services.SourceTestResult{
				{Source: "good", Outcome: services.SourceTestPassed, Elapsed: "1ms"},
				{Source: "bad", Outcome: services.SourceTestFailed, Elapsed: "2ms", Error: "command exited with status 7"},
			}, Passed: 1, Failed: 1, Elapsed: "3ms"},
			want: []string{"good", "passed", "bad", "command exited with status 7", "1 passed, 1 failed in 3ms"},
		},
		{
			name: "validation",
			result: &services.ValidationReport{Pages: 2, Errors: 1, Warnings: 1, Issues: []services.Issue{
				{URI: "wiki/a.md", Line: 4, Severity: services.SeverityError, Message: "broken"},
				{URI: "wiki/b.md", Severity: services.SeverityWarning, Message: "incomplete"},
			}},
			want: []string{"wiki/a.md:4", "wiki/b.md", "2 page(s): 1 error(s), 1 warning(s)"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err, rendered := writeText(&output, test.result)
			if err != nil || !rendered {
				t.Fatalf("writeText(%T) rendered=%v error=%v", test.result, rendered, err)
			}
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Errorf("output omits %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func TestFindAndRecordTextRenderUsefulSummariesAndNestedJSON(t *testing.T) {
	find := &services.FindResult{Pages: []services.SearchHit{
		{URI: "wiki/titled.md", Layer: core.LayerWiki, Title: "Titled page", Excerpt: "ignored excerpt"},
		{URI: "projects/untitled.md", Layer: core.LayerProjects, Excerpt: "Fallback excerpt"},
	}}
	var output bytes.Buffer
	writeFindText(&textWriter{out: &output}, find)
	for _, want := range []string{"Titled page", "Fallback excerpt", "2 page(s)"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("find output omits %q:\n%s", want, output.String())
		}
	}

	record := sources.Record{
		"nil": nil, "empty": "", "bool": true, "float": 12.5,
		"number": json.Number("9007199254740993"),
		"nested": map[string]any{"items": []any{"one", float64(2)}},
	}
	output.Reset()
	writeRecordText(&textWriter{out: &output}, record)
	for _, want := range []string{
		"nil", "-", "empty", "bool", "true", "12.5", "9007199254740993", "nested", `"items": [`, `"one"`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("record output omits %q:\n%s", want, output.String())
		}
	}
	if err, rendered := writeText(&bytes.Buffer{}, struct{}{}); err != nil || rendered {
		t.Fatalf("unknown text type rendered=%v error=%v", rendered, err)
	}
}
