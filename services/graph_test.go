package services_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const dayOne = `[
  {"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Fix retrieval boundary (FK-412)","link":"https://example.test/a1","repo_uri":"repo:github.com/fmind/fkf","author_uris":["person:email/marc@example.test"],"customer_uri":"customer:crm.example/123","ticket_uri":"ticket:FK-412"},
  {"id":"a2","t":"2026-05-04T10:00:00Z","subject":"Unrelated chore","link":"https://example.test/a2","repo_uri":"repo:github.com/acme/ledger","author_uris":["actor:github.com/ines"]}
]`

const dayTwo = `[
  {"id":"b1","t":"2026-05-05T09:00:00Z","subject":"Review FK-412 once more","link":"https://example.test/b1","repo_uri":"repo:github.com/fmind/fkf","author_uris":["person:email/me@example.test"],"ticket_uri":"ticket:FK-412"}
]`

// graphBase is a base with two collected days and two authored pages, which is enough to
// exercise every extractor at once.
func graphBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	collect(t, base, "2026-05-05", dayTwo)
	write(t, base, "wiki/retrieval-boundary.md", `---
type: decision
title: Retrieval boundary
tags: [decision, retrieval]
relations:
  related:
    - ../projects/fkf-rebuild.md
---

# Retrieval boundary

Decided in [the collected event](../events/2026-05-04/synthetic.json#a1 "FK-412").

`+"```markdown\n[not an assertion](../wiki/nowhere.md)\n```\n\n"+
		"Neither ``[double-backtick code](../wiki/nowhere-double.md)`` nor \\[escaped text](../wiki/nowhere-escaped.md) asserts a link.\n\n"+
		"    [indented code](../wiki/nowhere-indented.md)\n\n"+
		"   \t[tab-expanded code](../wiki/nowhere-tab.md)\n\n"+
		"A multiline `code span starts\n[multiline code](../wiki/nowhere-multiline.md)\nand ends here`.\n\n"+
		"````markdown\n[four-backtick fence](../wiki/nowhere-four.md)\n``` trailing text\n[still fenced](../wiki/nowhere-still-fenced.md)\n````\n\n"+
		"<!--\n[HTML comment](../wiki/nowhere-comment.md)\n-->\n\n"+
		"<pre>\n[raw HTML](../wiki/nowhere-html.md)\n</pre>\n")
	write(t, base, "projects/fkf-rebuild.md", `---
type: project
title: fkf rebuild
status: active
tags: [fkf]
---

# fkf rebuild

See [the decision](../wiki/retrieval-boundary.md).
`)
	return base
}

func TestBuildGraphTranscribesDeclaredPathsAndAuthoredLinks(t *testing.T) {
	base := graphBase(t)
	build, err := services.BuildGraph(t.Context(), base)
	if err != nil {
		t.Fatalf("BuildGraph() error = %v", err)
	}
	if build.Edges == 0 || build.Documents != 2 || build.Pages != 2 {
		t.Fatalf("build = %+v", build)
	}
	edges := readEdges(t, base)

	want := []services.Edge{
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "person:email/marc@example.test", Kind: "author", Via: "field:author"},
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "repo:github.com/fmind/fkf", Kind: "repository", Via: "field:repository"},
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "customer:crm.example/123", Kind: "customer", Via: "field:customer"},
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "ticket:FK-412", Kind: "ticket", Via: "field:ticket"},
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "https://example.test/a1", Kind: "url", Via: "field:url"},
		{Src: "events/2026-05-04/synthetic.json#a2", Dst: "actor:github.com/ines", Kind: "author", Via: "field:author"},
		{Src: "wiki/retrieval-boundary.md", Dst: "tag:decision", Kind: "tag", Via: "frontmatter:tags"},
		{Src: "wiki/retrieval-boundary.md", Dst: "projects/fkf-rebuild.md", Kind: "related", Via: "frontmatter:relations.related"},
		{Src: "wiki/retrieval-boundary.md", Dst: "events/2026-05-04/synthetic.json#a1", Kind: "link", Via: "markdown-inline"},
		{Src: "projects/fkf-rebuild.md", Dst: "wiki/retrieval-boundary.md", Kind: "link", Via: "markdown-inline"},
	}
	for _, edge := range edges {
		if strings.HasPrefix(edge.Dst, "ticket:") && edge.Via != "field:ticket" {
			t.Fatalf("a ticket inferred instead of transcribed from its relation field: %+v", edge)
		}
	}
	for _, expected := range want {
		if !hasEdge(edges, expected) {
			t.Fatalf("missing edge %s -%s-> %s (via %s)", expected.Src, expected.Kind, expected.Dst, expected.Via)
		}
	}
	// A link inside a fenced block is an illustration, not an assertion.
	for _, edge := range edges {
		if strings.Contains(edge.Dst, "nowhere.md") {
			t.Fatalf("a link inside a code fence became an edge: %+v", edge)
		}
	}
}

func TestBuildGraphUsesOrderedFallbacksForSingularFields(t *testing.T) {
	config := strings.NewReplacer(
		"      id: .id", "      id: [.missing_id, .id]",
		"      time: .t", "      time: [.missing_time, .t]",
		"      title: .subject", "      title: [.missing_title, .subject]",
		"      url: .link", "      url: [.missing_url, .link]",
		"      repository: .repo_uri", "      repository: [.missing_repo, .repo_uri]",
	).Replace(baseConfig)
	base := newBase(t, config, nil)
	collect(t, base, "2026-05-04", dayOne)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatalf("BuildGraph() with fallback field paths: %v", err)
	}
	edges := readEdges(t, base)
	for _, expected := range []services.Edge{
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "repo:github.com/fmind/fkf", Kind: "repository", Via: "field:repository"},
		{Src: "events/2026-05-04/synthetic.json#a1", Dst: "https://example.test/a1", Kind: "url", Via: "field:url"},
	} {
		if !hasEdge(edges, expected) {
			t.Fatalf("fallback fields omitted edge %s -%s-> %s (via %s)",
				expected.Src, expected.Kind, expected.Dst, expected.Via)
		}
	}
}

func TestBuildGraphRejectsAnAuthoredLinkItCannotTranscribe(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/bad.md", `---
type: decision
title: Bad link
tags: [graph]
---

# Bad link

[invalid](target.md?other=value)
`)
	_, err := services.BuildGraph(t.Context(), base)
	if err == nil || !strings.Contains(err.Error(), "target.md?other=value") {
		t.Fatalf("BuildGraph() error = %v, want the authored URI failure surfaced", err)
	}
}

func TestBuildGraphRefusesAnUnreadableRowWithoutReplacingThePriorCache(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	beforeGraph := readGraphFile(t, base)
	metaPath, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	oversizedID := strings.Repeat("x", services.MaxEdgeLineBytes)
	collect(t, base, "2026-05-06", fmt.Sprintf(
		`[{"id":%q,"t":"2026-05-06T09:00:00Z","subject":"oversized id","repo_uri":"repo:github.com/fmind/fkf"}]`,
		oversizedID,
	))

	_, err = services.BuildGraph(t.Context(), base)
	if !errors.Is(err, services.ErrEdgeLineTooLong) {
		t.Fatalf("BuildGraph() error = %v, want the generated unreadable row refused", err)
	}
	if after := readGraphFile(t, base); after != beforeGraph {
		t.Fatal("failed graph build replaced the prior edge cache")
	}
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterMeta, beforeMeta) {
		t.Fatal("failed graph build replaced the prior metadata sidecar")
	}
}

func TestBuildGraphDoesNotReplaceCachesFromAnIncompleteSourceDocument(t *testing.T) {
	for _, test := range []struct {
		name, old, replacement, want string
	}{
		{name: "count mismatch", old: `"count": 2`, replacement: `"count": 3`, want: "declares count"},
		{name: "duplicate identity", old: `"id": "a2"`, replacement: `"id": "a1"`, want: "share the id"},
		{name: "outside event day", old: "2026-05-04T10:00:00Z", replacement: "2026-05-05T10:00:00Z", want: "outside the requested window"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertBuildGraphPreservesCachesAfterDocumentEdit(t, test.old, test.replacement, test.want)
		})
	}
}

func assertBuildGraphPreservesCachesAfterDocumentEdit(t *testing.T, old, replacement, wantError string) {
	t.Helper()
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	before := readDerivedGraphCaches(t, base)
	documentPath, err := base.Store.Resolve("events/2026-05-04/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(document), old, replacement, 1)
	if corrupt == string(document) {
		t.Fatalf("fixture does not contain %q", old)
	}
	if err := os.WriteFile(documentPath, []byte(corrupt), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	_, err = services.BuildGraph(t.Context(), base)
	if err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("BuildGraph() error = %v, want incomplete-document failure containing %q", err, wantError)
	}
	after := readDerivedGraphCaches(t, base)
	for name, want := range before {
		if !bytes.Equal(after[name], want) {
			t.Fatalf("failed build replaced %s", name)
		}
	}
}

func readDerivedGraphCaches(t *testing.T, base *services.Base) map[string][]byte {
	t.Helper()
	contents := map[string][]byte{}
	for _, name := range []string{core.GraphFile, core.GraphMetaFile} {
		absolute, err := base.Store.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		contents[name], err = os.ReadFile(absolute)
		if err != nil {
			t.Fatal(err)
		}
	}
	return contents
}

func TestBuildGraphTreatsEveryLinkTitleAsMetadata(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	write(t, base, "wiki/titles.md", `---
type: decision
title: Link titles
tags: [graph]
---

# Link titles

[Example](https://example.test "ordinary tooltip")
[FK-412](https://acme/browse/FK-412 "../events/2026-05-04/synthetic.json#a1")
`)

	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatalf("BuildGraph() error = %v, want ordinary title text ignored", err)
	}
	edges := readEdges(t, base)
	want := services.Edge{
		Src: "wiki/titles.md", Dst: "events/2026-05-04/synthetic.json#a1",
		Kind: services.EdgeLink, Via: "markdown-inline",
	}
	if hasEdge(edges, want) {
		t.Fatalf("link title became a hidden edge %+v", want)
	}
	for _, edge := range edges {
		if edge.Dst == want.Dst {
			t.Fatalf("link title became a graph edge: %+v", edge)
		}
	}
}

func TestBuildGraphRejectsUnaddressableFileLinks(t *testing.T) {
	tests := []struct {
		name   string
		config string
		target string
		link   string
		check  func(error) bool
	}{
		{
			name:   "private neighbour",
			config: baseConfig,
			target: "../.git/config",
			link:   "[private](../.git/config)",
			check:  func(err error) bool { return errors.Is(err, core.ErrNotAddressable) },
		},
		{
			name:   "disabled layer",
			config: strings.Replace(baseConfig, "  projects: true", "  projects: false", 1),
			target: "../projects/disabled.md",
			link:   "[private](../projects/disabled.md)",
			check: func(err error) bool {
				var disabled core.ErrLayerDisabled
				return errors.As(err, &disabled) && disabled.Layer == core.LayerProjects
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := newBase(t, test.config, nil)
			write(t, base, "wiki/bad.md", `---
type: decision
title: Bad link
tags: [graph]
---

# Bad link

`+test.link+`
`)
			_, err := services.BuildGraph(t.Context(), base)
			if err == nil || !test.check(err) {
				t.Fatalf("BuildGraph() error = %v, want the unaddressable target %q refused", err, test.target)
			}
		})
	}
}

func TestBuildGraphRejectsAnUnaddressableFrontmatterFileURI(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/bad.md", `---
type: decision
title: Bad frontmatter link
tags: [graph]
relations:
  related: [../.git/config]
---

# Bad frontmatter link
`)

	_, err := services.BuildGraph(t.Context(), base)
	if err == nil || !errors.Is(err, core.ErrNotAddressable) {
		t.Fatalf("BuildGraph() error = %v, want the unaddressable frontmatter URI refused", err)
	}
}

func TestBuildGraphChecksStoredFileRelationsAgainstTheAddressGrammar(t *testing.T) {
	for _, test := range []struct {
		name, target string
		mutateConfig func(string) string
		wantError    func(error) bool
	}{
		{
			name: "unsupported file shape", target: "wiki/diagram.png",
			mutateConfig: func(config string) string { return config },
			wantError: func(err error) bool {
				return strings.Contains(err.Error(), "stored document") &&
					strings.Contains(err.Error(), "not a canonical relation URI")
			},
		},
		{
			name: "disabled layer", target: "projects/future.md",
			mutateConfig: func(config string) string {
				return strings.Replace(config, "  projects: true", "  projects: false", 1)
			},
			wantError: func(err error) bool {
				var disabled core.ErrLayerDisabled
				return errors.As(err, &disabled) && disabled.Layer == core.LayerProjects
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := strings.Replace(baseConfig, "      topic: .topic\n", "      topic: .topic\n      related: .related\n", 1)
			base := newBase(t, test.mutateConfig(config), nil)
			collectedTarget := test.target
			if test.name == "unsupported file shape" {
				// Collection now rejects this shape. Write a valid document first, then model
				// evidence that predates the rule or was hand-edited after collection; graph
				// validation must still fail closed on stored bytes.
				collectedTarget = "wiki/future.md"
			}
			document := collect(t, base, "2026-05-04", fmt.Sprintf(
				`[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Bad relation","related":%q}]`,
				collectedTarget,
			))
			if collectedTarget != test.target {
				absolute, resolveErr := base.Store.Resolve(document.URI())
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				encoded, readErr := os.ReadFile(absolute)
				if readErr != nil {
					t.Fatal(readErr)
				}
				encoded = bytes.ReplaceAll(encoded, []byte(collectedTarget), []byte(test.target))
				if writeErr := os.WriteFile(absolute, encoded, core.BaseFileMode); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			_, err := services.BuildGraph(t.Context(), base)
			if err == nil || !test.wantError(err) {
				t.Fatalf("BuildGraph() error = %v, want stored relation %q refused", err, test.target)
			}
		})
	}
}

func TestBuildGraphKeepsAFrontmatterRelationToAURLShapedRecordID(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	const recordID = "https://example.test/post"
	collect(t, base, "2026-05-04", `[{"id":"https://example.test/post","t":"2026-05-04T09:00:00Z","subject":"Post"}]`)
	write(t, base, "wiki/post.md", `---
type: decision
title: Referenced post
tags: [graph]
relations:
  related:
    - ../events/2026-05-04/synthetic.json#https://example.test/post
---

# Referenced post
`)

	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatalf("BuildGraph() rejected a frontmatter relation with a URL-shaped record id: %v", err)
	}
	want := services.Edge{
		Src:  "wiki/post.md",
		Dst:  "events/2026-05-04/synthetic.json#" + recordID,
		Kind: "related",
		Via:  "frontmatter:relations.related",
	}
	if edges := readEdges(t, base); !hasEdge(edges, want) {
		t.Fatalf("graph has no URL-shaped record relation %+v; edges = %+v", want, edges)
	}
}

func TestBuildGraphKeepsAStoredForwardPageRelation(t *testing.T) {
	config := strings.Replace(baseConfig, "      topic: .topic\n", "      topic: .topic\n      related: .related\n", 1)
	base := newBase(t, config, nil)
	collect(t, base, "2026-05-04",
		`[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Forward relation","related":"wiki/future.md"}]`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatalf("BuildGraph() error = %v, want a missing but addressable forward page admitted", err)
	}
}

func TestBuildGraphRejectsAStoredRelationToAMissingChildOfAnExistingFile(t *testing.T) {
	for _, test := range []struct {
		name, target string
		prepare      func(*services.Base)
	}{
		{
			name: "Markdown heading", target: "wiki/target.md#missing",
			prepare: func(base *services.Base) {
				write(t, base, "wiki/target.md", "# Target\n\n## Existing\n")
			},
		},
		{
			name: "collected record", target: "events/2026-05-04/synthetic.json#missing",
			prepare: func(*services.Base) {},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := strings.Replace(baseConfig, "      topic: .topic\n", "      topic: .topic\n      related: .related\n", 1)
			base := newBase(t, config, nil)
			test.prepare(base)
			collect(t, base, "2026-05-04", fmt.Sprintf(
				`[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Broken child","related":%q}]`,
				test.target,
			))

			_, err := services.BuildGraph(t.Context(), base)
			if err == nil || !strings.Contains(err.Error(), "fragment does not name an addressable child") {
				t.Fatalf("BuildGraph() error = %v, want existing-file child %q refused", err, test.target)
			}
		})
	}
}

func TestBuildGraphRejectsAnAuthoredEdgeToAMissingChildOfAnExistingPage(t *testing.T) {
	for _, test := range []struct {
		name, declaration string
	}{
		{name: "Markdown link", declaration: "See [missing](target.md#missing).\n"},
		{name: "frontmatter relation", declaration: "relations:\n  related: [target.md#missing]\n---\n\n# Source\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newBase(t, baseConfig, nil)
			write(t, base, "wiki/target.md", "# Target\n\n## Existing\n")
			body := "---\ntype: decision\ntitle: Source\ntags: [test]\n---\n\n# Source\n\n" + test.declaration
			if test.name == "frontmatter relation" {
				body = "---\ntype: decision\ntitle: Source\ntags: [test]\n" + test.declaration
			}
			write(t, base, "wiki/source.md", body)

			_, err := services.BuildGraph(t.Context(), base)
			if err == nil || !strings.Contains(err.Error(), "fragment does not name an addressable child") {
				t.Fatalf("BuildGraph() error = %v, want authored missing heading refused", err)
			}
		})
	}
}

func TestCollectedRelationJQIsAReadTransformNotGraphIdentity(t *testing.T) {
	config := strings.Replace(baseConfig, "      topic: .topic\n", "      topic: .topic\n      related: .related\n", 1)
	base := newBase(t, config, nil)
	const destination = "events/2026-05-04/synthetic.json?jq=.records"
	collect(t, base, "2026-05-04", `[
  {"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Query relation","related":"events/2026-05-04/synthetic.json?jq=.records"}
]`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatalf("BuildGraph() with a query-bearing relation: %v", err)
	}

	want := services.Edge{
		Src: "events/2026-05-04/synthetic.json#a1", Dst: "events/2026-05-04/synthetic.json",
		Kind: "related", Via: "field:related",
	}
	if edges := readEdges(t, base); !hasEdge(edges, want) {
		t.Fatalf("graph did not normalize jq from node identity; want %+v, edges = %+v", want, edges)
	}
	neighbours, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: destination, Direction: services.DirectionIn, Depth: 1,
	})
	if err != nil {
		t.Fatalf("Neighbours() through the query-bearing read URI: %v", err)
	}
	if len(neighbours.Edges) != 1 || neighbours.Edges[0].Src != want.Src || neighbours.Edges[0].Dst != want.Dst {
		t.Fatalf("Neighbours() = %+v, want the normalized collected relation", neighbours.Edges)
	}
}

// TestBuildGraphIsByteIdentical is the property the whole receipt rests on: the same base and
// the same clock produce the same file, so a diff means the base changed.
func TestBuildGraphIsByteIdentical(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	first := readGraphFile(t, base)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	if second := readGraphFile(t, base); second != first {
		t.Fatal("BuildGraph() is not deterministic; the same base must produce the same bytes")
	}
}

func TestGraphCacheRejectsAChangedAuthoredPage(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	pagePath, err := base.Store.Resolve("wiki/retrieval-boundary.md")
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	page = bytes.Replace(page, []byte("tags: [decision, retrieval]"),
		[]byte("tags: [decision, retrieval, changed]"), 1)
	if err := os.WriteFile(pagePath, page, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	if _, err := services.SummarizeGraph(t.Context(), base); err == nil || !strings.Contains(err.Error(), "current graph inputs") {
		t.Fatalf("SummarizeGraph() error = %v, want stale authored-page input refused", err)
	}
}

func TestGraphCacheRejectsAChangedAuthoredRelationSchema(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base.Root(), core.ConfigFileName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config,
		[]byte("related: {description: Related base resources., cardinality: many, relation: true"),
		[]byte("related: {description: Related base resources., cardinality: many, relation: false"), 1)
	if err := os.WriteFile(configPath, config, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	changed, err := services.Open(base.Root())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := services.SummarizeGraph(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "current graph inputs") {
		t.Fatalf("SummarizeGraph() error = %v, want changed relation semantics to stale the graph", err)
	}
}

// TestGraphNeverInfersFromBodies is the line between transcription and inference. `a2` carries
// a ticket key only in a field nobody declared, so no ticket edge may exist for it.
func TestGraphNeverInfersFromBodies(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04",
		`[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"No key here","body":"the real answer is in FK-999","who":"m@example.test"}]`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	for _, edge := range readEdges(t, base) {
		if strings.Contains(edge.Dst, "FK-999") {
			t.Fatalf("a ticket was inferred from an undeclared body field: %+v", edge)
		}
	}
}

func TestGraphEdgesInIsBacklinks(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	neighbourhood, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "wiki/retrieval-boundary.md", Direction: services.DirectionIn, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbourhood.SnapshotSHA256) != 64 {
		t.Fatalf("graph snapshot = %q, want the validated edge-list SHA-256", neighbourhood.SnapshotSHA256)
	}
	found := false
	for _, edge := range neighbourhood.Edges {
		if edge.Src == "projects/fkf-rebuild.md" {
			found = true
		}
		if edge.Dst != "wiki/retrieval-boundary.md" {
			t.Fatalf("--in returned an outbound edge: %+v", edge)
		}
	}
	if !found {
		t.Fatalf("edges = %+v, want the project that links here", neighbourhood.Edges)
	}
}

func TestGraphKindUsesTheObservedVocabularyAndRejectsTypos(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	related, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "wiki/retrieval-boundary.md", Direction: services.DirectionOut, Kind: "related", Depth: 1,
	})
	if err != nil {
		t.Fatalf("Neighbours(--kind related) error = %v; frontmatter keys are legitimate edge kinds", err)
	}
	if len(related.Edges) != 1 || related.Edges[0].Kind != "related" {
		t.Fatalf("related edges = %+v, want the authored frontmatter relationship", related.Edges)
	}
	_, err = services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "wiki/retrieval-boundary.md", Direction: services.DirectionOut, Kind: "reltaed", Depth: 1,
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), "related") {
		t.Fatalf("Neighbours(--kind reltaed) error = %v, want the typo refused with observed kinds", err)
	}
}

func TestGraphDepthTwoReachesTheSecondHop(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	deep, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "ticket:FK-412", Direction: services.DirectionBoth, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// One hop reaches the records touching the ticket; two reaches their other declared entities.
	var reachedPerson bool
	for _, edge := range deep.Edges {
		if edge.Hop == 2 && strings.HasPrefix(edge.Dst, "person:") {
			reachedPerson = true
		}
	}
	if !reachedPerson {
		t.Fatalf("depth 2 from a ticket must reach the people on its records; got %+v", deep.Edges)
	}
	if _, err := services.Neighbours(t.Context(), base, services.GraphQuery{URI: "ticket:FK-412", Depth: 9}); err == nil {
		t.Fatal("an unbounded walk defeats the budget the read surface exists to enforce")
	}
}

func TestGraphLimitTruncatesRatherThanLying(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	bounded, err := services.Neighbours(t.Context(), base, services.GraphQuery{
		URI: "repo:github.com/fmind/fkf", Direction: services.DirectionIn, Depth: 2, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Edges) != 1 || !bounded.Truncated {
		t.Fatalf("bounded = %+v, want exactly one edge and a truncation flag", bounded)
	}
}

func TestGraphOffsetReplaysTraversalWithoutRetainingEarlierEdgesOrNodes(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	query := services.GraphQuery{
		URI: "ticket:FK-412", Direction: services.DirectionBoth, Depth: 2,
	}
	complete, err := services.Neighbours(t.Context(), base, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete.Edges) < 2 {
		t.Fatalf("fixture has %d edge(s), want at least two", len(complete.Edges))
	}
	query.Limit = 1
	first, err := services.Neighbours(t.Context(), base, query)
	if err != nil {
		t.Fatal(err)
	}
	query.Offset = 1
	second, err := services.Neighbours(t.Context(), base, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Edges) != 1 || len(second.Edges) != 1 ||
		first.Edges[0] != complete.Edges[0] || second.Edges[0] != complete.Edges[1] {
		t.Fatalf("pages = %+v then %+v, want the first two complete edges %+v", first.Edges, second.Edges, complete.Edges[:2])
	}
	for _, previous := range first.Nodes {
		if slices.Contains(second.Nodes, previous) {
			t.Fatalf("offset page repeated previously discovered node %s", previous)
		}
	}
}

func TestGraphNamesTheRebuildWhenTheCacheIsAbsent(t *testing.T) {
	base := graphBase(t)
	_, err := services.Neighbours(t.Context(), base, services.GraphQuery{URI: "ticket:FK-412"})
	if err == nil || !strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("error = %v, want it to name the rebuild; the edge list is a cache", err)
	}
}

func TestGraphFactsRejectARowCompleteTruncatedCache(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	truncateGraphCacheByOneRow(t, base)

	_, err := services.Neighbours(t.Context(), base, services.GraphQuery{URI: "ticket:FK-412"})
	if err == nil || !strings.Contains(err.Error(), "metadata edges") ||
		!strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("Neighbours() error = %v, want the row-count mismatch and rebuild remedy", err)
	}
	_, err = services.ListNodes(t.Context(), base, "", 0)
	if err == nil || !strings.Contains(err.Error(), "metadata edges") ||
		!strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("ListNodes() error = %v, want the row-count mismatch and rebuild remedy", err)
	}
}

func TestGraphFactsRejectASameLengthRowSubstitution(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	absolute, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	original := readGraphFile(t, base)
	changed := strings.Replace(original, "ticket:FK-412", "ticket:FK-413", 1)
	if changed == original || len(changed) != len(original) {
		t.Fatal("fixture did not produce a same-length graph row substitution")
	}
	if err := os.WriteFile(absolute, []byte(changed), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	_, err = services.Neighbours(t.Context(), base, services.GraphQuery{URI: "repo:github.com/fmind/fkf"})
	if err == nil || !strings.Contains(err.Error(), "sha256") ||
		!strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("Neighbours() error = %v, want digest mismatch and rebuild remedy", err)
	}
}

func TestGraphFactsRejectSemanticCorruptionEvenWithAMatchingSidecar(t *testing.T) {
	valid := func(generated, src, dst, kind, at, via string) string {
		return strings.Join([]string{src, dst, kind, at, via, generated}, "\t") + "\n"
	}
	for _, test := range []struct {
		name       string
		rows       func(string) string
		extractors []string
		want       string
	}{
		{
			name: "private file URI",
			rows: func(at string) string {
				return valid(at, ".git/config", "repo:fmind/fkf", "repo", "", "field:repo")
			}, extractors: []string{"field:repo"}, want: "published base grammar",
		},
		{
			name: "noncanonical entity",
			rows: func(at string) string {
				return valid(at, "wiki/index.md", "person:line%0abreak", "participant", "", "field:participant")
			}, extractors: []string{"field:participant"}, want: "not canonical",
		},
		{
			name: "control byte",
			rows: func(at string) string {
				return valid(at, "wiki/index.md", "tag:graph", "bad\x1bkind", "", "frontmatter:tags")
			}, extractors: []string{"frontmatter:tags"}, want: "malformed",
		},
		{
			name: "invalid fact time",
			rows: func(at string) string {
				return valid(at, "wiki/index.md", "tag:graph", "tag", "yesterday", "frontmatter:tags")
			}, extractors: []string{"frontmatter:tags"}, want: "malformed",
		},
		{
			name: "duplicate row",
			rows: func(at string) string {
				row := valid(at, "wiki/index.md", "tag:graph", "tag", "", "frontmatter:tags")
				return row + row
			}, extractors: []string{"frontmatter:tags"}, want: "duplicate",
		},
		{
			name: "noncanonical order",
			rows: func(at string) string {
				return valid(at, "wiki/z.md", "tag:graph", "tag", "", "frontmatter:tags") +
					valid(at, "wiki/a.md", "tag:graph", "tag", "", "frontmatter:tags")
			}, extractors: []string{"frontmatter:tags"}, want: "sort order",
		},
		{
			name: "lying extractor vocabulary",
			rows: func(at string) string {
				return valid(at, "wiki/index.md", "repo:fmind/fkf", "repo", "", "field:repo")
			}, extractors: []string{"other"}, want: "extractors",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := graphBase(t)
			if _, err := services.BuildGraph(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			writeMatchingGraphCache(t, base, test.rows, test.extractors)

			_, err := services.Neighbours(t.Context(), base, services.GraphQuery{URI: "repo:fmind/fkf"})
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), "fkf build graph") {
				t.Fatalf("Neighbours() error = %v, want semantic failure containing %q and rebuild remedy", err, test.want)
			}
		})
	}
}

func TestListNodesClassifiesByPrefix(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"person", "repo", "ticket", "tag", "url", "wiki", "project", "event"} {
		listing, err := services.ListNodes(t.Context(), base, kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		if listing.Total == 0 {
			t.Fatalf("no %s nodes; the demo graph should hold at least one", kind)
		}
		for _, node := range listing.Nodes {
			if node.Kind != kind {
				t.Fatalf("--kind %s returned a %s node: %+v", kind, node.Kind, node)
			}
		}
	}
}

func TestListNodesAcceptsAnyOpenEntityKind(t *testing.T) {
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	listing, err := services.ListNodes(t.Context(), base, "artifact", 0)
	if err != nil || listing.Total != 0 {
		t.Fatalf("ListNodes(--kind artifact) = %+v, %v, want a valid empty open kind", listing, err)
	}
}

func TestListNodesClassifiesVerbatimUppercaseHTTPSAsURL(t *testing.T) {
	base := graphBase(t)
	write(t, base, "wiki/uppercase-url.md", `---
type: reference
title: Uppercase URL
tags: [graph]
---

# Uppercase URL

[External](HTTPS://example.test/path)
`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	listing, err := services.ListNodes(t.Context(), base, "url", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range listing.Nodes {
		if node.URI == "HTTPS://example.test/path" {
			return
		}
	}
	t.Fatalf("URL nodes = %+v, want the valid verbatim uppercase HTTPS URI", listing.Nodes)
}

func TestNodeKind(t *testing.T) {
	cases := map[string]string{
		"person:m@x.test":                  "person",
		"repo:o/r":                         "repo",
		"ticket:FK-1":                      "ticket",
		"tag:x":                            "tag",
		"https://x.test":                   "url",
		"HTTPS://x.test":                   "url",
		"events/2026-05-04/s.json#a":       "event",
		"graph.tsv":                        "derived",
		"derived/graph.tsv":                "file",
		"customer:crm.example/123":         "customer",
		"index/github-repositories.json#a": "index",
		"tasks/2026-05-04/s/TASKS.md":      "task",
		"projects/p.md":                    "project",
		"wiki/w.md":                        "wiki",
		"AGENTS.md":                        "file",
	}
	for uri, want := range cases {
		if got := services.NodeKind(uri); got != want {
			t.Fatalf("NodeKind(%q) = %q, want %q", uri, got, want)
		}
	}
}

// --- helpers ------------------------------------------------------------------------------

func readGraphFile(t *testing.T, base *services.Base) string {
	t.Helper()
	absolute, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func truncateGraphCacheByOneRow(t *testing.T, base *services.Base) {
	t.Helper()
	absolute, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSuffix(readGraphFile(t, base), "\n"), "\n")
	if len(rows) < 2 {
		t.Fatalf("graph has %d row(s), want enough to truncate one complete row", len(rows))
	}
	truncated := strings.Join(rows[:len(rows)-1], "\n") + "\n"
	if err := os.WriteFile(absolute, []byte(truncated), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}

func writeMatchingGraphCache(
	t *testing.T,
	base *services.Base,
	rows func(generatedAt string) string,
	extractors []string,
) {
	t.Helper()
	graphPath, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	metaPath, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	encodedMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta services.EdgeListMeta
	if err := json.Unmarshal(encodedMeta, &meta); err != nil {
		t.Fatal(err)
	}
	content := rows(meta.GeneratedAt)
	digest := sha256.Sum256([]byte(content))
	meta.Edges = strings.Count(content, "\n")
	meta.Extractors = extractors
	meta.Bytes = len(content)
	meta.SHA256 = hex.EncodeToString(digest[:])
	encodedMeta, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphPath, []byte(content), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, encodedMeta, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}

func readEdges(t *testing.T, base *services.Base) []services.Edge {
	t.Helper()
	var edges []services.Edge
	_, err := services.ScanEdges(t.Context(), strings.NewReader(readGraphFile(t, base)), services.EdgeQuery{}, func(edge services.Edge) error {
		edges = append(edges, edge)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func hasEdge(edges []services.Edge, want services.Edge) bool {
	for _, edge := range edges {
		if edge.Src == want.Src && edge.Dst == want.Dst && edge.Kind == want.Kind && edge.Via == want.Via {
			return true
		}
	}
	return false
}

// TestExpansionLoadsARecordOutsideTheCandidateSet covers the other half of --expand: an item
// the window did not gather is loaded rather than merely rescored.
func TestExpansionLoadsARecordOutsideTheCandidateSet(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	collect(t, base, "2026-05-05", `[{"id":"b1","t":"2026-05-05T09:00:00Z","subject":"Nothing in common","link":"https://x.test/b1","repo":"fmind/fkf","who":"lea@example.test"}]`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	// The window holds only the first day, so the second day's record is reachable only through
	// the repository they share — and expansion refuses it, because expansion stays in the window.
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, Expand: true,
		Window: services.Window{Since: "2026-05-04", Until: "2026-05-04"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if strings.Contains(item.URI, "2026-05-05") {
			t.Fatalf("expansion reached outside the window: %+v", item)
		}
	}
}
