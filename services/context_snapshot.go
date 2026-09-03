package services

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

const (
	contextSnapshotVersion       = 3
	contextSnapshotRetention     = 16
	maxContextSnapshotCompressed = 64 << 20
	maxContextSnapshotDecoded    = 128 << 20
)

type contextSnapshot struct {
	Version     int                    `json:"version"`
	Base        string                 `json:"base"`
	InputDigest string                 `json:"input_digest"`
	RequestKey  string                 `json:"request_key"`
	Query       string                 `json:"query"`
	Window      Window                 `json:"window"`
	AsOf        string                 `json:"as_of"`
	Entries     []contextSnapshotEntry `json:"entries"`
}

type contextSnapshotEntry struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

// contextDeltaCandidates compares semantic candidate digests rather than timestamps. A file
// copied between machines, a same-length edit, and a cached-body refresh therefore have the
// same meaning: unchanged bytes disappear from the delta; changed bytes remain.
func contextDeltaCandidates(
	base *Base,
	request ContextRequest,
	candidates []*ContextItem,
) ([]*ContextItem, error) {
	if !validContextInputDigest(request.SinceReceipt) {
		return nil, fmt.Errorf("%w: --since-receipt must be one lowercase 16-character SHA-256 input digest", core.ErrConfig)
	}
	snapshot, err := loadContextSnapshot(base, request.SinceReceipt)
	if err != nil {
		return nil, err
	}
	key := contextSnapshotRequestKey(request)
	if snapshot.RequestKey != key {
		return nil, fmt.Errorf("%w: receipt snapshot %s belongs to context query %q, window %s..%s, and as_of %s; reuse the same query, window, and as_of",
			core.ErrConfig, request.SinceReceipt, snapshot.Query,
			snapshot.Window.Since, snapshot.Window.Until, snapshot.AsOf)
	}
	previous := make(map[string]string, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		previous[entry.URI] = entry.SHA256
	}
	changed := make([]*ContextItem, 0)
	for _, candidate := range candidates {
		if previous[candidate.URI] != contextCandidateDigest(candidate) {
			changed = append(changed, candidate)
		}
	}
	return changed, nil
}

// storeContextSnapshot persists only semantic digests, never evidence or rendered commands.
// It is rebuildable, machine-local state: the base stays a repository of authored and collected
// truth, while a later process can still prove which current items differ from one receipt.
func storeContextSnapshot(
	base *Base,
	request ContextRequest,
	inputDigest string,
	candidates []*ContextItem,
) error {
	physicalRoot, err := core.ResolvePhysicalPath(base.Root())
	if err != nil {
		return fmt.Errorf("save context receipt snapshot: resolve physical base: %w", err)
	}
	path, err := contextSnapshotPath(physicalRoot, inputDigest)
	if err != nil {
		return fmt.Errorf("save context receipt snapshot: %w", err)
	}
	entries := make([]contextSnapshotEntry, 0, len(candidates))
	for _, candidate := range candidates {
		entries = append(entries, contextSnapshotEntry{
			URI: candidate.URI, SHA256: contextCandidateDigest(candidate),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].URI < entries[j].URI })
	for index := 1; index < len(entries); index++ {
		if entries[index-1].URI == entries[index].URI {
			return fmt.Errorf("save context receipt snapshot: duplicate candidate URI %q", entries[index].URI)
		}
	}
	snapshot := contextSnapshot{
		Version: contextSnapshotVersion, Base: physicalRoot, InputDigest: inputDigest,
		RequestKey: contextSnapshotRequestKey(request), Query: request.Query,
		Window: request.Window, AsOf: request.asOf, Entries: entries,
	}
	var compressed bytes.Buffer
	zipper, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("save context receipt snapshot: start compression: %w", err)
	}
	encoder := json.NewEncoder(zipper)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(snapshot); err != nil {
		_ = zipper.Close()
		return fmt.Errorf("save context receipt snapshot: encode: %w", err)
	}
	if err := zipper.Close(); err != nil {
		return fmt.Errorf("save context receipt snapshot: finish compression: %w", err)
	}
	if compressed.Len() > maxContextSnapshotCompressed {
		return fmt.Errorf("save context receipt snapshot: compressed manifest is %d bytes; maximum is %d",
			compressed.Len(), maxContextSnapshotCompressed)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("save context receipt snapshot: %w: %s is a symbolic link", core.ErrUnsafePath, path)
	} else if err == nil && info.Mode().IsRegular() {
		existing, readErr := core.ReadFileLimit(path, maxContextSnapshotCompressed)
		if readErr != nil {
			return fmt.Errorf("save context receipt snapshot: compare existing snapshot: %w", readErr)
		}
		if bytes.Equal(existing, compressed.Bytes()) {
			return nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("save context receipt snapshot: inspect target: %w", err)
	}
	if err := core.WriteFileAtomicMode(path, compressed.Bytes(), core.BaseFileMode); err != nil {
		return fmt.Errorf("save context receipt snapshot: %w", err)
	}
	if err := pruneContextSnapshots(filepath.Dir(path), filepath.Base(path)); err != nil {
		return fmt.Errorf("prune context receipt snapshots: %w", err)
	}
	return nil
}

func loadContextSnapshot(base *Base, digest string) (*contextSnapshot, error) {
	physicalRoot, err := core.ResolvePhysicalPath(base.Root())
	if err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: resolve physical base: %w", digest, err)
	}
	path, err := contextSnapshotPath(physicalRoot, digest)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: receipt snapshot %s is not available on this machine; run the original context query once without --since-receipt to seed it",
			core.ErrConfig, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: %w", digest, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("read receipt snapshot %s: %w: state entry is not a regular file", digest, core.ErrUnsafePath)
	}
	encoded, err := core.ReadFileLimit(path, maxContextSnapshotCompressed)
	if err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: %w", digest, err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: invalid gzip: %w", digest, err)
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxContextSnapshotDecoded+1))
	closeErr := reader.Close()
	if err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: decompress: %w", digest, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: finish decompression: %w", digest, closeErr)
	}
	if len(decoded) > maxContextSnapshotDecoded {
		return nil, fmt.Errorf("read receipt snapshot %s: decoded manifest exceeds %d bytes", digest, maxContextSnapshotDecoded)
	}
	var snapshot contextSnapshot
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: decode: %w", digest, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("read receipt snapshot %s: trailing JSON value", digest)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read receipt snapshot %s: invalid trailing JSON: %w", digest, err)
	}
	if err := validateContextSnapshot(physicalRoot, digest, &snapshot); err != nil {
		return nil, fmt.Errorf("read receipt snapshot %s: %w", digest, err)
	}
	return &snapshot, nil
}

func validateContextSnapshot(physicalRoot, digest string, snapshot *contextSnapshot) error {
	if snapshot.Version != contextSnapshotVersion || snapshot.Base != physicalRoot || snapshot.InputDigest != digest ||
		!validSHA256(snapshot.RequestKey) {
		return errors.New("manifest identity or version does not match the requested base and receipt")
	}
	previous := ""
	for _, entry := range snapshot.Entries {
		if entry.URI == "" || !validSHA256(entry.SHA256) || previous >= entry.URI {
			return errors.New("manifest entries are invalid, duplicated, or out of order")
		}
		previous = entry.URI
	}
	return nil
}

func contextSnapshotPath(physicalRoot, digest string) (string, error) {
	if !validContextInputDigest(digest) {
		return "", fmt.Errorf("%w: invalid context input digest", core.ErrConfig)
	}
	state := core.StateDir()
	if state == "" {
		return "", core.ErrStateDirectoryUnavailable
	}
	sum := sha256.Sum256([]byte(physicalRoot))
	return filepath.Join(state, "receipts", hex.EncodeToString(sum[:]), digest+".json.gz"), nil
}

func contextSnapshotRequestKey(request ContextRequest) string {
	// Budget, delivery format, and explanation detail affect presentation, not which semantic
	// candidates a prior snapshot represents. The resolved bounds and evaluation day do affect
	// validity and membership, so a receipt must fail closed when either one changes.
	input := struct {
		Version int      `json:"version"`
		Query   string   `json:"query"`
		Since   string   `json:"since"`
		Until   string   `json:"until"`
		AsOf    string   `json:"as_of"`
		Pins    []string `json:"pins,omitempty"`
		Expand  bool     `json:"expand"`
		Newest  bool     `json:"newest"`
	}{
		Version: RankingVersion, Query: request.Query,
		Since: request.Window.Since, Until: request.Window.Until, AsOf: request.asOf,
		Pins: append([]string(nil), request.Pins...), Expand: request.Expand, Newest: request.Newest,
	}
	encoded, _ := json.Marshal(input)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func contextCandidateDigest(candidate *ContextItem) string {
	semanticDigest := candidate.semanticDigest
	if semanticDigest == "" {
		semanticDigest = contextCandidateSemanticDigest(candidate)
	}
	input := struct {
		SemanticDigest string   `json:"semantic_digest"`
		CollapsedURIs  []string `json:"collapsed_uris,omitempty"`
		Count          int      `json:"count,omitempty"`
		SupersededBy   string   `json:"superseded_by,omitempty"`
		SupersededRank string   `json:"superseded_rank,omitempty"`
	}{
		SemanticDigest: semanticDigest,
		CollapsedURIs:  candidate.collapsedURIs,
		Count:          candidate.Count,
		SupersededBy:   candidate.supersededBy,
		SupersededRank: candidate.supersededRank,
	}
	encoded, _ := json.Marshal(input)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// contextCandidateSemanticDigest excludes query-dependent rank state. The lexical index stores
// this digest beside each compact summary; the receipt snapshot then binds it to the query-time
// collapse metadata without trusting the cache for evidence contents.
func contextCandidateSemanticDigest(candidate *ContextItem) string {
	identities := make([]string, 0, len(candidate.identityTerms))
	for value := range candidate.identityTerms {
		identities = append(identities, value)
	}
	sort.Strings(identities)
	identifiers := make([]string, 0, len(candidate.identifierKeys))
	for value := range candidate.identifierKeys {
		identifiers = append(identifiers, value)
	}
	sort.Strings(identifiers)
	semantic := struct {
		URI, Kind, Source, Date, Time, Title, URL, Status, Haystack string
		DefaultExcluded, ValidityRank                               string
		Tags, Identities, Identifiers, Supersedes                   []string
		Fields                                                      map[string][]string
		Segments                                                    []contextSegment
		Created                                                     bool
	}{
		URI: candidate.URI, Kind: candidate.Kind, Source: candidate.Source,
		Date: candidate.Date, Time: candidate.Time, Title: candidate.Title,
		URL: candidate.URL, Status: candidate.Status, Haystack: candidate.haystack,
		DefaultExcluded: candidate.defaultExcluded, ValidityRank: candidate.validityRank,
		Tags:   candidate.Tags,
		Fields: candidate.Fields, Segments: candidate.segments, Identities: identities,
		Identifiers: identifiers, Supersedes: candidate.supersedes,
		Created: candidate.createdEvidence,
	}
	encoded, _ := json.Marshal(semantic)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func pruneContextSnapshots(directory, keep string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type retained struct {
		name    string
		modTime int64
	}
	files := make([]retained, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json.gz") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, retained{name: entry.Name(), modTime: info.ModTime().UnixNano()})
		}
	}
	if len(files) <= contextSnapshotRetention {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime != files[j].modTime {
			return files[i].modTime < files[j].modTime
		}
		return files[i].name < files[j].name
	})
	remove := len(files) - contextSnapshotRetention
	for _, file := range files {
		if remove == 0 {
			break
		}
		if file.name == keep {
			continue
		}
		if err := os.Remove(filepath.Join(directory, file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remove--
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validContextInputDigest(value string) bool {
	if len(value) != 16 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
