package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

// A URI you have to already know is a URI you cannot discover. Every listing prints one, but a
// half-remembered slug still refuses with nothing to go on, so a read that names nothing in the
// base answers with the closest URIs it does hold. Nothing new becomes addressable: the
// suggestions are the published grammar, printed back.

// maxSuggestions bounds what an error message may print, and maxSuggestionScan bounds the work
// done to produce it. This runs only after a read has already failed, so it must never be the
// slow part of finding out that you made a typo.
const (
	maxSuggestions    = 5
	maxSuggestionScan = 2000
)

// withSuggestions decorates an error that means "that is not here" with the near misses. Any
// other failure — a refused path, a disabled layer, a corrupt document — is returned untouched,
// because those are answers rather than misspellings.
func withSuggestions(ctx context.Context, base *Base, raw string, err error) error {
	if !namesNothing(err) {
		return err
	}
	matches := SuggestURIs(ctx, base, raw)
	if len(matches) == 0 {
		return err
	}
	return fmt.Errorf("%w\ndid you mean:\n  %s\n(every listing prints the URI to pass here)",
		err, strings.Join(matches, "\n  "))
}

// namesNothing is true for the two ways an argument can fail to name anything: it is not a URI
// at all, or it is a well-formed URI for a file the base does not have.
func namesNothing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, ErrURI) ||
		errors.Is(err, core.ErrNotAddressable)
}

// suggestion is one addressable URI that could be what was meant. `exact` marks the ones whose
// own last segment IS the word typed, which is almost always the answer.
type suggestion struct {
	uri   string
	exact bool
}

// SuggestURIs returns the addressable URIs closest to what was typed, best first. Matching is
// substring and case-insensitive on purpose: the failure it exists for is a remembered word,
// not a transposed character, and a fuzzy score would make the list harder to trust.
func SuggestURIs(ctx context.Context, base *Base, raw string) []string {
	needle := strings.ToLower(strings.TrimSpace(raw))
	if needle == "" {
		return nil
	}
	// The fragment is dropped before matching so `wiki/retrieval.md#gone` still finds the page.
	if trimmed, _, found := strings.Cut(needle, "#"); found {
		needle = trimmed
	}
	// Two needles, because the two things people actually type are a whole path with something
	// wrong in it and a bare remembered word. `wiki/retrieval.md` has to reach
	// `wiki/retrieval-boundary.md`, which only the stem does.
	found := collectSuggestions(ctx, base, needle, stemOf(needle))
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].exact != found[j].exact {
			return found[i].exact
		}
		if len(found[i].uri) != len(found[j].uri) {
			return len(found[i].uri) < len(found[j].uri)
		}
		return found[i].uri < found[j].uri
	})
	uris := make([]string, 0, maxSuggestions)
	for _, entry := range found[:min(maxSuggestions, len(found))] {
		uris = append(uris, entry.uri)
	}
	return uris
}

// collectSuggestions walks the addressable surface once, layer by layer, keeping whatever
// contains either needle.
func collectSuggestions(ctx context.Context, base *Base, needle, stem string) []suggestion {
	var found []suggestion
	consider := func(uri string) {
		if len(found) >= maxSuggestionScan {
			return
		}
		lower := strings.ToLower(uri)
		if !strings.Contains(lower, needle) && !strings.Contains(lower, stem) {
			return
		}
		found = append(found, suggestion{uri: uri, exact: stemOf(lower) == stem})
	}
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		considerPages(ctx, base, layer, consider)
	}
	if base.Store.Enabled(core.LayerIndex) {
		if names, err := base.IndexDocuments(); err == nil {
			for _, name := range names {
				consider("index/" + name + ".json")
			}
		}
	}
	if base.Store.Enabled(core.LayerEvents) {
		if dates, err := base.EventDates(); err == nil {
			for _, date := range dates {
				consider("events/" + date + "/")
			}
		}
	}
	return found
}

// considerPages offers a Markdown layer's pages and their headings. A load failure is skipped
// rather than reported: this whole path exists to improve an error that has already happened.
func considerPages(ctx context.Context, base *Base, layer core.Layer, consider func(string)) {
	if !base.Store.Enabled(layer) {
		return
	}
	pages, _, err := loadMarkdownLayer(ctx, base, layer)
	if err != nil {
		return
	}
	for _, page := range pages {
		consider(page.URI)
		for _, heading := range page.Headings {
			if AnchorSlug(heading.Text) == "" {
				continue
			}
			// The title heading usually slugs to the page's own name, and offering both
			// spells one destination twice.
			if heading.Anchor == stemOf(page.URI) {
				continue
			}
			consider(page.URI + "#" + heading.Anchor)
		}
	}
}

// stemOf is the last path segment without its extension or trailing slash: the part of a URI a
// person remembers and the part a typo usually leaves intact.
func stemOf(uri string) string {
	trimmed := strings.TrimSuffix(uri, "/")
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 {
		trimmed = trimmed[slash+1:]
	}
	if dot := strings.LastIndex(trimmed, "."); dot > 0 {
		trimmed = trimmed[:dot]
	}
	return trimmed
}
