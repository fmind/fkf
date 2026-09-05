package services_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestEvalMeasuresEachConsumerDelivery(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 3
budget: 850
delivery: text
recall_threshold: 1
queries:
  - name: startup
    question: Retrieval boundary
    expected_uris: [wiki/retrieval-boundary.md]
  - name: compact
    question: Retrieval boundary
    budget: 600
    expected_uris: [wiki/retrieval-boundary.md]
  - name: structured
    question: Retrieval boundary
    budget: 1500
    delivery: json
    expected_uris: [wiki/retrieval-boundary.md]
  - name: pipeline
    question: Retrieval boundary
    delivery: jsonl
    expected_uris: [wiki/retrieval-boundary.md]
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	formats := []string{"text", "text", "json", "jsonl"}
	for index, query := range report.Queries {
		pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
			Query: query.Question, Window: query.Window, Budget: query.Budget, DeliveryFormat: formats[index],
		})
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if formats[index] == "text" {
			buffer.WriteString(services.RenderContextText(pack))
		} else {
			encoder := json.NewEncoder(&buffer)
			encoder.SetEscapeHTML(false)
			if formats[index] == "json" {
				encoder.SetIndent("", "  ")
			}
			if err := encoder.Encode(pack); err != nil {
				t.Fatal(err)
			}
		}
		encoded := buffer.Bytes()

		if !query.Passed || query.DeliveredBytes != len(encoded) ||
			query.DeliveredTokens != (len(encoded)+3)/4 || len(encoded) > query.Budget*4 {
			t.Fatalf("%s delivery = %+v, want %d bytes within budget", formats[index], query, len(encoded))
		}
	}
}

func TestEvalRejectsUnknownDelivery(t *testing.T) {
	base := contextBase(t)
	for _, declaration := range []string{"delivery: html\n", ""} {
		body := "fkf: 1\nk: 1\nrecall_threshold: 1\n" + declaration +
			"queries:\n  - name: invalid\n    question: boundary\n    delivery: html\n    expect_empty: true\n"
		writeEvalSuite(t, base, body)
		if _, err := services.Evaluate(t.Context(), base); err == nil || !strings.Contains(err.Error(), "delivery") {
			t.Fatalf("Evaluate() error = %v, want invalid delivery refused", err)
		}
	}
}

func TestEvalRunsDeclaredQueriesAndReportsRecallAtK(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 3
recall_threshold: 1
queries:
  - name: retrieval-boundary
    question: Retrieval boundary
    window:
      since: 2026-05-04
      until: 2026-05-05
    expected_uris: [wiki/retrieval-boundary.md]
    forbidden_uris: [wiki/not-the-answer.md]
`)

	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Failed != 0 || report.K != 3 || report.RecallThreshold != 1 {
		t.Fatalf("report = %+v, want one passing query at recall@3 threshold 1", report)
	}
	if len(report.Queries) != 1 || !report.Queries[0].Passed || report.Queries[0].Recall != 1 {
		t.Fatalf("queries = %+v, want exact expected URI recalled", report.Queries)
	}
	query := report.Queries[0]
	if len(query.DeliveredURIs) == 0 || query.InputDigest == "" || query.Budget != services.DefaultBudget ||
		query.DeliveredBytes == 0 || query.DeliveredTokens == 0 || len(query.ExpectedRanks) != 1 ||
		query.ExpectedRanks[0].Rank < 1 || report.Budget != services.DefaultBudget || report.EvaluationTime.IsZero() {
		t.Fatalf("query = %+v, want ranked URIs and the context receipt digest", report.Queries[0])
	}
}

func TestEvalBindsEveryQueryToOneEvaluationInstant(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 3
recall_threshold: 1
queries:
  - name: retrieval-boundary
    question: Retrieval boundary
    window: {since: today, until: today}
    expected_uris: [wiki/retrieval-boundary.md]
    forbidden_uris: []
`)
	clockReads := 0
	base.Now = func() time.Time {
		clockReads++
		return testClock.AddDate(0, 0, clockReads-1)
	}
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	query := report.Queries[0]
	if clockReads != 1 || query.Window.Since != "2026-05-10" || query.Window.Until != "2026-05-10" {
		t.Fatalf("clock reads = %d, query = %+v; want one shared evaluation instant", clockReads, query)
	}
}

func TestEvalHasIdenticalSemanticResultsWithAndWithoutTheLexicalCache(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 3
budget: 1500
recall_threshold: 1
queries:
  - name: retrieval-boundary
    question: Retrieval boundary
    window: {since: 2026-05-04, until: 2026-05-05}
    expected_uris: [wiki/retrieval-boundary.md]
`)
	fallback, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Queries[0].Index.Used {
		t.Fatalf("fallback index = %+v, want a missing-cache scan", fallback.Queries[0].Index)
	}
	if _, err := services.BuildLexicalIndex(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	indexed, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed.Queries[0].Index.Used {
		t.Fatalf("indexed result = %+v, want cache use", indexed.Queries[0].Index)
	}
	want, got := fallback.Queries[0], indexed.Queries[0]
	if !slices.Equal(got.DeliveredURIs, want.DeliveredURIs) ||
		!slices.Equal(got.MissingExpected, want.MissingExpected) ||
		!slices.Equal(got.ForbiddenFound, want.ForbiddenFound) ||
		!slices.Equal(got.ExpectedRanks, want.ExpectedRanks) || got.InputDigest != want.InputDigest ||
		got.Recall != want.Recall || got.Passed != want.Passed {
		t.Fatalf("indexed and fallback semantics differ\nfallback: %+v\nindexed: %+v", want, got)
	}
}

func TestEvalFailsClosedOnMissesForbiddenHitsAndUnknownFields(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 1
recall_threshold: 1
queries:
  - name: deliberately-failing
    question: Retrieval boundary
    window: {since: 2026-05-04, until: 2026-05-05}
    expected_uris: [events/2026-05-04/synthetic.json#a1]
    forbidden_uris: [wiki/retrieval-boundary.md]
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Failed != 1 || len(report.Queries) != 1 {
		t.Fatalf("report = %+v, want one failed evaluation", report)
	}
	query := report.Queries[0]
	if query.Passed || query.Recall != 0 || len(query.MissingExpected) != 1 || len(query.ForbiddenFound) != 1 ||
		len(query.ExpectedRanks) != 1 || query.ExpectedRanks[0].Rank <= query.K {
		t.Fatalf("query = %+v, want both the miss and forbidden hit named", query)
	}

	writeEvalSuite(t, base, "fkf: 1\nk: 3\nrecall_threshold: 1\nunknown: true\nqueries: []\n")
	_, err = services.Evaluate(t.Context(), base)
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Evaluate() error = %v, want strict unknown-field configuration error", err)
	}
}

func TestEvalFailsWhenExpectedEvidenceRanksTenthAtKThree(t *testing.T) {
	base := contextBase(t)
	for index, suffix := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"} {
		name := string(rune('a' + index))
		write(t, base, "wiki/"+name+".md", "# Quartz ranking "+suffix+"\n\nStable evaluation fixture.\n")
	}
	writeEvalSuite(t, base, `fkf: 1
k: 3
budget: 4096
recall_threshold: 1
queries:
  - name: tenth
    question: quartz ranking
    window: {}
    expected_uris: [wiki/j.md]
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	query := report.Queries[0]
	if query.Passed || query.ExpectedRanks[0].Rank != 10 || query.FoundExpected != 0 {
		t.Fatalf("query = %+v, want delivered rank ten to fail k three", query)
	}
}

func TestEvalUsesTheFinalBudgetedDelivery(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	collect(t, base, "2026-05-04", `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"boundary FK-412"}]`)
	full, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "FK-412", Budget: 4096})
	if err != nil || len(full.Items) == 0 || full.Items[0].URI != "events/2026-05-04/synthetic.json#a1" {
		t.Fatalf("generous context = %+v, %v; want the expected record ranked first", full, err)
	}
	writeEvalSuite(t, base, `fkf: 1
k: 1
budget: 300
recall_threshold: 1
queries:
  - name: delivery-boundary
    question: FK-412
    window: {since: 2026-05-04, until: 2026-05-04}
    expected_uris: [events/2026-05-04/synthetic.json#a1]
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	query := report.Queries[0]
	if query.Passed || query.FoundExpected != 0 || query.ExpectedRanks[0].Rank != 0 ||
		len(query.DeliveredURIs) != 0 || query.DeliveredTokens > query.Budget {
		t.Fatalf("budgeted query = %+v, want the pre-serialization winner absent from delivery", query)
	}
}

func TestEvalSupportsOverridesAndAnExplicitEmptyAnswer(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 10
budget: 1500
recall_threshold: 1
queries:
  - name: narrow
    question: Retrieval boundary
    k: 3
    budget: 3000
    window: {since: 2026-05-04, until: 2026-05-05}
    expected_uris: [wiki/retrieval-boundary.md]
  - name: no-answer
    question: zzz-nothing-matches-zzz
    budget: 600
    window: {since: 2026-05-04, until: 2026-05-05}
    expect_empty: true
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Budget != 1500 || len(report.Queries) != 2 {
		t.Fatalf("report = %+v, want both override cases to pass", report)
	}
	if got := report.Queries[0]; got.K != 3 || got.Budget != 3000 || !got.Passed {
		t.Fatalf("override query = %+v", got)
	}
	if got := report.Queries[1]; !got.ExpectEmpty || got.Recall != 1 || len(got.DeliveredURIs) != 0 || !got.Passed {
		t.Fatalf("empty query = %+v, want finite perfect empty-case score", got)
	}
}

func TestEvalDoesNotMistakeABudgetOmissionForNoAnswer(t *testing.T) {
	base := contextBase(t)
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "Retrieval boundary", Budget: 1})
	var budgetError *services.ContextBudgetError
	if !errors.As(err, &budgetError) {
		t.Fatalf("BuildContext() error = %v, want the exact minimum", err)
	}
	writeEvalSuite(t, base, "fkf: 1\nk: 3\nbudget: "+fmt.Sprint(budgetError.Minimum)+`
recall_threshold: 1
queries:
  - name: false-empty
    question: Retrieval boundary
    window: {since: 2026-05-04, until: 2026-05-05}
    expect_empty: true
`)
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	query := report.Queries[0]
	if query.Passed || query.Recall != 0 || len(query.DeliveredURIs) != 0 ||
		query.DeliveredTokens != budgetError.Minimum {
		t.Fatalf("query = %+v, want an exact-minimum budget omission to fail no-answer", query)
	}
}

func TestEvalRejectsInvalidBudgetsRanksAndEmptyCombinations(t *testing.T) {
	base := contextBase(t)
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "suite budget", body: "budget: -1\n", want: "budget"},
		{name: "zero suite budget", body: "budget: 0\n", want: "budget"},
		{name: "query budget", body: "", want: "budget"},
		{name: "zero query budget", body: "", want: "budget"},
		{name: "query k", body: "", want: ".k"},
		{name: "zero query k", body: "", want: ".k"},
		{name: "empty expected", body: "", want: "expected_uris"},
		{name: "empty conflict", body: "", want: "expect_empty"},
	}
	queries := []string{
		"  - name: invalid\n    question: query\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n    budget: -1\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n    budget: 0\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n    k: 101\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n    k: 0\n    expected_uris: [wiki/index.md]\n",
		"  - name: invalid\n    question: query\n",
		"  - name: invalid\n    question: query\n    expect_empty: true\n    forbidden_uris: [wiki/index.md]\n",
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeEvalSuite(t, base, "fkf: 1\nk: 3\n"+test.body+"recall_threshold: 1\nqueries:\n"+queries[index])
			_, err := services.Evaluate(t.Context(), base)
			if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Evaluate() error = %v, want %q configuration error", err, test.want)
			}
		})
	}
}

func TestEvalRefusesInvalidThresholdsDuplicateURIsAndSymlinks(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 3
recall_threshold: 1.1
queries:
  - name: invalid
    question: Retrieval boundary
    window: {since: 2026-05-04, until: 2026-05-05}
    expected_uris: [wiki/retrieval-boundary.md, wiki/retrieval-boundary.md]
`)
	if _, err := services.Evaluate(t.Context(), base); !errors.Is(err, core.ErrConfig) {
		t.Fatalf("Evaluate() error = %v, want invalid evaluation contract", err)
	}

	evals := filepath.Join(base.Root(), "evals")
	if err := os.RemoveAll(evals); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "queries.yaml"), []byte("fkf: 1\nk: 1\nrecall_threshold: 1\nqueries: []\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, evals); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Evaluate(t.Context(), base); !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("Evaluate() through symlink error = %v, want unsafe path refusal", err)
	}
}

func writeEvalSuite(t *testing.T, base *services.Base, content string) {
	t.Helper()
	directory := filepath.Join(base.Root(), "evals")
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "queries.yaml"), []byte(content), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}
