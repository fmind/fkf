package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// graph.tsv at the base root is the edge list over the base's URIs. It is derived, gitignored,
// rebuildable, and never a source of truth — deleting it costs speed, never data.
//
// Every edge is transcription, never inference. A record contributes edges only through fields
// its stored schema declares as relations; a page contributes explicit relations, tags, and
// links its author wrote. Bodies are never scanned. The moment a graph edge can be wrong in an
// interesting way, the receipt built on top of it stops being credible.

// Edge kinds.
const (
	EdgeTag  = "tag"
	EdgeLink = "link"
)

// GraphBuild reports one derive step.
type GraphBuild struct {
	URI       string       `json:"uri"`
	Edges     int          `json:"edges"`
	Documents int          `json:"documents"`
	Pages     int          `json:"pages"`
	Mode      string       `json:"mode"`
	Elapsed   string       `json:"elapsed"`
	Meta      EdgeListMeta `json:"meta"`
}

// BuildGraph rescans the whole base and replaces the derived edge cache. It is a pure function of
// the files on disk and the clock it is given, so the same base and clock yield byte-identical output.
func BuildGraph(ctx context.Context, base *Base) (*GraphBuild, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	started := base.Now()
	inputsSHA256, err := graphInputSHA256(ctx, base)
	if err != nil {
		return nil, err
	}
	edges, counts, err := ExtractEdges(ctx, base)
	if err != nil {
		return nil, err
	}
	confirmedInputsSHA256, err := graphInputSHA256(ctx, base)
	if err != nil {
		return nil, err
	}
	if confirmedInputsSHA256 != inputsSHA256 {
		return nil, errors.New("graph inputs changed while the derived caches were being built; retry")
	}
	meta, err := writeGraph(base, edges, started, inputsSHA256)
	if err != nil {
		return nil, err
	}
	return &GraphBuild{
		URI: core.GraphFile, Edges: len(DedupeEdges(edges)),
		Documents: counts.documents, Pages: counts.pages, Mode: "full",
		Elapsed: base.Now().Sub(started).Round(time.Millisecond).String(),
		Meta:    meta,
	}, nil
}

func writeGraph(
	base *Base, edges []Edge, at time.Time, inputsSHA256 GraphInputSHA256,
) (EdgeListMeta, error) {
	indexed := at.UTC().Format(time.RFC3339)
	for index := range edges {
		edges[index].Indexed = indexed
	}
	rows, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		return EdgeListMeta{}, err
	}
	meta, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		return EdgeListMeta{}, err
	}
	if err := os.MkdirAll(path.Dir(rows), core.BaseDirMode); err != nil {
		return EdgeListMeta{}, fmt.Errorf("create %s: %w", path.Dir(rows), err)
	}
	metadata, err := NewEdgeListMeta(edges, at, inputsSHA256)
	if err != nil {
		return EdgeListMeta{}, err
	}
	if err := WriteEdgeList(rows, meta, edges, metadata); err != nil {
		return EdgeListMeta{}, err
	}
	return metadata, nil
}

// graphInputSHA256 binds the cache to separately diagnosable logical components. Each component
// is domain-separated and framed, so empty inputs remain unambiguous and filesystem order never
// participates. Descriptions and examples stay outside the schema digest because they cannot
// change an edge.
func graphInputSHA256(ctx context.Context, base *Base) (GraphInputSHA256, error) {
	events, err := graphDocumentInputSHA256(ctx, base, core.LayerEvents)
	if err != nil {
		return GraphInputSHA256{}, err
	}
	index, err := graphDocumentInputSHA256(ctx, base, core.LayerIndex)
	if err != nil {
		return GraphInputSHA256{}, err
	}
	projects, err := graphAuthoredInputSHA256(ctx, base, core.LayerProjects)
	if err != nil {
		return GraphInputSHA256{}, err
	}
	tasks, err := graphAuthoredInputSHA256(ctx, base, core.LayerTasks)
	if err != nil {
		return GraphInputSHA256{}, err
	}
	wiki, err := graphAuthoredInputSHA256(ctx, base, core.LayerWiki)
	if err != nil {
		return GraphInputSHA256{}, err
	}
	return NewGraphInputSHA256(events, index, projects, tasks, wiki, graphSchemaInputSHA256(base))
}

func graphDocumentInputSHA256(ctx context.Context, base *Base, layer core.Layer) (string, error) {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-input-" + string(layer) + "-v1\x00"))
	if !base.Store.Enabled(layer) {
		return hex.EncodeToString(digest.Sum(nil)), nil
	}
	var uris []string
	switch layer {
	case core.LayerEvents:
		dates, err := base.EventDates()
		if err != nil {
			return "", err
		}
		for _, date := range dates {
			names, err := base.DayDocuments(date)
			if err != nil {
				return "", err
			}
			for _, name := range names {
				uris = append(uris, sources.EventDocumentURI(date, name))
			}
		}
	case core.LayerIndex:
		names, err := base.IndexDocuments()
		if err != nil {
			return "", err
		}
		for _, name := range names {
			uris = append(uris, sources.IndexDocumentURI(name))
		}
	default:
		return "", fmt.Errorf("%s is not a collected graph input layer", layer)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		if err := checkContext(ctx); err != nil {
			return "", err
		}
		document, err := base.ReadDocumentContext(ctx, uri)
		if err != nil {
			return "", err
		}
		encoded, err := sources.EncodeDocument(document)
		if err != nil {
			return "", err
		}
		writeDigestValue(digest, []byte(uri))
		writeDigestValue(digest, encoded)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func graphSchemaInputSHA256(base *Base) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-input-schema-v1\x00"))
	for _, name := range base.Config.Schema.Names() {
		definition := base.Config.Schema[name]
		writeDigestValue(digest, []byte(name))
		writeDigestValue(digest, []byte(definition.Cardinality))
		if definition.Relation {
			writeDigestValue(digest, []byte("relation"))
		} else {
			writeDigestValue(digest, []byte("value"))
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func graphAuthoredInputSHA256(ctx context.Context, base *Base, layer core.Layer) (string, error) {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-input-" + string(layer) + "-v1\x00"))
	pages, err := graphInputPageURIs(ctx, base, layer)
	if err != nil {
		return "", err
	}
	for _, uri := range pages {
		if err := checkContext(ctx); err != nil {
			return "", err
		}
		data, err := base.ReadFileContext(ctx, uri, core.MaxNarrativeBytes)
		if err != nil {
			return "", fmt.Errorf("read graph input %s: %w", uri, err)
		}
		writeDigestValue(digest, []byte(uri))
		writeDigestValue(digest, data)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func graphInputPageURIs(ctx context.Context, base *Base, layer core.Layer) ([]string, error) {
	if !base.Store.Enabled(layer) {
		return nil, nil
	}
	var uris []string
	switch layer {
	case core.LayerProjects, core.LayerWiki:
		pages, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		for _, page := range pages {
			uris = append(uris, page.URI)
		}
	case core.LayerTasks:
		listing, err := ListTasks(ctx, base, Window{}, 0)
		if err != nil {
			return nil, err
		}
		for _, trace := range listing.Traces {
			uris = append(uris, trace.URI)
		}
	default:
		return nil, fmt.Errorf("%s is not an authored graph input layer", layer)
	}
	sort.Strings(uris)
	return uris, nil
}

type extractCounts struct{ documents, pages int }

// ExtractEdges derives every edge from local files. A graph build always walks the complete
// base: the edge list is a cache, and replacing it is the only strategy that also removes facts
// after a force re-collection or an authored link edit.
func ExtractEdges(ctx context.Context, base *Base) ([]Edge, extractCounts, error) {
	var (
		edges  []Edge
		counts extractCounts
	)
	dayEdges, dayCounts, err := eventEdges(ctx, base)
	if err != nil {
		return nil, counts, err
	}
	edges, counts = append(edges, dayEdges...), dayCounts
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return nil, counts, err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return nil, counts, err
			}
			document, err := base.ReadDocumentContext(ctx, sources.IndexDocumentURI(name))
			if err != nil {
				return nil, counts, err
			}
			documentRows, err := documentEdges(ctx, base, document)
			if err != nil {
				return nil, counts, err
			}
			edges = append(edges, documentRows...)
			counts.documents++
		}
	}
	pageCount, pageRows, err := markdownEdges(ctx, base)
	if err != nil {
		return nil, counts, err
	}
	counts.pages += pageCount
	edges = append(edges, pageRows...)
	return edges, counts, nil
}

// eventEdges transcribes collected documents.
func eventEdges(ctx context.Context, base *Base) ([]Edge, extractCounts, error) {
	var (
		edges  []Edge
		counts extractCounts
	)
	if !base.Store.Enabled(core.LayerEvents) {
		return nil, counts, nil
	}
	days, err := base.EventDates()
	if err != nil {
		return nil, counts, err
	}
	for _, date := range days {
		if err := checkContext(ctx); err != nil {
			return nil, counts, err
		}
		names, err := base.DayDocuments(date)
		if err != nil {
			return nil, counts, err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return nil, counts, err
			}
			document, err := base.ReadDocumentContext(ctx, sources.EventDocumentURI(date, name))
			if err != nil {
				return nil, counts, err
			}
			documentRows, err := documentEdges(ctx, base, document)
			if err != nil {
				return nil, counts, err
			}
			edges = append(edges, documentRows...)
			counts.documents++
		}
	}
	return edges, counts, nil
}

// documentEdges transcribes one collected document. Every edge comes from a relation path and
// semantic definition stored in the document at collection time.
func documentEdges(ctx context.Context, base *Base, document *sources.Document) ([]Edge, error) {
	edges := make([]Edge, 0, len(document.Records)*2)
	for _, record := range document.Records {
		recordEdges, err := edgesForRecord(ctx, base, document, record)
		if err != nil {
			return nil, err
		}
		edges = append(edges, recordEdges...)
	}
	return edges, nil
}

type recordEdgeCollector struct {
	ctx   context.Context
	base  *Base
	src   string
	at    string
	edges []Edge
}

func edgesForRecord(ctx context.Context, base *Base, document *sources.Document, record sources.Record) ([]Edge, error) {
	src, ok := document.RecordURI(record)
	if !ok {
		return nil, fmt.Errorf("%s has a record with no addressable identity", document.URI())
	}
	if _, err := ParseURI(src); err != nil {
		return nil, fmt.Errorf("record URI %q: %w", src, err)
	}
	values := map[string]any(record)
	collector := recordEdgeCollector{ctx: ctx, base: base, src: src, at: recordEdgeTime(document, values)}
	for _, name := range document.Schema.Names() {
		definition := document.Schema[name]
		if !definition.Relation {
			continue
		}
		resolved, err := document.Fields.EvalRelation(name, values)
		if err != nil {
			return nil, fmt.Errorf("derive field:%s from %s: %w", name, src, err)
		}
		if !definition.Cardinality.Allows(len(resolved)) {
			return nil, fmt.Errorf("derive field:%s from %s: selected %d values, cardinality %s does not allow that count",
				name, src, len(resolved), definition.Cardinality)
		}
		for _, destination := range resolved {
			if err := core.ValidateRelationValue(destination); err != nil {
				return nil, fmt.Errorf("derive field:%s from %s: %w", name, src, err)
			}
			if err := collector.add(destination, name, "field:"+name); err != nil {
				return nil, err
			}
		}
	}
	return collector.edges, nil
}

func recordEdgeTime(document *sources.Document, values map[string]any) string {
	raw, found := document.Fields.EvalString(core.FieldTime, values)
	if !found {
		return ""
	}
	parsed, err := sources.ParseRecordTime(raw)
	if err != nil {
		return ""
	}
	return parsed.Format(time.RFC3339)
}

func (collector *recordEdgeCollector) add(destination, kind, via string) error {
	parsed, err := ParseURI(destination)
	if err != nil {
		return fmt.Errorf("derive %s from %s: destination %q: %w", via, collector.src, destination, err)
	}
	if parsed.String() != destination {
		return fmt.Errorf("derive %s from %s: destination %q is not canonical; want %q",
			via, collector.src, destination, parsed.String())
	}
	if parsed.Scheme == SchemeFile {
		if _, err := collector.base.Store.Resolve(parsed.Path); err != nil {
			return fmt.Errorf("derive %s from %s: destination %q: %w", via, collector.src, destination, err)
		}
		if err := validateExistingRelationChild(collector.ctx, collector.base, parsed); err != nil {
			return fmt.Errorf("derive %s from %s: destination %q: %w", via, collector.src, destination, err)
		}
	}
	// A jq expression is a read transform over a node, never node identity. Authored links
	// already cross this boundary through NodeURI; collected relations must mint the same
	// graph destination or a query-bearing edge cannot be reached from `fkf graph <uri>`.
	edge := Edge{Src: collector.src, Dst: parsed.NodeURI(), Kind: kind, At: collector.at, Via: via}
	if err := edge.Valid(); err != nil {
		return fmt.Errorf("derive %s from %s: %w", via, collector.src, err)
	}
	collector.edges = append(collector.edges, edge)
	return nil
}

func markdownEdges(ctx context.Context, base *Base) (int, []Edge, error) {
	var edges []Edge
	count := 0
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if !base.Store.Enabled(layer) {
			continue
		}
		pages, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return 0, nil, err
		}
		for _, page := range pages {
			if err := checkContext(ctx); err != nil {
				return 0, nil, err
			}
			pageRows, err := pageEdges(ctx, base, page)
			if err != nil {
				return 0, nil, err
			}
			edges = append(edges, pageRows...)
			count++
		}
	}
	if base.Store.Enabled(core.LayerTasks) {
		listing, err := ListTasks(ctx, base, Window{}, 0)
		if err != nil {
			return 0, nil, err
		}
		for _, trace := range listing.Traces {
			if err := checkContext(ctx); err != nil {
				return 0, nil, err
			}
			page, err := ReadPageContext(ctx, base, trace.URI)
			if err != nil {
				return 0, nil, err
			}
			pageRows, err := pageEdges(ctx, base, page)
			if err != nil {
				return 0, nil, err
			}
			edges = append(edges, pageRows...)
			count++
		}
	}
	return count, edges, nil
}

// pageEdges transcribes one authored page: its tags, links outside code, and only relation
// lists explicitly nested under frontmatter `relations:`.
func pageEdges(ctx context.Context, base *Base, page Page) ([]Edge, error) {
	edges := make([]Edge, 0, len(page.Links)+len(page.Tags))
	at := page.Date
	add := func(dst, kind, via string) error {
		edge := Edge{Src: page.URI, Dst: dst, Kind: kind, At: at, Via: via}
		if err := edge.Valid(); err != nil {
			return fmt.Errorf("derive %s from %s: %w", via, page.URI, err)
		}
		edges = append(edges, edge)
		return nil
	}
	if err := addPageTagEdges(page, add); err != nil {
		return nil, err
	}
	if err := addPageLinkEdges(ctx, base, page, add); err != nil {
		return nil, err
	}
	if err := addPageRelationEdges(ctx, base, page, add); err != nil {
		return nil, err
	}
	sortEdgesForDeterminism(edges)
	return edges, nil
}

func addPageLinkEdges(ctx context.Context, base *Base, page Page, add func(string, string, string) error) error {
	for _, link := range page.Links {
		if target := strings.TrimSpace(link.Target); target == "" || strings.HasPrefix(target, "#") {
			continue
		}
		resolved, err := resolveAddressablePageLink(base, page.URI, link.Target)
		if err != nil {
			return fmt.Errorf("%s:%d: link %q: %w", page.URI, link.Line, link.Target, err)
		}
		if err := validateExistingRelationChild(ctx, base, resolved); err != nil {
			return fmt.Errorf("%s:%d: link %q: %w", page.URI, link.Line, link.Target, err)
		}
		if err := add(resolved.NodeURI(), EdgeLink, link.Via); err != nil {
			return err
		}
	}
	return nil
}

func addPageRelationEdges(
	ctx context.Context,
	base *Base,
	page Page,
	add func(string, string, string) error,
) error {
	relationNames := make([]string, 0, len(page.Relations))
	for name := range page.Relations {
		relationNames = append(relationNames, name)
	}
	sort.Strings(relationNames)
	for _, name := range relationNames {
		definition, declared := base.Config.Schema[name]
		if !declared {
			return fmt.Errorf("%s: frontmatter relations.%s is not declared in fkf.yaml schema", page.URI, name)
		}
		if !definition.Relation {
			return fmt.Errorf("%s: frontmatter relations.%s is not declared as a relation", page.URI, name)
		}
		values := page.Relations[name]
		if !definition.Cardinality.Allows(len(values)) {
			return fmt.Errorf("%s: frontmatter relations.%s has %d values; cardinality %s does not allow that count",
				page.URI, name, len(values), definition.Cardinality)
		}
		if err := addPageRelationValues(ctx, base, page, name, values, add); err != nil {
			return err
		}
	}
	return nil
}

func addPageRelationValues(
	ctx context.Context,
	base *Base,
	page Page,
	name string,
	values []string,
	add func(string, string, string) error,
) error {
	for _, candidate := range values {
		resolved, err := resolveAddressablePageLink(base, page.URI, candidate)
		if err != nil {
			return fmt.Errorf("%s: frontmatter relations.%s URI %q: %w", page.URI, name, candidate, err)
		}
		if err := core.ValidateRelationValue(resolved.NodeURI()); err != nil {
			return fmt.Errorf("%s: frontmatter relations.%s URI %q: %w", page.URI, name, candidate, err)
		}
		if err := validateExistingRelationChild(ctx, base, resolved); err != nil {
			return fmt.Errorf("%s: frontmatter relations.%s URI %q: %w", page.URI, name, candidate, err)
		}
		if err := add(resolved.NodeURI(), name, "frontmatter:relations."+name); err != nil {
			return err
		}
	}
	return nil
}

// validateExistingRelationChild preserves useful forward links to an addressable file that
// has not been written yet, but never invents a child below a file already present. The same
// non-executing read boundary as `fkf read` checks record IDs and Markdown anchors.
func validateExistingRelationChild(ctx context.Context, base *Base, uri URI) error {
	if uri.Scheme != SchemeFile || uri.Fragment == "" || !base.Exists(uri.Path) {
		return nil
	}
	if _, err := resolveRead(ctx, base, uri.NodeURI(), ReadOptions{}); err != nil {
		return fmt.Errorf("fragment does not name an addressable child: %w", err)
	}
	return nil
}

func addPageTagEdges(page Page, add func(dst, kind, via string) error) error {
	for _, tag := range page.Tags {
		destination, err := entityURIString(SchemeTag, tag)
		if err != nil {
			return fmt.Errorf("derive frontmatter:tags from %s: %w", page.URI, err)
		}
		if err := add(destination, EdgeTag, "frontmatter:tags"); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortEdgesForDeterminism(edges []Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Dst != edges[j].Dst {
			return edges[i].Dst < edges[j].Dst
		}
		return edges[i].Via < edges[j].Via
	})
}

// --- queries -------------------------------------------------------------------------------

// Direction selects which side of an edge a neighbourhood walk follows.
type Direction string

const (
	// DirectionOut follows edges away from the node: a prefix scan.
	DirectionOut Direction = "out"
	// DirectionIn follows edges into the node: backlinks, a contains scan.
	DirectionIn Direction = "in"
	// DirectionBoth follows either side.
	DirectionBoth Direction = "both"
)

// ParseDirection reads a direction name.
func ParseDirection(value string) (Direction, error) {
	switch Direction(strings.ToLower(strings.TrimSpace(value))) {
	case "", DirectionBoth:
		return DirectionBoth, nil
	case DirectionOut:
		return DirectionOut, nil
	case DirectionIn:
		return DirectionIn, nil
	default:
		return "", fmt.Errorf("direction %q must be in, out, or both", value)
	}
}

// GraphQuery bounds one neighbourhood walk.
type GraphQuery struct {
	URI       string
	Direction Direction
	Kind      string
	Depth     int
	// Offset replays but does not retain this many canonical traversal edges. It is an
	// internal continuation seam: the public CLI always starts at zero.
	Offset int
	Limit  int
}

// NeighbourEdge is one edge in a neighbourhood, with the hop that reached it.
type NeighbourEdge struct {
	Edge
	Hop int `json:"hop"`
}

// Neighbourhood is what `fkf graph <uri>` returns.
type Neighbourhood struct {
	URI       string          `json:"uri"`
	Direction Direction       `json:"direction"`
	Depth     int             `json:"depth"`
	Edges     []NeighbourEdge `json:"edges"`
	Nodes     []string        `json:"nodes"`
	Truncated bool            `json:"truncated,omitempty"`
	Stats     EdgeScanStats   `json:"stats"`
	// SnapshotSHA256 is the validated edge-list generation used for the complete walk. MCP
	// binds continuation cursors to it without adding an implementation field to public JSON.
	SnapshotSHA256 string `json:"-"`
	// Skipped is the number of canonical traversal edges consumed before this page. It lets a
	// continuation caller reject an offset beyond the complete neighbourhood without exposing
	// pagination machinery in public JSON.
	Skipped int `json:"-"`
}

// MaxGraphDepth bounds a walk. Three hops from a connected entity already reaches most of a base, and
// an unbounded walk is a way to defeat the token budget the read surface exists to enforce.
const MaxGraphDepth = 3

// Neighbours walks the edge list from one URI. Each hop is one linear scan, which is tens of
// milliseconds on a year of a busy base; the file is a cache, so the engine can change later
// with no migration if a measured query ever exceeds 200 ms.
func Neighbours(ctx context.Context, base *Base, query GraphQuery) (*Neighbourhood, error) {
	query, start, err := prepareGraphQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	cache, _, _, err := openValidatedGraphCache(ctx, base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cache.close() }()
	result, err := walkNeighbourhood(ctx, cache, query, start)
	if err != nil {
		return nil, err
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// neighboursFromCache walks one query against an already validated graph generation. A caller
// that composes several neighbourhoods, such as context expansion, owns one cache and performs
// one final byte revalidation after every query has completed.
func neighboursFromCache(ctx context.Context, cache *validatedGraphCache, query GraphQuery) (*Neighbourhood, error) {
	query, start, err := prepareGraphQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	return walkNeighbourhood(ctx, cache, query, start)
}

func prepareGraphQuery(ctx context.Context, query GraphQuery) (GraphQuery, URI, error) {
	if err := checkContext(ctx); err != nil {
		return query, URI{}, err
	}
	if query.Depth < 1 {
		query.Depth = 1
	}
	if query.Offset < 0 {
		return query, URI{}, fmt.Errorf("graph offset %d must be non-negative", query.Offset)
	}
	if query.Depth > MaxGraphDepth {
		return query, URI{}, fmt.Errorf("depth %d exceeds the maximum of %d; walk in steps and read what matters",
			query.Depth, MaxGraphDepth)
	}
	query.Kind = strings.TrimSpace(query.Kind)
	start, err := ParseURI(query.URI)
	if err != nil {
		return query, URI{}, err
	}
	return query, start, nil
}

func walkNeighbourhood(
	ctx context.Context, cache *validatedGraphCache, query GraphQuery, start URI,
) (*Neighbourhood, error) {
	if query.Kind != "" {
		observed, err := observedEdgeKinds(ctx, cache)
		if err != nil {
			return nil, err
		}
		if err := requireKnown("edge kind", []string{query.Kind}, observed); err != nil {
			return nil, err
		}
	}
	result := &Neighbourhood{
		URI: start.String(), Direction: query.Direction, Depth: query.Depth,
		Edges: []NeighbourEdge{}, Nodes: []string{}, SnapshotSHA256: cache.meta.SHA256.Outputs.GraphTSV,
	}
	walk := &walkState{
		result:   result,
		frontier: []string{start.NodeURI()},
		visited:  map[string]struct{}{start.NodeURI(): {}},
		seen:     map[[5]string]struct{}{},
		offset:   query.Offset,
	}
	for hop := 1; hop <= query.Depth && len(walk.frontier) > 0; hop++ {
		done, err := walk.step(ctx, cache, query, hop)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	sort.Strings(result.Nodes)
	result.Skipped = walk.skipped
	return result, nil
}

func observedEdgeKinds(ctx context.Context, cache *validatedGraphCache) ([]string, error) {
	kinds := map[string]struct{}{}
	_, err := cache.scan(ctx, EdgeQuery{}, func(edge Edge) error {
		kinds[edge.Kind] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(kinds)), nil
}

// requireCleanGraphStats is a second, narrow-scan guard after the full cache validation. It
// catches a row changed between validation and retrieval instead of returning a partial fact.
func requireCleanGraphStats(stats EdgeScanStats) error {
	if stats.Malformed == 0 {
		return nil
	}
	return fmt.Errorf("invalid derived graph cache: %s has %d malformed matching row(s); run `fkf build graph`",
		core.GraphFile, stats.Malformed)
}

// walkState is one breadth-first traversal. It lives in a struct so the per-hop step stays a
// short function: a walk that grew a fifth level of nesting is a walk nobody can check.
type walkState struct {
	result   *Neighbourhood
	frontier []string
	visited  map[string]struct{}
	seen     map[[5]string]struct{}
	offset   int
	skipped  int
}

// step follows one hop, reporting whether the limit ended the walk.
func (w *walkState) step(ctx context.Context, cache *validatedGraphCache, query GraphQuery, hop int) (bool, error) {
	next := make([]string, 0, len(w.frontier))
	for _, node := range w.frontier {
		if err := checkContext(ctx); err != nil {
			return false, err
		}
		edges, stats, err := scanNeighbours(ctx, cache, node, query)
		if err != nil {
			return false, err
		}
		w.result.Stats.Lines += stats.Lines
		w.result.Stats.Matched += stats.Matched
		w.result.Stats.Malformed += stats.Malformed
		for _, edge := range edges {
			if _, duplicate := w.seen[edge.sortKey()]; duplicate {
				continue
			}
			w.seen[edge.sortKey()] = struct{}{}
			if w.skipped < w.offset {
				w.skipped++
				next = append(next, w.discover(edge, false)...)
				continue
			}
			if query.Limit > 0 && len(w.result.Edges) >= query.Limit {
				w.result.Truncated = true
				return true, nil
			}
			w.result.Edges = append(w.result.Edges, NeighbourEdge{Edge: edge, Hop: hop})
			next = append(next, w.discover(edge, true)...)
		}
	}
	w.frontier = next
	return false, nil
}

// discover records the nodes an edge introduces and returns the ones worth following.
func (w *walkState) discover(edge Edge, report bool) []string {
	var found []string
	for _, side := range []string{edge.Src, edge.Dst} {
		if _, known := w.visited[side]; known {
			continue
		}
		w.visited[side] = struct{}{}
		if report {
			w.result.Nodes = append(w.result.Nodes, side)
		}
		found = append(found, side)
	}
	return found
}

func scanNeighbours(ctx context.Context, cache *validatedGraphCache, node string, query GraphQuery) ([]Edge, EdgeScanStats, error) {
	var queries []EdgeQuery
	switch query.Direction {
	case DirectionOut:
		queries = []EdgeQuery{{Src: node, Kind: query.Kind}}
	case DirectionIn:
		queries = []EdgeQuery{{Dst: node, Kind: query.Kind}}
	default:
		queries = []EdgeQuery{{Src: node, Kind: query.Kind}, {Dst: node, Kind: query.Kind}}
	}
	var (
		found []Edge
		total EdgeScanStats
	)
	for _, edgeQuery := range queries {
		stats, err := cache.scan(ctx, edgeQuery, func(edge Edge) error {
			found = append(found, edge)
			return nil
		})
		if err != nil {
			return nil, total, err
		}
		total.Lines += stats.Lines
		total.Matched += stats.Matched
		total.Malformed += stats.Malformed
	}
	SortEdges(found)
	return found, total, nil
}

// ErrDerivedMissing marks the one benign absence in the read path: the derived graph has not
// been built yet. A fresh clone legitimately has no cache; an entity read can still return its
// identity or use an explicitly requested resolver, while every other graph error propagates.
var ErrDerivedMissing = errors.New("derived file not built")

// validatedGraphCache keeps the file descriptor that was digest-validated. Atomic replacement
// can change the path while a multi-hop read is running, but an open descriptor continues to
// address one immutable generation, so every hop is drawn from the bytes the sidecar proved.
type validatedGraphCache struct {
	file *os.File
	meta EdgeListMeta
}

func (cache *validatedGraphCache) close() error { return cache.file.Close() }

func (cache *validatedGraphCache) scan(ctx context.Context, query EdgeQuery, visit func(Edge) error) (EdgeScanStats, error) {
	if _, err := cache.file.Seek(0, io.SeekStart); err != nil {
		return EdgeScanStats{}, fmt.Errorf("rewind the edge-list snapshot: %w", err)
	}
	return ScanEdges(ctx, cache.file, query, visit)
}

func (cache *validatedGraphCache) revalidateBytes(ctx context.Context) error {
	if _, err := cache.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind the edge-list snapshot: %w", err)
	}
	digest := sha256.New()
	bytes, err := io.Copy(digest, contextReader{ctx: ctx, reader: cache.file})
	if err != nil {
		return fmt.Errorf("revalidate the edge-list snapshot: %w", err)
	}
	if bytes != int64(cache.meta.Bytes) || hex.EncodeToString(digest.Sum(nil)) != cache.meta.SHA256.Outputs.GraphTSV {
		return fmt.Errorf("invalid derived graph cache: %s changed during the read; run `fkf build graph`",
			core.GraphFile)
	}
	return nil
}

func openGraphFile(base *Base) (*os.File, error) {
	absolute, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		return nil, err
	}
	file, err := core.OpenRegularFile(absolute)
	if errors.Is(err, os.ErrNotExist) {
		// Wrapped so a caller can tell "not built yet" from "built and unreadable". A fresh
		// clone has no derived file at all, which is expected; a corrupt one is not.
		return nil, fmt.Errorf(
			"%w: %s does not exist; run `fkf build graph` to derive it (it is a rebuildable cache, never a source of truth)",
			ErrDerivedMissing, core.GraphFile,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("open the edge list: %w", err)
	}
	return file, nil
}

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
	if uri == core.GraphFile || uri == core.GraphMetaFile {
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
	listing := &NodeListing{Kind: kind, Nodes: []NodeCount{}}
	counts := map[string]*NodeCount{}
	record := func(uri string, outgoing bool) {
		nodeKind := NodeKind(uri)
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
	stats, _, err := scanValidatedGraphCache(ctx, base, func(edge Edge) error {
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
	stats, meta, err := scanValidatedGraphCache(ctx, base, func(edge Edge) error {
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
	return summary, nil
}

// scanValidatedGraphCache performs the one full scan needed to prove that graph.tsv and its
// sidecar describe the same complete cache. Callers that need subsequent narrow scans use
// openValidatedGraphCache instead, keeping this exact validated file generation open.
func scanValidatedGraphCache(ctx context.Context, base *Base, visit func(Edge) error) (EdgeScanStats, EdgeListMeta, error) {
	cache, stats, meta, err := openValidatedGraphCacheWithVisit(ctx, base, visit)
	if cache != nil {
		_ = cache.close()
	}
	return stats, meta, err
}

func openValidatedGraphCache(ctx context.Context, base *Base) (*validatedGraphCache, EdgeScanStats, EdgeListMeta, error) {
	return openValidatedGraphCacheWithVisit(ctx, base, nil)
}

func openValidatedGraphCacheWithVisit(
	ctx context.Context, base *Base, visit func(Edge) error,
) (*validatedGraphCache, EdgeScanStats, EdgeListMeta, error) {
	var meta EdgeListMeta
	indexed := map[string]struct{}{}
	vias := map[string]struct{}{}
	edges := 0
	var previous [5]string
	havePrevious := false
	semanticProblem := ""
	file, err := openGraphFile(base)
	if err != nil {
		return nil, EdgeScanStats{}, meta, err
	}
	cache := &validatedGraphCache{file: file}
	digest := sha256.New()
	counter := byteCounter(0)
	stats, err := ScanEdges(ctx, io.TeeReader(cache.file, io.MultiWriter(digest, &counter)), EdgeQuery{}, func(edge Edge) error {
		edges++
		indexed[edge.Indexed] = struct{}{}
		vias[edge.Via] = struct{}{}
		if semanticProblem == "" {
			if err := validateCachedEdge(base, edge); err != nil {
				semanticProblem = err.Error()
			}
		}
		key := edge.sortKey()
		if semanticProblem == "" && havePrevious {
			switch compareEdgeSortKeys(previous, key) {
			case 0:
				semanticProblem = "graph rows contain a duplicate canonical edge"
			case 1:
				semanticProblem = "graph rows are not in canonical sort order"
			}
		}
		previous, havePrevious = key, true
		if visit != nil {
			return visit(edge)
		}
		return nil
	})
	if err != nil {
		_ = cache.close()
		return nil, stats, meta, err
	}
	problems := make([]string, 0, 2)
	if stats.Malformed > 0 {
		problems = append(problems, fmt.Sprintf("%s has %d malformed row(s)",
			core.GraphFile, stats.Malformed))
	}
	if semanticProblem != "" {
		problems = append(problems, semanticProblem)
	}
	meta, err = readGraphMeta(ctx, base)
	if err != nil {
		if ctxErr := checkContext(ctx); ctxErr != nil {
			_ = cache.close()
			return nil, stats, meta, ctxErr
		}
		problems = append(problems, err.Error())
	} else {
		problems = append(problems, graphMetaProblems(
			meta, edges, int(counter), hex.EncodeToString(digest.Sum(nil)), indexed, vias,
		)...)
		problems = append(problems, currentGraphInputProblems(ctx, base, meta)...)
		if ctxErr := checkContext(ctx); ctxErr != nil {
			_ = cache.close()
			return nil, stats, meta, ctxErr
		}
	}
	if len(problems) > 0 {
		_ = cache.close()
		return nil, stats, meta, fmt.Errorf("invalid derived graph cache: %s; run `fkf build graph`",
			strings.Join(problems, "; "))
	}
	cache.meta = meta
	return cache, stats, meta, nil
}

func currentGraphInputProblems(ctx context.Context, base *Base, meta EdgeListMeta) []string {
	inputsSHA256, err := graphInputSHA256(ctx, base)
	if err != nil {
		return []string{fmt.Sprintf("cannot validate current graph inputs: %v", err)}
	}
	problems := make([]string, 0, len(graphInputNames))
	for _, name := range graphInputNames {
		if meta.SHA256.Inputs.named(name) != inputsSHA256.named(name) {
			problems = append(problems, name+" input changed")
		}
	}
	return problems
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

func graphMetaProblems(
	meta EdgeListMeta, edges, bytes int, digest string,
	indexed, vias map[string]struct{},
) []string {
	problems := make([]string, 0, 9)
	if meta.SchemaVersion != EdgeSchemaVersion {
		problems = append(problems, fmt.Sprintf("metadata schema_version is %d, want %d",
			meta.SchemaVersion, EdgeSchemaVersion))
	}
	if meta.ExtractorVersion != GraphExtractorVersion {
		problems = append(problems, fmt.Sprintf("metadata extractor_version is %d, want %d",
			meta.ExtractorVersion, GraphExtractorVersion))
	}
	if !slices.Equal(meta.Columns, EdgeColumns) {
		problems = append(problems, fmt.Sprintf("metadata columns are %v, want %v", meta.Columns, EdgeColumns))
	}
	if meta.Separator != "\\t" {
		problems = append(problems, fmt.Sprintf("metadata separator is %q, want %q", meta.Separator, "\\t"))
	}
	problems = append(problems, graphInputDigestProblems(meta.SHA256.Inputs)...)
	if meta.Edges != edges {
		problems = append(problems, fmt.Sprintf("metadata edges is %d, but %s holds %d valid row(s)",
			meta.Edges, core.GraphFile, edges))
	}
	if meta.Bytes != bytes {
		problems = append(problems, fmt.Sprintf("metadata bytes is %d, but %s holds %d byte(s)",
			meta.Bytes, core.GraphFile, bytes))
	}
	if meta.SHA256.Outputs.GraphTSV != digest {
		problems = append(problems, fmt.Sprintf("metadata sha256.outputs[%q] %q does not match %s",
			core.GraphFile, meta.SHA256.Outputs.GraphTSV, core.GraphFile))
	}
	if observed := slices.Sorted(maps.Keys(vias)); !slices.Equal(meta.Extractors, observed) {
		problems = append(problems, fmt.Sprintf("metadata extractors are %v, but rows use %v",
			meta.Extractors, observed))
	}
	generatedAt, generatedErr := time.Parse(time.RFC3339, meta.GeneratedAt)
	if generatedErr != nil || meta.GeneratedAt != generatedAt.UTC().Format(time.RFC3339) {
		problems = append(problems, fmt.Sprintf("metadata generated_at %q is not RFC3339", meta.GeneratedAt))
	} else {
		for value := range indexed {
			if value != meta.GeneratedAt {
				problems = append(problems, fmt.Sprintf(
					"metadata generated_at %q does not match every indexed column", meta.GeneratedAt))
				break
			}
		}
	}
	return problems
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
