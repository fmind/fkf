package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/fmind/fkf/core"
)

// BuildReport is what `fkf build` returns.
type BuildReport struct {
	Graph *GraphBuild      `json:"graph,omitempty"`
	Wiki  *WikiIndexReport `json:"wiki,omitempty"`
}

// Build runs derived file generation for graph, wiki index, or both.
func Build(ctx context.Context, base *Base, target string, check bool) (*BuildReport, error) {
	if check && target != "wiki" {
		return nil, errors.New("--check is supported only for the wiki target; graph checks are not implemented")
	}
	report := &BuildReport{}
	switch target {
	case "graph":
		graph, err := BuildGraph(ctx, base)
		if err != nil {
			return nil, err
		}
		report.Graph = graph
	case "wiki":
		if !base.Store.Enabled(core.LayerWiki) {
			return nil, fmt.Errorf("wiki layer is disabled in %s", core.ConfigFileName)
		}
		wiki, err := BuildWikiIndex(ctx, base, !check)
		if err != nil {
			return nil, err
		}
		report.Wiki = wiki
	case "", "all":
		if base.Store.Enabled(core.LayerWiki) {
			wiki, err := BuildWikiIndex(ctx, base, !check)
			if err != nil {
				return nil, err
			}
			report.Wiki = wiki
		}
		// The graph digest covers exact authored Markdown, including wiki/index.md. Generate
		// that page first so a successful all-build cannot invalidate its own graph cache.
		graph, err := BuildGraph(ctx, base)
		if err != nil {
			return nil, err
		}
		report.Graph = graph
	default:
		return nil, fmt.Errorf("unknown build target %q; expected graph, wiki, or all", target)
	}
	return report, nil
}
