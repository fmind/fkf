package services_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestBuildIfStaleWritesOnlyWhenADerivedInputChanged(t *testing.T) {
	base := graphBase(t)
	first, err := services.BuildIfStale(t.Context(), base, "all")
	if err != nil || first.NothingStale || first.Graph == nil || first.Wiki == nil {
		t.Fatalf("first BuildIfStale = %+v, %v; want both missing caches built", first, err)
	}
	graphPath := filepath.Join(base.Root(), core.GraphFile)
	metaPath := filepath.Join(base.Root(), core.GraphMetaFile)
	graphBefore, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	metaBefore, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := services.BuildStale(t.Context(), base, "all")
	if err != nil || stale {
		t.Fatalf("BuildStale after build = %t, %v; want current", stale, err)
	}
	second, err := services.BuildIfStale(t.Context(), base, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !second.NothingStale || second.Graph != nil || second.Wiki != nil {
		t.Fatalf("second BuildIfStale = %+v, want a no-write result", second)
	}
	if graphAfter, err := os.ReadFile(graphPath); err != nil || string(graphAfter) != string(graphBefore) {
		t.Fatalf("no-op build changed graph.tsv: %v", err)
	}
	if metaAfter, err := os.ReadFile(metaPath); err != nil || string(metaAfter) != string(metaBefore) {
		t.Fatalf("no-op build changed graph metadata: %v", err)
	}

	page := filepath.Join(base.Root(), string(core.LayerWiki), "retrieval-boundary.md")
	body, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, append(body, []byte("\nchanged\n")...), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	stale, err = services.BuildStale(t.Context(), base, "all")
	if err != nil || !stale {
		t.Fatalf("BuildStale after authored change = %t, %v; want stale", stale, err)
	}
	rebuilt, err := services.BuildIfStale(t.Context(), base, "all")
	if err != nil || rebuilt.NothingStale || rebuilt.Graph == nil || rebuilt.Wiki == nil {
		t.Fatalf("rebuilt = %+v, %v; want both derived stages refreshed", rebuilt, err)
	}
}
