package services_test

import (
	"slices"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestIndexDocumentsIncludesACollectedSourceNamedPeople(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "people", Layer: core.LayerIndex,
		CollectedAt: "2026-05-10T12:00:00Z",
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}},
		Count:       1, Records: []sources.Record{{"id": "person-1"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}

	documents, err := base.IndexDocuments()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(documents, "people") {
		t.Fatalf("IndexDocuments() = %v, want the collected people source", documents)
	}
	if _, err := services.Read(t.Context(), base, "index/people.json", services.ReadOptions{}); err != nil {
		t.Fatalf("Read(index/people.json) error = %v", err)
	}
}
