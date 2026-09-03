package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// lexicalRankCandidatePayload is the small projection needed to rank and render a compact
// text result. Scoring text stays in the separately authenticated term rows, while the final
// selected durable records and pages are reopened before the result is returned.
type lexicalRankCandidatePayload struct {
	Title            string                   `json:"title,omitempty"`
	URL              string                   `json:"url,omitempty"`
	Status           string                   `json:"status,omitempty"`
	Excerpt          string                   `json:"excerpt,omitempty"`
	Tags             []string                 `json:"tags,omitempty"`
	Fields           map[string][]string      `json:"fields,omitempty"`
	DefaultExcluded  string                   `json:"default_excluded,omitempty"`
	ValidityRank     string                   `json:"validity_rank,omitempty"`
	Supersedes       []string                 `json:"supersedes,omitempty"`
	CreatedEvidence  bool                     `json:"created_evidence,omitempty"`
	BodyAvailable    bool                     `json:"body_available,omitempty"`
	IdentifierBounds []lexicalIdentifierBound `json:"identifier_bounds,omitempty"`
	SemanticDigest   string                   `json:"semantic_digest"`
}

const lexicalIdentifierBloomBytes = 32

// lexicalIdentifierBound is a conservative substring proof for segments that could improve
// an identifier term's cached score. Bloom false positives hydrate durable evidence; false
// negatives are impossible because every source trigram sets all four derived bits.
type lexicalIdentifierBound struct {
	Weight int    `json:"w"`
	Points int    `json:"p"`
	Bloom  []byte `json:"b"`
}

type lexicalTermScore struct {
	Analysis     contextTermAnalysis
	ExcerptBytes int
}

func encodeLexicalRankCandidate(candidate *ContextItem) (string, error) {
	if candidate == nil {
		return "", errors.New("lexical rank candidate is missing")
	}
	semanticDigest := candidate.semanticDigest
	if semanticDigest == "" {
		semanticDigest = contextCandidateSemanticDigest(candidate)
	}
	payload := lexicalRankCandidatePayload{
		Title: candidate.Title, URL: candidate.URL, Status: candidate.Status,
		Excerpt: candidate.Excerpt, Tags: candidate.Tags, Fields: candidate.Fields,
		DefaultExcluded: candidate.defaultExcluded, ValidityRank: candidate.validityRank,
		Supersedes: candidate.supersedes, CreatedEvidence: candidate.createdEvidence,
		BodyAvailable:    candidate.body != "" || candidate.bodyAvailable,
		IdentifierBounds: lexicalCandidateIdentifierBounds(candidate),
		SemanticDigest:   semanticDigest,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode lexical rank candidate: %w", err)
	}
	return string(encoded), nil
}

func decodeLexicalRankCandidate(entry lexicalEntry) (*ContextItem, error) {
	if entry.Rank == "" {
		return nil, errors.New("lexical entry has no rank candidate")
	}
	decoder := json.NewDecoder(strings.NewReader(entry.Rank))
	decoder.DisallowUnknownFields()
	var payload lexicalRankCandidatePayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode lexical rank candidate: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("decode lexical rank candidate: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode lexical rank candidate trailing JSON: %w", err)
	}
	kind := entry.Kind
	if entry.isRecord() {
		kind = "record"
	}
	candidate := &ContextItem{
		URI: entry.URI, Kind: kind, Source: entry.Source, Date: entry.Date, Time: entry.Time,
		Title: payload.Title, URL: payload.URL, Status: payload.Status, Excerpt: payload.Excerpt,
		Tags: payload.Tags, Fields: payload.Fields, defaultExcluded: payload.DefaultExcluded,
		validityRank: payload.ValidityRank, supersedes: payload.Supersedes,
		createdEvidence: payload.CreatedEvidence, bodyAvailable: payload.BodyAvailable,
		identifierBounds: payload.IdentifierBounds,
		semanticDigest:   payload.SemanticDigest,
		Count:            entry.Count, collapsedURIs: append([]string(nil), entry.Collapsed...),
	}
	if !isCanonicalSHA256(candidate.semanticDigest) {
		return nil, errors.New("lexical rank candidate has an invalid semantic digest")
	}
	if !validLexicalIdentifierBounds(candidate.identifierBounds) {
		return nil, errors.New("lexical rank candidate has invalid identifier bounds")
	}
	canonical, err := encodeLexicalRankCandidate(candidate)
	if err != nil || !bytes.Equal([]byte(canonical), []byte(entry.Rank)) {
		return nil, errors.New("lexical rank candidate is not canonically encoded")
	}
	return candidate, nil
}

func lexicalCandidateIdentifierBounds(candidate *ContextItem) []lexicalIdentifierBound {
	if candidate.identifierBounds != nil {
		return candidate.identifierBounds
	}
	type boundKey struct {
		weight int
		points int
	}
	groups := make(map[boundKey][]byte)
	for _, segment := range candidate.segments {
		grams := lexicalTrigrams(segment.Text)
		if len(grams) == 0 {
			continue
		}
		normalizer := max(1, bits.Len(uint(segmentTokenCount(segment.Text))))
		key := boundKey{weight: segment.Weight, points: lexicalTermSegmentPoints(segment.Weight, normalizer)}
		bloom := groups[key]
		if bloom == nil {
			bloom = make([]byte, lexicalIdentifierBloomBytes)
		}
		for _, gram := range grams {
			addLexicalIdentifierBloom(bloom, gram)
		}
		groups[key] = bloom
	}
	keys := make([]boundKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].weight < keys[j].weight || keys[i].weight == keys[j].weight && keys[i].points < keys[j].points
	})
	bounds := make([]lexicalIdentifierBound, 0, len(keys))
	for _, key := range keys {
		bounds = append(bounds, lexicalIdentifierBound{Weight: key.weight, Points: key.points, Bloom: groups[key]})
	}
	return bounds
}

func validLexicalIdentifierBounds(bounds []lexicalIdentifierBound) bool {
	previousWeight, previousPoints := -1, -1
	for _, bound := range bounds {
		ordered := bound.Weight > previousWeight || bound.Weight == previousWeight && bound.Points > previousPoints
		if !ordered || bound.Weight <= 0 || bound.Points < pointsTerm || len(bound.Bloom) != lexicalIdentifierBloomBytes ||
			!slices.ContainsFunc(bound.Bloom, func(value byte) bool { return value != 0 }) {
			return false
		}
		previousWeight, previousPoints = bound.Weight, bound.Points
	}
	return true
}

func addLexicalIdentifierBloom(bloom []byte, gram string) {
	digest := sha256.Sum256([]byte(gram))
	for _, position := range []uint16{
		uint16(digest[0])<<8 | uint16(digest[1]),
		uint16(digest[2])<<8 | uint16(digest[3]),
		uint16(digest[4])<<8 | uint16(digest[5]),
		uint16(digest[6])<<8 | uint16(digest[7]),
	} {
		bit := int(position) % (len(bloom) * 8)
		bloom[bit/8] |= 1 << (bit % 8)
	}
}

func lexicalIdentifierBloomContains(bloom []byte, gram string) bool {
	digest := sha256.Sum256([]byte(gram))
	for _, position := range []uint16{
		uint16(digest[0])<<8 | uint16(digest[1]),
		uint16(digest[2])<<8 | uint16(digest[3]),
		uint16(digest[4])<<8 | uint16(digest[5]),
		uint16(digest[6])<<8 | uint16(digest[7]),
	} {
		bit := int(position) % (len(bloom) * 8)
		if bloom[bit/8]&(1<<(bit%8)) == 0 {
			return false
		}
	}
	return true
}

func lexicalTermScores(candidate *ContextItem) map[string]lexicalTermScore {
	scores := map[string]lexicalTermScore{}
	for _, segment := range candidate.segments {
		tokens := strings.FieldsFunc(strings.ToLower(segment.Text), func(r rune) bool {
			return !isTermRune(r)
		})
		normalizer := max(1, bits.Len(uint(max(1, len(tokens)))))
		seen := map[string]struct{}{}
		for _, token := range tokens {
			seen[token] = struct{}{}
			for _, subterm := range lexicalIdentifierSubterms(token) {
				seen[subterm] = struct{}{}
			}
		}
		for term := range seen {
			score := scores[term]
			score.Analysis.matched = true
			score.Analysis.maxWeight = max(score.Analysis.maxWeight, segment.Weight)
			candidateSegment := contextTermSegment{
				Field: segment.Field, weight: segment.Weight, normalizer: normalizer,
			}
			if len(score.Analysis.segments) == 0 || lexicalTermSegmentPrecedes(candidateSegment, score.Analysis.segments[0]) {
				score.Analysis.segments = []contextTermSegment{candidateSegment}
			}
			scores[term] = score
		}
	}
	for identifier := range candidate.identityTerms {
		score := scores[identifier]
		score.Analysis.matched = true
		score.Analysis.identifierPriority = max(score.Analysis.identifierPriority, relatedIdentifierPriority)
		scores[identifier] = score
	}
	for identifier := range candidate.identifierKeys {
		score := scores[identifier]
		score.Analysis.matched = true
		priority := relatedIdentifierPriority
		if _, direct := candidate.directIdentifiers[identifier]; direct {
			priority = directIdentifierPriority
		}
		score.Analysis.identifierPriority = max(score.Analysis.identifierPriority, priority)
		scores[identifier] = score
	}
	return scores
}

func lexicalTermSegmentPrecedes(left, right contextTermSegment) bool {
	leftPoints := lexicalTermSegmentPoints(left.weight, left.normalizer)
	rightPoints := lexicalTermSegmentPoints(right.weight, right.normalizer)
	return leftPoints > rightPoints || leftPoints == rightPoints && left.Field < right.Field
}

func lexicalTermSegmentPoints(weight, normalizer int) int {
	return max(pointsTerm, pointsTerm*weight/max(1, normalizer))
}

func lexicalTermScoreIsComplete(candidate *ContextItem, term string, analysis contextTermAnalysis) bool {
	bestPoints := 0
	if len(analysis.segments) > 0 {
		segment := analysis.segments[0]
		bestPoints = lexicalTermSegmentPoints(segment.weight, segment.normalizer)
	}
	grams := lexicalTrigrams(term)
	for _, bound := range candidate.identifierBounds {
		if bound.Weight <= analysis.maxWeight && bound.Points <= bestPoints {
			continue
		}
		if grams == nil {
			return false
		}
		possible := true
		for _, gram := range grams {
			if !lexicalIdentifierBloomContains(bound.Bloom, gram) {
				possible = false
				break
			}
		}
		if possible {
			return false
		}
	}
	return true
}

// lexicalIdentifierSubterms covers the path-like suffixes people actually query (for example
// fmind/fkf inside github.com/fmind/fkf). Any unusual substring remains a conservative trigram
// candidate and makes that query use the durable projection path.
func lexicalIdentifierSubterms(value string) []string {
	value = strings.ToLower(value)
	if !identifierShaped(value) {
		return nil
	}
	runes := []rune(value)
	starts := []int{0}
	ends := []int{len(runes)}
	for index, char := range runes {
		if !strings.ContainsRune("/:@#%", char) {
			continue
		}
		if index > 0 {
			ends = append(ends, index)
		}
		if index+1 < len(runes) {
			starts = append(starts, index+1)
		}
	}
	seen := map[string]struct{}{}
	for _, start := range starts {
		for _, end := range ends {
			if end <= start {
				continue
			}
			candidate := string(runes[start:end])
			if utf8.RuneCountInString(candidate) >= 2 && strings.ContainsRune(candidate, '/') {
				seen[candidate] = struct{}{}
			}
		}
	}
	result := mapsKeys(seen)
	sort.Strings(result)
	return result
}

func mapsKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}

func encodeLexicalTermScore(id int, score lexicalTermScore) (string, error) {
	segment := contextTermSegment{}
	if len(score.Analysis.segments) > 0 {
		segment = score.Analysis.segments[0]
	}
	if id < 0 || score.Analysis.identifierPriority < 0 || score.Analysis.identifierPriority > directIdentifierPriority ||
		score.Analysis.maxWeight < 0 || segment.weight < 0 || segment.normalizer < 0 || score.ExcerptBytes < 0 {
		return "", errors.New("lexical term score has invalid values")
	}
	field := base64.RawURLEncoding.EncodeToString([]byte(segment.Field))
	matched := "0"
	if score.Analysis.matched {
		matched = "1"
	}
	return strings.Join([]string{
		strconv.Itoa(id), matched, strconv.Itoa(score.Analysis.identifierPriority),
		strconv.Itoa(score.Analysis.maxWeight), strconv.Itoa(segment.weight),
		strconv.Itoa(segment.normalizer), field, strconv.Itoa(score.ExcerptBytes),
	}, ","), nil
}

func decodeLexicalTermScore(field []byte, entryCount int) (int, lexicalTermScore, error) {
	parts := bytes.Split(field, []byte{','})
	if len(parts) != 8 || string(parts[1]) != "0" && string(parts[1]) != "1" {
		return 0, lexicalTermScore{}, errors.New("lexical term score has invalid fields")
	}
	values := make([]int, 6)
	for index, part := range [][]byte{parts[0], parts[2], parts[3], parts[4], parts[5], parts[7]} {
		value, ok := parseCanonicalLexicalInt(part)
		if !ok {
			return 0, lexicalTermScore{}, errors.New("lexical term score has invalid integers")
		}
		values[index] = value
	}
	if values[0] >= entryCount || values[1] > directIdentifierPriority {
		return 0, lexicalTermScore{}, errors.New("lexical term score is outside its bounds")
	}
	decoded := make([]byte, base64.RawURLEncoding.DecodedLen(len(parts[6])))
	written, err := base64.RawURLEncoding.Decode(decoded, parts[6])
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded[:written]) != string(parts[6]) ||
		!utf8.Valid(decoded[:written]) {
		return 0, lexicalTermScore{}, errors.New("lexical term score has an invalid field")
	}
	analysis := contextTermAnalysis{
		matched: string(parts[1]) == "1", identifierPriority: values[1], maxWeight: values[2],
	}
	if values[3] > 0 || values[4] > 0 || written > 0 {
		if values[3] < 1 || values[4] < 1 || written == 0 {
			return 0, lexicalTermScore{}, errors.New("lexical term score has incomplete segment metadata")
		}
		analysis.segments = []contextTermSegment{{
			Field: string(decoded[:written]), weight: values[3], normalizer: values[4],
		}}
	}
	if !analysis.matched && (analysis.identifierPriority != 0 || analysis.maxWeight != 0 ||
		len(analysis.segments) != 0 || values[5] != 0) {
		return 0, lexicalTermScore{}, errors.New("unmatched lexical term score has scoring metadata")
	}
	return values[0], lexicalTermScore{Analysis: analysis, ExcerptBytes: values[5]}, nil
}
