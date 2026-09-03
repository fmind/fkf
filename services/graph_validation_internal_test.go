package services

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOrdinaryGraphValidationHashesOnlyStatChangedInputs(t *testing.T) {
	base := statusDocumentBase(t)
	if _, err := BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	meta, err := readGraphMeta(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	hashes := 0
	hasher := func(ctx context.Context, base *Base, uri string) (GraphFileManifest, error) {
		hashes++
		return hashGraphInput(ctx, base, uri)
	}
	if problems := currentGraphInputProblemsWithOptions(
		t.Context(), base, meta, graphValidationOptions{hashInput: hasher},
	); len(problems) != 0 {
		t.Fatalf("unchanged graph inputs = %v", problems)
	}
	if hashes != 0 {
		t.Fatalf("ordinary validation hashed %d unchanged inputs, want 0", hashes)
	}

	changed := meta.Inputs[len(meta.Inputs)/2]
	absolute, err := base.Store.Resolve(changed.URI)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime().Add(time.Second)
	if err := os.Chtimes(absolute, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if problems := currentGraphInputProblemsWithOptions(
		t.Context(), base, meta, graphValidationOptions{hashInput: hasher},
	); len(problems) != 0 {
		t.Fatalf("mtime-only input change = %v, want content-equivalent", problems)
	}
	if hashes != 1 {
		t.Fatalf("ordinary validation hashed %d inputs after one stat changed, want 1", hashes)
	}
}
