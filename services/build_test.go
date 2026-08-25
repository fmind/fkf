package services_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestBuildRunsGraphAndWiki(t *testing.T) {
	base := graphBase(t)
	report, err := services.Build(t.Context(), base, "all", false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Graph == nil {
		t.Fatal("report.Graph is nil")
	}
	if report.Wiki == nil {
		t.Fatal("report.Wiki is nil")
	}
	graphPath := filepath.Join(base.Root(), core.GraphFile)
	if _, err := os.Stat(graphPath); err != nil {
		t.Fatalf("graph.tsv was not built: %v", err)
	}
	if _, err := services.SummarizeGraph(t.Context(), base); err != nil {
		t.Fatalf("Build(all) left its graph stale after writing the wiki index: %v", err)
	}
}

func TestBuildTargetSpecific(t *testing.T) {
	base := graphBase(t)
	graphOnly, err := services.Build(t.Context(), base, "graph", false)
	if err != nil {
		t.Fatal(err)
	}
	if graphOnly.Graph == nil || graphOnly.Wiki != nil {
		t.Fatalf("graphOnly = %+v, want only Graph", graphOnly)
	}

	wikiOnly, err := services.Build(t.Context(), base, "wiki", false)
	if err != nil {
		t.Fatal(err)
	}
	if wikiOnly.Wiki == nil || wikiOnly.Graph != nil {
		t.Fatalf("wikiOnly = %+v, want only Wiki", wikiOnly)
	}
}
