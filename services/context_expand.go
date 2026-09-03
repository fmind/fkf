package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

// applyExpansion follows one hop from the strongest matches through every entity edge. What it
// reaches is usually already a candidate — the window gathered it — so the hop mostly
// RESCORES: an item that shares any declared relation with a top hit gets a fixed discount and
// a named reason, which is often what lifts it over the floor. An item the window did not
// gather is loaded and added. It is opt-in because it widens the pack,
// and cheap because the graph makes it a prefix scan rather than a join engine.
func applyExpansion(
	ctx context.Context,
	base *Base,
	candidates []*ContextItem,
	request ContextRequest,
	resolver *IdentityResolver,
	asOf string,
) ([]*ContextItem, []string, string, error) {
	byURI := make(map[string]*ContextItem, len(candidates))
	for _, candidate := range candidates {
		byURI[candidate.URI] = candidate
	}
	ranked := make([]*ContextItem, len(candidates))
	copy(ranked, candidates)
	sortCandidates(ranked)
	// Preserve the no-op contract for an empty or wholly irrelevant candidate set: with no
	// seed there is no graph question to ask and therefore no derived cache to require.
	if len(ranked) == 0 || ranked[0].Score < relevanceFloor {
		return nil, nil, "", nil
	}
	cache, _, _, err := openValidatedGraphCache(ctx, base)
	if err != nil {
		return nil, nil, "", err
	}
	defer func() { _ = cache.close() }()

	seeds, entities, err := expansionSeedsOf(ctx, cache, ranked, resolver)
	if err != nil {
		return nil, nil, "", err
	}
	var added []*ContextItem
	var truncatedEntities []string
	for _, entity := range entities {
		if err := checkContext(ctx); err != nil {
			return nil, nil, "", err
		}
		edges, truncated, err := newestInboundExpansionEdges(ctx, cache, entity, request.Window)
		if err != nil {
			return nil, nil, "", err
		}
		if truncated {
			truncatedEntities = append(truncatedEntities, entity)
		}
		for _, edge := range edges {
			item, fresh, err := reachedItem(ctx, base, byURI, seeds, edge.Src, request.Window, asOf)
			if err != nil {
				return nil, nil, "", fmt.Errorf("load graph target %s: %w", edge.Src, err)
			}
			if item == nil {
				continue
			}
			if fresh {
				added = append(added, item)
			}
			if item.expanded {
				continue
			}
			item.expanded = true
			item.addReason("join-expansion", pointsExpansion, "one hop through "+entity)
		}
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return nil, nil, "", err
	}
	sort.Slice(added, func(i, j int) bool { return added[i].URI < added[j].URI })
	sort.Strings(truncatedEntities)
	return added, truncatedEntities, cache.meta.SHA256.Outputs.GraphTSV, nil
}

func newestInboundExpansionEdges(
	ctx context.Context, cache *validatedGraphCache, entity string, window Window,
) ([]Edge, bool, error) {
	edges := make([]Edge, 0, expansionEdgeLimit+1)
	stats, err := cache.scan(ctx, EdgeQuery{Dst: entity}, func(edge Edge) error {
		if edge.At != "" {
			date := edge.At
			if len(date) >= len(time.DateOnly) {
				date = date[:len(time.DateOnly)]
			}
			if !window.Contains(date) {
				return nil
			}
		}
		edges = append(edges, edge)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if err := requireCleanGraphStats(stats); err != nil {
		return nil, false, err
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].At != edges[j].At {
			return edges[i].At > edges[j].At
		}
		if edges[i].Src != edges[j].Src {
			return edges[i].Src < edges[j].Src
		}
		return edges[i].Kind < edges[j].Kind
	})
	truncated := len(edges) > expansionEdgeLimit
	if truncated {
		edges = edges[:expansionEdgeLimit]
	}
	return edges, truncated, nil
}

// expansionSeedsOf collects the strongest matches and the entities they point at.
func expansionSeedsOf(
	ctx context.Context, cache *validatedGraphCache, ranked []*ContextItem, resolver *IdentityResolver,
) (map[string]struct{}, []string, error) {
	seeds := map[string]struct{}{}
	var entities []string
	for index, candidate := range ranked {
		if err := checkContext(ctx); err != nil {
			return nil, nil, err
		}
		if index >= expansionSeeds || candidate.Score < relevanceFloor {
			break
		}
		seeds[candidate.URI] = struct{}{}
		neighbourhood, err := neighboursFromCache(ctx, cache, GraphQuery{
			URI: candidate.URI, Direction: DirectionOut, Depth: 1, Limit: expansionEdgeLimit,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := requireCompleteExpansion(neighbourhood, candidate.URI); err != nil {
			return nil, nil, err
		}
		for _, edge := range neighbourhood.Edges {
			uri, parseErr := ParseURI(edge.Dst)
			if parseErr == nil && uri.IsEntity() && !resolver.IsOwner(edge.Dst) {
				entities = appendUnique(entities, edge.Dst)
			}
		}
	}
	sort.Strings(entities)
	return seeds, entities, nil
}

func requireCompleteExpansion(neighbourhood *Neighbourhood, from string) error {
	if err := requireCleanGraphStats(neighbourhood.Stats); err != nil {
		return err
	}
	if neighbourhood.Truncated {
		return fmt.Errorf("graph expansion from %s exceeds the %d-edge safety limit; narrow the query or omit --expand rather than use a partial join",
			from, expansionEdgeLimit)
	}
	return nil
}

// reachedItem returns the candidate one hop reached, loading it when the window did not gather
// it, and reports whether it is new. A seed is never expanded into itself.
func reachedItem(
	ctx context.Context,
	base *Base,
	byURI map[string]*ContextItem,
	seeds map[string]struct{},
	uri string,
	window Window,
	asOf string,
) (item *ContextItem, fresh bool, err error) {
	if _, isSeed := seeds[uri]; isSeed {
		return nil, false, nil
	}
	if known, ok := byURI[uri]; ok {
		return known, false, nil
	}
	loaded, err := expandedCandidate(ctx, base, uri, window, asOf)
	if err != nil {
		return nil, false, err
	}
	if loaded == nil {
		return nil, false, nil
	}
	byURI[uri] = loaded
	return loaded, true, nil
}

func expandedCandidate(
	ctx context.Context, base *Base, uri string, window Window, asOf string,
) (*ContextItem, error) {
	parsed, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != SchemeFile {
		return nil, fmt.Errorf("%s is not a stored page or document URI", uri)
	}
	if strings.HasSuffix(parsed.Path, core.MarkdownExtension) {
		if parsed.Fragment != "" {
			return nil, fmt.Errorf("graph target %s is a page fragment, not a page node", uri)
		}
		page, err := ReadPageContext(ctx, base, parsed.Path)
		if err != nil {
			return nil, err
		}
		if !page.ValidAt(asOf) {
			return nil, nil
		}
		layer, _ := base.Store.LayerOf(parsed.Path)
		if layer == core.LayerTasks {
			// Task dates are structural path metadata, not frontmatter. ListTasks supplies the
			// same value during index construction, so revalidation must reconstruct it too.
			parts := strings.Split(parsed.Path, "/")
			if len(parts) == 4 {
				page.Date = parts[1]
			}
		}
		return pageCandidate(page, pageKind(layer), base.Config.Schema), nil
	}
	document, err := base.ReadDocumentContext(ctx, parsed.Path)
	if err != nil {
		return nil, err
	}
	if document.Date != "" && !window.Contains(document.Date) {
		return nil, nil
	}
	record, found := document.FindRecord(parsed.Fragment)
	if !found {
		return nil, fmt.Errorf("%s holds no record with id %q; rebuild the graph", parsed.Path, parsed.Fragment)
	}
	projected := project(document, record)
	return recordCandidate(projected, contextSchemaForSource(base.Config, projected.Source)), nil
}
