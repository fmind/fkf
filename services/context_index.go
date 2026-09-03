package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

type contextCandidateSet struct {
	candidates         []*ContextItem
	omitted            []string
	consultedBodies    []string
	frequency          map[string]int
	total              int
	pinnable           []string
	index              LexicalIndexUse
	inputsSHA256       string
	indexInputs        []LexicalInputFile
	cachedPages        map[string]lexicalCandidateProof
	rankedEntries      map[string]lexicalEntry
	summarized         bool
	unharvestedBullets *int
	indexTerms         []string
}

type lexicalCandidateProof struct {
	ID     int
	SHA256 string
}

// prepareContextCandidateSet keeps the derived file out of scoring. The index supplies only
// a conservative URI set and corpus statistics; every selected URI is loaded from durable
// evidence and matched again by the canonical Go scorer.
func prepareContextCandidateSet(
	ctx context.Context,
	base *Base,
	request ContextRequest,
	asOf string,
	terms []string,
	resolver *IdentityResolver,
) (*contextCandidateSet, error) {
	if request.indexFallback != "" {
		use := LexicalIndexUse{Path: LexicalIndexPath, Reason: request.indexFallback}
		return scanContextCandidates(ctx, base, request, asOf, terms, resolver, use)
	}
	summarize := request.DeliveryFormat == ContextDeliveryText && !request.Explain && !request.Expand &&
		request.SinceReceipt == "" && len(request.Pins) == 0
	plan, use, err := queryContextLexicalIndex(
		ctx, base, terms, request.Pins, request.Window, asOf, request.Query, summarize,
	)
	if err != nil {
		return nil, err
	}
	if use.Used {
		set, loadErr := loadIndexedContextCandidates(ctx, base, terms, request.Pins, resolver, plan, use)
		if loadErr == nil {
			return set, nil
		}
		current, err := lexicalInputsMatch(ctx, base, plan.inputs, plan.inputsSHA256)
		if err != nil {
			return nil, err
		}
		if current && !errors.Is(loadErr, errLexicalIndexCorrupt) {
			return nil, loadErr
		}
		use = LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackStale}
		if errors.Is(loadErr, errLexicalIndexCorrupt) {
			use.Reason = LexicalIndexFallbackCorrupt
		}
	}
	return scanContextCandidates(ctx, base, request, asOf, terms, resolver, use)
}

func revalidateIndexedContextPages(
	ctx context.Context,
	base *Base,
	request ContextRequest,
	asOf string,
	resolver *IdentityResolver,
	set *contextCandidateSet,
	selected []ContextItem,
) error {
	if set.inputsSHA256 == "" {
		return nil
	}
	var validationErr error
	if set.summarized {
		validationErr = revalidateSummarizedContextItems(ctx, base, request, asOf, resolver, set, selected)
	}
	for _, item := range selected {
		if validationErr != nil {
			break
		}
		proof, cached := set.cachedPages[item.URI]
		if !cached {
			continue
		}
		candidate, err := expandedCandidate(ctx, base, item.URI, request.Window, asOf)
		if err != nil {
			validationErr = err
			break
		}
		if candidate == nil {
			validationErr = fmt.Errorf("%w: selected page %s is no longer active", errLexicalIndexCorrupt, item.URI)
			break
		}
		canonicalizeContextCandidates([]*ContextItem{candidate}, resolver)
		encoded, err := encodeLexicalContextCandidate(candidate)
		row, rowErr := encodeLexicalCandidateRow(proof.ID, encoded)
		if err != nil || rowErr != nil || lexicalBytesSHA256(row) != proof.SHA256 {
			validationErr = fmt.Errorf("%w: selected page %s no longer matches its cached projection", errLexicalIndexCorrupt, item.URI)
			break
		}
	}
	current, err := lexicalInputsMatch(ctx, base, set.indexInputs, set.inputsSHA256)
	if err != nil {
		return err
	}
	if !current {
		return errLexicalIndexStale
	}
	return validationErr
}

func revalidateSummarizedContextItems(
	ctx context.Context,
	base *Base,
	request ContextRequest,
	asOf string,
	resolver *IdentityResolver,
	set *contextCandidateSet,
	selected []ContextItem,
) error {
	now, err := time.Parse(time.DateOnly, asOf)
	if err != nil {
		return err
	}
	phrase := strings.ToLower(strings.TrimSpace(request.Query))
	liveItems := make([]*ContextItem, len(selected))
	for index := range selected {
		item := selected[index]
		_, found := set.rankedEntries[item.URI]
		if !found {
			return fmt.Errorf("%w: selected summary %s has no entry proof", errLexicalIndexCorrupt, item.URI)
		}
		live, err := expandedCandidate(ctx, base, item.URI, request.Window, asOf)
		if err != nil {
			return err
		}
		if live == nil {
			return fmt.Errorf("%w: selected summary %s is no longer active", errLexicalIndexCorrupt, item.URI)
		}
		liveItems[index] = live
	}
	if _, err := attachCachedContextBodies(ctx, base, liveItems); err != nil {
		return err
	}
	canonicalizeContextCandidates(liveItems, resolver)
	for index, live := range liveItems {
		item := &selected[index]
		entry := set.rankedEntries[item.URI]
		// Bounds decide only whether the summary must be hydrated. Once this durable item is
		// loaded, canonical rescoring below is authoritative; reuse the authenticated bounds
		// while comparing the semantic rank payload instead of re-tokenizing large bodies.
		live.identifierBounds = entry.candidate.identifierBounds
		encoded, err := encodeLexicalRankCandidate(live)
		if err != nil || encoded != entry.Rank {
			return fmt.Errorf("%w: selected summary %s no longer matches durable evidence", errLexicalIndexCorrupt, item.URI)
		}
		live.Count = item.Count
		live.collapsedURIs = append([]string(nil), item.collapsedURIs...)
		live.supersededBy, live.supersededRank = item.supersededBy, item.supersededRank
		scoreCandidate(live, phrase, set.indexTerms, set.frequency, set.total, now, base.Config)
		live.Pinned = item.Pinned
		live.Tokens = contextSelectionTokens(live, request)
		reasonsChanged := request.Explain && !slices.Equal(live.Reasons, item.Reasons)
		if live.Score != item.Score || reasonsChanged ||
			live.matchedTerms != item.matchedTerms ||
			live.matchWeight != item.matchWeight || live.explicitIdentity != item.explicitIdentity ||
			live.directIdentity != item.directIdentity || live.matchedIdentity != item.matchedIdentity ||
			live.explicitPolicy != item.explicitPolicy {
			return fmt.Errorf("%w: selected summary %s changed canonical scoring", errLexicalIndexCorrupt, item.URI)
		}
		if !request.Explain {
			live.Reasons = nil
		}
		selected[index] = *live
	}
	return nil
}

func loadIndexedContextCandidates(
	ctx context.Context,
	base *Base,
	terms []string,
	pins []string,
	resolver *IdentityResolver,
	plan *lexicalContextPlan,
	use LexicalIndexUse,
) (*contextCandidateSet, error) {
	if plan.summarized {
		return loadSummarizedContextCandidates(ctx, base, terms, resolver, plan, use)
	}
	byURI := make(map[string]*ContextItem, len(plan.entries))
	cachedPages := make(map[string]lexicalCandidateProof)
	recordsByDocument := make(map[string][]lexicalEntry)
	for _, entry := range plan.entries {
		if entry.isRecord() {
			parsed, err := ParseURI(entry.URI)
			if err != nil {
				return nil, fmt.Errorf("%w: entry %s is not a record URI", errLexicalIndexCorrupt, entry.URI)
			}
			recordsByDocument[parsed.Path] = append(recordsByDocument[parsed.Path], entry)
			continue
		}
		if entry.candidate == nil {
			return nil, fmt.Errorf("%w: entry %s has no authenticated candidate", errLexicalIndexCorrupt, entry.URI)
		}
		candidate := entry.candidate
		if !lexicalCandidateMatchesEntry(candidate, entry) {
			return nil, fmt.Errorf("%w: entry %s no longer matches durable evidence", errLexicalIndexCorrupt, entry.URI)
		}
		if core.Layer(entry.Kind) == core.LayerTasks {
			candidate.Date = entry.Date
		}
		byURI[entry.URI] = candidate
		cachedPages[entry.URI] = lexicalCandidateProof{ID: entry.ID, SHA256: entry.CandidateSHA256}
	}
	records, err := loadIndexedRecordCandidates(ctx, base, recordsByDocument)
	if err != nil {
		return nil, err
	}
	if _, err := attachCachedContextBodies(ctx, base, records); err != nil {
		return nil, fmt.Errorf("read cached bodies for context: %w", err)
	}
	canonicalizeContextCandidates(records, resolver)
	for _, candidate := range records {
		byURI[candidate.URI] = candidate
	}
	candidates := make([]*ContextItem, 0, len(plan.entries))
	for _, entry := range plan.entries {
		candidate := byURI[entry.URI]
		if candidate == nil {
			return nil, fmt.Errorf("%w: entry %s produced no candidate", errLexicalIndexCorrupt, entry.URI)
		}
		candidates = append(candidates, candidate)
	}
	applyIndexedContextSupersessions(candidates, plan.supersessions)
	candidates, rejected := filterIndexedContextCandidates(candidates, terms, pins)
	plan.omitted = append(plan.omitted, rejected...)
	set := newContextCandidateSet(
		candidates, plan.omitted, plan.consultedBodies, plan.pinnable, plan.total, terms, use, plan.inputsSHA256,
	)
	set.indexInputs = append([]LexicalInputFile(nil), plan.inputs...)
	set.cachedPages = cachedPages
	backlog := plan.unharvestedBullets
	set.unharvestedBullets = &backlog
	return set, nil
}

func loadSummarizedContextCandidates(
	ctx context.Context,
	base *Base,
	terms []string,
	resolver *IdentityResolver,
	plan *lexicalContextPlan,
	use LexicalIndexUse,
) (*contextCandidateSet, error) {
	loaded := make(map[int]*ContextItem, len(plan.hydrateIDs))
	pageEntries := make([]lexicalEntry, 0, len(plan.hydrateIDs))
	recordsByDocument := make(map[string][]lexicalEntry)
	for _, entry := range plan.entries {
		if _, hydrate := plan.hydrateIDs[entry.ID]; !hydrate {
			continue
		}
		if entry.isRecord() {
			parsed, err := ParseURI(entry.URI)
			if err != nil {
				return nil, fmt.Errorf("%w: entry %s is not a record URI", errLexicalIndexCorrupt, entry.URI)
			}
			recordsByDocument[parsed.Path] = append(recordsByDocument[parsed.Path], entry)
		} else {
			pageEntries = append(pageEntries, entry)
		}
	}
	if err := loadLexicalContextCandidates(ctx, base, plan.meta, pageEntries); err != nil {
		return nil, err
	}
	for _, entry := range pageEntries {
		loaded[entry.ID] = entry.candidate
	}
	records, err := loadIndexedRecordCandidates(ctx, base, recordsByDocument)
	if err != nil {
		return nil, err
	}
	if _, err := attachCachedContextBodies(ctx, base, records); err != nil {
		return nil, fmt.Errorf("read cached bodies for context: %w", err)
	}
	canonicalizeContextCandidates(records, resolver)
	byRecordURI := make(map[string]*ContextItem, len(records))
	for _, candidate := range records {
		byRecordURI[candidate.URI] = candidate
	}
	for _, entries := range recordsByDocument {
		for _, entry := range entries {
			loaded[entry.ID] = byRecordURI[entry.URI]
		}
	}
	candidates := make([]*ContextItem, 0, len(plan.entries))
	rankedEntries := make(map[string]lexicalEntry, len(plan.entries))
	for _, entry := range plan.entries {
		candidate := entry.candidate
		if hydrated := loaded[entry.ID]; hydrated != nil {
			candidate = hydrated
		}
		if candidate == nil || !lexicalCandidateMatchesEntry(candidate, entry) {
			return nil, fmt.Errorf("%w: entry %s has an invalid rank candidate", errLexicalIndexCorrupt, entry.URI)
		}
		if core.Layer(entry.Kind) == core.LayerTasks {
			candidate.Date = entry.Date
		}
		candidate.Count = entry.Count
		candidate.collapsedURIs = append([]string(nil), entry.Collapsed...)
		candidates = append(candidates, candidate)
		rankedEntries[entry.URI] = entry
	}
	applyIndexedContextSupersessions(candidates, plan.supersessions)
	candidates, rejected := filterIndexedContextCandidates(candidates, terms, nil)
	plan.omitted = append(plan.omitted, rejected...)
	set := newContextCandidateSet(
		candidates, plan.omitted, plan.consultedBodies, plan.pinnable, plan.total, terms, use, plan.inputsSHA256,
	)
	set.indexInputs = append([]LexicalInputFile(nil), plan.inputs...)
	set.rankedEntries = rankedEntries
	set.summarized = true
	backlog := plan.unharvestedBullets
	set.unharvestedBullets = &backlog
	return set, nil
}

func applyIndexedContextSupersessions(
	candidates []*ContextItem,
	supersessions map[string]lexicalContextSupersession,
) {
	for _, candidate := range candidates {
		state := supersessions[candidate.URI]
		candidate.supersededBy = state.by
		candidate.supersededRank = state.rank
	}
}

func filterIndexedContextCandidates(
	candidates []*ContextItem,
	terms, pins []string,
) ([]*ContextItem, []string) {
	kept := make([]*ContextItem, 0, len(candidates))
	rejected := make([]string, 0)
	for _, candidate := range candidates {
		matched := slices.Contains(pins, candidate.URI) || slices.ContainsFunc(terms, func(term string) bool {
			return candidateMatchesTerm(candidate, term)
		})
		if matched {
			kept = append(kept, candidate)
		} else {
			rejected = append(rejected, candidate.URI)
		}
	}
	return kept, rejected
}

func loadIndexedRecordCandidates(
	ctx context.Context, base *Base, grouped map[string][]lexicalEntry,
) ([]*ContextItem, error) {
	paths := make([]string, 0, len(grouped))
	for path := range grouped {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	candidates := make([]*ContextItem, 0)
	for _, path := range paths {
		document, err := base.ReadDocumentContext(ctx, path)
		if err != nil {
			return nil, err
		}
		wanted := make(map[string]lexicalEntry, len(grouped[path]))
		for _, entry := range grouped[path] {
			parsed, _ := ParseURI(entry.URI)
			wanted[parsed.Fragment] = entry
		}
		found := make(map[string]struct{}, len(wanted))
		for _, record := range document.Records {
			id, ok := document.Fields.EvalString(core.FieldID, map[string]any(record))
			entry, selected := wanted[id]
			if !ok || !selected {
				continue
			}
			candidate := recordCandidate(project(document, record), contextSchemaForSource(base.Config, document.Source))
			if !lexicalCandidateMatchesEntry(candidate, entry) {
				return nil, fmt.Errorf("%w: entry %s no longer matches durable evidence", errLexicalIndexCorrupt, entry.URI)
			}
			candidate.Count = entry.Count
			candidate.collapsedURIs = append([]string(nil), entry.Collapsed...)
			candidates = append(candidates, candidate)
			found[id] = struct{}{}
		}
		if len(found) != len(wanted) {
			return nil, fmt.Errorf("%w: document %s no longer holds every indexed record", errLexicalIndexCorrupt, path)
		}
	}
	return candidates, nil
}

func lexicalCandidateMatchesEntry(candidate *ContextItem, entry lexicalEntry) bool {
	wantKind := entry.Kind
	if core.Layer(entry.Kind) == core.LayerEvents || core.Layer(entry.Kind) == core.LayerIndex {
		wantKind = "record"
	}
	return candidate.URI == entry.URI && candidate.Kind == wantKind && candidate.Source == entry.Source &&
		(core.Layer(entry.Kind) == core.LayerTasks || candidate.Date == entry.Date)
}

func scanContextCandidates(
	ctx context.Context,
	base *Base,
	request ContextRequest,
	asOf string,
	terms []string,
	resolver *IdentityResolver,
	use LexicalIndexUse,
) (*contextCandidateSet, error) {
	inputs, _, inputsSHA256, err := lexicalInputs(ctx, base, nil)
	if err != nil {
		return nil, err
	}
	candidates, err := gatherCandidates(ctx, base, request, asOf)
	if err != nil {
		return nil, err
	}
	consulted, err := attachCachedContextBodies(ctx, base, candidates)
	if err != nil {
		return nil, fmt.Errorf("read cached bodies for context: %w", err)
	}
	canonicalizeContextCandidates(candidates, resolver)
	if request.afterCandidateScan != nil {
		request.afterCandidateScan()
	}
	candidates = collapseContextCandidates(candidates)
	total := len(candidates)
	pinnable := pinnableURIs(candidates)
	relevant, omitted := partitionContextCandidates(candidates, terms, request.Pins)
	set := newContextCandidateSet(relevant, omitted, consulted, pinnable, total, terms, use, inputsSHA256)
	set.indexInputs = inputs
	return set, nil
}

func newContextCandidateSet(
	candidates []*ContextItem,
	omitted []string,
	consulted []string,
	pinnable []string,
	total int,
	terms []string,
	use LexicalIndexUse,
	inputsSHA256 string,
) *contextCandidateSet {
	candidates = collapseContextResourceDuplicates(candidates, terms)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].URI < candidates[j].URI })
	return &contextCandidateSet{
		candidates: candidates, omitted: omitted,
		consultedBodies: relevantConsultedBodies(candidates, consulted),
		frequency:       contextTermFrequencies(candidates, terms), total: total,
		pinnable: append([]string(nil), pinnable...), index: use,
		inputsSHA256: inputsSHA256, indexTerms: append([]string(nil), terms...),
	}
}

func relevantConsultedBodies(candidates []*ContextItem, consulted []string) []string {
	// Body discovery must inspect the whole active window, but the receipt should name only
	// cached bodies retained by query admission, including provenance collapsed under a result.
	relevant := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		for _, uri := range contextCollapsedURIs(candidate) {
			relevant[uri] = struct{}{}
		}
	}
	filtered := make([]string, 0, min(len(relevant), len(consulted)))
	for _, uri := range consulted {
		if _, found := relevant[uri]; found {
			filtered = append(filtered, uri)
		}
	}
	return filtered
}

func partitionContextCandidates(
	candidates []*ContextItem, terms, pins []string,
) (relevant []*ContextItem, omitted []string) {
	selected := make([]*ContextItem, 0, len(candidates))
	for _, candidate := range candidates {
		if slices.Contains(pins, candidate.URI) ||
			slices.ContainsFunc(terms, func(term string) bool { return candidateMatchesTerm(candidate, term) }) {
			selected = append(selected, candidate)
		} else {
			omitted = append(omitted, candidate.URI)
		}
	}
	return selected, omitted
}

func appendOmittedContextDrops(pack *ContextPack, omitted []string) {
	if len(omitted) == 0 {
		return
	}
	full := len(pack.Receipt.Dropped)
	if pack.Receipt.DroppedTotal > full {
		full = pack.Receipt.DroppedTotal
	}
	full += len(omitted)
	limit := droppedCap(pack.Receipt.Budget)
	for _, uri := range omitted[:min(len(omitted), max(0, limit-len(pack.Receipt.Dropped)))] {
		pack.Receipt.Dropped = append(pack.Receipt.Dropped, DroppedItem{URI: uri, Reason: "below-floor"})
	}
	sortDropped(pack.Receipt.Dropped)
	if len(pack.Receipt.Dropped) > limit {
		pack.Receipt.Dropped = pack.Receipt.Dropped[:limit]
	}
	if len(pack.Receipt.Dropped) < full {
		pack.Receipt.DroppedTotal = full
	} else {
		pack.Receipt.DroppedTotal = 0
	}
}

func scoreContextCandidateSet(
	set *contextCandidateSet, query string, terms []string, now time.Time, config *core.Config,
) {
	phrase := strings.ToLower(strings.TrimSpace(query))
	for _, candidate := range set.candidates {
		scoreCandidate(candidate, phrase, terms, set.frequency, set.total, now, config)
	}
}

func bindContextInputDigest(candidateDigest, inputsSHA256 string) string {
	digest := sha256.Sum256([]byte("fkf-context-input-v1\x00" + inputsSHA256 + "\x00" + candidateDigest))
	return hex.EncodeToString(digest[:])[:16]
}
