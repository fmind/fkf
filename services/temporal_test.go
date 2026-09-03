package services_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
)

func TestParseTemporalQueryUsesOneClosedBoundaryGrammar(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local) // Sunday.
	tests := []struct {
		query, clean, since, until, derived string
		newest                              bool
	}{
		{"yesterday release work", "release work", "2026-05-09", "2026-05-09", "yesterday", false},
		{"release work today", "release work", "2026-05-10", "2026-05-10", "today", false},
		{"What did I do yesterday?", "What did I do", "2026-05-09", "2026-05-09", "yesterday", false},
		{"last week release work", "release work", "2026-04-27", "2026-05-03", "last week", false},
		{"release work this week", "release work", "2026-05-04", "2026-05-10", "this week", false},
		{"last friday release work", "release work", "2026-05-08", "2026-05-08", "last friday", false},
		{"release work friday", "release work", "2026-05-08", "2026-05-08", "friday", false},
		{"2026-04 release work", "release work", "2026-04-01", "2026-04-30", "2026-04", false},
		{"release work 2026-05-02", "release work", "2026-05-02", "2026-05-02", "2026-05-02", false},
		{"since 2026-05-01 release work", "release work", "2026-05-01", "2026-05-10", "since 2026-05-01", false},
		{"release work since 2026-05-01", "release work", "2026-05-01", "2026-05-10", "since 2026-05-01", false},
		{"last meeting notes", "meeting notes", "", "", "last", true},
		{"meeting notes last", "meeting notes", "", "", "last", true},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			parsed, err := services.ParseTemporalQuery(test.query, now)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Query != test.clean || parsed.Window.Since != test.since || parsed.Window.Until != test.until ||
				parsed.Window.DerivedFrom != test.derived || parsed.Newest != test.newest {
				t.Fatalf("ParseTemporalQuery() = %+v, want query %q window %s..%s from %q newest=%t",
					parsed, test.clean, test.since, test.until, test.derived, test.newest)
			}
		})
	}
}

func TestParseTemporalQueryRejectsAmbiguousBoundaryGrammar(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	for _, query := range []string{
		"yesterday release work today",
		"last release work last week",
		"today yesterday release work",
		"release work today yesterday",
		"since 2026-99-01 release work",
		"release work 2026-99",
	} {
		if _, err := services.ParseTemporalQuery(query, now); err == nil || !strings.Contains(err.Error(), "temporal") {
			t.Fatalf("ParseTemporalQuery(%q) error = %v, want an explicit temporal refusal", query, err)
		}
	}
}

func TestContextUntilOnlyGetsABoundedDerivedStart(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Budget: 4096, Window: services.Window{Until: "2026-05-05"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Receipt.Window.Since != "2026-04-06" || pack.Receipt.Window.Until != "2026-05-05" ||
		pack.Receipt.Window.DerivedFrom != "--until" {
		t.Fatalf("receipt window = %+v, want a bounded 30-day window derived from --until", pack.Receipt.Window)
	}
}

func TestContextRejectsTemporalQueryWithExplicitWindow(t *testing.T) {
	base := contextBase(t)
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "yesterday retrieval", Budget: 4096,
		Window: services.Window{Since: "2026-05-01", Until: "2026-05-05"},
	})
	if err == nil || !strings.Contains(err.Error(), "temporal") || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("BuildContext() error = %v, want ambiguous temporal inputs refused", err)
	}
}

func TestContextRejectsTemporalQueryWhoseDerivedWindowIsReversed(t *testing.T) {
	base := contextBase(t)
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "since 2026-05-11 retrieval", Budget: 4096,
	})
	if err == nil || !strings.Contains(err.Error(), "--since 2026-05-11 is after --until 2026-05-10") {
		t.Fatalf("BuildContext() error = %v, want reversed temporal window refused", err)
	}
}

func TestContextLastOrdersMatchingEvidenceByNewestFirst(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "last FK-412", Budget: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Query != "FK-412" || pack.Receipt.Query != "FK-412" || pack.Receipt.Window.DerivedFrom != "last" {
		t.Fatalf("pack temporal framing = query %q receipt %+v", pack.Query, pack.Receipt)
	}
	if len(pack.Items) == 0 || pack.Items[0].Date != "2026-05-05" {
		t.Fatalf("items = %+v, want the newest matching record first", pack.Items)
	}
}
