package services

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

const maxWhoRecent = 10

// WhoReport resolves a human spelling to every matching declared identity component.
type WhoReport struct {
	Query   string     `json:"query"`
	Matches []WhoMatch `json:"matches"`
}

// WhoMatch combines the durable page, canonical identity, graph neighbourhood, and recent
// stored interactions without executing a provider command.
type WhoMatch struct {
	Canonical              string              `json:"canonical"`
	Kind                   string              `json:"kind,omitempty"`
	Owner                  bool                `json:"owner,omitempty"`
	Aliases                []string            `json:"aliases"`
	Names                  []string            `json:"names,omitempty"`
	Pages                  []Page              `json:"pages,omitempty"`
	Neighbourhood          []WhoNeighbourGroup `json:"neighbourhood,omitempty"`
	NeighbourhoodTruncated bool                `json:"neighbourhood_truncated,omitempty"`
	Counts                 []SourceCount       `json:"counts"`
	Recent                 []FindRecord        `json:"recent"`
	Total                  int                 `json:"total"`
}

// WhoNeighbourGroup groups adjacent canonical nodes by their stable node classification.
type WhoNeighbourGroup struct {
	Kind  string   `json:"kind"`
	Nodes []string `json:"nodes"`
}

// Who answers one identity question from stored evidence and the validated derived graph.
func Who(ctx context.Context, base *Base, query string) (*WhoReport, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("who needs a name or URI")
	}
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	report := &WhoReport{Query: query, Matches: []WhoMatch{}}
	for _, identity := range resolver.Match(query) {
		match, err := buildWhoMatch(ctx, base, resolver, identity)
		if err != nil {
			return nil, err
		}
		report.Matches = append(report.Matches, match)
	}
	return report, nil
}

func buildWhoMatch(
	ctx context.Context,
	base *Base,
	resolver *IdentityResolver,
	identity ResolvedIdentity,
) (WhoMatch, error) {
	match := WhoMatch{
		Canonical: identity.Canonical, Kind: string(identity.Kind), Owner: identity.Owner,
		Aliases: append([]string(nil), identity.Aliases...), Names: append([]string(nil), identity.Names...),
		Counts: []SourceCount{}, Recent: []FindRecord{},
	}
	for _, uri := range identity.Pages {
		page, err := ReadPageContext(ctx, base, uri)
		if err != nil {
			return match, err
		}
		match.Pages = append(match.Pages, page)
	}
	result, err := Find(ctx, base, FindFilter{Grep: []string{identity.Canonical}, Limit: NoFindLimit}, false)
	if err != nil {
		return match, err
	}
	records := make([]FindRecord, 0, len(result.Records))
	direct := make(map[string]struct{}, len(result.Records))
	directNewestFirst := make([]string, 0, len(result.Records))
	for _, record := range result.Records {
		if !findRecordNamesIdentity(record, identity.Canonical, resolver) {
			continue
		}
		record.Record = nil
		records = append(records, record)
		direct[record.URI] = struct{}{}
		directNewestFirst = append(directNewestFirst, record.URI)
	}
	neighbourhood, linked, err := whoLinkedRecordURIs(
		ctx, base, identity.Canonical, direct, directNewestFirst,
	)
	if err != nil {
		return match, err
	}
	for _, uri := range linked {
		if _, found := direct[uri]; found {
			continue
		}
		record, err := readWhoRecord(ctx, base, uri, resolver)
		if err != nil {
			return match, err
		}
		records = append(records, record)
		direct[uri] = struct{}{}
	}
	SortFindRecords(records)
	match.Total = len(records)
	counts := map[string]int{}
	for _, record := range records {
		counts[record.Source]++
	}
	match.Recent = records
	if len(records) > maxWhoRecent {
		match.Recent = records[:maxWhoRecent]
	}
	for _, source := range sortedCountKeys(counts) {
		match.Counts = append(match.Counts, SourceCount{Source: source, Count: counts[source]})
	}
	match.Neighbourhood = groupWhoNeighbours(identity.Canonical, neighbourhood.Edges, resolver)
	match.NeighbourhoodTruncated = neighbourhood.Truncated
	return match, nil
}

// whoLinkedRecordURIs follows only one record-to-record edge from interactions that directly
// name the identity. That admits an attached meeting note without turning who into an
// unrestricted two-hop walk through every entity on those records.
func whoLinkedRecordURIs(
	ctx context.Context,
	base *Base,
	canonical string,
	direct map[string]struct{},
	directNewestFirst []string,
) (*Neighbourhood, []string, error) {
	cache, _, _, err := openValidatedGraphCache(ctx, base)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cache.close() }()

	neighbourhood, err := neighboursFromCache(ctx, cache, GraphQuery{
		URI: canonical, Direction: DirectionBoth, Depth: 1, Limit: expansionEdgeLimit,
	})
	if err != nil {
		return nil, nil, err
	}
	linked := map[string]struct{}{}
	seenEdges := map[[5]string]struct{}{}
	for index, seed := range directNewestFirst {
		if index >= expansionEdgeLimit || len(seenEdges) >= expansionEdgeLimit {
			neighbourhood.Truncated = true
			break
		}
		truncated, err := collectWhoAdjacentLinks(ctx, base, cache, seed, direct, linked, seenEdges)
		if err != nil {
			return nil, nil, err
		}
		if truncated {
			neighbourhood.Truncated = true
		}
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return nil, nil, err
	}
	return neighbourhood, sortedIdentitySet(linked), nil
}

func collectWhoAdjacentLinks(
	ctx context.Context,
	base *Base,
	cache *validatedGraphCache,
	seed string,
	direct, linked map[string]struct{},
	seenEdges map[[5]string]struct{},
) (bool, error) {
	adjacent, err := neighboursFromCache(ctx, cache, GraphQuery{
		URI: seed, Direction: DirectionBoth, Depth: 1, Limit: expansionEdgeLimit,
	})
	if err != nil {
		return false, err
	}
	truncated := adjacent.Truncated
	for _, neighbour := range adjacent.Edges {
		key := neighbour.sortKey()
		if _, seen := seenEdges[key]; seen {
			continue
		}
		if len(seenEdges) >= expansionEdgeLimit {
			return true, nil
		}
		seenEdges[key] = struct{}{}
		for _, endpoint := range []string{neighbour.Src, neighbour.Dst} {
			if endpoint == seed || !isWhoRecordURI(base, endpoint) {
				continue
			}
			if _, namesIdentity := direct[endpoint]; !namesIdentity {
				linked[endpoint] = struct{}{}
			}
		}
	}
	return truncated, nil
}

func isWhoRecordURI(base *Base, raw string) bool {
	uri, err := ParseURI(raw)
	if err != nil || uri.Scheme != SchemeFile || uri.Fragment == "" || uri.JQ != "" ||
		!strings.HasSuffix(uri.Path, ".json") {
		return false
	}
	layer, found := base.Store.LayerOf(uri.Path)
	return found && (layer == core.LayerEvents || layer == core.LayerIndex)
}

func readWhoRecord(
	ctx context.Context, base *Base, raw string, resolver *IdentityResolver,
) (FindRecord, error) {
	uri, err := ParseURI(raw)
	if err != nil {
		return FindRecord{}, err
	}
	document, err := base.ReadDocumentContext(ctx, uri.Path)
	if err != nil {
		return FindRecord{}, fmt.Errorf("read identity-linked record %s: %w", raw, err)
	}
	record, found := document.FindRecord(uri.Fragment)
	if !found {
		return FindRecord{}, fmt.Errorf("identity-linked document %s holds no record %q", uri.Path, uri.Fragment)
	}
	projected := project(document, record)
	projected.Record = nil
	canonicalizeFindRecord(&projected, resolver)
	return projected, nil
}

func findRecordNamesIdentity(record FindRecord, canonical string, resolver *IdentityResolver) bool {
	for _, value := range recordRelationValues(record) {
		if resolver.Canonical(value) == canonical {
			return true
		}
	}
	return false
}

func sortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func groupWhoNeighbours(canonical string, edges []NeighbourEdge, resolver *IdentityResolver) []WhoNeighbourGroup {
	byKind := map[string]map[string]struct{}{}
	for _, neighbour := range edges {
		other := neighbour.Dst
		if resolver.Canonical(other) == canonical {
			other = neighbour.Src
		}
		other = resolver.Canonical(other)
		if other == canonical {
			continue
		}
		kind := NodeKind(other)
		if identityKind := resolver.Kind(other); identityKind != "" {
			kind = string(identityKind)
		}
		if byKind[kind] == nil {
			byKind[kind] = map[string]struct{}{}
		}
		byKind[kind][other] = struct{}{}
	}
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	groups := make([]WhoNeighbourGroup, 0, len(kinds))
	for _, kind := range kinds {
		groups = append(groups, WhoNeighbourGroup{Kind: kind, Nodes: sortedIdentitySet(byKind[kind])})
	}
	return groups
}
