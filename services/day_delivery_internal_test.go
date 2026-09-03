package services

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/sources"
)

func TestTimelineTextBudgetKeepsTenCommitsAndCalendarEntries(t *testing.T) {
	records := make([]FindRecord, 0, 13)
	for index := 1; index <= 10; index++ {
		records = append(records, FindRecord{
			URI:    fmt.Sprintf("events/2026-08-28/git-commits.json#fgraph-%02d", index),
			Source: "git-commits",
			Date:   "2026-08-28",
			Time:   fmt.Sprintf("2026-08-28T%02d:00:00Z", index+7),
			Title:  fmt.Sprintf("fgraph commit %02d", index),
			Fields: map[string][]string{
				"repository": {"repo:github.com/fmind/fgraph"},
			},
		})
	}
	records = append(records,
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#aaif-lunch", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T12:30:00+02:00", Title: "AAIF lunch",
			Fields: map[string][]string{
				"participant": {"person:email/maxime@example.test", "person:email/lea@example.test"},
			},
		},
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#decathlon-am", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T09:00:00+02:00", Title: "Decathlon busy block",
		},
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#decathlon-pm", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T15:00:00+02:00", Title: "Decathlon busy block",
		},
	)

	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	now := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	resolver := &IdentityResolver{}
	textReport, err := buildTimelineReport(records, TimelineRequest{
		Window: window, Budget: 600, All: true, DeliveryFormat: DigestDeliveryText,
	}, window, now, resolver)
	if err != nil {
		t.Fatal(err)
	}
	text := []byte(RenderTimelineText(textReport))
	if textReport.Receipt.Records != 13 || textReport.Receipt.Selected != 13 || textReport.Receipt.Dropped != 0 {
		t.Fatalf("text receipt = %+v, want all ten commits and three calendar records", textReport.Receipt)
	}
	for index := 1; index <= 10; index++ {
		if title := fmt.Sprintf("fgraph commit %02d", index); !strings.Contains(string(text), title) {
			t.Fatalf("text digest omitted %q:\n%s", title, text)
		}
	}
	for _, title := range []string{"AAIF lunch", "Decathlon busy block"} {
		if !strings.Contains(string(text), title) {
			t.Fatalf("text digest omitted calendar title %q:\n%s", title, text)
		}
	}
	assertTimelineDeliverySize(t, textReport, text)

	jsonReport, err := buildTimelineReport(records, TimelineRequest{
		Window: window, Budget: 600, All: true, DeliveryFormat: DigestDeliveryJSON,
	}, window, now, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if jsonReport.Receipt.Selected >= textReport.Receipt.Selected {
		t.Fatalf("JSON selected %d records and text selected %d; want indented JSON to trim more under the same budget",
			jsonReport.Receipt.Selected, textReport.Receipt.Selected)
	}
	assertTimelineDeliverySize(t, jsonReport, marshalTimelineJSON(jsonReport))
}

func TestTimelineBudgetPrefersCreatedWorkAndNamedCalendarEvidence(t *testing.T) {
	records := make([]FindRecord, 0, 260)
	for index := 1; index <= 10; index++ {
		records = append(records, FindRecord{
			URI:    fmt.Sprintf("events/2026-08-28/git-commits.json#fmind/fgraph@%040d", index),
			Source: "git-commits", Date: "2026-08-28",
			Time:  fmt.Sprintf("2026-08-28T%02d:00:00Z", index+7),
			Title: fmt.Sprintf("A deliberately verbose fgraph commit subject %02d", index),
			Fields: map[string][]string{
				"repository": {"repo:github.com/fmind/fgraph"},
			},
			relations: map[string]struct{}{"repository": {}},
		})
	}
	for index := 1; index <= 10; index++ {
		records = append(records, FindRecord{
			URI:    fmt.Sprintf("events/2026-08-28/github-commits.json#https://github.com/fmind/fgraph/commit/%040d", index),
			Source: "github-commits", Date: "2026-08-28",
			Time:  fmt.Sprintf("2026-08-28T%02d:00:00Z", index+7),
			Title: fmt.Sprintf("A deliberately verbose fgraph commit subject %02d", index),
			Fields: map[string][]string{
				"repository":  {"repo:github.com/fmind/fgraph"},
				"participant": {"person:email/contributor@example.test"},
			},
			relations: map[string]struct{}{"repository": {}, "participant": {}},
		})
	}
	records = append(records, FindRecord{
		URI:    "events/2026-08-28/github-commits.json#https://github.com/fmind/publications/commit/1111111111111111111111111111111111111111",
		Source: "github-commits", Date: "2026-08-28", Time: "2026-08-28T08:00:00Z",
		Title: "A publication update outside the primary repository",
		Fields: map[string][]string{
			"repository":  {"repo:github.com/fmind/publications"},
			"participant": {"person:email/contributor@example.test"},
		},
		relations: map[string]struct{}{"repository": {}, "participant": {}},
	})
	records = append(records,
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#owner@example.test%7E3hj2l26ojds1k6qg06nr0gqm17", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T10:30:00Z", Title: "Lunch: AAIF Lux Organizers",
			Fields: map[string][]string{
				"participant": {
					"person:email/hajar@example.test", "person:email/hazal@example.test",
					"person:email/mustafa@example.test", "person:email/lea@example.test",
					"person:email/marc@example.test", "person:email/sara@example.test",
				},
			},
			relations: map[string]struct{}{"participant": {}},
		},
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#owner.partner@decathlon.com%7E27ljbho70lq6qqlmnpkoo0h82s_20260828T090000Z", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T09:00:00Z",
			Record: sources.Record{
				"visibility": "private",
				"calendar":   map[string]any{"summary": "owner.partner@decathlon.com"},
			},
		},
		FindRecord{
			URI: "events/2026-08-28/google-calendar-events.json#owner.partner@decathlon.com%7E087avo4tivtaivoj51ob4v1qjd_20260828T140000Z", Source: "google-calendar-events",
			Date: "2026-08-28", Time: "2026-08-28T15:00:00Z",
			Record: sources.Record{
				"visibility": "private",
				"calendar":   map[string]any{"summary": "owner.partner@decathlon.com"},
			},
		},
	)
	for _, noisy := range []struct {
		source string
		count  int
	}{
		{source: "google-drive-changes", count: 122},
		{source: "agent-prompts", count: 25},
		{source: "shell-commands", count: 15},
		{source: "github-runs", count: 21},
	} {
		for item := range noisy.count {
			record := FindRecord{
				URI:    fmt.Sprintf("events/2026-08-28/%s.json#item-%03d", noisy.source, item),
				Source: noisy.source, Date: "2026-08-28", Time: "2026-08-28T08:00:00Z",
				Title: "Noisy evidence represented by one truthful source count",
			}
			// GitHub runs are summarized in the digest, but their repository relations still
			// contribute to the receipt pressure that exposed the live-base packing boundary.
			if noisy.source == "github-runs" && item < 5 {
				record.Fields = map[string][]string{
					"repository": {fmt.Sprintf("repo:github.com/fmind/project-%d", item)},
				}
				record.relations = map[string]struct{}{"repository": {}}
			}
			records = append(records, record)
		}
	}

	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	report, err := buildTimelineReport(records, TimelineRequest{
		Window: window, Budget: 600, DeliveryFormat: DigestDeliveryText,
	}, window, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), &IdentityResolver{})
	if err != nil {
		t.Fatal(err)
	}
	text := RenderTimelineText(report)
	for _, want := range []string{
		"Lunch: AAIF Lux Organizers", "Busy — Decathlon x2",
		"fmind/fgraph commits x10", "person:email/hajar@example.test",
		"person:email/hazal@example.test", "person:email/mustafa@example.test",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("digest omitted %q under realistic competing volume:\n%s", want, text)
		}
	}
	assertTimelineDeliverySize(t, report, []byte(text))

	jsonReport, err := buildTimelineReport(records, TimelineRequest{
		Window: window, Budget: 600, DeliveryFormat: DigestDeliveryJSON,
	}, window, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), &IdentityResolver{})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range jsonReport.Groups {
		for _, item := range group.Items {
			if item.Title == "Busy — Decathlon" && item.Count == 2 {
				assertTimelineDeliverySize(t, jsonReport, marshalTimelineJSON(jsonReport))
				return
			}
		}
	}
	t.Fatalf("JSON digest omitted collapsed Decathlon busy blocks: %+v", jsonReport.Groups)
}

func TestTimelineAccountsForEachExactDeliveryEncoder(t *testing.T) {
	records := []FindRecord{{
		URI: "events/2026-08-28/git-commits.json#special", Source: "git-commits",
		Date: "2026-08-28", Time: "2026-08-28T09:00:00Z", Title: "Keep <M&A> bytes exact",
	}}
	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	now := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		format string
		render func(*TimelineReport) []byte
	}{
		{format: DigestDeliveryJSON, render: marshalTimelineJSON},
		{format: DigestDeliveryJSONL, render: marshalTimelineJSONL},
		{format: DigestDeliveryCompactJSON, render: marshalTimelineCompactJSON},
		{format: DigestDeliveryText, render: func(report *TimelineReport) []byte {
			return []byte(RenderTimelineText(report))
		}},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			report, err := buildTimelineReport(records, TimelineRequest{
				Window: window, Budget: 600, All: true, DeliveryFormat: test.format,
			}, window, now, &IdentityResolver{})
			if err != nil {
				t.Fatal(err)
			}
			if report.Receipt.Format != test.format {
				t.Fatalf("receipt format = %q, want %q", report.Receipt.Format, test.format)
			}
			assertTimelineDeliverySize(t, report, test.render(report))
		})
	}
}

func TestTimelineReceiptFloorIsDeliverySpecificAndExact(t *testing.T) {
	records := []FindRecord{{
		URI: "events/2026-08-28/git-commits.json#one", Source: "git-commits",
		Date: "2026-08-28", Time: "2026-08-28T09:00:00Z", Title: "One commit",
	}}
	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	now := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	renderers := map[string]func(*TimelineReport) []byte{
		DigestDeliveryJSON:        marshalTimelineJSON,
		DigestDeliveryJSONL:       marshalTimelineJSONL,
		DigestDeliveryCompactJSON: marshalTimelineCompactJSON,
		DigestDeliveryText: func(report *TimelineReport) []byte {
			return []byte(RenderTimelineText(report))
		},
	}
	minimums := make(map[string]int, len(renderers))
	for format, render := range renderers {
		t.Run(format, func(t *testing.T) {
			request := TimelineRequest{Window: window, Budget: 1, All: true, DeliveryFormat: format}
			_, err := buildTimelineReport(records, request, window, now, &IdentityResolver{})
			var budgetErr *DigestBudgetError
			if !errors.As(err, &budgetErr) || budgetErr.Minimum <= 1 {
				t.Fatalf("budget-one error = %v, want a format-specific receipt minimum", err)
			}
			minimums[format] = budgetErr.Minimum
			request.Budget = budgetErr.Minimum
			report, err := buildTimelineReport(records, request, window, now, &IdentityResolver{})
			if err != nil {
				t.Fatalf("reported minimum %d failed: %v", budgetErr.Minimum, err)
			}
			assertTimelineDeliverySize(t, report, render(report))
		})
	}
	if minimums[DigestDeliveryText] >= minimums[DigestDeliveryJSON] {
		t.Fatalf("text receipt floor = %d, JSON floor = %d; want the compact text floor to be smaller",
			minimums[DigestDeliveryText], minimums[DigestDeliveryJSON])
	}
}

func TestTimelineInputDigestBindsRenderedGroupingSemantics(t *testing.T) {
	calendarRecord := func() FindRecord {
		return FindRecord{
			URI:    "events/2026-08-28/google-calendar-events.json#busy",
			Source: "google-calendar-events", Date: "2026-08-28", Time: "2026-08-28T09:00:00Z",
			Record: sources.Record{
				"visibility": "private",
				"calendar":   map[string]any{"summary": "owner.partner@decathlon.com"},
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(*FindRecord)
	}{
		{name: "source", mutate: func(record *FindRecord) {
			record.Source = "google-calendar-agenda"
		}},
		{name: "calendar summary", mutate: func(record *FindRecord) {
			record.Record["calendar"] = map[string]any{"summary": "owner.partner@acme.com"}
		}},
		{name: "visibility", mutate: func(record *FindRecord) {
			record.Record["visibility"] = "public"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeRecord := calendarRecord()
			afterRecord := calendarRecord()
			test.mutate(&afterRecord)
			before := timelineDigestReport(t, beforeRecord)
			after := timelineDigestReport(t, afterRecord)
			if reflect.DeepEqual(before.Groups, after.Groups) {
				t.Fatalf("groups did not change after %s mutation: %+v", test.name, before.Groups)
			}
			if before.Receipt.InputDigest == after.Receipt.InputDigest {
				t.Fatalf("input digest %q did not bind %s-derived grouping", before.Receipt.InputDigest, test.name)
			}
		})
	}
}

func TestTimelineInputDigestBindsRawPrioritySemantics(t *testing.T) {
	beforeRecord := FindRecord{
		URI:    "events/2026-08-28/google-calendar-events.json#office",
		Source: "google-calendar-events", Date: "2026-08-28", Time: "2026-08-28T09:00:00Z",
		Title: "Office", Record: sources.Record{"eventType": "default"},
	}
	afterRecord := beforeRecord
	afterRecord.Record = sources.Record{"eventType": "workingLocation"}
	if digestRecordPriority(beforeRecord) == digestRecordPriority(afterRecord) {
		t.Fatal("eventType mutation did not change the packing priority")
	}
	before := timelineDigestReport(t, beforeRecord)
	after := timelineDigestReport(t, afterRecord)
	if before.Receipt.InputDigest == after.Receipt.InputDigest {
		t.Fatalf("input digest %q did not bind the eventType-derived packing priority", before.Receipt.InputDigest)
	}
}

func timelineDigestReport(t *testing.T, record FindRecord) *TimelineReport {
	t.Helper()
	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	report, err := buildTimelineReport(
		[]FindRecord{record},
		TimelineRequest{Window: window, Budget: 2000, All: true, DeliveryFormat: DigestDeliveryJSON},
		window, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), &IdentityResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func assertTimelineDeliverySize(t *testing.T, report *TimelineReport, delivered []byte) {
	t.Helper()
	if len(delivered) > report.Receipt.Budget*4 {
		t.Fatalf("%s delivery = %d bytes, over %d-byte budget", report.Receipt.Format, len(delivered), report.Receipt.Budget*4)
	}
	if want := bytesToTokens(len(delivered)); report.Receipt.UsedTokens != want {
		t.Fatalf("%s used_tokens = %d, want %d for %d delivered bytes",
			report.Receipt.Format, report.Receipt.UsedTokens, want, len(delivered))
	}
}
