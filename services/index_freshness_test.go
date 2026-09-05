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

const mixedIndexFreshnessConfig = `name: brain
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sync: {index_max_age_hours: 24}
sources:
  fast:
    enabled: true
    layer: index
    max_age_hours: 2
    run: [cli, fast]
    fields: {id: .id, title: .name}
  slow:
    enabled: true
    layer: index
    max_age_hours: 48
    run: [cli, slow]
    fields: {id: .id, title: .name}
`

func writeIndexFreshnessSnapshot(
	t *testing.T, base *services.Base, collectedAt, modifiedAt time.Time,
) {
	writeNamedIndexFreshnessSnapshot(t, base, "snapshot", collectedAt, modifiedAt, []sources.Record{{"id": "fmind/fkf", "name": "fkf"}})
}

func writeNamedIndexFreshnessSnapshot(
	t *testing.T, base *services.Base, name string, collectedAt, modifiedAt time.Time, records []sources.Record,
) {
	t.Helper()
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: name, Layer: core.LayerIndex,
		CollectedAt: collectedAt.UTC().Format(time.RFC3339),
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}, core.FieldTitle: {mustFieldPath(t, ".name")}},
		Count:       len(records),
		Records:     records,
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

func TestConfiguredIndexFreshnessAgreesAcrossStatusSyncAndListing(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"fast": `[{"id":"fast/new","name":"new fast"}]`,
		"slow": `[{"id":"slow/new","name":"new slow"}]`,
	}}
	base := newBase(t, mixedIndexFreshnessConfig, runner)
	collectedAt := testClock.Add(-12 * time.Hour)
	writeNamedIndexFreshnessSnapshot(t, base, "fast", collectedAt, collectedAt, []sources.Record{{"id": "fast/old", "name": "old fast"}})
	writeNamedIndexFreshnessSnapshot(t, base, "slow", collectedAt, collectedAt, []sources.Record{{"id": "slow/old", "name": "old slow"}})

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]services.SourceStatus{}
	for _, source := range status.Sources {
		byName[source.Name] = source
	}
	if !byName["fast"].Stale || byName["slow"].Stale {
		t.Fatalf("configured status freshness = %+v, want fast stale and slow fresh", byName)
	}
	listing, err := services.ListIndex(t.Context(), base, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, entry := range listing.Entries {
		listed[entry.Name] = entry.Stale
	}
	if !listed["fast"] || listed["slow"] {
		t.Fatalf("configured listing freshness = %+v, want fast stale and slow fresh", listed)
	}

	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]services.SyncOutcome{}
	for _, unit := range report.Units {
		outcomes[unit.Source] = unit.Outcome
	}
	if outcomes["fast"] != services.OutcomeWritten || outcomes["slow"] != services.OutcomeFresh {
		t.Fatalf("sync outcomes = %+v, want fast written and slow fresh", outcomes)
	}
}

func TestExplicitStatusFreshnessOverridesConfiguredSourceAges(t *testing.T) {
	base := newBase(t, mixedIndexFreshnessConfig, nil)
	collectedAt := testClock.Add(-12 * time.Hour)
	writeNamedIndexFreshnessSnapshot(t, base, "fast", collectedAt, collectedAt, nil)
	writeNamedIndexFreshnessSnapshot(t, base, "slow", collectedAt, collectedAt, nil)

	status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if status.Stale {
		t.Fatalf("explicit 24h override marked 12h empty snapshots stale: %+v", status.Sources)
	}
}

func TestIndexFreshnessIsStaleAtTheExactThreshold(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"fast": `[]`, "slow": `[]`}}
	base := newBase(t, mixedIndexFreshnessConfig, runner)
	writeNamedIndexFreshnessSnapshot(t, base, "fast", testClock.Add(-2*time.Hour), testClock, nil)
	writeNamedIndexFreshnessSnapshot(t, base, "slow", testClock, testClock, nil)

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range status.Sources {
		if source.Name == "fast" && !source.Stale {
			t.Fatalf("source at its exact 2h threshold is fresh: %+v", source)
		}
	}
	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range report.Units {
		if unit.Source == "fast" && unit.Outcome != services.OutcomeWritten {
			t.Fatalf("source at its exact 2h threshold has outcome %q, want written", unit.Outcome)
		}
	}
}

func TestConfiguredIndexFreshnessHandlesMissingInvalidAndDisabledSources(t *testing.T) {
	t.Run("missing and invalid snapshots are stale", func(t *testing.T) {
		base := newBase(t, mixedIndexFreshnessConfig, nil)
		writeNamedIndexFreshnessSnapshot(t, base, "fast", testClock, testClock, nil)
		path, err := base.Store.Resolve(sources.IndexDocumentURI("fast"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data), testClock.Format(time.RFC3339), "not-a-timestamp", 1))
		if err := os.WriteFile(path, data, core.BaseFileMode); err != nil {
			t.Fatal(err)
		}

		status, err := services.Report(t.Context(), base, services.StatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]services.SourceStatus{}
		for _, source := range status.Sources {
			byName[source.Name] = source
		}
		if !byName["fast"].Stale || !byName["slow"].Stale {
			t.Fatalf("invalid and missing source freshness = %+v, want both stale", byName)
		}
	})

	t.Run("disabled source is not stale", func(t *testing.T) {
		config := strings.Replace(mixedIndexFreshnessConfig, "  fast:\n    enabled: true", "  fast:\n    enabled: false", 1)
		base := newBase(t, config, nil)
		writeNamedIndexFreshnessSnapshot(t, base, "slow", testClock, testClock, nil)

		status, err := services.Report(t.Context(), base, services.StatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if status.Stale {
			t.Fatalf("disabled missing source made status stale: %+v", status.Sources)
		}
	})
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
