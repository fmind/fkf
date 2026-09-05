package services

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestTimelineUsesProviderNeutralVolumeAndProjectionRules(t *testing.T) {
	records := make([]FindRecord, 0, 10)
	for index := range 8 {
		records = append(records, FindRecord{
			URI: fmt.Sprintf("events/2026-08-28/activity.json#%d", index), Source: "activity",
			Date: "2026-08-28", Time: fmt.Sprintf("2026-08-28T%02d:00:00Z", index+8),
			Title: fmt.Sprintf("Activity %d", index),
		})
	}
	records = append(records,
		FindRecord{
			URI: "events/2026-08-28/decisions.json#one", Source: "decisions", Date: "2026-08-28",
			Time: "2026-08-28T16:00:00Z", Title: "Approve bounded delivery",
			Fields:    map[string][]string{"owner": {"person:email/owner@example.test"}},
			relations: map[string]struct{}{"owner": {}},
		},
		FindRecord{
			URI: "events/2026-08-28/decisions.json#two", Source: "decisions", Date: "2026-08-28",
			Time: "2026-08-28T17:00:00Z",
		},
	)
	window := Window{Since: "2026-08-28", Until: "2026-08-28"}
	report, err := buildTimelineReport(records, TimelineRequest{
		Window: window, Budget: 600, DeliveryFormat: DigestDeliveryText,
	}, window, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), &IdentityResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 2 || !report.Groups[0].Summarized || report.Groups[0].Count != 8 {
		t.Fatalf("groups = %+v, want every high-volume source summarized by the same rule", report.Groups)
	}
	if report.Groups[1].Summarized || len(report.Groups[1].Items) != 2 ||
		report.Groups[1].Items[0].Title != "Approve bounded delivery" ||
		report.Groups[1].Items[1].Title != records[9].URI {
		t.Fatalf("decisions = %+v, want projected title then URI fallback", report.Groups[1])
	}
	if !reflect.DeepEqual(report.People, []string{"person:email/owner@example.test"}) {
		t.Fatalf("people = %v, want explicit relation values retained", report.People)
	}
	assertTimelineDeliverySize(t, report, []byte(RenderTimelineText(report)))
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

func TestTimelineInputDigestBindsProjectedGroupingSemantics(t *testing.T) {
	base := FindRecord{
		URI: "events/2026-08-28/source.json#one", Source: "source", Date: "2026-08-28",
		Time: "2026-08-28T09:00:00Z", Title: "Projected title",
	}
	for _, test := range []struct {
		name   string
		mutate func(*FindRecord)
	}{
		{name: "source", mutate: func(record *FindRecord) { record.Source = "renamed-source" }},
		{name: "title", mutate: func(record *FindRecord) { record.Title = "Changed projected title" }},
		{name: "relation", mutate: func(record *FindRecord) {
			record.Fields = map[string][]string{"owner": {"person:email/owner@example.test"}}
			record.relations = map[string]struct{}{"owner": {}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			beforeRecord, afterRecord := base, base
			test.mutate(&afterRecord)
			before, after := timelineDigestReport(t, beforeRecord), timelineDigestReport(t, afterRecord)
			if before.Receipt.InputDigest == after.Receipt.InputDigest {
				t.Fatalf("input digest %q did not bind %s projection", before.Receipt.InputDigest, test.name)
			}
		})
	}
}

func TestTimelineIgnoresUnprojectedProviderPayload(t *testing.T) {
	beforeRecord := FindRecord{
		URI: "events/2026-08-28/source.json#one", Source: "source", Date: "2026-08-28",
		Time: "2026-08-28T09:00:00Z", Title: "Projected title", Record: map[string]any{"private": "one"},
	}
	afterRecord := beforeRecord
	afterRecord.Record = map[string]any{"private": "two", "provider_specific": true}
	before, after := timelineDigestReport(t, beforeRecord), timelineDigestReport(t, afterRecord)
	if !reflect.DeepEqual(before.Groups, after.Groups) || before.Receipt.InputDigest != after.Receipt.InputDigest {
		t.Fatalf("unprojected provider payload changed generic digest: before=%+v after=%+v", before, after)
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
