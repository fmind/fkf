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

// An interrupted selective prune unlinks a body while its manifest entry survives. The cache is
// ignored and rebuildable, so every offline reader must treat that as a miss: before this, one
// dangling entry failed `context`, `find --bodies`, and `build index` on the whole base.
func TestCachedBodiesForURIsTreatsAnUnlinkedBodyAsAMiss(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Meeting body.\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base.Root(), filepath.FromSlash(entry.Path))); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := manifest.Entries[uri]; !found {
		t.Fatalf("the manifest entry must survive for this regression to be meaningful")
	}
	bodies, err := CachedBodiesForURIs(t.Context(), base, []string{uri})
	if err != nil {
		t.Fatalf("an unlinked body must not fail the read: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("bodies = %#v, want no entry for the unlinked body", bodies)
	}
}

// A body whose bytes no longer match the manifest is real corruption, not a miss, and must stay
// loud so a wrong body can never reach a context pack.
func TestCachedBodiesForURIsStillRefusesACorruptBody(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Meeting body.\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	if err := os.WriteFile(path, []byte("tampered.\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedBodiesForURIs(t.Context(), base, []string{uri}); err == nil {
		t.Fatal("a body that does not match its manifest must fail the read")
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

func TestPruneBodiesSelectiveBySourceAndAge(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Body 1.\n"); err != nil {
		t.Fatal(err)
	}

	doc2 := *document
	doc2.Source = "another-source"
	uri2 := "another:uri-2"
	record2 := sources.Record{"id": "another-2"}
	if _, err := cacheBody(t.Context(), base, &doc2, record2, uri2, "Body 2.\n"); err != nil {
		t.Fatal(err)
	}

	// A source the base neither declares nor caches is a typo, not an empty selection
	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "non-existent"}); err == nil ||
		!errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), `unknown source "non-existent"`) {
		t.Fatalf("prune --source non-existent error = %v, want an unknown-source refusal", err)
	}

	// A source-and-age selection with only young entries must preserve both cache bytes and the
	// event-attempt marker that prevents the next sync from repeating historical provider work.
	manifestPath := bodyManifestPath(base)
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EventAttempts["meeting-notes"] = true
	if err := writeBodyManifest(base, manifest); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{
		Source: "meeting-notes", OlderThan: 365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pruned != 0 || report.Message != "nothing matched; body cache unchanged" {
		t.Fatalf("no-op prune report = %+v", report)
	}
	after, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a prune that matched nothing rewrote the body manifest")
	}
	manifest, err = loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.EventAttempts["meeting-notes"] {
		t.Fatal("a source-and-age no-op prune rearmed the source's event restore")
	}

	// Pruning with Source "another-source" prunes only doc2
	report, err = PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "another-source"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pruned != 1 || report.Bytes != int64(len("Body 2.\n")) {
		t.Fatalf("pruned report = %+v", report)
	}

	// Verify doc1 is still in cache
	bodies, err := CachedBodiesForURIs(t.Context(), base, []string{uri})
	if err != nil {
		t.Fatal(err)
	}
	if bodies[uri] != "Body 1.\n" {
		t.Fatalf("cached body = %q, want Body 1.\n", bodies[uri])
	}

	// Pruning with OlderThan older than the entry prunes nothing
	report, err = PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pruned != 0 {
		t.Fatalf("pruned = %d, want 0", report.Pruned)
	}

	// Advance base.Now by 2 days so the entry is older than 24 hours
	base.Now = func() time.Time { return time.Date(2026, time.May, 7, 12, 0, 0, 0, time.UTC) }
	report, err = PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pruned != 1 || report.Bytes != int64(len("Body 1.\n")) {
		t.Fatalf("pruned report = %+v", report)
	}

	// Body directory should now be empty and removed
	if _, err := os.Stat(filepath.Join(base.Root(), BodiesDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("body directory after all pruned = %v, want absent", err)
	}
}

// TestPruneBodiesPublishesTheManifestBeforeUnlinkingBodies pins the crash-consistent order: a
// manifest entry whose file is gone fails every offline read, while an orphan file no entry
// names is inert, so the trimmed manifest has to become durable first.
func TestPruneBodiesPublishesTheManifestBeforeUnlinkingBodies(t *testing.T) {
	requireUnprivilegedModes(t)
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Body 1.\n"); err != nil {
		t.Fatal(err)
	}
	doc2 := *document
	doc2.Source = "another-source"
	if _, err := cacheBody(t.Context(), base, &doc2, sources.Record{"id": "another-2"}, "another:uri-2", "Body 2.\n"); err != nil {
		t.Fatal(err)
	}

	// A read-only bodies/ fails the atomic manifest write while each source directory below it
	// still permits unlinking, which is exactly the window the ordering has to survive.
	directory := filepath.Join(base.Root(), BodiesDirectory)
	chmodForTest(t, directory, 0o500)

	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "another-source"}); err == nil {
		t.Fatal("prune reported success although it could not publish the trimmed manifest")
	}
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 2 {
		t.Fatalf("manifest entries after a failed prune = %d, want the untouched 2", len(manifest.Entries))
	}
	for name, entry := range manifest.Entries {
		if _, err := os.Stat(filepath.Join(base.Root(), filepath.FromSlash(entry.Path))); err != nil {
			t.Errorf("manifest names %s but its body was unlinked before the manifest was durable: %v", name, err)
		}
	}
}

func TestPruneBodiesReportsABodyItCannotUnlink(t *testing.T) {
	requireUnprivilegedModes(t)
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Body 1.\n")
	if err != nil {
		t.Fatal(err)
	}
	doc2 := *document
	doc2.Source = "another-source"
	if _, err := cacheBody(t.Context(), base, &doc2, sources.Record{"id": "another-2"}, "another:uri-2", "Body 2.\n"); err != nil {
		t.Fatal(err)
	}
	chmodForTest(t, filepath.Dir(filepath.Join(base.Root(), filepath.FromSlash(entry.Path))), 0o500)

	_, err = PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "meeting-notes"})
	if err == nil || !strings.Contains(err.Error(), "prune cached body") {
		t.Fatalf("prune error = %v, want the body it could not unlink", err)
	}
	// The entry is already out of the published manifest, so what survives is an orphan file no
	// reader follows rather than an entry pointing at a body that offline reads would demand.
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := manifest.Entries[uri]; found {
		t.Fatal("the manifest still names a body the prune had already accounted for")
	}
}

func requireUnprivilegedModes(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
}

// chmodForTest restricts a directory and restores it, so TempDir teardown can still remove it.
func chmodForTest(t *testing.T, directory string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(directory, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(directory, core.BaseDirMode); err != nil {
			t.Error(err)
		}
	})
}

func TestPruneBodiesClearsEventAttempts(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Body 1.\n"); err != nil {
		t.Fatal(err)
	}

	doc2 := *document
	doc2.Source = "another-source"
	uri2 := "another:uri-2"
	record2 := sources.Record{"id": "another-2"}
	if _, err := cacheBody(t.Context(), base, &doc2, record2, uri2, "Body 2.\n"); err != nil {
		t.Fatal(err)
	}

	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EventAttempts["meeting-notes"] = true
	manifest.EventAttempts["another-source"] = true
	manifest.EventAttempts["unrelated-empty"] = true
	if err := writeBodyManifest(base, manifest); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "another-source"}); err != nil {
		t.Fatal(err)
	}
	manifest, err = loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EventAttempts["another-source"] {
		t.Errorf("expected another-source EventAttempts to be cleared")
	}
	if !manifest.EventAttempts["meeting-notes"] {
		t.Errorf("expected meeting-notes EventAttempts to remain")
	}
	if !manifest.EventAttempts["unrelated-empty"] {
		t.Errorf("expected unrelated-empty EventAttempts to remain when another source was pruned")
	}

	// Age pruning that removes all meeting-notes entries should clear meeting-notes attempts
	base.Now = func() time.Time { return time.Date(2026, time.May, 7, 12, 0, 0, 0, time.UTC) }
	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{OlderThan: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	// All entries are gone, but the unrelated marker remains durable rather than re-arming its
	// source merely because a filtered prune happened to remove the final body.
	manifest, err = loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 0 || !manifest.EventAttempts["unrelated-empty"] {
		t.Fatalf("manifest after age prune = %+v, want only the unrelated restore marker", manifest)
	}
}

// Removing a source's last cached body must not discard another source's restore marker. That
// marker prevents the next scheduled sync from retrying a vanished historical provider object.
func TestPruneBodiesSelectivePreservesUnrelatedEventAttemptsAfterLastEntry(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	if _, err := cacheBody(t.Context(), base, document, record, uri, "Body.\n"); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EventAttempts["another-source"] = true
	if err := writeBodyManifest(base, manifest); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "meeting-notes"}); err != nil {
		t.Fatal(err)
	}
	manifest, err = loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 0 || !manifest.EventAttempts["another-source"] {
		t.Fatalf("manifest after selective last-entry prune = %+v, want only the unrelated restore marker", manifest)
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

func TestPruneBodiesRefusesASymlinkedCacheRootWithoutTouchingItsTarget(t *testing.T) {
	base, _, _, _ := bodyCacheFixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, bodyManifestFile)
	if err := os.WriteFile(sentinel, []byte("outside\n"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(base.Root(), BodiesDirectory)
	if err := os.Symlink(outside, directory); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	if _, err := PruneBodies(t.Context(), base); !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("PruneBodies() error = %v, want unsafe-path refusal", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside\n" {
		t.Fatalf("outside manifest after refused prune = %q, %v; want it untouched", data, err)
	}
	if info, err := os.Lstat(directory); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("body cache link after refused prune = %v, %v; want it untouched", info, err)
	}
}

// A selective prune cannot know which cache entries match when the manifest is corrupt. The
// unfiltered command remains the explicit whole-cache recovery path, but a source or age filter
// must fail closed instead of silently deleting unrelated cached bodies.
func TestPruneBodiesSelectiveRefusesACorruptManifest(t *testing.T) {
	for _, test := range []struct {
		name    string
		options PruneBodiesOptions
	}{
		{name: "source", options: PruneBodiesOptions{Source: "meeting-notes"}},
		{name: "age", options: PruneBodiesOptions{OlderThan: 24 * time.Hour}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, document, record, uri := bodyCacheFixture(t)
			entry, err := cacheBody(t.Context(), base, document, record, uri, "Body.\n")
			if err != nil {
				t.Fatal(err)
			}
			manifest := bodyManifestPath(base)
			if err := os.WriteFile(manifest, []byte("not json\n"), core.BaseFileMode); err != nil {
				t.Fatal(err)
			}

			if _, err := PruneBodiesWithOptions(t.Context(), base, test.options); err == nil ||
				!strings.Contains(err.Error(), "selective body prune requires a readable manifest") {
				t.Fatalf("selective prune error = %v, want a readable-manifest refusal", err)
			}
			body := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
			if data, err := os.ReadFile(body); err != nil || string(data) != "Body.\n" {
				t.Fatalf("cached body after refused prune = %q, %v; want it preserved", data, err)
			}
			if data, err := os.ReadFile(manifest); err != nil || string(data) != "not json\n" {
				t.Fatalf("manifest after refused prune = %q, %v; want it untouched", data, err)
			}
		})
	}
}

// An age filter cannot decide whether an entry matches when its cache timestamp is malformed.
// The unfiltered prune remains the explicit recovery path, while a selective prune fails closed
// and preserves the body it cannot classify.
func TestPruneBodiesByAgeRefusesAMalformedFetchedAt(t *testing.T) {
	base, document, record, uri := bodyCacheFixture(t)
	entry, err := cacheBody(t.Context(), base, document, record, uri, "Body.\n")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBodyManifest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	entry.FetchedAt = "not-a-timestamp"
	manifest.Entries[uri] = *entry
	if err := writeBodyManifest(base, manifest); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{OlderThan: time.Hour}); err == nil ||
		!strings.Contains(err.Error(), "fetched_at") {
		t.Fatalf("age prune error = %v, want the malformed fetched_at named", err)
	}
	absolute := filepath.Join(base.Root(), filepath.FromSlash(entry.Path))
	if data, err := os.ReadFile(absolute); err != nil || string(data) != "Body.\n" {
		t.Fatalf("body after refused age prune = %q, %v; want it preserved", data, err)
	}
	if _, err := PruneBodies(t.Context(), base); err != nil {
		t.Fatalf("whole-cache recovery prune = %v", err)
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
			FetchedAt: "2026-05-05T12:00:00Z",
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

func TestPruneBodiesValidatesOptions(t *testing.T) {
	base, _, _, _ := bodyCacheFixture(t)
	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{Source: "INVALID_NAME"}); err == nil {
		t.Fatal("expected error for invalid source name, got nil")
	}
	if _, err := PruneBodiesWithOptions(t.Context(), base, PruneBodiesOptions{OlderThan: -time.Minute}); err == nil {
		t.Fatal("expected error for negative duration, got nil")
	}
}

func bodyCacheFixture(t *testing.T) (*Base, *sources.Document, sources.Record, string) {
	t.Helper()
	root := t.TempDir()
	base := &Base{
		Config: &core.Config{Sources: map[string]*core.Source{
			"meeting-notes":  {Name: "meeting-notes"},
			"another-source": {Name: "another-source"},
		}},
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
