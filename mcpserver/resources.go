package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func privateResourceResponses(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		if resource, ok := result.(*mcp.ReadResourceResult); ok && resource != nil {
			// The SDK applies its public default after a resource handler returns. Enforce the
			// private-base boundary on the completed protocol result instead of trusting the
			// handler field to survive that defaulting step.
			resource.CacheScope = "private"
		}
		return result, err
	}
}

func addResources(server *mcp.Server, base *services.Base) {
	authority := "fkf://" + base.Config.Name
	resources := []struct {
		uri, name, description string
		layer                  core.Layer
		read                   func(context.Context) (any, error)
	}{
		{
			authority + "/wiki/index", "wiki index", "The wiki's own index page: the entry point to durable knowledge.",
			core.LayerWiki, func(ctx context.Context) (any, error) {
				return services.Read(ctx, base, "wiki/index.md", services.ReadOptions{})
			},
		},
		{
			authority + "/wiki/tags", "wiki tags", "The wiki's complete tag vocabulary with its usage. A flat layer is navigated by tags.",
			core.LayerWiki, func(ctx context.Context) (any, error) {
				return services.BuildTagVocabulary(ctx, base, core.LayerWiki)
			},
		},
		{
			authority + "/projects", "projects", "Up to 100 project pages with status and tags; total reports the full count.",
			core.LayerProjects, func(ctx context.Context) (any, error) {
				return services.ListPages(ctx, base, core.LayerProjects, services.PageFilter{Limit: PageSize})
			},
		},
		{
			authority + "/status", "status", "Which sources this base declares, what it last collected, and whether anything went quiet.",
			"", func(ctx context.Context) (any, error) {
				status, err := services.Report(ctx, base, services.StatusRequest{SkipGitAudit: true})
				if err != nil {
					return nil, safeClientError(base, err)
				}
				return projectStatusForMCP(base, status), nil
			},
		},
	}
	for _, resource := range resources {
		if resource.layer != "" && !base.Store.Enabled(resource.layer) {
			continue
		}
		read := resource.read
		uri := resource.uri
		server.AddResource(
			&mcp.Resource{URI: uri, Name: resource.name, Description: resource.description, MIMEType: "application/json"},
			func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				value, err := read(ctx)
				if err != nil {
					return nil, safeClientError(base, err)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, safeClientError(base, err)
				}
				result := &mcp.ReadResourceResult{
					Meta:      mcp.Meta{mcp.MetaKeyServerInfo: serverImplementation(base)},
					Cacheable: mcp.Cacheable{CacheScope: "private"},
					Contents: []*mcp.ResourceContents{
						{URI: uri, MIMEType: "application/json", Text: string(encoded)},
					},
				}
				size, err := encodedResourceResultSize(result)
				if err != nil {
					return nil, safeClientError(base, err)
				}
				if size > maxResponseBytes {
					return nil, fmt.Errorf("%w: MCP resource %s returned %d bytes, over the %d-byte limit for one read; use a filtered tool or the CLI to narrow the result",
						core.ErrFileTooLarge, uri, size, maxResponseBytes)
				}
				slog.Info("fkf mcp resource", "uri", uri, "bytes", size)
				return result, nil
			},
		)
	}
}

func encodedResourceResultSize(result *mcp.ReadResourceResult) (int, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	return len(encoded) + len(completeResultField), nil
}

type mcpTrustStatus struct {
	Trusted bool               `json:"trusted"`
	Changes []core.TrustChange `json:"changes,omitempty"`
}

type mcpStatusFinding struct {
	Check    string            `json:"check"`
	Severity services.Severity `json:"severity"`
	Message  string            `json:"message"`
	Paths    []string          `json:"paths,omitempty"`
}

type mcpStatus struct {
	Name           string                   `json:"name"`
	Trust          mcpTrustStatus           `json:"trust"`
	Versioned      bool                     `json:"versioned"`
	TrackCollected bool                     `json:"track_collected"`
	Layers         []services.LayerOverview `json:"layers"`
	Sources        []services.SourceStatus  `json:"sources"`
	Findings       []mcpStatusFinding       `json:"findings"`
	Graph          *services.GraphSummary   `json:"graph,omitempty"`
	Unharvested    int                      `json:"unharvested,omitempty"`
	Enabled        int                      `json:"enabled"`
	Missing        int                      `json:"missing_requirements"`
	Quiet          int                      `json:"quiet"`
	Errors         int                      `json:"errors"`
	Warnings       int                      `json:"warnings"`
	OK             bool                     `json:"ok"`
	Stale          bool                     `json:"stale"`
	LastSync       string                   `json:"last_sync,omitempty"`
	StaleDays      int                      `json:"stale_days,omitempty"`
	MaxAge         int                      `json:"max_age_hours,omitempty"`
}

func projectStatusForMCP(base *services.Base, status *services.Status) mcpStatus {
	sourcesView := make([]services.SourceStatus, len(status.Sources))
	copy(sourcesView, status.Sources)
	for index := range sourcesView {
		sourcesView[index].Install = ""
	}
	findings := make([]mcpStatusFinding, 0, len(status.Findings))
	for _, finding := range status.Findings {
		paths := make([]string, len(finding.Paths))
		for index, item := range finding.Paths {
			paths[index] = safeClientPath(base, item)
		}
		findings = append(findings, mcpStatusFinding{
			Check: finding.Check, Severity: finding.Severity, Message: safeClientText(base, finding.Message),
			Paths: paths,
		})
	}
	return mcpStatus{
		Name: status.Name, Trust: mcpTrustStatus{Trusted: status.Trust.Trusted, Changes: status.Trust.Changes},
		Versioned: status.Versioned, TrackCollected: status.TrackCollected,
		Layers: status.Layers, Sources: sourcesView, Findings: findings, Graph: status.Graph,
		Unharvested: status.Unharvested, Enabled: status.Enabled, Missing: status.Missing,
		Quiet: status.Quiet, Errors: status.Errors, Warnings: status.Warnings, OK: status.OK,
		Stale: status.Stale, LastSync: status.LastSync, StaleDays: status.StaleDays, MaxAge: status.MaxAge,
	}
}
