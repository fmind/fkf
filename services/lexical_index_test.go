package services_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestContextIsSemanticallyIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{
		Query: "retrieval boundary FK-412", Budget: 4096, Explain: true,
	}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Receipt.Index.Used || fallback.Receipt.Index.Reason != services.LexicalIndexFallbackMissing {
		t.Fatalf("fallback index receipt = %+v, want missing", fallback.Receipt.Index)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used || indexed.Receipt.Index.Reason != "" {
		t.Fatalf("indexed receipt = %+v, want used", indexed.Receipt.Index)
	}

	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("indexed and fallback packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestCompactTextContextIsSemanticallyIdenticalWithRankSummaries(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{
		Query: "retrieval boundary FK-412", Budget: 4096,
		DeliveryFormat: services.ContextDeliveryText, SaveSnapshot: true,
	}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used {
		t.Fatalf("indexed receipt = %+v, want rank-summary index use", indexed.Receipt.Index)
	}
	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rank-summary and fallback text packs differ outside diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestCompactTextRankSummariesApplyAnUnmatchedSuperseder(t *testing.T) {
	base := newBase(t, `fkf: 1
name: indexed-supersession
schema:
  id: {description: Stable identity., cardinality: one}
  supersedes: {description: Replaced knowledge., cardinality: many}
layers: {events: false, index: false, tasks: false, projects: false, wiki: true}
sources: {}
`, nil)
	write(t, base, "wiki/old-policy.md", `---
type: decision
title: Targetneedle policy
---

# Targetneedle policy

The original policy.
`)
	write(t, base, "wiki/replacement.md", `---
type: decision
title: Replacement policy
relations:
  supersedes: [wiki/old-policy.md]
---

# Replacement policy

The current rule.
`)
	request := services.ContextRequest{
		Query: "targetneedle", Budget: 4096, DeliveryFormat: services.ContextDeliveryText,
	}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used {
		t.Fatalf("indexed receipt = %+v, want rank-summary index use", indexed.Receipt.Index)
	}
	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rank-summary supersession differs from fallback\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestCompactTextRankSummariesDiscardIdentifierTrigramFalsePositives(t *testing.T) {
	base := newBase(t, `fkf: 1
name: indexed-identifier-filter
schema:
  id: {description: Stable identity., cardinality: one}
layers: {events: false, index: false, tasks: false, projects: false, wiki: true}
sources: {}
`, nil)
	write(t, base, "wiki/exact.md", "# abc/def\n\nThe exact identifier.\n")
	write(t, base, "wiki/false-positive.md", "# Separate grams\n\nabc bc/ c/d /de def\n")
	request := services.ContextRequest{
		Query: "abc/def", Budget: 4096, DeliveryFormat: services.ContextDeliveryText,
	}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used {
		t.Fatalf("indexed receipt = %+v, want rank-summary index use", indexed.Receipt.Index)
	}
	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rank-summary false-positive filtering differs from fallback\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestContextUsesLexicalIndexForTaskTraceWithoutFrontmatterDate(t *testing.T) {
	base := contextBase(t)
	write(t, base, "tasks/2026-05-10/aubergine-resume/TASKS.md",
		"# Aubergine resume\n\nContinue the aubergine retrieval work.\n")
	request := services.ContextRequest{Query: "aubergine resume", Budget: 4096, Explain: true}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used || indexed.Receipt.Index.Reason != "" {
		t.Fatalf("indexed receipt = %+v, want the task trace to retain its path-derived date", indexed.Receipt.Index)
	}
	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("indexed and fallback task packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestContextRejectsSameFingerprintLookupCorruption(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval boundary FK-412", Budget: 4096, Explain: true}
	want, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	report, err := services.BuildLexicalIndex(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base.Root(), filepath.FromSlash(services.LexicalIndexPath))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the requested lookup key by a different, structurally valid key in the same
	// hash partition. Size and mtime remain unchanged, so only the partition proof prevents
	// this from looking like an authenticated absence and silently omitting candidates.
	const original = "retrieval"
	replacement := sameLexicalLookupShard(t, original)
	originalKey := base64.RawURLEncoding.EncodeToString([]byte("T\x00" + original))
	replacementKey := base64.RawURLEncoding.EncodeToString([]byte("T\x00" + replacement))
	lookup := data[report.Meta.LookupOffset:report.Meta.CandidatesOffset]
	position := bytes.Index(lookup, []byte("L\t"+originalKey+"\t"))
	if position < 0 {
		t.Fatalf("lookup section has no context-token key for %q", original)
	}
	copy(lookup[position+2:position+2+len(originalKey)], replacementKey)
	if err := os.WriteFile(path, data, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Size() != info.Size() || changed.ModTime().UnixNano() != info.ModTime().UnixNano() {
		t.Fatalf("corrupt index fingerprint = %d/%d, want preserved %d/%d",
			changed.Size(), changed.ModTime().UnixNano(), info.Size(), info.ModTime().UnixNano())
	}

	got, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipt.Index.Used || got.Receipt.Index.Reason != services.LexicalIndexFallbackCorrupt {
		t.Fatalf("index receipt after same-fingerprint corruption = %+v, want corrupt fallback", got.Receipt.Index)
	}
	want.Receipt.Index, got.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	want.Receipt.EncodedTokens, got.Receipt.EncodedTokens = 0, 0
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("corrupt-index fallback changed the semantic pack\nwithout: %s\ncorrupt: %s", wantJSON, gotJSON)
	}
}

func sameLexicalLookupShard(t *testing.T, original string) string {
	t.Helper()
	want := sha256.Sum256([]byte("T\x00" + original))
	for left := byte('a'); left <= 'z'; left++ {
		for middle := byte('a'); middle <= 'z'; middle++ {
			for right := byte('a'); right <= 'z'; right++ {
				candidate := original[:len(original)-3] + string([]byte{left, middle, right})
				if candidate == original {
					continue
				}
				digest := sha256.Sum256([]byte("T\x00" + candidate))
				if digest[0] == want[0] && digest[1]>>4 == want[1]>>4 {
					return candidate
				}
			}
		}
	}
	t.Fatalf("no same-length lookup key shares the requested partition with %q", original)
	return ""
}

func TestNaturalContextQuestionsAreIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	for _, query := range []string{
		"Take my last retrieval boundary",
		"What changed in FK-412?",
	} {
		t.Run(query, func(t *testing.T) {
			base := contextBase(t)
			request := services.ContextRequest{Query: query, Budget: 4096, Explain: true}
			fallback, err := services.BuildContext(t.Context(), base, request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			indexed, err := services.BuildContext(t.Context(), base, request)
			if err != nil {
				t.Fatal(err)
			}
			if !indexed.Receipt.Index.Used {
				t.Fatalf("indexed receipt = %+v, want natural query to use the cache", indexed.Receipt.Index)
			}
			fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
			fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
			want, err := json.Marshal(fallback)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(indexed)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("indexed and fallback natural-query packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
			}
		})
	}
}

func TestLastContextPrefersWindowedEventToTimestampedIndexInventory(t *testing.T) {
	assertLastContextIndexParity(t, services.ContextRequest{
		Query: "Take my last meeting notes", Budget: 4096, Explain: true,
	})
}

func TestLastContextSummaryPrefersWindowedEventToTimestampedIndexInventory(t *testing.T) {
	assertLastContextIndexParity(t, services.ContextRequest{
		Query: "Take my last meeting notes", Budget: 4096,
		DeliveryFormat: services.ContextDeliveryText,
	})
}

func assertLastContextIndexParity(t *testing.T, request services.ContextRequest) {
	t.Helper()
	base := contextBase(t)
	collect(t, base, "2026-05-09", `[
		{"id":"newest-notes","t":"2026-05-09T09:00:00Z","subject":"Project sync - Notes by Gemini"}
	]`)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "drive-files", Layer: core.LayerIndex,
		CollectedAt: "2026-05-10T09:00:00Z",
		Fields: sources.Fields{
			core.FieldID:    {mustFieldPath(t, ".id")},
			core.FieldTime:  {mustFieldPath(t, ".modified")},
			core.FieldTitle: {mustFieldPath(t, ".title")},
		},
		Count: 1,
		Records: []sources.Record{{
			"id": "archived-notes", "modified": "2025-08-15T09:00:00Z", "title": "meeting notes",
		}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}

	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	assertLastContextOrder(t, fallback, false)
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	assertLastContextOrder(t, indexed, true)

	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("last-query indexed and fallback packs differ outside diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func assertLastContextOrder(t *testing.T, pack *services.ContextPack, indexed bool) {
	t.Helper()
	if pack.Receipt.Index.Used != indexed {
		t.Fatalf("index receipt = %+v, want used=%t", pack.Receipt.Index, indexed)
	}
	if len(pack.Items) < 2 || pack.Items[0].URI != "events/2026-05-09/synthetic.json#newest-notes" {
		t.Fatalf("items = %+v, want the newest window-addressed notes first", pack.Items)
	}
	for _, item := range pack.Items {
		if item.URI == "index/drive-files.json#archived-notes" {
			return
		}
	}
	t.Fatalf("items = %+v, want the timestamped current-state inventory to remain searchable", pack.Items)
}

func TestExactPageURIIsIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "wiki/retrieval-boundary.md", Budget: 4096, Explain: true}
	want, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Items) == 0 || want.Items[0].URI != "wiki/retrieval-boundary.md" {
		t.Fatalf("fallback items = %+v, want the exact authored-page URI", want.Items)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	got, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Receipt.Index.Used {
		t.Fatalf("indexed receipt = %+v, want used", got.Receipt.Index)
	}
	want.Receipt.Index, got.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	want.Receipt.EncodedTokens, got.Receipt.EncodedTokens = 0, 0
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("indexed and fallback exact-URI packs differ outside diagnostics\nwithout: %s\nindexed: %s", wantJSON, gotJSON)
	}
}

func TestExactResourceDeduplicationIsIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	const sharedURL = "https://example.test/meeting-notes-1"
	collect(t, base, "2026-05-09", `[
		{"id":"meeting-notes-1","t":"2026-05-09T09:00:00Z","subject":"Natural meeting notes","link":"`+sharedURL+`"}
	]`)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "drive-files", Layer: core.LayerIndex,
		CollectedAt: "2026-05-10T09:00:00Z",
		Fields: sources.Fields{
			core.FieldID:    {mustFieldPath(t, ".id")},
			core.FieldTitle: {mustFieldPath(t, ".title")},
			core.FieldURL:   {mustFieldPath(t, ".url")},
		},
		Count: 1,
		Records: []sources.Record{{
			"id": "meeting-notes-1", "title": "Natural meeting notes", "url": sharedURL,
		}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	request := services.ContextRequest{Query: "Take my last meeting notes", Budget: 4096, Explain: true}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallback.Items) == 0 || fallback.Items[0].Count != 2 {
		t.Fatalf("fallback items = %+v, want one top representation of the two exact-resource records", fallback.Items)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used {
		t.Fatalf("indexed receipt = %+v, want exact-resource query to use the cache", indexed.Receipt.Index)
	}
	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("indexed and fallback exact-resource packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestContextHistoricalWindowIsIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	collect(t, base, "2026-04-01", `[
		{"id":"old-early","t":"2026-04-01T08:00:00Z","subject":"Straddled duplicate","link":"https://example.test/old-early"},
		{"id":"old-late","t":"2026-04-01T09:00:00Z","subject":"Straddled duplicate","link":"https://example.test/old-late"}
	]`)
	collect(t, base, "2026-05-09", `[{"id":"new-repeat","t":"2026-05-09T09:00:00Z","subject":"Straddled duplicate","link":"https://example.test/new-repeat"}]`)
	request := services.ContextRequest{
		Query: "straddled duplicate", Budget: 4096, Explain: true,
		Window: services.Window{Since: "2026-04-01", Until: "2026-04-01"},
	}

	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallback.Items) != 1 || fallback.Items[0].URI != "events/2026-04-01/synthetic.json#old-late" ||
		fallback.Items[0].Count != 2 {
		t.Fatalf("fallback items = %+v, want the newest in-window representative with count two", fallback.Items)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used || indexed.Receipt.Index.Reason != "" {
		t.Fatalf("indexed receipt = %+v, want the lexical index path", indexed.Receipt.Index)
	}

	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("historical indexed and fallback packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestHistoricalUntitledRecordIsIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	source := base.Config.Sources["synthetic"]
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: source.Name, Layer: source.Layer, Date: "2026-05-08",
		WindowStart: "2026-05-08T00:00:00Z", WindowEnd: "2026-05-09T00:00:00Z",
		CollectedAt: testClock.Format("2006-01-02T15:04:05Z07:00"),
		Fields:      sources.FieldsOf(source), Count: 1,
		Records: []sources.Record{{"id": "legacy-untitled", "t": "2026-05-08T09:00:00Z"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	request := services.ContextRequest{Query: "legacy-untitled", Budget: 4096, Explain: true}
	fallback, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallback.Items) != 1 || fallback.Items[0].URI != "events/2026-05-08/synthetic.json#legacy-untitled" {
		t.Fatalf("fallback items = %+v, want the historical untitled record", fallback.Items)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used || indexed.Receipt.Index.Reason != "" {
		t.Fatalf("indexed receipt = %+v, want the lexical index path", indexed.Receipt.Index)
	}

	fallback.Receipt.Index, indexed.Receipt.Index = services.LexicalIndexUse{}, services.LexicalIndexUse{}
	fallback.Receipt.EncodedTokens, indexed.Receipt.EncodedTokens = 0, 0
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("historical indexed and fallback packs differ outside execution diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestBuildIndexTargetAndStaleCheck(t *testing.T) {
	base := contextBase(t)
	if stale, err := services.BuildStale(t.Context(), base, "index"); err != nil || !stale {
		t.Fatalf("BuildStale(index) = %v, %v; want missing cache to be stale", stale, err)
	}
	report, err := services.Build(t.Context(), base, "index", false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Index == nil || report.Graph != nil || report.Wiki != nil {
		t.Fatalf("Build(index) = %+v, want only the lexical index", report)
	}
	if stale, err := services.BuildStale(t.Context(), base, "index"); err != nil || stale {
		t.Fatalf("BuildStale(index) = %v, %v; want current cache", stale, err)
	}
	if all, err := services.Build(t.Context(), base, "all", false); err != nil || all.Index == nil {
		t.Fatalf("Build(all) = %+v, %v; want lexical cache after wiki and graph", all, err)
	}
}

func TestFindIsSemanticallyIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	filter := services.FindFilter{Grep: []string{"retrieval", "boundary"}, Limit: services.NoFindLimit}
	fallback, err := services.Find(t.Context(), base, filter, false)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Index == nil || fallback.Index.Used || fallback.Index.Reason != services.LexicalIndexFallbackMissing {
		t.Fatalf("fallback find index = %+v, want missing", fallback.Index)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.Find(t.Context(), base, filter, false)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.Index == nil || !indexed.Index.Used || indexed.Index.Reason != "" {
		t.Fatalf("indexed find = %+v, want index used", indexed.Index)
	}
	fallback.Index, indexed.Index = nil, nil
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("indexed and fallback find differ outside execution diagnostic\nwithout: %s\nindexed: %s", want, got)
	}

	short, err := services.Find(t.Context(), base, services.FindFilter{Grep: []string{"re"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if short.Index == nil || short.Index.Used || short.Index.Reason != services.LexicalIndexFallbackQueryTooShort {
		t.Fatalf("short-query index = %+v, want explicit scan reason", short.Index)
	}
}

func TestBoundedFindIsSemanticallyIdenticalWithAndWithoutLexicalIndex(t *testing.T) {
	base := contextBase(t)
	filter := services.FindFilter{Grep: []string{"retrieval"}}
	fallback, err := services.FindBounded(t.Context(), base, filter, false, 100, services.FindPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.FindBounded(t.Context(), base, filter, false, 100, services.FindPosition{})
	if err != nil {
		t.Fatal(err)
	}
	fallback.Result.Index, indexed.Result.Index = nil, nil
	want, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("indexed and fallback bounded find differ outside execution diagnostic\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestSyncRebuildsLexicalIndexOnlyWhenSearchableBytesChange(t *testing.T) {
	base, _ := syncBase(t, oneRecord)
	first, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Graph != nil || first.Index == nil {
		t.Fatalf("first sync = %+v, want lexical index even when graph rebuild is disabled", first)
	}
	path := filepath.Join(base.Root(), filepath.FromSlash(services.LexicalIndexPath))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	again, err := services.Sync(t.Context(), base, services.SyncRequest{Days: 1, Force: true, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Index != nil {
		t.Fatalf("identical forced recollection rebuilt a current lexical index: %+v", again.Index)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("identical recollection changed deterministic lexical postings")
	}
}

func TestLexicalIndexBuildIsDeterministicAndClassifiesFallbacks(t *testing.T) {
	base := contextBase(t)
	first, err := services.BuildLexicalIndex(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if first.URI != services.LexicalIndexPath || first.Entries == 0 || first.Postings == 0 {
		t.Fatalf("BuildLexicalIndex() = %+v, want a non-empty postings cache", first)
	}
	firstBytes, err := os.ReadFile(filepath.Join(base.Root(), filepath.FromSlash(services.LexicalIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if use, err := services.LexicalIndexStatus(t.Context(), base); err != nil || !use.Used || use.Reason != "" {
		t.Fatalf("LexicalIndexStatus() = %+v, %v; want a current cache", use, err)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(base.Root(), filepath.FromSlash(services.LexicalIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(firstBytes) {
		t.Fatal("identical searchable inputs produced different postings bytes")
	}

	page := filepath.Join(base.Root(), "wiki", "retrieval-boundary.md")
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, append(data, []byte("\nChanged searchable text.\n")...), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if use, err := services.LexicalIndexStatus(t.Context(), base); err != nil || use.Used || use.Reason != services.LexicalIndexFallbackStale {
		t.Fatalf("status after an authored edit = %+v, %v; want stale fallback", use, err)
	}

	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(base.Root(), filepath.FromSlash(services.LexicalIndexPath))
	if err := os.WriteFile(indexPath, []byte("not a postings index\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if use, err := services.LexicalIndexStatus(t.Context(), base); err != nil || use.Used || use.Reason != services.LexicalIndexFallbackCorrupt {
		t.Fatalf("status after output corruption = %+v, %v; want corrupt fallback", use, err)
	}

	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if use, err := services.LexicalIndexStatus(t.Context(), base); err != nil || use.Used || use.Reason != services.LexicalIndexFallbackMissing {
		t.Fatalf("status after removal = %+v, %v; want missing fallback", use, err)
	}
}
