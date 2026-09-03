package services_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestFindBoundedSearchesTaskPagesAndIndexRecordsAcrossAContinuation(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "tasks/2026-05-04/memory/TASKS.md", `---
title: Needle task trace
---

# Needle task trace

The task preserves bounded retrieval evidence.
`)
	ignored := filepath.Join(base.Root(), "tasks", "2026-05-04", "no-trace", "README.md")
	if err := os.MkdirAll(filepath.Dir(ignored), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignored, []byte("needle but not TASKS.md\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	writeIndexDocument(t, base, "catalog", []sources.Record{
		{"id": "one", "title": "Needle index record"},
		{"id": "two", "title": "Unrelated index record"},
	})

	filter := services.FindFilter{
		Grep: []string{"needle"}, Layers: []core.Layer{core.LayerTasks, core.LayerIndex},
	}
	first, err := services.FindBounded(t.Context(), base, filter, false, 1, services.FindPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.Pages) != 1 || first.Result.Pages[0].URI != "tasks/2026-05-04/memory/TASKS.md" ||
		len(first.Result.Records) != 0 || first.Next == nil || first.Next.Phase != services.FindPhasePage {
		t.Fatalf("first bounded page = %+v, next=%+v", first.Result, first.Next)
	}
	second, err := services.FindBounded(t.Context(), base, filter, false, 1, *first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.Pages) != 0 || len(second.Result.Records) != 1 ||
		second.Result.Records[0].URI != "index/catalog.json#one" || second.Next != nil {
		t.Fatalf("second bounded page = %+v, next=%+v", second.Result, second.Next)
	}
	if second.SnapshotSHA256 != first.SnapshotSHA256 {
		t.Fatalf("snapshot changed across task/index continuation: %s != %s", second.SnapshotSHA256, first.SnapshotSHA256)
	}

	counts, err := services.FindBounded(t.Context(), base, filter, true, 10, services.FindPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(counts.Result.Pages) != 0 || len(counts.Result.Volumes) != 1 ||
		counts.Result.Volumes[0].Total != 1 || counts.Result.Volumes[0].Sources[0].Source != "catalog" {
		t.Fatalf("bounded count = %+v", counts.Result)
	}
}
