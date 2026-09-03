// Package services implements everything fkf does with a base: collecting into it, deriving
// the graph over it, and reading a bounded slice of it back out.
//
// Every function here takes an open Base rather than a root path, so path resolution,
// layer activation, and confinement are applied once, in one place, instead of being
// re-derived — and occasionally forgotten — at each call site.
package services

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// Base is one opened base: its configuration, its resolved layout, the environment its
// declared commands run in, and the clock and runner the tests replace.
type Base struct {
	Config *core.Config
	Store  core.Store
	Env    sources.Environment
	Runner sources.Runner
	Now    func() time.Time
	Origin core.BaseOrigin
}

// Open discovers and loads a base. It never creates one: a mistyped `--base` that silently
// scaffolds an empty directory is how collected data ends up in the wrong place.
func Open(explicit string) (*Base, error) {
	root, origin, err := core.DiscoverBase(explicit)
	if err != nil {
		return nil, err
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	return &Base{
		Config: config, Store: config.Store(), Env: sources.NewEnvironment(config),
		Runner: sources.ExecRunner(), Now: time.Now, Origin: origin,
	}, nil
}

// Root is the base directory.
func (b *Base) Root() string { return b.Store.Root() }

// Source returns one declared source by name, with the fix named when it is absent.
func (b *Base) Source(name string) (*core.Source, error) {
	if source, ok := b.Config.Sources[name]; ok {
		return source, nil
	}
	declared := b.Config.SourceNames()
	if len(declared) == 0 {
		return nil, fmt.Errorf("%w: %s declares no sources; add one under `sources:` in %s",
			core.ErrConfig, b.Config.Name, b.Config.Path)
	}
	return nil, fmt.Errorf("%w: source %q is not declared in %s; declared sources are %s",
		core.ErrConfig, name, b.Config.Path, strings.Join(declared, ", "))
}

// RequireLayer refuses a request for a layer the base does not enable.
func (b *Base) RequireLayer(layer core.Layer) error {
	if !b.Store.Enabled(layer) {
		return core.ErrLayerDisabled{Layer: layer}
	}
	return nil
}

// ReadFileContext loads one bounded base-relative file with cooperative cancellation.
func (b *Base) ReadFileContext(ctx context.Context, relative string, limit int64) ([]byte, error) {
	absolute, err := b.Store.Resolve(relative)
	if err != nil {
		return nil, err
	}
	return core.ReadFileLimitContext(ctx, absolute, limit)
}

// ReadDocumentContext loads and verifies one stored document with cooperative cancellation.
func (b *Base) ReadDocumentContext(ctx context.Context, relative string) (*sources.Document, error) {
	absolute, err := b.Store.Resolve(relative)
	if err != nil {
		return nil, err
	}
	document, err := sources.ReadDocumentContext(ctx, absolute)
	if err != nil {
		return nil, err
	}
	requested := path.Clean(relative)
	if minted := document.URI(); requested != minted {
		return nil, fmt.Errorf("stored document %s mints %s from its source, layer, and date metadata; re-collect it",
			requested, minted)
	}
	if err := validateCompleteDocument(document); err != nil {
		return nil, fmt.Errorf("stored document %s is incomplete: %w", requested, err)
	}
	return document, nil
}

// WriteDocument files one complete document atomically.
func (b *Base) WriteDocument(document *sources.Document) error {
	if err := validateCompleteDocument(document); err != nil {
		return fmt.Errorf("document %s is incomplete: %w", document.URI(), err)
	}
	absolute, err := b.Store.Resolve(document.URI())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path.Dir(absolute), core.BaseDirMode); err != nil {
		return fmt.Errorf("create %s: %w", path.Dir(absolute), err)
	}
	return sources.WriteDocument(absolute, document)
}

func validateCompleteDocument(document *sources.Document) error {
	if document.Count != len(document.Records) {
		return fmt.Errorf("declares count %d but holds %d record(s)", document.Count, len(document.Records))
	}
	return sources.VerifyRecords(document)
}

// EventDates lists the days under events/ that hold at least one document, newest last. It is
// the window-first step every read starts from: listing dates is one cheap directory read,
// and only the surviving days are ever opened.
func (b *Base) EventDates() ([]string, error) {
	if err := b.RequireLayer(core.LayerEvents); err != nil {
		return nil, err
	}
	directory, err := b.Store.Dir(core.LayerEvents)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", directory, err)
	}
	dates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := time.Parse(time.DateOnly, entry.Name()); err != nil {
			continue
		}
		dates = append(dates, entry.Name())
	}
	sort.Strings(dates)
	return dates, nil
}

// DayDocuments lists the source documents filed for one day, in stable order.
func (b *Base) DayDocuments(date string) ([]string, error) {
	directory, err := b.Store.Resolve(path.Join(string(core.LayerEvents), date))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// IndexDocuments lists every collected point-in-time document under index/. Hidden files are
// reserved for rebuildable caches and never become source names or published evidence URIs.
func (b *Base) IndexDocuments() ([]string, error) {
	if err := b.RequireLayer(core.LayerIndex); err != nil {
		return nil, err
	}
	directory, err := b.Store.Dir(core.LayerIndex)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// RequireTrust is the gate before anything a base declares is executed. Read commands never
// call it, because they execute nothing and so need no trust.
func (b *Base) RequireTrust(ctx context.Context) error {
	return core.RequireTrust(ctx, b.Config)
}

// Timeout resolves the effective per-command timeout for one source.
func (b *Base) Timeout(source *core.Source) time.Duration {
	if source.Timeout > 0 {
		return source.Timeout
	}
	return b.Config.Sync.Timeout
}

// RunBody fetches one record's body through the source's current trusted argv command, while
// interpreting the historical record through the field map stored beside it.
func (b *Base) RunBody(
	ctx context.Context, source *core.Source, fields sources.Fields, record sources.Record,
) (string, error) {
	if !source.Enabled {
		return "", fmt.Errorf("source %s is disabled; enable and re-trust it before fetching a body", source.Name)
	}
	if err := b.RequireTrust(ctx); err != nil {
		return "", err
	}
	body, _, err := sources.FetchBody(ctx, b.Runner, source, fields, b.Env, record, b.Timeout(source))
	return body, err
}
