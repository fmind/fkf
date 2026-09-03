package services

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
)

// queryTerms splits a query into the stable lexical terms used for every field and page.
func queryTerms(ctx context.Context, query string) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	var terms []string
	seen := map[string]struct{}{}
	for _, field := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !isTermRune(r) }) {
		minimum := 3
		if identifierShaped(field) {
			minimum = 2
		}
		if utf8.RuneCountInString(field) < minimum {
			continue
		}
		if isContextQueryScaffolding(field) {
			continue
		}
		if _, duplicate := seen[field]; duplicate {
			continue
		}
		seen[field] = struct{}{}
		terms = append(terms, field)
	}
	return terms, nil
}

// isContextQueryScaffolding removes a closed, deliberately small set of conversational
// wrappers. These words describe how the answer was requested rather than what evidence it
// should contain; letting corpus rarity score them made natural questions less precise than
// keyword queries.
func isContextQueryScaffolding(term string) bool {
	switch term {
	case "about", "can", "could", "did", "do", "does", "for", "from", "give", "how", "i", "is", "last",
		"me", "my", "our", "please", "prepare", "show", "summarize", "take", "tell", "the", "was",
		"were", "what", "when", "where", "which", "who", "why", "with", "would", "your":
		return true
	default:
		return false
	}
}

func isTermRune(r rune) bool {
	return r == '-' || r == '_' || r == '.' || r == '@' || r == '/' || r == ':' ||
		r == '#' || r == '%' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func identifierShaped(term string) bool {
	return strings.ContainsAny(term, "-/:@.")
}

func contextTermMatches(text, term string) bool {
	text, term = strings.ToLower(text), strings.ToLower(term)
	if identifierShaped(term) {
		return strings.Contains(text, term)
	}
	for _, token := range strings.FieldsFunc(text, func(r rune) bool { return !isTermRune(r) }) {
		if token == term {
			return true
		}
	}
	return false
}

// scoreCandidates applies the arithmetic. Rarity is the count of candidates divided by the
// count carrying the term, capped: a term in one item out of two hundred says far more about
// that item than a term in half of them.
func scoreCandidates(candidates []*ContextItem, query string, terms []string, now time.Time, config *core.Config) {
	frequency := contextTermFrequencies(candidates, terms)
	phrase := strings.ToLower(strings.TrimSpace(query))
	for _, candidate := range candidates {
		scoreCandidate(candidate, phrase, terms, frequency, len(candidates), now, config)
	}
}

func contextTermFrequencies(candidates []*ContextItem, terms []string) map[string]int {
	frequency := make(map[string]int, len(terms))
	for _, term := range terms {
		for _, candidate := range candidates {
			if candidateMatchesTerm(candidate, term) {
				frequency[term]++
			}
		}
	}
	return frequency
}

func scoreCandidate(
	candidate *ContextItem,
	phrase string,
	terms []string,
	frequency map[string]int,
	total int,
	now time.Time,
	config *core.Config,
) {
	candidate.explicitPolicy = contextPolicyExplicit(candidate, terms)
	if candidate.body != "" {
		candidate.Excerpt = bodyMatchExcerpt(candidate.body, terms)
	}
	if len(terms) > 1 && phrase != "" && strings.EqualFold(strings.TrimSpace(candidate.Title), phrase) {
		candidate.addReason("exact-identifier", pointsIdentifier, phrase)
		candidate.explicitIdentity = true
	}
	scoreCandidateTerms(candidate, terms, frequency, total)
	if len(terms) > 1 && contextPhraseMatches(candidate, phrase) {
		candidate.addReason("exact-phrase", pointsPhrase, phrase)
	}
	addContextRecency(candidate, now, config)
	addContextPenalties(candidate)
}

func scoreCandidateTerms(candidate *ContextItem, terms []string, frequency map[string]int, total int) {
	for _, term := range terms {
		analysis := analyzeContextTerm(candidate, term)
		if !analysis.matched {
			continue
		}
		// Exact declared identities are a different signal from lexical term frequency. A
		// common word may stop, but an explicit URI/id/title still selects what it names.
		identifierPriority := analysis.identifierPriority
		if identifierPriority > 0 {
			candidate.recordTermMatch(analysis)
			candidate.addReason("exact-identifier", pointsIdentifier, term)
			candidate.explicitIdentity = true
			candidate.matchedIdentity = true
			candidate.directIdentity = candidate.directIdentity || identifierPriority == directIdentifierPriority
			continue
		}
		rarity := rarityFactor(total, frequency[term])
		if rarity == 0 {
			continue
		}
		candidate.recordTermMatch(analysis)
		if points, detail := weightedAnalyzedTermPoints(term, rarity, analysis); points > 0 {
			candidate.addReason("term", points, detail)
		}
	}
}

func addContextRecency(candidate *ContextItem, now time.Time, config *core.Config) {
	// Recency is a modifier on relevance, never a source of it: gated on the candidate
	// having already earned a positive score from matching the query, so a merely-recent
	// but wholly unrelated record can never clear the floor on freshness alone.
	if candidate.Score <= 0 {
		return
	}
	if candidate.createdEvidence {
		candidate.addReason("created-evidence", pointsTerm, "category: created")
	}
	halfLife := 0
	if config != nil {
		if source, found := config.Sources[candidate.Source]; found {
			halfLife = source.Recency.HalfLifeDays
		}
	}
	if bonus, ageDays := recencyBonus(candidate.Date, now, halfLife); bonus > 0 {
		candidate.addReason("recency", bonus, fmt.Sprintf("%d day(s) old", ageDays))
	}
}

func addContextPenalties(candidate *ContextItem) {
	if candidate.supersededBy != "" {
		candidate.addReason("superseded", -penaltySuperseded, "superseded by "+candidate.supersededBy)
	} else if candidate.Status == "done" || candidate.Status == "deprecated" {
		candidate.addReason("superseded", -penaltySuperseded, "status: "+candidate.Status)
	}
	if candidate.Score > 0 && (candidate.URI == "wiki/index.md" || candidate.URI == "wiki/log.md") {
		candidate.addReason("navigation-page", -pointsPhrase, "curated navigation ranks below concept pages")
	}
}

func candidateMatchesTerm(candidate *ContextItem, term string) bool {
	return analyzeContextTerm(candidate, term).matched
}

type contextTermSegment struct {
	Field      string
	weight     int
	normalizer int
}

type contextTermAnalysis struct {
	matched            bool
	identifierPriority int
	maxWeight          int
	segments           []contextTermSegment
}

// analyzeContextTerm memoizes the expensive Unicode token walk shared by candidate
// admission, frequency, scoring, and tie-breaking. Candidates are immutable after this
// point, so one exact analysis preserves the scorer while avoiding four scans of large task
// and cached-body segments for every query term.
func analyzeContextTerm(candidate *ContextItem, term string) contextTermAnalysis {
	if candidate.termAnalysis != nil {
		if analysis, found := candidate.termAnalysis[term]; found {
			return analysis
		}
	} else {
		candidate.termAnalysis = make(map[string]contextTermAnalysis)
	}
	analysis := contextTermAnalysis{identifierPriority: candidateIdentifierPriority(candidate, term)}
	for _, segment := range candidate.segments {
		matched, normalizer := analyzeContextSegment(segment.Text, term)
		if !matched {
			continue
		}
		analysis.matched = true
		analysis.maxWeight = max(analysis.maxWeight, segment.Weight)
		analysis.segments = append(analysis.segments, contextTermSegment{
			Field: segment.Field, weight: segment.Weight, normalizer: normalizer,
		})
	}
	if _, found := candidate.identifierKeys[normalizeIdentityKey(term)]; found {
		analysis.matched = true
	}
	candidate.termAnalysis[term] = analysis
	return analysis
}

func analyzeContextSegment(text, term string) (bool, int) {
	text, term = strings.ToLower(text), strings.ToLower(term)
	if identifierShaped(term) && !strings.Contains(text, term) {
		return false, 0
	}
	tokens := strings.FieldsFunc(text, func(r rune) bool { return !isTermRune(r) })
	if !identifierShaped(term) && !slices.Contains(tokens, term) {
		return false, 0
	}
	return true, max(1, bits.Len(uint(max(1, len(tokens)))))
}

func contextPhraseMatches(candidate *ContextItem, phrase string) bool {
	if phrase == "" {
		return false
	}
	if _, matched := candidate.indexedPhrases[phrase]; matched {
		return true
	}
	for _, segment := range candidate.segments {
		if strings.Contains(strings.ToLower(segment.Text), phrase) {
			return true
		}
	}
	return false
}

func weightedTermPoints(candidate *ContextItem, term string, rarity int) (int, string) {
	return weightedAnalyzedTermPoints(term, rarity, analyzeContextTerm(candidate, term))
}

func weightedAnalyzedTermPoints(term string, rarity int, analysis contextTermAnalysis) (int, string) {
	bestPoints, bestField, bestWeight, bestLength := 0, "", 0, 0
	for _, segment := range analysis.segments {
		// Normalization prevents a configured weight from multiplying an unbounded blob, but
		// a unique weight-1 body match must still clear the ordinary lexical contribution.
		points := max(pointsTerm*rarity, pointsTerm*segment.weight*rarity/segment.normalizer)
		if points > bestPoints || points == bestPoints && segment.Field < bestField {
			bestPoints, bestField, bestWeight, bestLength = points, segment.Field, segment.weight, segment.normalizer
		}
	}
	if bestPoints == 0 {
		return 0, ""
	}
	return bestPoints, fmt.Sprintf("%s (%s weight %d, length %dx, rarity %dx)",
		term, bestField, bestWeight, bestLength, rarity)
}

func segmentTokenCount(text string) int {
	count := 0
	for _, token := range strings.FieldsFunc(text, func(r rune) bool { return !isTermRune(r) }) {
		if token != "" {
			count++
		}
	}
	return max(1, count)
}

// bodyMatchExcerpt returns context only when the query directly occurs in authored body text.
// Metadata-only and graph-only matches keep an empty excerpt rather than presenting unrelated
// opening prose as though it explained the match. Query order chooses among multiple matches,
// matching the deterministic page-search contract.
func bodyMatchExcerpt(body string, terms []string) string {
	for _, term := range terms {
		if excerpt := excerptAround(body, term); excerpt != "" {
			return excerpt
		}
	}
	return ""
}

// candidateNamesIdentifier confirms that an identifier-shaped term matches a field that
// actually identifies the item, not merely a substring of its prose.
func candidateNamesIdentifier(candidate *ContextItem, term string) bool {
	return candidateIdentifierPriority(candidate, term) > 0
}

const (
	noIdentifierPriority = iota
	relatedIdentifierPriority
	directIdentifierPriority
)

func candidateIdentifierPriority(candidate *ContextItem, term string) int {
	key := normalizeIdentityKey(term)
	if _, found := candidate.directIdentifiers[key]; found {
		return directIdentifierPriority
	}
	if _, found := candidate.identifierKeys[key]; found {
		return relatedIdentifierPriority
	}
	if _, found := candidate.identityTerms[key]; found {
		return relatedIdentifierPriority
	}
	return noIdentifierPriority
}

func (candidate *ContextItem) recordTermMatch(analysis contextTermAnalysis) {
	candidate.matchedTerms++
	candidate.matchWeight = max(candidate.matchWeight, analysis.maxWeight)
}

func rarityFactor(total, matching int) int {
	if matching <= 0 || total <= 0 {
		return 0
	}
	// A one-item corpus has no meaningful document-frequency stop set; zeroing its only
	// evidence would make a valid small base unretrievable.
	if total == 1 {
		return 1
	}
	if matching*2 > total {
		return 0
	}
	return min(maxRarityFactor, max(1, bits.Len(uint(total/matching))))
}

// recencyBonus rewards a DATED item for being new, decaying linearly from pointsRecencyMax
// today to 0 at DefaultContextDays old — the same horizon the default window already uses, so
// the bonus never reaches further than a query already looks by default. ageDays is returned
// even when it earns no points, so a candidate's receipt can still say how old it is.
//
// An item with no date — a wiki concept, most projects pages — gets neither the bonus nor a
// penalty. OKF is explicit that wiki/ holds what is durably true, so a concept with no shelf
// life is scored on relevance alone rather than punished for carrying no date. A date in the
// future — clock skew, a bad record — also earns nothing: it is data to distrust, not evidence
// to prefer.
func recencyBonus(date string, now time.Time, halfLifeDays int) (points, ageDays int) {
	if date == "" || halfLifeDays <= 0 {
		return 0, -1
	}
	parsed, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, -1
	}
	// Both sides parsed from a date string, the way collectionFreshness compares them, so the
	// subtraction is a whole number of days regardless of the base's timezone.
	today, err := time.Parse(time.DateOnly, now.Format(time.DateOnly))
	if err != nil {
		return 0, -1
	}
	ageDays = int(today.Sub(parsed).Hours() / 24)
	if ageDays < 0 {
		return 0, ageDays
	}
	decay := math.Pow(0.5, float64(ageDays)/float64(halfLifeDays))
	return int(math.Round(float64(pointsRecencyMax) * decay)), ageDays
}

func (i *ContextItem) addReason(reason string, points int, detail string) {
	i.Reasons = append(i.Reasons, Reason{Reason: reason, Points: points, Detail: detail})
	i.Score += points
}
