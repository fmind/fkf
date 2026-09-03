package services

import (
	"slices"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestIdentityAwareReadsUseOnlyDeclaredRelationFields(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime, actor:github.com/maxime]
    kind: person
`)
	writeRelationBoundaryDocument(t, base)
	if _, err := BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	found, err := Find(t.Context(), base, FindFilter{
		Grep: []string{"maxime"}, Limit: NoFindLimit,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Records) != 2 {
		t.Fatalf("find records = %+v, want the literal metadata and true relation matches", found.Records)
	}
	byURI := map[string]FindRecord{}
	for _, record := range found.Records {
		byURI[record.URI] = record
	}
	if got := byURI["events/2026-09-01/boundary.json#metadata-person"].Fields["category"]; !slices.Equal(got, []string{"maxime"}) {
		t.Fatalf("non-relation category = %v, want literal provider metadata", got)
	}
	if got := byURI["events/2026-09-01/boundary.json#related-person"].Fields["participant"]; !slices.Equal(got, []string{"person:email/maxime@example.com"}) {
		t.Fatalf("declared participant = %v, want canonical identity", got)
	}

	who, err := Who(t.Context(), base, "maxime")
	if err != nil {
		t.Fatal(err)
	}
	if len(who.Matches) != 1 || who.Matches[0].Total != 1 ||
		who.Matches[0].Recent[0].URI != "events/2026-09-01/boundary.json#related-person" {
		t.Fatalf("who report = %+v, want only the true participant relation", who)
	}
	pack, err := BuildContext(t.Context(), base, ContextRequest{
		Query:  "person:email/maxime@example.com",
		Window: Window{Since: "2026-09-01", Until: "2026-09-01"},
		Budget: 2000, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if item.URI == "events/2026-09-01/boundary.json#metadata-person" {
			t.Fatalf("non-relation identity metadata gained context identity semantics: %+v", item)
		}
	}
	if !slices.ContainsFunc(pack.Items, func(item ContextItem) bool {
		return item.URI == "events/2026-09-01/boundary.json#related-person"
	}) {
		t.Fatalf("context items = %+v, want the true participant relation", pack.Items)
	}

	timeline, err := Timeline(t.Context(), base, TimelineRequest{
		Window: Window{Since: "2026-09-01", Until: "2026-09-01"},
		Budget: 2000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(timeline.People, []string{"person:email/maxime@example.com"}) ||
		!slices.Equal(timeline.Repositories, []string{"repo:github.com/fmind/real"}) {
		t.Fatalf("timeline entities = people %v repositories %v, want relation-only summaries",
			timeline.People, timeline.Repositories)
	}
	filtered, err := Timeline(t.Context(), base, TimelineRequest{
		Window:     Window{Since: "2026-09-01", Until: "2026-09-01"},
		Repository: "repo:github.com/fmind/impostor", Budget: 1000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Receipt.Records != 0 {
		t.Fatalf("non-relation repository filter returned %+v", filtered)
	}
}

func writeRelationBoundaryDocument(t *testing.T, base *Base) {
	t.Helper()
	paths := core.FieldMap{}
	for name := range map[string]struct{}{
		core.FieldID: {}, core.FieldTime: {}, core.FieldTitle: {},
		"category": {}, "participant": {}, "repository": {},
	} {
		parsed, err := core.ParseFieldPath("." + name)
		if err != nil {
			t.Fatal(err)
		}
		paths[name] = core.FieldPaths{parsed}
	}
	schema := core.FieldSchema{
		core.FieldID:    {Description: "id", Cardinality: core.CardinalityOne},
		core.FieldTime:  {Description: "time", Cardinality: core.CardinalityOne},
		core.FieldTitle: {Description: "title", Cardinality: core.CardinalityOne},
		"category":      {Description: "ordinary provider metadata", Cardinality: core.CardinalityOptional},
		"participant":   {Description: "person relation", Cardinality: core.CardinalityOptional, Relation: true},
		"repository":    {Description: "repository relation", Cardinality: core.CardinalityOptional, Relation: true},
	}
	records := []sources.Record{
		{"id": "metadata-person", "time": "2026-09-01T09:00:00Z", "title": "Metadata", "category": "maxime"},
		{"id": "related-person", "time": "2026-09-01T10:00:00Z", "title": "Interaction", "participant": "actor:github.com/maxime"},
		{"id": "metadata-repository", "time": "2026-09-01T11:00:00Z", "title": "Metadata repo", "category": "repo:github.com/fmind/impostor"},
		{"id": "related-repository", "time": "2026-09-01T12:00:00Z", "title": "Repository work", "repository": "repo:github.com/fmind/real"},
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "boundary", Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z", Schema: schema, Fields: paths,
		Count: len(records), Records: records,
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}
