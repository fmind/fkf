package services

import (
	"context"
	"maps"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// gatherCandidates projects windowed event and index records, project and wiki pages, and task
// traces into one comparable shape. Records carry a bounded excerpt rather
// than the whole record, because the budget is the point and the record is one `fkf read` away.
// pinnableURIs is every wiki or projects page URI among the gathered candidates — exactly the
// vocabulary `--pin` is checked against and matched to later. A record or a task trace has no
// meaningful "pin", so neither contributes one.
func pinnableURIs(candidates []*ContextItem) []string {
	pages := make([]string, 0, len(candidates))
	for _, item := range candidates {
		if isPinnable(item) {
			pages = append(pages, item.URI)
		}
	}
	sort.Strings(pages)
	return pages
}

func isPinnable(item *ContextItem) bool {
	return item.Kind == string(core.LayerWiki) || item.Kind == string(core.LayerProjects)
}

func gatherCandidates(ctx context.Context, base *Base, request ContextRequest, asOf string) ([]*ContextItem, error) {
	var candidates []*ContextItem
	if base.Store.Enabled(core.LayerEvents) {
		// NoFindLimit, not the service default: with `Limit: 0` this asked for the newest 200
		// records whatever the window said, so a 30-day window was really a six-day one — and
		// the receipt printed the full window as if it had all been searched. Cost stays bounded
		// by the window (DefaultContextDays), which is the bound a reader can see.
		result, err := Find(ctx, base, FindFilter{Window: request.Window, Limit: NoFindLimit}, false)
		if err != nil {
			return nil, err
		}
		for _, record := range result.Records {
			candidates = append(candidates, recordCandidate(record, contextSchemaForSource(base.Config, record.Source)))
		}
	}
	// The index is unbounded by the window on purpose: an index document is the state
	// of things now, not something that happened on a day. Excluding it made `context` blind to
	// the most durable half of a real base — current inventories and catalogues — while the
	// graph indexed those same records and `read` resolved them.
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			document, err := base.ReadDocumentContext(ctx, sources.IndexDocumentURI(name))
			if err != nil {
				return nil, err
			}
			for _, record := range document.Records {
				projected := project(document, record)
				candidates = append(candidates, recordCandidate(projected, contextSchemaForSource(base.Config, projected.Source)))
			}
		}
	}
	for _, layer := range []core.Layer{core.LayerProjects, core.LayerWiki} {
		if !base.Store.Enabled(layer) {
			continue
		}
		pages, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		for _, page := range pages {
			if !page.ValidAt(asOf) {
				continue
			}
			candidates = append(candidates, pageCandidate(page, string(layer), base.Config.Schema))
		}
	}
	traces, err := traceCandidates(ctx, base, request.Window, asOf)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, traces...)
	applyContextSupersedes(candidates)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].URI < candidates[j].URI })
	return candidates, nil
}

// traceCandidates ranks task traces inside the same dated window as events. The
// implicit window ends today, so a trace written after the latest completed collection remains
// available; an explicit historical --until still means the same thing for every dated layer.
func traceCandidates(ctx context.Context, base *Base, window Window, asOf string) ([]*ContextItem, error) {
	if !base.Store.Enabled(core.LayerTasks) {
		return nil, nil
	}
	listing, err := ListTasks(ctx, base, window, 0)
	if err != nil {
		return nil, err
	}
	items := make([]*ContextItem, 0, len(listing.Traces))
	for _, trace := range listing.Traces {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		page := trace.page
		page.Date = trace.Date
		if !page.ValidAt(asOf) {
			continue
		}
		items = append(items, pageCandidate(page, string(core.LayerTasks), base.Config.Schema))
	}
	return items, nil
}

func pageKind(layer core.Layer) string {
	return string(layer)
}

func recordCandidate(record FindRecord, schema core.FieldSchema) *ContextItem {
	excerpt := truncateRunes(strings.Join(compact([]string{record.Title, record.URL}), " — "), excerptRunes)
	item := &ContextItem{
		URI: record.URI, Kind: "record", Source: record.Source, Date: record.Date,
		Time: record.Time, Title: record.Title, URL: record.URL,
		Fields: record.Fields, Excerpt: excerpt,
		relationFields: contextRecordRelationFields(record, schema),
	}
	identity := ""
	if parsed, err := ParseURI(record.URI); err == nil {
		identity = parsed.Fragment
	}
	item.addSegment(core.FieldID, identity, schema.Weight(core.FieldID))
	item.addIdentifier(record.URI)
	item.addIdentifier(identity)
	item.addSegment(core.FieldTitle, record.Title, schema.Weight(core.FieldTitle))
	item.addIdentifier(record.Title)
	item.addSegment(core.FieldURL, record.URL, schema.Weight(core.FieldURL))
	for _, name := range sortedContextFieldNames(record.Fields) {
		values := record.Fields[name]
		item.addSegment(name, strings.Join(values, " "), schema.Weight(name))
		if definition, found := schema[name]; found && definition.Relation {
			for _, value := range values {
				item.addEntityIdentifier(value)
			}
		}
	}
	category := firstContextFieldValue(record.Fields[core.FieldCategory])
	visibility := firstContextFieldValue(record.Fields[core.FieldVisibility])
	item.createdEvidence = strings.EqualFold(category, "created")
	if strings.EqualFold(category, "received") {
		item.defaultExcluded = core.FieldCategory + ":received"
	}
	if strings.EqualFold(visibility, "private") {
		item.defaultExcluded = core.FieldVisibility + ":private"
	}
	item.rebuildHaystack()
	item.Tokens = estimateTokens(item, false)
	return item
}

func contextRecordRelationFields(record FindRecord, schema core.FieldSchema) map[string]struct{} {
	if record.relations != nil {
		return maps.Clone(record.relations)
	}
	relations := map[string]struct{}{}
	for name, definition := range schema {
		if definition.Relation {
			relations[name] = struct{}{}
		}
	}
	return relations
}

func sortedContextFieldNames(fields map[string][]string) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pageCandidate projects a Markdown page without inferring identifiers from prose.
func pageCandidate(page Page, kind string, schema core.FieldSchema) *ContextItem {
	item := &ContextItem{
		URI: page.URI, Kind: kind, Title: page.Title, Status: page.Status,
		Tags: page.Tags, Date: page.Date, Fields: page.Relations, body: page.Body,
		validityRank: page.ValidFrom, supersedes: append([]string(nil), page.Relations["supersedes"]...),
		relationFields: map[string]struct{}{},
	}
	if item.validityRank == "" {
		item.validityRank = page.Date
	}
	item.addSegment(core.FieldID, page.Slug, schema.Weight(core.FieldID))
	item.addIdentifier(page.URI)
	item.addIdentifier(page.Slug)
	item.addSegment(core.FieldTitle, page.Title, schema.Weight(core.FieldTitle))
	item.addIdentifier(page.Title)
	item.addSegment("description", page.Description, core.DefaultFieldWeight)
	item.addSegment("type", page.Type, core.DefaultFieldWeight)
	item.addSegment("tags", strings.Join(page.Tags, " "), core.DefaultFieldWeight)
	item.addSegment("body", page.Body, core.DefaultFieldWeight)
	for _, name := range sortedContextFieldNames(page.Relations) {
		values := page.Relations[name]
		item.addSegment(name, strings.Join(values, " "), schema.Weight(name))
		if definition, found := schema[name]; found && definition.Relation {
			item.relationFields[name] = struct{}{}
			for _, value := range values {
				item.addEntityIdentifier(value)
			}
		}
	}
	item.rebuildHaystack()
	item.Tokens = estimateTokens(item, false)
	return item
}
