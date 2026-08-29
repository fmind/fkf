package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// SourceTestOutcome is the closed result vocabulary for one declared source verification hook.
type SourceTestOutcome string

const (
	SourceTestPassed SourceTestOutcome = "passed"
	SourceTestFailed SourceTestOutcome = "failed"
)

// SourceTestRequest selects enabled hooks by default, every declared hook with All, or the exact
// named sources regardless of their enabled state.
type SourceTestRequest struct {
	Targets []string
	All     bool
}

// SourceTestResult is one source hook execution. Captured stdout and provider stderr are never
// exposed; the reviewed argv and safe process status are enough to reproduce a failure.
type SourceTestResult struct {
	Source  string            `json:"source"`
	Enabled bool              `json:"enabled"`
	Command string            `json:"command"`
	Outcome SourceTestOutcome `json:"outcome"`
	Elapsed string            `json:"elapsed"`
	Error   string            `json:"error,omitempty"`
}

// SourceTestReport summarizes one bounded verification run.
type SourceTestReport struct {
	Base     string             `json:"base"`
	Sources  []SourceTestResult `json:"sources"`
	Passed   int                `json:"passed"`
	Failed   int                `json:"failed"`
	Complete bool               `json:"complete"`
	Elapsed  string             `json:"elapsed"`
}

// FailureSummary renders only reviewed source names, argv, and safe process diagnostics.
func (report *SourceTestReport) FailureSummary() string {
	lines := make([]string, 0, report.Failed)
	for _, result := range report.Sources {
		if result.Outcome != SourceTestFailed {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s (command: %s)", result.Source, result.Error, result.Command))
	}
	return strings.Join(lines, "\n")
}

// TestSources runs the selected trusted hooks sequentially in stable order. Hooks are optional;
// a default or --all run with none is a successful empty verification, while naming a source
// without a hook is an actionable configuration error.
func TestSources(ctx context.Context, base *Base, request SourceTestRequest) (*SourceTestReport, error) {
	started := base.Now()
	report := &SourceTestReport{Base: base.Root(), Sources: []SourceTestResult{}, Complete: true}
	targets, err := resolveSourceTestTargets(base, request)
	if err != nil {
		return report, err
	}
	if len(targets) == 0 {
		report.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
		return report, nil
	}
	if err := base.RequireTrust(ctx); err != nil {
		return report, err
	}
	for _, source := range targets {
		if err := checkContext(ctx); err != nil {
			return report, err
		}
		resultStarted := base.Now()
		command := sources.BuildTestCommand(source, base.Env, base.Config.Sync.Timeout)
		result := SourceTestResult{
			Source: source.Name, Enabled: source.Enabled, Command: command.Display(), Outcome: SourceTestPassed,
		}
		_, runErr := base.Runner.Run(ctx, command)
		result.Elapsed = base.Now().Sub(resultStarted).Round(time.Millisecond).String()
		if runErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(runErr, ctxErr) {
				return report, ctxErr
			}
			result.Outcome, result.Error = SourceTestFailed, runErr.Error()
			report.Failed++
			report.Complete = false
		} else {
			report.Passed++
		}
		report.Sources = append(report.Sources, result)
	}
	report.Elapsed = base.Now().Sub(started).Round(time.Millisecond).String()
	return report, nil
}

func resolveSourceTestTargets(base *Base, request SourceTestRequest) ([]*core.Source, error) {
	if request.All && len(request.Targets) > 0 {
		return nil, fmt.Errorf("%w: --all cannot be combined with source names", core.ErrConfig)
	}
	if len(request.Targets) == 0 {
		targets := make([]*core.Source, 0, len(base.Config.Sources))
		for _, name := range base.Config.SourceNames() {
			source := base.Config.Sources[name]
			if len(source.Test) > 0 && (request.All || source.Enabled) {
				targets = append(targets, source)
			}
		}
		return targets, nil
	}
	targets := make([]*core.Source, 0, len(request.Targets))
	seen := make(map[string]struct{}, len(request.Targets))
	for _, name := range request.Targets {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate source %q; name each test target once", core.ErrConfig, name)
		}
		seen[name] = struct{}{}
		source, err := base.Source(name)
		if err != nil {
			return nil, err
		}
		if len(source.Test) == 0 {
			return nil, fmt.Errorf("%w: source %s declares no test hook in %s", core.ErrConfig, name, base.Config.Path)
		}
		targets = append(targets, source)
	}
	return targets, nil
}
