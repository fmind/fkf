package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestContextPhrasePostingsIncludeExactPhraseInsideMarkdownPunctuation(t *testing.T) {
	item := &ContextItem{}
	item.addSegment("body", "prints `fmind/fkf main`).", 1)
	postings := map[lexicalPostingKey]map[int]struct{}{}
	addContextPhrasePostings(postings, 7, item)
	if _, found := postings[lexicalPostingKey{Kind: lexicalContextPhrase, Value: "fmind/fkf main"}][7]; !found {
		t.Fatal("exact phrase inside Markdown punctuation was not indexed")
	}
}

func TestContextBodyReceiptFitsDefaultBudgetAndMatchesIndexedFallback(t *testing.T) {
	base := statusDocumentBase(t)
	idPath, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	titlePath, err := core.ParseFieldPath(".title")
	if err != nil {
		t.Fatal(err)
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "cached-notes", Layer: core.LayerIndex,
		CollectedAt: "2026-08-25T00:00:00Z",
		Schema: core.FieldSchema{
			core.FieldID:    {Description: "Stable identity.", Cardinality: core.CardinalityOne},
			core.FieldTitle: {Description: "Display title.", Cardinality: core.CardinalityOne},
		},
		Fields: core.FieldMap{core.FieldID: {idPath}, core.FieldTitle: {titlePath}},
		Count:  220, Records: make([]sources.Record, 0, 220),
	}
	for index := range document.Count {
		id := fmt.Sprintf("%s%03d", strings.Repeat("unrelated-cache-id-", 5), index)
		document.Records = append(document.Records, sources.Record{
			"id": id, "title": fmt.Sprintf("Archived item %03d", index),
		})
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	var matchedURI string
	for index, record := range document.Records {
		uri, ok := document.RecordURI(record)
		if !ok {
			t.Fatalf("record %d has no URI", index)
		}
		body := "Unrelated cached narrative."
		if index == 0 {
			body = "The unique-body-needle appears only here."
			matchedURI = uri
		}
		if _, err := cacheBody(t.Context(), base, document, record, uri, body); err != nil {
			t.Fatal(err)
		}
	}

	request := ContextRequest{Query: "unique-body-needle", Budget: DefaultBudget}
	fallback, err := BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Receipt.EncodedTokens > DefaultBudget ||
		!slices.Equal(fallback.Receipt.ConsultedBodies, []string{matchedURI}) {
		t.Fatalf("fallback receipt = %+v, want one relevant body inside the default budget", fallback.Receipt)
	}
	if _, err := BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Receipt.Index.Used || !slices.Equal(indexed.Receipt.ConsultedBodies, []string{matchedURI}) {
		t.Fatalf("indexed receipt = %+v, want the same one relevant body", indexed.Receipt)
	}

	fallback.Receipt.Index, indexed.Receipt.Index = LexicalIndexUse{}, LexicalIndexUse{}
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
		t.Fatalf("body receipt indexed and fallback packs differ outside diagnostics\nwithout: %s\nindexed: %s", want, got)
	}
}

func TestLexicalTermScoreIncludesIdentifierSubstringMatchesAcrossSegments(t *testing.T) {
	item := &ContextItem{}
	item.addSegment(core.FieldID, "fmind/fkf@abcdef", 10)
	item.addSegment("repo", "fmind/fkf", 1)

	score, found := lexicalTermScores(item)["fmind/fkf"]
	if !found {
		t.Fatal("path prefix inside an identifier token produced no exact term summary")
	}
	if !score.Analysis.matched || score.Analysis.maxWeight != 10 || len(score.Analysis.segments) != 1 ||
		score.Analysis.segments[0].Field != core.FieldID {
		t.Fatalf("term analysis = %+v, want the canonical weight-10 id match", score.Analysis)
	}
}

func TestLexicalTermScoreHydratesAnUnboundedHigherWeightSubstring(t *testing.T) {
	item := &ContextItem{}
	item.addSegment(core.FieldID, "prefixabc/defsuffix", 10)
	item.addSegment("repo", "abc/def", 1)
	score := lexicalTermScores(item)["abc/def"]
	item.identifierBounds = lexicalCandidateIdentifierBounds(item)

	if lexicalTermScoreIsComplete(item, "abc/def", score.Analysis) {
		t.Fatalf("term analysis = %+v, want the possible higher-weight substring hydrated", score.Analysis)
	}

	unrelated := &ContextItem{}
	unrelated.addSegment(core.FieldID, "unrelated/identifier", 10)
	unrelated.addSegment("repo", "abc/def", 1)
	score = lexicalTermScores(unrelated)["abc/def"]
	unrelated.identifierBounds = lexicalCandidateIdentifierBounds(unrelated)
	if !lexicalTermScoreIsComplete(unrelated, "abc/def", score.Analysis) {
		t.Fatal("an unrelated high-weight segment caused needless hydration")
	}
}

func TestResolveLexicalInputDigestHashesOnlyChangedStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	const relative = "events/2026-09-02/source.json"
	want := strings.Repeat("a", 64)
	known := map[string]LexicalInputFile{relative: {
		Path: relative, Bytes: info.Size(), ModifiedUnixNano: info.ModTime().UnixNano(), SHA256: want,
	}}
	hashed := 0
	hasher := func(context.Context, string) (int64, string, error) {
		hashed++
		return info.Size(), strings.Repeat("b", 64), nil
	}

	got, err := resolveLexicalInputDigest(t.Context(), path, relative, info, known, hasher)
	if err != nil || got != want || hashed != 0 {
		t.Fatalf("unchanged digest = %q, %v; hashes = %d, want manifest digest and no content hash", got, err, hashed)
	}

	changed := lexicalTestFileInfo{FileInfo: info, modified: info.ModTime().Add(time.Nanosecond)}
	got, err = resolveLexicalInputDigest(t.Context(), path, relative, changed, known, hasher)
	if err != nil || got != strings.Repeat("b", 64) || hashed != 1 {
		t.Fatalf("changed digest = %q, %v; hashes = %d, want one content hash", got, err, hashed)
	}
}

type lexicalTestFileInfo struct {
	os.FileInfo
	modified time.Time
}

func (info lexicalTestFileInfo) ModTime() time.Time { return info.modified }

func TestLexicalCorpusKeepsPagePostingsOnlyForContext(t *testing.T) {
	page := &ContextItem{URI: "wiki/index.md", Kind: string(core.LayerWiki), Title: "Authored needle"}
	page.addSegment(core.FieldTitle, page.Title, core.DefaultFieldWeight)
	record := &ContextItem{
		URI: "events/2026-09-02/source.json#one", Kind: "record", Source: "source", Title: "Record needle",
	}
	record.addSegment(core.FieldTitle, record.Title, core.DefaultFieldWeight)
	corpus := &lexicalCorpus{entries: []*lexicalEntry{
		{ID: 0, URI: page.URI, Kind: string(core.LayerWiki), Context: true, candidate: page, findTexts: []string{page.Title}},
		{
			ID: 1, URI: record.URI, Kind: string(core.LayerEvents), Source: record.Source, Context: true,
			Collapsed: []string{record.URI}, candidate: record, findTexts: []string{record.Title},
		},
	}}

	encoded, err := encodeLexicalCorpus(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if encoded.Postings == 0 {
		t.Fatal("record produced no lexical postings")
	}
	pageContextPosting := false
	postingBytes := encoded.Rows[encoded.PostingsOffset:encoded.LookupOffset]
	for _, line := range strings.Split(strings.TrimSpace(string(postingBytes)), "\n") {
		fields := strings.Split(line, "\t")
		for _, id := range fields[2:] {
			if id == "0" {
				if fields[0] == lexicalFindTrigram || fields[0] == lexicalBodyTrigram {
					t.Fatalf("authored page leaked into find posting row kind %s", fields[0])
				}
				pageContextPosting = true
			}
		}
	}
	if !pageContextPosting {
		t.Fatal("authored page produced no context posting")
	}
}

func TestLexicalLookupShardAuthenticatesInclusionAndAbsence(t *testing.T) {
	page := &ContextItem{URI: "wiki/index.md", Kind: string(core.LayerWiki), Title: "Authored needle"}
	page.addSegment(core.FieldTitle, page.Title, core.DefaultFieldWeight)
	corpus := &lexicalCorpus{entries: []*lexicalEntry{{
		ID: 0, URI: page.URI, Kind: string(core.LayerWiki), Context: true, candidate: page,
	}}}
	encoded, err := encodeLexicalCorpus(corpus)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "postings.tsv")
	if err := os.WriteFile(path, encoded.Rows, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	meta := LexicalIndexMeta{
		Bytes: len(encoded.Rows), Entries: 1, Postings: encoded.Postings, PostingRows: encoded.PostingRows,
		PostingsOffset: encoded.PostingsOffset, LookupOffset: encoded.LookupOffset,
		CandidatesOffset: encoded.CandidatesOffset, LookupShards: encoded.LookupShards,
	}
	key := lexicalPostingKey{Kind: lexicalContextToken, Value: "needle"}
	lookup, err := readLexicalLookupShard(t.Context(), file, meta, lexicalPostingShard(key))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := lookup[key]
	if !found {
		t.Fatalf("lookup keys = %+v, want %+v", lookup, key)
	}
	ids, _, err := readLexicalPosting(t.Context(), file, descriptor, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := ids[0]; !found {
		t.Fatalf("posting IDs = %+v, want entry 0", ids)
	}
	missing := lexicalPostingKey{Kind: lexicalContextToken, Value: "missing"}
	lookup, err = readLexicalLookupShard(t.Context(), file, meta, lexicalPostingShard(missing))
	if err != nil {
		t.Fatal(err)
	}
	if _, found := lookup[missing]; found {
		t.Fatalf("lookup unexpectedly contains missing key %+v", missing)
	}
}

func TestFindRetriesWhenEvidenceChangesAfterIndexAdmission(t *testing.T) {
	base := statusDocumentBase(t)
	if _, err := BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	filter := FindFilter{Grep: []string{"newly-matching"}, Limit: NoFindLimit}
	selected, err := prepareFindScan(t.Context(), base, &filter)
	if err != nil {
		t.Fatal(err)
	}
	document, err := base.ReadDocumentContext(t.Context(), "events/2026-08-24/events.json")
	if err != nil {
		t.Fatal(err)
	}
	document.Records[0]["note"] = "newly-matching"
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	result, err := findPrepared(t.Context(), base, filter, false, selected)
	if err != nil {
		t.Fatal(err)
	}
	if result.Index == nil || result.Index.Used || result.Index.Reason != LexicalIndexFallbackStale {
		t.Fatalf("index receipt = %+v, want stale fallback", result.Index)
	}
	if len(result.Records) != 1 || result.Records[0].URI != "events/2026-08-24/events.json#event-1" {
		t.Fatalf("records = %+v, want the match added after index admission", result.Records)
	}
}

func TestFindFallbackRetriesWhenEvidenceChangesAfterItsScan(t *testing.T) {
	base := statusDocumentBase(t)
	var once sync.Once
	filter := FindFilter{
		Grep: []string{"newly-matching"}, Limit: NoFindLimit,
		afterScan: func() {
			once.Do(func() {
				document, err := base.ReadDocumentContext(t.Context(), "events/2026-08-24/events.json")
				if err != nil {
					t.Fatal(err)
				}
				document.Records[0]["id"] = "newly-matching"
				if err := base.WriteDocument(document); err != nil {
					t.Fatal(err)
				}
			})
		},
	}
	result, err := Find(t.Context(), base, filter, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || result.Records[0].URI != "events/2026-08-24/events.json#newly-matching" {
		t.Fatalf("fallback records = %+v, want only the retried generation", result.Records)
	}
	if result.Index == nil || result.Index.Used || result.Index.Reason != LexicalIndexFallbackStale {
		t.Fatalf("fallback receipt = %+v, want a generation-drift retry", result.Index)
	}
}

func TestContextRevalidatesEventOnlyIndexInputsAfterAdmission(t *testing.T) {
	base := statusDocumentBase(t)
	if _, err := BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	resolver, err := LoadIdentityResolver(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	request := ContextRequest{Query: "event-1", Budget: 1000}
	set, err := prepareContextCandidateSet(
		t.Context(), base, request, "2026-08-25", []string{"event-1"}, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !set.index.Used || len(set.cachedPages) != 0 {
		t.Fatalf("candidate set index=%+v cached_pages=%d, want an indexed record-only set", set.index, len(set.cachedPages))
	}
	document, err := base.ReadDocumentContext(t.Context(), "events/2026-08-24/events.json")
	if err != nil {
		t.Fatal(err)
	}
	document.Records[0]["note"] = "changed after context index admission"
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	err = revalidateIndexedContextPages(t.Context(), base, request, "2026-08-25", resolver, set, nil)
	if !errors.Is(err, errLexicalIndexStale) {
		t.Fatalf("record-only context revalidation error = %v, want stale-index retry", err)
	}
}

func TestContextFallbackRetriesWhenEvidenceChangesAfterItsScan(t *testing.T) {
	base := statusDocumentBase(t)
	var once sync.Once
	request := ContextRequest{
		Query: "newly-matching", Budget: 1000,
		afterCandidateScan: func() {
			once.Do(func() {
				document, err := base.ReadDocumentContext(t.Context(), "events/2026-08-24/events.json")
				if err != nil {
					t.Fatal(err)
				}
				document.Records[0]["id"] = "newly-matching"
				if err := base.WriteDocument(document); err != nil {
					t.Fatal(err)
				}
			})
		},
	}
	pack, err := BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) != 1 || pack.Items[0].URI != "events/2026-08-24/events.json#newly-matching" {
		t.Fatalf("fallback items = %+v, want only the retried generation", pack.Items)
	}
	if pack.Receipt.Index.Used || pack.Receipt.Index.Reason != LexicalIndexFallbackStale {
		t.Fatalf("fallback receipt = %+v, want a generation-drift retry", pack.Receipt.Index)
	}
}
