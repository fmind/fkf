package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

const maxBodyFailureDetails = 3

// syncBodyDocumentDue makes `sync --if-due` notice a wiped sync-policy cache without executing
// or taking the writer lock during preflight. Index snapshots repair every missing current body.
// Event sources get one newest-document restore pass after a full prune, then an attempt marker
// prevents vanished historical resources from becoming perpetual sync work.
func syncBodyDocumentDue(ctx context.Context, base *Base, source *core.Source, uri string) (bool, error) {
	if source.Bodies != core.BodiesSync {
		return false, nil
	}
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return false, err
	}
	if source.Layer == core.LayerEvents {
		return eventBodyRestorePending(manifest, source.Name), nil
	}
	if source.Layer != core.LayerIndex {
		return false, nil
	}
	document, err := base.ReadDocumentContext(ctx, uri)
	if err != nil {
		return false, err
	}
	for _, record := range document.Records {
		current, err := bodyCacheCurrent(ctx, base, manifest, document, record)
		if err != nil {
			return false, err
		}
		if !current {
			return true, nil
		}
	}
	return false, nil
}

func bodyCacheCurrent(
	ctx context.Context,
	base *Base,
	manifest *BodyManifest,
	document *sources.Document,
	record sources.Record,
) (bool, error) {
	uri, ok := document.RecordURI(record)
	if !ok {
		return false, fmt.Errorf("document %s has a record without its declared identity", document.URI())
	}
	_, entry, found, err := readCachedBodyFromManifest(ctx, base, manifest, uri)
	if err != nil || !found {
		return false, err
	}
	return entry.ProviderModifiedAt == bodyProviderModifiedAt(document, record), nil
}

// syncRequestedBodies fills only missing or provider-modified sync-policy entries after the
// evidence run. Documents remain complete even when this rebuildable-cache phase reports a gap.
// A pruned cache restores the newest selected event document once; ordinary historical holes
// still require an explicit body read or re-collection.
func syncRequestedBodies(ctx context.Context, base *Base, report *SyncReport) error {
	auth := newAuthProbeCache(base)
	restore, err := eventBodyRestoreCandidates(ctx, base, report)
	if err != nil {
		return err
	}
	for index := range report.Units {
		unit := &report.Units[index]
		if err := syncUnitBodies(ctx, base, auth, unit, restore[unit.Source] == unit.URI); err != nil {
			return err
		}
	}
	return nil
}

func syncUnitBodies(
	ctx context.Context, base *Base, auth *authProbeCache, unit *SyncUnit, restoreEvent bool,
) error {
	// Written documents contain newly collected records. A fresh index document is the current
	// provider snapshot and may repair a wiped cache. Only the selected newest skipped event is
	// admitted after a complete prune.
	if unit.Outcome != OutcomeWritten && unit.Outcome != OutcomeFresh && !restoreEvent {
		return nil
	}
	source, err := base.Source(unit.Source)
	if err != nil || source.Bodies != core.BodiesSync {
		return err
	}
	document, err := base.ReadDocumentContext(ctx, unit.URI)
	if err != nil {
		return err
	}
	missing, err := missingCachedBodyRecords(ctx, base, document)
	if err != nil {
		return err
	}
	attemptEvent := source.Layer == core.LayerEvents && (unit.Outcome == OutcomeWritten || restoreEvent)
	if attemptEvent {
		// False arms one later retry for a new document. The retry closes the marker even on
		// failure, so a vanished provider object cannot become perpetual scheduled work.
		if err := markEventBodyAttempt(ctx, base, source.Name, false); err != nil {
			return err
		}
	}
	if len(missing) == 0 {
		if attemptEvent {
			return markEventBodyAttempt(ctx, base, source.Name, true)
		}
		return nil
	}
	ready, err := auth.ready(ctx, source)
	if err != nil {
		return err
	}
	if !ready {
		unit.BodyAuthRequired = true
		return nil
	}
	cacheMissingBodies(ctx, base, source, document, missing, unit)
	if attemptEvent && (unit.BodyFailures == 0 || restoreEvent) {
		return markEventBodyAttempt(ctx, base, source.Name, true)
	}
	return nil
}

func eventBodyRestoreCandidates(
	ctx context.Context, base *Base, report *SyncReport,
) (map[string]string, error) {
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]string)
	for index := range report.Units {
		unit := &report.Units[index]
		if unit.Kind != core.LayerEvents || unit.Outcome != OutcomeSkipped {
			continue
		}
		source, err := base.Source(unit.Source)
		if err != nil {
			return nil, err
		}
		if source.Bodies != core.BodiesSync || !eventBodyRestorePending(manifest, source.Name) {
			continue
		}
		if current := candidates[unit.Source]; current == "" || unit.URI > current {
			candidates[unit.Source] = unit.URI
		}
	}
	return candidates, nil
}

func eventBodyRestorePending(manifest *BodyManifest, source string) bool {
	if attempted, found := manifest.EventAttempts[source]; found {
		return !attempted
	}
	for _, entry := range manifest.Entries {
		if entry.Source == source {
			return false
		}
	}
	return true
}

func markEventBodyAttempt(ctx context.Context, base *Base, source string, attempted bool) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return err
	}
	manifest.EventAttempts[source] = attempted
	return writeBodyManifest(base, manifest)
}

func missingCachedBodyRecords(
	ctx context.Context, base *Base, document *sources.Document,
) ([]sources.Record, error) {
	manifest, err := loadBodyManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	missing := make([]sources.Record, 0)
	for _, record := range document.Records {
		current, err := bodyCacheCurrent(ctx, base, manifest, document, record)
		if err != nil {
			return nil, err
		}
		if !current {
			missing = append(missing, record)
		}
	}
	return missing, nil
}

func cacheMissingBodies(
	ctx context.Context, base *Base, source *core.Source, document *sources.Document,
	records []sources.Record, unit *SyncUnit,
) {
	diagnostics := make([]string, 0, maxBodyFailureDetails)
	for _, record := range records {
		uri, _ := document.RecordURI(record)
		body, err := base.RunBody(ctx, source, document.Fields, record)
		if err == nil {
			_, err = cacheBody(ctx, base, document, record, uri, body)
		}
		if err == nil {
			unit.BodiesCached++
			continue
		}
		unit.BodyFailures++
		if len(diagnostics) < maxBodyFailureDetails {
			diagnostics = append(diagnostics, fmt.Sprintf("%s: %v", uri, err))
		}
	}
	if unit.BodyFailures > 0 {
		unit.BodyError = fmt.Sprintf("%d body fetch(es) failed: %s", unit.BodyFailures, strings.Join(diagnostics, "; "))
	}
}
