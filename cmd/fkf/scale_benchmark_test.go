package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

const scaleConfig = `fkf: 1
name: scale
schema:
  id: {description: Stable benchmark record identity., cardinality: one}
  time: {description: Fixed benchmark event time., cardinality: one}
  title: {description: Searchable benchmark title., cardinality: one}
  related: {description: Synthetic benchmark relationships., cardinality: many, relation: true}
layers:
  events: true
  index: false
  tasks: false
  projects: false
  wiki: false
sources:
  scale:
    enabled: true
    layer: events
    run: [printf, "[]"]
    fields:
      id: .id
      time: .time
      title: .title
      related: .related[]
`

var scaleNow = time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC)

type scaleCorpus struct {
	root           string
	runtime        string
	records        int
	edges          int
	window         services.Window
	firstRecordURI string
}

func TestScaleCorpusShape(t *testing.T) {
	corpus, err := createScaleCorpus(t.Context(), t.TempDir(), 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if corpus.records != 10 || corpus.edges != 50 {
		t.Fatalf("scale corpus = %d records and %d edges, want 10 and 50", corpus.records, corpus.edges)
	}

	base, err := services.Open(corpus.root)
	if err != nil {
		t.Fatal(err)
	}
	base.Now = func() time.Time { return scaleNow }
	found, err := services.Find(t.Context(), base, services.FindFilter{
		Sources: []string{"scale"}, Window: corpus.window,
		Grep: []string{"benchmark"}, Limit: services.NoFindLimit,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if found.Scanned != corpus.records || found.Matched != corpus.records || len(found.Records) != corpus.records {
		t.Fatalf("find = %d scanned, %d matched, %d returned; want %d of each",
			found.Scanned, found.Matched, len(found.Records), corpus.records)
	}

	const budget = 2048
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "record-000009", Window: corpus.window, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) == 0 || pack.Receipt.EncodedTokens > budget {
		t.Fatalf("context returned %d item(s) using %d tokens; want at least one item within %d",
			len(pack.Items), pack.Receipt.EncodedTokens, budget)
	}

	neighbours, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: corpus.firstRecordURI, Direction: services.DirectionOut, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbours.Edges) != 5 {
		t.Fatalf("first record has %d outgoing edges, want 5", len(neighbours.Edges))
	}
}

// BenchmarkScaleEnvelope measures the supported v1 scale through separate fkf processes.
// -benchtime=1x is intentional: every reported row is one complete operation over the exact
// same deterministic 100k-record/500k-edge base, including CLI decoding and rendering.
func BenchmarkScaleEnvelope(b *testing.B) {
	corpus, err := createScaleCorpus(b.Context(), b.TempDir(), 100_000, 5)
	if err != nil {
		b.Fatal(err)
	}
	binary := filepath.Join(scaleRepositoryRoot(), "bin", "fkf")
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		b.Fatalf("benchmark binary %s is unavailable; run `mise run build` first", binary)
	}

	operations := []struct {
		name string
		args []string
	}{
		{
			name: "Find100kRecords",
			args: []string{
				"find", "benchmark", "--source", "scale", "--since", corpus.window.Since,
				"--until", corpus.window.Until, "--count",
			},
		},
		{
			name: "Context100kRecords",
			args: []string{
				"context", "record-099999", "--since", corpus.window.Since,
				"--until", corpus.window.Until, "--budget", "2048",
			},
		},
		{name: "Build500kEdges", args: []string{"build", "graph"}},
		{
			name: "Navigate500kEdges",
			args: []string{"graph", corpus.firstRecordURI, "--out", "--limit", "100"},
		},
	}
	for _, operation := range operations {
		b.Run(operation.name, func(b *testing.B) {
			benchmarkScaleCommand(b, binary, corpus, operation.args)
		})
	}
}

func createScaleCorpus(ctx context.Context, workspace string, recordCount, relationsPerRecord int) (*scaleCorpus, error) {
	if recordCount < 1 || relationsPerRecord < 1 {
		return nil, fmt.Errorf("scale dimensions must be positive, got %d records and %d relations", recordCount, relationsPerRecord)
	}
	root := filepath.Join(workspace, "base")
	runtimeDirectory := filepath.Join(workspace, "runtime")
	if err := os.MkdirAll(root, core.BaseDirMode); err != nil {
		return nil, fmt.Errorf("create scale base: %w", err)
	}
	if err := os.MkdirAll(runtimeDirectory, core.BaseDirMode); err != nil {
		return nil, fmt.Errorf("create scale runtime: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(scaleConfig), core.BaseFileMode); err != nil {
		return nil, fmt.Errorf("write scale config: %w", err)
	}
	base, err := services.Open(root)
	if err != nil {
		return nil, err
	}
	base.Now = func() time.Time { return scaleNow }

	documentCount := min(recordCount, 10)
	firstDate := scaleNow.AddDate(0, 0, -documentCount)
	fields, err := scaleFieldMap()
	if err != nil {
		return nil, err
	}
	schema := core.FieldSchema{
		core.FieldID:    {Description: "Stable benchmark record identity.", Cardinality: core.CardinalityOne},
		core.FieldTime:  {Description: "Fixed benchmark event time.", Cardinality: core.CardinalityOne},
		core.FieldTitle: {Description: "Searchable benchmark title.", Cardinality: core.CardinalityOne},
		"related":       {Description: "Synthetic benchmark relationships.", Cardinality: core.CardinalityMany, Relation: true},
	}
	for documentIndex := range documentCount {
		date := firstDate.AddDate(0, 0, documentIndex).Format(time.DateOnly)
		start := documentIndex * recordCount / documentCount
		end := (documentIndex + 1) * recordCount / documentCount
		records := make([]sources.Record, 0, end-start)
		for recordIndex := start; recordIndex < end; recordIndex++ {
			id := fmt.Sprintf("record-%06d", recordIndex)
			relations := make([]string, relationsPerRecord)
			for relationIndex := range relationsPerRecord {
				relations[relationIndex] = "topic:scale/" + id + "/" + strconv.Itoa(relationIndex)
			}
			records = append(records, sources.Record{
				"id": id, "time": date + "T12:00:00Z",
				"title": "scale benchmark " + id, "related": relations,
			})
		}
		document := &sources.Document{
			FKF: sources.SchemaVersion, Source: "scale", Layer: core.LayerEvents, Date: date,
			CollectedAt: scaleNow.Format(time.RFC3339), Schema: schema, Fields: fields,
			Body: false, Count: len(records), Records: records,
		}
		if err := base.WriteDocument(document); err != nil {
			return nil, fmt.Errorf("write scale document %s: %w", document.URI(), err)
		}
	}
	graph, err := services.BuildGraph(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("build scale graph: %w", err)
	}
	wantEdges := recordCount * relationsPerRecord
	if graph.Edges != wantEdges {
		return nil, fmt.Errorf("scale graph has %d edges, want %d", graph.Edges, wantEdges)
	}
	lastDate := firstDate.AddDate(0, 0, documentCount-1).Format(time.DateOnly)
	return &scaleCorpus{
		root: root, runtime: runtimeDirectory, records: recordCount, edges: wantEdges,
		window:         services.Window{Since: firstDate.Format(time.DateOnly), Until: lastDate},
		firstRecordURI: sources.EventDocumentURI(firstDate.Format(time.DateOnly), "scale") + "#record-000000",
	}, nil
}

func scaleFieldMap() (core.FieldMap, error) {
	fields := core.FieldMap{}
	for name, raw := range map[string]string{
		core.FieldID: ".id", core.FieldTime: ".time", core.FieldTitle: ".title", "related": ".related[]",
	} {
		parsed, err := core.ParseFieldPath(raw)
		if err != nil {
			return nil, fmt.Errorf("parse scale field %s: %w", name, err)
		}
		fields[name] = core.FieldPaths{parsed}
	}
	return fields, nil
}

func benchmarkScaleCommand(b *testing.B, binary string, corpus *scaleCorpus, arguments []string) {
	b.Helper()
	var peakRSS uint64
	b.ResetTimer()
	for range b.N {
		args := []string{"--base", corpus.root, "--format", "json"}
		command := exec.CommandContext(b.Context(), binary, append(args, arguments...)...)
		command.Env = scaleCommandEnvironment(corpus.runtime)
		command.Stdout = io.Discard
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			b.Fatalf("fkf %s: %v: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
		}
		rss, err := processPeakRSSBytes(command.ProcessState)
		if err != nil {
			b.Fatal(err)
		}
		peakRSS += rss
	}
	b.StopTimer()
	b.ReportMetric(float64(corpus.records), "records")
	b.ReportMetric(float64(corpus.edges), "edges")
	b.ReportMetric(float64(peakRSS)/float64(b.N), "rss-bytes/op")
}

func scaleCommandEnvironment(runtimeDirectory string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "FKF_BASE" || key == "HOME" || key == "XDG_STATE_HOME" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"HOME="+filepath.Join(runtimeDirectory, "home"),
		"XDG_STATE_HOME="+filepath.Join(runtimeDirectory, "state"),
	)
}

func scaleRepositoryRoot() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
