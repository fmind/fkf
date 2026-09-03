package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

type statusDocumentReader func(context.Context, string, int64) ([]byte, error)

type statusDocumentEntry struct {
	uri      string
	data     []byte
	document *sources.Document
	err      error
}

type statusDocuments struct {
	ordered []statusDocumentEntry
	byURI   map[string]*statusDocumentEntry
	known   map[string]GraphFileManifest
}

func loadStatusDocuments(
	ctx context.Context, base *Base, read statusDocumentReader,
) (*statusDocuments, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	uris, err := documentURIs(ctx, base)
	if err != nil {
		return nil, err
	}
	snapshot := &statusDocuments{
		ordered: make([]statusDocumentEntry, 0, len(uris)),
		byURI:   make(map[string]*statusDocumentEntry, len(uris)),
		known:   make(map[string]GraphFileManifest, len(uris)),
	}
	for _, uri := range uris {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		absolute, resolveErr := base.Store.Resolve(uri)
		before, statErr := os.Lstat(absolute)
		if resolveErr != nil {
			statErr = resolveErr
		}
		data, readErr := read(ctx, uri, core.MaxSourceDocumentBytes)
		entry := statusDocumentEntry{uri: uri, data: data, err: readErr}
		after, afterErr := os.Lstat(absolute)
		if entry.err == nil && statErr != nil {
			entry.err = statErr
		}
		if entry.err == nil && afterErr != nil {
			entry.err = afterErr
		}
		if entry.err == nil && (before.Size() != after.Size() || before.ModTime() != after.ModTime()) {
			entry.err = fmt.Errorf("stored document %s changed while status read it; retry", uri)
		}
		if entry.err == nil {
			digest := sha256.Sum256(data)
			snapshot.known[uri] = GraphFileManifest{
				URI: uri, Bytes: after.Size(), ModifiedUnixNano: after.ModTime().UnixNano(),
				SHA256: hex.EncodeToString(digest[:]),
			}
			entry.document, entry.err = decodeStatusDocument(ctx, uri, data)
		}
		snapshot.ordered = append(snapshot.ordered, entry)
	}
	for index := range snapshot.ordered {
		entry := &snapshot.ordered[index]
		snapshot.byURI[entry.uri] = entry
	}
	return snapshot, nil
}

func decodeStatusDocument(ctx context.Context, uri string, data []byte) (*sources.Document, error) {
	document, err := sources.DecodeDocumentContext(ctx, data, uri)
	if err != nil {
		return nil, err
	}
	requested := path.Clean(uri)
	if minted := document.URI(); requested != minted {
		return nil, fmt.Errorf("stored document %s mints %s from its source, layer, and date metadata; re-collect it",
			requested, minted)
	}
	if err := validateCompleteDocument(document); err != nil {
		return nil, fmt.Errorf("stored document %s is incomplete: %w", requested, err)
	}
	return document, nil
}

func (snapshot *statusDocuments) volumeHistory(ctx context.Context) (map[string][]dayVolume, error) {
	history := map[string][]dayVolume{}
	for _, entry := range snapshot.ordered {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(entry.uri, string(core.LayerEvents)+"/") || entry.err != nil {
			continue
		}
		lastCollected, err := eventCollectionBoundary(entry.document)
		if err != nil {
			return nil, fmt.Errorf("read collection boundary for %s: %w", entry.uri, err)
		}
		history[entry.document.Source] = append(history[entry.document.Source], dayVolume{
			date: entry.document.Date, count: entry.document.Count, lastCollected: lastCollected,
		})
	}
	for name := range history {
		sort.Slice(history[name], func(i, j int) bool { return history[name][i].date < history[name][j].date })
	}
	return history, nil
}

func (snapshot *statusDocuments) indexNames() []string {
	var names []string
	for _, entry := range snapshot.ordered {
		if strings.HasPrefix(entry.uri, string(core.LayerIndex)+"/") {
			name := strings.TrimSuffix(path.Base(entry.uri), ".json")
			names = append(names, name)
		}
	}
	return names
}

func (snapshot *statusDocuments) applyIndex(entry *SourceStatus) {
	stored := snapshot.byURI[sources.IndexDocumentURI(entry.Name)]
	if stored == nil || stored.err != nil {
		return
	}
	collected, err := time.Parse(time.RFC3339, stored.document.CollectedAt)
	if err != nil {
		return
	}
	entry.LastCount, entry.Days = stored.document.Count, 1
	entry.LastDate = collected.Local().Format(time.DateOnly)
	entry.lastCollected = collected
}

func (snapshot *statusDocuments) indexListing(base *Base, now time.Time) (*IndexListing, error) {
	listing := &IndexListing{Entries: []IndexEntry{}}
	maxAge := time.Duration(base.Config.Sync.IndexMaxAgeHours) * time.Hour
	for _, stored := range snapshot.ordered {
		if !strings.HasPrefix(stored.uri, string(core.LayerIndex)+"/") {
			continue
		}
		if stored.err != nil {
			// Integrity findings belong to checkDocuments. Keep the overview available so one
			// corrupt snapshot cannot suppress the rest of status or its actionable finding.
			continue
		}
		collected, err := time.Parse(time.RFC3339, stored.document.CollectedAt)
		if err != nil {
			return nil, fmt.Errorf("parse %s collected_at: %w", stored.uri, err)
		}
		age := now.Sub(collected)
		listing.Entries = append(listing.Entries, IndexEntry{
			Name: stored.document.Source, URI: stored.uri, Count: stored.document.Count,
			Bytes: int64(len(stored.data)), CollectedAt: stored.document.CollectedAt,
			AgeHours: max(0, int(age/time.Hour)), Stale: age < 0 || age >= maxAge,
		})
	}
	listing.Total = len(listing.Entries)
	return listing, nil
}

func (snapshot *statusDocuments) conflicted() []string {
	var uris []string
	for _, entry := range snapshot.ordered {
		if containsConflictMarker(entry.data) {
			uris = append(uris, entry.uri)
		}
	}
	return uris
}

func (snapshot *statusDocuments) verifyReport(base *Base) *VerifyReport {
	report := &VerifyReport{Base: base.Root(), Findings: []VerifyFinding{}}
	for _, entry := range snapshot.ordered {
		report.Documents++
		if entry.err != nil {
			report.Findings = append(report.Findings, VerifyFinding{URI: entry.uri, Problem: entry.err.Error()})
			continue
		}
		report.Records += entry.document.Count
	}
	report.OK = len(report.Findings) == 0
	return report
}
