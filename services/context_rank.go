package services

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
)

type contextSegment struct {
	Field  string
	Text   string
	Weight int
}

func (item *ContextItem) addSegment(field, text string, weight int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	item.termAnalysis = nil
	item.segments = append(item.segments, contextSegment{Field: field, Text: text, Weight: max(1, weight)})
}

func (item *ContextItem) rebuildHaystack() {
	parts := make([]string, 0, len(item.segments))
	for _, segment := range item.segments {
		parts = append(parts, segment.Text)
	}
	item.haystack = strings.ToLower(strings.Join(parts, " "))
}

func (item *ContextItem) addIdentifier(value string) {
	value = normalizeIdentityKey(strings.TrimSpace(value))
	if value == "" {
		return
	}
	item.termAnalysis = nil
	if item.identifierKeys == nil {
		item.identifierKeys = map[string]struct{}{}
	}
	if item.directIdentifiers == nil {
		item.directIdentifiers = map[string]struct{}{}
	}
	item.identifierKeys[value] = struct{}{}
	item.directIdentifiers[value] = struct{}{}
}

func (item *ContextItem) addEntityIdentifier(value string) {
	item.addRelatedIdentifier(value)
	parsed, err := ParseURI(value)
	if err != nil || !parsed.IsEntity() {
		return
	}
	item.addRelatedIdentifier(parsed.Value)
	if slash := strings.LastIndex(parsed.Value, "/"); slash >= 0 && slash+1 < len(parsed.Value) {
		item.addRelatedIdentifier(parsed.Value[slash+1:])
	}
}

func (item *ContextItem) addRelatedIdentifier(value string) {
	value = normalizeIdentityKey(strings.TrimSpace(value))
	if value == "" {
		return
	}
	item.termAnalysis = nil
	if item.identifierKeys == nil {
		item.identifierKeys = map[string]struct{}{}
	}
	item.identifierKeys[value] = struct{}{}
}

func contextSchemaForSource(config *core.Config, source string) core.FieldSchema {
	if config != nil {
		if declared, found := config.Sources[source]; found {
			return declared.Schema
		}
		return config.Schema
	}
	return nil
}

func attachCachedContextBodies(
	ctx context.Context, base *Base, candidates []*ContextItem,
) ([]string, error) {
	uris := make([]string, 0, len(candidates))
	byURI := make(map[string]*ContextItem, len(candidates))
	for _, candidate := range candidates {
		uris = append(uris, candidate.URI)
		byURI[candidate.URI] = candidate
	}
	bodies, err := CachedBodiesForURIs(ctx, base, uris)
	if err != nil {
		return nil, err
	}
	consulted := make([]string, 0, len(bodies))
	for uri, body := range bodies {
		candidate := byURI[uri]
		if candidate == nil {
			continue
		}
		candidate.body = body
		candidate.addSegment("body", body, core.DefaultFieldWeight)
		candidate.rebuildHaystack()
		candidate.Tokens = estimateTokens(candidate, false)
		consulted = append(consulted, uri)
	}
	sort.Strings(consulted)
	return consulted, nil
}

func collapseContextCandidates(candidates []*ContextItem) []*ContextItem {
	byRun := make(map[string]*ContextItem)
	collapsed := make([]*ContextItem, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Kind != "record" {
			collapsed = append(collapsed, candidate)
			continue
		}
		// Historical v1 evidence may predate the meaningful-title collection contract. Keep an
		// ungroupable record as an explicit singleton so fallback and indexed collapse semantics
		// remain identical without rewriting or discarding durable evidence.
		candidate.collapsedURIs = []string{candidate.URI}
		if candidate.Source == "" || strings.TrimSpace(candidate.Title) == "" {
			collapsed = append(collapsed, candidate)
			continue
		}
		key := candidate.Source + "\x00" + strings.ToLower(strings.TrimSpace(candidate.Title))
		kept, found := byRun[key]
		if !found {
			byRun[key] = candidate
			collapsed = append(collapsed, candidate)
			continue
		}
		kept.collapsedURIs = append(kept.collapsedURIs, candidate.URI)
		kept.Count = len(kept.collapsedURIs)
		if contextItemChronology(candidate) > contextItemChronology(kept) {
			index := 0
			for collapsed[index] != kept {
				index++
			}
			candidate.collapsedURIs = kept.collapsedURIs
			candidate.Count = len(candidate.collapsedURIs)
			byRun[key], collapsed[index] = candidate, candidate
		}
	}
	for _, candidate := range collapsed {
		sort.Strings(candidate.collapsedURIs)
	}
	sort.Slice(collapsed, func(i, j int) bool { return collapsed[i].URI < collapsed[j].URI })
	return collapsed
}

// collapseContextResourceDuplicates keeps one representation when multiple sources project
// the same exact external resource URL. The query's coverage chooses first; verified body and
// projected-field richness then retain the representation that can best answer a follow-up.
// Every stored URI remains named in collapsedURIs, so this removes duplicate pack entries, not
// provenance.
func collapseContextResourceDuplicates(candidates []*ContextItem, terms []string) []*ContextItem {
	byURL := make(map[string]int)
	collapsed := make([]*ContextItem, 0, len(candidates))
	for _, candidate := range candidates {
		url := strings.TrimSpace(candidate.URL)
		if candidate.Kind != "record" || url == "" {
			collapsed = append(collapsed, candidate)
			continue
		}
		index, found := byURL[url]
		if !found {
			candidate.collapsedURIs = contextCollapsedURIs(candidate)
			candidate.Count = contextCollapsedCount(candidate.collapsedURIs)
			byURL[url] = len(collapsed)
			collapsed = append(collapsed, candidate)
			continue
		}

		kept := collapsed[index]
		members := append(contextCollapsedURIs(kept), contextCollapsedURIs(candidate)...)
		if contextResourceRepresentationPrecedes(candidate, kept, terms) {
			kept = candidate
		}
		sort.Strings(members)
		kept.collapsedURIs = slices.Compact(members)
		kept.Count = contextCollapsedCount(kept.collapsedURIs)
		collapsed[index] = kept
	}
	sort.Slice(collapsed, func(i, j int) bool { return collapsed[i].URI < collapsed[j].URI })
	return collapsed
}

func contextResourceRepresentationPrecedes(left, right *ContextItem, terms []string) bool {
	leftMatches, rightMatches := contextCandidateMatchCount(left, terms), contextCandidateMatchCount(right, terms)
	if leftMatches != rightMatches {
		return leftMatches > rightMatches
	}
	if contextCandidateHasBody(left) != contextCandidateHasBody(right) {
		return contextCandidateHasBody(left)
	}
	leftFields, rightFields := contextFieldValueCount(left.Fields), contextFieldValueCount(right.Fields)
	if leftFields != rightFields {
		return leftFields > rightFields
	}
	leftAt, rightAt := contextItemChronology(left), contextItemChronology(right)
	if leftAt != rightAt {
		return leftAt > rightAt
	}
	return left.URI < right.URI
}

func contextCandidateHasBody(candidate *ContextItem) bool {
	return candidate.body != "" || candidate.bodyAvailable
}

func contextCandidateMatchCount(candidate *ContextItem, terms []string) int {
	count := 0
	for _, term := range terms {
		if candidateMatchesTerm(candidate, term) {
			count++
		}
	}
	return count
}

func contextFieldValueCount(fields map[string][]string) int {
	count := 0
	for _, values := range fields {
		count += len(values)
	}
	return count
}

func contextCollapsedURIs(candidate *ContextItem) []string {
	if len(candidate.collapsedURIs) > 0 {
		return append([]string(nil), candidate.collapsedURIs...)
	}
	return []string{candidate.URI}
}

func contextCollapsedCount(uris []string) int {
	if len(uris) > 1 {
		return len(uris)
	}
	return 0
}

func configuredRecencyModel(config *core.Config) map[string]int {
	model := map[string]int{}
	if config == nil {
		return model
	}
	for _, name := range config.SourceNames() {
		if halfLife := config.Sources[name].Recency.HalfLifeDays; halfLife > 0 {
			model[name] = halfLife
		}
	}
	return model
}

func applyContextSupersedes(candidates []*ContextItem) {
	byURI := make(map[string]*ContextItem, len(candidates))
	for _, candidate := range candidates {
		if candidate.Kind != "record" {
			byURI[candidate.URI] = candidate
		}
	}
	byTarget := map[string][]*ContextItem{}
	for _, candidate := range candidates {
		for _, target := range candidate.supersedes {
			if byURI[target] != nil && target != candidate.URI {
				byTarget[target] = append(byTarget[target], candidate)
			}
		}
	}
	for target, superseders := range byTarget {
		sort.SliceStable(superseders, func(i, j int) bool {
			if superseders[i].validityRank != superseders[j].validityRank {
				return superseders[i].validityRank > superseders[j].validityRank
			}
			return superseders[i].URI < superseders[j].URI
		})
		winner := superseders[0]
		setContextSuperseder(byURI[target], winner)
		for _, loser := range superseders[1:] {
			setContextSuperseder(loser, winner)
		}
	}
}

func setContextSuperseder(candidate, winner *ContextItem) {
	if candidate == nil || winner == nil || candidate == winner {
		return
	}
	if candidate.supersededBy == "" || winner.validityRank > candidate.supersededRank ||
		winner.validityRank == candidate.supersededRank && winner.URI < candidate.supersededBy {
		candidate.supersededBy = winner.URI
		candidate.supersededRank = winner.validityRank
	}
}

func firstContextFieldValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func contextCandidateAllowed(item *ContextItem, terms []string) bool {
	if item.defaultExcluded == "" || item.Pinned || item.explicitIdentity || item.explicitPolicy {
		return true
	}
	return contextPolicyExplicit(item, terms)
}

func contextPolicyExplicit(item *ContextItem, terms []string) bool {
	if item.defaultExcluded == "" {
		return false
	}
	_, value, _ := strings.Cut(item.defaultExcluded, ":")
	for _, term := range terms {
		if strings.EqualFold(term, value) || strings.EqualFold(term, item.defaultExcluded) {
			return true
		}
	}
	return false
}

type contextSelection struct {
	pack       *ContextPack
	ranked     []*ContextItem
	request    ContextRequest
	itemBudget int
	used       int
	selected   map[string]struct{}
}

func newContextSelection(
	pack *ContextPack, candidates []*ContextItem, request ContextRequest,
) *contextSelection {
	ranked := append([]*ContextItem(nil), candidates...)
	// Candidates are built before scoring attaches reason lines. Re-estimate their final
	// delivered form so --explain spends budget on the evidence it actually returns.
	for _, item := range ranked {
		item.Tokens = contextSelectionTokens(item, request)
	}
	sortCandidatesForRequest(ranked, request.Newest)
	// The receipt ships with the pack, so reserve its cost before admitting items.
	itemBudget := max(request.Budget/2, request.Budget-receiptReserve(request.Budget))
	if request.DeliveryFormat == ContextDeliveryText {
		// Text item costs exclude the receipt, so provisionally use the whole budget and let the
		// exact text fitter remove the weakest tail. Starting with too few items cannot be
		// repaired after ranking because the omitted candidates are no longer in the pack.
		itemBudget = request.Budget
	}
	return &contextSelection{
		pack:       pack,
		ranked:     ranked,
		request:    request,
		itemBudget: itemBudget,
		selected:   map[string]struct{}{},
	}
}

func (selection *contextSelection) admit(item *ContextItem, ceiling int) bool {
	if selection.used+item.Tokens > ceiling {
		selection.drop(item, "budget")
		if item.Pinned {
			selection.pack.Receipt.RejectedPins = append(selection.pack.Receipt.RejectedPins, item.URI)
		}
		return false
	}
	selection.used += item.Tokens
	selection.pack.Items = append(selection.pack.Items, *item)
	selection.selected[item.URI] = struct{}{}
	return true
}

func (selection *contextSelection) admitPins() {
	pins := make(map[string]struct{}, len(selection.request.Pins))
	for _, pin := range selection.request.Pins {
		pins[pin] = struct{}{}
	}
	for _, item := range selection.ranked {
		if !isPinnable(item) {
			continue
		}
		if _, pinned := pins[item.URI]; !pinned {
			continue
		}
		item.Pinned = true
		item.addReason("pinned", 0, "--pin "+item.URI)
		// Pinning changes both the delivered flag and, with --explain, its reason line.
		item.Tokens = contextSelectionTokens(item, selection.request)
		selection.admit(item, selection.itemBudget/3)
	}
}

func contextSelectionTokens(item *ContextItem, request ContextRequest) int {
	if request.DeliveryFormat != ContextDeliveryText {
		return estimateTokens(item, request.Explain)
	}
	return estimateContextTextItemTokens(item, request.Explain)
}

// estimateContextTextItemTokens mirrors the compact line's delivered fields. Raw byte lengths
// are a safe upper bound for values whose terminal-active runes are replaced by one space at
// rendering time; the service's exact final pass still owns the complete receipt-inclusive bound.
func estimateContextTextItemTokens(item *ContextItem, withReasons bool) int {
	size := len(strconv.Itoa(item.Score)) + 1 + len(item.Kind) + 1 + contextTextValueLen(item.Date) +
		1 + len(item.URI) + 1 + contextTextValueLen(item.Title) + 1 // final newline
	fields, fieldBytes := 0, 0
	addField := func(length int) {
		if fields > 0 {
			fieldBytes++ // separator between compact fields
		}
		fieldBytes += length
		fields++
	}
	if item.Source != "" {
		addField(len("source=") + len(item.Source))
	}
	if item.Status != "" {
		addField(len("status=") + len(item.Status))
	}
	if len(item.Tags) > 0 {
		length := len("tags=") + len(item.Tags) - 1
		for _, tag := range item.Tags {
			length += len(tag)
		}
		addField(length)
	}
	for name, values := range item.Fields {
		length := len(name) + 1
		if len(values) > 0 {
			length += len(values) - 1
		}
		for _, value := range values {
			length += len(value)
		}
		addField(length)
	}
	if item.Pinned {
		addField(len("pinned=true"))
	}
	if item.Count > 1 {
		addField(len("count=") + len(strconv.Itoa(item.Count)))
	}
	if withReasons && len(item.Reasons) > 0 {
		length := len("why=") + len(item.Reasons) - 1
		for _, reason := range item.Reasons {
			length += len(reason.Reason) + 1 + len(strconv.Itoa(reason.Points)) + 1 // explicit sign
			if reason.Detail != "" {
				length += len(reason.Detail) + 2
			}
		}
		addField(length)
	}
	if fields > 0 {
		size += len(" · ") + fieldBytes
	}
	return (size + 3) / 4
}

func contextTextValueLen(value string) int {
	if strings.TrimSpace(value) == "" {
		return 1 // the renderer's dash placeholder
	}
	return len(value)
}

func (selection *contextSelection) diversityTargets() (int, int) {
	// Size shares against what the budget could admit before diversity. This keeps the 40%
	// source cap stable instead of changing it as each item is greedily appended.
	targetItems, targetTokens := 0, 0
	for _, item := range selection.ranked {
		if item.Score < relevanceFloor || !contextCandidateAllowed(item, selection.pack.Receipt.Terms) {
			continue
		}
		if targetTokens+item.Tokens <= selection.itemBudget {
			targetTokens += item.Tokens
			targetItems++
		}
	}
	sourceLimit := max(1, (targetItems*2+4)/5)
	if targetItems < 5 {
		sourceLimit = max(1, targetItems)
	}
	return sourceLimit, max(1, (targetItems+4)/5)
}

func (selection *contextSelection) reserveAuthoredPages(target int) {
	selectedPages := countAuthoredContextItems(selection.pack.Items)
	// Reserve one fifth of the potential slots for authored wiki/project evidence. Final
	// ordering remains score-first; reservation affects admission, not answer order.
	for _, item := range selection.ranked {
		if selectedPages >= target {
			return
		}
		if item.Pinned || item.Score < relevanceFloor || !isPinnable(item) {
			continue
		}
		if selection.admit(item, selection.itemBudget) {
			selectedPages++
		}
	}
}

func countAuthoredContextItems(items []ContextItem) int {
	count := 0
	for index := range items {
		if isPinnable(&items[index]) {
			count++
		}
	}
	return count
}

func (selection *contextSelection) admitRanked(sourceLimit int) {
	sourceCounts := selectedContextSourceCounts(selection.pack.Items)
	for _, item := range selection.ranked {
		if item.Pinned {
			continue
		}
		if _, found := selection.selected[item.URI]; found {
			continue
		}
		if reason := selection.exclusionReason(item, sourceCounts, sourceLimit); reason != "" {
			selection.drop(item, reason)
			continue
		}
		if selection.admit(item, selection.itemBudget) && item.Source != "" && !item.explicitPolicy {
			sourceCounts[item.Source]++
		}
	}
}

func selectedContextSourceCounts(items []ContextItem) map[string]int {
	counts := map[string]int{}
	for _, item := range items {
		if item.Source != "" && !item.explicitPolicy {
			counts[item.Source]++
		}
	}
	return counts
}

func (selection *contextSelection) exclusionReason(
	item *ContextItem, sourceCounts map[string]int, sourceLimit int,
) string {
	if item.Score < relevanceFloor {
		return "below-floor"
	}
	if !contextCandidateAllowed(item, selection.pack.Receipt.Terms) {
		return "default-excluded"
	}
	if item.Source != "" && !item.explicitPolicy && sourceCounts[item.Source] >= sourceLimit {
		return "source-cap"
	}
	return ""
}

func (selection *contextSelection) drop(item *ContextItem, reason string) {
	dropped := DroppedItem{URI: item.URI, Reason: reason, Score: item.Score}
	// RejectedPins already preserves a pin independently of this bounded detail. Omitting its
	// estimated token cost keeps the higher-value URI/reason audit line viable in tiny packs.
	if reason == "source-cap" || reason == "budget" && !item.Pinned {
		dropped.Tokens = item.Tokens
	}
	if reason == "budget" {
		dropped.Pinned = item.Pinned
	}
	selection.pack.Receipt.Dropped = append(selection.pack.Receipt.Dropped, dropped)
}

func (selection *contextSelection) finish(candidateCount int) {
	sortSelectedContextItems(selection.pack.Items, selection.request.Newest)
	selection.used = enforceContextSourceShare(selection.pack, selection.used)
	sort.Strings(selection.pack.Receipt.RejectedPins)
	sortDropped(selection.pack.Receipt.Dropped)
	// Compute the warning before the bounded dropped list is cut, so the empty result remains
	// honest even when its receipt cannot enumerate every rejected candidate.
	if len(selection.pack.Items) == 0 {
		selection.pack.Receipt.Warning = emptyPackWarning(
			candidateCount, selection.pack.Receipt.Dropped, selection.request.Budget,
		)
	}
	if limit := droppedCap(selection.request.Budget); len(selection.pack.Receipt.Dropped) > limit {
		selection.pack.Receipt.DroppedTotal = len(selection.pack.Receipt.Dropped)
		selection.pack.Receipt.Dropped = selection.pack.Receipt.Dropped[:limit]
	}
	selection.pack.Receipt.UsedTokens = selection.used
	selection.pack.Receipt.Selected = len(selection.pack.Items)
}
