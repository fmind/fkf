package services

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
)

const internalTestContract = `fkf: 1
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
`

func TestGraphSnapshotRefusesAFIFOBeforeOpenCanBlock(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	graphPath, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(graphPath), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(graphPath, uint32(core.BaseFileMode)); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphGenerationState(
		filepath.Join(root, core.GraphGenerationFile), graphGenerationCurrent, strings.Repeat("0", 64),
	); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = SummarizeGraph(t.Context(), base)
	if !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("FIFO graph error = %v, want ErrUnsafePath", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO graph refusal took %s; inspection must reject it before open", elapsed)
	}
}

func TestValidatedGraphCacheKeepsOneFileGenerationAcrossScans(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	oldEdge := Edge{Src: "tag:old", Dst: "tag:snapshot", Kind: EdgeTag, Via: "frontmatter:tags"}
	writeSnapshotTestGraph(t, base, []Edge{oldEdge}, time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC))

	cache, _, _, err := openValidatedGraphCache(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.close() }()

	newEdge := Edge{Src: "tag:new", Dst: "tag:generation", Kind: EdgeTag, Via: "frontmatter:tags"}
	writeSnapshotTestGraph(t, base, []Edge{newEdge}, time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC))
	var got []Edge
	if _, err := cache.scan(t.Context(), EdgeQuery{}, func(edge Edge) error {
		got = append(got, edge)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Src != oldEdge.Src {
		t.Fatalf("snapshot scan = %+v, want only the validated old generation", got)
	}
}

func TestValidatedGraphCacheRejectsSameStatArtifactsFromAnotherGeneration(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	writeSnapshotTestGraph(t, base, []Edge{{
		Src: "tag:aaa", Dst: "tag:one", Kind: EdgeTag, Via: "frontmatter:tags",
	}}, generatedAt)
	metaPath, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	firstMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Equal-length identifiers and one generated_at preserve the output size and mtime. Restoring
	// only the first metadata file simulates a reader observing a mixed multi-rename generation.
	writeSnapshotTestGraph(t, base, []Edge{{
		Src: "tag:bbb", Dst: "tag:two", Kind: EdgeTag, Via: "frontmatter:tags",
	}}, generatedAt)
	if err := os.WriteFile(metaPath, firstMeta, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	_, err = SummarizeGraph(t.Context(), base)
	if err == nil || !strings.Contains(err.Error(), "generation marker does not match metadata") {
		t.Fatalf("mixed graph generation error = %v, want generation-marker rejection", err)
	}
}

func TestNeighbourQueriesReuseOneValidatedGraphGeneration(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	oldEdges := []Edge{
		{Src: "tag:peer", Dst: "tag:shared", Kind: EdgeTag, Via: "frontmatter:tags"},
		{Src: "tag:seed", Dst: "tag:shared", Kind: EdgeTag, Via: "frontmatter:tags"},
	}
	writeSnapshotTestGraph(t, base, oldEdges, time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC))
	cache, _, meta, err := openValidatedGraphCache(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.close() }()

	writeSnapshotTestGraph(t, base, []Edge{
		{Src: "tag:new", Dst: "tag:generation", Kind: EdgeTag, Via: "frontmatter:tags"},
	}, time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC))
	out, err := neighboursFromCache(t.Context(), cache, GraphQuery{
		URI: "tag:seed", Direction: DirectionOut, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := neighboursFromCache(t.Context(), cache, GraphQuery{
		URI: "tag:shared", Direction: DirectionIn, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.SnapshotSHA256 != meta.SHA256.Outputs.GraphTSV || in.SnapshotSHA256 != meta.SHA256.Outputs.GraphTSV ||
		len(out.Edges) != 1 || out.Edges[0].Dst != "tag:shared" || len(in.Edges) != 2 {
		t.Fatalf("out = %+v, in = %+v; want both queries bound to the validated old generation", out, in)
	}
	if err := cache.revalidateBytes(t.Context()); err != nil {
		t.Fatalf("atomic path replacement changed the open generation: %v", err)
	}
}

func TestValidatedGraphCacheRejectsAnInPlaceChangeDuringTheRead(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	oldEdge := Edge{Src: "tag:old", Dst: "tag:snapshot", Kind: EdgeTag, Via: "frontmatter:tags"}
	writeSnapshotTestGraph(t, base, []Edge{oldEdge}, generatedAt)
	cache, _, _, err := openValidatedGraphCache(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.close() }()

	newEdge := Edge{
		Src: "tag:new", Dst: "tag:generation", Kind: EdgeTag,
		Via: "frontmatter:tags", Indexed: generatedAt.Format(time.RFC3339),
	}
	var replacement bytes.Buffer
	if err := EncodeEdges(&replacement, []Edge{newEdge}); err != nil {
		t.Fatal(err)
	}
	graphPath, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphPath, replacement.Bytes(), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.scan(t.Context(), EdgeQuery{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := cache.revalidateBytes(t.Context()); err == nil {
		t.Fatal("an in-place graph change during the read was accepted")
	}
}

func TestScanValidatedGraphCacheRejectsAnInPlaceChangeBeforeReturning(t *testing.T) {
	root := t.TempDir()
	config := internalTestContract + "name: snapshot\nlayers: {events: true, index: true, tasks: true, projects: true, wiki: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	oldEdge := Edge{Src: "tag:old", Dst: "tag:cache", Kind: EdgeTag, Via: "frontmatter:tags"}
	writeSnapshotTestGraph(t, base, []Edge{oldEdge}, generatedAt)

	newEdge := Edge{
		Src: "tag:new", Dst: "tag:cache", Kind: EdgeTag,
		Via: "frontmatter:tags", Indexed: generatedAt.Format(time.RFC3339),
	}
	var replacement bytes.Buffer
	if err := EncodeEdges(&replacement, []Edge{newEdge}); err != nil {
		t.Fatal(err)
	}
	graphPath, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	mutated := false
	_, err = scanValidatedGraphCache(t.Context(), base, func(Edge) error {
		if mutated {
			return nil
		}
		mutated = true
		file, err := os.OpenFile(graphPath, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if _, err := file.WriteAt(replacement.Bytes(), 0); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Chtimes(graphPath, generatedAt.Add(time.Second), generatedAt.Add(time.Second))
	})
	if !mutated {
		t.Fatal("validated graph scan did not visit its row")
	}
	if err == nil || !strings.Contains(err.Error(), "graph.tsv changed during the read") {
		t.Fatalf("scanValidatedGraphCache() error = %v, want in-place mutation rejection", err)
	}
}

func writeSnapshotTestGraph(t *testing.T, base *Base, edges []Edge, generatedAt time.Time) {
	t.Helper()
	inputs, err := readGraphInputState(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	for index := range edges {
		edges[index].Indexed = generatedAt.Format(time.RFC3339)
	}
	meta, err := NewEdgeListMeta(edges, generatedAt, inputs.SHA256, inputs.Files...)
	if err != nil {
		t.Fatal(err)
	}
	graphPath, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	metaPath, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(graphPath), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := WriteEdgeList(graphPath, metaPath, edges, meta); err != nil {
		t.Fatal(err)
	}
}
