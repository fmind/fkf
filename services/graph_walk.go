package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

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
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	query.URI = resolver.Canonical(query.URI)
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
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return slices.Clone(cache.meta.Kinds), nil
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
	file    *os.File
	dst     *os.File
	offsets *os.File
	meta    EdgeListMeta
	files   []graphFileSnapshot
}

type graphFileSnapshot struct {
	uri              string
	file             *os.File
	bytes            int64
	modifiedUnixNano int64
}

func (cache *validatedGraphCache) close() error {
	var failures []error
	for _, file := range []*os.File{cache.file, cache.dst, cache.offsets} {
		if file != nil {
			failures = append(failures, file.Close())
		}
	}
	return errors.Join(failures...)
}

func (cache *validatedGraphCache) scan(ctx context.Context, query EdgeQuery, visit func(Edge) error) (EdgeScanStats, error) {
	// Some internal callers deliberately wrap a single immutable graph.tsv descriptor. Keep
	// that seam useful while production caches use the seek artifacts for narrow walks.
	if query.Src != "" && cache.offsets != nil {
		return cache.scanRange(ctx, cache.file, "src", query.Src, query, visit)
	}
	if query.Dst != "" && cache.dst != nil && cache.offsets != nil {
		return cache.scanRange(ctx, cache.dst, "dst", query.Dst, query, visit)
	}
	if _, err := cache.file.Seek(0, io.SeekStart); err != nil {
		return EdgeScanStats{}, fmt.Errorf("rewind the edge-list snapshot: %w", err)
	}
	return ScanEdges(ctx, cache.file, query, visit)
}

func (cache *validatedGraphCache) revalidateBytes(ctx context.Context) error {
	for _, snapshot := range cache.files {
		if err := checkContext(ctx); err != nil {
			return err
		}
		info, err := snapshot.file.Stat()
		if err != nil {
			return fmt.Errorf("revalidate %s snapshot: %w", snapshot.uri, err)
		}
		if info.Size() != snapshot.bytes || info.ModTime().UnixNano() != snapshot.modifiedUnixNano {
			return fmt.Errorf("invalid derived graph cache: %s changed during the read; run `fkf build graph`",
				snapshot.uri)
		}
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
