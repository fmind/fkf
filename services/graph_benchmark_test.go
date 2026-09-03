package services

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkGraphNeighbourSeek(b *testing.B) {
	const edgeCount = 100_000
	indexed := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	edges := make([]Edge, 0, edgeCount)
	for index := 0; index < edgeCount-1; index++ {
		edges = append(edges, Edge{
			Src: fmt.Sprintf("repo:example/noise-%06d", index), Dst: "tag:noise",
			Kind: EdgeTag, Via: "benchmark", Indexed: indexed,
		})
	}
	edges = append(edges, Edge{
		Src: "repo:example/target", Dst: "tag:signal", Kind: EdgeTag, Via: "benchmark", Indexed: indexed,
	})
	artifacts, err := encodeGraphArtifacts(edges)
	if err != nil {
		b.Fatal(err)
	}
	directory := b.TempDir()
	open := func(name string, data []byte) *os.File {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			b.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = file.Close() })
		return file
	}
	cache := &validatedGraphCache{
		file: open("src.tsv", artifacts.src), dst: open("dst.tsv", artifacts.dst),
		offsets: open("offsets.tsv", artifacts.offsets),
	}
	query := EdgeQuery{Src: "repo:example/target"}

	b.Run("indexed", func(b *testing.B) {
		for b.Loop() {
			stats, err := cache.scan(b.Context(), query, nil)
			if err != nil || stats.Lines != 1 {
				b.Fatalf("indexed scan = %+v, %v", stats, err)
			}
		}
		b.ReportMetric(1, "rows/op")
	})
	b.Run("full-scan", func(b *testing.B) {
		for b.Loop() {
			if _, err := cache.file.Seek(0, 0); err != nil {
				b.Fatal(err)
			}
			stats, err := ScanEdges(b.Context(), cache.file, query, nil)
			if err != nil || stats.Lines != edgeCount {
				b.Fatalf("full scan = %+v, %v", stats, err)
			}
		}
		b.ReportMetric(edgeCount, "rows/op")
	})
}
