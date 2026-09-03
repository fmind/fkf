package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/sources"
)

// selectWithinBudget admits the pins first — capped at a third of the budget so a pin can
// never crowd out the answer — then everything above the floor, richest first.
func selectWithinBudget(pack *ContextPack, candidates []*ContextItem, request ContextRequest) {
	selection := newContextSelection(pack, candidates, request)
	selection.admitPins()
	sourceLimit, pageTarget := selection.diversityTargets()
	selection.reserveAuthoredPages(pageTarget)
	selection.admitRanked(sourceLimit)
	selection.finish(pack.Receipt.Candidates)
}

func sortSelectedContextItems(items []ContextItem, newest bool) {
	var orders map[string]contextCandidateOrder
	if newest {
		orders = make(map[string]contextCandidateOrder, len(items))
		for index := range items {
			orders[items[index].URI] = newContextCandidateOrder(&items[index])
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Pinned != items[j].Pinned {
			return items[i].Pinned
		}
		return contextCandidatePrecedes(
			&items[i], &items[j], newest, orders[items[i].URI], orders[items[j].URI],
		)
	})
	reserveAuthoredContextOrder(items)
}

// reserveAuthoredContextOrder makes the one-fifth page reservation visible in bounded prefixes,
// not merely in the full admitted set. Without this, a large budget admitted the right pages
// but a top-k consumer could still see only transient task or event rows.
func reserveAuthoredContextOrder(items []ContextItem) {
	for end := 5; end <= len(items); end += 5 {
		wanted := end / 5
		present := 0
		for index := 0; index < end; index++ {
			if isPinnable(&items[index]) {
				present++
			}
		}
		for present < wanted {
			page := slices.IndexFunc(items[end:], func(item ContextItem) bool { return isPinnable(&item) })
			if page < 0 {
				break
			}
			page += end
			reserved := items[page]
			copy(items[end:page+1], items[end-1:page])
			items[end-1] = reserved
			present++
		}
	}
}

func enforceContextSourceShare(pack *ContextPack, used int) int {
	for {
		if len(pack.Items) < 5 {
			return used
		}
		counts := map[string]int{}
		for _, item := range pack.Items {
			if item.Source != "" && !item.explicitPolicy {
				counts[item.Source]++
			}
		}
		limit := max(1, (len(pack.Items)*2+4)/5)
		over := ""
		for source, count := range counts {
			if count > limit && (over == "" || source < over) {
				over = source
			}
		}
		if over == "" {
			return used
		}
		for index := len(pack.Items) - 1; index >= 0; index-- {
			if pack.Items[index].Source != over || pack.Items[index].explicitPolicy {
				continue
			}
			item := pack.Items[index]
			pack.Items = append(pack.Items[:index], pack.Items[index+1:]...)
			used -= item.Tokens
			pack.Receipt.Dropped = append(pack.Receipt.Dropped, DroppedItem{
				URI: item.URI, Reason: "source-cap", Score: item.Score, Tokens: item.Tokens,
			})
			break
		}
	}
}

// emptyPackWarning explains why nothing was selected. "Nothing matched" and "something matched
// but the budget was too small for any of it" look identical from an empty Items list alone,
// and the fix for one ("try fewer terms, a wider --since") is exactly wrong for the other
// ("raise --budget") — so the receipt has to say which one happened rather than leave a reader
// guessing from a generic message.
func emptyPackWarning(candidateCount int, dropped []DroppedItem, budget int) string {
	if candidateCount == 0 {
		return "no candidates in this window; try a wider --since, fewer filters, or `fkf status` to see what this base holds"
	}
	budgetDropped := 0
	for _, item := range dropped {
		if item.Reason == "budget" {
			budgetDropped++
		}
	}
	if budgetDropped > 0 {
		return fmt.Sprintf("matches exceed the %d-token budget; raise --budget", budget)
	}
	return "nothing matched; try fewer terms or a wider --since"
}

func sortCandidates(items []*ContextItem) {
	sortCandidatesForRequest(items, false)
}

func sortCandidatesForRequest(items []*ContextItem, newest bool) {
	var orders map[*ContextItem]contextCandidateOrder
	if newest {
		orders = make(map[*ContextItem]contextCandidateOrder, len(items))
		for _, item := range items {
			orders[item] = newContextCandidateOrder(item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return contextCandidatePrecedes(items[i], items[j], newest, orders[items[i]], orders[items[j]])
	})
}

type contextCandidateOrder struct {
	at                 string
	directMatchedTerms int
	windowAddressed    bool
}

func newContextCandidateOrder(item *ContextItem) contextCandidateOrder {
	return contextCandidateOrder{
		at:                 contextItemChronology(item),
		directMatchedTerms: contextDirectMatchedTerms(item),
		windowAddressed:    item.Date != "",
	}
}

// contextCandidatePrecedes keeps intent signals ahead of corpus-dependent point totals. An
// item's own id/title/page identity wins first, then broader meaningful-term coverage; a
// related entity identity wins over prose at equal coverage. A last query first asks for evidence
// addressed by the requested window, then exact identity, the strongest matching field, and
// direct-field coverage. A provider timestamp does not make a current-state index inventory
// window-addressed. Equivalent direct matches then use chronology, so an older item's extra body
// mention cannot hide the newest title-level match.
func contextCandidatePrecedes(
	left, right *ContextItem,
	newest bool,
	leftOrder, rightOrder contextCandidateOrder,
) bool {
	if newest {
		if leftOrder.windowAddressed != rightOrder.windowAddressed {
			return leftOrder.windowAddressed
		}
		leftAt, rightAt := leftOrder.at, rightOrder.at
		if (leftAt != "") != (rightAt != "") {
			return leftAt != ""
		}
		if left.explicitIdentity != right.explicitIdentity {
			return left.explicitIdentity
		}
		if left.directIdentity != right.directIdentity {
			return left.directIdentity
		}
		if left.matchedIdentity != right.matchedIdentity {
			return left.matchedIdentity
		}
		if left.matchWeight != right.matchWeight {
			return left.matchWeight > right.matchWeight
		}
		leftDirect, rightDirect := leftOrder.directMatchedTerms, rightOrder.directMatchedTerms
		if leftDirect != rightDirect {
			return leftDirect > rightDirect
		}
		if leftDirect > 0 && leftAt != rightAt {
			return leftAt > rightAt
		}
		if left.matchedTerms != right.matchedTerms {
			return left.matchedTerms > right.matchedTerms
		}
		if leftAt != rightAt {
			return leftAt > rightAt
		}
	}
	if !newest && left.directIdentity != right.directIdentity {
		return left.directIdentity
	}
	if !newest && left.matchedTerms != right.matchedTerms {
		return left.matchedTerms > right.matchedTerms
	}
	if !newest && left.matchedIdentity != right.matchedIdentity {
		return left.matchedIdentity
	}
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	if left.Time != right.Time {
		return left.Time > right.Time
	}
	return left.URI < right.URI
}

func contextDirectMatchedTerms(item *ContextItem) int {
	matched := 0
	for term, analysis := range item.termAnalysis {
		if !analysis.matched || !contextTermMatchesDirectEvidence(item, term, analysis) {
			continue
		}
		matched++
	}
	return matched
}

func contextTermMatchesDirectEvidence(item *ContextItem, term string, analysis contextTermAnalysis) bool {
	if analysis.identifierPriority > noIdentifierPriority ||
		contextTermMatches(item.URI, term) || contextTermMatches(item.Title, term) ||
		contextTermMatches(item.URL, term) || contextTermMatches(item.Status, term) {
		return true
	}
	for _, tag := range item.Tags {
		if contextTermMatches(tag, term) {
			return true
		}
	}
	for _, values := range item.Fields {
		for _, value := range values {
			if contextTermMatches(value, term) {
				return true
			}
		}
	}
	// Indexed record candidates retain only their best scoring segment per term, which may be
	// a body segment on a points tie. The visible fields above recover direct evidence in that
	// case; full-scan and authored-page candidates retain every matching segment here.
	return slices.ContainsFunc(analysis.segments, func(segment contextTermSegment) bool {
		return segment.Field != "body"
	})
}

func contextItemChronology(item *ContextItem) string {
	if item.Time != "" {
		if parsed, err := sources.ParseRecordTime(item.Time); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return item.Time
	}
	return item.Date
}

// estimateTokens is a deliberate approximation — four bytes to a token — because the budget
// only has to be reproducible and roughly right, and a real tokenizer would put a model in
// the read path.
// withReasons is separate because a candidate is built before it is scored, so at construction
// time Reasons is always empty — the loop below silently measured nothing, and an --explain
// pack was charged the same as a plain one while carrying every breakdown line. Selection
// re-estimates once scoring has run, and only when --explain will actually deliver them.
func estimateTokens(item *ContextItem, withReasons bool) int {
	size := len(item.URI) + len(item.Title) + len(item.Excerpt) + len(item.URL) +
		len(item.Kind) + len(item.Date) + len(item.Source)
	for _, values := range item.Fields {
		for _, value := range values {
			size += len(value)
		}
	}
	if withReasons {
		for _, reason := range item.Reasons {
			size += len(reason.Reason) + len(reason.Detail) + 8
		}
	}
	// JSON structure — keys, quotes, braces — is real payload too. A flat per-item allowance
	// keeps the estimate reproducible without encoding every candidate during ranking.
	size += jsonOverheadPerItem
	return (size + 3) / 4
}

// encodedTokens measures the selected structured representation with the same four-bytes-to-a-
// token rule the per-item estimate uses. Whitespace, HTML escaping, and the trailing newline
// are part of the delivery contract rather than transport-dependent guesses.
func encodedTokens(pack *ContextPack) int {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	if pack.Receipt.Format == ContextDeliveryJSON {
		encoder.SetIndent("", "  ")
	}
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(pack); err != nil {
		return 0
	}
	return (encoded.Len() + 3) / 4
}

// fitContextBudget turns the selection estimate into an exact delivery bound. Evidence is the
// useful payload, so the variable-length dropped-item detail is reduced first; DroppedTotal
// preserves the complete count. If the fixed, honest receipt itself cannot fit, the request is
// rejected with the minimum that this specific query needs instead of returning an oversize
// pack or stripping its trust notice.
func fitContextBudget(pack *ContextPack, budget int) error {
	details := append([]DroppedItem(nil), pack.Receipt.Dropped...)
	fullDropped := len(details)
	if pack.Receipt.DroppedTotal > fullDropped {
		fullDropped = pack.Receipt.DroppedTotal
	}
	pack.Receipt.Dropped = []DroppedItem{}
	pack.Receipt.DroppedTotal = fullDropped

	for stabilizeEncodedTokens(pack) > budget && len(pack.Items) > 0 {
		last := len(pack.Items) - 1
		item := pack.Items[last]
		pack.Items = pack.Items[:last]
		pack.Receipt.UsedTokens -= item.Tokens
		pack.Receipt.Selected = len(pack.Items)
		fullDropped++
		pack.Receipt.DroppedTotal = fullDropped
		details = append(details, DroppedItem{
			URI: item.URI, Reason: "budget", Score: item.Score, Tokens: item.Tokens,
			Pinned: item.Pinned,
		})
		if item.Pinned {
			pack.Receipt.RejectedPins = append(pack.Receipt.RejectedPins, item.URI)
			sort.Strings(pack.Receipt.RejectedPins)
		}
		if len(pack.Items) == 0 {
			pack.Receipt.Warning = emptyPackWarning(pack.Receipt.Candidates, details, budget)
		}
	}
	minimum := stabilizeEncodedTokens(pack)
	if minimum > budget {
		minimum = selfConsistentMinimum(pack, details, minimum)
		return &ContextBudgetError{Requested: budget, Minimum: minimum}
	}

	sortDropped(details)
	for _, detail := range details {
		pack.Receipt.Dropped = append(pack.Receipt.Dropped, detail)
		// An explicitly rejected pin is higher-value audit evidence than the weakest admitted
		// item. At moderate budgets, make room for that one bounded detail; at tiny receipt-only
		// budgets it still falls back to rejected_pins without violating the hard ceiling.
		for detail.Pinned && stabilizeEncodedTokens(pack) > budget && len(pack.Items) > 0 {
			last := len(pack.Items) - 1
			removed := pack.Items[last]
			pack.Items = pack.Items[:last]
			pack.Receipt.UsedTokens -= removed.Tokens
			pack.Receipt.Selected = len(pack.Items)
			fullDropped++
			pack.Receipt.DroppedTotal = fullDropped
			if removed.Pinned {
				pack.Receipt.RejectedPins = append(pack.Receipt.RejectedPins, removed.URI)
				sort.Strings(pack.Receipt.RejectedPins)
			}
			if len(pack.Items) == 0 {
				pack.Receipt.Warning = emptyPackWarning(pack.Receipt.Candidates, details, budget)
			}
		}
		if stabilizeEncodedTokens(pack) <= budget {
			continue
		}
		pack.Receipt.Dropped = pack.Receipt.Dropped[:len(pack.Receipt.Dropped)-1]
	}
	if len(pack.Receipt.Dropped) == fullDropped {
		pack.Receipt.DroppedTotal = 0
	}
	stabilizeEncodedTokens(pack)
	return nil
}

func selfConsistentMinimum(pack *ContextPack, details []DroppedItem, minimum int) int {
	for {
		pack.Receipt.Budget = minimum
		if len(pack.Items) == 0 {
			pack.Receipt.Warning = emptyPackWarning(pack.Receipt.Candidates, details, minimum)
		}
		required := stabilizeEncodedTokens(pack)
		if required <= minimum {
			return minimum
		}
		minimum = required
	}
}

// sortDropped keeps requested-pin refusals first, then other budget refusals, then below-floor
// noise. A pin that could not fit is a user-visible decision and must survive even a short
// receipt with thousands of matching candidates; URIs make each tier deterministic.
func sortDropped(items []DroppedItem) {
	priority := func(item DroppedItem) int {
		if item.Pinned {
			return 0
		}
		if item.Reason == "budget" {
			return 1
		}
		return 2
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := priority(items[i]), priority(items[j])
		if left != right {
			return left < right
		}
		return items[i].URI < items[j].URI
	})
}

// stabilizeEncodedTokens includes EncodedTokens's own digits in the measured payload. Starting
// from zero reaches a fixed point after at most the field's digit count changes once.
func stabilizeEncodedTokens(pack *ContextPack) int {
	pack.Receipt.EncodedTokens = 0
	for {
		measured := encodedTokens(pack)
		if measured == pack.Receipt.EncodedTokens {
			return measured
		}
		pack.Receipt.EncodedTokens = measured
	}
}

// inputDigest fixes the request and query-dependent candidate state. BuildContext binds this
// value to the generation digest of every searchable byte before publishing it, so a same-stat
// edit still changes the final receipt without serializing large candidate bodies a second time.
func inputDigest(
	request ContextRequest,
	candidates []*ContextItem,
	asOf string,
	recencyModel map[string]int,
	consultedBodies, truncatedEntities []string,
) string {
	type digestCandidate struct {
		URI, SupersededBy, SupersededRank                           string
		CollapsedURIs                                               []string
		Expanded, ExplicitIdentity, DirectIdentity, MatchedIdentity bool
		ExplicitPolicy, Pinned                                      bool
		Count, Score, MatchedTerms, MatchWeight                     int
	}
	type digestInput struct {
		RankingVersion  int
		AsOf, Query     string
		Window          Window
		Budget          int
		Pins            []string
		Expand, Explain bool
		Newest          bool
		DeliveryFormat  string
		Candidates      []digestCandidate
		RecencyModel    map[string]int
		ConsultedBodies []string
		Truncated       []string
	}
	input := digestInput{
		RankingVersion: RankingVersion, AsOf: asOf, Query: request.Query,
		Window: request.Window, Budget: request.Budget, Pins: request.Pins,
		Expand: request.Expand, Explain: request.Explain, Newest: request.Newest,
		DeliveryFormat:  request.DeliveryFormat,
		Candidates:      make([]digestCandidate, 0, len(candidates)),
		RecencyModel:    recencyModel,
		ConsultedBodies: append([]string(nil), consultedBodies...),
		Truncated:       append([]string(nil), truncatedEntities...),
	}
	for _, candidate := range candidates {
		input.Candidates = append(input.Candidates, digestCandidate{
			URI: candidate.URI, SupersededBy: candidate.supersededBy,
			SupersededRank: candidate.supersededRank,
			CollapsedURIs:  append([]string(nil), candidate.collapsedURIs...),
			Expanded:       candidate.expanded, ExplicitIdentity: candidate.explicitIdentity,
			DirectIdentity: candidate.directIdentity, MatchedIdentity: candidate.matchedIdentity,
			ExplicitPolicy: candidate.explicitPolicy, Pinned: candidate.Pinned,
			Count: candidate.Count, Score: candidate.Score,
			MatchedTerms: candidate.matchedTerms, MatchWeight: candidate.matchWeight,
		})
	}
	encoded, _ := json.Marshal(input) // This struct contains only JSON-native values.
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:16]
}

func truncateRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func compact(values []string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			kept = append(kept, value)
		}
	}
	return kept
}
