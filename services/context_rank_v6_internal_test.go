package services

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
)

func TestRankingV6TermGrammarAndMatching(t *testing.T) {
	if RankingVersion != 6 {
		t.Fatalf("RankingVersion = %d, want 6", RankingVersion)
	}
	terms, err := queryTerms(t.Context(), "go graph fkf-v6 docs/context person:email/marc@x.test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"graph", "fkf-v6", "docs/context", "person:email/marc@x.test"}
	if !slices.Equal(terms, want) {
		t.Fatalf("terms = %q, want %q", terms, want)
	}
	if contextTermMatches("catalog graphing graph", "cat") {
		t.Fatal("plain term matched a substring")
	}
	if !contextTermMatches("catalog graphing graph", "graph") {
		t.Fatal("plain term did not match a whole token")
	}
	if !contextTermMatches("ticket FK-412 is ready", "fk-41") {
		t.Fatal("identifier-shaped term did not use substring matching")
	}
}

func TestRankingV6DropsConversationalQueryScaffolding(t *testing.T) {
	for _, test := range []struct {
		query string
		want  []string
	}{
		{"Take my last meeting notes", []string{"meeting", "notes"}},
		{"What was my meeting with IMA about?", []string{"meeting", "ima"}},
		{"What changed in kagglathon?", []string{"changed", "kagglathon"}},
	} {
		terms, err := queryTerms(t.Context(), test.query)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(terms, test.want) {
			t.Errorf("queryTerms(%q) = %q, want %q", test.query, terms, test.want)
		}
	}
	semantic := trimLeadingContextQueryScaffolding("Take my last meeting notes")
	temporal, err := ParseTemporalQuery(semantic, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if semantic != "last meeting notes" || temporal.Query != "meeting notes" || !temporal.Newest {
		t.Fatalf("semantic query = %q, temporal = %+v; want the natural wrapper to expose boundary `last`", semantic, temporal)
	}
	base := &Base{Now: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }}
	request := ContextRequest{
		Query:  "Take my last meeting notes",
		Window: Window{Since: "2026-08-01", Until: "2026-09-01"},
	}
	if _, err := normalizeContextRequest(base, &request); err != nil {
		t.Fatalf("normalize natural last query with explicit evaluation window: %v", err)
	}
	if request.Query != "meeting notes" || !request.Newest || request.Window.Since != "2026-08-01" {
		t.Fatalf("normalized request = %+v; want newest ordering inside the explicit window", request)
	}
}

func TestRankingV6PrefersDirectIdentityThenCoverageAndRelatedIdentity(t *testing.T) {
	page := &ContextItem{URI: "projects/kagglathon.md", Kind: string(core.LayerProjects), Title: "Kagglathon"}
	page.addSegment(core.FieldTitle, page.Title, core.DefaultTitleFieldWeight)
	page.addIdentifier(page.Title)

	change := &ContextItem{URI: "events/2026-08-28/commits.json#one", Kind: "record", Title: "Changed evaluator"}
	change.addSegment(core.FieldTitle, change.Title, core.DefaultTitleFieldWeight)
	change.addEntityIdentifier("repo:github.com/fmind/kagglathon")

	generic := &ContextItem{URI: "index/rss.json#one", Kind: "record", Title: "What changed was the order"}
	generic.addSegment(core.FieldTitle, generic.Title, core.MaxFieldWeight)

	candidates := []*ContextItem{generic, page, change}
	for index := range 32 {
		noise := &ContextItem{URI: fmt.Sprintf("index/noise.json#%02d", index), Kind: "record"}
		noise.addSegment(core.FieldTitle, "unrelated material", core.DefaultTitleFieldWeight)
		candidates = append(candidates, noise)
	}
	terms, err := queryTerms(t.Context(), "What changed in kagglathon?")
	if err != nil {
		t.Fatal(err)
	}
	scoreCandidates(candidates, "What changed in kagglathon?", terms, time.Now(), nil)
	sortCandidates(candidates)
	if candidates[0] != page || candidates[1] != change || candidates[2] != generic {
		t.Fatalf("ranked URIs = %q, %q, %q; want direct page, multi-term related evidence, then generic prose",
			candidates[0].URI, candidates[1].URI, candidates[2].URI)
	}
}

func TestRankingV6LastUsesStrongestMatchingFieldBeforeChronology(t *testing.T) {
	notes := &ContextItem{
		URI: "events/2026-08-27/notes.json#one", Kind: "record", Date: "2026-08-27",
		Title: "Team Notes by Gemini",
	}
	notes.addSegment(core.FieldTitle, notes.Title, core.DefaultTitleFieldWeight)

	newerBodyMention := &ContextItem{
		URI: "tasks/2026-08-30/cleanup/TASKS.md", Kind: string(core.LayerTasks), Date: "2026-08-30",
		Title: "Cleanup",
	}
	newerBodyMention.addSegment("body", "archive old notes", core.DefaultFieldWeight)

	undatedFolder := &ContextItem{URI: "index/drive.json#folder", Kind: "record", Title: "Meeting Notes"}
	undatedFolder.addSegment(core.FieldTitle, undatedFolder.Title, core.DefaultTitleFieldWeight)
	undatedFolder.addIdentifier(undatedFolder.Title)

	candidates := []*ContextItem{newerBodyMention, undatedFolder, notes}
	for index := range 4 {
		noise := &ContextItem{URI: fmt.Sprintf("index/noise.json#%02d", index), Kind: "record"}
		noise.addSegment(core.FieldTitle, "unrelated material", core.DefaultTitleFieldWeight)
		candidates = append(candidates, noise)
	}
	terms := []string{"meeting", "notes"}
	scoreCandidates(candidates, "meeting notes", terms, time.Now(), nil)
	sortCandidatesForRequest(candidates, true)
	if candidates[0] != notes {
		t.Fatalf("first = %s; want the newest dated title match, ahead of a newer body mention and an undated folder", candidates[0].URI)
	}
}

func TestRankingV6LastUsesQueryCoverageBeforeChronologyWithinOneField(t *testing.T) {
	notes := &ContextItem{
		URI: "events/2026-08-27/notes.json#one", Kind: "record", Date: "2026-08-27",
		Title: "Team meeting notes",
	}
	notes.addSegment(core.FieldTitle, notes.Title, core.DefaultTitleFieldWeight)

	newerGeneric := &ContextItem{
		URI: "events/2026-08-30/calendar.json#one", Kind: "record", Date: "2026-08-30",
		Title: "Meeting",
	}
	newerGeneric.addSegment(core.FieldTitle, newerGeneric.Title, core.DefaultTitleFieldWeight)

	candidates := []*ContextItem{newerGeneric, notes}
	for index := range 3 {
		noise := &ContextItem{URI: fmt.Sprintf("index/noise.json#%02d", index), Kind: "record"}
		noise.addSegment(core.FieldTitle, "unrelated material", core.DefaultTitleFieldWeight)
		candidates = append(candidates, noise)
	}
	terms := []string{"meeting", "notes"}
	scoreCandidates(candidates, "meeting notes", terms, time.Now(), nil)
	sortCandidatesForRequest(candidates, true)
	if candidates[0] != notes {
		t.Fatalf("first = %s; want the two-term title match ahead of a newer one-term title match", candidates[0].URI)
	}
}

func TestRankingV6LastPrefersExactThenNewestEquivalentDirectMatch(t *testing.T) {
	exact := &ContextItem{
		URI: "events/2026-08-20/meeting-notes.json#exact", Kind: "record",
		Time: "2026-08-20T09:00:00Z", Title: "meeting notes",
	}
	exact.addSegment(core.FieldTitle, exact.Title, core.DefaultTitleFieldWeight)
	exact.addIdentifier(exact.Title)

	newestComplete := &ContextItem{
		URI: "events/2026-09-04/meeting-notes.json#complete", Kind: "record",
		Time: "2026-09-04T09:00:00Z", Title: "Latest meeting notes summary",
	}
	newestComplete.addSegment(core.FieldTitle, newestComplete.Title, core.DefaultTitleFieldWeight)

	newestEquivalent := &ContextItem{
		URI: "events/2026-09-02/meeting-notes.json#newest", Kind: "record",
		Time: "2026-09-02T09:00:00Z", Title: "Client sync - Notes by Gemini",
	}
	newestEquivalent.addSegment(core.FieldTitle, newestEquivalent.Title, core.DefaultTitleFieldWeight)

	olderWithBodyMatch := &ContextItem{
		URI: "events/2026-08-31/meeting-notes.json#older", Kind: "record",
		Time: "2026-08-31T09:00:00Z", Title: "Team sync - Notes by Gemini",
	}
	olderWithBodyMatch.addSegment(core.FieldTitle, olderWithBodyMatch.Title, core.DefaultTitleFieldWeight)
	olderWithBodyMatch.addSegment("body", "meeting discussion", core.DefaultFieldWeight)

	newerBodyOnly := &ContextItem{
		URI: "tasks/2026-09-05/cleanup/TASKS.md", Kind: string(core.LayerTasks),
		Time: "2026-09-05T09:00:00Z", Title: "Cleanup",
	}
	newerBodyOnly.addSegment("body", "meeting notes", core.DefaultFieldWeight)

	candidates := []*ContextItem{newerBodyOnly, olderWithBodyMatch, newestEquivalent, newestComplete, exact}
	for index := range 5 {
		noise := &ContextItem{URI: fmt.Sprintf("index/noise.json#%02d", index), Kind: "record"}
		noise.addSegment(core.FieldTitle, "unrelated material", core.DefaultTitleFieldWeight)
		candidates = append(candidates, noise)
	}
	scoreCandidates(candidates, "meeting notes", []string{"meeting", "notes"}, time.Now(), nil)
	if !exact.explicitIdentity || olderWithBodyMatch.matchedTerms <= newestEquivalent.matchedTerms ||
		olderWithBodyMatch.matchWeight != newestEquivalent.matchWeight {
		t.Fatalf("ranking preconditions changed: exact=%t older terms/weight=%d/%d newest=%d/%d",
			exact.explicitIdentity, olderWithBodyMatch.matchedTerms, olderWithBodyMatch.matchWeight,
			newestEquivalent.matchedTerms, newestEquivalent.matchWeight)
	}

	sortCandidatesForRequest(candidates, true)
	want := []*ContextItem{exact, newestComplete, newestEquivalent, olderWithBodyMatch, newerBodyOnly}
	if !slices.Equal(candidates[:len(want)], want) {
		got := make([]string, 0, len(want))
		for _, candidate := range candidates[:len(want)] {
			got = append(got, candidate.URI)
		}
		t.Fatalf("ranked URIs = %q; want exact title, complete direct coverage, newest equivalent direct match, older body-assisted match, then body-only mention",
			got)
	}
}

func TestRankingV6CollapsesExactURLToRichestEvidence(t *testing.T) {
	const (
		title = "Team sync - Notes by Gemini"
		url   = "https://docs.example.test/document/notes-1"
	)
	driveChange := &ContextItem{
		URI: "events/2026-09-02/drive-changes.json#notes-1", Kind: "record", Source: "drive-changes",
		Date: "2026-09-02", Time: "2026-09-02T11:23:18Z", Title: title, URL: url,
		Fields: map[string][]string{"owner_email": {"owner@example.test"}},
	}
	currentFile := &ContextItem{
		URI: "index/drive-files.json#notes-1", Kind: "record", Source: "drive-files",
		Time: "2026-09-02T11:23:18Z", Title: title, URL: url,
		Fields: map[string][]string{
			"editor_email": {"editor@example.test"},
			"owner_email":  {"owner@example.test"},
		},
	}
	meetingNotes := &ContextItem{
		URI: "events/2026-09-02/meeting-notes.json#notes-1", Kind: "record", Source: "meeting-notes",
		Date: "2026-09-02", Time: "2026-09-02T11:22:43Z", Title: title, URL: url,
		Fields: map[string][]string{
			"attachment": {"document:example.test/notes-1"},
			"meeting":    {"events/2026-09-02/calendar.json#meeting-1"},
			"modified":   {"2026-09-02T11:23:18Z"},
			"owner":      {"person:email/owner@example.test"},
		},
		body: "Full meeting notes",
	}

	set := newContextCandidateSet(
		[]*ContextItem{driveChange, currentFile, meetingNotes}, nil, nil, nil, 3,
		[]string{"meeting", "notes"}, LexicalIndexUse{}, "inputs",
	)
	if len(set.candidates) != 1 {
		t.Fatalf("candidate count = %d, want one exact-resource representation", len(set.candidates))
	}
	winner := set.candidates[0]
	if winner.URI != meetingNotes.URI || winner.Count != 3 ||
		!slices.Equal(winner.collapsedURIs, []string{driveChange.URI, meetingNotes.URI, currentFile.URI}) {
		t.Fatalf("collapsed candidate = %+v, members = %q; want the richest meeting-notes evidence",
			winner, winner.collapsedURIs)
	}
}

func TestRankingV6DocumentFrequencyAndLogRarity(t *testing.T) {
	if got := rarityFactor(10, 6); got != 0 {
		t.Fatalf("rarity for a term in more than half the candidates = %d, want stop value 0", got)
	}
	if got := rarityFactor(16, 1); got <= rarityFactor(16, 4) {
		t.Fatalf("rare term factor = %d, want greater than common factor %d", got, rarityFactor(16, 4))
	}
	if got := rarityFactor(1<<20, 1); got != maxRarityFactor {
		t.Fatalf("rarity cap = %d, want %d", got, maxRarityFactor)
	}
}

func TestRankingV6ExponentialRecency(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		date, name string
		halfLife   int
		want       int
	}{
		{"2026-09-02", "same day", 14, pointsRecencyMax},
		{"2026-08-19", "one half-life", 14, 8},
		{"2026-08-05", "two half-lives", 14, 4},
		{"2026-09-02", "policy off", 0, 0},
		{"", "undated", 14, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := recencyBonus(test.date, now, test.halfLife)
			if got != test.want {
				t.Fatalf("recencyBonus(%q, half-life %d) = %d, want %d", test.date, test.halfLife, got, test.want)
			}
		})
	}
}

func TestRankingV6IdentifierScopeAndEntitySuffix(t *testing.T) {
	schema := core.FieldSchema{
		core.FieldID:    {Description: "id", Cardinality: core.CardinalityOne},
		core.FieldTitle: {Description: "title", Cardinality: core.CardinalityOptional},
		"topic":         {Description: "topic", Cardinality: core.CardinalityOptional},
		"people":        {Description: "people", Cardinality: core.CardinalityMany, Relation: true},
	}
	item := recordCandidate(FindRecord{
		URI: "events/2026-09-02/needle-source.json#opaque-1", Source: "needle-source",
		Title: "Canonical design", Fields: map[string][]string{
			"topic": {"ordinary-value"}, "people": {"person:email/marc@x.test"},
		},
	}, schema)
	if candidateMatchesTerm(item, "needle-source") {
		t.Fatal("source name leaked into the lexical haystack")
	}
	if candidateMatchesTerm(item, "people") || candidateMatchesTerm(item, "topic") {
		t.Fatal("field name leaked into the lexical haystack")
	}
	if candidateNamesIdentifier(item, "ordinary-value") {
		t.Fatal("an arbitrary non-relation field received the identifier bonus")
	}
	for _, exact := range []string{"opaque-1", "Canonical design", "person:email/marc@x.test", "marc@x.test"} {
		if !candidateNamesIdentifier(item, exact) {
			t.Errorf("%q did not receive the exact identifier match", exact)
		}
	}
}

func TestRankingV6WeightsNormalizeEachField(t *testing.T) {
	short := &ContextItem{}
	short.addSegment("topic", "ranking", 8)
	long := &ContextItem{}
	long.addSegment("topic", "ranking "+strings.Repeat("background ", 63), 1)
	shortPoints, _ := weightedTermPoints(short, "ranking", 2)
	longPoints, _ := weightedTermPoints(long, "ranking", 2)
	if shortPoints <= longPoints {
		t.Fatalf("weighted short field = %d, long ordinary field = %d; want configured weight and length normalization", shortPoints, longPoints)
	}
}

func TestRankingV6StopRuleOverridesCommonExactValues(t *testing.T) {
	items := []*ContextItem{{}, {}, {}}
	for _, item := range items[:2] {
		item.addSegment(core.FieldTitle, "common", core.DefaultTitleFieldWeight)
	}
	items[2].addSegment(core.FieldTitle, "different", core.DefaultTitleFieldWeight)
	scoreCandidates(items, "common", []string{"common"}, time.Now(), &core.Config{Sources: map[string]*core.Source{}})
	if items[0].Score != 0 || items[1].Score != 0 {
		t.Fatalf("common exact scores = %d, %d; a term in more than half the candidates must score zero", items[0].Score, items[1].Score)
	}
}

func TestRankingV6CollapsesRunsAndCapsSourceShare(t *testing.T) {
	run := []*ContextItem{
		{URI: "new", Kind: "record", Source: "ci", Title: "main", Time: "2026-09-02T10:00:00Z"},
		{URI: "old", Kind: "record", Source: "ci", Title: "main", Time: "2026-09-01T10:00:00Z"},
		{URI: "older", Kind: "record", Source: "ci", Title: "MAIN", Time: "2026-08-31T10:00:00Z"},
	}
	collapsed := collapseContextCandidates(run)
	if len(collapsed) != 1 || collapsed[0].URI != "new" || collapsed[0].Count != 3 {
		t.Fatalf("collapsed = %+v, want newest representative with count 3", collapsed)
	}

	var candidates []*ContextItem
	for index := range 6 {
		candidates = append(candidates, &ContextItem{
			URI: "ci-" + string(rune('a'+index)), Kind: "record", Source: "ci", Score: 100 - index, Tokens: 20,
		})
	}
	candidates = append(candidates,
		&ContextItem{URI: "mail-a", Kind: "record", Source: "mail", Score: 90, Tokens: 20},
		&ContextItem{URI: "wiki/concept.md", Kind: string(core.LayerWiki), Score: 80, Tokens: 20},
	)
	pack := &ContextPack{Receipt: Receipt{Dropped: []DroppedItem{}}}
	selectWithinBudget(pack, candidates, ContextRequest{Budget: 4096})
	counts := map[string]int{}
	for _, item := range pack.Items {
		counts[item.Source]++
	}
	limit := max(1, (len(pack.Items)*2+4)/5)
	if counts["ci"] > limit {
		t.Fatalf("selected %d/%d CI items; 40 percent ceiling is %d", counts["ci"], len(pack.Items), limit)
	}
	if !slices.ContainsFunc(pack.Items, func(item ContextItem) bool { return item.URI == "wiki/concept.md" }) {
		t.Fatal("authored page reservation did not admit the relevant concept page")
	}
}

func TestRankingV6ReservesAuthoredPagesInsideTopTen(t *testing.T) {
	candidates := make([]*ContextItem, 0, 12)
	for index := range 10 {
		candidates = append(candidates, &ContextItem{
			URI:  "tasks/2026-09-02/kagglathon-" + string(rune('a'+index)) + "/TASKS.md",
			Kind: string(core.LayerTasks), Score: 200 - index, Title: "Kagglathon session",
		})
	}
	candidates = append(candidates,
		&ContextItem{URI: "wiki/repositories.md", Kind: string(core.LayerWiki), Score: 100, Title: "Repositories"},
		&ContextItem{URI: "projects/kagglathon.md", Kind: string(core.LayerProjects), Score: 90, Title: "Kagglathon"},
	)
	pack := &ContextPack{Receipt: Receipt{Terms: []string{"kagglathon"}, Dropped: []DroppedItem{}}}
	selectWithinBudget(pack, candidates, ContextRequest{Budget: 4096})
	top := pack.Items[:min(10, len(pack.Items))]
	pages := 0
	for index := range top {
		if isPinnable(&top[index]) {
			pages++
		}
	}
	if pages < 2 {
		t.Fatalf("top ten = %+v, want the reserved one-fifth wiki/project share", top)
	}
	if !slices.ContainsFunc(top, func(item ContextItem) bool { return item.URI == "wiki/repositories.md" }) {
		t.Fatalf("top ten = %+v, want wiki/repositories.md protected from task-page crowd-out", top)
	}
}

func TestRankingV6UsesOnlyVerifiedCachedBodies(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Unique cached narrative"); err != nil {
		t.Fatal(err)
	}
	item := &ContextItem{URI: uri, Kind: "record"}
	consulted, err := attachCachedContextBodies(t.Context(), base, []*ContextItem{item})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(consulted, []string{uri}) || !candidateMatchesTerm(item, "narrative") {
		t.Fatalf("consulted = %q, haystack = %q; cached body did not participate", consulted, item.haystack)
	}
	if len(item.segments) != 1 || item.segments[0].Weight != core.DefaultFieldWeight {
		t.Fatalf("body segments = %+v, want one weight-1 segment", item.segments)
	}
}

func TestContextCandidateSetReportsOnlyRelevantCachedBodies(t *testing.T) {
	relevant := &ContextItem{
		URI:           "events/2026-05-04/notes.json#current",
		collapsedURIs: []string{"events/2026-05-04/notes.json#current", "events/2026-05-04/notes.json#duplicate"},
	}
	set := newContextCandidateSet(
		[]*ContextItem{relevant}, nil,
		[]string{
			"events/2026-05-04/notes.json#current",
			"events/2026-05-04/notes.json#duplicate",
			"events/2026-05-04/notes.json#unrelated",
		},
		nil, 2, []string{"notes"}, LexicalIndexUse{}, "",
	)
	want := []string{
		"events/2026-05-04/notes.json#current",
		"events/2026-05-04/notes.json#duplicate",
	}
	if !slices.Equal(set.consultedBodies, want) {
		t.Fatalf("consulted bodies = %q, want only relevant collapsed candidates %q", set.consultedBodies, want)
	}
}

func TestRankingV6CategoryVisibilityDefaultsAreExplicitAndDeterministic(t *testing.T) {
	makeCandidates := func() []*ContextItem {
		roles := []map[string][]string{
			{core.FieldCategory: {"created"}, core.FieldVisibility: {"shared"}},
			{core.FieldCategory: {"received"}, core.FieldVisibility: {"shared"}},
			{core.FieldCategory: {"saved"}, core.FieldVisibility: {"private"}},
		}
		items := make([]*ContextItem, 0, 6)
		for index, fields := range roles {
			items = append(items, recordCandidate(FindRecord{
				URI:    "events/2026-09-02/notes.json#role-" + string(rune('a'+index)),
				Source: "notes", Title: "Design review", Fields: fields,
			}, nil))
		}
		for index := range 3 {
			items = append(items, recordCandidate(FindRecord{
				URI:    "events/2026-09-02/other.json#other-" + string(rune('a'+index)),
				Source: "other", Title: "Unrelated evidence",
			}, nil))
		}
		return items
	}
	config := &core.Config{Sources: map[string]*core.Source{
		"notes": {Name: "notes"}, "other": {Name: "other"},
	}}
	generic := makeCandidates()
	scoreCandidates(generic, "design", []string{"design"}, time.Now(), config)
	pack := &ContextPack{Receipt: Receipt{Terms: []string{"design"}, Dropped: []DroppedItem{}}}
	selectWithinBudget(pack, generic, ContextRequest{Budget: 4096})
	if slices.ContainsFunc(pack.Items, func(item ContextItem) bool {
		return item.defaultExcluded != ""
	}) {
		t.Fatalf("generic selection exposed received/private evidence: %+v", pack.Items)
	}
	created := slices.IndexFunc(pack.Items, func(item ContextItem) bool { return item.createdEvidence })
	if created < 0 {
		t.Fatal("created evidence was not favored into the default selection")
	}

	explicit := makeCandidates()
	terms := []string{"design", "visibility:private"}
	scoreCandidates(explicit, "design visibility:private", terms, time.Now(), config)
	explicitPack := &ContextPack{Receipt: Receipt{Terms: terms, Dropped: []DroppedItem{}}}
	selectWithinBudget(explicitPack, explicit, ContextRequest{Budget: 4096})
	if !slices.ContainsFunc(explicitPack.Items, func(item ContextItem) bool {
		return item.defaultExcluded == core.FieldVisibility+":private"
	}) {
		t.Fatalf("explicit visibility query did not recover private evidence: %+v", explicitPack.Items)
	}

	digestA := inputDigest(ContextRequest{Query: "design"}, generic, "2026-09-02", nil, nil, nil)
	digestB := inputDigest(ContextRequest{Query: "design"}, generic, "2026-09-02", nil, nil, nil)
	if digestA == "" || digestA != digestB {
		t.Fatalf("policy-bound receipt digest = %q then %q, want deterministic", digestA, digestB)
	}
}

func TestRankingV6TruncatesInboundHubsToNewestEdgesInWindow(t *testing.T) {
	entity := "person:email/marc@x.test"
	edges := make([]Edge, 0, expansionEdgeLimit+2)
	for index := range expansionEdgeLimit + 1 {
		date := time.Date(2026, time.September, 1, 0, 0, index, 0, time.UTC).Format(time.RFC3339)
		edges = append(edges, Edge{Src: "record:" + date, Dst: entity, Kind: "person", At: date, Via: "field:person"})
	}
	edges = append(edges, Edge{Src: "record:old", Dst: entity, Kind: "person", At: "2025-01-01", Via: "field:person"})
	path := filepath.Join(t.TempDir(), "graph.tsv")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := EncodeEdges(file, edges); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	selected, truncated, err := newestInboundExpansionEdges(t.Context(), &validatedGraphCache{file: file}, entity, Window{
		Since: "2026-09-01", Until: "2026-09-02",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(selected) != expansionEdgeLimit {
		t.Fatalf("selected %d, truncated %t; want newest %d with truncation", len(selected), truncated, expansionEdgeLimit)
	}
	if selected[0].At <= selected[len(selected)-1].At {
		t.Fatalf("edges are not newest first: first %q, last %q", selected[0].At, selected[len(selected)-1].At)
	}
}

func TestContextValidityAndSupersedesChooseNewestValidKnowledge(t *testing.T) {
	old := pageCandidate(Page{URI: "wiki/old.md", Slug: "old", Title: "Old policy"}, string(core.LayerWiki), nil)
	first := pageCandidate(Page{
		URI: "wiki/first.md", Slug: "first", Title: "First policy", ValidFrom: "2026-01-01",
		Relations: map[string][]string{"supersedes": {"wiki/old.md"}},
	}, string(core.LayerWiki), nil)
	winner := pageCandidate(Page{
		URI: "wiki/winner.md", Slug: "winner", Title: "Current policy", ValidFrom: "2026-06-01",
		Relations: map[string][]string{"supersedes": {"wiki/old.md"}},
	}, string(core.LayerWiki), nil)
	applyContextSupersedes([]*ContextItem{old, first, winner})
	if old.supersededBy != winner.URI || first.supersededBy != winner.URI || winner.supersededBy != "" {
		t.Fatalf("supersedes result old=%q first=%q winner=%q", old.supersededBy, first.supersededBy, winner.supersededBy)
	}
	if (Page{ValidFrom: "2026-09-04"}).ValidAt("2026-09-03") ||
		(Page{ValidUntil: "2026-09-02"}).ValidAt("2026-09-03") {
		t.Fatal("future or expired authored page is valid at the retrieval day")
	}
}
