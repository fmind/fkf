package services

import (
	"context"
	"fmt"
	"time"

	"github.com/fmind/fkf/core"
)

// `fkf status` is the unified inspection command for a base: it summarizes the base layout,
// audits repository health and document integrity, and reports per-source readiness and freshness.

const (
	quietWindow       = 14
	quietArmingDays   = 7
	quietRatioPercent = 20
	gitTimeout        = 15 * time.Second
)

// Finding is one health or integrity issue worth acting on.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Paths    []string `json:"paths,omitempty"`
	Fix      string   `json:"fix,omitempty"`
}

// LayerOverview is one layer's line in the overview.
type LayerOverview struct {
	Layer   core.Layer `json:"layer"`
	Enabled bool       `json:"enabled"`
	URI     string     `json:"uri"`
	Count   int        `json:"count"`
	Unit    string     `json:"unit"`
	Since   string     `json:"since,omitempty"`
	Until   string     `json:"until,omitempty"`
	Note    string     `json:"note,omitempty"`
}

// RequirementStatus is one executable a source explicitly asks status to check.
type RequirementStatus struct {
	Name   string `json:"name"`
	OnPath bool   `json:"on_path"`
}

// SourceStatus is one source's readiness and recent volume.
type SourceStatus struct {
	Name            string              `json:"name"`
	Enabled         bool                `json:"enabled"`
	Kind            core.Layer          `json:"kind"`
	Requires        []RequirementStatus `json:"requires,omitempty"`
	Install         string              `json:"install,omitempty"`
	Test            *RequirementStatus  `json:"test,omitempty"`
	Body            bool                `json:"body"`
	Auth            bool                `json:"auth"`
	AuthRequired    bool                `json:"auth_required,omitempty"`
	Undeclared      bool                `json:"undeclared,omitempty"`
	LastDate        string              `json:"last_date,omitempty"`
	LastCollectedAt string              `json:"last_collected_at,omitempty"`
	LagHours        int                 `json:"lag_hours,omitempty"`
	Stale           bool                `json:"stale,omitempty"`
	LastCount       int                 `json:"last_count,omitempty"`
	Median          int                 `json:"median,omitempty"`
	Days            int                 `json:"days,omitempty"`
	Quiet           bool                `json:"quiet,omitempty"`
	QuietReason     string              `json:"quiet_reason,omitempty"`
	// lastCollected retains the exact evidence boundary used for hour-level freshness.
	// LastDate remains the stable public summary.
	lastCollected time.Time
}

// Status is what `fkf status` returns.
type Status struct {
	Base           string                `json:"base"`
	Name           string                `json:"name"`
	Origin         core.BaseOrigin       `json:"base_origin"`
	Trust          core.TrustState       `json:"trust"`
	Versioned      bool                  `json:"versioned"`
	TrackCollected bool                  `json:"track_collected"`
	Layers         []LayerOverview       `json:"layers"`
	Sources        []SourceStatus        `json:"sources"`
	Harnesses      []HarnessRegistration `json:"harnesses,omitempty"`
	AuthRequired   []string              `json:"auth_required,omitempty"`
	Findings       []Finding             `json:"findings"`
	Graph          *GraphSummary         `json:"graph,omitempty"`
	Unharvested    int                   `json:"unharvested,omitempty"`
	Enabled        int                   `json:"enabled"`
	Missing        int                   `json:"missing_requirements"`
	MissingTests   int                   `json:"missing_test_hooks"`
	Quiet          int                   `json:"quiet"`
	Errors         int                   `json:"errors"`
	Warnings       int                   `json:"warnings"`
	OK             bool                  `json:"ok"`
	Stale          bool                  `json:"stale"`
	LastSync       string                `json:"last_sync,omitempty"`
	StaleDays      int                   `json:"stale_days,omitempty"`
	MaxAge         int                   `json:"max_age_hours,omitempty"`
	Next           []string              `json:"next"`
}

func (s *Status) addFinding(check string, severity Severity, message, fix string, paths ...string) {
	s.Findings = append(s.Findings, Finding{
		Check: check, Severity: severity, Message: message, Fix: fix, Paths: paths,
	})
}

// StatusRequest tunes the status report.
type StatusRequest struct {
	MaxAgeHours    int
	Executable     string
	evaluationTime time.Time
	// Live enables the two user-scope checks that are intentionally absent from MCP and other
	// stored-read callers: trusted auth probes and harness registration inspection.
	Live bool
	// SkipGitAudit keeps MCP's status resource subprocess-free. The CLI leaves it false and
	// runs the fixed, sanitized tracked-file audit.
	SkipGitAudit bool
}

// Report compiles the complete base status.
func Report(ctx context.Context, base *Base, request StatusRequest) (*Status, error) {
	return reportWithDocumentReader(ctx, base, request, base.ReadFileContext)
}

func reportWithDocumentReader(
	ctx context.Context, base *Base, request StatusRequest, read statusDocumentReader,
) (*Status, error) {
	now := request.evaluationTime
	if now.IsZero() {
		now = base.Now()
		request.evaluationTime = now
	}
	trust, err := core.ReadTrust(ctx, base.Config)
	if err != nil {
		return nil, err
	}
	documents, err := loadStatusDocuments(ctx, base, read)
	if err != nil {
		return nil, err
	}
	trackCollected, err := TracksCollected(base.Root())
	if err != nil {
		return nil, err
	}

	status := &Status{
		Base:           base.Root(),
		Name:           base.Config.Name,
		Origin:         base.Origin,
		Trust:          trust,
		Versioned:      base.Store.Versioned(),
		TrackCollected: trackCollected,
		Layers:         make([]LayerOverview, 0, len(core.Layers)),
		Sources:        []SourceStatus{},
		Findings:       []Finding{},
		MaxAge:         request.MaxAgeHours,
		Next:           []string{},
	}

	if err := populateLayerOverviews(ctx, base, status, documents, now); err != nil {
		return nil, err
	}
	if err := populateSourceStatuses(ctx, base, status, request, documents); err != nil {
		return nil, err
	}
	if request.Live {
		if status.Trust.Trusted {
			status.AuthRequired, err = ProbeSourceAuth(ctx, base, base.Config.EnabledSources(), true)
			if err != nil {
				return nil, err
			}
			markAuthRequired(status)
		}
		status.Harnesses, err = InspectHarnesses(ctx, base.Root(), "", request.Executable)
		if err != nil {
			return nil, err
		}
	}
	if err := auditHealth(ctx, base, status, request, trackCollected, documents); err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	status.Next = suggestNext(status)
	return status, nil
}

func suggestNext(status *Status) []string {
	next := make([]string, 0, 3)
	if !status.Trust.Trusted {
		next = append(next, baseCommand(status.Base, "trust")+"  read the commands this base declares, then record them")
	}
	events := layerOverview(status, core.LayerEvents)
	switch {
	case events != nil && events.Count == 0:
		next = append(next, baseCommand(status.Base, "sync --days 7")+"  collect the last seven completed days")
	case status.StaleDays > 1:
		next = append(next, fmt.Sprintf(
			"%s  the newest day here is %d day(s) old",
			baseCommand(status.Base, fmt.Sprintf("sync --days %d", min(status.StaleDays, 30))), status.StaleDays))
	}
	if status.Graph == nil {
		next = append(next, baseCommand(status.Base, "build graph")+"  derive the edge list the graph and --expand read")
	}
	if status.Unharvested > 0 {
		next = append(next, fmt.Sprintf(
			"%s  %d bullet(s) no page has promoted yet",
			baseCommand(status.Base, "list tasks learned --unharvested"), status.Unharvested))
	}
	return append(next,
		baseCommand(status.Base, `context "<terms>"`)+"  the evidence pack you hand an agent",
		baseCommand(status.Base, "find <term>")+"  every match, in every layer",
		baseCommand(status.Base, "graph <uri>")+"  what is connected to one thing",
	)
}

func baseCommand(root, arguments string) string {
	return "fkf --base " + shellArg(root) + " " + arguments
}

func layerOverview(status *Status, layer core.Layer) *LayerOverview {
	for index := range status.Layers {
		if status.Layers[index].Layer == layer {
			return &status.Layers[index]
		}
	}
	return nil
}
