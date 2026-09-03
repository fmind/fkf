package services

import (
	"context"
	"fmt"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestWhoJoinsPagesCountsAndRecentInteractions(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [actor:github.com/maxime, maxime@work.example]
    kind: person
`)
	writeIdentityPage(t, base, "wiki/maxime.md", "---\ntype: person\ntitle: Maxime Cordy\naliases: [actor:github.com/maxime]\n---\n")
	writeIdentityEvent(t, base, "actor:github.com/maxime")
	if _, err := BuildGraph(context.Background(), base); err != nil {
		t.Fatal(err)
	}

	report, err := Who(context.Background(), base, "Maxime Cordy")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 {
		t.Fatalf("Who() matches = %+v", report.Matches)
	}
	match := report.Matches[0]
	if match.Canonical != "person:email/maxime@example.com" || match.Total != 1 {
		t.Fatalf("Who() match = %+v", match)
	}
	if len(match.Pages) != 1 || match.Pages[0].URI != "wiki/maxime.md" {
		t.Fatalf("Who() pages = %+v", match.Pages)
	}
	if len(match.Counts) != 1 || match.Counts[0].Source != "mail" || match.Counts[0].Count != 1 {
		t.Fatalf("Who() counts = %+v", match.Counts)
	}
	if len(match.Recent) != 1 || match.Recent[0].URI != "events/2026-09-01/mail.json#message-1" {
		t.Fatalf("Who() recent = %+v", match.Recent)
	}
}

func TestWhoIncludesRecordsLinkedFromMatchingInteractions(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime@work.example, actor:github.com/maxime]
    kind: person
`)
	calendarURI := "events/2026-09-01/calendar.json#event-1"
	notesURI := "events/2026-09-01/meeting-notes.json#notes-1"
	unrelatedURI := "events/2026-09-01/follow-ups.json#follow-up-1"
	writeWhoRelationRecord(t, base, "calendar", "event-1", "2026-09-01T12:00:00Z", "IMA meeting", map[string]string{
		"attendee": "actor:github.com/maxime",
		"notes":    notesURI,
	})
	writeWhoRelationRecord(t, base, "meeting-notes", "notes-1", "2026-09-01T12:05:00Z", "IMA meeting notes", map[string]string{
		"follow_up": unrelatedURI,
		"meeting":   calendarURI,
	})
	writeWhoRelationRecord(t, base, "follow-ups", "follow-up-1", "2026-09-01T12:10:00Z", "Unrelated follow-up", nil)
	if _, err := BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	report, err := Who(t.Context(), base, "maxime@work.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 {
		t.Fatalf("Who() matches = %+v", report.Matches)
	}
	match := report.Matches[0]
	if match.Total != 2 {
		t.Fatalf("Who() total = %d, want the calendar and its directly linked notes", match.Total)
	}
	if len(match.Recent) != 2 || match.Recent[0].URI != notesURI || match.Recent[1].URI != calendarURI {
		t.Fatalf("Who() recent = %+v, want linked notes then calendar without a second-hop follow-up", match.Recent)
	}
	if len(match.Counts) != 2 ||
		match.Counts[0] != (SourceCount{Source: "calendar", Count: 1}) ||
		match.Counts[1] != (SourceCount{Source: "meeting-notes", Count: 1}) {
		t.Fatalf("Who() counts = %+v", match.Counts)
	}
}

func TestWhoKeepsRecentInteractionsWhenTheNeighbourhoodIsTruncated(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime]
    kind: person
`)
	idPath, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	timePath, err := core.ParseFieldPath(".time")
	if err != nil {
		t.Fatal(err)
	}
	participantPath, err := core.ParseFieldPath(".participant")
	if err != nil {
		t.Fatal(err)
	}
	notesPath, err := core.ParseFieldPath(".notes")
	if err != nil {
		t.Fatal(err)
	}
	newestURI := "events/2026-09-01/busy.json#interaction-200"
	notesURI := "events/2026-09-01/meeting-notes.json#latest-notes"
	records := make([]sources.Record, 0, 201)
	for index := range 201 {
		record := sources.Record{
			"id":          fmt.Sprintf("interaction-%03d", index),
			"time":        fmt.Sprintf("2026-09-01T12:%02d:%02dZ", index/60, index%60),
			"participant": "person:email/maxime@example.com",
		}
		if index == 200 {
			record["notes"] = notesURI
		}
		records = append(records, record)
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "busy", Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z",
		Schema: core.FieldSchema{
			core.FieldID:   {Description: "id", Cardinality: core.CardinalityOne},
			core.FieldTime: {Description: "time", Cardinality: core.CardinalityOne},
			"participant":  {Description: "participant", Cardinality: core.CardinalityOne, Relation: true},
			"notes":        {Description: "notes", Cardinality: core.CardinalityOptional, Relation: true},
		},
		Fields: core.FieldMap{
			core.FieldID: {idPath}, core.FieldTime: {timePath},
			"participant": {participantPath}, "notes": {notesPath},
		},
		Count: len(records), Records: records,
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	writeWhoRelationRecord(
		t, base, "meeting-notes", "latest-notes", "2026-09-01T13:00:00Z", "Newest linked notes",
		map[string]string{"meeting": newestURI},
	)
	if _, err := BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	report, err := Who(t.Context(), base, "maxime")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 || report.Matches[0].Total != 202 || len(report.Matches[0].Recent) != 10 ||
		!report.Matches[0].NeighbourhoodTruncated || report.Matches[0].Recent[0].URI != notesURI {
		t.Fatalf("busy identity report = %+v, want 201 direct records, newest linked notes, ten recent, and named truncation", report)
	}
}

func writeWhoRelationRecord(
	t *testing.T,
	base *Base,
	source, id, at, title string,
	relations map[string]string,
) {
	t.Helper()
	paths := sources.Fields{}
	schema := core.FieldSchema{}
	record := sources.Record{"id": id, "time": at, "title": title}
	for _, field := range []string{core.FieldID, core.FieldTime, core.FieldTitle} {
		path, err := core.ParseFieldPath("." + field)
		if err != nil {
			t.Fatal(err)
		}
		paths[field] = core.FieldPaths{path}
		schema[field] = core.FieldDefinition{Description: field, Cardinality: core.CardinalityOne}
	}
	for field, value := range relations {
		path, err := core.ParseFieldPath("." + field)
		if err != nil {
			t.Fatal(err)
		}
		paths[field] = core.FieldPaths{path}
		schema[field] = core.FieldDefinition{
			Description: field, Cardinality: core.CardinalityOne, Relation: true,
		}
		record[field] = value
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: source, Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z", Schema: schema, Fields: paths, Count: 1,
		Records: []sources.Record{record},
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}
