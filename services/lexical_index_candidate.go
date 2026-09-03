package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// lexicalContextCandidatePayload is the Go scorer's authored-page projection. It is cached
// beside entry metadata so a query can rank matching pages before reopening only the final
// selected Markdown. Records stay grounded by grouped reads of their durable JSON documents.
type lexicalContextCandidatePayload struct {
	Title             string                    `json:"title"`
	URL               string                    `json:"url"`
	Status            string                    `json:"status"`
	Excerpt           string                    `json:"excerpt"`
	Tags              []string                  `json:"tags"`
	Fields            map[string][]string       `json:"fields"`
	Body              string                    `json:"body"`
	Segments          []lexicalCandidateSegment `json:"segments"`
	Identifiers       []string                  `json:"identifiers"`
	DirectIdentifiers []string                  `json:"direct_identifiers"`
	IdentityTerms     []string                  `json:"identity_terms"`
	DefaultExcluded   string                    `json:"default_excluded"`
	CreatedEvidence   bool                      `json:"created_evidence"`
	ValidityRank      string                    `json:"validity_rank"`
	Supersedes        []string                  `json:"supersedes"`
}

type lexicalCandidateSegment struct {
	Field  string `json:"field"`
	Text   string `json:"text,omitempty"`
	Weight int    `json:"weight"`
	Body   bool   `json:"body,omitempty"`
}

func encodeLexicalContextCandidate(candidate *ContextItem) (string, error) {
	if candidate == nil || candidate.Kind == "record" {
		return "", errors.New("lexical authored-page candidate is missing or is a record")
	}
	payload := lexicalContextCandidatePayload{
		Title: candidate.Title, URL: candidate.URL, Status: candidate.Status, Excerpt: candidate.Excerpt,
		Tags: candidate.Tags, Fields: candidate.Fields, Body: candidate.body,
		DefaultExcluded: candidate.defaultExcluded, CreatedEvidence: candidate.createdEvidence,
		ValidityRank: candidate.validityRank, Supersedes: candidate.supersedes,
		Identifiers:       sortedLexicalCandidateSet(candidate.identifierKeys),
		DirectIdentifiers: sortedLexicalCandidateSet(candidate.directIdentifiers),
		IdentityTerms:     sortedLexicalCandidateSet(candidate.identityTerms),
		Segments:          make([]lexicalCandidateSegment, 0, len(candidate.segments)),
	}
	bodyMarked := candidate.body == ""
	for _, segment := range candidate.segments {
		encoded := lexicalCandidateSegment{Field: segment.Field, Text: segment.Text, Weight: segment.Weight}
		if !bodyMarked && segment.Field == "body" && segment.Text == strings.TrimSpace(candidate.body) {
			encoded.Text = ""
			encoded.Body = true
			bodyMarked = true
		}
		payload.Segments = append(payload.Segments, encoded)
	}
	if !bodyMarked {
		return "", errors.New("lexical authored-page body has no matching scoring segment")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode lexical authored-page candidate: %w", err)
	}
	return string(encoded), nil
}

func decodeLexicalContextCandidate(entry lexicalEntry, encoded string) (*ContextItem, error) {
	if entry.isRecord() || encoded == "" {
		return nil, errors.New("lexical authored-page entry has no candidate projection")
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var payload lexicalContextCandidatePayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode lexical authored-page candidate: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("decode lexical authored-page candidate: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode lexical authored-page candidate trailing JSON: %w", err)
	}
	if !strictlySortedLexicalValues(payload.Identifiers) ||
		!strictlySortedLexicalValues(payload.DirectIdentifiers) ||
		!strictlySortedLexicalValues(payload.IdentityTerms) {
		return nil, errors.New("lexical authored-page candidate identifiers are not canonical")
	}
	candidate := &ContextItem{
		URI: entry.URI, Kind: entry.Kind, Source: entry.Source, Date: entry.Date, Time: entry.Time,
		Title: payload.Title, URL: payload.URL, Status: payload.Status, Excerpt: payload.Excerpt,
		Tags: payload.Tags, Fields: payload.Fields, body: payload.Body,
		defaultExcluded: payload.DefaultExcluded, createdEvidence: payload.CreatedEvidence,
		validityRank: payload.ValidityRank, supersedes: payload.Supersedes,
		identifierKeys:    lexicalCandidateSet(payload.Identifiers),
		directIdentifiers: lexicalCandidateSet(payload.DirectIdentifiers),
		identityTerms:     lexicalCandidateSet(payload.IdentityTerms),
		segments:          make([]contextSegment, 0, len(payload.Segments)),
	}
	bodyMarkers := 0
	for _, segment := range payload.Segments {
		text := segment.Text
		if segment.Body {
			bodyMarkers++
			text = strings.TrimSpace(payload.Body)
		}
		if segment.Field == "" || segment.Weight < 1 || strings.TrimSpace(text) == "" ||
			strings.TrimSpace(text) != text || segment.Body && segment.Text != "" {
			return nil, errors.New("lexical authored-page candidate has an invalid scoring segment")
		}
		candidate.segments = append(candidate.segments, contextSegment{
			Field: segment.Field, Text: text, Weight: segment.Weight,
		})
	}
	if bodyMarkers > 1 || payload.Body != "" && bodyMarkers != 1 || payload.Body == "" && bodyMarkers != 0 {
		return nil, errors.New("lexical authored-page candidate has invalid body metadata")
	}
	candidate.rebuildHaystack()
	candidate.Tokens = estimateTokens(candidate, false)
	canonical, err := encodeLexicalContextCandidate(candidate)
	if err != nil || !bytes.Equal([]byte(canonical), []byte(encoded)) {
		return nil, errors.New("lexical authored-page candidate is not canonically encoded")
	}
	return candidate, nil
}

func sortedLexicalCandidateSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func lexicalCandidateSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func strictlySortedLexicalValues(values []string) bool {
	for index, value := range values {
		if value == "" || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}
