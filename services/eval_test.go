package services_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

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
	if len(report.Queries[0].TopURIs) == 0 || report.Queries[0].InputDigest == "" {
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

func TestEvalFailsClosedOnMissesForbiddenHitsAndUnknownFields(t *testing.T) {
	base := contextBase(t)
	writeEvalSuite(t, base, `fkf: 1
k: 1
recall_threshold: 1
queries:
  - name: deliberately-failing
    question: Retrieval boundary
    window: {since: 2026-05-04, until: 2026-05-05}
    expected_uris: [projects/fkf-rebuild.md]
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
	if query.Passed || query.Recall != 0 || len(query.MissingExpected) != 1 || len(query.ForbiddenFound) != 1 {
		t.Fatalf("query = %+v, want both the miss and forbidden hit named", query)
	}

	writeEvalSuite(t, base, "fkf: 1\nk: 3\nrecall_threshold: 1\nunknown: true\nqueries: []\n")
	_, err = services.Evaluate(t.Context(), base)
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Evaluate() error = %v, want strict unknown-field configuration error", err)
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
