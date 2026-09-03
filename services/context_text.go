package services

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// RenderContextText is the canonical compact delivery used by terminals and session hooks.
// Keeping it beside selection lets the service measure and fill the exact bytes it returns.
func RenderContextText(pack *ContextPack) string {
	if pack == nil {
		return ""
	}
	return contextTextBlock(renderContextTextRaw(pack, true))
}

func renderContextTextRaw(pack *ContextPack, includeItems bool) string {
	var output strings.Builder
	receipt := pack.Receipt
	fmt.Fprintf(&output, "notice %s\n", receipt.Notice)
	if len(pack.Items) == 0 {
		fmt.Fprintf(&output, "warning %s\n", receipt.Warning)
	}
	if includeItems {
		for _, item := range pack.Items {
			output.WriteString(renderContextTextItem(item))
		}
	}
	dropped := len(receipt.Dropped)
	if receipt.DroppedTotal > 0 {
		dropped = receipt.DroppedTotal
	}
	fmt.Fprintf(&output, "receipt pack for %q · %d/%d selected · %d/%d %s tokens · floor %d\n",
		pack.Query, receipt.Selected, receipt.Candidates, receipt.EncodedTokens, receipt.Budget,
		contextTextOrDash(receipt.Format), receipt.Floor)
	fmt.Fprintf(&output, "window %s · as_of %s", renderContextWindow(receipt.Window), receipt.AsOf)
	if receipt.NewestEventDay != "" {
		fmt.Fprintf(&output, " · newest %s (%dd stale)", receipt.NewestEventDay, receipt.StaleDays)
	}
	output.WriteByte('\n')
	fmt.Fprintf(&output, "digest %s · ranking v%d · fkf %s · dropped %d",
		receipt.InputDigest, receipt.RankingVersion, receipt.ToolVersion, dropped)
	if state := renderLexicalIndexUse(receipt.Index); state != "" {
		fmt.Fprintf(&output, " · index %s", state)
	}
	output.WriteByte('\n')
	if receipt.SinceReceipt != "" {
		fmt.Fprintf(&output, "delta since %s · changed %d\n", receipt.SinceReceipt, receipt.Changed)
	}
	if receipt.UnharvestedBullets > 0 {
		fmt.Fprintf(&output, "learn %d unharvested bullet(s) · fkf list tasks learned --unharvested\n",
			receipt.UnharvestedBullets)
	}
	return output.String()
}

func renderContextTextItem(item ContextItem) string {
	var output strings.Builder
	fmt.Fprintf(&output, "%d %s %s %s %s", item.Score, item.Kind,
		contextTextOrDash(item.Date), item.URI, contextTextOrDash(contextTextInline(item.Title)))
	if fields := compactContextTextFields(item); len(fields) > 0 {
		fmt.Fprintf(&output, " · %s", strings.Join(fields, " "))
	}
	output.WriteByte('\n')
	return output.String()
}

func fitContextTextBudget(
	pack *ContextPack,
	budget int,
	candidates []*ContextItem,
	request ContextRequest,
) error {
	requested := budget
	pack.Receipt.Budget = budget
	pack.Receipt.Format = ContextDeliveryText
	fullDropped := contextDroppedCount(pack.Receipt)
	for stabilizeContextTextTokens(pack) > budget && len(pack.Items) > 0 {
		last := len(pack.Items) - 1
		item := pack.Items[last]
		pack.Items = pack.Items[:last]
		pack.Receipt.UsedTokens = max(0, pack.Receipt.UsedTokens-item.Tokens)
		pack.Receipt.Selected = len(pack.Items)
		fullDropped++
		pack.Receipt.Dropped = append(pack.Receipt.Dropped, DroppedItem{
			URI: item.URI, Reason: "budget", Score: item.Score, Tokens: item.Tokens, Pinned: item.Pinned,
		})
		if item.Pinned {
			pack.Receipt.RejectedPins = append(pack.Receipt.RejectedPins, item.URI)
			slices.Sort(pack.Receipt.RejectedPins)
			pack.Receipt.RejectedPins = slices.Compact(pack.Receipt.RejectedPins)
		}
		setContextDroppedCount(&pack.Receipt, fullDropped)
	}
	if len(pack.Items) == 0 && pack.Receipt.Candidates > 0 {
		pack.Receipt.Warning = emptyPackWarning(pack.Receipt.Candidates, pack.Receipt.Dropped, budget)
	}
	minimum := stabilizeContextTextTokens(pack)
	if minimum > budget {
		for {
			pack.Receipt.Budget = minimum
			if len(pack.Items) == 0 && pack.Receipt.Candidates > 0 {
				pack.Receipt.Warning = emptyPackWarning(pack.Receipt.Candidates, pack.Receipt.Dropped, minimum)
			}
			required := stabilizeContextTextTokens(pack)
			if required <= minimum {
				return &ContextBudgetError{Requested: requested, Minimum: minimum}
			}
			minimum = required
		}
	}

	backfillContextText(pack, candidates, request, budget)
	boundContextTextDrops(pack)
	stabilizeContextTextTokens(pack)
	return nil
}

// backfillContextText tries every remaining candidate in ranking order. A large candidate may
// not fit while a later compact one does, so stopping at the first miss would leave useful
// delivery space empty. Each trial includes the changed receipt digits and dropped count.
func backfillContextText(pack *ContextPack, candidates []*ContextItem, request ContextRequest, budget int) {
	ranked := append([]*ContextItem(nil), candidates...)
	sortCandidatesForRequest(ranked, request.Newest)
	selected := make(map[string]struct{}, len(pack.Items))
	itemBytes := make(map[string]int, len(pack.Items)+1)
	for _, item := range pack.Items {
		selected[item.URI] = struct{}{}
		itemBytes[item.URI] = len(contextTextBlock(renderContextTextItem(item)))
	}
	fastTrial := len(pack.Items) > 0 && pack.Receipt.Warning == ""
	receiptBase, selectedBytes := 0, 0
	if fastTrial {
		receiptBase = contextTextMutableReceiptBase(pack)
		for _, size := range itemBytes {
			selectedBytes += size
		}
	}
	for _, candidate := range ranked {
		if _, found := selected[candidate.URI]; found {
			continue
		}
		if !candidate.Pinned && (candidate.Score < relevanceFloor ||
			!contextCandidateAllowed(candidate, pack.Receipt.Terms)) {
			continue
		}
		if !contextTextSourceAdmitted(pack.Items, candidate) ||
			candidate.Pinned && contextPinnedTokens(pack.Items)+candidate.Tokens > budget/3 {
			continue
		}
		if pack.Receipt.UsedTokens+candidate.Tokens > budget {
			continue
		}
		candidateBytes := len(contextTextBlock(renderContextTextItem(*candidate)))
		itemBytes[candidate.URI] = candidateBytes
		if fastTrial {
			trialTokens := contextTextTrialTokens(
				receiptBase, len(pack.Items)+1, max(0, contextDroppedCount(pack.Receipt)-1),
				selectedBytes+candidateBytes,
			)
			if trialTokens > budget {
				continue
			}
			pack.Items = append(pack.Items, *candidate)
			pack.Receipt.Selected = len(pack.Items)
			pack.Receipt.UsedTokens += candidate.Tokens
			removeContextTextDrop(&pack.Receipt, candidate.URI)
			pack.Receipt.RejectedPins = slices.DeleteFunc(pack.Receipt.RejectedPins, func(uri string) bool {
				return uri == candidate.URI
			})
			pack.Receipt.Warning = ""
			pack.Receipt.EncodedTokens = trialTokens
			selected[candidate.URI] = struct{}{}
			selectedBytes += candidateBytes
			continue
		}
		trial := cloneContextTextPack(pack)
		trial.Items = append(trial.Items, *candidate)
		sortSelectedContextItems(trial.Items, request.Newest)
		trial.Receipt.Selected = len(trial.Items)
		trial.Receipt.UsedTokens += candidate.Tokens
		removeContextTextDrop(&trial.Receipt, candidate.URI)
		trial.Receipt.RejectedPins = slices.DeleteFunc(trial.Receipt.RejectedPins, func(uri string) bool {
			return uri == candidate.URI
		})
		trial.Receipt.Warning = ""
		if stabilizeContextTextTokensWithItemBytes(trial, itemBytes) > budget {
			continue
		}
		*pack = *trial
		selected[candidate.URI] = struct{}{}
	}
	if fastTrial {
		sortSelectedContextItems(pack.Items, request.Newest)
	}
}

// The compact receipt does not render dropped details, used-token estimates, or rejected-pin
// arrays. During backfill only three printed integers change, so exact byte accounting can test
// a candidate without cloning and re-rendering the whole pack on every miss.
func contextTextMutableReceiptBase(pack *ContextPack) int {
	encoded := pack.Receipt.EncodedTokens
	pack.Receipt.EncodedTokens = 0
	bytes := len(contextTextBlock(renderContextTextRaw(pack, false)))
	pack.Receipt.EncodedTokens = encoded
	return bytes - len(strconv.Itoa(pack.Receipt.Selected)) - 1 -
		len(strconv.Itoa(contextDroppedCount(pack.Receipt)))
}

func contextTextTrialTokens(receiptBase, selected, dropped, itemBytes int) int {
	fixed := receiptBase + len(strconv.Itoa(selected)) + len(strconv.Itoa(dropped)) + itemBytes
	encoded := 0
	for {
		measured := (fixed + len(strconv.Itoa(encoded)) + 3) / 4
		if measured == encoded {
			return measured
		}
		encoded = measured
	}
}

func cloneContextTextPack(pack *ContextPack) *ContextPack {
	cloned := *pack
	cloned.Items = append([]ContextItem(nil), pack.Items...)
	cloned.Receipt = pack.Receipt
	cloned.Receipt.Terms = append([]string(nil), pack.Receipt.Terms...)
	cloned.Receipt.Dropped = append([]DroppedItem(nil), pack.Receipt.Dropped...)
	cloned.Receipt.RejectedPins = append([]string(nil), pack.Receipt.RejectedPins...)
	cloned.Receipt.ConsultedBodies = append([]string(nil), pack.Receipt.ConsultedBodies...)
	cloned.Receipt.TruncatedEntities = append([]string(nil), pack.Receipt.TruncatedEntities...)
	return &cloned
}

func contextPinnedTokens(items []ContextItem) int {
	tokens := 0
	for _, item := range items {
		if item.Pinned {
			tokens += item.Tokens
		}
	}
	return tokens
}

func contextTextSourceAdmitted(items []ContextItem, candidate *ContextItem) bool {
	if candidate.Source == "" || candidate.explicitPolicy {
		return true
	}
	count := 1
	for _, item := range items {
		if item.Source == candidate.Source && !item.explicitPolicy {
			count++
		}
	}
	limit := max(1, ((len(items)+1)*2+4)/5)
	return count <= limit
}

func contextDroppedCount(receipt Receipt) int {
	return max(len(receipt.Dropped), receipt.DroppedTotal)
}

func setContextDroppedCount(receipt *Receipt, total int) {
	if total > len(receipt.Dropped) {
		receipt.DroppedTotal = total
	} else {
		receipt.DroppedTotal = 0
	}
}

func removeContextTextDrop(receipt *Receipt, uri string) {
	total := contextDroppedCount(*receipt)
	for index, dropped := range receipt.Dropped {
		if dropped.URI != uri {
			continue
		}
		receipt.Dropped = append(receipt.Dropped[:index], receipt.Dropped[index+1:]...)
		break
	}
	if total > 0 {
		total--
	}
	setContextDroppedCount(receipt, total)
}

func boundContextTextDrops(pack *ContextPack) {
	total := contextDroppedCount(pack.Receipt)
	sortDropped(pack.Receipt.Dropped)
	if limit := droppedCap(pack.Receipt.Budget); len(pack.Receipt.Dropped) > limit {
		pack.Receipt.Dropped = pack.Receipt.Dropped[:limit]
	}
	setContextDroppedCount(&pack.Receipt, total)
}

func stabilizeContextTextTokens(pack *ContextPack) int {
	itemBytes := make(map[string]int, len(pack.Items))
	for _, item := range pack.Items {
		itemBytes[item.URI] = len(contextTextBlock(renderContextTextItem(item)))
	}
	return stabilizeContextTextTokensWithItemBytes(pack, itemBytes)
}

func stabilizeContextTextTokensWithItemBytes(pack *ContextPack, itemBytes map[string]int) int {
	pack.Receipt.EncodedTokens = 0
	for {
		bytes := len(contextTextBlock(renderContextTextRaw(pack, false)))
		for _, item := range pack.Items {
			bytes += itemBytes[item.URI]
		}
		measured := (bytes + 3) / 4
		if measured == pack.Receipt.EncodedTokens {
			return measured
		}
		pack.Receipt.EncodedTokens = measured
	}
}

func compactContextTextFields(item ContextItem) []string {
	fields := make([]string, 0, len(item.Fields)+6)
	if item.Source != "" {
		fields = append(fields, "source="+contextTextInline(item.Source))
	}
	if item.Status != "" {
		fields = append(fields, "status="+contextTextInline(item.Status))
	}
	if len(item.Tags) > 0 {
		fields = append(fields, "tags="+contextTextInline(strings.Join(item.Tags, ",")))
	}
	for _, name := range slices.Sorted(maps.Keys(item.Fields)) {
		values := make([]string, 0, len(item.Fields[name]))
		for _, value := range item.Fields[name] {
			values = append(values, contextTextInline(value))
		}
		fields = append(fields, contextTextInline(name)+"="+strings.Join(values, ","))
	}
	if item.Pinned {
		fields = append(fields, "pinned=true")
	}
	if item.Count > 1 {
		fields = append(fields, fmt.Sprintf("count=%d", item.Count))
	}
	if len(item.Reasons) > 0 {
		reasons := make([]string, 0, len(item.Reasons))
		for _, reason := range item.Reasons {
			detail := ""
			if reason.Detail != "" {
				detail = "(" + contextTextInline(reason.Detail) + ")"
			}
			reasons = append(reasons, fmt.Sprintf("%s:%+d%s",
				contextTextInline(reason.Reason), reason.Points, detail))
		}
		fields = append(fields, "why="+strings.Join(reasons, ","))
	}
	return fields
}

func renderContextWindow(window Window) string {
	suffix := ""
	if window.DerivedFrom != "" {
		suffix = " (" + contextTextInline(window.DerivedFrom) + ")"
	}
	switch {
	case window.Since != "" && window.Until != "":
		return window.Since + ".." + window.Until + suffix
	case window.Since != "":
		return window.Since + ".." + suffix
	case window.Until != "":
		return ".." + window.Until + suffix
	default:
		return "all" + suffix
	}
}

func renderLexicalIndexUse(index LexicalIndexUse) string {
	if index.Path == "" {
		return ""
	}
	if index.Used {
		return index.Path + " used"
	}
	return index.Path + " fallback=" + contextTextOrDash(index.Reason)
}

func contextTextInline(value string) string {
	if !strings.ContainsFunc(value, contextTextBreaking) {
		return value
	}
	return strings.Map(func(char rune) rune {
		if contextTextBreaking(char) {
			return ' '
		}
		return char
	}, value)
}

func contextTextBreaking(char rune) bool {
	return char == '\n' || char == '\t' || contextTextTerminalActive(char)
}

func contextTextBlock(value string) string {
	if !strings.ContainsFunc(value, contextTextTerminalActive) {
		return value
	}
	return strings.Map(func(char rune) rune {
		if contextTextTerminalActive(char) {
			return ' '
		}
		return char
	}, value)
}

func contextTextTerminalActive(char rune) bool {
	if (char < 0x20 && char != '\n' && char != '\t') || char == 0x7f || (char >= 0x80 && char <= 0x9f) {
		return true
	}
	_, _, invisible := FindInvisible(string(char))
	return invisible
}

func contextTextOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
