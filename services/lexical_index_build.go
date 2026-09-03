package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

const (
	lexicalEntryRow       = "E"
	lexicalContextToken   = "T"
	lexicalContextTrigram = "G"
	lexicalContextPhrase  = "P"
	lexicalFindTrigram    = "F"
	lexicalBodyTrigram    = "B"
	lexicalLookupRow      = "L"
	lexicalCandidateRow   = "C"
)

type lexicalEntry struct {
	ID              int
	URI             string
	Kind            string
	Source          string
	Date            string
	Time            string
	ValidFrom       string
	ValidUntil      string
	Context         bool
	BodyCached      bool
	Count           int
	CandidateOffset int64
	CandidateBytes  int
	CandidateSHA256 string
	Rank            string
	Collapsed       []string
	candidate       *ContextItem
	findTexts       []string
	cachedBody      string
}

func (entry lexicalEntry) active(window Window, asOf string) bool {
	switch core.Layer(entry.Kind) {
	case core.LayerEvents, core.LayerTasks:
		if entry.Date != "" && !window.Contains(entry.Date) {
			return false
		}
	case core.LayerWiki, core.LayerProjects:
		if entry.ValidFrom != "" && asOf < entry.ValidFrom {
			return false
		}
		if entry.ValidUntil != "" && asOf > entry.ValidUntil {
			return false
		}
	}
	return true
}

func (entry lexicalEntry) isRecord() bool {
	layer := core.Layer(entry.Kind)
	return layer == core.LayerEvents || layer == core.LayerIndex
}

type lexicalCorpus struct {
	entries        []*lexicalEntry
	contextEntries int
}

func collectLexicalCorpus(ctx context.Context, base *Base) (*lexicalCorpus, error) {
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	corpus := &lexicalCorpus{}
	byURI := map[string]*lexicalEntry{}
	records := make([]*ContextItem, 0)
	if err := collectLexicalDocuments(ctx, base, resolver, manifest, corpus, byURI, &records); err != nil {
		return nil, err
	}
	if err := collectLexicalPages(ctx, base, corpus, byURI); err != nil {
		return nil, err
	}
	candidates := make([]*ContextItem, 0, len(corpus.entries))
	for _, entry := range corpus.entries {
		candidates = append(candidates, entry.candidate)
	}
	canonicalizeContextCandidates(candidates, resolver)
	for _, candidate := range collapseContextCandidates(records) {
		entry := byURI[candidate.URI]
		if entry == nil {
			return nil, fmt.Errorf("collapsed lexical candidate %s has no entry", candidate.URI)
		}
		members := contextCollapsedURIs(candidate)
		entry.Context = true
		entry.Count = contextCollapsedCount(members)
		entry.Collapsed = members
		entry.candidate = candidate
	}
	for _, entry := range corpus.entries {
		if core.Layer(entry.Kind) == core.LayerWiki || core.Layer(entry.Kind) == core.LayerProjects ||
			core.Layer(entry.Kind) == core.LayerTasks {
			entry.Context = true
		}
		if entry.Context {
			corpus.contextEntries++
		}
	}
	sort.Slice(corpus.entries, func(i, j int) bool { return corpus.entries[i].URI < corpus.entries[j].URI })
	if len(corpus.entries) > maxLexicalIndexEntries {
		return nil, fmt.Errorf("lexical index has %d entries; maximum is %d", len(corpus.entries), maxLexicalIndexEntries)
	}
	for id, entry := range corpus.entries {
		entry.ID = id
	}
	return corpus, nil
}

func collectLexicalDocuments(
	ctx context.Context,
	base *Base,
	resolver *IdentityResolver,
	manifest *BodyManifest,
	corpus *lexicalCorpus,
	byURI map[string]*lexicalEntry,
	records *[]*ContextItem,
) error {
	uris, err := lexicalDocumentURIs(base)
	if err != nil {
		return err
	}
	for _, uri := range uris {
		if err := collectLexicalDocument(ctx, base, resolver, manifest, corpus, byURI, records, uri); err != nil {
			return err
		}
	}
	return nil
}

func lexicalDocumentURIs(base *Base) ([]string, error) {
	var uris []string
	if base.Store.Enabled(core.LayerEvents) {
		dates, err := base.EventDates()
		if err != nil {
			return nil, err
		}
		for _, date := range dates {
			names, err := base.DayDocuments(date)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				uris = append(uris, sources.EventDocumentURI(date, name))
			}
		}
	}
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			uris = append(uris, sources.IndexDocumentURI(name))
		}
	}
	return uris, nil
}

func collectLexicalDocument(
	ctx context.Context,
	base *Base,
	resolver *IdentityResolver,
	manifest *BodyManifest,
	corpus *lexicalCorpus,
	byURI map[string]*lexicalEntry,
	records *[]*ContextItem,
	uri string,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	document, err := base.ReadDocumentContext(ctx, uri)
	if err != nil {
		return err
	}
	for _, record := range document.Records {
		projected := project(document, record)
		candidate := recordCandidate(projected, contextSchemaForSource(base.Config, projected.Source))
		body, _, found, err := readCachedBodyFromManifest(ctx, base, manifest, projected.URI)
		if err != nil {
			return err
		}
		if found {
			candidate.body = body
			candidate.addSegment("body", body, core.DefaultFieldWeight)
			candidate.rebuildHaystack()
		}
		texts := lexicalRecordTexts(record, resolver)
		entry := &lexicalEntry{
			URI: projected.URI, Kind: string(document.Layer), Source: document.Source,
			Date: document.Date, Time: projected.Time, BodyCached: found,
			candidate: candidate, findTexts: texts, cachedBody: body,
		}
		if err := appendLexicalEntry(corpus, byURI, entry); err != nil {
			return err
		}
		*records = append(*records, candidate)
	}
	return nil
}

func lexicalRecordTexts(record sources.Record, resolver *IdentityResolver) []string {
	texts := make([]string, 0)
	walkScalarLeaves(map[string]any(record), func(value string) bool {
		texts = append(texts, value)
		if identity, found := resolver.Exact(value); found {
			texts = append(texts, identity.Canonical)
			texts = append(texts, identity.Aliases...)
			texts = append(texts, identity.Names...)
		}
		return true
	})
	return texts
}

func collectLexicalPages(
	ctx context.Context, base *Base, corpus *lexicalCorpus, byURI map[string]*lexicalEntry,
) error {
	for _, layer := range []core.Layer{core.LayerProjects, core.LayerWiki} {
		if !base.Store.Enabled(layer) {
			continue
		}
		pages, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return err
		}
		for _, page := range pages {
			if err := appendLexicalPage(corpus, byURI, page, string(layer), page.Date, base.Config.Schema); err != nil {
				return err
			}
		}
	}
	if !base.Store.Enabled(core.LayerTasks) {
		return nil
	}
	listing, err := ListTasks(ctx, base, Window{}, 0)
	if err != nil {
		return err
	}
	for _, trace := range listing.Traces {
		page := trace.page
		page.Date = trace.Date
		if err := appendLexicalPage(corpus, byURI, page, string(core.LayerTasks), trace.Date, base.Config.Schema); err != nil {
			return err
		}
	}
	return nil
}

func appendLexicalPage(
	corpus *lexicalCorpus, byURI map[string]*lexicalEntry, page Page, kind, date string, schema core.FieldSchema,
) error {
	candidate := pageCandidate(page, kind, schema)
	texts := []string{
		page.Title, page.Slug, page.Description, strings.Join(page.Aliases, " "),
		strings.Join(page.Tags, " "), page.Body,
	}
	entry := &lexicalEntry{
		URI: page.URI, Kind: kind, Date: date, ValidFrom: page.ValidFrom, ValidUntil: page.ValidUntil,
		candidate: candidate, findTexts: texts,
	}
	return appendLexicalEntry(corpus, byURI, entry)
}

func appendLexicalEntry(corpus *lexicalCorpus, byURI map[string]*lexicalEntry, entry *lexicalEntry) error {
	if entry == nil || entry.URI == "" || entry.candidate == nil {
		return errors.New("lexical entry is missing its URI or candidate")
	}
	if _, duplicate := byURI[entry.URI]; duplicate {
		return fmt.Errorf("lexical index entry URI %s is duplicated", entry.URI)
	}
	byURI[entry.URI] = entry
	corpus.entries = append(corpus.entries, entry)
	return nil
}

type lexicalPostingKey struct {
	Kind  string
	Value string
}

func lexicalPostingShard(key lexicalPostingKey) int {
	digest := sha256.Sum256([]byte(key.Kind + "\x00" + key.Value))
	return int(digest[0])<<4 | int(digest[1]>>4)
}

func encodeLexicalLookupKey(key lexicalPostingKey) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key.Kind + "\x00" + key.Value))
}

type lexicalIndexEncoding struct {
	Rows             []byte
	Postings         int
	PostingRows      int
	PostingsOffset   int64
	LookupOffset     int64
	CandidatesOffset int64
	EntriesSHA256    string
	LookupShards     []LexicalLookupShard
}

func encodeLexicalCorpus(corpus *lexicalCorpus) (lexicalIndexEncoding, error) {
	postings, scores := buildLexicalPostings(corpus)
	candidateRows, err := buildLexicalCandidateRows(corpus)
	if err != nil {
		return lexicalIndexEncoding{}, err
	}
	var buffer bytes.Buffer
	for _, entry := range corpus.entries {
		rank, err := encodeLexicalRankCandidate(entry.candidate)
		if err != nil {
			return lexicalIndexEncoding{}, fmt.Errorf("encode lexical rank candidate %s: %w", entry.URI, err)
		}
		entry.Rank = rank
		candidateOffset, candidateBytes := "", ""
		if entry.CandidateBytes > 0 {
			candidateOffset = strconv.FormatInt(entry.CandidateOffset, 10)
			candidateBytes = strconv.Itoa(entry.CandidateBytes)
		}
		fields := []string{
			lexicalEntryRow, strconv.Itoa(entry.ID), entry.URI, entry.Kind, entry.Source, entry.Date,
			entry.Time, entry.ValidFrom, entry.ValidUntil, strconv.FormatBool(entry.Context),
			strconv.FormatBool(entry.BodyCached), strconv.Itoa(entry.Count),
			candidateOffset, candidateBytes, entry.CandidateSHA256, entry.Rank,
		}
		fields = append(fields, entry.Collapsed...)
		if err := writeLexicalRow(&buffer, fields); err != nil {
			return lexicalIndexEncoding{}, err
		}
	}
	return encodeLexicalPostingSections(&buffer, postings, scores, candidateRows)
}

func buildLexicalPostings(corpus *lexicalCorpus) (
	map[lexicalPostingKey]map[int]struct{}, map[string]map[int]lexicalTermScore,
) {
	postings := make(map[lexicalPostingKey]map[int]struct{})
	scores := make(map[string]map[int]lexicalTermScore)
	for _, entry := range corpus.entries {
		// Find matches authored pages against durable Markdown, so page F postings would only
		// duplicate that exact scan. Context postings remain useful: they let a query defer
		// decoding and revalidating unrelated task-page projections until after selection.
		if !entry.isRecord() {
			addContextPostings(postings, entry.ID, entry.candidate)
		} else {
			addFindPostings(postings, lexicalFindTrigram, entry.ID, entry.findTexts)
			if entry.cachedBody != "" {
				addFindPostings(postings, lexicalBodyTrigram, entry.ID, []string{entry.cachedBody})
			}
			// Every raw record needs postings because the representative of an identical-title run
			// depends on the requested window. The query still admits only one representative and Go
			// still loads and scores that durable record.
			addContextPostings(postings, entry.ID, entry.candidate)
		}
		addContextPhrasePostings(postings, entry.ID, entry.candidate)
		for term, score := range lexicalTermScores(entry.candidate) {
			if scores[term] == nil {
				scores[term] = make(map[int]lexicalTermScore)
			}
			scores[term][entry.ID] = score
			addLexicalPosting(postings, lexicalContextToken, term, entry.ID)
		}
	}
	return postings, scores
}

func buildLexicalCandidateRows(corpus *lexicalCorpus) ([]byte, error) {
	var candidates bytes.Buffer
	for _, entry := range corpus.entries {
		if entry.isRecord() {
			continue
		}
		candidate, err := encodeLexicalContextCandidate(entry.candidate)
		if err != nil {
			return nil, fmt.Errorf("encode lexical candidate %s: %w", entry.URI, err)
		}
		entry.CandidateOffset = int64(candidates.Len())
		row, err := encodeLexicalCandidateRow(entry.ID, candidate)
		if err != nil {
			return nil, err
		}
		_, _ = candidates.Write(row)
		entry.CandidateBytes = len(row)
		entry.CandidateSHA256 = lexicalBytesSHA256(row)
	}
	return bytes.Clone(candidates.Bytes()), nil
}

func encodeLexicalPostingSections(
	buffer *bytes.Buffer,
	postings map[lexicalPostingKey]map[int]struct{},
	scores map[string]map[int]lexicalTermScore,
	candidates []byte,
) (lexicalIndexEncoding, error) {
	postingsOffset := int64(buffer.Len())
	entriesSHA256 := lexicalBytesSHA256(buffer.Bytes())
	keys := make([]lexicalPostingKey, 0, len(postings))
	for key := range postings {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Value < keys[j].Value
	})
	var lookup [lexicalLookupShardCount]bytes.Buffer
	lookupRows := [lexicalLookupShardCount]int{}
	pairs := 0
	for _, key := range keys {
		ids := make([]int, 0, len(postings[key]))
		for id := range postings[key] {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		fields := []string{key.Kind, base64.RawURLEncoding.EncodeToString([]byte(key.Value))}
		for _, id := range ids {
			if key.Kind == lexicalContextToken {
				// Context postings are conservative and include tokenized identifier aliases that the
				// scorer may reject. Their authenticated zero summary records that exact rejection.
				score := scores[key.Value][id]
				encoded, err := encodeLexicalTermScore(id, score)
				if err != nil {
					return lexicalIndexEncoding{}, err
				}
				fields = append(fields, encoded)
				continue
			}
			fields = append(fields, strconv.Itoa(id))
		}
		start := buffer.Len()
		if err := writeLexicalRow(buffer, fields); err != nil {
			return lexicalIndexEncoding{}, err
		}
		row := buffer.Bytes()[start:buffer.Len()]
		shard := lexicalPostingShard(key)
		lookupFields := []string{
			lexicalLookupRow, encodeLexicalLookupKey(key), strconv.Itoa(start), strconv.Itoa(len(row)),
			strconv.Itoa(len(ids)), lexicalBytesSHA256(row),
		}
		if err := writeLexicalRow(&lookup[shard], lookupFields); err != nil {
			return lexicalIndexEncoding{}, err
		}
		lookupRows[shard]++
		pairs += len(ids)
	}
	lookupOffset := int64(buffer.Len())
	shards := make([]LexicalLookupShard, lexicalLookupShardCount)
	for index := range lookup {
		data := lookup[index].Bytes()
		shards[index] = LexicalLookupShard{
			Offset: int64(buffer.Len()), Bytes: int64(len(data)), Rows: lookupRows[index],
			SHA256: lexicalBytesSHA256(data),
		}
		_, _ = buffer.Write(data)
	}
	candidatesOffset := int64(buffer.Len())
	_, _ = buffer.Write(candidates)
	if buffer.Len() > maxLexicalIndexBytes {
		return lexicalIndexEncoding{}, fmt.Errorf("lexical index is %d bytes; maximum is %d", buffer.Len(), maxLexicalIndexBytes)
	}
	return lexicalIndexEncoding{
		Rows: buffer.Bytes(), Postings: pairs, PostingRows: len(keys),
		PostingsOffset: postingsOffset, LookupOffset: lookupOffset, CandidatesOffset: candidatesOffset,
		EntriesSHA256: entriesSHA256, LookupShards: shards,
	}, nil
}

func lexicalBytesSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func encodeLexicalCandidateRow(id int, candidate string) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeLexicalRow(&buffer, []string{lexicalCandidateRow, strconv.Itoa(id), candidate}); err != nil {
		return nil, err
	}
	return bytes.Clone(buffer.Bytes()), nil
}

func writeLexicalRow(buffer *bytes.Buffer, fields []string) error {
	start := buffer.Len()
	for index, field := range fields {
		if strings.ContainsAny(field, "\t\r\n") {
			return fmt.Errorf("lexical index field %d contains a TSV separator", index)
		}
		if index > 0 {
			_ = buffer.WriteByte('\t')
		}
		_, _ = buffer.WriteString(field)
	}
	_ = buffer.WriteByte('\n')
	if buffer.Len()-start > maxLexicalIndexLineBytes {
		return fmt.Errorf("lexical index row is %d bytes; maximum is %d", buffer.Len()-start, maxLexicalIndexLineBytes)
	}
	return nil
}

func addContextPostings(postings map[lexicalPostingKey]map[int]struct{}, id int, item *ContextItem) {
	for _, segment := range item.segments {
		addContextTextPostings(postings, id, segment.Text)
	}
	// Exact URIs and identity aliases are scorer inputs even when they are not projected into
	// a displayed field. Indexing them here keeps candidate generation conservative without
	// granting the cache any scoring authority.
	for identifier := range item.identifierKeys {
		addContextTextPostings(postings, id, identifier)
	}
}

func addContextTextPostings(postings map[lexicalPostingKey]map[int]struct{}, id int, text string) {
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !isTermRune(r)
	}) {
		addLexicalPosting(postings, lexicalContextToken, token, id)
	}
	for _, gram := range lexicalTrigrams(text) {
		addLexicalPosting(postings, lexicalContextTrigram, gram, id)
	}
}

func addContextPhrasePostings(postings map[lexicalPostingKey]map[int]struct{}, id int, item *ContextItem) {
	for _, segment := range item.segments {
		lower := strings.ToLower(segment.Text)
		words := lexicalPhraseWords(lower)
		for start := range words {
			for count := 2; count <= 4 && start+count <= len(words); count++ {
				phraseWords := words[start : start+count]
				if !slices.ContainsFunc(phraseWords, identifierShaped) {
					continue
				}
				phrase := strings.Join(phraseWords, " ")
				if strings.Contains(lower, phrase) {
					addLexicalPosting(postings, lexicalContextPhrase, phrase, id)
				}
			}
		}
	}
}

func lexicalPhraseWords(value string) []string {
	words := strings.Fields(value)
	for index := range words {
		// The scorer accepts an exact phrase inside Markdown/code punctuation. Identifier
		// separators remain intact when they are inside the word, while surrounding markup is
		// removed so `fmind/fkf main` and the same plain-text query share one posting key.
		words[index] = strings.TrimFunc(words[index], func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
	}
	return words
}

func addFindPostings(
	postings map[lexicalPostingKey]map[int]struct{}, kind string, id int, texts []string,
) {
	for _, text := range texts {
		for _, gram := range lexicalTrigrams(text) {
			addLexicalPosting(postings, kind, gram, id)
		}
	}
}

func addLexicalPosting(postings map[lexicalPostingKey]map[int]struct{}, kind, value string, id int) {
	if value == "" {
		return
	}
	key := lexicalPostingKey{Kind: kind, Value: value}
	if postings[key] == nil {
		postings[key] = map[int]struct{}{}
	}
	postings[key][id] = struct{}{}
}

func lexicalTrigrams(value string) []string {
	runes := []rune(strings.ToLower(value))
	if len(runes) < 3 {
		return nil
	}
	seen := make(map[string]struct{}, len(runes)-2)
	grams := make([]string, 0, len(runes)-2)
	for index := 0; index+3 <= len(runes); index++ {
		gram := string(runes[index : index+3])
		if _, duplicate := seen[gram]; duplicate {
			continue
		}
		seen[gram] = struct{}{}
		grams = append(grams, gram)
	}
	sort.Strings(grams)
	return grams
}
