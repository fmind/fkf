package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

type baseTool[In any] func(context.Context, *services.Base, In) (any, int, error)

func bindBase[In any](base *services.Base, handler baseTool[In]) func(context.Context, In) (any, int, error) {
	return func(ctx context.Context, input In) (any, int, error) {
		return handler(ctx, base, input)
	}
}

func findTool(ctx context.Context, base *services.Base, input FindInput) (any, int, error) {
	window, err := services.ParseWindow(input.Since, input.Until, base.Now())
	if err != nil {
		return nil, 0, err
	}
	limit := capLimit(input.Limit)
	query := input
	query.Cursor, query.Limit = "", limit
	// A relative window is part of the effective query only after it resolves against the
	// base clock. Binding the cursor to raw "today" would let it cross midnight unnoticed.
	query.Since, query.Until = window.Since, window.Until
	cursor, err := openFindCursor(input.Cursor, query)
	if err != nil {
		return nil, 0, err
	}
	filter := services.FindFilter{
		Sources: input.Source, Window: window, Grep: input.Grep,
	}
	for _, value := range input.Layer {
		layer, err := core.ParseLayer(value)
		if err != nil {
			return nil, 0, err
		}
		filter.Layers = append(filter.Layers, layer)
	}
	for _, value := range input.Where {
		clause, err := services.ParseWhere(value)
		if err != nil {
			return nil, 0, err
		}
		filter.Where = append(filter.Where, clause)
	}
	bounded, err := services.FindBounded(ctx, base, filter, input.Count, limit, cursor.Position)
	if err != nil {
		return nil, 0, err
	}
	if err := cursor.bindSnapshot(bounded.SnapshotSHA256); err != nil {
		return nil, 0, err
	}
	nextCursor := ""
	if bounded.Next != nil {
		nextCursor, err = cursor.next(bounded.SnapshotSHA256, *bounded.Next)
		if err != nil {
			return nil, 0, err
		}
	}
	services.CompactFindResult(bounded.Result)
	encoded, err := pageJSON(bounded.Result, nextCursor)
	if err != nil {
		return nil, 0, err
	}
	return encoded, len(bounded.Result.Pages) + len(bounded.Result.Records) + len(bounded.Result.Volumes), nil
}

func contextTool(ctx context.Context, base *services.Base, input ContextInput) (any, int, error) {
	pack, err := services.BuildContext(ctx, base, services.ContextRequest{
		Query: input.Query, Window: services.Window{Since: input.Since, Until: input.Until}, Budget: input.Budget,
		Pins: input.Pin, Expand: input.Expand, Explain: input.Explain,
	})
	if err != nil {
		return nil, 0, err
	}
	if pack.GraphGenerationSHA256 != "" {
		return graphBoundResult(pack, pack.GraphGenerationSHA256), len(pack.Items), nil
	}
	return pack, len(pack.Items), nil
}

func dayTool(ctx context.Context, base *services.Base, input DayInput) (any, int, error) {
	report, err := services.Day(ctx, base, services.DayRequest{
		Date: input.Date, Budget: input.Budget, All: input.All,
		DeliveryFormat: services.DigestDeliveryCompactJSON,
	})
	if err != nil {
		return nil, 0, err
	}
	return report, report.Receipt.Selected, nil
}

func timelineTool(ctx context.Context, base *services.Base, input TimelineInput) (any, int, error) {
	var around time.Duration
	if input.Around != "" {
		parsed, err := time.ParseDuration(input.Around)
		if err != nil || parsed <= 0 {
			return nil, 0, fmt.Errorf("%w: around must be a positive duration such as 2h", core.ErrConfig)
		}
		around = parsed
	}
	report, err := services.Timeline(ctx, base, services.TimelineRequest{
		Window:  services.Window{Since: input.Since, Until: input.Until},
		Sources: input.Source, Repository: input.Repo, Person: input.Person,
		AroundURI: input.URI, Around: around, Budget: input.Budget, All: input.All,
		DeliveryFormat: services.DigestDeliveryCompactJSON,
	})
	if err != nil {
		return nil, 0, err
	}
	return report, report.Receipt.Selected, nil
}

func readTool(ctx context.Context, base *services.Base, input ReadInput) (any, int, error) {
	query := input
	query.Cursor = ""
	cursor, err := openPageCursor(input.Cursor, "read", query)
	if err != nil {
		return nil, 0, err
	}
	uri, err := services.ParseURI(input.URI)
	if err != nil {
		return nil, 0, err
	}
	limit := 0
	if uri.IsEntity() || uri.Scheme == services.SchemeExternal {
		limit = PageSize
	}
	// ReadOptions.Body is deliberately never set here: it runs the source's declared command,
	// and a server an agent drives must not be able to execute one.
	result, err := services.Read(ctx, base, input.URI, services.ReadOptions{Limit: limit, Offset: cursor.Offset})
	if err != nil {
		return nil, 0, err
	}
	switch result.Kind {
	case "directory":
		return readDirectoryPage(result, cursor)
	case "entity":
		return readEntityPage(result, cursor)
	default:
		if cursor.continued {
			return nil, 0, invalidCursor("read cursors apply only to directories and entities, not %s", result.Kind)
		}
		return result, 1, nil
	}
}

func readDirectoryPage(result *services.ReadResult, cursor pageCursor) (any, int, error) {
	snapshotSHA256, err := jsonSHA256(result)
	if err != nil {
		return nil, 0, err
	}
	entries, nextCursor, err := pageValues(cursor, snapshotSHA256, result.Entries, PageSize)
	if err != nil {
		return nil, 0, err
	}
	page := *result
	page.Entries = entries
	encoded, err := pageJSON(&page, nextCursor)
	return encoded, len(page.Entries), err
}

func readEntityPage(result *services.ReadResult, cursor pageCursor) (any, int, error) {
	if err := cursor.bindSnapshot(result.SnapshotSHA256); err != nil {
		return nil, 0, err
	}
	if result.Entity == nil {
		return nil, 0, errors.New("entity read returned no entity")
	}
	if cursor.continued && len(result.Entity.Neighbours) == 0 && !result.Entity.NeighbourCap {
		return nil, 0, invalidCursor("offset %d is outside the entity neighbourhood", cursor.Offset)
	}
	page := *result
	entity := *result.Entity
	page.Entity = &entity
	entity.Neighbours = result.Entity.Neighbours
	more := result.Entity.NeighbourCap
	entity.NeighbourCap = more
	nextCursor := ""
	var err error
	if more {
		nextCursor, err = cursor.next(result.SnapshotSHA256, cursor.Offset+len(entity.Neighbours))
		if err != nil {
			return nil, 0, err
		}
	}
	encoded, err := pageJSON(&page, nextCursor)
	if err != nil {
		return nil, 0, err
	}
	return graphBoundResult(encoded, result.SnapshotSHA256), len(entity.Neighbours), nil
}

func graphTool(ctx context.Context, base *services.Base, input GraphInput) (any, int, error) {
	direction, err := services.ParseDirection(input.Direction)
	if err != nil {
		return nil, 0, err
	}
	limit := capLimit(input.Limit)
	query := input
	query.Cursor, query.Limit = "", limit
	cursor, err := openPageCursor(input.Cursor, "graph", query)
	if err != nil {
		return nil, 0, err
	}
	neighbourhood, err := services.Neighbours(ctx, base, services.GraphQuery{
		URI: input.URI, Direction: direction, Kind: input.Kind,
		Depth: input.Depth, Offset: cursor.Offset, Limit: limit,
	})
	if err != nil {
		return nil, 0, err
	}
	if err := cursor.bindSnapshot(neighbourhood.SnapshotSHA256); err != nil {
		return nil, 0, err
	}
	if cursor.continued && neighbourhood.Skipped < cursor.Offset {
		return nil, 0, invalidCursor("offset %d is outside the graph neighbourhood", cursor.Offset)
	}
	if cursor.continued && len(neighbourhood.Edges) == 0 && !neighbourhood.Truncated {
		return nil, 0, invalidCursor("offset %d is outside the graph neighbourhood", cursor.Offset)
	}
	page := *neighbourhood
	page.Edges = neighbourhood.Edges
	more := neighbourhood.Truncated
	page.Truncated = more
	nextCursor := ""
	if more {
		nextCursor, err = cursor.next(neighbourhood.SnapshotSHA256, cursor.Offset+len(page.Edges))
		if err != nil {
			return nil, 0, err
		}
	}
	encoded, err := pageJSON(&page, nextCursor)
	if err != nil {
		return nil, 0, err
	}
	return graphBoundResult(encoded, neighbourhood.SnapshotSHA256), len(page.Edges), nil
}

type graphBoundToolResult struct {
	value      any
	generation string
}

func graphBoundResult(value any, generation string) graphBoundToolResult {
	return graphBoundToolResult{value: value, generation: generation}
}

func unwrapToolResult(value any) (any, string) {
	if bound, ok := value.(graphBoundToolResult); ok {
		return bound.value, bound.generation
	}
	return value, ""
}

func listLayer(ctx context.Context, base *services.Base, input ListInput) (any, int, error) {
	layer, err := core.ParseLayer(input.Layer)
	if err != nil {
		return nil, 0, err
	}
	if err := base.RequireLayer(layer); err != nil {
		return nil, 0, err
	}
	if err := validateListFilters(layer, input); err != nil {
		return nil, 0, err
	}
	limit := capLimit(input.Limit)
	query := input
	query.Cursor, query.Limit = "", limit
	cursor, err := openPageCursor(input.Cursor, "list", query)
	if err != nil {
		return nil, 0, err
	}
	switch layer {
	case core.LayerEvents:
		return listEventsPage(ctx, base, input, cursor, limit)
	case core.LayerIndex:
		return listIndexPage(ctx, base, cursor, limit)
	case core.LayerTasks:
		return listTasksPage(ctx, base, input, cursor, limit)
	default:
		return listPagesPage(ctx, base, layer, input, cursor, limit)
	}
}

func listEventsPage(
	ctx context.Context,
	base *services.Base,
	input ListInput,
	cursor pageCursor,
	limit int,
) (any, int, error) {
	window, err := services.ParseWindow(input.Since, input.Until, base.Now())
	if err != nil {
		return nil, 0, err
	}
	listing, err := services.ListEvents(ctx, base, window, input.Source, 0)
	if err != nil {
		return nil, 0, err
	}
	snapshotSHA256, err := jsonSHA256(listing)
	if err != nil {
		return nil, 0, err
	}
	page := *listing
	days, nextCursor, err := pageValues(cursor, snapshotSHA256, listing.Days, limit)
	if err != nil {
		return nil, 0, err
	}
	page.Days = days
	encoded, err := pageJSON(&page, nextCursor)
	return encoded, len(page.Days), err
}

func listIndexPage(ctx context.Context, base *services.Base, cursor pageCursor, limit int) (any, int, error) {
	listing, err := services.ListIndex(ctx, base, 0)
	if err != nil {
		return nil, 0, err
	}
	snapshotSHA256, err := jsonSHA256(listing)
	if err != nil {
		return nil, 0, err
	}
	page := *listing
	entries, nextCursor, err := pageValues(cursor, snapshotSHA256, listing.Entries, limit)
	if err != nil {
		return nil, 0, err
	}
	page.Entries = entries
	encoded, err := pageJSON(&page, nextCursor)
	return encoded, len(page.Entries), err
}

func listTasksPage(
	ctx context.Context,
	base *services.Base,
	input ListInput,
	cursor pageCursor,
	limit int,
) (any, int, error) {
	window, err := services.ParseWindow(input.Since, input.Until, base.Now())
	if err != nil {
		return nil, 0, err
	}
	listing, err := services.ListTasks(ctx, base, window, 0)
	if err != nil {
		return nil, 0, err
	}
	snapshotSHA256, err := jsonSHA256(listing)
	if err != nil {
		return nil, 0, err
	}
	page := *listing
	traces, nextCursor, err := pageValues(cursor, snapshotSHA256, listing.Traces, limit)
	if err != nil {
		return nil, 0, err
	}
	page.Traces = traces
	encoded, err := pageJSON(&page, nextCursor)
	return encoded, len(page.Traces), err
}

func listPagesPage(
	ctx context.Context,
	base *services.Base,
	layer core.Layer,
	input ListInput,
	cursor pageCursor,
	limit int,
) (any, int, error) {
	listing, err := services.ListPages(ctx, base, layer, services.PageFilter{
		Tags: input.Tag, Status: input.Status, Type: input.Type,
	})
	if err != nil {
		return nil, 0, err
	}
	snapshotSHA256, err := jsonSHA256(listing)
	if err != nil {
		return nil, 0, err
	}
	page := *listing
	pages, nextCursor, err := pageValues(cursor, snapshotSHA256, listing.Pages, limit)
	if err != nil {
		return nil, 0, err
	}
	page.Pages = pages
	encoded, err := pageJSON(&page, nextCursor)
	return encoded, len(page.Pages), err
}

func validateListFilters(layer core.Layer, input ListInput) error {
	invalid := make([]string, 0, 5)
	add := func(name string, present bool) {
		if present {
			invalid = append(invalid, name)
		}
	}
	window := input.Since != "" || input.Until != ""
	switch layer {
	case core.LayerEvents:
		add("tag", len(input.Tag) > 0)
		add("status", input.Status != "")
		add("type", input.Type != "")
	case core.LayerTasks:
		add("source", input.Source != "")
		add("tag", len(input.Tag) > 0)
		add("status", input.Status != "")
		add("type", input.Type != "")
	case core.LayerIndex:
		add("since/until", window)
		add("source", input.Source != "")
		add("tag", len(input.Tag) > 0)
		add("status", input.Status != "")
		add("type", input.Type != "")
	case core.LayerProjects:
		add("since/until", window)
		add("source", input.Source != "")
		add("type", input.Type != "")
	case core.LayerWiki:
		add("since/until", window)
		add("source", input.Source != "")
		add("status", input.Status != "")
	}
	if len(invalid) > 0 {
		return fmt.Errorf("%w: list %s does not accept %s", core.ErrConfig, layer, strings.Join(invalid, ", "))
	}
	return nil
}

func capLimit(requested int) int {
	if requested <= 0 || requested > PageSize {
		return PageSize
	}
	return requested
}
