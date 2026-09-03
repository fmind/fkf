package services_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestContextSinceReceiptReturnsOnlyNewAndChangedCandidates(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval", Budget: 4096, SaveSnapshot: true}
	before, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}

	write(t, base, "wiki/retrieval-boundary.md", `---
type: decision
title: Changed retrieval boundary
tags: [retrieval]
---

# Changed retrieval boundary

The durable retrieval rule changed.
`)
	write(t, base, "wiki/retrieval-receipts.md", `---
type: insight
title: Retrieval receipts
tags: [retrieval]
---

# Retrieval receipts

Receipts make retrieval reproducible.
`)

	request.SinceReceipt = before.Receipt.InputDigest
	delta, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(delta.Items))
	for _, item := range delta.Items {
		got = append(got, item.URI)
	}
	slices.Sort(got)
	want := []string{"wiki/retrieval-boundary.md", "wiki/retrieval-receipts.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("delta URIs = %v, want only new and changed %v", got, want)
	}
	if delta.Receipt.SinceReceipt != before.Receipt.InputDigest || delta.Receipt.Changed != 2 ||
		delta.Receipt.Candidates != 2 || delta.Receipt.InputDigest == before.Receipt.InputDigest {
		t.Fatalf("delta receipt = %+v", delta.Receipt)
	}

	request.SinceReceipt = delta.Receipt.InputDigest
	unchanged, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Items) != 0 || unchanged.Receipt.Changed != 0 ||
		!strings.Contains(unchanged.Receipt.Warning, "nothing changed") {
		t.Fatalf("unchanged delta = %+v, want an explicit empty delta", unchanged)
	}
}

func TestContextSinceReceiptDetectsAnOmittedSupersederChangingAMatch(t *testing.T) {
	base := contextBase(t)
	write(t, base, "wiki/legacy.md", `---
type: insight
title: Durable quasar policy
tags: [policy]
---

# Durable quasar policy

The quasar decision remains active.
`)
	request := services.ContextRequest{Query: "quasar", Budget: 4096, SaveSnapshot: true}
	before, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) == 0 || before.Items[0].URI != "wiki/legacy.md" {
		t.Fatalf("initial items = %+v, want the matching legacy page", before.Items)
	}

	write(t, base, "wiki/replacement.md", `---
type: decision
title: Replacement policy
tags: [policy]
valid_from: 2026-05-09
relations:
  supersedes: [wiki/legacy.md]
---

# Replacement policy

This page replaces an older decision without repeating its subject.
`)
	request.SinceReceipt = before.Receipt.InputDigest
	delta, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Items) != 1 || delta.Items[0].URI != "wiki/legacy.md" ||
		delta.Receipt.Changed != 1 {
		t.Fatalf("delta = %+v, want the matching target changed by an omitted superseder", delta)
	}
}

func TestContextSinceReceiptAccountsForAMatchInvalidatedByAnUnmatchedSuperseder(t *testing.T) {
	base := newBase(t, `fkf: 1
name: supersession-delta
schema:
  id: {description: Stable identity., cardinality: one}
  supersedes: {description: Replaced knowledge., cardinality: many}
layers: {events: false, index: false, tasks: false, projects: false, wiki: true}
sources: {}
`, nil)
	write(t, base, "wiki/old-policy.md", `---
type: decision
title: Targetneedle policy
---

# Targetneedle policy

The original policy.
`)
	request := services.ContextRequest{Query: "targetneedle", Budget: 4096, SaveSnapshot: true}
	before, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) != 1 || before.Items[0].URI != "wiki/old-policy.md" {
		t.Fatalf("initial items = %+v, want the matching original policy", before.Items)
	}

	// The replacement deliberately does not match the query. It still changes the matching
	// target's computed supersession penalty, so the delta must distinguish that invalidation
	// from an unchanged snapshot even when ordinary relevance filtering drops the old page.
	write(t, base, "wiki/replacement.md", `---
type: decision
title: Replacement policy
relations:
  supersedes: [wiki/old-policy.md]
---

# Replacement policy

The current rule.
`)
	request.SinceReceipt = before.Receipt.InputDigest
	delta, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Items) != 0 || delta.Receipt.Changed != 1 || delta.Receipt.Candidates != 1 ||
		len(delta.Receipt.Dropped) != 1 || delta.Receipt.Dropped[0].URI != "wiki/old-policy.md" ||
		delta.Receipt.Dropped[0].Reason != "below-floor" ||
		strings.Contains(delta.Receipt.Warning, "nothing changed") {
		t.Fatalf("delta = %+v, want one changed candidate explicitly dropped below the relevance floor", delta)
	}
}

func TestContextSinceReceiptFailsClosedWithoutTheSnapshot(t *testing.T) {
	base := contextBase(t)
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Budget: 4096, SinceReceipt: strings.Repeat("a", 16),
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "seed") {
		t.Fatalf("BuildContext() error = %v, want an actionable missing-snapshot refusal", err)
	}
}

func TestContextSinceReceiptRejectsTrailingSnapshotData(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval", Budget: 4096, SaveSnapshot: true}
	before, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(
		core.StateDir(), "receipts", "*", before.Receipt.InputDigest+".json.gz",
	))
	if err != nil || len(paths) != 1 {
		t.Fatalf("receipt paths = %v, error = %v, want exactly one snapshot", paths, err)
	}
	compressed, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	var replacement bytes.Buffer
	writer := gzip.NewWriter(&replacement)
	if _, err := writer.Write(append(decoded, []byte("{}")...)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], replacement.Bytes(), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	request.SinceReceipt = before.Receipt.InputDigest
	_, err = services.BuildContext(t.Context(), base, request)
	if err == nil || !strings.Contains(err.Error(), "trailing JSON value") {
		t.Fatalf("BuildContext() error = %v, want a trailing snapshot value refusal", err)
	}
}

func TestContextSinceReceiptRejectsAnotherQuerySnapshot(t *testing.T) {
	base := contextBase(t)
	first, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Budget: 4096, SaveSnapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, SinceReceipt: first.Receipt.InputDigest,
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "reuse the same query") {
		t.Fatalf("BuildContext() error = %v, want a query-bound snapshot refusal", err)
	}
}

func TestContextSinceReceiptRejectsAnotherResolvedWindow(t *testing.T) {
	base := contextBase(t)
	first, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Window: services.Window{Since: "2026-05-04", Until: "2026-05-04"},
		Budget: 4096, SaveSnapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Window: services.Window{Since: "2026-05-05", Until: "2026-05-05"},
		Budget: 4096, SinceReceipt: first.Receipt.InputDigest,
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "reuse the same query, window, and as_of") {
		t.Fatalf("BuildContext() error = %v, want a window-bound snapshot refusal", err)
	}
}

func TestContextSinceReceiptRejectsAnotherAsOfDay(t *testing.T) {
	base := contextBase(t)
	window := services.Window{Since: "2026-05-04", Until: "2026-05-05"}
	first, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Window: window, Budget: 4096, SaveSnapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base.Now = func() time.Time { return testClock.AddDate(0, 0, 1) }
	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Window: window, Budget: 4096, SinceReceipt: first.Receipt.InputDigest,
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "reuse the same query, window, and as_of") {
		t.Fatalf("BuildContext() error = %v, want an as_of-bound snapshot refusal", err)
	}
}

func TestContextSinceReceiptUsesOnePhysicalBaseAcrossASymlinkAlias(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval", Budget: 4096, SaveSnapshot: true}
	before, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "brain-alias")
	if err := os.Symlink(base.Root(), alias); err != nil {
		t.Fatal(err)
	}
	aliased, err := services.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliased.Now = base.Now
	request.SinceReceipt = before.Receipt.InputDigest
	delta, err := services.BuildContext(t.Context(), aliased, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Items) != 0 || delta.Receipt.SinceReceipt != before.Receipt.InputDigest {
		t.Fatalf("alias delta = %+v, want the real base's unchanged snapshot", delta)
	}
}
