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
	"github.com/fmind/fkf/sources"
)

const (
	// DefaultDigestBudget keeps a session-start day digest small enough to be useful as context.
	DefaultDigestBudget = 600
	// MaxDigestBudget keeps an MCP request inside the same narrative response bound as every
	// other read. The CLI shares the bound so the same request has one meaning everywhere.
	MaxDigestBudget = int(core.MaxNarrativeBytes / 4)
)

const (
	// DigestDeliveryJSON is the indented JSON plus trailing newline emitted by --format json.
	DigestDeliveryJSON = "json"
	// DigestDeliveryJSONL is the compact JSON plus trailing newline emitted by --format jsonl.
	DigestDeliveryJSONL = "jsonl"
	// DigestDeliveryText is the compact chronological rendering emitted by --format text.
	DigestDeliveryText = "text"
	// DigestDeliveryCompactJSON is the compact JSON value embedded in an MCP tool result.
	DigestDeliveryCompactJSON = "json-compact"
)

// NoisyDigestSources are high-volume activity streams whose individual rows usually add less
// signal than one truthful count. --all expands them without changing the stored evidence.
var NoisyDigestSources = map[string]struct{}{
	"shell-commands":       {},
	"agent-prompts":        {},
	"github-runs":          {},
	"google-drive-changes": {},
}

// DigestBudgetError names the exact smallest receipt that can be returned for this request.
// A caller can retry with Minimum without guessing or receiving an over-budget answer.
type DigestBudgetError struct {
	Requested int `json:"requested"`
	Minimum   int `json:"minimum"`
}

func (e *DigestBudgetError) Error() string {
	return fmt.Sprintf("digest budget %d is too small; minimum for this receipt is %d", e.Requested, e.Minimum)
}

func (e *DigestBudgetError) Unwrap() error { return core.ErrConfig }

// DigestItem is one chronologically placed line. Count appears only when two or more records
// with the same title were collapsed.
type DigestItem struct {
	Time  string `json:"time,omitempty"`
	URI   string `json:"uri"`
	Title string `json:"title"`
	Count int    `json:"count,omitempty"`

	priority int
}

// DigestGroup is one source's contribution to the requested range. Count is the complete
// matching source count even when the budget admits only some item lines.
type DigestGroup struct {
	Source     string       `json:"source"`
	Count      int          `json:"count"`
	Summarized bool         `json:"summarized,omitempty"`
	Items      []DigestItem `json:"items,omitempty"`

	priority int
}

// DigestReceipt makes a day or timeline answer reproducible and accounts for every omission.
type DigestReceipt struct {
	Window              Window   `json:"window"`
	Budget              int      `json:"budget"`
	Format              string   `json:"format"`
	UsedTokens          int      `json:"used_tokens"`
	JSONTokens          int      `json:"json_tokens"`
	TextTokens          int      `json:"text_tokens"`
	Records             int      `json:"records"`
	Selected            int      `json:"selected"`
	Dropped             int      `json:"dropped,omitempty"`
	People              int      `json:"people,omitempty"`
	DroppedPeople       int      `json:"dropped_people,omitempty"`
	Repositories        int      `json:"repositories,omitempty"`
	DroppedRepositories int      `json:"dropped_repositories,omitempty"`
	InputDigest         string   `json:"input_digest"`
	AsOf                string   `json:"as_of"`
	Sources             []string `json:"sources,omitempty"`
	Repository          string   `json:"repository,omitempty"`
	Person              string   `json:"person,omitempty"`
	Around              string   `json:"around,omitempty"`
	AroundWindow        string   `json:"around_window,omitempty"`
}

// TimelineReport is the common response used by one-day and range digests.
type TimelineReport struct {
	Groups       []DigestGroup `json:"groups"`
	People       []string      `json:"people,omitempty"`
	Repositories []string      `json:"repositories,omitempty"`
	Receipt      DigestReceipt `json:"receipt"`

	peoplePriority     map[string]int
	repositoryPriority map[string]int
}

// DayRequest selects one local calendar day.
type DayRequest struct {
	Date           string
	Budget         int
	All            bool
	DeliveryFormat string
}

// TimelineRequest selects either a dated range or records around one record URI.
type TimelineRequest struct {
	Window         Window
	Sources        []string
	Repository     string
	Person         string
	AroundURI      string
	Around         time.Duration
	Budget         int
	All            bool
	DeliveryFormat string
}

// Day renders one local calendar day. An omitted date means today; relative names are resolved
// once from the base clock so the receipt and scan cannot cross midnight differently.
func Day(ctx context.Context, base *Base, request DayRequest) (*TimelineReport, error) {
	now := base.Now()
	date, err := resolveDigestDay(request.Date, now)
	if err != nil {
		return nil, err
	}
	return timelineAt(ctx, base, TimelineRequest{
		Window: Window{Since: date, Until: date, DerivedFrom: digestDayOrigin(request.Date)},
		Budget: request.Budget, All: request.All, DeliveryFormat: request.DeliveryFormat,
	}, now)
}

// Timeline renders a range chronologically, with exact relation filters and an optional window
// around one stored record. It reads stored documents only and never runs a source command.
func Timeline(ctx context.Context, base *Base, request TimelineRequest) (*TimelineReport, error) {
	return timelineAt(ctx, base, request, base.Now())
}

func timelineAt(
	ctx context.Context, base *Base, request TimelineRequest, now time.Time,
) (*TimelineReport, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := base.RequireLayer(core.LayerEvents); err != nil {
		return nil, err
	}
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	if request.Repository != "" {
		request.Repository = resolver.Canonical(request.Repository)
	}
	if request.Person != "" {
		request.Person = resolver.Canonical(request.Person)
	}
	if err := validateTimelineRequest(base, resolver, &request); err != nil {
		return nil, err
	}

	window, err := validateDigestWindow(request.Window, now)
	if err != nil {
		return nil, err
	}
	var lower, upper time.Time
	if request.AroundURI != "" {
		if request.Around == 0 {
			request.Around = 2 * time.Hour
		}
		center, err := recordTimeForURI(ctx, base, request.AroundURI)
		if err != nil {
			return nil, err
		}
		lower, upper = center.Add(-request.Around), center.Add(request.Around)
		location := now.Location()
		window = Window{
			Since: lower.In(location).Format(time.DateOnly), Until: upper.In(location).Format(time.DateOnly),
			DerivedFrom: "--around " + request.Around.String(),
		}
	}

	found, err := Find(ctx, base, FindFilter{
		Sources: request.Sources, Layers: []core.Layer{core.LayerEvents},
		Window: window, Limit: NoFindLimit,
	}, false)
	if err != nil {
		return nil, err
	}
	records := make([]FindRecord, 0, len(found.Records))
	for _, record := range found.Records {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !recordMatchesDigestRelations(record, request.Repository, request.Person) {
			continue
		}
		if !lower.IsZero() && !recordWithin(record, lower, upper) {
			continue
		}
		records = append(records, record)
	}
	return buildTimelineReport(records, request, window, now, resolver)
}

func validateTimelineRequest(base *Base, resolver *IdentityResolver, request *TimelineRequest) error {
	if request.Budget < 0 || request.Budget > MaxDigestBudget {
		return fmt.Errorf("%w: --budget must be between 1 and %d", core.ErrConfig, MaxDigestBudget)
	}
	if request.Around < 0 {
		return fmt.Errorf("%w: --around must be a positive duration", core.ErrConfig)
	}
	if request.AroundURI != "" && (request.Window.Since != "" || request.Window.Until != "") {
		return fmt.Errorf("%w: a record URI with --around cannot be combined with --since or --until", core.ErrConfig)
	}
	if request.AroundURI == "" && request.Around != 0 {
		return fmt.Errorf("%w: --around needs one record URI", core.ErrConfig)
	}
	if request.AroundURI == "" && request.Window.Since == "" {
		return fmt.Errorf("%w: timeline needs --since, or one record URI with --around", core.ErrConfig)
	}
	if request.DeliveryFormat == "" {
		request.DeliveryFormat = DigestDeliveryJSON
	}
	switch request.DeliveryFormat {
	case DigestDeliveryJSON, DigestDeliveryJSONL, DigestDeliveryText, DigestDeliveryCompactJSON:
	default:
		return fmt.Errorf("%w: digest delivery format %q is not json, jsonl, text, or json-compact",
			core.ErrConfig, request.DeliveryFormat)
	}
	for _, filter := range []struct {
		flag, value string
		kind        core.IdentityKind
	}{
		{"--repo", request.Repository, core.IdentityRepository},
		{"--person", request.Person, core.IdentityPerson},
	} {
		if err := validateDigestEntity(resolver, filter.flag, filter.value, filter.kind); err != nil {
			return err
		}
	}
	request.Sources = normalizedDigestSources(request.Sources)
	if err := requireKnown("source", request.Sources, base.Config.SourceNames()); err != nil {
		return err
	}
	return nil
}

func validateDigestEntity(
	resolver *IdentityResolver, flag, value string, expected core.IdentityKind,
) error {
	if value == "" {
		return nil
	}
	uri, err := ParseURI(value)
	if err != nil || !uri.IsEntity() {
		return fmt.Errorf("%w: %s needs an entity URI such as repo:github.com/fmind/fkf or person:email/name@example.test", core.ErrConfig, flag)
	}
	kind := digestIdentityKind(resolver, value)
	if kind != expected {
		actual := string(kind)
		if actual == "" {
			actual = "unclassified"
		}
		return fmt.Errorf("%w: %s needs a %s identity; %q is %s",
			core.ErrConfig, flag, expected, value, actual)
	}
	return nil
}

func digestIdentityKind(resolver *IdentityResolver, value string) core.IdentityKind {
	// A declaration can classify any open entity scheme; conventional URI schemes remain
	// useful without forcing every repository or person encountered in evidence into config.
	if kind := resolver.Kind(value); kind != "" {
		return kind
	}
	return (core.Identity{Canonical: value}).EffectiveKind()
}

func normalizedDigestSources(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func resolveDigestDay(value string, now time.Time) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		trimmed = "today"
	}
	window, err := ParseWindow(trimmed, trimmed, now)
	if err != nil {
		return "", fmt.Errorf("%w: day %q: %w", core.ErrConfig, value, err)
	}
	if window.Since != window.Until {
		return "", fmt.Errorf("%w: day %q does not resolve to one date", core.ErrConfig, value)
	}
	return window.Since, nil
}

func digestDayOrigin(value string) string {
	if strings.TrimSpace(value) == "" {
		return "today"
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func validateDigestWindow(window Window, now time.Time) (Window, error) {
	derived := window.DerivedFrom
	validated, err := ParseWindow(window.Since, window.Until, now)
	if err != nil {
		return Window{}, fmt.Errorf("%w: %w", core.ErrConfig, err)
	}
	validated.DerivedFrom = derived
	return validated, nil
}

func recordTimeForURI(ctx context.Context, base *Base, raw string) (time.Time, error) {
	uri, err := ParseURI(raw)
	if err != nil {
		return time.Time{}, err
	}
	if uri.Scheme != SchemeFile || uri.Fragment == "" || !strings.HasSuffix(uri.Path, ".json") {
		return time.Time{}, fmt.Errorf("%w: timeline --around needs one stored record URI", core.ErrConfig)
	}
	document, err := base.ReadDocumentContext(ctx, uri.Path)
	if err != nil {
		return time.Time{}, err
	}
	record, found := document.FindRecord(uri.Fragment)
	if !found {
		return time.Time{}, fmt.Errorf("%s holds no record with id %q", uri.Path, uri.Fragment)
	}
	value, found := document.Fields.EvalString(core.FieldTime, map[string]any(record))
	if !found {
		return time.Time{}, fmt.Errorf("%s has no event time", raw)
	}
	parsed, err := sources.ParseRecordTime(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s event time: %w", raw, err)
	}
	return parsed, nil
}

func recordWithin(record FindRecord, lower, upper time.Time) bool {
	parsed, err := sources.ParseRecordTime(record.Time)
	if err != nil {
		return false
	}
	return !parsed.Before(lower) && !parsed.After(upper)
}

func recordMatchesDigestRelations(record FindRecord, repository, person string) bool {
	if repository != "" && !recordHasExactValue(record, repository) {
		return false
	}
	return person == "" || recordHasExactValue(record, person)
}

func recordHasExactValue(record FindRecord, wanted string) bool {
	for _, value := range recordRelationValues(record) {
		if value == wanted {
			return true
		}
	}
	return false
}

func buildTimelineReport(
	records []FindRecord,
	request TimelineRequest,
	window Window,
	now time.Time,
	resolver *IdentityResolver,
) (*TimelineReport, error) {
	sort.SliceStable(records, func(i, j int) bool {
		left, leftErr := sources.ParseRecordTime(records[i].Time)
		right, rightErr := sources.ParseRecordTime(records[j].Time)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.Before(right)
		}
		if records[i].Time != records[j].Time {
			return records[i].Time < records[j].Time
		}
		return records[i].URI < records[j].URI
	})
	budget := request.Budget
	if budget == 0 {
		budget = DefaultDigestBudget
	}
	request.Budget = budget
	people, repositories, peoplePriority, repositoryPriority := digestEntities(
		records, resolver, request.Person != "" && resolver.IsOwner(request.Person),
	)
	report := &TimelineReport{
		Groups: groupDigestRecords(records, request.All), People: people, Repositories: repositories,
		peoplePriority: peoplePriority, repositoryPriority: repositoryPriority,
		Receipt: DigestReceipt{
			Window: window, Budget: budget, Format: request.DeliveryFormat,
			Records: len(records), AsOf: now.Format(time.DateOnly),
			People: len(people), Repositories: len(repositories),
			Sources: append([]string(nil), request.Sources...), Repository: request.Repository,
			Person: request.Person, Around: request.AroundURI,
		},
	}
	if request.AroundURI != "" {
		report.Receipt.AroundWindow = request.Around.String()
	}
	report.Receipt.InputDigest = digestReportInput(
		records, request, window, report.Receipt.AsOf, people, repositories,
	)
	minimum := minimumTimelineBudget(report)
	if budget < minimum {
		return nil, &DigestBudgetError{Requested: budget, Minimum: minimum}
	}
	for {
		accountTimelineReport(report)
		if timelineFits(report) {
			return report, nil
		}
		if !trimTimelineReport(report) {
			return nil, &DigestBudgetError{Requested: budget, Minimum: minimum}
		}
	}
}

func groupDigestRecords(records []FindRecord, all bool) []DigestGroup {
	type grouped struct {
		group   DigestGroup
		byTitle map[string]int
	}
	bySource := map[string]*grouped{}
	order := make([]*grouped, 0)
	commitCounts := digestCommitCounts(records)
	for _, record := range records {
		entry := bySource[record.Source]
		if entry == nil {
			entry = &grouped{group: DigestGroup{Source: record.Source}, byTitle: map[string]int{}}
			if _, noisy := NoisyDigestSources[record.Source]; noisy && !all {
				entry.group.Summarized = true
			}
			bySource[record.Source] = entry
			order = append(order, entry)
		}
		entry.group.Count++
		if entry.group.Summarized {
			continue
		}
		title, key := digestRecordGrouping(record, all, commitCounts)
		priority := digestRecordPriority(record)
		entry.group.priority = max(entry.group.priority, priority)
		if index, exists := entry.byTitle[key]; exists {
			if entry.group.Items[index].Count == 0 {
				entry.group.Items[index].Count = 2
			} else {
				entry.group.Items[index].Count++
			}
			entry.group.Items[index].priority = max(entry.group.Items[index].priority, priority)
			continue
		}
		entry.byTitle[key] = len(entry.group.Items)
		entry.group.Items = append(entry.group.Items, DigestItem{
			Time: record.Time, URI: record.URI, Title: title, priority: priority,
		})
	}
	groups := make([]DigestGroup, 0, len(order))
	for _, entry := range order {
		groups = append(groups, entry.group)
	}
	return groups
}

func digestRecordGrouping(record FindRecord, all bool, commitCounts map[string]int) (string, string) {
	title := digestRecordTitle(record)
	if title == "" {
		title = record.URI
	}
	key := title
	if !all {
		if repository, found := digestCommitRepository(record); found &&
			commitCounts[record.Source+"\x00"+repository] >= 3 {
			title = strings.TrimPrefix(repository, "repo:github.com/") + " commits"
			key = "repository:" + repository
		}
	}
	return title, key
}

func digestRecordTitle(record FindRecord) string {
	if title := digestOneLine(record.Title); title != "" {
		return title
	}
	if record.Source != "google-calendar-events" && record.Source != "google-calendar-agenda" {
		return ""
	}
	calendar, ok := record.Record["calendar"].(map[string]any)
	if !ok {
		return ""
	}
	summary, _ := calendar["summary"].(string)
	summary = digestOneLine(summary)
	if summary == "" {
		return ""
	}
	if visibility, _ := record.Record["visibility"].(string); visibility == "private" {
		return "Busy — " + digestCalendarName(summary)
	}
	return summary
}

func digestCalendarName(summary string) string {
	if strings.ContainsAny(summary, " \t") {
		return summary
	}
	_, domain, email := strings.Cut(summary, "@")
	if !email {
		return summary
	}
	label, _, _ := strings.Cut(domain, ".")
	if label == "" {
		return summary
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

func digestCommitRepository(record FindRecord) (string, bool) {
	if record.Source != "git-commits" && record.Source != "github-commits" {
		return "", false
	}
	values := record.Fields["repository"]
	if len(values) != 1 || !strings.HasPrefix(values[0], "repo:") {
		return "", false
	}
	return values[0], true
}

func digestCommitCounts(records []FindRecord) map[string]int {
	counts := map[string]int{}
	for _, record := range records {
		if repository, found := digestCommitRepository(record); found {
			counts[record.Source+"\x00"+repository]++
		}
	}
	return counts
}

const digestCorePriority = 300

func digestRecordPriority(record FindRecord) int {
	priority := 100
	switch record.Source {
	case "google-calendar-events", "google-calendar-agenda":
		priority = 400
	case "meeting-notes":
		priority = 425
	case "git-commits":
		priority = 350
	case "github-commits":
		priority = 250
	}
	if eventType, _ := record.Record["eventType"].(string); eventType == "workingLocation" {
		return 50
	}
	if len(record.Fields["participant"]) > 0 {
		priority += 50
	}
	return priority
}

func digestItemCount(item DigestItem) int {
	if item.Count > 1 {
		return item.Count
	}
	return 1
}

func selectedDigestRecords(groups []DigestGroup) int {
	selected := 0
	for _, group := range groups {
		if group.Summarized {
			selected += group.Count
			continue
		}
		for _, item := range group.Items {
			selected += digestItemCount(item)
		}
	}
	return selected
}

func accountTimelineReport(report *TimelineReport) {
	report.Receipt.Selected = selectedDigestRecords(report.Groups)
	report.Receipt.Dropped = report.Receipt.Records - report.Receipt.Selected
	report.Receipt.DroppedPeople = report.Receipt.People - len(report.People)
	report.Receipt.DroppedRepositories = report.Receipt.Repositories - len(report.Repositories)
	stabilizeTimelineUsage(report)
}

func stabilizeTimelineUsage(report *TimelineReport) {
	for range 12 {
		jsonTokens := bytesToTokens(len(marshalTimelineJSON(report)))
		textTokens := bytesToTokens(len(RenderTimelineText(report)))
		used := bytesToTokens(len(marshalTimelineDelivery(report)))
		if report.Receipt.JSONTokens == jsonTokens && report.Receipt.TextTokens == textTokens && report.Receipt.UsedTokens == used {
			return
		}
		report.Receipt.JSONTokens = jsonTokens
		report.Receipt.TextTokens = textTokens
		report.Receipt.UsedTokens = used
	}
}

func timelineFits(report *TimelineReport) bool {
	limit := report.Receipt.Budget * 4
	return len(marshalTimelineDelivery(report)) <= limit
}

func minimumTimelineBudget(report *TimelineReport) int {
	minimum := *report
	minimum.Groups = []DigestGroup{}
	minimum.People = nil
	minimum.Repositories = nil
	minimum.Receipt.Selected = 0
	minimum.Receipt.Dropped = minimum.Receipt.Records
	minimum.Receipt.DroppedPeople = minimum.Receipt.People
	minimum.Receipt.DroppedRepositories = minimum.Receipt.Repositories
	minimum.Receipt.Budget = 1
	for range 12 {
		stabilizeTimelineUsage(&minimum)
		needed := minimum.Receipt.UsedTokens
		if needed <= minimum.Receipt.Budget {
			return minimum.Receipt.Budget
		}
		minimum.Receipt.Budget = needed
	}
	return minimum.Receipt.UsedTokens
}

// trimTimelineReport preserves source diversity first: it removes low-priority evidence, then
// entity lists, then the least useful extra line before a source representative. Summaries are
// atomic because a partial count would be false.
func trimTimelineReport(report *TimelineReport) bool {
	if trimLowPriorityDigestGroup(report) {
		return true
	}
	if trimLowPriorityDigestEntity(&report.Repositories, report.repositoryPriority) {
		return true
	}
	if trimLowPriorityDigestEntity(&report.People, report.peoplePriority) {
		return true
	}
	groupIndex, itemIndex := -1, -1
	most, priority := 1, 0
	for candidate := range report.Groups {
		group := &report.Groups[candidate]
		count := len(group.Items)
		if count <= 1 {
			continue
		}
		item := lowestPriorityDigestItem(group)
		candidatePriority := group.Items[item].priority
		// Preserve one line per source, but discard a less useful extra line before
		// higher-value calendar evidence. Source verbosity and stable order break ties.
		if groupIndex < 0 || candidatePriority < priority ||
			candidatePriority == priority && (count > most || count == most && candidate > groupIndex) {
			groupIndex, itemIndex, most, priority = candidate, item, count, candidatePriority
		}
	}
	if groupIndex >= 0 {
		group := &report.Groups[groupIndex]
		group.Items = append(group.Items[:itemIndex], group.Items[itemIndex+1:]...)
		return true
	}
	if len(report.Repositories) > 1 {
		report.Repositories = report.Repositories[:len(report.Repositories)-1]
		return true
	}
	if len(report.People) > 1 {
		report.People = report.People[:len(report.People)-1]
		return true
	}
	if len(report.Groups) > 0 {
		report.Groups = report.Groups[:len(report.Groups)-1]
		return true
	}
	if len(report.Repositories) > 0 {
		report.Repositories = report.Repositories[:len(report.Repositories)-1]
		return true
	}
	if len(report.People) > 0 {
		report.People = report.People[:len(report.People)-1]
		return true
	}
	return false
}

func trimLowPriorityDigestGroup(report *TimelineReport) bool {
	groupIndex, itemIndex, priority := -1, -1, digestCorePriority
	for index := range report.Groups {
		group := &report.Groups[index]
		if group.Summarized {
			// A noisy source's one-line count is already its minimum truthful form.
			continue
		}
		for item := range group.Items {
			candidate := group.Items[item].priority
			if candidate < priority || candidate == priority && index >= groupIndex {
				groupIndex, itemIndex, priority = index, item, candidate
			}
		}
	}
	// A floor-priority source representative is less useful than a higher-priority extra
	// calendar line. Treat the boundary as removable so source diversity cannot hide the
	// named evidence the digest is meant to preserve.
	if groupIndex < 0 || priority > digestCorePriority {
		return false
	}
	group := &report.Groups[groupIndex]
	if itemIndex >= 0 && len(group.Items) > 1 {
		group.Items = append(group.Items[:itemIndex], group.Items[itemIndex+1:]...)
		return true
	}
	report.Groups = append(report.Groups[:groupIndex], report.Groups[groupIndex+1:]...)
	return true
}

func trimLowPriorityDigestEntity(values *[]string, priorities map[string]int) bool {
	for index := len(*values) - 1; index >= 0; index-- {
		if priorities[(*values)[index]] >= digestCorePriority {
			continue
		}
		*values = append((*values)[:index], (*values)[index+1:]...)
		return true
	}
	return false
}

func lowestPriorityDigestItem(group *DigestGroup) int {
	index := 0
	for candidate := 1; candidate < len(group.Items); candidate++ {
		if group.Items[candidate].priority <= group.Items[index].priority {
			index = candidate
		}
	}
	return index
}

func marshalTimelineJSON(report *TimelineReport) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		panic(fmt.Errorf("encode timeline report: %w", err))
	}
	return buffer.Bytes()
}

func marshalTimelineJSONL(report *TimelineReport) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		panic(fmt.Errorf("encode timeline report as JSONL: %w", err))
	}
	return buffer.Bytes()
}

func marshalTimelineCompactJSON(report *TimelineReport) []byte {
	encoded, err := json.Marshal(report)
	if err != nil {
		panic(fmt.Errorf("encode compact timeline report: %w", err))
	}
	return encoded
}

// marshalTimelineDelivery mirrors the exact public encoder selected by each caller. Whitespace,
// the trailing newline, and JSON HTML escaping all count because the budget is a byte contract.
func marshalTimelineDelivery(report *TimelineReport) []byte {
	switch report.Receipt.Format {
	case DigestDeliveryText:
		return []byte(RenderTimelineText(report))
	case DigestDeliveryJSONL:
		return marshalTimelineJSONL(report)
	case DigestDeliveryCompactJSON:
		return marshalTimelineCompactJSON(report)
	default:
		return marshalTimelineJSON(report)
	}
}

func bytesToTokens(size int) int { return (size + 3) / 4 }

// RenderTimelineText is the canonical compact line rendering used by the CLI and session hook.
// The service owns it so budget accounting measures the exact bytes every caller delivers.
func RenderTimelineText(report *TimelineReport) string {
	var output strings.Builder
	for _, group := range report.Groups {
		if group.Summarized {
			fmt.Fprintf(&output, "[%s] %d records summarized\n", group.Source, group.Count)
			continue
		}
		shown := 0
		for _, item := range group.Items {
			shown += digestItemCount(item)
		}
		fmt.Fprintf(&output, "[%s] %d/%d records\n", group.Source, shown, group.Count)
		for _, item := range group.Items {
			count := ""
			if item.Count > 1 {
				count = fmt.Sprintf(" x%d", item.Count)
			}
			fmt.Fprintf(&output, "%s %s%s · %s\n", item.Time, item.Title, count, item.URI)
		}
	}
	if len(report.People) > 0 {
		fmt.Fprintf(&output, "people: %s\n", strings.Join(report.People, ", "))
	}
	if len(report.Repositories) > 0 {
		fmt.Fprintf(&output, "repositories: %s\n", strings.Join(report.Repositories, ", "))
	}
	receipt := report.Receipt
	fmt.Fprintf(&output, "receipt: %s..%s · records %d · selected %d · dropped %d\n",
		receipt.Window.Since, receipt.Window.Until, receipt.Records, receipt.Selected, receipt.Dropped)
	fmt.Fprintf(&output, "receipt: budget %d · used %d · json %d · text %d\n",
		receipt.Budget, receipt.UsedTokens, receipt.JSONTokens, receipt.TextTokens)
	fmt.Fprintf(&output, "receipt: as_of %s · input_sha256 %s\n", receipt.AsOf, receipt.InputDigest)
	return output.String()
}

func digestOneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func digestEntities(
	records []FindRecord, resolver *IdentityResolver, includeOwner bool,
) ([]string, []string, map[string]int, map[string]int) {
	people, repositories := map[string]int{}, map[string]int{}
	for _, record := range records {
		priority := digestRecordPriority(record)
		for _, value := range recordRelationValues(record) {
			value = resolver.Canonical(value)
			switch digestIdentityKind(resolver, value) {
			case core.IdentityPerson:
				if includeOwner || !resolver.IsOwner(value) {
					people[value] = max(people[value], priority)
				}
			case core.IdentityRepository:
				repositories[value] = max(repositories[value], priority)
			}
		}
	}
	return sortedWeightedSet(people), sortedWeightedSet(repositories), people, repositories
}

func sortedWeightedSet(values map[string]int) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if values[result[i]] != values[result[j]] {
			return values[result[i]] > values[result[j]]
		}
		return result[i] < result[j]
	})
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneDigestFields(fields map[string][]string) map[string][]string {
	if len(fields) == 0 {
		return nil
	}
	cloned := make(map[string][]string, len(fields))
	for name, values := range fields {
		cloned[name] = append([]string(nil), values...)
	}
	return cloned
}

func digestReportInput(
	records []FindRecord,
	request TimelineRequest,
	window Window,
	asOf string,
	people, repositories []string,
) string {
	// Relation membership and the resolved entity lists are semantic inputs even though the
	// compact record projection does not expose its internal relation-field set.
	type inputRecord struct {
		URI, Source, Time, Title string
		GroupTitle, GroupKey     string
		Priority                 int
		Fields                   map[string][]string
		Relations                []string
	}
	input := struct {
		Window         Window
		AsOf           string
		Sources        []string
		Repository     string
		Person         string
		AroundURI      string
		Around         string
		Budget         int
		All            bool
		DeliveryFormat string
		People         []string
		Repositories   []string
		Records        []inputRecord
	}{
		Window: window, AsOf: asOf, Sources: append([]string(nil), request.Sources...),
		Repository: request.Repository, Person: request.Person, AroundURI: request.AroundURI,
		Around: request.Around.String(), Budget: request.Budget, All: request.All,
		DeliveryFormat: request.DeliveryFormat, People: append([]string(nil), people...),
		Repositories: append([]string(nil), repositories...),
		Records:      make([]inputRecord, 0, len(records)),
	}
	commitCounts := digestCommitCounts(records)
	for _, record := range records {
		groupTitle, groupKey := digestRecordGrouping(record, request.All, commitCounts)
		input.Records = append(input.Records, inputRecord{
			URI: record.URI, Source: record.Source, Time: record.Time, Title: record.Title,
			GroupTitle: groupTitle, GroupKey: groupKey, Priority: digestRecordPriority(record),
			Fields: cloneDigestFields(record.Fields), Relations: sortedSet(record.relations),
		})
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		panic(fmt.Errorf("marshal deterministic digest input: %w", err))
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
