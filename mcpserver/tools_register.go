package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// readOnlyAnnotations is the same four hints on all seven tools, because all seven ARE the same
// four things: read-only by construction (there is no tool that writes, shells, or fetches),
// side-effect-free so calling one twice is calling it once (idempotent), never destructive, and
// never reaching outside this one base (not an open world — no network at read time). A client
// that trusts hints can auto-approve every call without asking, which is the whole point of a
// server whose entire thesis is "read-only" being declared as machine-readable fact rather than
// left for a human to take on faith from the package doc comment.
func readOnlyAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint: true, IdempotentHint: true,
		DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false),
	}
}

func boolPtr(value bool) *bool { return &value }

func register(server *mcp.Server, base *services.Base) error {
	findSchema, err := boundedInputSchema[FindInput](inputDomains{
		numeric: map[string]numericDomain{"limit": {minimum: 0}},
		strings: map[string]int{
			"since": maxInputTextLength, "until": maxInputTextLength, "cursor": maxInputTextLength,
		},
		arrays: map[string]arrayDomain{
			"source": {maxItems: maxRepeatedInputs, itemMaxLength: core.MaxSourceNameLength},
			"grep":   {maxItems: maxRepeatedInputs, itemMaxLength: maxInputTextLength},
			"where":  {maxItems: maxRepeatedInputs, itemMaxLength: maxInputTextLength},
			"layer":  {maxItems: maxRepeatedInputs, itemMaxLength: maxInputTextLength},
		},
	})
	if err != nil {
		return err
	}
	contextSchema, err := boundedInputSchema[ContextInput](inputDomains{
		numeric: map[string]numericDomain{
			"budget": {minimum: 1, maximum: float64(maxContextBudget), hasMaximum: true},
		},
		strings: map[string]int{
			"query": maxInputTextLength, "since": maxInputTextLength, "until": maxInputTextLength,
		},
		arrays: map[string]arrayDomain{
			"pin": {maxItems: maxRepeatedInputs, itemMaxLength: maxInputTextLength},
		},
	})
	if err != nil {
		return err
	}
	daySchema, err := boundedInputSchema[DayInput](inputDomains{
		numeric: map[string]numericDomain{
			"budget": {minimum: 1, maximum: float64(services.MaxDigestBudget), hasMaximum: true},
		},
		strings: map[string]int{"date": maxInputTextLength},
	})
	if err != nil {
		return err
	}
	timelineSchema, err := boundedInputSchema[TimelineInput](inputDomains{
		numeric: map[string]numericDomain{
			"budget": {minimum: 1, maximum: float64(services.MaxDigestBudget), hasMaximum: true},
		},
		strings: map[string]int{
			"since": maxInputTextLength, "until": maxInputTextLength, "repo": maxInputTextLength,
			"person": maxInputTextLength, "uri": maxInputTextLength, "around": maxInputTextLength,
		},
		arrays: map[string]arrayDomain{
			"source": {maxItems: maxRepeatedInputs, itemMaxLength: core.MaxSourceNameLength},
		},
	})
	if err != nil {
		return err
	}
	listSchema, err := boundedInputSchema[ListInput](inputDomains{
		numeric: map[string]numericDomain{"limit": {minimum: 0}},
		strings: map[string]int{
			"layer": maxInputTextLength, "since": maxInputTextLength, "until": maxInputTextLength,
			"source": core.MaxSourceNameLength, "status": maxInputTextLength,
			"type": maxInputTextLength, "cursor": maxInputTextLength,
		},
		arrays: map[string]arrayDomain{
			"tag": {maxItems: maxRepeatedInputs, itemMaxLength: maxInputTextLength},
		},
	})
	if err != nil {
		return err
	}
	readSchema, err := boundedInputSchema[ReadInput](inputDomains{strings: map[string]int{
		"uri": maxInputTextLength, "cursor": maxInputTextLength,
	}})
	if err != nil {
		return err
	}
	graphSchema, err := boundedInputSchema[GraphInput](inputDomains{
		numeric: map[string]numericDomain{
			"depth": {minimum: 1, maximum: services.MaxGraphDepth, hasMaximum: true},
			"limit": {minimum: 0},
		},
		strings: map[string]int{
			"uri": maxInputTextLength, "direction": maxInputTextLength,
			"kind": maxInputTextLength, "cursor": maxInputTextLength,
		},
	})
	if err != nil {
		return err
	}
	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(PageSize),
		Name:  "find",
		Title: "Find every match in the base",
		Description: "Where does this appear? Search the wiki concepts, the project pages, the " +
			"task traces, and the collected records under events/ and index/, window first for " +
			"the dated ones. Filters read each document's own field map. Every result carries the " +
			"uri you can pass to read or graph. Use context instead when you want the best few " +
			"under a token budget rather than every match. Default: search every enabled layer " +
			"and return at most 100 matches. Example: {\"grep\":[\"FK-412\"],\"since\":\"7d\"}.",
		Annotations: readOnlyAnnotations(), InputSchema: findSchema,
	}, wrap(base, "find", bindBase(base, findTool)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(0),
		Name:  "context",
		Title: "Build a token-budgeted evidence pack",
		Description: "Rank the windowed records and task traces, plus the projects and the wiki, against a query and " +
			"return a pack with a receipt: selected-item reasons, bounded drop detail with the full count, " +
			"rejected pins, and the digests that make the result reproducible. Default: use a " +
			"4096-token budget over the latest 30 populated days. Example: {\"query\":\"declarative source runner\",\"budget\":900}.",
		Annotations: readOnlyAnnotations(), InputSchema: contextSchema,
	}, wrap(base, "context", bindBase(base, contextTool)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(0),
		Name:  "day",
		Title: "Summarize one stored day",
		Description: "Render one event day chronologically in per-source groups. Identical titles " +
			"collapse with a count, noisy sources are summarized unless all is true, and the receipt " +
			"accounts for every record and the exact JSON and text byte budgets. Default: today, " +
			"a 600-token budget, and noisy sources collapsed. Example: {\"date\":\"yesterday\",\"budget\":900}.",
		Annotations: readOnlyAnnotations(), InputSchema: daySchema,
	}, wrap(base, "day", bindBase(base, dayTool)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(0),
		Name:  "timeline",
		Title: "Summarize a stored range or records around one event",
		Description: "Render an event range with exact source, repository, and person filters, or " +
			"provide uri plus around to see nearby records across sources. Stored reads only; no " +
			"provider command or network request is executed. Default: a 600-token budget and a " +
			"2h around window when uri is set. Example: {\"since\":\"7d\",\"repo\":\"repo:github.com/fmind/fkf\"}.",
		Annotations: readOnlyAnnotations(), InputSchema: timelineSchema,
	}, wrap(base, "timeline", bindBase(base, timelineTool)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(PageSize),
		Name:  "list",
		Title: "List one layer",
		Description: "Enumerate an enabled layer: events days, index documents, task traces, projects, or wiki pages. " +
			"Default: return at most 100 items with no optional filters. Example: {\"layer\":\"wiki\",\"tag\":[\"security\"]}.",
		Annotations: readOnlyAnnotations(), InputSchema: listSchema,
	}, wrap(base, "list", bindBase(base, listLayer)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(PageSize),
		Name:  "read",
		Title: "Resolve one URI",
		Description: "Read exactly one thing: a document, a record by its declared id, a heading in " +
			"a page, a jq selection over a document, an entity URI, or an external HTTPS graph node. " +
			"Never fetches over the network and never runs a source command. Default: resolve the " +
			"complete URI from its first page. Example: {\"uri\":\"wiki/a-decision.md\"}.",
		Annotations: readOnlyAnnotations(), InputSchema: readSchema,
	}, wrap(base, "read", bindBase(base, readTool)))

	mcp.AddTool(server, &mcp.Tool{
		Meta:  resultSizeMeta(PageSize),
		Name:  "graph",
		Title: "Walk the derived edge list",
		Description: "Follow the edges around a URI. An inbound walk returns every record or page " +
			"that points at the entity. The edge list transcribes declared relation fields and " +
			"authored links; it is never inferred from record bodies. Default: both directions, " +
			"one hop, and at most 100 edges. Example: {\"uri\":\"ticket:FK-412\",\"direction\":\"in\"}.",
		Annotations: readOnlyAnnotations(), InputSchema: graphSchema,
	}, wrap(base, "graph", bindBase(base, graphTool)))
	return nil
}

func resultSizeMeta(maxItems int) mcp.Meta {
	hint := map[string]any{"maxBytes": maxResponseBytes}
	if maxItems > 0 {
		hint["maxItems"] = maxItems
	}
	return mcp.Meta{ResultSizeMetaKey: hint}
}
