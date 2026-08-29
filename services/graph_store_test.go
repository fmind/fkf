package services_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
)

func sampleEdges() []services.Edge {
	return []services.Edge{
		{
			Src: "../events/2026-08-20/gmail.json#/messages/a1b2", Dst: "actor:directory/marc",
			Kind: "participates", At: "2026-08-20T14:02:00Z", Via: "email-address", Indexed: "2026-08-21T06:00:00Z",
		},
		{
			Src: "../events/2026-08-20/gmail.json#/messages/a1b2", Dst: "https://example.invalid/browse/FK-412?a=1&b=2",
			Kind: "mentions", At: "2026-08-20T14:02:00Z", Via: "ticket-key", Indexed: "2026-08-21T06:00:00Z",
		},
	}
}

func sampleGraphInputs(t *testing.T) services.GraphInputSHA256 {
	t.Helper()
	inputs, err := services.NewGraphInputSHA256(
		strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64),
		strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func TestEncodeEdgesIsCanonicalAndDeterministic(t *testing.T) {
	t.Parallel()

	edges := sampleEdges()
	reversed := []services.Edge{edges[1], edges[0], edges[0]} // out of order, with a duplicate

	var first, second bytes.Buffer
	if err := services.EncodeEdges(&first, edges); err != nil {
		t.Fatalf("encode edges: %v", err)
	}
	if err := services.EncodeEdges(&second, reversed); err != nil {
		t.Fatalf("encode reversed edges: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("encoding is not order-independent:\n%q\n%q", first.String(), second.String())
	}
	if lines := strings.Count(first.String(), "\n"); lines != 2 {
		t.Fatalf("expected 2 rows after dedupe, got %d", lines)
	}

	// The prefilter contract: src is the first column, so a query is a line-prefix test, and
	// nothing rewrites the bytes of a provider URL on the way to disk.
	if !strings.HasPrefix(first.String(), "../events/2026-08-20/gmail.json#/messages/a1b2\t") {
		t.Fatalf("src is not the leading column: %q", first.String())
	}
	if !strings.Contains(first.String(), `?a=1&b=2`) {
		t.Fatalf("a provider URL was rewritten, which would break literal prefilters: %s", first.String())
	}
	if columns := strings.Count(strings.SplitN(first.String(), "\n", 2)[0], "\t"); columns != 5 {
		t.Fatalf("expected 6 columns, found %d separators", columns)
	}
}

func TestEncodeEdgesRejectsSeparatorBytes(t *testing.T) {
	t.Parallel()

	// A field carrying a tab or newline would silently corrupt the row layout, so an extractor
	// bug must fail loudly at the write boundary rather than be escaped away.
	for name, edge := range map[string]services.Edge{
		"tab in dst":     {Src: "a", Dst: "b\tc", Kind: "mentions", Via: "test"},
		"newline in src": {Src: "a\nb", Dst: "c", Kind: "mentions", Via: "test"},
		"return in via":  {Src: "a", Dst: "b", Kind: "mentions", Via: "te\rst"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := services.EncodeEdges(&bytes.Buffer{}, []services.Edge{edge})
			if !errors.Is(err, services.ErrEdgeSeparator) {
				t.Fatalf("expected a separator rejection, got %v", err)
			}
		})
	}
}

func TestScanEdgesMatchesByQuery(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := services.EncodeEdges(&encoded, sampleEdges()); err != nil {
		t.Fatalf("encode edges: %v", err)
	}

	for name, testCase := range map[string]struct {
		query   services.EdgeQuery
		matches int
	}{
		"by destination": {services.EdgeQuery{Dst: "actor:directory/marc"}, 1},
		"by url with ampersand": {
			services.EdgeQuery{Dst: "https://example.invalid/browse/FK-412?a=1&b=2"}, 1,
		},
		"by kind":         {services.EdgeQuery{Kind: "mentions"}, 1},
		"by source":       {services.EdgeQuery{Src: "../events/2026-08-20/gmail.json#/messages/a1b2"}, 2},
		"empty is a scan": {services.EdgeQuery{}, 2},
		"no match":        {services.EdgeQuery{Dst: "actor:directory/absent"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var seen []services.Edge
			stats, err := services.ScanEdges(t.Context(), bytes.NewReader(encoded.Bytes()), testCase.query,
				func(edge services.Edge) error { seen = append(seen, edge); return nil })
			if err != nil {
				t.Fatalf("scan edges: %v", err)
			}
			if len(seen) != testCase.matches || stats.Matched != testCase.matches {
				t.Fatalf("expected %d matches, got %d visited and %d counted",
					testCase.matches, len(seen), stats.Matched)
			}
			if stats.Malformed != 0 {
				t.Fatalf("unexpected malformed rows: %d", stats.Malformed)
			}
		})
	}
}

func TestScanEdgesChecksCancellationBetweenRows(t *testing.T) {
	var encoded bytes.Buffer
	if err := services.EncodeEdges(&encoded, sampleEdges()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	visited := 0
	stats, err := services.ScanEdges(ctx, &encoded, services.EdgeQuery{}, func(services.Edge) error {
		visited++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
	if visited != 1 || stats.Lines != 1 {
		t.Fatalf("cancelled scan visited %d edge(s) across %d line(s), want one completed row", visited, stats.Lines)
	}
}

func TestScanEdgesSkipsMalformedRowsWithoutFailing(t *testing.T) {
	t.Parallel()

	index := "a\tb\tmentions\t\ttest\t\n" +
		"a\tnot-enough-columns\n" + "\n" +
		"a\tc\tmentions\t\ttest\t\n"

	// A narrow query never parses the corrupt row, because it cannot pass the prefilter.
	narrow, err := services.ScanEdges(t.Context(), strings.NewReader(index), services.EdgeQuery{Kind: "mentions"}, nil2visit(t))
	if err != nil {
		t.Fatalf("a corrupt row must not fail the scan: %v", err)
	}
	if narrow.Matched != 2 || narrow.Malformed != 0 {
		t.Fatalf("expected 2 matched and 0 malformed under a query, got %d and %d",
			narrow.Matched, narrow.Malformed)
	}

	// An integrity audit uses the zero query, which parses every row and so sees the corruption.
	audit, err := services.ScanEdges(t.Context(), strings.NewReader(index), services.EdgeQuery{}, nil2visit(t))
	if err != nil {
		t.Fatalf("a corrupt row must not fail a full scan: %v", err)
	}
	if audit.Matched != 2 || audit.Malformed != 1 {
		t.Fatalf("expected 2 matched and 1 malformed under a full scan, got %d and %d",
			audit.Matched, audit.Malformed)
	}
}

func nil2visit(t *testing.T) func(services.Edge) error {
	t.Helper()
	return func(services.Edge) error { return nil }
}

func TestEncodeEdgesRejectsIncompleteRows(t *testing.T) {
	t.Parallel()

	for name, edge := range map[string]services.Edge{
		"no src":  {Dst: "b", Kind: "mentions", Via: "test"},
		"no dst":  {Src: "a", Kind: "mentions", Via: "test"},
		"no kind": {Src: "a", Dst: "b", Via: "test"},
		"no via":  {Src: "a", Dst: "b", Kind: "mentions"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := services.EncodeEdges(&bytes.Buffer{}, []services.Edge{edge}); err == nil {
				t.Fatal("expected an incomplete edge to be rejected")
			}
		})
	}
}

func TestEncodeEdgesMatchesTheScannerRowBound(t *testing.T) {
	base := services.Edge{Src: "a", Kind: "k", Via: "v"}
	// Five tabs and the trailing newline consume six bytes; the other three non-empty fields
	// consume three. A total exactly at MaxEdgeLineBytes is the largest row Scanner admits.
	base.Dst = strings.Repeat("d", services.MaxEdgeLineBytes-9)
	var encoded bytes.Buffer
	if err := services.EncodeEdges(&encoded, []services.Edge{base}); err != nil {
		t.Fatalf("EncodeEdges(exact bound) error = %v", err)
	}
	if encoded.Len() != services.MaxEdgeLineBytes {
		t.Fatalf("encoded bytes = %d, want %d", encoded.Len(), services.MaxEdgeLineBytes)
	}
	if _, err := services.ScanEdges(t.Context(), bytes.NewReader(encoded.Bytes()), services.EdgeQuery{}, nil2visit(t)); err != nil {
		t.Fatalf("ScanEdges(exact bound) error = %v", err)
	}

	base.Dst += "d"
	err := services.EncodeEdges(&bytes.Buffer{}, []services.Edge{base})
	if !errors.Is(err, services.ErrEdgeLineTooLong) {
		t.Fatalf("EncodeEdges(over bound) error = %v, want ErrEdgeLineTooLong", err)
	}
}

func TestWriteAndAppendEdgeList(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "links.jsonl")
	metaPath := filepath.Join(dir, "links.meta.json")
	generated := time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC)

	edges := sampleEdges()
	meta, err := services.NewEdgeListMeta(edges, generated, sampleGraphInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Edges != 2 || meta.GeneratedAt != "2026-08-21T06:00:00Z" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if strings.Join(meta.Extractors, ",") != "email-address,ticket-key" {
		t.Fatalf("extractors must be sorted and deduped, got %v", meta.Extractors)
	}
	if err := services.WriteEdgeList(path, metaPath, edges, meta); err != nil {
		t.Fatalf("write edge list: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat index: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("index must be owner-only, got %o", mode)
	}

	appended := services.Edge{
		Src: "a", Dst: "b", Kind: "mentions", Via: "test", Indexed: generated.Format(time.RFC3339),
	}
	edges = append(edges, appended)
	meta, err = services.NewEdgeListMeta(edges, generated, sampleGraphInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := services.WriteEdgeList(path, metaPath, edges, meta); err != nil {
		t.Fatalf("replace edge list: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer func() { _ = file.Close() }()

	stats, err := services.ScanEdges(t.Context(), file, services.EdgeQuery{}, nil2visit(t))
	if err != nil {
		t.Fatalf("scan replaced index: %v", err)
	}
	if stats.Matched != 3 {
		t.Fatalf("expected 3 rows after replacement, got %d", stats.Matched)
	}
}

func TestMetadataWriteFailureLeavesAnOldSidecarThatCannotValidateNewRows(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rowsDir, metaDir := filepath.Join(root, "rows"), filepath.Join(root, "meta")
	for _, directory := range []string{rowsDir, metaDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path, metaPath := filepath.Join(rowsDir, "graph.tsv"), filepath.Join(metaDir, "graph.meta.json")
	generated := time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC)
	indexed := generated.Format(time.RFC3339)
	oldEdges := []services.Edge{{Src: "a", Dst: "b", Kind: "mentions", Via: "test", Indexed: indexed}}
	oldMeta, err := services.NewEdgeListMeta(oldEdges, generated, sampleGraphInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := services.WriteEdgeList(path, metaPath, oldEdges, oldMeta); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(metaDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(metaDir, 0o700) })

	newEdges := []services.Edge{{Src: "a", Dst: "c", Kind: "mentions", Via: "test", Indexed: indexed}}
	newMeta, err := services.NewEdgeListMeta(newEdges, generated, sampleGraphInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := services.WriteEdgeList(path, metaPath, newEdges, newMeta); err == nil {
		t.Fatal("WriteEdgeList() succeeded despite an unwritable metadata directory")
	}
	encodedRows, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encodedRows, []byte("\tc\t")) {
		t.Fatalf("rows = %q, want the rows-first publication failure simulated", encodedRows)
	}
	encodedMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var retained services.EdgeListMeta
	if err := json.Unmarshal(encodedMeta, &retained); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encodedRows)
	if retained.SHA256.Outputs.GraphTSV == hex.EncodeToString(digest[:]) {
		t.Fatal("the retained old sidecar accidentally validates the newly published rows")
	}
}
