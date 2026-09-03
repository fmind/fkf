package services_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestBuildGraphWritesExactPerFileManifestsAndSeekArtifacts(t *testing.T) {
	base := graphManifestBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(base.Root(), core.GraphMetaFile))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		SchemaVersion int `json:"schema_version"`
		Inputs        []struct {
			URI              string `json:"uri"`
			Bytes            int64  `json:"bytes"`
			ModifiedUnixNano int64  `json:"modified_unix_nano"`
			SHA256           string `json:"sha256"`
		} `json:"inputs"`
		Outputs []struct {
			URI              string `json:"uri"`
			Bytes            int64  `json:"bytes"`
			ModifiedUnixNano int64  `json:"modified_unix_nano"`
			SHA256           string `json:"sha256"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SchemaVersion != 3 {
		t.Fatalf("graph metadata schema = %d, want 3", metadata.SchemaVersion)
	}
	if len(metadata.Inputs) < 5 {
		t.Fatalf("graph input manifest = %+v, want every collected and authored input", metadata.Inputs)
	}
	assertExactGraphFileManifest(t, base.Root(), metadata.Inputs)
	if len(metadata.Outputs) != 3 {
		t.Fatalf("graph output manifest = %+v, want primary, dst twin, and offsets", metadata.Outputs)
	}
	assertExactGraphFileManifest(t, base.Root(), metadata.Outputs)
}

func TestGraphWalkSeeksOnlyTheMatchingEdgeRange(t *testing.T) {
	base := graphBase(t)
	build, err := services.BuildGraph(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	indexed := base.Now().UTC().Format(time.RFC3339)
	edges := make([]services.Edge, 0, 1001)
	for index := 0; index < 1000; index++ {
		edges = append(edges, services.Edge{
			Src: fmt.Sprintf("repo:example/unrelated-%04d", index), Dst: "tag:noise",
			Kind: services.EdgeTag, Via: "test", Indexed: indexed,
		})
	}
	edges = append(edges, services.Edge{
		Src: "repo:example/target", Dst: "tag:signal", Kind: services.EdgeTag,
		Via: "test", Indexed: indexed,
	})
	metadata, err := services.NewEdgeListMeta(edges, base.Now(), build.Meta.SHA256.Inputs, build.Meta.Inputs...)
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
	if err := services.WriteEdgeList(graphPath, metaPath, edges, metadata); err != nil {
		t.Fatal(err)
	}
	result, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "repo:example/target", Direction: services.DirectionOut, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Dst != "tag:signal" {
		t.Fatalf("neighbourhood = %+v, want the target edge", result.Edges)
	}
	if result.Stats.Lines != 1 {
		t.Fatalf("walk scanned %d rows for one contiguous source range; want exactly 1", result.Stats.Lines)
	}
}

func TestVerifyGraphHashesInputsEvenWhenTheirStatsMatch(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(base.Root(), "wiki", "retrieval-boundary.md")
	info, err := os.Stat(page)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(string(data), "Retrieval boundary", "Retrieval boundarz", 1))
	if len(changed) != len(data) || string(changed) == string(data) {
		t.Fatal("fixture did not make a same-size input change")
	}
	if err := os.WriteFile(page, changed, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(page, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := services.SummarizeGraph(t.Context(), base); err != nil {
		t.Fatalf("ordinary stat-first validation unexpectedly hashed an unchanged fingerprint: %v", err)
	}
	if _, err := services.VerifyGraph(t.Context(), base); err == nil || !strings.Contains(err.Error(), "wiki input changed") {
		t.Fatalf("VerifyGraph() error = %v, want full input digest mismatch", err)
	}
}

func assertExactGraphFileManifest(t *testing.T, root string, entries []struct {
	URI              string `json:"uri"`
	Bytes            int64  `json:"bytes"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
	SHA256           string `json:"sha256"`
},
) {
	t.Helper()
	previous := ""
	for _, entry := range entries {
		if entry.URI <= previous {
			t.Fatalf("graph file manifest is not strictly URI-sorted: %q after %q", entry.URI, previous)
		}
		previous = entry.URI
		absolute := filepath.Join(root, filepath.FromSlash(entry.URI))
		data, err := os.ReadFile(absolute)
		if err != nil {
			t.Fatalf("read manifested %s: %v", entry.URI, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatalf("stat manifested %s: %v", entry.URI, err)
		}
		digest := sha256.Sum256(data)
		if entry.Bytes != info.Size() || entry.ModifiedUnixNano != info.ModTime().UnixNano() ||
			entry.SHA256 != hex.EncodeToString(digest[:]) {
			t.Errorf("manifest[%s] = %+v, want exact size, mtime, and SHA-256", entry.URI, entry)
		}
	}
}
