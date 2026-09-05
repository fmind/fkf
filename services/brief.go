package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

const (
	// DefaultBriefBudget leaves room for attention, recent evidence, and authored context.
	DefaultBriefBudget = 1200
	MaxBriefBudget     = int(core.MaxNarrativeBytes / 4)
	briefVersion       = 2
)

// BriefBudgetError reports the smallest complete receipt for a daily brief.
type BriefBudgetError struct {
	Requested int `json:"requested"`
	Minimum   int `json:"minimum"`
}

func (e *BriefBudgetError) Error() string {
	return fmt.Sprintf("brief budget %d is too small; minimum for this receipt is %d", e.Requested, e.Minimum)
}

func (e *BriefBudgetError) Unwrap() error { return core.ErrConfig }

// BriefRequest selects the complete daily brief. All temporal boundaries resolve from one base
// clock read so a long scan cannot put "yesterday" and "today" on different sides of midnight.
type BriefRequest struct {
	Budget int
}

// BriefItem is one actionable or citable line.
type BriefItem struct {
	URI    string `json:"uri,omitempty"`
	Time   string `json:"time,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// BriefSection keeps a stable daily vocabulary while Total discloses budget trimming.
type BriefSection struct {
	Name  string      `json:"name"`
	Title string      `json:"title"`
	Total int         `json:"total"`
	Items []BriefItem `json:"items"`
}

// BriefReceipt accounts for the complete offline JSON and text envelopes.
type BriefReceipt struct {
	Base         string   `json:"base"`
	Budget       int      `json:"budget"`
	UsedTokens   int      `json:"used_tokens"`
	JSONTokens   int      `json:"json_tokens"`
	TextTokens   int      `json:"text_tokens"`
	Candidates   int      `json:"candidates"`
	Selected     int      `json:"selected"`
	Dropped      int      `json:"dropped,omitempty"`
	AsOf         string   `json:"as_of"`
	InputDigest  string   `json:"input_digest"`
	Owner        string   `json:"owner,omitempty"`
	StaleSources []string `json:"stale_sources,omitempty"`
	Unharvested  int      `json:"unharvested,omitempty"`
	BriefVersion int      `json:"brief_version"`
	ToolVersion  string   `json:"tool_version"`
}

// BriefReport is the daily control surface consumed by humans and the daily-brief skill.
type BriefReport struct {
	Sections []BriefSection `json:"sections"`
	Receipt  BriefReceipt   `json:"receipt"`
}

// Brief composes stored evidence, source health, and authored context. It never runs a source,
// auth probe, body command, or collection; live provider readiness belongs to status --live.
func Brief(ctx context.Context, base *Base, request BriefRequest) (*BriefReport, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	budget := request.Budget
	if budget == 0 {
		budget = DefaultBriefBudget
	}
	if budget < 1 || budget > MaxBriefBudget {
		return nil, fmt.Errorf("%w: --budget must be between 1 and %d", core.ErrConfig, MaxBriefBudget)
	}
	now := base.Now()
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	owner := briefOwner(resolver)

	status, err := Report(ctx, base, StatusRequest{
		SkipGitAudit: true, evaluationTime: now,
	})
	if err != nil {
		return nil, fmt.Errorf("inspect daily source health: %w", err)
	}

	sections := make([]BriefSection, 0, 5)
	sections = append(sections, briefAttention(status))
	today, err := briefRecentEvidence(ctx, base, now, 0, "today", "Today")
	if err != nil {
		return nil, err
	}
	sections = append(sections, today)
	due, err := briefAuthoredTasksDue(ctx, base, now)
	if err != nil {
		return nil, err
	}
	sections = append(sections, due)
	yesterday, err := briefRecentEvidence(ctx, base, now, -1, "yesterday", "Yesterday")
	if err != nil {
		return nil, err
	}
	sections = append(sections, yesterday)
	projects, err := briefActiveProjects(ctx, base, now)
	if err != nil {
		return nil, err
	}
	sections = append(sections, projects)

	staleSources := briefStaleSources(status)
	report := &BriefReport{
		Sections: sections,
		Receipt: BriefReceipt{
			Base: base.Config.Name, Budget: budget, AsOf: now.Format(time.DateOnly), Owner: owner,
			StaleSources: staleSources, Unharvested: status.Unharvested,
			BriefVersion: briefVersion, ToolVersion: core.Version,
		},
	}
	report.Receipt.Candidates = briefItemCount(report.Sections)
	report.Receipt.InputDigest = briefInputDigest(report)
	minimum := minimumBriefBudget(report)
	if budget < minimum {
		return nil, &BriefBudgetError{Requested: budget, Minimum: minimum}
	}
	for {
		accountBrief(report)
		if briefFits(report) {
			return report, nil
		}
		if !trimBrief(report) {
			return nil, &BriefBudgetError{Requested: budget, Minimum: minimum}
		}
	}
}

func briefOwner(resolver *IdentityResolver) string {
	for _, identity := range resolver.Identities() {
		if identity.Owner {
			return identity.Canonical
		}
	}
	return ""
}

func briefAttention(status *Status) BriefSection {
	section := BriefSection{Name: "attention", Title: "Attention", Items: []BriefItem{}}
	for _, source := range status.Sources {
		if !source.Enabled || !source.Stale {
			continue
		}
		detail := "missing or beyond its configured freshness limit"
		if source.LastCollectedAt != "" {
			detail = fmt.Sprintf("%dh since last collection", source.LagHours)
		}
		section.Items = append(section.Items, BriefItem{
			URI: core.ConfigFileName, Title: "Refresh stale source " + source.Name, Detail: detail,
		})
	}
	if status.Unharvested > 0 {
		section.Items = append(section.Items, BriefItem{
			URI: string(core.LayerTasks) + "/", Title: "Review unharvested learnings",
			Detail: "fkf list tasks learned --unharvested", Count: status.Unharvested,
		})
	}
	if !status.Trust.Trusted {
		section.Items = append(section.Items, BriefItem{
			URI: core.ConfigFileName, Title: "Review and trust this base", Detail: "fkf trust",
		})
	}
	section.Total = len(section.Items)
	return section
}

func briefRecentEvidence(
	ctx context.Context, base *Base, now time.Time, offset int, name, title string,
) (BriefSection, error) {
	section := BriefSection{Name: name, Title: title, Items: []BriefItem{}}
	if !base.Store.Enabled(core.LayerEvents) {
		return section, nil
	}
	date := now.AddDate(0, 0, offset).Format(time.DateOnly)
	report, err := timelineAt(ctx, base, TimelineRequest{
		Window: Window{Since: date, Until: date, DerivedFrom: name},
		Budget: MaxDigestBudget, DeliveryFormat: DigestDeliveryJSON,
	}, now)
	if err != nil {
		return section, fmt.Errorf("build %s evidence digest: %w", name, err)
	}
	for _, group := range report.Groups {
		if group.Summarized {
			section.Items = append(section.Items, BriefItem{
				Title: group.Source, Detail: "records summarized", Count: group.Count,
			})
			continue
		}
		for _, item := range group.Items {
			section.Items = append(section.Items, BriefItem{
				URI: item.URI, Time: item.Time, Title: item.Title, Detail: group.Source, Count: item.Count,
			})
		}
	}
	section.Total = len(section.Items)
	return section, nil
}

func briefAuthoredTasksDue(ctx context.Context, base *Base, now time.Time) (BriefSection, error) {
	section := BriefSection{Name: "tasks_due", Title: "Authored tasks due", Items: []BriefItem{}}
	if !base.Store.Enabled(core.LayerTasks) {
		return section, nil
	}
	today := now.Format(time.DateOnly)
	listing, err := ListTasks(ctx, base, Window{}, 0)
	if err != nil {
		return section, fmt.Errorf("read authored tasks due: %w", err)
	}
	for _, trace := range listing.Traces {
		page := trace.page
		due, valid := briefDate(frontmatterString(page.Frontmatter, "due"))
		if !valid || due > today || briefClosedStatus(page.Status) {
			continue
		}
		section.Items = append(section.Items, BriefItem{
			URI: page.URI, Title: briefPageTitle(page), Detail: "due " + due,
		})
	}
	sortBriefItems(section.Items)
	section.Total = len(section.Items)
	return section, nil
}

func briefActiveProjects(ctx context.Context, base *Base, now time.Time) (BriefSection, error) {
	section := BriefSection{Name: "active_projects", Title: "Active projects touched this week", Items: []BriefItem{}}
	if !base.Store.Enabled(core.LayerProjects) {
		return section, nil
	}
	pages, _, err := loadMarkdownLayer(ctx, base, core.LayerProjects)
	if err != nil {
		return section, fmt.Errorf("read active projects: %w", err)
	}
	since, today := startOfWeek(now).Format(time.DateOnly), now.Format(time.DateOnly)
	for _, page := range pages {
		if page.Status != "" && !strings.EqualFold(page.Status, "active") {
			continue
		}
		updated, err := time.Parse(time.RFC3339, page.Updated)
		if err != nil {
			continue
		}
		// Page.Updated is the same filesystem fact used by stale-project lint. Reusing it keeps
		// ordinary `fkf new project` pages visible without inventing a second authored timestamp.
		touched := updated.In(now.Location()).Format(time.DateOnly)
		if touched < since || touched > today {
			continue
		}
		section.Items = append(section.Items, BriefItem{
			URI: page.URI, Title: briefPageTitle(page), Detail: "touched " + touched,
		})
	}
	sort.Slice(section.Items, func(i, j int) bool {
		if section.Items[i].Detail != section.Items[j].Detail {
			return section.Items[i].Detail > section.Items[j].Detail
		}
		return section.Items[i].URI < section.Items[j].URI
	})
	section.Total = len(section.Items)
	return section, nil
}

func briefPageTitle(page Page) string {
	if title := digestOneLine(page.Title); title != "" {
		return title
	}
	return page.URI
}

func briefClosedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "closed", "cancelled", "canceled", "complete", "completed", "archived":
		return true
	default:
		return false
	}
}

func briefDate(value string) (string, bool) {
	if len(value) >= len(time.DateOnly) {
		value = value[:len(time.DateOnly)]
	}
	parsed, err := time.Parse(time.DateOnly, value)
	return value, err == nil && parsed.Format(time.DateOnly) == value
}

func sortBriefItems(items []BriefItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Time != items[j].Time {
			return items[i].Time > items[j].Time
		}
		return items[i].URI < items[j].URI
	})
}

func briefStaleSources(status *Status) []string {
	stale := make([]string, 0)
	for _, source := range status.Sources {
		if source.Enabled && source.Stale {
			stale = append(stale, source.Name)
		}
	}
	sort.Strings(stale)
	return stale
}

func briefItemCount(sections []BriefSection) int {
	total := 0
	for _, section := range sections {
		total += len(section.Items)
	}
	return total
}

func briefInputDigest(report *BriefReport) string {
	input := struct {
		Version      int            `json:"version"`
		Base         string         `json:"base"`
		AsOf         string         `json:"as_of"`
		Owner        string         `json:"owner,omitempty"`
		StaleSources []string       `json:"stale_sources,omitempty"`
		Unharvested  int            `json:"unharvested,omitempty"`
		Sections     []BriefSection `json:"sections"`
	}{
		Version: briefVersion, Base: report.Receipt.Base, AsOf: report.Receipt.AsOf, Owner: report.Receipt.Owner,
		StaleSources: report.Receipt.StaleSources, Unharvested: report.Receipt.Unharvested,
		Sections: report.Sections,
	}
	encoded, _ := json.Marshal(input)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func accountBrief(report *BriefReport) {
	report.Receipt.Selected = briefItemCount(report.Sections)
	report.Receipt.Dropped = report.Receipt.Candidates - report.Receipt.Selected
	for range 12 {
		jsonTokens := bytesToTokens(len(marshalBriefJSON(report)))
		textTokens := bytesToTokens(len(RenderBriefText(report)))
		used := max(jsonTokens, textTokens)
		if report.Receipt.JSONTokens == jsonTokens && report.Receipt.TextTokens == textTokens &&
			report.Receipt.UsedTokens == used {
			return
		}
		report.Receipt.JSONTokens = jsonTokens
		report.Receipt.TextTokens = textTokens
		report.Receipt.UsedTokens = used
	}
}

func briefFits(report *BriefReport) bool {
	limit := report.Receipt.Budget * 4
	return len(marshalBriefJSON(report)) <= limit && len(RenderBriefText(report)) <= limit
}

func minimumBriefBudget(report *BriefReport) int {
	minimum := *report
	minimum.Sections = make([]BriefSection, len(report.Sections))
	for index, section := range report.Sections {
		minimum.Sections[index] = section
		minimum.Sections[index].Items = []BriefItem{}
	}
	minimum.Receipt.Budget = 1
	minimum.Receipt.Selected = 0
	minimum.Receipt.Dropped = minimum.Receipt.Candidates
	for range 12 {
		accountBrief(&minimum)
		if minimum.Receipt.UsedTokens <= minimum.Receipt.Budget {
			return minimum.Receipt.Budget
		}
		minimum.Receipt.Budget = minimum.Receipt.UsedTokens
	}
	return minimum.Receipt.UsedTokens
}

func trimBrief(report *BriefReport) bool {
	for index := len(report.Sections) - 1; index >= 0; index-- {
		items := report.Sections[index].Items
		if len(items) > 1 {
			report.Sections[index].Items = items[:len(items)-1]
			return true
		}
	}
	for index := len(report.Sections) - 1; index >= 0; index-- {
		if len(report.Sections[index].Items) == 1 {
			report.Sections[index].Items = []BriefItem{}
			return true
		}
	}
	return false
}

func marshalBriefJSON(report *BriefReport) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		panic(fmt.Errorf("encode brief report: %w", err))
	}
	return buffer.Bytes()
}

// RenderBriefText is the exact compact representation included in budget accounting.
func RenderBriefText(report *BriefReport) string {
	var output strings.Builder
	fmt.Fprintf(&output, "brief %s\n", report.Receipt.AsOf)
	for _, section := range report.Sections {
		fmt.Fprintf(&output, "[%s] %d/%d\n", section.Title, len(section.Items), section.Total)
		for _, item := range section.Items {
			prefix := "-"
			if item.Time != "" {
				prefix = item.Time
			}
			count := ""
			if item.Count > 1 {
				count = fmt.Sprintf(" x%d", item.Count)
			}
			detail := ""
			if item.Detail != "" {
				detail = " · " + item.Detail
			}
			uri := ""
			if item.URI != "" {
				uri = " · " + qualifiedCitation(report.Receipt.Base, item.URI)
			}
			fmt.Fprintf(&output, "%s %s%s%s%s\n", prefix, item.Title, count, detail, uri)
		}
	}
	receipt := report.Receipt
	fmt.Fprintf(&output, "receipt: selected %d/%d · dropped %d · budget %d · used %d · base %s\n",
		receipt.Selected, receipt.Candidates, receipt.Dropped, receipt.Budget, receipt.UsedTokens, receipt.Base)
	fmt.Fprintf(&output, "receipt: json %d · text %d · owner %s · offline=true\n",
		receipt.JSONTokens, receipt.TextTokens, briefDash(receipt.Owner))
	fmt.Fprintf(&output, "receipt: input_sha256 %s · brief v%d · fkf %s\n",
		receipt.InputDigest, receipt.BriefVersion, receipt.ToolVersion)
	return output.String()
}

func briefDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
