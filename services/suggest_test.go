package services_test

import (
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

// TestReadSuggestsTheClosestURIsItHolds pins the discoverability rule: a URI you have to
// already know is a URI you cannot find, so a read that names nothing answers with what the
// base does hold. Nothing new becomes addressable — the suggestions are the published grammar.
func TestReadSuggestsTheClosestURIsItHolds(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	trust(t, base)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\ntags: [decision]\n---\n\n# Retrieval boundary\n\n## Decision\n\nText.\n")
	collect(t, base, "2026-05-04", `[{"id":"a","t":"2026-05-04T09:00:00Z","subject":"one"}]`)

	cases := []struct{ name, argument, want string }{
		// A bare slug is outside the grammar, which is the commonest way to get this wrong.
		{"a bare slug", "retrieval-boundary", "wiki/retrieval-boundary.md"},
		// A well-formed URI for a file that is not there: the stem is what survives the typo.
		{"a near-miss path", "wiki/retrieval.md", "wiki/retrieval-boundary.md"},
		// A date names a day directory, which the events layer can offer.
		{"a bare date", "2026-05-04", "events/2026-05-04/"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := services.Read(t.Context(), base, test.argument, services.ReadOptions{})
			if err == nil {
				t.Fatalf("Read(%q) succeeded; the argument names nothing", test.argument)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Read(%q) error = %v, want it to suggest %s", test.argument, err, test.want)
			}
		})
	}
}

// TestReadSuggestsNothingForAnArgumentThatMatchesNothing keeps the suggestion honest: an error
// that always prints a list teaches a reader to ignore the list.
func TestReadSuggestsNothingForAnArgumentThatMatchesNothing(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	write(t, base, "wiki/retrieval-boundary.md", "---\ntype: decision\ntitle: Retrieval boundary\n---\n\n# Retrieval boundary\n")

	_, err := services.Read(t.Context(), base, "zzzznotathinganywhere", services.ReadOptions{})
	if err == nil {
		t.Fatal("Read() succeeded on an argument that names nothing")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("Read() error = %v, want no suggestion block when nothing is close", err)
	}
}

// TestSuggestionsNeverLeaveThePublishedGrammar: a suggestion is a URI the caller can paste back
// into `read`, so an unaddressable path must never appear in one.
func TestSuggestionsNeverLeaveThePublishedGrammar(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	write(t, base, "wiki/notes.md", "---\ntype: insight\ntitle: Notes\n---\n\n# Notes\n\n## !!!\n")

	for _, uri := range services.SuggestURIs(t.Context(), base, "notes") {
		if _, err := services.ParseURI(uri); err != nil {
			t.Fatalf("suggested %q, which is not a URI: %v", uri, err)
		}
		if _, err := base.Store.Resolve(strings.SplitN(uri, "#", 2)[0]); err != nil {
			t.Fatalf("suggested %q, which the store refuses: %v", uri, err)
		}
	}
}
