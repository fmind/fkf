package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

// NodeCount is one distinct node and how many edges touch it.
type NodeCount struct {
	URI   string `json:"uri"`
	Kind  string `json:"kind"`
	Out   int    `json:"out"`
	In    int    `json:"in"`
	Total int    `json:"total"`
}

// NodeListing is what `fkf graph nodes` returns.
type NodeListing struct {
	Kind  string        `json:"kind,omitempty"`
	Nodes []NodeCount   `json:"nodes"`
	Total int           `json:"total"`
	Stats EdgeScanStats `json:"stats"`
}

// NodeKind classifies a URI for `--kind`, using the parser as the source of truth for schemes.
func NodeKind(uri string) string {
	if parsed, err := ParseURI(uri); err == nil {
		switch {
		case parsed.Scheme == SchemeExternal:
			return "url"
		case parsed.IsEntity():
			return string(parsed.Scheme)
		}
	}
	first, _, _ := strings.Cut(uri, "/")
	if uri == core.GraphFile || uri == core.GraphDstFile || uri == core.GraphOffsetsFile ||
		uri == core.GraphMetaFile || uri == core.GraphGenerationFile {
		return "derived"
	}
	switch core.Layer(first) {
	case core.LayerEvents:
		return "event"
	case core.LayerIndex:
		return "index"
	case core.LayerTasks:
		return "task"
	case core.LayerProjects:
		return "project"
	case core.LayerWiki:
		return "wiki"
	default:
		return "file"
	}
}

// ListNodes reports the distinct nodes in the edge list, busiest first.
func ListNodes(ctx context.Context, base *Base, kind string, limit int) (*NodeListing, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	kind = strings.TrimSpace(kind)
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	listing := &NodeListing{Kind: kind, Nodes: []NodeCount{}}
	counts := map[string]*NodeCount{}
	record := func(uri string, outgoing bool) {
		nodeKind := NodeKind(uri)
		if identityKind := resolver.Kind(uri); identityKind != "" {
			switch identityKind {
			case core.IdentityPerson:
				nodeKind = "person"
			case core.IdentityOrganization:
				nodeKind = "organization"
			case core.IdentityRepository:
				nodeKind = "repository"
			}
			if kind == nodeKind {
				uri = resolver.Canonical(uri)
			}
		}
		if kind != "" && nodeKind != kind {
			return
		}
		entry, known := counts[uri]
		if !known {
			entry = &NodeCount{URI: uri, Kind: nodeKind}
			counts[uri] = entry
		}
		if outgoing {
			entry.Out++
		} else {
			entry.In++
		}
		entry.Total++
	}
	stats, err := scanValidatedGraphCache(ctx, base, func(edge Edge) error {
		record(edge.Src, true)
		record(edge.Dst, false)
		return nil
	})
	if err != nil {
		return nil, err
	}
	listing.Stats = stats
	for _, entry := range counts {
		listing.Nodes = append(listing.Nodes, *entry)
	}
	sort.Slice(listing.Nodes, func(i, j int) bool {
		if listing.Nodes[i].Total != listing.Nodes[j].Total {
			return listing.Nodes[i].Total > listing.Nodes[j].Total
		}
		return listing.Nodes[i].URI < listing.Nodes[j].URI
	})
	listing.Total = len(listing.Nodes)
	if limit > 0 && len(listing.Nodes) > limit {
		listing.Nodes = listing.Nodes[:limit]
	}
	return listing, nil
}

// KindCount is one classification and how many rows carry it.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// GraphSummary is what the bare `fkf graph` returns: the shape of the edge list, so a reader
// knows what there is to walk before choosing a node. It is one scan, like every other read.
type GraphSummary struct {
	URI         string        `json:"uri"`
	GeneratedAt string        `json:"generated_at,omitempty"`
	Edges       int           `json:"edges"`
	Nodes       int           `json:"nodes"`
	EdgeKinds   []KindCount   `json:"edge_kinds"`
	NodeKinds   []KindCount   `json:"node_kinds"`
	Extractors  []string      `json:"extractors,omitempty"`
	Stats       EdgeScanStats `json:"stats"`
}

// SummarizeGraph counts the edge list by edge kind and node kind in one pass, then verifies that
// its sidecar describes those exact rows. Both files are one rebuildable cache; accepting one
// without the other would hide an interrupted or hand-edited build.
func SummarizeGraph(ctx context.Context, base *Base) (*GraphSummary, error) {
	return summarizeGraphWithOptions(ctx, base, graphValidationOptions{})
}

// VerifyGraph performs the explicit slow integrity path: it hashes every graph input and all
// three generated artifacts before validating the complete primary edge list. It never writes.
func VerifyGraph(ctx context.Context, base *Base) (*GraphSummary, error) {
	return summarizeGraphWithOptions(ctx, base, graphValidationOptions{fullInputs: true, fullOutputs: true})
}

func summarizeGraphWithOptions(
	ctx context.Context, base *Base, options graphValidationOptions,
) (*GraphSummary, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	summary := &GraphSummary{URI: core.GraphFile, EdgeKinds: []KindCount{}, NodeKinds: []KindCount{}}
	edgeKinds, nodeKinds := map[string]int{}, map[string]int{}
	nodes := map[string]struct{}{}
	note := func(uri string) {
		if _, known := nodes[uri]; known {
			return
		}
		nodes[uri] = struct{}{}
		nodeKinds[NodeKind(uri)]++
	}
	cache, _, meta, err := openValidatedGraphCacheWithOptions(ctx, base, options)
	if err != nil {
		return summary, err
	}
	defer func() { _ = cache.close() }()
	stats, err := scanGraphRows(ctx, base, cache, func(edge Edge) error {
		edgeKinds[edge.Kind]++
		note(edge.Src)
		note(edge.Dst)
		summary.Edges++
		return nil
	})
	summary.Stats, summary.Nodes = stats, len(nodes)
	summary.EdgeKinds, summary.NodeKinds = sortedKindCounts(edgeKinds), sortedKindCounts(nodeKinds)
	if meta.GeneratedAt != "" {
		summary.GeneratedAt, summary.Extractors = meta.GeneratedAt, meta.Extractors
	}
	if err != nil {
		return summary, err
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return summary, err
	}
	return summary, nil
}

// scanValidatedGraphCache performs the one full scan needed to prove that graph.tsv and its
// sidecar describe the same complete cache. Callers that need subsequent narrow scans use
// openValidatedGraphCache instead, keeping this exact validated file generation open.
func scanValidatedGraphCache(ctx context.Context, base *Base, visit func(Edge) error) (EdgeScanStats, error) {
	cache, _, _, err := openValidatedGraphCache(ctx, base)
	if err != nil {
		return EdgeScanStats{}, err
	}
	defer func() { _ = cache.close() }()
	stats, err := scanGraphRows(ctx, base, cache, visit)
	if err != nil {
		return stats, err
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return stats, err
	}
	return stats, nil
}

func openValidatedGraphCache(ctx context.Context, base *Base) (*validatedGraphCache, EdgeScanStats, EdgeListMeta, error) {
	return openValidatedGraphCacheWithOptions(ctx, base, graphValidationOptions{})
}

func openValidatedGraphCacheWithOptions(
	ctx context.Context, base *Base, options graphValidationOptions,
) (*validatedGraphCache, EdgeScanStats, EdgeListMeta, error) {
	var meta EdgeListMeta
	// Probe the primary before the generation marker so a present unsafe graph is never
	// misclassified as a benign cache that has not been built yet. Reopen it after reading the
	// marker; the pre/post marker check then brackets every descriptor used by this read.
	probe, err := openGraphFile(base)
	if err != nil {
		return nil, EdgeScanStats{}, meta, err
	}
	if err := probe.Close(); err != nil {
		return nil, EdgeScanStats{}, meta, fmt.Errorf("close graph preflight descriptor: %w", err)
	}
	generation, err := readCurrentGraphGeneration(ctx, base)
	if err != nil {
		return nil, EdgeScanStats{}, meta, err
	}
	file, err := openGraphFile(base)
	if err != nil {
		return nil, EdgeScanStats{}, meta, err
	}
	cache := &validatedGraphCache{file: file}
	meta, err = readGraphMeta(ctx, base)
	if err != nil {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, fmt.Errorf(
			"invalid derived graph cache: %w; run `fkf build graph`", err)
	}
	problems := graphMetaStaticProblems(meta)
	if generation != graphGenerationSHA256(meta) {
		problems = append(problems, "graph generation marker does not match metadata")
	}
	problems = append(problems, currentGraphInputProblemsWithOptions(ctx, base, meta, options)...)
	if ctxErr := checkContext(ctx); ctxErr != nil {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, ctxErr
	}
	cache.meta = meta
	if err := cache.openSeekArtifacts(base); err != nil {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, err
	}
	outputProblems, err := cache.validateGraphOutputs(ctx, options.fullOutputs)
	if err != nil {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, err
	}
	// A changed primary is already invalid, but scanning that changed file on the error path
	// preserves the more actionable malformed-row and row-count diagnostics. Healthy walks
	// never pay for this full scan.
	if graphTSVChanged(outputProblems) {
		if _, rowErr := scanGraphRows(ctx, base, cache, nil); rowErr != nil {
			_ = cache.close()
			return nil, EdgeScanStats{}, meta, rowErr
		}
	}
	problems = append(problems, outputProblems...)
	currentGeneration, generationErr := readCurrentGraphGeneration(ctx, base)
	if generationErr != nil {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, generationErr
	}
	if currentGeneration != generation {
		problems = append(problems, "graph generation changed while its artifacts were opened; retry")
	}
	if len(problems) > 0 {
		_ = cache.close()
		return nil, EdgeScanStats{}, meta, fmt.Errorf("invalid derived graph cache: %s; run `fkf build graph`",
			strings.Join(problems, "; "))
	}
	return cache, EdgeScanStats{}, meta, nil
}

func validateCachedEdge(base *Base, edge Edge) error {
	for _, item := range []struct{ field, value string }{{"src", edge.Src}, {"dst", edge.Dst}} {
		field, value := item.field, item.value
		parsed, err := ParseURI(value)
		if err != nil {
			return fmt.Errorf("graph row %s %q is not a URI: %w", field, value, err)
		}
		if parsed.String() != value {
			return fmt.Errorf("graph row %s %q is not canonical; want %q", field, value, parsed.String())
		}
		if parsed.Scheme != SchemeFile {
			continue
		}
		if parsed.Dir || parsed.Path == "" || parsed.JQ != "" {
			return fmt.Errorf("graph row %s %q is not an addressable file or record node", field, value)
		}
		if _, err := base.Store.Resolve(parsed.Path); err != nil {
			return fmt.Errorf("graph row %s %q is outside the published base grammar: %w", field, value, err)
		}
	}
	return nil
}

type byteCounter int

func (counter *byteCounter) Write(data []byte) (int, error) {
	*counter += byteCounter(len(data))
	return len(data), nil
}

// sortedKindCounts orders by volume, then by name, so the same base always prints the same list.
func sortedKindCounts(counts map[string]int) []KindCount {
	ordered := make([]KindCount, 0, len(counts))
	for kind, count := range counts {
		ordered = append(ordered, KindCount{Kind: kind, Count: count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Count != ordered[j].Count {
			return ordered[i].Count > ordered[j].Count
		}
		return ordered[i].Kind < ordered[j].Kind
	})
	return ordered
}

// readGraphMeta loads the required sidecar for the edge-list cache.
func readGraphMeta(ctx context.Context, base *Base) (EdgeListMeta, error) {
	var meta EdgeListMeta
	data, err := base.ReadFileContext(ctx, core.GraphMetaFile, core.MaxSourceDocumentBytes)
	if err != nil {
		return meta, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&meta); err != nil {
		return meta, fmt.Errorf("decode %s: %w", core.GraphMetaFile, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return meta, fmt.Errorf("decode %s: trailing JSON holds more than one document",
			core.GraphMetaFile)
	} else if !errors.Is(err, io.EOF) {
		return meta, fmt.Errorf("decode %s: invalid trailing JSON: %w",
			core.GraphMetaFile, err)
	}
	return meta, nil
}
