package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

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
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, counts, err
	}
	edges = canonicalIdentityEdges(edges, resolver)
	return edges, counts, nil
}

func canonicalIdentityEdges(edges []Edge, resolver *IdentityResolver) []Edge {
	canonical := make([]Edge, 0, len(edges)+len(resolver.GraphAliases()))
	for _, edge := range edges {
		edge.Src = resolver.Canonical(edge.Src)
		edge.Dst = resolver.Canonical(edge.Dst)
		canonical = append(canonical, edge)
	}
	for _, alias := range resolver.GraphAliases() {
		if alias.Alias == alias.Canonical {
			continue
		}
		canonical = append(canonical, Edge{
			Src: alias.Alias, Dst: alias.Canonical, Kind: EdgeSameAs, Via: alias.Via,
		})
	}
	return canonical
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
