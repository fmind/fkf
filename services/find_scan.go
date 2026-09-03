package services

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// findScanEngine owns the one exhaustive traversal used by ordinary and paginated find.
// Callers decide how many matches to retain; the engine always scans the complete admitted
// corpus so counters and continuation snapshots describe the same semantic result.
type findScanEngine struct {
	ctx      context.Context
	base     *Base
	filter   FindFilter
	counting bool
	result   *FindResult

	onPage   func(SearchHit) error
	onRecord func(FindRecord) error
	onVolume func(DayVolume) error
}

func prepareFindScan(ctx context.Context, base *Base, filter *FindFilter) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := validateFindRequest(base, *filter); err != nil {
		return nil, err
	}
	resolver, err := LoadIdentityResolver(ctx, base)
	if err != nil {
		return nil, err
	}
	filter.identity = resolver
	if filter.Bodies {
		filter.bodyManifest, err = loadBodyManifest(ctx, base)
		if err != nil {
			return nil, err
		}
	}
	filter.Grep = normalizeTerms(filter.Grep)
	if err := prepareFindLexicalIndex(ctx, base, filter); err != nil {
		return nil, err
	}
	return resolveEventDates(base, *filter)
}

func (scan *findScanEngine) scan(selected []string) error {
	if scan.filter.selects() && !scan.counting {
		if err := scan.scanPages(); err != nil {
			return err
		}
	}
	return scan.scanRecords(selected)
}

func (scan *findScanEngine) scanPages() error {
	terms := scan.filter.pageTerms()
	if len(terms) == 0 || scan.filter.recordOnly() {
		return nil
	}
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if !scan.filter.wants(layer) || !scan.base.Store.Enabled(layer) {
			continue
		}
		if err := scan.scanFlatPages(layer, terms); err != nil {
			return err
		}
	}
	if scan.filter.wants(core.LayerTasks) && scan.base.Store.Enabled(core.LayerTasks) {
		return scan.scanTaskPages(terms)
	}
	return nil
}

func (scan *findScanEngine) scanFlatPages(layer core.Layer, terms []string) error {
	directory, err := scan.base.Store.Dir(layer)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", directory, err)
	}
	for _, entry := range entries {
		if err := checkContext(scan.ctx); err != nil {
			return err
		}
		if entry.IsDir() || path.Ext(entry.Name()) != core.MarkdownExtension {
			continue
		}
		page, err := ReadPageContext(scan.ctx, scan.base, path.Join(string(layer), entry.Name()))
		if err != nil {
			return err
		}
		hit, matched := scorePage(page, terms)
		if !matched || !scan.filter.admitsCandidate(page.URI) {
			continue
		}
		hit.Layer = layer
		if err := scan.onPage(hit); err != nil {
			return err
		}
	}
	return nil
}

func (scan *findScanEngine) scanTaskPages(terms []string) error {
	directory, err := scan.base.Store.Dir(core.LayerTasks)
	if err != nil {
		return err
	}
	dates, err := readDateDirectories(directory)
	if err != nil {
		return err
	}
	for index := len(dates) - 1; index >= 0; index-- {
		date := dates[index]
		if !scan.filter.Window.Contains(date) {
			continue
		}
		slugs, err := readSubdirectories(path.Join(directory, date))
		if err != nil {
			return err
		}
		for _, slug := range slugs {
			if err := checkContext(scan.ctx); err != nil {
				return err
			}
			uri := path.Join(string(core.LayerTasks), date, slug, core.TaskTraceFile)
			if !scan.base.Exists(uri) {
				continue
			}
			page, err := ReadPageContext(scan.ctx, scan.base, uri)
			if err != nil {
				return err
			}
			hit, matched := scorePage(page, terms)
			if !matched || !scan.filter.admitsCandidate(page.URI) {
				continue
			}
			hit.Layer, hit.Date, hit.Slug = core.LayerTasks, date, slug
			if err := scan.onPage(hit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (scan *findScanEngine) scanRecords(selected []string) error {
	for index := len(selected) - 1; index >= 0; index-- {
		volume, err := scan.scanDay(selected[index])
		if err != nil {
			return err
		}
		if scan.counting && volume.Total > 0 {
			if err := scan.onVolume(volume); err != nil {
				return err
			}
		}
	}
	if !scan.filter.selects() || !scan.filter.scansIndex(scan.base) {
		return nil
	}
	volume, err := scan.scanIndex()
	if err != nil {
		return err
	}
	if scan.counting && volume.Total > 0 {
		return scan.onVolume(volume)
	}
	return nil
}

func (scan *findScanEngine) scanDay(date string) (DayVolume, error) {
	volume := DayVolume{Date: date, Sources: []SourceCount{}}
	names, err := scan.base.DayDocuments(date)
	if err != nil {
		return volume, err
	}
	for _, name := range names {
		if err := checkContext(scan.ctx); err != nil {
			return volume, err
		}
		if len(scan.filter.Sources) > 0 && !contains(scan.filter.Sources, name) {
			continue
		}
		matched, err := scan.scanDocument(sources.EventDocumentURI(date, name))
		if err != nil {
			return volume, err
		}
		if matched > 0 {
			volume.Total += matched
			volume.Sources = append(volume.Sources, SourceCount{Source: name, Count: matched})
		}
	}
	return volume, nil
}

func (scan *findScanEngine) scanIndex() (DayVolume, error) {
	volume := DayVolume{Sources: []SourceCount{}}
	names, err := scan.base.IndexDocuments()
	if err != nil {
		return volume, err
	}
	for _, name := range names {
		if err := checkContext(scan.ctx); err != nil {
			return volume, err
		}
		if len(scan.filter.Sources) > 0 && !contains(scan.filter.Sources, name) {
			continue
		}
		matched, err := scan.scanDocument(sources.IndexDocumentURI(name))
		if err != nil {
			return volume, err
		}
		if matched > 0 {
			volume.Total += matched
			volume.Sources = append(volume.Sources, SourceCount{Source: name, Count: matched})
		}
	}
	return volume, nil
}

func (scan *findScanEngine) scanDocument(uri string) (int, error) {
	document, err := scan.base.ReadDocumentContext(scan.ctx, uri)
	if err != nil {
		return 0, err
	}
	scan.result.Scanned += document.Count
	matched := 0
	for _, record := range document.Records {
		if err := checkContext(scan.ctx); err != nil {
			return 0, err
		}
		projected := project(document, record)
		if !scan.filter.admitsCandidate(projected.URI) {
			continue
		}
		body := ""
		if scan.filter.Bodies {
			var found bool
			body, _, found, err = readCachedBodyFromManifest(
				scan.ctx, scan.base, scan.filter.bodyManifest, projected.URI,
			)
			if err != nil {
				return 0, err
			}
			projected.BodyCached = found
		}
		if !matchesRecord(map[string]any(record), body, scan.filter) {
			continue
		}
		matched++
		scan.result.Matched++
		if !scan.counting {
			canonicalizeFindRecord(&projected, scan.filter.identity)
			if err := scan.onRecord(projected); err != nil {
				return 0, err
			}
		}
	}
	return matched, nil
}
