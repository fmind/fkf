package services_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

const indexFreshnessConfig = `name: brain
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sync: {index_max_age_hours: 24}
sources:
  snapshot:
    enabled: true
    layer: index
    run: [cli, list]
    fields:
      id: .id
      title: .name
`

func writeIndexFreshnessSnapshot(
	t *testing.T, base *services.Base, collectedAt, modifiedAt time.Time,
) {
	t.Helper()
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "snapshot", Layer: core.LayerIndex,
		CollectedAt: collectedAt.UTC().Format(time.RFC3339),
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}, core.FieldTitle: {mustFieldPath(t, ".name")}},
		Count:       1,
		Records:     []sources.Record{{"id": "fmind/fkf", "name": "fkf"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	absolute, err := base.Store.Resolve(document.URI())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(absolute, modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}
}

func TestSyncIndexFreshnessComesFromCollectedAt(t *testing.T) {
	cases := map[string]struct {
		collectedAgo time.Duration
		modifiedAgo  time.Duration
		wantOutcome  services.SyncOutcome
		wantCalls    int
	}{
		"old metadata with fresh mtime is recollected": {
			collectedAgo: 48 * time.Hour,
			modifiedAgo:  time.Hour,
			wantOutcome:  services.OutcomeWritten,
			wantCalls:    1,
		},
		"fresh metadata with old mtime is skipped": {
			collectedAgo: time.Hour,
			modifiedAgo:  48 * time.Hour,
			wantOutcome:  services.OutcomeFresh,
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"": `[{"id":"fmind/fkf","name":"fkf"}]`,
			}}
			base := newBase(t, indexFreshnessConfig, runner)
			writeIndexFreshnessSnapshot(t, base, testClock.Add(-test.collectedAgo), testClock.Add(-test.modifiedAgo))
			trust(t, base)

			report, err := services.Sync(t.Context(), base, services.SyncRequest{Targets: []string{"snapshot"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Units) != 1 || report.Units[0].Outcome != test.wantOutcome {
				t.Fatalf("units = %+v, want outcome %q", report.Units, test.wantOutcome)
			}
			if len(runner.calls) != test.wantCalls {
				t.Fatalf("runner calls = %d, want %d from collected_at freshness", len(runner.calls), test.wantCalls)
			}
		})
	}
}

func TestListIndexDescribesCollectedAtRatherThanMtime(t *testing.T) {
	cases := map[string]struct {
		collectedAgo time.Duration
		modifiedAgo  time.Duration
		wantAge      int
		wantStale    bool
	}{
		"old metadata with fresh mtime": {
			collectedAgo: 48 * time.Hour,
			modifiedAgo:  time.Hour,
			wantAge:      48,
			wantStale:    true,
		},
		"fresh metadata with old mtime": {
			collectedAgo: time.Hour,
			modifiedAgo:  48 * time.Hour,
			wantAge:      1,
		},
		"future metadata": {
			collectedAgo: -time.Hour,
			modifiedAgo:  time.Hour,
			wantAge:      0,
			wantStale:    true,
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			base := newBase(t, indexFreshnessConfig, nil)
			collectedAt := testClock.Add(-test.collectedAgo)
			writeIndexFreshnessSnapshot(t, base, collectedAt, testClock.Add(-test.modifiedAgo))

			listing, err := services.ListIndex(t.Context(), base, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(listing.Entries) != 1 {
				t.Fatalf("entries = %+v", listing.Entries)
			}
			entry := listing.Entries[0]
			if entry.AgeHours != test.wantAge || entry.Stale != test.wantStale {
				t.Fatalf("entry = %+v, want age_hours=%d stale=%t", entry, test.wantAge, test.wantStale)
			}
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			wantCollected := `"collected_at":"` + collectedAt.UTC().Format(time.RFC3339) + `"`
			if !strings.Contains(string(encoded), wantCollected) || strings.Contains(string(encoded), `"modified"`) {
				t.Fatalf("entry JSON = %s, want %s and no filesystem-mtime field", encoded, wantCollected)
			}
		})
	}
}

func TestStatusNeverTreatsAFutureIndexCollectionAsFresh(t *testing.T) {
	base := newBase(t, indexFreshnessConfig, nil)
	writeIndexFreshnessSnapshot(t, base, testClock.Add(time.Hour), testClock)

	status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale || len(status.Sources) != 1 || !status.Sources[0].Stale || status.Sources[0].LagHours != 0 {
		t.Fatalf("status = %+v, want the future-dated source stale with a non-negative public lag", status)
	}
}

func TestStatusDescribesIndexAgeFromCollectedAt(t *testing.T) {
	cases := map[string]struct {
		collectedAgo time.Duration
		modifiedAgo  time.Duration
		wantNote     string
	}{
		"old metadata with fresh mtime": {
			collectedAgo: 48 * time.Hour,
			modifiedAgo:  time.Hour,
			wantNote:     "oldest refreshed 48h ago",
		},
		"fresh metadata with old mtime": {
			collectedAgo: time.Hour,
			modifiedAgo:  48 * time.Hour,
			wantNote:     "oldest refreshed 1h ago",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			base := newBase(t, indexFreshnessConfig, nil)
			collectedAt := testClock.Add(-test.collectedAgo)
			writeIndexFreshnessSnapshot(t, base, collectedAt, testClock.Add(-test.modifiedAgo))

			status, err := services.Report(t.Context(), base, services.StatusRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var index services.LayerOverview
			for _, layer := range status.Layers {
				if layer.Layer == core.LayerIndex {
					index = layer
					break
				}
			}
			if index.Note != test.wantNote {
				t.Fatalf("index layer = %+v, want note %q from collected_at", index, test.wantNote)
			}
			if len(status.Sources) != 1 || status.Sources[0].LastDate != collectedAt.Local().Format(time.DateOnly) {
				t.Fatalf("sources = %+v, want last_date from collected_at", status.Sources)
			}
		})
	}
}
