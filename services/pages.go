package services

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
)

// wiki/ and projects/ are the same layer with different required frontmatter, so they share
// the parser, the listing, and the validator. A flat layer has no directories to navigate by,
// which is why the tag index is the navigation surface rather than a convenience.

// PageListing is what `fkf list wiki` and `fkf list projects` return.
type PageListing struct {
	Layer core.Layer `json:"layer"`
	Pages []Page     `json:"pages"`
	Total int        `json:"total"`
}

// PageFilter narrows a listing.
type PageFilter struct {
	Tags   []string
	Status string
	Type   string
	Limit  int
}

// ListPages returns the pages of a flat Markdown layer, filtered and in slug order.
func ListPages(ctx context.Context, base *Base, layer core.Layer, filter PageFilter) (*PageListing, error) {
	pages, _, err := loadMarkdownLayer(ctx, base, layer)
	if err != nil {
		return nil, err
	}
	// `--status wibble` used to return "0 pages", exit 0 — silently indistinguishable from
	// zero paused projects. Status is a closed three-value set regardless of what is in this
	// base, so it is checked before the pages a typo would have hidden from the caller.
	if filter.Status != "" {
		if err := requireKnown("status", []string{filter.Status}, ProjectStatuses); err != nil {
			return nil, err
		}
	}
	// Tags are this base's own vocabulary rather than a fixed set, so they are checked against
	// what these very pages already carry — no second scan. The check folds case the same way
	// hasTag's actual match does below, or a validly-cased filter value could be refused as
	// unknown right before the match that would have accepted it.
	if len(filter.Tags) > 0 {
		known, display := map[string]bool{}, presentTags(pages)
		for _, tag := range display {
			known[strings.ToLower(tag)] = true
		}
		for _, wanted := range filter.Tags {
			if !known[strings.ToLower(strings.TrimSpace(wanted))] {
				return nil, unknownValueError("tag", wanted, display)
			}
		}
	}
	listing := &PageListing{Layer: layer, Pages: []Page{}}
	for _, page := range pages {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !matchesFilter(page, filter) {
			continue
		}
		page.Body, page.Links, page.Headings = "", nil, nil
		listing.Pages = append(listing.Pages, page)
	}
	listing.Total = len(listing.Pages)
	if filter.Limit > 0 && len(listing.Pages) > filter.Limit {
		listing.Pages = listing.Pages[:filter.Limit]
	}
	return listing, nil
}

// matchesFilter requires every requested tag rather than any of them. Tags narrow a flat
// namespace, and an any-match filter over a real vocabulary returns almost everything.
func matchesFilter(page Page, filter PageFilter) bool {
	if filter.Status != "" && page.Status != filter.Status {
		return false
	}
	if filter.Type != "" && page.Type != filter.Type {
		return false
	}
	for _, wanted := range filter.Tags {
		if !hasTag(page.Tags, wanted) {
			return false
		}
	}
	return true
}

// presentTags is the sorted, deduped set of tags these already-loaded pages carry, in the
// casing they were first authored with — display form for the refusal message, not the fold
// used to compare against it.
func presentTags(pages []Page) []string {
	seen := map[string]string{}
	for _, page := range pages {
		for _, tag := range page.Tags {
			key := strings.ToLower(strings.TrimSpace(tag))
			if _, exists := seen[key]; !exists {
				seen[key] = tag
			}
		}
	}
	tags := make([]string, 0, len(seen))
	for _, display := range seen {
		tags = append(tags, display)
	}
	sort.Strings(tags)
	return tags
}

func hasTag(tags []string, wanted string) bool {
	wanted = strings.ToLower(strings.TrimSpace(wanted))
	for _, tag := range tags {
		if strings.ToLower(tag) == wanted {
			return true
		}
	}
	return false
}

// ReadPageBySlug returns one page of a flat layer.
func ReadPageBySlug(base *Base, layer core.Layer, slug string) (Page, error) {
	return ReadPageBySlugContext(context.Background(), base, layer, slug)
}

// ReadPageBySlugContext returns one page of a flat layer with cooperative cancellation.
func ReadPageBySlugContext(ctx context.Context, base *Base, layer core.Layer, slug string) (Page, error) {
	slug = strings.TrimSuffix(strings.TrimSpace(slug), core.MarkdownExtension)
	if slug == "" {
		return Page{}, fmt.Errorf("a %s page is addressed by its slug, for example retrieval-boundary", layer)
	}
	return ReadPageContext(ctx, base, path.Join(string(layer), slug+core.MarkdownExtension))
}

// TagCount is one tag and the pages carrying it.
type TagCount struct {
	Tag   string   `json:"tag"`
	Count int      `json:"count"`
	Pages []string `json:"pages"`
}

// TagVocabulary is a layer's complete tag vocabulary with its usage.
type TagVocabulary struct {
	Layer    core.Layer `json:"layer"`
	Tags     []TagCount `json:"tags"`
	Untagged []string   `json:"untagged,omitempty"`
	Pages    int        `json:"pages"`
}

// BuildTagVocabulary groups a layer's pages by tag, most-used first. The histogram is what makes a
// vocabulary legible, and reusing it is what stops a wiki growing four spellings of one idea.
func BuildTagVocabulary(ctx context.Context, base *Base, layer core.Layer) (*TagVocabulary, error) {
	pages, _, err := loadMarkdownLayer(ctx, base, layer)
	if err != nil {
		return nil, err
	}
	index := &TagVocabulary{Layer: layer, Tags: []TagCount{}, Pages: len(pages)}
	grouped := map[string][]string{}
	for _, page := range pages {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if len(page.Tags) == 0 {
			index.Untagged = append(index.Untagged, page.Slug)
			continue
		}
		for _, tag := range page.Tags {
			normalized := strings.ToLower(strings.TrimSpace(tag))
			grouped[normalized] = append(grouped[normalized], page.Slug)
		}
	}
	for tag, slugs := range grouped {
		sort.Strings(slugs)
		index.Tags = append(index.Tags, TagCount{Tag: tag, Count: len(slugs), Pages: slugs})
	}
	sort.Slice(index.Tags, func(i, j int) bool {
		if index.Tags[i].Count == index.Tags[j].Count {
			return index.Tags[i].Tag < index.Tags[j].Tag
		}
		return index.Tags[i].Count > index.Tags[j].Count
	})
	sort.Strings(index.Untagged)
	return index, nil
}

// SearchHit is one Markdown document matching a search, with the excerpt that explains why.
// Layer and Date are carried on the hit rather than only on the enclosing result because
// `fkf find` returns hits from several layers at once and a reader has to tell them apart.
type SearchHit struct {
	URI     string     `json:"uri"`
	Layer   core.Layer `json:"layer,omitempty"`
	Slug    string     `json:"slug"`
	Title   string     `json:"title,omitempty"`
	Type    string     `json:"type,omitempty"`
	Date    string     `json:"date,omitempty"`
	Tags    []string   `json:"tags,omitempty"`
	Score   int        `json:"score"`
	Matched []string   `json:"matched"`
	Excerpt string     `json:"excerpt,omitempty"`
}

// SearchResult is the layer-scoped lexical result used by the universal `fkf find` command.
type SearchResult struct {
	Layer core.Layer  `json:"layer"`
	Terms []string    `json:"terms"`
	Hits  []SearchHit `json:"hits"`
}

// SearchPages is a lexical, deterministic scan: a title or tag match outweighs a body match,
// and every hit reports which terms matched. There is no ranking model and no index engine —
// a flat layer of a few hundred pages is a scan, and a scan is explainable.
func SearchPages(ctx context.Context, base *Base, layer core.Layer, terms []string, filter PageFilter) (*SearchResult, error) {
	pages, _, err := loadMarkdownLayer(ctx, base, layer)
	if err != nil {
		return nil, err
	}
	normalized := normalizeTerms(terms)
	if len(normalized) == 0 {
		return nil, errors.New("search needs at least one term")
	}
	result := &SearchResult{Layer: layer, Terms: normalized, Hits: []SearchHit{}}
	for _, page := range pages {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !matchesFilter(page, filter) {
			continue
		}
		if hit, ok := scorePage(page, normalized); ok {
			hit.Layer = layer
			result.Hits = append(result.Hits, hit)
		}
	}
	SortSearchHits(result.Hits)
	if filter.Limit > 0 && len(result.Hits) > filter.Limit {
		result.Hits = result.Hits[:filter.Limit]
	}
	return result, nil
}

// normalizeTerms lowercases and drops the empties once, so every lexical caller compares the
// same way. `fkf find` assembles its terms from four flags and would otherwise repeat this.
func normalizeTerms(terms []string) []string {
	normalized := make([]string, 0, len(terms))
	for _, term := range terms {
		if trimmed := strings.ToLower(strings.TrimSpace(term)); trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return normalized
}

func scorePage(page Page, terms []string) (SearchHit, bool) {
	haystackTitle := strings.ToLower(page.Title + " " + page.Slug + " " + page.Description)
	haystackTags := strings.ToLower(strings.Join(page.Tags, " "))
	haystackBody := strings.ToLower(page.Body)
	hit := SearchHit{
		URI: page.URI, Slug: page.Slug, Title: page.Title,
		Type: page.Type, Tags: page.Tags, Matched: []string{},
	}
	for _, term := range terms {
		points := 0
		switch {
		case strings.Contains(haystackTitle, term):
			points = 10
		case strings.Contains(haystackTags, term):
			points = 6
		case strings.Contains(haystackBody, term):
			points = 2
		}
		if points == 0 {
			// Grep terms are conjunctive across records and pages. Returning a partial page
			// match here made one command silently switch from AND to OR by layer.
			return SearchHit{}, false
		}
		hit.Score += points
		hit.Matched = append(hit.Matched, term)
		if hit.Excerpt == "" {
			hit.Excerpt = excerptAround(page.Body, term)
		}
	}
	return hit, true
}

// excerptAround returns a bounded window of the body around the first match, so a hit can be
// judged without opening the page.
func excerptAround(body, term string) string {
	foldedBody := strings.ToLower(body)
	byteIndex := strings.Index(foldedBody, term)
	if byteIndex < 0 {
		return ""
	}
	// Unicode lowercasing preserves rune positions, but not byte positions. Slice the original
	// text by rune index so a case-folded match cannot produce invalid or reversed byte bounds.
	bodyRunes := []rune(body)
	matchIndex := utf8.RuneCountInString(foldedBody[:byteIndex])
	matchLength := utf8.RuneCountInString(term)
	const radius = 90
	start, end := max(0, matchIndex-radius), min(len(bodyRunes), matchIndex+matchLength+radius)
	excerpt := strings.Join(strings.Fields(string(bodyRunes[start:end])), " ")
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if end < len(bodyRunes) {
		excerpt += "…"
	}
	return excerpt
}
