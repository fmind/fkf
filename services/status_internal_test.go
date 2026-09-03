package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestCheckHelpersUsesTheBoundedBaseReader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := t.TempDir()
	config := internalTestContract + `name: brain
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources: {}
`
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, core.BaseBinDir), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, core.BaseBinDir, hookScript)
	if err := os.WriteFile(hook, make([]byte, core.MaxControlFileBytes+1), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHelpers(t.Context(), base, &Status{}); !errors.Is(err, core.ErrFileTooLarge) {
		t.Fatalf("checkHelpers() error = %v, want core.ErrFileTooLarge", err)
	}
}

func TestVolumeHistoryHonoursCancellation(t *testing.T) {
	base := statusInternalBase(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := volumeHistory(ctx, base); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled volume history error = %v, want context.Canceled", err)
	}
}

func TestReportReadsEachCollectedDocumentExactlyOnce(t *testing.T) {
	base := statusDocumentBase(t)
	reads := map[string]int{}
	reader := func(ctx context.Context, uri string, limit int64) ([]byte, error) {
		reads[uri]++
		return base.ReadFileContext(ctx, uri, limit)
	}
	if _, err := reportWithDocumentReader(t.Context(), base, StatusRequest{SkipGitAudit: true}, reader); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"events/2026-08-24/events.json", "index/snapshot.json"} {
		if reads[uri] != 1 {
			t.Errorf("status read %s %d times, want exactly once", uri, reads[uri])
		}
	}
	if len(reads) != 2 {
		t.Fatalf("status read unexpected collected documents: %v", reads)
	}
}

func TestReportKeepsStatusAvailableWhenAnIndexDocumentIsCorrupt(t *testing.T) {
	base := statusDocumentBase(t)
	corrupt := filepath.Join(base.Root(), "index", "snapshot.json")
	if err := os.WriteFile(corrupt, []byte("not json\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	status, err := Report(t.Context(), base, StatusRequest{SkipGitAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range status.Findings {
		if finding.Check == "documents" && slices.Contains(finding.Paths, "index/snapshot.json") {
			found = true
			break
		}
	}
	if !found || status.OK || status.Errors == 0 {
		t.Fatalf("status = %+v, want a document finding without aborting the report", status)
	}
}

func statusDocumentBase(t *testing.T) *Base {
	t.Helper()
	base := statusInternalBase(t)
	base.Store = core.NewStore(base.Root(), map[core.Layer]bool{
		core.LayerEvents: true, core.LayerIndex: true,
	})
	idPath, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	timePath, err := core.ParseFieldPath(".time")
	if err != nil {
		t.Fatal(err)
	}
	collected := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	event := &sources.Document{
		FKF: sources.SchemaVersion, Source: "events", Layer: core.LayerEvents, Date: "2026-08-24",
		WindowStart: "2026-08-24T00:00:00Z", WindowEnd: "2026-08-25T00:00:00Z", CollectedAt: collected,
		Schema: core.FieldSchema{
			core.FieldID:   {Description: "Stable identity.", Cardinality: core.CardinalityOne},
			core.FieldTime: {Description: "Event time.", Cardinality: core.CardinalityOne},
		},
		Fields: core.FieldMap{core.FieldID: {idPath}, core.FieldTime: {timePath}},
		Count:  1, Records: []sources.Record{{"id": "event-1", "time": "2026-08-24T12:00:00Z"}},
	}
	index := &sources.Document{
		FKF: sources.SchemaVersion, Source: "snapshot", Layer: core.LayerIndex, CollectedAt: collected,
		Schema: core.FieldSchema{core.FieldID: {Description: "Stable identity.", Cardinality: core.CardinalityOne}},
		Fields: core.FieldMap{core.FieldID: {idPath}}, Count: 1,
		Records: []sources.Record{{"id": "snapshot-1"}},
	}
	for _, document := range []*sources.Document{event, index} {
		if err := base.WriteDocument(document); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func statusInternalBase(t *testing.T) *Base {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := t.TempDir()
	config := internalTestContract + "name: brain\nlayers: {events: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return base
}
