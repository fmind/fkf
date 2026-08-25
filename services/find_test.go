package services_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func queryBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	collect(t, base, "2026-05-05", dayTwo)
	return base
}

func TestQueryProjectsEveryRecordWithItsURI(t *testing.T) {
	base := queryBase(t)
	result, err := services.Find(t.Context(), base, services.FindFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 3 || len(result.Records) != 3 {
		t.Fatalf("result = %+v, want the three records", result)
	}
	for _, record := range result.Records {
		if record.URI == "" || record.Source != "synthetic" || record.Date == "" {
			t.Fatalf("record = %+v, want it stamped with uri, source, and date", record)
		}
		if record.Record == nil {
			t.Fatal("the whole record travels, so `--format jsonl | duckdb` loses nothing")
		}
	}
}

func TestFindBoundedContinuesOneLargeDocumentWithConstantPageMemory(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	records := make([]map[string]any, 251)
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	for index := range records {
		records[index] = map[string]any{
			"id":      fmt.Sprintf("item-%03d", index),
			"t":       start.Add(time.Duration(index) * time.Second).Format(time.RFC3339),
			"subject": "needle",
		}
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, base, "2026-05-04", string(encoded))

	const limit = 17
	var after services.FindPosition
	var snapshot string
	seen := map[string]bool{}
	for calls := 0; ; calls++ {
		if calls > 20 {
			t.Fatal("bounded find did not terminate")
		}
		page, err := services.FindBounded(t.Context(), base, services.FindFilter{
			Grep: []string{"needle"},
		}, false, limit, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Result.Pages)+len(page.Result.Records) > limit {
			t.Fatalf("page retained %d primary items, want at most %d",
				len(page.Result.Pages)+len(page.Result.Records), limit)
		}
		if page.Result.Scanned != len(records) || page.Result.Matched != len(records) {
			t.Fatalf("page counters = %d/%d, want exhaustive %d/%d",
				page.Result.Matched, page.Result.Scanned, len(records), len(records))
		}
		if len(page.Result.Days) != 0 {
			t.Fatalf("bounded result exposed %d selected-day metadata rows", len(page.Result.Days))
		}
		if snapshot == "" {
			snapshot = page.SnapshotSHA256
		} else if page.SnapshotSHA256 != snapshot {
			t.Fatalf("snapshot changed across an unchanged continuation: %s != %s",
				page.SnapshotSHA256, snapshot)
		}
		for _, record := range page.Result.Records {
			if seen[record.URI] {
				t.Fatalf("record %s repeated across keyset pages", record.URI)
			}
			seen[record.URI] = true
		}
		if page.Next == nil {
			break
		}
		after = *page.Next
	}
	if len(seen) != len(records) {
		t.Fatalf("bounded continuation returned %d unique records, want %d", len(seen), len(records))
	}
}

// TestQueryGrepMatchesValuesNotKeys is the defect this design fixed: `--grep author` used to
// return every record from every source that happened to have an author field, which is the
// opposite of what the flag reads as.
func TestQueryGrepMatchesValuesNotKeys(t *testing.T) {
	base := queryBase(t)
	byKey, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"subject"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if byKey.Matched != 0 {
		t.Fatalf("--grep on a key name matched %d record(s); it must match values only", byKey.Matched)
	}
	byValue, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"retrieval"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if byValue.Matched != 1 {
		t.Fatalf("--grep on a value matched %d record(s), want 1", byValue.Matched)
	}
}

func TestQueryGrepMatchesOnlyScalarLeaves(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[{
	  "id":"scalar-leaves",
	  "t":"2026-05-04T09:00:00Z",
	  "subject":"plain text",
	  "count":42.5,
	  "active":true,
	  "nested":{"label":"inside"},
	  "items":[false,7],
	  "nothing":null
	}]`)

	for _, term := range []string{"plain", "42.5", "true", "inside", "false", "7"} {
		result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{term}}, false)
		if err != nil {
			t.Fatalf("Find(grep=%q) error = %v", term, err)
		}
		if result.Matched != 1 {
			t.Errorf("--grep %q matched %d record(s), want the scalar leaf", term, result.Matched)
		}
	}

	for _, term := range []string{"count", "active", "nested", "label", "items", "nothing", `{"label"`, "[false", "null"} {
		result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{term}}, false)
		if err != nil {
			t.Fatalf("Find(grep=%q) error = %v", term, err)
		}
		if result.Matched != 0 {
			t.Errorf("--grep %q matched %d record(s), want keys, compounds, and null ignored", term, result.Matched)
		}
	}
}

func TestFindRejectsBlankGrepInsteadOfExpandingToTheWholeBase(t *testing.T) {
	base := queryBase(t)
	for _, grep := range [][]string{{""}, {" \t "}} {
		if _, err := services.Find(t.Context(), base, services.FindFilter{Grep: grep}, false); err == nil ||
			!errors.Is(err, core.ErrConfig) {
			t.Fatalf("Find(grep=%q) error = %v, want a configuration refusal", grep, err)
		}
	}
}

func TestQueryFilters(t *testing.T) {
	base := queryBase(t)
	cases := []struct {
		name   string
		filter services.FindFilter
		want   int
	}{
		{"by arbitrary relation value", services.FindFilter{Grep: []string{"repo:github.com/fmind/fkf"}}, 2},
		{"by title value", services.FindFilter{Grep: []string{"FK-412"}}, 2},
		{"by arbitrary author value", services.FindFilter{Grep: []string{"person:email/marc@example.test"}}, 1},
		{"by window", services.FindFilter{Window: services.Window{Since: "2026-05-05"}}, 1},
		{"by two terms, all of which must match", services.FindFilter{Grep: []string{"retrieval", "boundary"}}, 1},
		{"by two terms where one misses", services.FindFilter{Grep: []string{"retrieval", "absent"}}, 0},
		{"by source", services.FindFilter{Sources: []string{"synthetic"}}, 3},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := services.Find(t.Context(), base, test.filter, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Matched != test.want {
				t.Fatalf("matched = %d, want %d", result.Matched, test.want)
			}
		})
	}
}

func TestQueryWhereUsesTheSameJQSubset(t *testing.T) {
	base := queryBase(t)
	clause, err := services.ParseWhere(".repo_uri=repo:github.com/acme/ledger")
	if err != nil {
		t.Fatal(err)
	}
	result, err := services.Find(t.Context(), base, services.FindFilter{Where: []services.WhereClause{clause}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 1 {
		t.Fatalf("matched = %d, want the one record in that repository", result.Matched)
	}
	if _, err := services.ParseWhere("no-equals-sign"); err == nil {
		t.Fatal("ParseWhere() must refuse an argument with no `=`")
	}
	if _, err := services.ParseWhere("repo=x"); err == nil {
		t.Fatal("ParseWhere() must refuse a path outside the jq subset")
	}
}

func TestQueryCountReportsVolumes(t *testing.T) {
	base := queryBase(t)
	result, err := services.Find(t.Context(), base, services.FindFilter{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Volumes) != 2 || len(result.Records) != 0 {
		t.Fatalf("result = %+v, want volumes and no records", result)
	}
	total := 0
	for _, day := range result.Volumes {
		total += day.Total
	}
	if total != 3 {
		t.Fatalf("counted %d record(s), want 3", total)
	}
}

// TestCountLimitBoundsOnlyTheReturnedVolumes pins the distinction between an output bound
// and a scan bound. MCP uses a positive limit to keep one response small, but Scanned and
// Matched must still describe the complete requested window. A zero limit remains exhaustive
// for the CLI's default `find --count` path.
func TestCountLimitBoundsOnlyTheReturnedVolumes(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	const days = 105
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for offset := range days {
		date := first.AddDate(0, 0, offset).Format(time.DateOnly)
		collect(t, base, date, fmt.Sprintf(
			`[{"id":"r-%d","t":"%sT09:00:00Z","subject":"bounded count"}]`,
			offset, date))
	}
	writeIndexDocument(t, base, "catalog", []sources.Record{{
		"id": "current", "title": "bounded count",
	}})
	wantRecords := days + 1

	bounded, err := services.Find(t.Context(), base, services.FindFilter{
		Grep: []string{"bounded count"}, Limit: 100,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Volumes) != 100 || !bounded.Truncated {
		t.Fatalf("volumes = %d, truncated = %t; want 100 and true",
			len(bounded.Volumes), bounded.Truncated)
	}
	if bounded.Scanned != wantRecords || bounded.Matched != wantRecords {
		t.Fatalf("scanned = %d, matched = %d; want all %d event and index records",
			bounded.Scanned, bounded.Matched, wantRecords)
	}
	if len(bounded.Days) != days {
		t.Fatalf("days metadata = %d, want all %d selected days", len(bounded.Days), days)
	}

	exhaustive, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"bounded count"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhaustive.Volumes) != wantRecords || exhaustive.Truncated {
		t.Fatalf("default count volumes = %d, truncated = %t; want %d and false",
			len(exhaustive.Volumes), exhaustive.Truncated, wantRecords)
	}
}

// TestQueryIsWindowFirst is the property that keeps a read cheap on years of history — and it
// is checked the only way that proves it: a document outside the window is corrupt, so opening
// it would fail.
func TestQueryNeverOpensADocumentOutsideTheWindow(t *testing.T) {
	base := queryBase(t)
	corrupt, err := base.Store.Resolve("events/2026-01-01/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(corrupt), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("this is not JSON at all"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Find(t.Context(), base, services.FindFilter{Window: services.Window{Since: "2026-05-01"}}, false); err != nil {
		t.Fatalf("a corrupt document outside the window must never be opened: %v", err)
	}
	if _, err := services.Find(t.Context(), base, services.FindFilter{Window: services.Window{Since: "2026-01-01"}}, false); err == nil {
		t.Fatal("a corrupt document INSIDE the window must be reported, not skipped")
	}
}

func TestQueryLimitTruncatesVisibly(t *testing.T) {
	base := queryBase(t)
	result, err := services.Find(t.Context(), base, services.FindFilter{Limit: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || !result.Truncated {
		t.Fatalf("result = %+v, want one record and a truncation flag", result)
	}
}

func TestParseWindow(t *testing.T) {
	cases := []struct{ since, until, wantSince, wantUntil string }{
		{"2026-05-01", "2026-05-04", "2026-05-01", "2026-05-04"},
		{"7d", "", "2026-05-03", ""},
		// A day keyword is one absolute date on either bound, which is what makes
		// `--since yesterday --until yesterday` mean one day rather than three.
		{"today", "today", "2026-05-10", "2026-05-10"},
		{"yesterday", "yesterday", "2026-05-09", "2026-05-09"},
		{"2w", "", "2026-04-26", ""},
		{"1m", "", "2026-04-10", ""},
		{"1y", "", "2025-05-10", ""},
		{"", "", "", ""},
	}
	for _, test := range cases {
		window, err := services.ParseWindow(test.since, test.until, testClock)
		if err != nil {
			t.Fatalf("ParseWindow(%q, %q) error = %v", test.since, test.until, err)
		}
		if window.Since != test.wantSince || window.Until != test.wantUntil {
			t.Fatalf("ParseWindow(%q, %q) = %+v, want %s .. %s", test.since, test.until, window, test.wantSince, test.wantUntil)
		}
	}
	for _, invalid := range []string{
		"tomorrow", "last week", "0d", "-3d", "7", "7x",
		"7xd", "7.5d", "7 d", "+7d", "01d", "7dd", "1D",
		"9223372036854775807y",
	} {
		if _, err := services.ParseWindow(invalid, "", testClock); err == nil {
			t.Fatalf("ParseWindow(%q) succeeded, want a usage error naming the accepted forms", invalid)
		}
	}
	if _, err := services.ParseWindow("2026-05-05", "2026-05-01", testClock); err == nil {
		t.Fatal("ParseWindow() must refuse a window that runs backwards")
	}
}

// --- layers ------------------------------------------------------------------------------

func TestListEventsAndReadDocument(t *testing.T) {
	base := queryBase(t)
	listing, err := services.ListEvents(t.Context(), base, services.Window{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Days) != 2 || listing.Days[0].Date != "2026-05-05" {
		t.Fatalf("listing = %+v, want two days, newest first", listing)
	}
	if listing.Total != 3 {
		t.Fatalf("total = %d, want 3", listing.Total)
	}
	document, err := services.ReadEventDocument(base, "2026-05-04", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if document.Count != 2 || document.URI() != "events/2026-05-04/synthetic.json" {
		t.Fatalf("document = %+v", document)
	}
	if _, err := services.ReadEventDocument(base, "not-a-date", "synthetic"); err == nil {
		t.Fatal("ReadEventDocument() must refuse a malformed date")
	}
}

func TestListEventsRefusesAnUnknownSource(t *testing.T) {
	base := queryBase(t)
	_, err := services.ListEvents(t.Context(), base, services.Window{}, "github-prs", 0)
	if !errors.Is(err, core.ErrConfig) {
		t.Fatalf("error = %v, want core.ErrConfig", err)
	}
	if !strings.Contains(err.Error(), `unknown source "github-prs"`) || !strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("error = %v, want it to name the typo and the base's real source", err)
	}
}

func TestListTasks(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "tasks/2026-05-04/first/TASKS.md", "# First trace\n\nRequest.\n")
	write(t, base, "tasks/2026-05-05/handle-second/TASKS.md", "# Second trace\n\nRequest.\n")
	listing, err := services.ListTasks(t.Context(), base, services.Window{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Traces) != 2 || listing.Traces[0].Date != "2026-05-05" {
		t.Fatalf("listing = %+v, want two traces, newest first", listing)
	}
	if listing.Traces[0].Title != "Second trace" {
		t.Fatalf("title = %q, want the trace's first heading", listing.Traces[0].Title)
	}
	page, err := services.ReadTaskTrace(base, "2026-05-04/first")
	if err != nil || page.Title != "First trace" {
		t.Fatalf("ReadTaskTrace() = %+v, %v", page, err)
	}
	if _, err := services.ReadTaskTrace(base, "no-slash"); err == nil {
		t.Fatal("ReadTaskTrace() must name the <date>/<slug> form")
	}
}

func TestListPagesAndTags(t *testing.T) {
	base := graphBase(t)
	write(t, base, "wiki/unrelated.md", "---\ntype: insight\ntitle: Unrelated\ntags: [other]\n---\n\n# Unrelated\n")
	listing, err := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 2 {
		t.Fatalf("listing = %+v, want both concepts", listing)
	}
	// Requiring every tag rather than any of them is the useful default in a flat namespace:
	// two real, declared tags that no single page carries both of still narrows to zero.
	both, err := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{Tags: []string{"decision", "retrieval"}})
	if err != nil {
		t.Fatal(err)
	}
	disjoint, err := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{Tags: []string{"decision", "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if both.Total != 1 || disjoint.Total != 0 {
		t.Fatalf("tag filtering must require every tag: %d then %d", both.Total, disjoint.Total)
	}
	index, err := services.BuildTagVocabulary(t.Context(), base, core.LayerWiki)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Tags) != 3 {
		t.Fatalf("tags = %+v, want the three declared", index.Tags)
	}
}

// TestListPagesRefusesAnUnknownTagOrStatus is the regression test for silent-empty on the two
// page-layer selectors. `--tag absent` and `--status wibble` used to return "0 pages", exit 0,
// indistinguishable from a real base with none — the refusal names the vocabulary instead.
func TestListPagesRefusesAnUnknownTagOrStatus(t *testing.T) {
	base := graphBase(t)
	_, err := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{Tags: []string{"decision", "absent"}})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), `unknown tag "absent"`) {
		t.Fatalf("error = %v, want an unknown-tag refusal", err)
	}
	// Case folds the same way the actual tag match does, so a validly-cased filter is never
	// refused right before the match that would have accepted it.
	if _, err := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{Tags: []string{"Decision"}}); err != nil {
		t.Fatalf("ListPages() error = %v, want a differently-cased real tag accepted", err)
	}
	_, err = services.ListPages(t.Context(), base, core.LayerProjects, services.PageFilter{Status: "wibble"})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), `unknown status "wibble"`) ||
		!strings.Contains(err.Error(), "active") {
		t.Fatalf("error = %v, want an unknown-status refusal naming the real values", err)
	}
}

func TestSearchPagesRanksTitleAboveBody(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/retrieval.md", "---\ntype: decision\ntitle: Retrieval boundary\ntags: [retrieval]\n---\n\n# Retrieval boundary\n\nText.\n")
	write(t, base, "wiki/other.md", "---\ntype: insight\ntitle: Something else\ntags: [misc]\n---\n\n# Something else\n\nMentions retrieval once.\n")
	result, err := services.SearchPages(t.Context(), base, core.LayerWiki, []string{"retrieval"}, services.PageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 || result.Hits[0].Slug != "retrieval" {
		t.Fatalf("hits = %+v, want the title match first", result.Hits)
	}
	if result.Hits[0].Excerpt == "" {
		t.Fatal("a hit carries an excerpt so it can be judged without opening the page")
	}
	if _, err := services.SearchPages(t.Context(), base, core.LayerWiki, nil, services.PageFilter{}); err == nil {
		t.Fatal("SearchPages() needs at least one term")
	}
}

func TestSearchPagesRequiresEveryTerm(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/alpha-only.md", "---\ntype: note\ntitle: Alpha only\ntags: [test]\n---\n\n# Alpha\n")
	write(t, base, "wiki/alpha-beta.md", "---\ntype: note\ntitle: Alpha beta\ntags: [test]\n---\n\n# Alpha beta\n")

	result, err := services.SearchPages(t.Context(), base, core.LayerWiki,
		[]string{"alpha", "beta"}, services.PageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].URI != "wiki/alpha-beta.md" {
		t.Fatalf("multi-term page hits = %+v, want only the page matching every term", result.Hits)
	}
}

func TestSearchPagesBuildsUnicodeSafeExcerptFromOriginalText(t *testing.T) {
	base := queryBase(t)
	body := strings.Repeat("Ⱥ", 200) + " Needle keeps its case."
	write(t, base, "wiki/unicode-excerpt.md", "---\ntype: note\ntitle: Unicode excerpt\ntags: [test]\n---\n\n# Unicode excerpt\n\n"+body+"\n")

	result, err := services.SearchPages(t.Context(), base, core.LayerWiki,
		[]string{"needle"}, services.PageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want the Unicode body match", result.Hits)
	}
	excerpt := result.Hits[0].Excerpt
	if !utf8.ValidString(excerpt) {
		t.Fatalf("excerpt = %q, want valid UTF-8", excerpt)
	}
	if !strings.Contains(excerpt, "Needle keeps its case.") {
		t.Fatalf("excerpt = %q, want the original matched text", excerpt)
	}
}

func TestReadPageBySlugTolerateTheExtension(t *testing.T) {
	base := graphBase(t)
	for _, slug := range []string{"retrieval-boundary", "retrieval-boundary.md"} {
		page, err := services.ReadPageBySlug(base, core.LayerWiki, slug)
		if err != nil || page.Slug != "retrieval-boundary" {
			t.Fatalf("ReadPageBySlug(%q) = %+v, %v", slug, page, err)
		}
	}
	if _, err := services.ReadPageBySlug(base, core.LayerWiki, "  "); err == nil {
		t.Fatal("ReadPageBySlug() must name the slug form")
	}
}

// TestListIndexHoldsOnlyWhatWasCollected pins the split that the `index` rename made
// possible. graph.tsv is computed FROM the base, not collected INTO it, so
// it lives at the root and the index listing has nothing to flag: every entry in it is
// a document some source wrote, and a reader no longer has to know which half is which.
func TestListIndexHoldsOnlyWhatWasCollected(t *testing.T) {
	base := queryBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	listing, err := services.ListIndex(t.Context(), base, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if !strings.HasPrefix(entry.URI, string(core.LayerIndex)+"/") {
			t.Errorf("entry %q is not an index document; graph files belong at the base root", entry.URI)
		}
	}
	for _, generated := range []string{core.GraphFile, core.GraphMetaFile} {
		if !base.Exists(generated) {
			t.Errorf("%s was not written at the base root", generated)
		}
	}
}

// writeIndexDocument gives a test base one point-in-time document, which is the half of a
// real base that `find` and `context` used to be unable to see at all.
func writeIndexDocument(t *testing.T, base *services.Base, name string, records []sources.Record) {
	t.Helper()
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: name, Layer: core.LayerIndex,
		CollectedAt: "2026-05-05T09:00:00Z",
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}, core.FieldTitle: {mustFieldPath(t, ".title")}},
		Count:       len(records), Records: records,
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}

// TestFindReachesTheIndexOnlyWhenTheFilterSelects pins the rule that lets one command
// answer both questions. A bare `fkf find` is "what arrived lately" and must stay the dated
// window, in recency order. The moment a term is given the question becomes "where does this
// appear", and an answer that silently omits the index is wrong — on a real base the
// index holds more records than events does.
func TestFindReachesTheIndexOnlyWhenTheFilterSelects(t *testing.T) {
	base := queryBase(t)
	writeIndexDocument(t, base, "repos", []sources.Record{
		{"id": "fmind/fkf", "title": "a declarative command runner"},
		{"id": "fmind/other", "title": "something else"},
	})
	bare, err := services.Find(t.Context(), base, services.FindFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range bare.Records {
		if strings.HasPrefix(record.URI, string(core.LayerIndex)+"/") {
			t.Fatalf("a bare find returned %s; with no filter the answer is the dated window", record.URI)
		}
	}
	selected, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"declarative"}, Limit: services.NoFindLimit}, false)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, record := range selected.Records {
		if record.URI == "index/repos.json#fmind/fkf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("records = %+v, want the index record the term names", selected.Records)
	}
}

func TestFindSourceOnlyIsExhaustiveAcrossEventsAndIndex(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	for day := 1; day <= services.DefaultFindDays+1; day++ {
		date := fmt.Sprintf("2026-05-%02d", day)
		record := fmt.Sprintf(`[{"id":"event-%02d","t":"%sT09:00:00Z","subject":"source-only"}]`, day, date)
		collect(t, base, date, record)
	}
	writeIndexDocument(t, base, "synthetic", []sources.Record{{"id": "index-match", "title": "source-only"}})

	result, err := services.Find(t.Context(), base, services.FindFilter{Sources: []string{"synthetic"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := services.DefaultFindDays + 2
	if result.Truncated || len(result.Records) != want || result.Matched != want {
		t.Fatalf("source-only result = %d returned, %d matched, truncated=%t; want all %d event and index records",
			len(result.Records), result.Matched, result.Truncated, want)
	}
	foundOldest := false
	for _, record := range result.Records {
		if record.URI == "events/2026-05-01/synthetic.json#event-01" {
			foundOldest = true
		}
	}
	if !foundOldest {
		t.Fatalf("records = %+v, want the event older than the bare seven-day discovery window", result.Records)
	}
	if result.Records[len(result.Records)-1].URI != "index/synthetic.json#index-match" {
		t.Fatalf("last URI = %q, want the point-in-time source document", result.Records[len(result.Records)-1].URI)
	}
}

func TestFindRefusesALayerWithoutAQuestion(t *testing.T) {
	base := queryBase(t)
	for _, layer := range []core.Layer{core.LayerIndex, core.LayerWiki, core.LayerTasks} {
		_, err := services.Find(t.Context(), base, services.FindFilter{Layers: []core.Layer{layer}}, false)
		if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "fkf list "+string(layer)) {
			t.Fatalf("Find(--layer %s) error = %v, want a refusal pointing to the listing command", layer, err)
		}
	}
}

// TestFindTermSearchDefaultsToEveryMatchBeyondTheDiscoveryLimit pins the difference between
// two zero-value requests. A bare find is a bounded recent-record discovery, while a term is
// an exhaustive question over every admitted layer. Applying the discovery cap to both not
// only dropped matching event records after 200; it stopped before scanning index/ at all.
func TestFindTermSearchDefaultsToEveryMatchBeyondTheDiscoveryLimit(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	records := make([]sources.Record, services.DefaultFindLimit+1)
	for index := range records {
		records[index] = sources.Record{
			"id":      fmt.Sprintf("event-%03d", index),
			"t":       "2026-05-04T09:00:00Z",
			"subject": "exhaustive signal",
		}
	}
	payload, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, base, "2026-05-04", string(payload))
	writeIndexDocument(t, base, "catalog", []sources.Record{
		{"id": "index-match", "title": "exhaustive signal from the index"},
	})

	result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"exhaustive signal"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := services.DefaultFindLimit + 2
	if result.Truncated || len(result.Records) != want || result.Matched != want {
		t.Fatalf("result = %d returned, %d matched, truncated=%t; want all %d matches",
			len(result.Records), result.Matched, result.Truncated, want)
	}
	if got := result.Records[len(result.Records)-1].URI; got != "index/catalog.json#index-match" {
		t.Fatalf("last URI = %q, want the index match reached after every event record", got)
	}

	bounded, err := services.Find(t.Context(), base, services.FindFilter{
		Grep: []string{"exhaustive signal"}, Limit: 7,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Records) != 7 || !bounded.Truncated {
		t.Fatalf("explicit --limit equivalent returned %d, truncated=%t; want 7 and true",
			len(bounded.Records), bounded.Truncated)
	}

	discovery, err := services.Find(t.Context(), base, services.FindFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Records) != services.DefaultFindLimit || !discovery.Truncated {
		t.Fatalf("bare discovery returned %d, truncated=%t; want the safe default %d and true",
			len(discovery.Records), discovery.Truncated, services.DefaultFindLimit)
	}
}

// TestFindCountsTheIndexToo keeps `--count` honest about the same corpus `find` searches.
func TestFindCountsTheIndexToo(t *testing.T) {
	base := queryBase(t)
	writeIndexDocument(t, base, "repos", []sources.Record{{"id": "fmind/fkf", "title": "declarative"}})
	result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"declarative"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var counted int
	for _, volume := range result.Volumes {
		for _, entry := range volume.Sources {
			if entry.Source == "repos" {
				counted += entry.Count
			}
		}
	}
	if counted != 1 {
		t.Fatalf("volumes = %+v, want the index document counted", result.Volumes)
	}
}

// TestFindReachesEveryLayer pins the reason `find` exists as one command: a lexical question
// does not know which layer holds its answer, so the answer covers all of them. A term that
// appears in a wiki concept, a project page, a task trace, and a record must return four
// results, not the one that happens to live in the layer the caller guessed.
func TestFindReachesEveryLayer(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\ntags: [decision]\n---\n\n# Retrieval boundary\n\nA claim about the retrieval boundary.\n")
	write(t, base, "projects/rebuild.md",
		"---\ntype: project\ntitle: Rebuild\nstatus: active\n---\n\n# Rebuild\n\nAn effort on the retrieval boundary.\n")
	write(t, base, "tasks/2026-05-04/session/TASKS.md",
		"---\ntitle: Session\n---\n\n# Session\n\n## Learned\n\n- A decision about the retrieval boundary.\n")

	result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"retrieval"}, Limit: services.NoFindLimit}, false)
	if err != nil {
		t.Fatal(err)
	}
	found := map[core.Layer]bool{}
	for _, hit := range result.Pages {
		found[hit.Layer] = true
	}
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects, core.LayerTasks} {
		if !found[layer] {
			t.Fatalf("pages = %+v, want a hit in %s", result.Pages, layer)
		}
	}
	if len(result.Records) == 0 {
		t.Fatal("the record half must still answer; find is one question over the whole base")
	}
	// Scanned and Matched stay record counters: folding pages in would make the "27 of 366"
	// line the text rendering prints mean two different things at once.
	if result.Scanned != 3 {
		t.Fatalf("scanned = %d, want the three collected records only", result.Scanned)
	}
}

// TestFindLayerNarrowsBothHalves keeps --layer honest: it is one filter over the whole base,
// not a page-only or a record-only switch.
func TestFindLayerNarrowsBothHalves(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\n---\n\n# Retrieval boundary\n\nA claim about the retrieval boundary.\n")

	pagesOnly, err := services.Find(t.Context(), base, services.FindFilter{
		Grep: []string{"retrieval"}, Layers: []core.Layer{core.LayerWiki}, Limit: services.NoFindLimit,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pagesOnly.Pages) != 1 || len(pagesOnly.Records) != 0 {
		t.Fatalf("result = %+v, want the wiki page and no record", pagesOnly)
	}
	eventsOnly, err := services.Find(t.Context(), base, services.FindFilter{
		Grep: []string{"retrieval"}, Layers: []core.Layer{core.LayerEvents}, Limit: services.NoFindLimit,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsOnly.Pages) != 0 {
		t.Fatalf("pages = %+v, want none when --layer names events", eventsOnly.Pages)
	}
}

// TestFindSkipsPagesForRecordOnlyFilters keeps a filter no page could satisfy from reporting
// "no pages matched" as though the question had been asked of them.
func TestFindSkipsPagesForRecordOnlyFilters(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\n---\n\n# Retrieval boundary\n\nA claim about the retrieval boundary.\n")

	for name, filter := range map[string]services.FindFilter{
		"source": {Grep: []string{"retrieval"}, Sources: []string{"synthetic"}},
		"where":  {Grep: []string{"retrieval"}, Where: []services.WhereClause{mustWhere(t, ".id=a1")}},
	} {
		result, err := services.Find(t.Context(), base, filter, false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(result.Pages) != 0 {
			t.Fatalf("%s: pages = %+v, want none for a record-only filter", name, result.Pages)
		}
	}
}

func TestFindMatchesPagesAndRecordsWithTheSameOpenTerm(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\n---\n\n# Retrieval boundary\n\nDecided under FK-412, at the retrieval boundary.\n")

	result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"FK-412"}, Limit: services.NoFindLimit}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 1 || result.Pages[0].URI != "wiki/retrieval-boundary.md" {
		t.Fatalf("pages = %+v, want the concept that names the ticket", result.Pages)
	}
}

func mustWhere(t *testing.T, argument string) services.WhereClause {
	t.Helper()
	clause, err := services.ParseWhere(argument)
	if err != nil {
		t.Fatal(err)
	}
	return clause
}

// TestFindCoversTheEnabledLayersOfABaseWithoutEvents is the base a knowledge-only user keeps:
// wiki and projects on, no collection at all. Resolving the event dates before any layer check
// made every `fkf find <terms>` there fail with "layer events is disabled" — including with an
// explicit `--layer wiki` — so the command that is supposed to cover every enabled layer could
// not answer from the only layers the base had.
func TestFindCoversTheEnabledLayersOfABaseWithoutEvents(t *testing.T) {
	base := newBase(t, `name: pages
layers:
  events: false
  index: false
  tasks: true
  projects: true
  wiki: true
sources: {}
`, nil)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\n---\n\n# Retrieval boundary\n\nA claim about the retrieval boundary.\n")

	for _, filter := range []services.FindFilter{
		{Grep: []string{"retrieval"}, Limit: services.NoFindLimit},
		{Grep: []string{"retrieval"}, Layers: []core.Layer{core.LayerWiki}, Limit: services.NoFindLimit},
	} {
		result, err := services.Find(t.Context(), base, filter, false)
		if err != nil {
			t.Fatalf("Find(%+v) error = %v, want the enabled layers searched", filter.Layers, err)
		}
		if len(result.Pages) != 1 {
			t.Fatalf("pages = %+v, want the one wiki page this base holds", result.Pages)
		}
	}

	// A direct request for the disabled layer still gets the refusal: "you turned it off" and
	// "it is empty" are different answers, and only the direct request earns the first.
	_, err := services.Find(t.Context(), base, services.FindFilter{
		Grep: []string{"retrieval"}, Layers: []core.Layer{core.LayerEvents}, Limit: services.NoFindLimit,
	}, false)
	var disabled core.ErrLayerDisabled
	if !errors.As(err, &disabled) || disabled.Layer != core.LayerEvents {
		t.Fatalf("error = %v, want `--layer events` on a disabled layer refused", err)
	}
}

func TestFindRefusesEveryExplicitlyRequestedDisabledLayer(t *testing.T) {
	base := newBase(t, `name: disabled
layers:
  events: false
  index: false
  tasks: false
  projects: false
  wiki: false
sources: {}
`, nil)
	for _, layer := range core.Layers {
		t.Run(string(layer), func(t *testing.T) {
			_, err := services.Find(t.Context(), base, services.FindFilter{
				Grep: []string{"term"}, Layers: []core.Layer{layer}, Limit: services.NoFindLimit,
			}, false)
			var disabled core.ErrLayerDisabled
			if !errors.As(err, &disabled) || disabled.Layer != layer {
				t.Fatalf("Find(--layer %s) error = %v, want that disabled layer refused", layer, err)
			}
		})
	}
}

// TestFindPagesAreRankedWhateverLayersAreEnabled pins the merged bound and the merged order.
// Gathering the task traces behind an early return left the wiki and project hits in layer
// order and unbounded whenever the tasks layer was off, so the same query gave a base without
// tasks a different ranking — and a different length — from a base with them.
func TestFindPagesAreRankedWhateverLayersAreEnabled(t *testing.T) {
	base := newBase(t, `name: pages
layers:
  events: false
  index: false
  tasks: false
  projects: true
  wiki: true
sources: {}
`, nil)
	// The project page says "retrieval" three times; the wiki page says it once. Ranked, the
	// project page wins — in layer order the wiki page comes first whatever it scores.
	write(t, base, "wiki/thin.md",
		"---\ntype: concept\ntitle: Thin\n---\n\n# Thin\n\nOne mention of retrieval.\n")
	write(t, base, "projects/dense.md",
		"---\ntype: project\ntitle: Dense\nstatus: active\n---\n\n# Dense\n\nRetrieval, retrieval, and retrieval again.\n")

	result, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"retrieval"}, Limit: services.NoFindLimit}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 2 {
		t.Fatalf("pages = %+v, want both", result.Pages)
	}
	if result.Pages[0].Score < result.Pages[1].Score {
		t.Fatalf("pages = %+v, want them ranked by score across layers, not left in layer order", result.Pages)
	}
	// And --limit bounds the merged list rather than being skipped with the tasks layer.
	bounded, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"retrieval"}, Limit: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Pages) != 1 || bounded.Pages[0].URI != result.Pages[0].URI {
		t.Fatalf("bounded = %+v, want the single best-scoring page", bounded.Pages)
	}
}

// TestListLearnedExtractsWrappedBulletsAndMarksTheBacklog is the loop-closing feature: a
// backlog nobody can enumerate is a backlog nobody works, so this has to find every bullet
// under every "## Learned" heading — including one this project's own convention hard-wraps
// across several indented continuation lines with no blank line between bullets — and say
// which ones some page has already promoted.
func TestListLearnedExtractsWrappedBulletsAndMarksTheBacklog(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "tasks/2026-05-04/session/TASKS.md", "# Session\n\n"+
		"## 1. First instruction\n\n"+
		"### Verification\n\n```text\n- this is code, not a bullet\n```\n\n"+
		"## Learned\n\n"+
		"- A short bullet cited elsewhere.\n"+
		"- A bullet that wraps across two\n  physical lines with no blank line between.\n\n"+
		"## 2. Second instruction\n\n"+
		"## Learned\n\n"+
		"- A bullet under the second Learned section.\n")
	write(t, base, "wiki/promoted.md", "---\ntype: decision\ntitle: Promoted\ntags: [x]\n"+
		"sources:\n  - ../tasks/2026-05-04/session/TASKS.md#learned\n---\n\n# Promoted\n")

	listing, err := services.ListLearned(t.Context(), base, services.Window{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Bullets) != 3 {
		t.Fatalf("bullets = %+v, want 3 (the fenced code line must never be read as a bullet)", listing.Bullets)
	}
	if listing.Harvested != 3 || listing.Unharvested != 0 {
		t.Fatalf("harvested/unharvested = %d/%d, want all 3 harvested: the wiki page cites the trace",
			listing.Harvested, listing.Unharvested)
	}
	wrapped := listing.Bullets[1]
	if wrapped.Text != "A bullet that wraps across two physical lines with no blank line between." {
		t.Fatalf("wrapped bullet text = %q, want the two physical lines folded into one", wrapped.Text)
	}
	for _, bullet := range listing.Bullets {
		if bullet.Trace != "tasks/2026-05-04/session/TASKS.md" {
			t.Fatalf("bullet.Trace = %q, want the trace's own URI", bullet.Trace)
		}
	}
}

// TestListLearnedTellsAnUnharvestedBaseFromAPromotedOne is the state that matters: a trace
// nothing cites yet is the whole point of the backlog, and --unharvested has to keep it while
// dropping what has already been promoted.
func TestListLearnedTellsAnUnharvestedBaseFromAPromotedOne(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "tasks/2026-05-04/promoted/TASKS.md", "# Promoted trace\n\n## Learned\n\n- Cited.\n")
	write(t, base, "tasks/2026-05-05/orphan/TASKS.md", "# Orphan trace\n\n## Learned\n\n- Not cited anywhere.\n")
	write(t, base, "wiki/from-promoted.md", "---\ntype: decision\ntitle: From promoted\ntags: [x]\n"+
		"sources:\n  - ../tasks/2026-05-04/promoted/TASKS.md#learned\n---\n\n# From promoted\n")

	all, err := services.ListLearned(t.Context(), base, services.Window{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Bullets) != 2 || all.Harvested != 1 || all.Unharvested != 1 {
		t.Fatalf("all = %+v, want one harvested and one not", all)
	}
	backlog, err := services.ListLearned(t.Context(), base, services.Window{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog.Bullets) != 1 || backlog.Bullets[0].Trace != "tasks/2026-05-05/orphan/TASKS.md" {
		t.Fatalf("backlog = %+v, want only the orphan trace's bullet", backlog.Bullets)
	}
	// The counts still describe the WHOLE base, not the filtered view, so the backlog size is
	// always visible even when --unharvested hides everything else.
	if backlog.Harvested != 1 || backlog.Unharvested != 1 {
		t.Fatalf("backlog counts = %+v, want the totals unaffected by the filter", backlog)
	}
}

// TestListLearnedIgnoresAnUnrelatedHeadingNamedSimilarly is the boundary the exact-match
// pins: only a heading spelled precisely "Learned" is read, so a page whose author wrote
// "## Lessons Learned" or "## What we learned" is left alone rather than guessed at.
func TestListLearnedIgnoresAnUnrelatedHeadingNamedSimilarly(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "tasks/2026-05-04/session/TASKS.md",
		"# Session\n\n## Lessons Learned\n\n- Not the exact heading, so not read.\n")
	listing, err := services.ListLearned(t.Context(), base, services.Window{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Bullets) != 0 {
		t.Fatalf("bullets = %+v, want none from a differently named heading", listing.Bullets)
	}
}

// TestFindRefusesAnUnknownSource is the regression test for silent-empty. A typo'd source
// name — "github-prs" for "github-pull-requests" — used to return "0 of 0 record(s)" with
// exit 0, indistinguishable from a genuinely empty base. For an agent that is a confident
// negative claim about the user's own history, so the refusal names what the base actually
// declares instead of pretending the question was answered.
func TestFindRefusesAnUnknownSource(t *testing.T) {
	base := queryBase(t)
	_, err := services.Find(t.Context(), base, services.FindFilter{Sources: []string{"github-prs"}}, false)
	if !errors.Is(err, core.ErrConfig) {
		t.Fatalf("error = %v, want core.ErrConfig", err)
	}
	if !strings.Contains(err.Error(), `unknown source "github-prs"`) || !strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("error = %v, want it to name the typo and the base's real source", err)
	}
}

// TestFindStillMatchesADisabledSourcesOldData is the other half: a source that was later
// disabled in fkf.yaml still has real documents on disk, and --source must keep finding them.
// AGENTS.md is explicit that a stored document never depends on the live configuration, and
// the validation this closes a hole in must not quietly contradict that.
func TestFindStillMatchesADisabledSourcesOldData(t *testing.T) {
	base := newBase(t, `name: brain
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  retired:
    enabled: false
    layer: events
    run: [echo, "[]"]
    fields:
      id: .id
      time: .t
`, nil)
	// Written directly rather than through the collect() helper, which always speaks for a
	// source named "synthetic": this fixture's whole point is a source under a DIFFERENT name.
	write(t, base, "events/2026-05-04/retired.json", `{
  "fkf": 1, "source": "retired", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo '[]'",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 1, "records": [{"id": "a1", "t": "2026-05-04T09:00:00Z"}]
}`)
	result, err := services.Find(t.Context(), base, services.FindFilter{Sources: []string{"retired"}}, false)
	if err != nil {
		t.Fatalf("Find() error = %v, want a disabled-but-declared source still searchable", err)
	}
	if result.Matched != 1 {
		t.Fatalf("matched = %d, want the one record a disabled source already collected", result.Matched)
	}
}
