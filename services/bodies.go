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
	"strings"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

const (
	BodiesDirectory     = "bodies"
	bodyManifestFile    = "manifest.json"
	bodyManifestSchema  = 1
	maxBodyCacheEntries = 4096
	maxBodyCacheBytes   = int64(512 << 20)
)

// BodyManifest binds each record URI to its bounded local text copy. It is a rebuildable,
// machine-local cache and never changes the collected evidence envelope.
type BodyManifest struct {
	SchemaVersion int                          `json:"schema_version"`
	Entries       map[string]BodyManifestEntry `json:"entries"`
	// EventAttempts prevents a vanished historical resource from becoming perpetual sync work.
	// Pruning the rebuildable cache removes these markers and deliberately arms one new restore.
	EventAttempts map[string]bool `json:"event_attempts,omitempty"`
}

// BodyManifestEntry is the integrity and freshness record for one cached body.
type BodyManifestEntry struct {
	URI                string `json:"uri"`
	Source             string `json:"source"`
	Path               string `json:"path"`
	SHA256             string `json:"sha256"`
	Bytes              int    `json:"bytes"`
	ProviderModifiedAt string `json:"provider_modified_at,omitempty"`
	FetchedAt          string `json:"fetched_at"`
}

// BodiesBuildReport reports an explicit cache maintenance operation.
type BodiesBuildReport struct {
	Pruned  int    `json:"pruned"`
	Bytes   int64  `json:"bytes"`
	Message string `json:"message"`
}

func newBodyManifest() *BodyManifest {
	return &BodyManifest{
		SchemaVersion: bodyManifestSchema,
		Entries:       map[string]BodyManifestEntry{},
		EventAttempts: map[string]bool{},
	}
}

func bodyCacheRelative(sourceName, uri string) string {
	digest := sha256.Sum256([]byte(uri))
	return filepath.ToSlash(filepath.Join(BodiesDirectory, sourceName, hex.EncodeToString(digest[:])+".txt"))
}

func bodyManifestPath(base *Base) string {
	return filepath.Join(base.Root(), BodiesDirectory, bodyManifestFile)
}

func loadBodyManifest(ctx context.Context, base *Base) (*BodyManifest, error) {
	path := bodyManifestPath(base)
	if err := core.ValidateWithinRoot(base.Root(), path); err != nil {
		return nil, err
	}
	data, err := core.ReadFileLimitContext(ctx, path, core.MaxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return newBodyManifest(), nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	manifest := newBodyManifest()
	if err := decoder.Decode(manifest); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.ToSlash(filepath.Join(BodiesDirectory, bodyManifestFile)), err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("decode %s: trailing JSON value", filepath.ToSlash(filepath.Join(BodiesDirectory, bodyManifestFile)))
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: invalid trailing JSON: %w",
			filepath.ToSlash(filepath.Join(BodiesDirectory, bodyManifestFile)), err)
	}
	if manifest.SchemaVersion != bodyManifestSchema {
		return nil, fmt.Errorf("body manifest schema_version is %d; expected %d", manifest.SchemaVersion, bodyManifestSchema)
	}
	if manifest.Entries == nil {
		manifest.Entries = map[string]BodyManifestEntry{}
	}
	if manifest.EventAttempts == nil {
		manifest.EventAttempts = map[string]bool{}
	}
	for uri, entry := range manifest.Entries {
		if err := validateBodyManifestEntry(base, uri, entry); err != nil {
			return nil, err
		}
	}
	for source := range manifest.EventAttempts {
		if err := core.ValidateSourceName(source); err != nil {
			return nil, fmt.Errorf("body manifest event_attempts entry %q: %w", source, err)
		}
	}
	if err := validateBodyManifestCapacity(manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func validateBodyManifestEntry(base *Base, key string, entry BodyManifestEntry) error {
	if entry.URI != key || entry.Source == "" || entry.Path != bodyCacheRelative(entry.Source, key) {
		return fmt.Errorf("body manifest entry %q does not match its URI, source, and canonical cache path", key)
	}
	if err := core.ValidateSourceName(entry.Source); err != nil {
		return fmt.Errorf("body manifest entry %q source: %w", key, err)
	}
	if len(entry.SHA256) != sha256.Size*2 || entry.Bytes < 0 || entry.Bytes > int(core.MaxNarrativeBytes) {
		return fmt.Errorf("body manifest entry %q has invalid digest or size", key)
	}
	if _, err := hex.DecodeString(entry.SHA256); err != nil {
		return fmt.Errorf("body manifest entry %q sha256: %w", key, err)
	}
	absolute := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	return core.ValidateWithinRoot(base.Root(), absolute)
}

func encodeBodyManifest(manifest *BodyManifest) ([]byte, error) {
	if err := validateBodyManifestCapacity(manifest); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode body manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > core.MaxConfigBytes {
		return nil, fmt.Errorf(
			"body cache manifest is %d bytes; limit %d; run `fkf build bodies --prune`",
			len(encoded), core.MaxConfigBytes,
		)
	}
	return encoded, nil
}

func validateBodyManifestCapacity(manifest *BodyManifest) error {
	if len(manifest.Entries) > maxBodyCacheEntries {
		return fmt.Errorf(
			"body cache has %d entries; limit %d; run `fkf build bodies --prune`",
			len(manifest.Entries), maxBodyCacheEntries,
		)
	}
	var total int64
	for _, entry := range manifest.Entries {
		if int64(entry.Bytes) > maxBodyCacheBytes-total {
			return fmt.Errorf(
				"body cache exceeds %d bytes; run `fkf build bodies --prune`",
				maxBodyCacheBytes,
			)
		}
		total += int64(entry.Bytes)
	}
	return nil
}

func writeBodyManifest(base *Base, manifest *BodyManifest) error {
	encoded, err := encodeBodyManifest(manifest)
	if err != nil {
		return err
	}
	path := bodyManifestPath(base)
	if err := core.ValidateWithinRoot(base.Root(), path); err != nil {
		return err
	}
	if err := core.ValidateDirectoryConfinement(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), core.BaseDirMode); err != nil {
		return fmt.Errorf("create body manifest directory: %w", err)
	}
	return core.WriteFileAtomicMode(path, encoded, core.BaseFileMode)
}

// readCachedBody returns a verified cached body without executing any command.
func readCachedBody(ctx context.Context, base *Base, uri string) (string, *BodyManifestEntry, bool, error) {
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return "", nil, false, err
	}
	return readCachedBodyFromManifest(ctx, base, manifest, uri)
}

func readCachedBodyFromManifest(
	ctx context.Context, base *Base, manifest *BodyManifest, uri string,
) (string, *BodyManifestEntry, bool, error) {
	entry, found := manifest.Entries[uri]
	if !found {
		return "", nil, false, nil
	}
	absolute := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	data, err := core.ReadFileLimitContext(ctx, absolute, core.MaxNarrativeBytes)
	if err != nil {
		return "", nil, false, fmt.Errorf("read cached body for %s: %w", uri, err)
	}
	digest := sha256.Sum256(data)
	if len(data) != entry.Bytes || hex.EncodeToString(digest[:]) != entry.SHA256 {
		return "", nil, false, fmt.Errorf("cached body for %s does not match its manifest; run `fkf build bodies --prune`", uri)
	}
	return string(data), &entry, true, nil
}

// cacheBody atomically writes one bounded UTF-8 body and then publishes its manifest entry.
func cacheBody(
	ctx context.Context,
	base *Base,
	document *sources.Document,
	record sources.Record,
	uri, body string,
) (*BodyManifestEntry, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if len(body) > int(core.MaxNarrativeBytes) {
		return nil, fmt.Errorf("body for %s is %d bytes; limit %d", uri, len(body), core.MaxNarrativeBytes)
	}
	if !utf8.ValidString(body) {
		return nil, fmt.Errorf("body for %s is not valid UTF-8", uri)
	}
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	relative := bodyCacheRelative(document.Source, uri)
	absolute := filepath.Join(base.Root(), filepath.FromSlash(relative))
	if err := core.ValidateWithinRoot(base.Root(), absolute); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(body))
	entry := BodyManifestEntry{
		URI: uri, Source: document.Source, Path: relative, SHA256: hex.EncodeToString(digest[:]),
		Bytes: len(body), ProviderModifiedAt: bodyProviderModifiedAt(document, record),
		FetchedAt: base.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	manifest.Entries[uri] = entry
	// Prove that the next manifest remains readable before publishing a body it could not name.
	if _, err := encodeBodyManifest(manifest); err != nil {
		return nil, err
	}
	if err := core.ValidateDirectoryConfinement(filepath.Dir(absolute)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
		return nil, fmt.Errorf("create body cache directory: %w", err)
	}
	if err := core.WriteFileAtomicMode(absolute, []byte(body), core.BaseFileMode); err != nil {
		return nil, err
	}
	if err := writeBodyManifest(base, manifest); err != nil {
		return nil, err
	}
	return &entry, nil
}

func bodyProviderModifiedAt(document *sources.Document, record sources.Record) string {
	for _, name := range []string{"modified", core.FieldTime} {
		if value, found := document.Fields.EvalString(name, map[string]any(record)); found {
			return value
		}
	}
	return ""
}

// PruneBodies removes the entire rebuildable body cache and recreates no state.
func PruneBodies(ctx context.Context, base *Base) (*BodiesBuildReport, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	directory := filepath.Join(base.Root(), BodiesDirectory)
	if err := core.ValidateWithinRoot(base.Root(), directory); err != nil {
		return nil, err
	}
	manifest, manifestErr := loadBodyManifest(ctx, base)
	report := &BodiesBuildReport{Message: "body cache is empty"}
	if manifestErr == nil {
		report.Pruned = len(manifest.Entries)
		for _, entry := range manifest.Entries {
			report.Bytes += int64(entry.Bytes)
		}
	} else {
		// Prune is the recovery path named by cache-integrity errors. The directory has already
		// passed the base-root check above, and RemoveAll never follows a symlink target.
		report.Message = "body cache is empty; invalid manifest discarded"
	}
	if err := os.RemoveAll(directory); err != nil {
		return nil, fmt.Errorf("prune body cache: %w", err)
	}
	return report, nil
}

// CachedBodiesForURIs reads only requested, manifest-verified body text. It is the offline
// retrieval seam used by context, find, and the lexical index; it never invokes body argv.
func CachedBodiesForURIs(ctx context.Context, base *Base, uris []string) (map[string]string, error) {
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	requested := append([]string(nil), uris...)
	sort.Strings(requested)
	requested = compact(requested)
	bodies := make(map[string]string)
	for _, uri := range requested {
		body, _, found, err := readCachedBodyFromManifest(ctx, base, manifest, uri)
		if err != nil {
			return nil, err
		}
		if found {
			bodies[uri] = body
		}
	}
	return bodies, nil
}
