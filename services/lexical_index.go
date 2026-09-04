package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// FKF deliberately uses a plain postings TSV instead of SQLite FTS5 here. The SQLite option
// would add a large pure-Go driver and its transitive notice surface to the one shipped binary;
// the sorted, rebuildable TSV needs only the standard library and stays inspectable with cut,
// sort, and awk. It is a candidate generator and term-statistics cache only: Go still loads the
// selected evidence and applies the canonical scorer.
const (
	LexicalIndexPath     = "index/.fkf-index.tsv"
	lexicalIndexMetaPath = "index/.fkf-index.meta.json"

	LexicalIndexFallbackMissing = "missing"
	LexicalIndexFallbackStale   = "stale"
	LexicalIndexFallbackCorrupt = "corrupt"

	lexicalIndexSchemaVersion    = 2
	lexicalIndexExtractorVersion = 11
	lexicalIndexFormat           = "postings-tsv-v1"
	maxLexicalIndexBytes         = 512 << 20
	maxLexicalIndexEntries       = 1_000_000
	lexicalLookupShardCount      = 4096
	minLexicalLookupRowBytes     = 78
)

var (
	errLexicalIndexStale   = errors.New("lexical index is stale")
	errLexicalIndexCorrupt = errors.New("lexical index is corrupt")
	// errLexicalInputAbsent marks an input the body manifest names but whose file is gone. That
	// cache is ignored, rebuildable, and never evidence, so a dangling entry must drop out of the
	// input set — leaving the stored generation stale — instead of failing every walk that sees it.
	errLexicalInputAbsent = errors.New("lexical input is absent")
)

// LexicalIndexUse is the explicit execution-path diagnostic carried by retrieval receipts.
// It is not a semantic ranking input: indexed and fallback scans must return the same answer.
type LexicalIndexUse struct {
	Path   string `json:"path"`
	Used   bool   `json:"used,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// MarshalJSON keeps the always-present context receipt compact enough for small honest
// budgets while still naming both the derived path and its fallback reason.
func (use LexicalIndexUse) MarshalJSON() ([]byte, error) {
	value := use.Path
	if use.Used {
		value += " (used)"
	} else if use.Reason != "" {
		value += " (" + use.Reason + ")"
	}
	return json.Marshal(value)
}

// UnmarshalJSON is the inverse used by CLI and MCP contract tests that decode public output.
func (use *LexicalIndexUse) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode lexical index use: %w", err)
	}
	*use = LexicalIndexUse{}
	if value == "" {
		return nil
	}
	path, state, found := strings.Cut(value, " (")
	if !found || !strings.HasSuffix(state, ")") {
		return fmt.Errorf("decode lexical index use %q: expected '<path> (<state>)'", value)
	}
	use.Path = path
	state = strings.TrimSuffix(state, ")")
	if state == "used" {
		use.Used = true
	} else {
		use.Reason = state
	}
	return nil
}

// LexicalIndexBuild reports one complete deterministic cache replacement.
type LexicalIndexBuild struct {
	URI            string           `json:"uri"`
	MetaURI        string           `json:"meta_uri"`
	Entries        int              `json:"entries"`
	ContextEntries int              `json:"context_entries"`
	Postings       int              `json:"postings"`
	Bytes          int              `json:"bytes"`
	Mode           string           `json:"mode"`
	Elapsed        string           `json:"elapsed"`
	Meta           LexicalIndexMeta `json:"meta"`
	Stale          bool             `json:"stale,omitempty"`
}

// LexicalInputFile binds one exact searchable file and supports stat-first freshness checks.
// The builder still hashes every input twice around extraction before publishing a generation.
type LexicalInputFile struct {
	Path             string `json:"path"`
	Bytes            int64  `json:"bytes"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
	SHA256           string `json:"sha256"`
}

// LexicalIndexMeta binds the TSV bytes to every searchable input and ranking semantic.
type LexicalIndexMeta struct {
	SchemaVersion      int                  `json:"schema_version"`
	ExtractorVersion   int                  `json:"extractor_version"`
	Format             string               `json:"format"`
	GeneratedAt        string               `json:"generated_at"`
	Entries            int                  `json:"entries"`
	ContextEntries     int                  `json:"context_entries"`
	Postings           int                  `json:"postings"`
	PostingRows        int                  `json:"posting_rows"`
	Bytes              int                  `json:"bytes"`
	PostingsOffset     int64                `json:"postings_offset"`
	LookupOffset       int64                `json:"lookup_offset"`
	CandidatesOffset   int64                `json:"candidates_offset"`
	EntriesSHA256      string               `json:"entries_sha256"`
	LookupShards       []LexicalLookupShard `json:"lookup_shards"`
	InputsSHA256       string               `json:"inputs_sha256"`
	SemanticsSHA256    string               `json:"semantics_sha256"`
	OutputSHA256       string               `json:"output_sha256"`
	UnharvestedBullets int                  `json:"unharvested_bullets"`
	Inputs             []LexicalInputFile   `json:"inputs"`
}

// LexicalLookupShard authenticates one hash partition of the compact key-to-row directory.
// Reading the complete named partition proves both inclusion and absence for any requested key.
type LexicalLookupShard struct {
	Offset int64  `json:"offset"`
	Bytes  int64  `json:"bytes"`
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

// BuildLexicalIndex rescans searchable local evidence and atomically publishes rows before
// metadata. A reader in the two-rename window rejects the mismatched output digest and scans.
func BuildLexicalIndex(ctx context.Context, base *Base) (*LexicalIndexBuild, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	started := base.Now()
	_, semantics, aggregate, err := lexicalInputs(ctx, base, nil)
	if err != nil {
		return nil, err
	}
	corpus, err := collectLexicalCorpus(ctx, base)
	if err != nil {
		return nil, err
	}
	unharvestedBullets, err := lexicalUnharvestedBullets(ctx, base)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeLexicalCorpus(corpus)
	if err != nil {
		return nil, err
	}
	// The second pass is a build-generation guard, not an ordinary freshness check. Hash every
	// input again so a same-stat rewrite during a long build cannot publish mixed generations.
	confirmedInputs, confirmedSemantics, confirmedAggregate, err := lexicalInputs(ctx, base, nil)
	if err != nil {
		return nil, err
	}
	if aggregate != confirmedAggregate || semantics != confirmedSemantics {
		return nil, errors.New("lexical index inputs changed while the cache was being built; retry")
	}
	outputDigest := sha256.Sum256(encoded.Rows)
	meta := LexicalIndexMeta{
		SchemaVersion: lexicalIndexSchemaVersion, ExtractorVersion: lexicalIndexExtractorVersion,
		Format: lexicalIndexFormat, GeneratedAt: started.UTC().Format(time.RFC3339),
		Entries: len(corpus.entries), ContextEntries: corpus.contextEntries,
		Postings: encoded.Postings, PostingRows: encoded.PostingRows, Bytes: len(encoded.Rows),
		PostingsOffset: encoded.PostingsOffset, LookupOffset: encoded.LookupOffset,
		CandidatesOffset: encoded.CandidatesOffset, EntriesSHA256: encoded.EntriesSHA256,
		LookupShards: encoded.LookupShards, InputsSHA256: aggregate,
		SemanticsSHA256: semantics, OutputSHA256: hex.EncodeToString(outputDigest[:]),
		UnharvestedBullets: unharvestedBullets, Inputs: confirmedInputs,
	}
	meta, err = writeLexicalIndex(base, encoded.Rows, meta)
	if err != nil {
		return nil, err
	}
	return &LexicalIndexBuild{
		URI: LexicalIndexPath, MetaURI: lexicalIndexMetaPath,
		Entries: len(corpus.entries), ContextEntries: corpus.contextEntries,
		Postings: encoded.Postings, Bytes: len(encoded.Rows), Mode: "full",
		Elapsed: base.Now().Sub(started).Round(time.Millisecond).String(), Meta: meta,
	}, nil
}

func lexicalUnharvestedBullets(ctx context.Context, base *Base) (int, error) {
	if !base.Store.Enabled(core.LayerTasks) {
		return 0, nil
	}
	listing, err := ListLearned(ctx, base, Window{}, true)
	if err != nil {
		return 0, fmt.Errorf("count lexical-index learned backlog: %w", err)
	}
	return len(listing.Bullets), nil
}

func writeLexicalIndex(base *Base, encoded []byte, meta LexicalIndexMeta) (LexicalIndexMeta, error) {
	rows, sidecar, err := lexicalIndexPaths(base)
	if err != nil {
		return LexicalIndexMeta{}, err
	}
	if err := core.WriteFileAtomicMode(rows, encoded, core.BaseFileMode); err != nil {
		return LexicalIndexMeta{}, fmt.Errorf("write %s: %w", LexicalIndexPath, err)
	}
	info, err := os.Stat(rows)
	if err != nil {
		return LexicalIndexMeta{}, fmt.Errorf("inspect written %s: %w", LexicalIndexPath, err)
	}
	if info.Size() != int64(len(encoded)) {
		return LexicalIndexMeta{}, fmt.Errorf("written %s is %d bytes; want %d", LexicalIndexPath, info.Size(), len(encoded))
	}
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return LexicalIndexMeta{}, fmt.Errorf("encode lexical index metadata: %w", err)
	}
	if err := core.WriteFileAtomicMode(sidecar, append(metadata, '\n'), core.BaseFileMode); err != nil {
		return LexicalIndexMeta{}, fmt.Errorf("write %s: %w", lexicalIndexMetaPath, err)
	}
	return meta, nil
}

// LexicalIndexStatus validates the current generation without exposing cache corruption as a
// retrieval failure. Only failures in the source-of-truth inputs or cancellation are returned.
func LexicalIndexStatus(ctx context.Context, base *Base) (LexicalIndexUse, error) {
	meta, use, err := currentLexicalIndexMeta(ctx, base)
	if err != nil || !use.Used {
		return use, err
	}
	_, err = decodeLexicalIndexFull(ctx, base, meta, nil)
	if errors.Is(err, os.ErrNotExist) {
		use.Used = false
		use.Reason = LexicalIndexFallbackMissing
		return use, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return use, err
		}
		use.Used = false
		use.Reason = LexicalIndexFallbackCorrupt
		return use, nil
	}
	return use, nil
}

// lexicalIndexHealth classifies the current generation from the sidecar, the source-of-truth
// inputs, and one stat of the postings artifact. `fkf status` reports from here so the routine
// diagnostic stays proportional to the base: hashing and decoding the whole artifact belongs to
// the explicit `fkf build index --check` path, and byte corruption under an unchanged size is
// caught at query time by the per-shard digests, which is what makes retrieval fall back.
func lexicalIndexHealth(ctx context.Context, base *Base) (LexicalIndexUse, error) {
	meta, use, err := currentLexicalIndexMeta(ctx, base)
	if err != nil || !use.Used {
		return use, err
	}
	file, err := openLexicalIndexFile(base, meta)
	if err != nil {
		use.Used = false
		use.Reason = LexicalIndexFallbackCorrupt
		if errors.Is(err, os.ErrNotExist) {
			use.Reason = LexicalIndexFallbackMissing
		}
		return use, nil
	}
	_ = file.Close()
	return use, nil
}

// currentLexicalIndexMeta validates the sidecar and the source-of-truth inputs without reading
// the postings file. A query then authenticates the complete entry prefix and each requested
// lookup partition and posting row; `build index --check` hashes and validates the full artifact.
func currentLexicalIndexMeta(
	ctx context.Context, base *Base,
) (LexicalIndexMeta, LexicalIndexUse, error) {
	use := LexicalIndexUse{Path: LexicalIndexPath}
	meta, err := readLexicalIndexMeta(ctx, base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			use.Reason = LexicalIndexFallbackMissing
			return LexicalIndexMeta{}, use, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return LexicalIndexMeta{}, use, err
		}
		use.Reason = LexicalIndexFallbackCorrupt
		return LexicalIndexMeta{}, use, nil
	}
	if err := validateLexicalIndexMeta(meta); err != nil {
		if errors.Is(err, errLexicalIndexStale) {
			use.Reason = LexicalIndexFallbackStale
		} else {
			use.Reason = LexicalIndexFallbackCorrupt
		}
		return LexicalIndexMeta{}, use, nil
	}
	_, semantics, aggregate, err := lexicalInputs(ctx, base, meta.Inputs)
	if err != nil {
		return LexicalIndexMeta{}, use, err
	}
	if semantics != meta.SemanticsSHA256 || aggregate != meta.InputsSHA256 {
		use.Reason = LexicalIndexFallbackStale
		return LexicalIndexMeta{}, use, nil
	}
	use.Used = true
	return meta, use, nil
}

// queryLexicalIndexMeta defers the corpus stat walk until the query has a complete candidate
// result. Context and find both recheck the full generation before returning; doing the same
// walk here as well doubled fresh-process inode work without closing another trust boundary.
func queryLexicalIndexMeta(
	ctx context.Context, base *Base,
) (LexicalIndexMeta, LexicalIndexUse, error) {
	use := LexicalIndexUse{Path: LexicalIndexPath}
	meta, err := readLexicalIndexMeta(ctx, base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			use.Reason = LexicalIndexFallbackMissing
			return LexicalIndexMeta{}, use, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return LexicalIndexMeta{}, use, err
		}
		use.Reason = LexicalIndexFallbackCorrupt
		return LexicalIndexMeta{}, use, nil
	}
	if err := validateLexicalIndexMeta(meta); err != nil {
		if errors.Is(err, errLexicalIndexStale) {
			use.Reason = LexicalIndexFallbackStale
		} else {
			use.Reason = LexicalIndexFallbackCorrupt
		}
		return LexicalIndexMeta{}, use, nil
	}
	semantics, err := lexicalSemanticsSHA256(base)
	if err != nil {
		return LexicalIndexMeta{}, use, err
	}
	if semantics != meta.SemanticsSHA256 {
		use.Reason = LexicalIndexFallbackStale
		return LexicalIndexMeta{}, use, nil
	}
	use.Used = true
	return meta, use, nil
}

func lexicalInputsMatch(
	ctx context.Context, base *Base, prior []LexicalInputFile, expected string,
) (bool, error) {
	_, _, aggregate, err := lexicalInputs(ctx, base, prior)
	if err != nil {
		return false, err
	}
	return aggregate == expected, nil
}

func readLexicalIndexMeta(ctx context.Context, base *Base) (LexicalIndexMeta, error) {
	_, path, err := lexicalIndexPaths(base)
	if err != nil {
		return LexicalIndexMeta{}, err
	}
	data, err := core.ReadFileLimitContext(ctx, path, core.MaxSourceDocumentBytes)
	if err != nil {
		return LexicalIndexMeta{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var meta LexicalIndexMeta
	if err := decoder.Decode(&meta); err != nil {
		return LexicalIndexMeta{}, fmt.Errorf("decode %s: %w", lexicalIndexMetaPath, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return LexicalIndexMeta{}, fmt.Errorf("decode %s: trailing JSON value", lexicalIndexMetaPath)
	} else if !errors.Is(err, io.EOF) {
		return LexicalIndexMeta{}, fmt.Errorf("decode %s trailing JSON: %w", lexicalIndexMetaPath, err)
	}
	return meta, nil
}

func validateLexicalIndexMeta(meta LexicalIndexMeta) error {
	switch {
	case meta.SchemaVersion != lexicalIndexSchemaVersion:
		return fmt.Errorf("%w: schema_version %d", errLexicalIndexCorrupt, meta.SchemaVersion)
	case meta.ExtractorVersion != lexicalIndexExtractorVersion:
		return fmt.Errorf("%w: extractor_version %d", errLexicalIndexStale, meta.ExtractorVersion)
	case meta.Format != lexicalIndexFormat:
		return fmt.Errorf("%w: format %q", errLexicalIndexCorrupt, meta.Format)
	case meta.Entries < 0 || meta.Entries > maxLexicalIndexEntries || meta.ContextEntries < 0 ||
		meta.ContextEntries > meta.Entries || meta.Postings < 0 || meta.PostingRows < 0 ||
		meta.PostingRows > meta.Postings || meta.UnharvestedBullets < 0:
		return fmt.Errorf("%w: invalid counts", errLexicalIndexCorrupt)
	case meta.Bytes < 0 || meta.Bytes > maxLexicalIndexBytes || meta.PostingsOffset < 0 ||
		meta.PostingsOffset > meta.LookupOffset || meta.LookupOffset > meta.CandidatesOffset ||
		meta.CandidatesOffset > int64(meta.Bytes):
		return fmt.Errorf("%w: invalid byte count", errLexicalIndexCorrupt)
	case meta.Entries == 0 && meta.PostingsOffset != 0 || meta.Entries > 0 && meta.PostingsOffset == 0:
		return fmt.Errorf("%w: invalid postings offset", errLexicalIndexCorrupt)
	case !isCanonicalSHA256(meta.EntriesSHA256) || !isCanonicalSHA256(meta.InputsSHA256) ||
		!isCanonicalSHA256(meta.SemanticsSHA256) ||
		!isCanonicalSHA256(meta.OutputSHA256):
		return fmt.Errorf("%w: invalid digest", errLexicalIndexCorrupt)
	}
	if err := validateLexicalLookupShards(meta); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, meta.GeneratedAt)
	if err != nil || parsed.UTC().Format(time.RFC3339) != meta.GeneratedAt {
		return fmt.Errorf("%w: generated_at is not canonical UTC RFC3339", errLexicalIndexCorrupt)
	}
	previous := ""
	for _, input := range meta.Inputs {
		if input.Path <= previous || input.Bytes < 0 || !isCanonicalSHA256(input.SHA256) {
			return fmt.Errorf("%w: invalid input manifest", errLexicalIndexCorrupt)
		}
		previous = input.Path
	}
	return nil
}

func validateLexicalLookupShards(meta LexicalIndexMeta) error {
	if len(meta.LookupShards) != lexicalLookupShardCount {
		return fmt.Errorf("%w: invalid lookup shard count", errLexicalIndexCorrupt)
	}
	offset := meta.LookupOffset
	rows := 0
	for _, shard := range meta.LookupShards {
		if shard.Offset != offset || shard.Bytes < 0 || shard.Rows < 0 ||
			shard.Bytes > int64(maxLexicalIndexBytes) ||
			int64(shard.Rows) > shard.Bytes/minLexicalLookupRowBytes ||
			!isCanonicalSHA256(shard.SHA256) {
			return fmt.Errorf("%w: invalid lookup shard metadata", errLexicalIndexCorrupt)
		}
		if shard.Bytes > meta.CandidatesOffset-offset {
			return fmt.Errorf("%w: lookup shard exceeds its section", errLexicalIndexCorrupt)
		}
		offset += shard.Bytes
		rows += shard.Rows
	}
	if offset != meta.CandidatesOffset || rows != meta.PostingRows {
		return fmt.Errorf("%w: lookup shards do not match metadata", errLexicalIndexCorrupt)
	}
	return nil
}

func lexicalIndexPaths(base *Base) (string, string, error) {
	rows := filepath.Join(base.Root(), filepath.FromSlash(LexicalIndexPath))
	meta := filepath.Join(base.Root(), filepath.FromSlash(lexicalIndexMetaPath))
	if err := core.ValidateWithinRoot(base.Root(), rows); err != nil {
		return "", "", err
	}
	if err := core.ValidateWithinRoot(base.Root(), meta); err != nil {
		return "", "", err
	}
	return rows, meta, nil
}

func lexicalInputs(
	ctx context.Context, base *Base, prior []LexicalInputFile,
) ([]LexicalInputFile, string, string, error) {
	paths, err := lexicalInputPaths(ctx, base)
	if err != nil {
		return nil, "", "", err
	}
	known := make(map[string]LexicalInputFile, len(prior))
	for _, input := range prior {
		known[input.Path] = input
	}
	type inputResult struct {
		input LexicalInputFile
		err   error
	}
	results := make([]inputResult, len(paths))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(32, len(paths)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index].input, results[index].err = inspectLexicalInput(ctx, base, paths[index], known)
			}
		}()
	}
	for index := range paths {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	inputs := make([]LexicalInputFile, 0, len(paths))
	for _, result := range results {
		if errors.Is(result.err, errLexicalInputAbsent) {
			continue
		}
		if result.err != nil {
			return nil, "", "", result.err
		}
		inputs = append(inputs, result.input)
	}
	semantics, err := lexicalSemanticsSHA256(base)
	if err != nil {
		return nil, "", "", err
	}
	aggregate := sha256.New()
	_, _ = aggregate.Write([]byte("fkf-lexical-inputs-v1\x00"))
	writeDigestValue(aggregate, []byte("extractor_version"))
	writeDigestValue(aggregate, []byte(strconv.Itoa(lexicalIndexExtractorVersion)))
	writeDigestValue(aggregate, []byte("semantics"))
	writeDigestValue(aggregate, []byte(semantics))
	for _, input := range inputs {
		writeDigestValue(aggregate, []byte(input.Path))
		writeDigestValue(aggregate, []byte(input.SHA256))
	}
	return inputs, semantics, hex.EncodeToString(aggregate.Sum(nil)), nil
}

func inspectLexicalInput(
	ctx context.Context,
	base *Base,
	relative string,
	known map[string]LexicalInputFile,
) (LexicalInputFile, error) {
	if err := checkContext(ctx); err != nil {
		return LexicalInputFile{}, err
	}
	absolute, err := lexicalInputAbsolute(base, relative)
	if err != nil {
		return LexicalInputFile{}, err
	}
	// Only the body cache may vanish between listing and inspection; every other input is evidence.
	cached := strings.HasPrefix(relative, BodiesDirectory+"/")
	info, err := os.Lstat(absolute)
	if err != nil {
		if cached && errors.Is(err, os.ErrNotExist) {
			return LexicalInputFile{}, errLexicalInputAbsent
		}
		return LexicalInputFile{}, fmt.Errorf("inspect lexical input %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return LexicalInputFile{}, fmt.Errorf("%w: lexical input %s is not a regular file", core.ErrUnsafePath, relative)
	}
	digest, err := resolveLexicalInputDigest(ctx, absolute, relative, info, known, hashLexicalFile)
	if err != nil {
		if cached && errors.Is(err, os.ErrNotExist) {
			return LexicalInputFile{}, errLexicalInputAbsent
		}
		return LexicalInputFile{}, fmt.Errorf("hash lexical input %s: %w", relative, err)
	}
	return LexicalInputFile{
		Path: relative, Bytes: info.Size(), ModifiedUnixNano: info.ModTime().UnixNano(), SHA256: digest,
	}, nil
}

type lexicalFileHasher func(context.Context, string) (int64, string, error)

func resolveLexicalInputDigest(
	ctx context.Context,
	absolute, relative string,
	info os.FileInfo,
	known map[string]LexicalInputFile,
	hash lexicalFileHasher,
) (string, error) {
	if previous, found := known[relative]; found && previous.Bytes == info.Size() &&
		previous.ModifiedUnixNano == info.ModTime().UnixNano() && isCanonicalSHA256(previous.SHA256) {
		return previous.SHA256, nil
	}
	_, digest, err := hash(ctx, absolute)
	return digest, err
}

func lexicalInputPaths(ctx context.Context, base *Base) ([]string, error) {
	paths, err := lexicalDocumentInputPaths(base)
	if err != nil {
		return nil, err
	}
	for _, layer := range []core.Layer{core.LayerProjects, core.LayerTasks, core.LayerWiki} {
		uris, err := graphInputPageURIs(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		paths = append(paths, uris...)
	}
	bodyPaths, err := lexicalBodyInputPaths(ctx, base)
	if err != nil {
		return nil, err
	}
	paths = append(paths, bodyPaths...)
	sort.Strings(paths)
	return compact(paths), nil
}

func lexicalDocumentInputPaths(base *Base) ([]string, error) {
	paths := make([]string, 0)
	for _, layer := range []core.Layer{core.LayerEvents, core.LayerIndex} {
		if !base.Store.Enabled(layer) {
			continue
		}
		switch layer {
		case core.LayerEvents:
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
					paths = append(paths, sources.EventDocumentURI(date, name))
				}
			}
		case core.LayerIndex:
			names, err := base.IndexDocuments()
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				paths = append(paths, sources.IndexDocumentURI(name))
			}
		}
	}
	return paths, nil
}

func lexicalBodyInputPaths(ctx context.Context, base *Base) ([]string, error) {
	manifestPath := bodyManifestPath(base)
	if _, err := os.Lstat(manifestPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect body manifest: %w", err)
	}
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.ToSlash(filepath.Join(BodiesDirectory, bodyManifestFile))}
	for _, entry := range manifest.Entries {
		paths = append(paths, entry.Path)
	}
	return paths, nil
}

func lexicalInputAbsolute(base *Base, relative string) (string, error) {
	if strings.HasPrefix(relative, BodiesDirectory+"/") {
		absolute := filepath.Join(base.Root(), filepath.FromSlash(relative))
		if err := core.ValidateWithinRoot(base.Root(), absolute); err != nil {
			return "", err
		}
		return absolute, nil
	}
	return base.Store.Resolve(relative)
}

func hashLexicalFile(ctx context.Context, path string) (int64, string, error) {
	file, err := core.OpenRegularFile(path)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	bytes, err := io.Copy(digest, io.LimitReader(&contextReader{ctx: ctx, reader: file}, maxLexicalIndexBytes+1))
	if err != nil {
		return 0, "", err
	}
	if bytes > maxLexicalIndexBytes {
		return 0, "", fmt.Errorf("%w: %s exceeds %d bytes", core.ErrFileTooLarge, path, maxLexicalIndexBytes)
	}
	return bytes, hex.EncodeToString(digest.Sum(nil)), nil
}

func lexicalSemanticsSHA256(base *Base) (string, error) {
	type field struct {
		Name     string `json:"name"`
		Weight   int    `json:"weight"`
		Relation bool   `json:"relation"`
	}
	type source struct {
		Name         string  `json:"name"`
		HalfLifeDays int     `json:"half_life_days"`
		Fields       []field `json:"fields"`
	}
	semantic := struct {
		RankingVersion int                       `json:"ranking_version"`
		Layers         []string                  `json:"layers"`
		Fields         []field                   `json:"fields"`
		Sources        []source                  `json:"sources"`
		Identities     map[string]*core.Identity `json:"identities"`
	}{RankingVersion: RankingVersion, Identities: base.Config.Identities}
	for _, layer := range core.Layers {
		if base.Store.Enabled(layer) {
			semantic.Layers = append(semantic.Layers, string(layer))
		}
	}
	for _, name := range base.Config.Schema.Names() {
		definition := base.Config.Schema[name]
		semantic.Fields = append(semantic.Fields, field{
			Name: name, Weight: base.Config.Schema.Weight(name), Relation: definition.Relation,
		})
	}
	for _, name := range base.Config.SourceNames() {
		declared := base.Config.Sources[name]
		entry := source{Name: name, HalfLifeDays: declared.Recency.HalfLifeDays}
		for _, fieldName := range declared.Schema.Names() {
			definition := declared.Schema[fieldName]
			entry.Fields = append(entry.Fields, field{
				Name: fieldName, Weight: declared.Schema.Weight(fieldName), Relation: definition.Relation,
			})
		}
		semantic.Sources = append(semantic.Sources, entry)
	}
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return "", fmt.Errorf("encode lexical ranking semantics: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
