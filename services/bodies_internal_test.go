package services

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestBodyCacheBindsURIContentAndProviderModification(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Meeting body.\n")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ProviderModifiedAt != "2026-05-04T09:30:00Z" || entry.Bytes != len("Meeting body.\n") {
		t.Fatalf("entry = %+v, want provider modification and exact byte count", entry)
	}
	body, loaded, found, err := readCachedBody(t.Context(), base, uri)
	if err != nil || !found || body != "Meeting body.\n" || loaded.SHA256 != entry.SHA256 {
		t.Fatalf("readCachedBody() = %q, %+v, %t, %v", body, loaded, found, err)
	}

	path := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	if err := os.WriteFile(path, []byte("tampered\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readCachedBody(t.Context(), base, uri); err == nil {
		t.Fatal("tampered cached body was accepted")
	}
}

func TestCachedBodiesForURIsReturnsOnlyVerifiedRequestedBodies(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Meeting body.\n"); err != nil {
		t.Fatal(err)
	}
	bodies, err := CachedBodiesForURIs(t.Context(), base, []string{"missing", uri, uri})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[uri] != "Meeting body.\n" {
		t.Fatalf("bodies = %#v", bodies)
	}
}

func TestBodyCacheRefusesLinksAndPrunesTheWholeRebuildableCache(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Body.\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readCachedBody(t.Context(), base, uri); !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("linked cache error = %v, want unsafe path", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Body.\n"); err != nil {
		t.Fatal(err)
	}
	report, err := PruneBodies(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pruned != 1 || report.Bytes != int64(len("Body.\n")) {
		t.Fatalf("prune report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(base.Root(), BodiesDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("body directory after prune = %v, want absent", err)
	}
}

func TestPruneBodiesRepairsACorruptManifest(t *testing.T) {
	base, _, _, _ := bodyCacheFixture(t)
	directory := filepath.Join(base.Root(), BodiesDirectory)
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, bodyManifestFile), []byte("not json\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	report, err := PruneBodies(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if report.Message != "body cache is empty; invalid manifest discarded" {
		t.Fatalf("prune report = %+v", report)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("body directory after repair = %v, want absent", err)
	}
}

func TestBodyCacheRefusesCapacityBeforePublishingBody(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	manifest := newBodyManifest()
	entryBytes := int(core.MaxNarrativeBytes)
	for index := 0; int64(index*entryBytes) < maxBodyCacheBytes; index++ {
		cachedURI := fmt.Sprintf("record:synthetic/%04d", index)
		manifest.Entries[cachedURI] = BodyManifestEntry{
			URI: cachedURI, Source: "synthetic", Path: bodyCacheRelative("synthetic", cachedURI),
			SHA256: strings.Repeat("0", sha256.Size*2), Bytes: entryBytes,
		}
	}
	if err := writeBodyManifest(base, manifest); err != nil {
		t.Fatal(err)
	}

	if _, err := cacheBody(t.Context(), base, document, record, uri, "one byte over the cache total"); err == nil || !strings.Contains(err.Error(), "body cache exceeds") {
		t.Fatalf("cacheBody() error = %v, want global byte-bound refusal", err)
	}
	target := filepath.Join(base.Root(), filepath.FromSlash(bodyCacheRelative(document.Source, uri)))
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("body published before capacity check: %v", err)
	}
}

func TestBodyManifestWriterNeverCreatesAnUnreadableManifest(t *testing.T) {
	manifest := newBodyManifest()
	uri := "record:synthetic/" + strings.Repeat("x", int(core.MaxConfigBytes))
	manifest.Entries[uri] = BodyManifestEntry{
		URI: uri, Source: "synthetic", Path: bodyCacheRelative("synthetic", uri),
		SHA256: strings.Repeat("0", sha256.Size*2),
	}
	if _, err := encodeBodyManifest(manifest); err == nil || !strings.Contains(err.Error(), "manifest is") {
		t.Fatalf("encodeBodyManifest() error = %v, want manifest byte-bound refusal", err)
	}
}

func bodyCacheFixture(t *testing.T) (*Base, *sources.Document, sources.Record, string) {
	t.Helper()
	root := t.TempDir()
	base := &Base{
		Store: core.NewStore(root, map[core.Layer]bool{core.LayerEvents: true}),
		Now:   func() time.Time { return time.Date(2026, time.May, 5, 12, 0, 0, 0, time.UTC) },
	}
	id, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	modified, err := core.ParseFieldPath(".modified")
	if err != nil {
		t.Fatal(err)
	}
	document := &sources.Document{
		Source: "meeting-notes", Layer: core.LayerEvents, Date: "2026-05-04",
		Fields: core.FieldMap{
			core.FieldID: {id},
			"modified":   {modified},
		},
	}
	record := sources.Record{"id": "doc-1", "modified": "2026-05-04T09:30:00Z"}
	uri, ok := document.RecordURI(record)
	if !ok {
		t.Fatal("fixture record has no URI")
	}
	return base, document, record, uri
}
