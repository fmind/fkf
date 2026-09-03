package services

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
)

const (
	LexicalIndexFallbackQueryTooShort = "query-too-short"
	maxLexicalIndexLineBytes          = 64 << 20
)

type lexicalIndexData struct {
	entries      []lexicalEntry
	postings     map[lexicalPostingKey]map[int]struct{}
	termScores   map[string]map[int]lexicalTermScore
	inputsSHA256 string
	inputs       []LexicalInputFile
	meta         LexicalIndexMeta
}

type lexicalContextPlan struct {
	entries            []lexicalEntry
	omitted            []string
	consultedBodies    []string
	pinnable           []string
	total              int
	inputsSHA256       string
	inputs             []LexicalInputFile
	unharvestedBullets int
	meta               LexicalIndexMeta
	summarized         bool
	hydrateIDs         map[int]struct{}
	supersessions      map[string]lexicalContextSupersession
}

type lexicalContextSupersession struct {
	by   string
	rank string
}

func queryContextLexicalIndex(
	ctx context.Context,
	base *Base,
	terms []string,
	pins []string,
	window Window,
	asOf string,
	query string,
	summarize bool,
) (*lexicalContextPlan, LexicalIndexUse, error) {
	keys, supported := contextLexicalKeys(terms)
	if !supported {
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackQueryTooShort}, nil
	}
	if summarize && len(terms) > 1 {
		phrase := strings.ToLower(strings.TrimSpace(query))
		if lexicalPhraseSupported(phrase) {
			keys[lexicalPostingKey{Kind: lexicalContextPhrase, Value: phrase}] = struct{}{}
		} else {
			for _, gram := range lexicalTrigrams(phrase) {
				keys[lexicalPostingKey{Kind: lexicalContextTrigram, Value: gram}] = struct{}{}
			}
		}
	}
	data, use, err := readLexicalIndexForKeys(ctx, base, keys)
	if err != nil || !use.Used {
		return nil, use, err
	}
	active, replacements, consultedBodies, err := windowedContextEntries(data.entries, window, asOf)
	if err != nil {
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackCorrupt}, nil
	}
	supersessions, err := indexedContextSupersessions(data.entries, active, replacements)
	if err != nil {
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackCorrupt}, nil
	}
	pinnedPageIDs := pinnedContextPageIDs(data.entries, active, replacements, pins)
	selected := selectContextLexicalIDs(data.postings, active, pinnedPageIDs, terms)
	entries, omitted, pinnable := partitionContextLexicalEntries(data.entries, active, replacements, selected)
	hydrateIDs := map[int]struct{}{}
	if summarize {
		if err := prepareLexicalRankCandidates(entries, data, terms, query, hydrateIDs); err != nil {
			return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackCorrupt}, nil
		}
	} else if err := loadLexicalContextCandidates(ctx, base, data.meta, entries); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, use, err
		}
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackCorrupt}, nil
	}
	return &lexicalContextPlan{
		entries: entries, omitted: omitted, consultedBodies: consultedBodies, pinnable: pinnable, total: len(active),
		inputsSHA256: data.inputsSHA256, inputs: data.inputs, unharvestedBullets: data.meta.UnharvestedBullets,
		meta: data.meta, summarized: summarize, hydrateIDs: hydrateIDs, supersessions: supersessions,
	}, use, nil
}

// indexedContextSupersessions evaluates the relation against every active authored page, not
// merely the query candidates. A replacement that does not contain the query still changes
// the target's canonical score and snapshot identity.
func indexedContextSupersessions(
	entries []lexicalEntry,
	active map[int]struct{},
	replacements map[int]lexicalEntry,
) (map[string]lexicalContextSupersession, error) {
	candidates := make([]*ContextItem, 0)
	for _, original := range entries {
		if _, found := active[original.ID]; !found {
			continue
		}
		entry := original
		if replacement, found := replacements[entry.ID]; found {
			entry = replacement
		}
		if entry.isRecord() {
			continue
		}
		candidate, err := decodeLexicalRankCandidate(entry)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	applyContextSupersedes(candidates)
	result := make(map[string]lexicalContextSupersession, len(candidates))
	for _, candidate := range candidates {
		result[candidate.URI] = lexicalContextSupersession{
			by: candidate.supersededBy, rank: candidate.supersededRank,
		}
	}
	return result, nil
}

func prepareLexicalRankCandidates(
	entries []lexicalEntry,
	data *lexicalIndexData,
	terms []string,
	query string,
	hydrate map[int]struct{},
) error {
	termIDs := make(map[string]map[int]struct{}, len(terms))
	exactTermIDs := make(map[string]map[int]struct{}, len(terms))
	for _, term := range terms {
		termIDs[term] = contextTermIDs(data.postings, term)
		exactTermIDs[term] = data.postings[lexicalPostingKey{Kind: lexicalContextToken, Value: term}]
	}
	phrase := strings.ToLower(strings.TrimSpace(query))
	phraseIDs := map[int]struct{}{}
	exactPhraseIDs := map[int]struct{}{}
	if len(terms) > 1 {
		if lexicalPhraseSupported(phrase) {
			exactPhraseIDs = data.postings[lexicalPostingKey{Kind: lexicalContextPhrase, Value: phrase}]
		} else {
			phraseIDs = contextPhraseLexicalIDs(data.postings, phrase)
		}
	}
	for index := range entries {
		entry := &entries[index]
		candidate, err := decodeLexicalRankCandidate(*entry)
		if err != nil {
			return err
		}
		candidate.termAnalysis = make(map[string]contextTermAnalysis, len(terms))
		for _, term := range terms {
			if _, matched := termIDs[term][entry.ID]; !matched {
				continue
			}
			// Identifier trigram rows are conservative. Only a T row carries a complete,
			// canonical score summary for this exact substring; hydrate every other hit.
			if identifierShaped(term) {
				if _, exact := exactTermIDs[term][entry.ID]; !exact {
					hydrate[entry.ID] = struct{}{}
					continue
				}
			}
			score, found := data.termScores[term][entry.ID]
			if !found {
				hydrate[entry.ID] = struct{}{}
				continue
			}
			if identifierShaped(term) && !lexicalTermScoreIsComplete(candidate, term, score.Analysis) {
				hydrate[entry.ID] = struct{}{}
				continue
			}
			candidate.termAnalysis[term] = score.Analysis
		}
		if _, exactPhrase := exactPhraseIDs[entry.ID]; exactPhrase {
			candidate.indexedPhrases = map[string]struct{}{phrase: {}}
		} else if _, possiblePhrase := phraseIDs[entry.ID]; possiblePhrase {
			hydrate[entry.ID] = struct{}{}
		}
		entry.candidate = candidate
	}
	return nil
}

func lexicalPhraseSupported(phrase string) bool {
	words := strings.Fields(phrase)
	return len(words) >= 2 && len(words) <= 4 && strings.Join(words, " ") == phrase &&
		slices.Equal(words, lexicalPhraseWords(phrase)) &&
		slices.ContainsFunc(words, identifierShaped)
}

func contextPhraseLexicalIDs(
	postings map[lexicalPostingKey]map[int]struct{}, phrase string,
) map[int]struct{} {
	var result map[int]struct{}
	for _, gram := range lexicalTrigrams(phrase) {
		ids := postings[lexicalPostingKey{Kind: lexicalContextTrigram, Value: gram}]
		if result == nil {
			result = cloneIDSet(ids)
		} else {
			intersectIDSets(result, ids)
		}
	}
	return result
}

func pinnedContextPageIDs(
	entries []lexicalEntry,
	active map[int]struct{},
	replacements map[int]lexicalEntry,
	pins []string,
) map[int]struct{} {
	pinned := make(map[string]struct{}, len(pins))
	for _, uri := range pins {
		pinned[uri] = struct{}{}
	}
	pinnedPageIDs := make(map[int]struct{}, len(pins))
	for _, entry := range entries {
		if _, found := active[entry.ID]; !found {
			continue
		}
		if replacement, found := replacements[entry.ID]; found {
			entry = replacement
		}
		if _, found := pinned[entry.URI]; found && !entry.isRecord() {
			pinnedPageIDs[entry.ID] = struct{}{}
		}
	}
	return pinnedPageIDs
}

func selectContextLexicalIDs(
	postings map[lexicalPostingKey]map[int]struct{},
	active map[int]struct{},
	pinnedPageIDs map[int]struct{},
	terms []string,
) map[int]struct{} {
	selected := cloneIDSet(pinnedPageIDs)
	for _, term := range terms {
		termIDs := contextTermIDs(postings, term)
		for id := range termIDs {
			if _, admitted := active[id]; admitted {
				selected[id] = struct{}{}
			}
		}
	}
	return selected
}

func partitionContextLexicalEntries(
	all []lexicalEntry,
	active map[int]struct{},
	replacements map[int]lexicalEntry,
	selected map[int]struct{},
) ([]lexicalEntry, []string, []string) {
	entries := make([]lexicalEntry, 0, len(selected))
	omitted := make([]string, 0, len(active)-len(selected))
	pinnable := make([]string, 0)
	for _, entry := range all {
		if _, admitted := active[entry.ID]; !admitted {
			continue
		}
		if replacement, found := replacements[entry.ID]; found {
			entry = replacement
		}
		if entry.Kind == string(core.LayerWiki) || entry.Kind == string(core.LayerProjects) {
			pinnable = append(pinnable, entry.URI)
		}
		if _, found := selected[entry.ID]; found {
			entries = append(entries, entry)
		} else {
			omitted = append(omitted, entry.URI)
		}
	}
	return entries, omitted, pinnable
}

// windowedContextEntries resolves identical-title runs after applying the request window. The
// cache stores each global run once, but its newest active member can differ from its newest
// member overall. Replacement copies keep the durable cache immutable across queries.
func windowedContextEntries(
	entries []lexicalEntry, window Window, asOf string,
) (map[int]struct{}, map[int]lexicalEntry, []string, error) {
	collapse, err := indexLexicalCollapseGroups(entries)
	if err != nil {
		return nil, nil, nil, err
	}
	active := make(map[int]struct{}, len(entries))
	groups := make(map[string][]lexicalEntry)
	consultedBodies := make([]string, 0)
	duplicateMembers := 0
	for _, entry := range entries {
		group, record, err := collapse.groupFor(entry)
		if err != nil {
			return nil, nil, nil, err
		}
		if group != "" {
			duplicateMembers++
		}
		if !entry.active(window, asOf) {
			continue
		}
		if entry.BodyCached {
			consultedBodies = append(consultedBodies, entry.URI)
		}
		if !record {
			if entry.Context {
				active[entry.ID] = struct{}{}
			}
			continue
		}
		if group == "" {
			active[entry.ID] = struct{}{}
			continue
		}
		groups[group] = append(groups[group], entry)
	}
	if duplicateMembers != len(collapse.byURI) {
		return nil, nil, nil, errors.New("lexical index collapse membership names a missing record")
	}
	replacements := resolveWindowedContextGroups(active, groups)
	sort.Strings(consultedBodies)
	return active, replacements, consultedBodies, nil
}

type lexicalCollapseIndex struct {
	byURI   map[string]string
	sources map[string]string
}

func indexLexicalCollapseGroups(entries []lexicalEntry) (lexicalCollapseIndex, error) {
	index := lexicalCollapseIndex{byURI: make(map[string]string), sources: make(map[string]string)}
	for _, representative := range entries {
		if !representative.isRecord() || !representative.Context || len(representative.Collapsed) == 1 {
			continue
		}
		index.sources[representative.URI] = representative.Source
		for _, uri := range representative.Collapsed {
			if _, duplicate := index.byURI[uri]; duplicate {
				return lexicalCollapseIndex{}, errors.New("lexical index repeats collapse membership")
			}
			index.byURI[uri] = representative.URI
		}
	}
	return index, nil
}

func validateLexicalCollapseEntries(entries []lexicalEntry) error {
	index, err := indexLexicalCollapseGroups(entries)
	if err != nil {
		return err
	}
	members := 0
	for _, entry := range entries {
		group, record, err := index.groupFor(entry)
		if err != nil {
			return err
		}
		if record && group != "" {
			members++
		}
	}
	if members != len(index.byURI) {
		return errors.New("lexical index collapse membership names a missing record")
	}
	return nil
}

func (index lexicalCollapseIndex) groupFor(entry lexicalEntry) (string, bool, error) {
	if !entry.isRecord() {
		return "", false, nil
	}
	if len(entry.Collapsed) == 1 {
		if !entry.Context || entry.Collapsed[0] != entry.URI {
			return "", true, errors.New("lexical index has invalid singleton collapse membership")
		}
		return "", true, nil
	}
	group := index.byURI[entry.URI]
	if group == "" || index.sources[group] != entry.Source {
		return "", true, errors.New("lexical index has invalid collapse membership")
	}
	return group, true, nil
}

func resolveWindowedContextGroups(
	active map[int]struct{}, groups map[string][]lexicalEntry,
) map[int]lexicalEntry {
	replacements := make(map[int]lexicalEntry, len(groups))
	for _, members := range groups {
		representative := windowedLexicalRepresentative(members)
		active[representative.ID] = struct{}{}
		replacements[representative.ID] = representative
	}
	return replacements
}

func windowedLexicalRepresentative(members []lexicalEntry) lexicalEntry {
	representative := members[0]
	collapsed := make([]string, 0, len(members))
	for _, member := range members {
		collapsed = append(collapsed, member.URI)
		left, right := lexicalEntryChronology(member), lexicalEntryChronology(representative)
		if left > right || left == right && member.URI < representative.URI {
			representative = member
		}
	}
	sort.Strings(collapsed)
	representative.Context = true
	representative.Collapsed = collapsed
	representative.Count = 0
	if len(collapsed) > 1 {
		representative.Count = len(collapsed)
	}
	return representative
}

func lexicalEntryChronology(entry lexicalEntry) string {
	if entry.Time != "" {
		return entry.Time
	}
	return entry.Date
}

func contextLexicalKeys(terms []string) (map[lexicalPostingKey]struct{}, bool) {
	keys := make(map[lexicalPostingKey]struct{})
	for _, term := range terms {
		keys[lexicalPostingKey{Kind: lexicalContextToken, Value: term}] = struct{}{}
		if !identifierShaped(term) {
			continue
		}
		grams := lexicalTrigrams(term)
		if len(grams) == 0 {
			return nil, false
		}
		for _, gram := range grams {
			keys[lexicalPostingKey{Kind: lexicalContextTrigram, Value: gram}] = struct{}{}
		}
	}
	return keys, true
}

func contextTermIDs(postings map[lexicalPostingKey]map[int]struct{}, term string) map[int]struct{} {
	if !identifierShaped(term) {
		return cloneIDSet(postings[lexicalPostingKey{Kind: lexicalContextToken, Value: term}])
	}
	var result map[int]struct{}
	for _, gram := range lexicalTrigrams(term) {
		ids := postings[lexicalPostingKey{Kind: lexicalContextTrigram, Value: gram}]
		if result == nil {
			result = cloneIDSet(ids)
		} else {
			intersectIDSets(result, ids)
		}
	}
	return result
}

func queryFindLexicalIndex(
	ctx context.Context, base *Base, filter FindFilter,
) (*lexicalFindPlan, LexicalIndexUse, error) {
	keys, supported := findLexicalKeys(filter.Grep, filter.Bodies)
	if !supported {
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackQueryTooShort}, nil
	}
	data, use, err := readLexicalIndexForKeys(ctx, base, keys)
	if err != nil || !use.Used {
		return nil, use, err
	}
	var selected map[int]struct{}
	for _, term := range filter.Grep {
		termIDs := findTermIDs(data.postings, term, filter.Bodies)
		if selected == nil {
			selected = termIDs
		} else {
			intersectIDSets(selected, termIDs)
		}
	}
	result := make(map[string]struct{}, len(selected))
	for _, entry := range data.entries {
		_, selectedRecord := selected[entry.ID]
		authoredPage := !entry.isRecord() && !filter.recordOnly()
		if (!selectedRecord && !authoredPage) || !lexicalEntryMatchesFind(base, entry, filter) {
			continue
		}
		result[entry.URI] = struct{}{}
	}
	return &lexicalFindPlan{
		candidates: result, inputsSHA256: data.inputsSHA256,
		inputs: append([]LexicalInputFile(nil), data.inputs...),
	}, use, nil
}

type lexicalFindPlan struct {
	candidates   map[string]struct{}
	inputsSHA256 string
	inputs       []LexicalInputFile
}

func findLexicalKeys(terms []string, bodies bool) (map[lexicalPostingKey]struct{}, bool) {
	keys := make(map[lexicalPostingKey]struct{})
	for _, term := range terms {
		if utf8.RuneCountInString(term) < 3 {
			return nil, false
		}
		for _, gram := range lexicalTrigrams(term) {
			keys[lexicalPostingKey{Kind: lexicalFindTrigram, Value: gram}] = struct{}{}
			if bodies {
				keys[lexicalPostingKey{Kind: lexicalBodyTrigram, Value: gram}] = struct{}{}
			}
		}
	}
	return keys, true
}

func findTermIDs(
	postings map[lexicalPostingKey]map[int]struct{}, term string, bodies bool,
) map[int]struct{} {
	var records map[int]struct{}
	var bodyRecords map[int]struct{}
	for _, gram := range lexicalTrigrams(term) {
		findIDs := postings[lexicalPostingKey{Kind: lexicalFindTrigram, Value: gram}]
		if records == nil {
			records = cloneIDSet(findIDs)
		} else {
			intersectIDSets(records, findIDs)
		}
		if bodies {
			bodyIDs := postings[lexicalPostingKey{Kind: lexicalBodyTrigram, Value: gram}]
			if bodyRecords == nil {
				bodyRecords = cloneIDSet(bodyIDs)
			} else {
				intersectIDSets(bodyRecords, bodyIDs)
			}
		}
	}
	for id := range bodyRecords {
		records[id] = struct{}{}
	}
	return records
}

func lexicalEntryMatchesFind(base *Base, entry lexicalEntry, filter FindFilter) bool {
	layer := core.Layer(entry.Kind)
	if !filter.wants(layer) || !base.Store.Enabled(layer) {
		return false
	}
	if filter.recordOnly() && layer != core.LayerEvents && layer != core.LayerIndex {
		return false
	}
	if len(filter.Sources) > 0 && !slices.Contains(filter.Sources, entry.Source) {
		return false
	}
	if layer == core.LayerEvents || layer == core.LayerTasks {
		return entry.Date == "" || filter.Window.Contains(entry.Date)
	}
	return true
}

func readLexicalIndexForKeys(
	ctx context.Context, base *Base, wanted map[lexicalPostingKey]struct{},
) (*lexicalIndexData, LexicalIndexUse, error) {
	meta, use, err := queryLexicalIndexMeta(ctx, base)
	if err != nil || !use.Used {
		return nil, use, err
	}
	data, err := decodeLexicalIndex(ctx, base, meta, wanted)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, use, err
		}
		return nil, LexicalIndexUse{Path: LexicalIndexPath, Reason: LexicalIndexFallbackCorrupt}, nil
	}
	return data, use, nil
}

func decodeLexicalIndex(
	ctx context.Context, base *Base, meta LexicalIndexMeta, wanted map[lexicalPostingKey]struct{},
) (*lexicalIndexData, error) {
	file, err := openLexicalIndexFile(base, meta)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	entries, err := decodeLexicalEntries(ctx, file, meta)
	if err != nil {
		return nil, err
	}
	data := &lexicalIndexData{
		entries: entries, postings: make(map[lexicalPostingKey]map[int]struct{}, len(wanted)),
		termScores:   make(map[string]map[int]lexicalTermScore),
		inputsSHA256: meta.InputsSHA256, inputs: append([]LexicalInputFile(nil), meta.Inputs...), meta: meta,
	}
	keys := make([]lexicalPostingKey, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].Kind < keys[j].Kind || keys[i].Kind == keys[j].Kind && keys[i].Value < keys[j].Value
	})
	grouped := make(map[int][]lexicalPostingKey)
	for _, key := range keys {
		shard := lexicalPostingShard(key)
		grouped[shard] = append(grouped[shard], key)
	}
	for shard := 0; shard < lexicalLookupShardCount; shard++ {
		shardKeys := grouped[shard]
		if len(shardKeys) == 0 {
			continue
		}
		lookup, err := readLexicalLookupShard(ctx, file, meta, shard)
		if err != nil {
			return nil, err
		}
		for _, key := range shardKeys {
			descriptor, found := lookup[key]
			if !found {
				continue
			}
			ids, scores, err := readLexicalPosting(ctx, file, descriptor, key, len(entries))
			if err != nil {
				return nil, err
			}
			data.postings[key] = ids
			if key.Kind == lexicalContextToken {
				data.termScores[key.Value] = scores
			}
		}
	}
	if err := lexicalIndexFileMatches(file, meta); err != nil {
		return nil, err
	}
	return data, nil
}

func openLexicalIndexFile(base *Base, meta LexicalIndexMeta) (*os.File, error) {
	path, _, err := lexicalIndexPaths(base)
	if err != nil {
		return nil, err
	}
	file, err := core.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	if err := lexicalIndexFileMatches(file, meta); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lexicalIndexFileMatches(file *os.File, meta LexicalIndexMeta) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened lexical index: %w", err)
	}
	if info.Size() != int64(meta.Bytes) {
		return errors.New("lexical index file fingerprint does not match metadata")
	}
	return nil
}

func decodeLexicalEntries(ctx context.Context, file *os.File, meta LexicalIndexMeta) ([]lexicalEntry, error) {
	if meta.PostingsOffset > 0 {
		var terminal [1]byte
		if _, err := file.ReadAt(terminal[:], meta.PostingsOffset-1); err != nil || terminal[0] != '\n' {
			return nil, errors.New("lexical index entry section is not newline terminated")
		}
	}
	reader := io.NewSectionReader(file, 0, meta.PostingsOffset)
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(&contextReader{ctx: ctx, reader: reader}, digest))
	scanner.Buffer(make([]byte, 64<<10), maxLexicalIndexLineBytes)
	entries := make([]lexicalEntry, 0, meta.Entries)
	consumed := int64(0)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != lexicalEntryRow[0] || len(line) > 1 && line[1] != '\t' {
			return nil, errors.New("lexical index entry section contains a non-entry row")
		}
		fields := strings.Split(string(line), "\t")
		entry, err := decodeLexicalEntry(fields, len(entries))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		consumed += int64(len(line) + 1)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan lexical index entries: %w", err)
	}
	if consumed != meta.PostingsOffset || len(entries) != meta.Entries ||
		hex.EncodeToString(digest.Sum(nil)) != meta.EntriesSHA256 {
		return nil, errors.New("lexical index entry rows do not match metadata")
	}
	if err := validateLexicalCollapseEntries(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

type lexicalPostingLookup struct {
	Key    lexicalPostingKey
	Offset int64
	Bytes  int
	Pairs  int
	SHA256 string
}

func readLexicalLookupShard(
	ctx context.Context, file *os.File, meta LexicalIndexMeta, shardIndex int,
) (map[lexicalPostingKey]lexicalPostingLookup, error) {
	if shardIndex < 0 || shardIndex >= len(meta.LookupShards) {
		return nil, errors.New("lexical lookup shard is outside the index")
	}
	shard := meta.LookupShards[shardIndex]
	digest := sha256.New()
	reader := io.NewSectionReader(file, shard.Offset, shard.Bytes)
	scanner := bufio.NewScanner(io.TeeReader(&contextReader{ctx: ctx, reader: reader}, digest))
	scanner.Buffer(make([]byte, 64<<10), maxLexicalIndexLineBytes)
	result := make(map[lexicalPostingKey]lexicalPostingLookup, min(shard.Rows, 4096))
	previous := lexicalPostingKey{}
	rows := 0
	for scanner.Scan() {
		descriptor, err := decodeLexicalLookupRow(scanner.Bytes(), meta, shardIndex)
		if err != nil {
			return nil, err
		}
		if previous != (lexicalPostingKey{}) && !lexicalPostingKeyLess(previous, descriptor.Key) {
			return nil, errors.New("lexical lookup rows are not in canonical order")
		}
		if _, duplicate := result[descriptor.Key]; duplicate {
			return nil, errors.New("lexical lookup repeats a posting key")
		}
		result[descriptor.Key] = descriptor
		previous = descriptor.Key
		rows++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan lexical lookup shard: %w", err)
	}
	if rows != shard.Rows || hex.EncodeToString(digest.Sum(nil)) != shard.SHA256 {
		return nil, errors.New("lexical lookup shard does not match metadata")
	}
	return result, nil
}

func decodeLexicalLookupRow(
	row []byte, meta LexicalIndexMeta, shardIndex int,
) (lexicalPostingLookup, error) {
	fields := bytes.Split(row, []byte{'\t'})
	if len(fields) != 6 || string(fields[0]) != lexicalLookupRow {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has invalid fields")
	}
	decoded := make([]byte, base64.RawURLEncoding.DecodedLen(len(fields[1])))
	written, err := base64.RawURLEncoding.Decode(decoded, fields[1])
	if err != nil || written == 0 || base64.RawURLEncoding.EncodeToString(decoded[:written]) != string(fields[1]) {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid key")
	}
	kind, value, found := bytes.Cut(decoded[:written], []byte{0})
	if !found || len(value) == 0 || !utf8.Valid(value) || bytes.Contains(value, []byte{0}) ||
		!validLexicalPostingKind(kind) {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid key")
	}
	key := lexicalPostingKey{Kind: string(kind), Value: string(value)}
	if lexicalPostingShard(key) != shardIndex {
		return lexicalPostingLookup{}, errors.New("lexical lookup row is in the wrong shard")
	}
	offset, ok := parseCanonicalLexicalInt64(fields[2])
	if !ok || offset < meta.PostingsOffset || offset >= meta.LookupOffset {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid posting offset")
	}
	rowBytes, ok := parseCanonicalLexicalInt(fields[3])
	if !ok || rowBytes < 2 || rowBytes > maxLexicalIndexLineBytes || int64(rowBytes) > meta.LookupOffset-offset {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid posting size")
	}
	pairs, ok := parseCanonicalLexicalInt(fields[4])
	if !ok || pairs < 1 {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid posting count")
	}
	sha := string(fields[5])
	if !isCanonicalSHA256(sha) {
		return lexicalPostingLookup{}, errors.New("lexical lookup row has an invalid posting digest")
	}
	return lexicalPostingLookup{Key: key, Offset: offset, Bytes: rowBytes, Pairs: pairs, SHA256: sha}, nil
}

func readLexicalPosting(
	ctx context.Context,
	file *os.File,
	descriptor lexicalPostingLookup,
	wanted lexicalPostingKey,
	entryCount int,
) (map[int]struct{}, map[int]lexicalTermScore, error) {
	row, err := readLexicalExactRow(ctx, file, descriptor.Offset, descriptor.Bytes, descriptor.SHA256)
	if err != nil {
		return nil, nil, err
	}
	key, err := lexicalPostingKeyFromRow(row)
	if err != nil || key != descriptor.Key || key != wanted {
		return nil, nil, errors.New("lexical lookup names the wrong posting row")
	}
	_, ids, scores, pairs, err := decodeLexicalPosting(row, key, entryCount, lexicalPostingKey{}, true)
	if err != nil {
		return nil, nil, err
	}
	if pairs != descriptor.Pairs {
		return nil, nil, errors.New("lexical posting count does not match its lookup")
	}
	return ids, scores, nil
}

func readLexicalExactRow(
	ctx context.Context, file *os.File, offset int64, rowBytes int, expectedSHA256 string,
) ([]byte, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	encoded := make([]byte, rowBytes)
	n, err := file.ReadAt(encoded, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read lexical row: %w", err)
	}
	if n != len(encoded) || len(encoded) < 2 || encoded[len(encoded)-1] != '\n' ||
		lexicalBytesSHA256(encoded) != expectedSHA256 {
		return nil, errors.New("lexical row does not match its authenticated lookup")
	}
	return encoded[:len(encoded)-1], nil
}

func lexicalPostingKeyLess(left, right lexicalPostingKey) bool {
	return left.Kind < right.Kind || left.Kind == right.Kind && left.Value < right.Value
}

func parseCanonicalLexicalInt(value []byte) (int, bool) {
	parsed, ok := parseLexicalEntryID(value)
	return parsed, ok && strconv.Itoa(parsed) == string(value)
}

func parseCanonicalLexicalInt64(value []byte) (int64, bool) {
	if len(value) == 0 {
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(value), 10, 64)
	return parsed, err == nil && strconv.FormatInt(parsed, 10) == string(value)
}

func decodeLexicalIndexFull(
	ctx context.Context, base *Base, meta LexicalIndexMeta, wanted map[lexicalPostingKey]struct{},
) (*lexicalIndexData, error) {
	file, err := openLexicalIndexFile(base, meta)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if err := validateLexicalOutputDigest(ctx, file, meta); err != nil {
		return nil, err
	}
	entries, err := decodeLexicalEntries(ctx, file, meta)
	if err != nil {
		return nil, err
	}
	data := &lexicalIndexData{
		entries:      entries,
		postings:     make(map[lexicalPostingKey]map[int]struct{}, len(wanted)),
		termScores:   make(map[string]map[int]lexicalTermScore),
		inputsSHA256: meta.InputsSHA256,
		inputs:       append([]LexicalInputFile(nil), meta.Inputs...),
		meta:         meta,
	}
	postingRows, postingPairs, err := scanLexicalPostings(ctx, file, meta, wanted, data.postings, data.termScores)
	if err != nil {
		return nil, err
	}
	if postingRows != meta.PostingRows || postingPairs != meta.Postings {
		return nil, errors.New("lexical posting rows do not match metadata")
	}
	if err := validateAllLexicalLookups(ctx, file, meta, len(entries)); err != nil {
		return nil, err
	}
	if err := validateAllLexicalCandidates(ctx, file, meta, entries); err != nil {
		return nil, err
	}
	if err := lexicalIndexFileMatches(file, meta); err != nil {
		return nil, err
	}
	return data, nil
}

func validateLexicalOutputDigest(ctx context.Context, file *os.File, meta LexicalIndexMeta) error {
	digest := sha256.New()
	reader := io.NewSectionReader(file, 0, int64(meta.Bytes))
	written, err := io.Copy(digest, &contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return fmt.Errorf("hash lexical index: %w", err)
	}
	if written != int64(meta.Bytes) || hex.EncodeToString(digest.Sum(nil)) != meta.OutputSHA256 {
		return errors.New("lexical index bytes do not match metadata")
	}
	return nil
}

func scanLexicalPostings(
	ctx context.Context,
	file *os.File,
	meta LexicalIndexMeta,
	wanted map[lexicalPostingKey]struct{},
	kept map[lexicalPostingKey]map[int]struct{},
	keptScores map[string]map[int]lexicalTermScore,
) (int, int, error) {
	reader := io.NewSectionReader(file, meta.PostingsOffset, meta.LookupOffset-meta.PostingsOffset)
	scanner := bufio.NewScanner(&contextReader{ctx: ctx, reader: reader})
	scanner.Buffer(make([]byte, 64<<10), maxLexicalIndexLineBytes)
	previous := lexicalPostingKey{}
	rows, pairs := 0, 0
	consumed := meta.PostingsOffset
	for scanner.Scan() {
		row := scanner.Bytes()
		key, err := lexicalPostingKeyFromRow(row)
		if err != nil {
			return 0, 0, err
		}
		_, keep := wanted[key]
		key, ids, scores, count, err := decodeLexicalPosting(row, key, meta.Entries, previous, keep)
		if err != nil {
			return 0, 0, err
		}
		if keep {
			kept[key] = ids
			if key.Kind == lexicalContextToken {
				keptScores[key.Value] = scores
			}
		}
		previous = key
		rows++
		pairs += count
		consumed += int64(len(row) + 1)
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan lexical postings: %w", err)
	}
	if consumed != meta.LookupOffset {
		return 0, 0, errors.New("lexical posting section is not newline terminated")
	}
	return rows, pairs, nil
}

func validateAllLexicalLookups(
	ctx context.Context, file *os.File, meta LexicalIndexMeta, entryCount int,
) error {
	rows, pairs := 0, 0
	for shard := range meta.LookupShards {
		lookup, err := readLexicalLookupShard(ctx, file, meta, shard)
		if err != nil {
			return err
		}
		for key, descriptor := range lookup {
			if _, _, err := readLexicalPosting(ctx, file, descriptor, key, entryCount); err != nil {
				return err
			}
			rows++
			pairs += descriptor.Pairs
		}
	}
	if rows != meta.PostingRows || pairs != meta.Postings {
		return errors.New("lexical lookup rows do not match posting metadata")
	}
	return nil
}

func validateAllLexicalCandidates(
	ctx context.Context, file *os.File, meta LexicalIndexMeta, entries []lexicalEntry,
) error {
	var relative int64
	for _, entry := range entries {
		if _, err := decodeLexicalRankCandidate(entry); err != nil {
			return fmt.Errorf("lexical entry %d rank candidate: %w", entry.ID, err)
		}
		if entry.isRecord() {
			continue
		}
		if entry.CandidateOffset != relative {
			return errors.New("lexical candidate rows are not contiguous")
		}
		if _, err := readLexicalContextCandidate(ctx, file, meta, entry); err != nil {
			return fmt.Errorf("lexical entry %d candidate: %w", entry.ID, err)
		}
		relative += int64(entry.CandidateBytes)
	}
	if relative != int64(meta.Bytes)-meta.CandidatesOffset {
		return errors.New("lexical candidate rows do not match their section")
	}
	return nil
}

func loadLexicalContextCandidates(
	ctx context.Context, base *Base, meta LexicalIndexMeta, entries []lexicalEntry,
) error {
	hasPage := false
	for _, entry := range entries {
		if !entry.isRecord() {
			hasPage = true
			break
		}
	}
	if !hasPage {
		return nil
	}
	file, err := openLexicalIndexFile(base, meta)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	for index := range entries {
		if entries[index].isRecord() {
			continue
		}
		candidate, err := readLexicalContextCandidate(ctx, file, meta, entries[index])
		if err != nil {
			return err
		}
		entries[index].candidate = candidate
	}
	return lexicalIndexFileMatches(file, meta)
}

func readLexicalContextCandidate(
	ctx context.Context, file *os.File, meta LexicalIndexMeta, entry lexicalEntry,
) (*ContextItem, error) {
	if entry.isRecord() || entry.CandidateBytes < 2 || entry.CandidateOffset < 0 ||
		int64(entry.CandidateBytes) > int64(meta.Bytes)-meta.CandidatesOffset-entry.CandidateOffset {
		return nil, errors.New("lexical candidate location is outside its section")
	}
	offset := meta.CandidatesOffset + entry.CandidateOffset
	row, err := readLexicalExactRow(ctx, file, offset, entry.CandidateBytes, entry.CandidateSHA256)
	if err != nil {
		return nil, err
	}
	fields := bytes.SplitN(row, []byte{'\t'}, 3)
	if len(fields) != 3 || string(fields[0]) != lexicalCandidateRow {
		return nil, errors.New("lexical candidate row has invalid fields")
	}
	id, ok := parseCanonicalLexicalInt(fields[1])
	if !ok || id != entry.ID || len(fields[2]) == 0 {
		return nil, errors.New("lexical candidate row names the wrong entry")
	}
	encoded := string(fields[2])
	candidate, err := decodeLexicalContextCandidate(entry, encoded)
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func decodeLexicalEntry(fields []string, expectedID int) (lexicalEntry, error) {
	if len(fields) < 16 {
		return lexicalEntry{}, errors.New("lexical entry row has too few fields")
	}
	id, err := strconv.Atoi(fields[1])
	if err != nil || id != expectedID || strconv.Itoa(id) != fields[1] {
		return lexicalEntry{}, errors.New("lexical entry IDs are not contiguous")
	}
	if !validLexicalEntryURI(fields[2]) {
		return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid URI", id)
	}
	// Only selected entries reach durable reads, where Store.Resolve rechecks publication and
	// symlink confinement. Avoiding one filesystem walk per corpus entry keeps candidate lookup
	// proportional to index bytes instead of inode count without trusting the cache for output.
	contextEntry, err := strconv.ParseBool(fields[9])
	if err != nil || strconv.FormatBool(contextEntry) != fields[9] {
		return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid context flag", id)
	}
	bodyCached, err := strconv.ParseBool(fields[10])
	if err != nil || strconv.FormatBool(bodyCached) != fields[10] {
		return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid body flag", id)
	}
	count, err := strconv.Atoi(fields[11])
	if err != nil || count < 0 || strconv.Itoa(count) != fields[11] {
		return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid count", id)
	}
	candidateOffset, candidateBytes := int64(0), 0
	if fields[12] != "" {
		candidateOffset, err = strconv.ParseInt(fields[12], 10, 64)
		if err != nil || candidateOffset < 0 || strconv.FormatInt(candidateOffset, 10) != fields[12] {
			return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid candidate offset", id)
		}
	}
	if fields[13] != "" {
		candidateBytes, err = strconv.Atoi(fields[13])
		if err != nil || candidateBytes < 0 || strconv.Itoa(candidateBytes) != fields[13] {
			return lexicalEntry{}, fmt.Errorf("lexical entry %d has invalid candidate size", id)
		}
	}
	entry := lexicalEntry{
		ID: id, URI: fields[2], Kind: fields[3], Source: fields[4], Date: fields[5], Time: fields[6],
		ValidFrom: fields[7], ValidUntil: fields[8], Context: contextEntry, BodyCached: bodyCached,
		Count: count, CandidateOffset: candidateOffset, CandidateBytes: candidateBytes,
		CandidateSHA256: fields[14], Rank: fields[15], Collapsed: append([]string(nil), fields[16:]...),
	}
	if err := validateLexicalEntry(entry); err != nil {
		return lexicalEntry{}, fmt.Errorf("lexical entry %d %w", id, err)
	}
	return entry, nil
}

func validLexicalEntryURI(uri string) bool {
	if uri == "" || strings.TrimSpace(uri) != uri {
		return false
	}
	path := uri
	if hash := strings.LastIndexByte(uri, '#'); hash >= 0 {
		if hash == len(uri)-1 || !validLexicalFragment(uri[hash+1:]) {
			return false
		}
		path = uri[:hash]
	}
	if strings.Contains(path, "?") || strings.Contains(path, "://") || strings.HasSuffix(path, "/") {
		return false
	}
	cleaned, err := core.CleanRelative(path)
	return err == nil && cleaned != "." && cleaned == path
}

func validLexicalFragment(fragment string) bool {
	for index := 0; index < len(fragment); index++ {
		char := fragment[index]
		if lexicalFragmentSafe(char) {
			continue
		}
		if char != '%' || index+2 >= len(fragment) || !upperHex(fragment[index+1]) || !upperHex(fragment[index+2]) {
			return false
		}
		decoded := hexValue(fragment[index+1])<<4 | hexValue(fragment[index+2])
		if lexicalFragmentSafe(decoded) {
			return false
		}
		index += 2
	}
	return true
}

func lexicalFragmentSafe(char byte) bool {
	if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
		return true
	}
	switch char {
	case '.', '_', ':', '/', '@', '+', '-':
		return true
	default:
		return false
	}
}

func upperHex(char byte) bool {
	return char >= '0' && char <= '9' || char >= 'A' && char <= 'F'
}

func hexValue(char byte) byte {
	if char <= '9' {
		return char - '0'
	}
	return char - 'A' + 10
}

func validateLexicalEntry(entry lexicalEntry) error {
	if !slices.Contains(core.Layers, core.Layer(entry.Kind)) {
		return fmt.Errorf("has invalid layer %q", entry.Kind)
	}
	// Collapse members are validated once as entry URIs, then
	// validateLexicalCollapseEntries proves exact membership. Re-parsing every duplicated URI
	// here doubles the dominant fresh-process validation scan without strengthening the proof.
	for _, date := range []string{entry.Date, entry.ValidFrom, entry.ValidUntil} {
		if date != "" && core.ValidateDate(date) != nil {
			return fmt.Errorf("has invalid date %q", date)
		}
	}
	if entry.Time != "" {
		parsed, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil || parsed.UTC().Format(time.RFC3339) != entry.Time {
			return fmt.Errorf("has invalid time %q", entry.Time)
		}
	}
	return validateLexicalCollapseMetadata(entry)
}

func validateLexicalCollapseMetadata(entry lexicalEntry) error {
	recordEntry := core.Layer(entry.Kind) == core.LayerEvents || core.Layer(entry.Kind) == core.LayerIndex
	if entry.Count == 1 || entry.Count > 0 && entry.Count != len(entry.Collapsed) ||
		recordEntry && entry.Context && len(entry.Collapsed) == 0 ||
		recordEntry && !entry.Context && len(entry.Collapsed) != 0 ||
		!recordEntry && (entry.Count != 0 || len(entry.Collapsed) != 0) ||
		recordEntry && (entry.CandidateOffset != 0 || entry.CandidateBytes != 0 || entry.CandidateSHA256 != "") ||
		!recordEntry && (entry.CandidateBytes < 2 || !isCanonicalSHA256(entry.CandidateSHA256)) {
		return errors.New("has invalid collapse metadata")
	}
	return nil
}

func lexicalPostingKeyFromRow(row []byte) (lexicalPostingKey, error) {
	first := bytes.IndexByte(row, '\t')
	if first != 1 || !validLexicalPostingKind(row[:first]) {
		return lexicalPostingKey{}, errors.New("lexical posting row has an invalid kind")
	}
	secondRelative := bytes.IndexByte(row[first+1:], '\t')
	if secondRelative <= 0 {
		return lexicalPostingKey{}, errors.New("lexical posting row has an invalid key or no entry IDs")
	}
	second := first + 1 + secondRelative
	value := make([]byte, base64.RawURLEncoding.DecodedLen(second-first-1))
	written, err := base64.RawURLEncoding.Decode(value, row[first+1:second])
	if err != nil || written == 0 || !utf8.Valid(value[:written]) ||
		base64.RawURLEncoding.EncodeToString(value[:written]) != string(row[first+1:second]) {
		return lexicalPostingKey{}, errors.New("lexical posting row has an invalid key")
	}
	return lexicalPostingKey{Kind: string(row[:first]), Value: string(value[:written])}, nil
}

func validLexicalPostingKind(kind []byte) bool {
	return len(kind) == 1 && (kind[0] == lexicalContextToken[0] ||
		kind[0] == lexicalContextTrigram[0] || kind[0] == lexicalContextPhrase[0] ||
		kind[0] == lexicalFindTrigram[0] ||
		kind[0] == lexicalBodyTrigram[0])
}

func decodeLexicalPosting(
	row []byte, key lexicalPostingKey, entryCount int, previous lexicalPostingKey, keep bool,
) (lexicalPostingKey, map[int]struct{}, map[int]lexicalTermScore, int, error) {
	if previous != (lexicalPostingKey{}) && (key.Kind < previous.Kind ||
		key.Kind == previous.Kind && key.Value <= previous.Value) {
		return lexicalPostingKey{}, nil, nil, 0, errors.New("lexical posting rows are not in canonical order")
	}
	second := bytes.IndexByte(row[2:], '\t') + 2
	remaining := row[second+1:]
	var ids map[int]struct{}
	var scores map[int]lexicalTermScore
	if keep {
		ids = make(map[int]struct{})
		if key.Kind == lexicalContextToken {
			scores = make(map[int]lexicalTermScore)
		}
	}
	previousID := -1
	pairs := 0
	for len(remaining) > 0 {
		field := remaining
		if separator := bytes.IndexByte(remaining, '\t'); separator >= 0 {
			field, remaining = remaining[:separator], remaining[separator+1:]
		} else {
			remaining = nil
		}
		id, valid := parseLexicalEntryID(field)
		var score lexicalTermScore
		if key.Kind == lexicalContextToken {
			var err error
			id, score, err = decodeLexicalTermScore(field, entryCount)
			valid = err == nil
		}
		if !valid || id >= entryCount || id <= previousID {
			return lexicalPostingKey{}, nil, nil, 0, errors.New("lexical posting row has invalid entry IDs")
		}
		if keep {
			ids[id] = struct{}{}
			if key.Kind == lexicalContextToken {
				scores[id] = score
			}
		}
		previousID = id
		pairs++
	}
	if pairs == 0 {
		return lexicalPostingKey{}, nil, nil, 0, errors.New("lexical posting row has no entry IDs")
	}
	return key, ids, scores, pairs, nil
}

func parseLexicalEntryID(value []byte) (int, bool) {
	if len(value) == 0 {
		return 0, false
	}
	result := 0
	maxInt := int(^uint(0) >> 1)
	for _, char := range value {
		if char < '0' || char > '9' || result > (maxInt-int(char-'0'))/10 {
			return 0, false
		}
		result = result*10 + int(char-'0')
	}
	return result, true
}

func cloneIDSet(source map[int]struct{}) map[int]struct{} {
	result := make(map[int]struct{}, len(source))
	for id := range source {
		result[id] = struct{}{}
	}
	return result
}

func intersectIDSets(left, right map[int]struct{}) {
	for id := range left {
		if _, found := right[id]; !found {
			delete(left, id)
		}
	}
}
