package services_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func wikiBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\ndescription: Why retrieval is lexical.\ntags: [retrieval, decision]\n---\n\n# Retrieval boundary\n")
	write(t, base, "wiki/declarative-sources.md",
		"---\ntype: pattern\ntitle: Declarative sources\ntags: [collection]\n---\n\n# Declarative sources\n")
	write(t, base, "wiki/log.md", "# Log\n\n## 2026-05-04\n\n- A thought.\n")
	return base
}

func TestWikiIndexGroupsEveryConceptByType(t *testing.T) {
	base := wikiBase(t)
	report, err := services.BuildWikiIndex(t.Context(), base, true)
	if err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	if report.Pages != 2 || report.Types != 2 || report.Tags != 3 {
		t.Fatalf("report = %+v, want 2 pages, 2 types, 3 tags", report)
	}
	body := readIndex(t, base)
	for _, want := range []string{
		"### decision", "### pattern",
		"[Retrieval boundary](retrieval-boundary.md) — Why retrieval is lexical&#46;",
		"[Declarative sources](declarative-sources.md)",
		"`collection` · `decision` · `retrieval`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index = %q, want it to contain %q", body, want)
		}
	}
	// log.md is a dated stream and index.md is the page being written; neither is a concept.
	if strings.Contains(body, "(log.md)") || strings.Contains(body, "(index.md)") {
		t.Fatalf("index lists a structural page:\n%s", body)
	}
}

// TestWikiIndexNeverTouchesTheHandWrittenHalf is the property the whole design turns on: OKF
// calls the index hand-curated, so generation may only own the marked block.
func TestWikiIndexNeverTouchesTheHandWrittenHalf(t *testing.T) {
	base := wikiBase(t)
	curated := "# Wiki\n\nStart with [the boundary](retrieval-boundary.md).\n\n"
	write(t, base, "wiki/index.md", curated)
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	body := readIndex(t, base)
	if !strings.HasPrefix(body, curated) {
		t.Fatalf("the owner's half was rewritten:\n%s", body)
	}

	// A second run with a page added replaces the block and still leaves the prose alone.
	write(t, base, "wiki/a-third.md", "---\ntype: insight\ntitle: A third\ntags: [x]\n---\n\n# A third\n")
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	body = readIndex(t, base)
	if !strings.HasPrefix(body, curated) {
		t.Fatalf("the owner's half was rewritten on the second run:\n%s", body)
	}
	if strings.Count(body, "<!-- >>> fkf managed block") != 1 {
		t.Fatalf("the block was appended rather than replaced:\n%s", body)
	}
	if !strings.Contains(body, "(a-third.md)") {
		t.Fatalf("the regenerated block omits the new page:\n%s", body)
	}
}

// TestWikiIndexIsDeterministicAndIdempotent: a generator that rewrites on every run makes a
// dirty working tree and a useless --check.
func TestWikiIndexIsDeterministicAndIdempotent(t *testing.T) {
	base := wikiBase(t)
	first, err := services.BuildWikiIndex(t.Context(), base, true)
	if err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	if !first.Created || !first.Changed {
		t.Fatalf("the first run = %+v, want a created page", first)
	}
	body := readIndex(t, base)
	second, err := services.BuildWikiIndex(t.Context(), base, true)
	if err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	if second.Changed || second.Created {
		t.Fatalf("the second run = %+v, want no change", second)
	}
	if readIndex(t, base) != body {
		t.Fatal("the second run produced different bytes")
	}
}

// TestWikiIndexCheckReportsStalenessAndWritesNothing is what a pre-commit hook depends on.
func TestWikiIndexCheckReportsStalenessAndWritesNothing(t *testing.T) {
	base := wikiBase(t)
	report, err := services.BuildWikiIndex(t.Context(), base, false)
	if err != nil {
		t.Fatalf("BuildWikiIndex() error = %v", err)
	}
	if !report.Stale {
		t.Fatalf("report = %+v, want a missing index reported as stale", report)
	}
	if _, err := os.Stat(filepath.Join(base.Root(), "wiki", "index.md")); !os.IsNotExist(err) {
		t.Fatal("--check wrote the page it was only asked to inspect")
	}
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
		t.Fatal(err)
	}
	after, err := services.BuildWikiIndex(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stale {
		t.Fatalf("report = %+v, want a freshly written index reported current", after)
	}
}

// TestWikiIndexRefusesATruncatedBlock: without an end boundary, generation cannot distinguish
// its stale block from authored content and must not guess by replacing through EOF.
func TestWikiIndexRefusesATruncatedBlock(t *testing.T) {
	base := wikiBase(t)
	existing := "# Wiki\n\nMine.\n\n<!-- >>> fkf managed block — regenerate with `fkf build wiki`; edits between the markers are lost -->\n\n## Pages\n\n## Authored after it\n\nKeep me.\n"
	write(t, base, "wiki/index.md", existing)
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err == nil || !strings.Contains(err.Error(), "no matching end marker") {
		t.Fatalf("BuildWikiIndex() error = %v, want a safe truncated-block refusal", err)
	}
	body := readIndex(t, base)
	if body != existing {
		t.Fatalf("failed build changed a page whose generated boundary is ambiguous:\n%s", body)
	}
}

func TestWikiIndexBoundsAnExistingNarrativeWithoutWriting(t *testing.T) {
	for _, writeChanges := range []bool{false, true} {
		t.Run(map[bool]string{false: "check", true: "write"}[writeChanges], func(t *testing.T) {
			base := wikiBase(t)
			absolute := filepath.Join(base.Root(), "wiki", "index.md")
			oversized := []byte("# Wiki\n\n" + strings.Repeat("x", int(core.MaxNarrativeBytes)))
			if err := os.WriteFile(absolute, oversized, core.BaseFileMode); err != nil {
				t.Fatal(err)
			}

			_, err := services.BuildWikiIndex(t.Context(), base, writeChanges)
			if !errors.Is(err, core.ErrFileTooLarge) {
				t.Fatalf("BuildWikiIndex(write=%t) error = %v, want the narrative bound", writeChanges, err)
			}
			after, readErr := os.ReadFile(absolute)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != string(oversized) {
				t.Fatal("a rejected oversized wiki index was modified")
			}
		})
	}
}

func TestWikiIndexRejectsANonCanonicalMarker(t *testing.T) {
	base := wikiBase(t)
	write(t, base, "wiki/index.md", "# Wiki\n\n<!-- >>> fkf managed block — regenerate with `fkf wiki index`; edits between the markers are lost -->\n<!-- <<< fkf managed block -->\n")
	_, err := services.BuildWikiIndex(t.Context(), base, true)
	if err == nil || !strings.Contains(err.Error(), "non-canonical begin marker") {
		t.Fatalf("BuildWikiIndex() error = %v, want the obsolete marker rejected", err)
	}
}

func TestWikiIndexRejectsDuplicateAndOrphanManagedMarkers(t *testing.T) {
	const begin = "<!-- >>> fkf managed block — regenerate with `fkf build wiki`; edits between the markers are lost -->"
	const end = "<!-- <<< fkf managed block -->"
	for _, test := range []struct {
		name, existing, want string
	}{
		{name: "duplicate begin", existing: "# Wiki\n\n" + begin + "\n" + begin + "\n" + end + "\n", want: "begin marker"},
		{name: "duplicate end", existing: "# Wiki\n\n" + begin + "\n" + end + "\n" + end + "\n", want: "end marker"},
		{name: "orphan end", existing: "# Wiki\n\n" + end + "\n", want: "no matching begin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := wikiBase(t)
			write(t, base, "wiki/index.md", test.existing)
			_, err := services.BuildWikiIndex(t.Context(), base, true)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildWikiIndex() error = %v, want ambiguity containing %q", err, test.want)
			}
			if after := readIndex(t, base); after != test.existing {
				t.Fatalf("failed build changed an ambiguous page:\n%s", after)
			}
		})
	}
}

func readIndex(t *testing.T, base *services.Base) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base.Root(), "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestWikiIndexCannotBeEndedByAPageItLists is the invariant test for "generation touches one
// block and nothing outside it". A page whose title or description quotes the end marker was
// written into the generated listing verbatim, and the NEXT build then found that copy first:
// everything after it was left behind as ordinary page content, the tail was duplicated on
// every build after that, and the file grew without bound. The reach is not only an author's
// typo — the fkf-learn skill writes wiki pages while summarising collected content, which is
// untrusted by construction.
func TestWikiIndexCannotBeEndedByAPageItLists(t *testing.T) {
	base := wikiBase(t)
	write(t, base, "wiki/quoting.md",
		"---\ntype: concept\ntitle: \"Note <!-- <<< fkf managed block --> and after\"\n"+
			"description: \"Ends with <!-- <<< fkf managed block -->\"\ntags: [x]\n---\n\n# Note\n")

	var previous string
	for build := range 3 {
		if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
			t.Fatal(err)
		}
		body := readIndex(t, base)
		var begins, ends, outside int
		after := false
		for _, line := range strings.Split(body, "\n") {
			switch {
			case strings.HasPrefix(line, "<!-- >>> fkf managed block"):
				begins++
			case line == "<!-- <<< fkf managed block -->":
				ends++
				after = true
			case after && strings.TrimSpace(line) != "":
				outside++
			}
		}
		if begins != 1 || ends != 1 {
			t.Fatalf("build %d left %d begin and %d end marker(s); the page ended the block that lists it:\n%s",
				build+1, begins, ends, body)
		}
		if outside != 0 {
			t.Fatalf("build %d wrote %d line(s) of generated content outside the block:\n%s", build+1, outside, body)
		}
		if build > 0 && body != previous {
			t.Fatalf("build %d changed a file build %d had already settled:\n%s", build+1, build, body)
		}
		previous = body
	}
	// And the page is still listed, readably: neutralising is not dropping.
	if !strings.Contains(previous, "quoting.md") {
		t.Fatalf("the page was dropped from the listing rather than neutralised:\n%s", previous)
	}
	if strings.Contains(previous, "\u200b") {
		t.Fatalf("the generated index contains an invisible separator:\n%s", previous)
	}
	validation, err := services.ValidateMarkdownLayer(t.Context(), base, "wiki", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.OK {
		t.Fatalf("build generated an index its own strict validator rejects: %+v", validation.Issues)
	}
}

// Frontmatter is authored input and can also be produced by an agent from collected,
// untrusted data. The generated index must display it without letting Markdown or HTML in a
// scalar become structure, a graph assertion, or a managed-block boundary.
func TestWikiIndexRendersFrontmatterAsSingleLineLiteralMarkdown(t *testing.T) {
	base := wikiBase(t)
	wikiType := "concept [Type edge](https://type.example.test) <script>type</script>"
	title := "Title](https://title.example.test) [edge # Injected heading"
	description := "Description [Description edge](https://description.example.test) " +
		"<!-- <<< fkf managed block --> <img src=x>"
	write(t, base, "wiki/hostile.md", fmt.Sprintf(
		"---\ntype: %q\ntitle: %q\ndescription: %q\ntags: [safety]\n---\n\n# Hostile metadata\n",
		wikiType, title, description))

	page, err := services.ReadPageContext(t.Context(), base, "wiki/hostile.md")
	if err != nil {
		t.Fatal(err)
	}
	if page.Type != wikiType || page.Title != title || page.Description != description {
		t.Fatalf("parsed values changed: type=%q title=%q description=%q", page.Type, page.Title, page.Description)
	}
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
		t.Fatal(err)
	}

	index, err := services.ReadPageContext(t.Context(), base, "wiki/index.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Links) != 3 { // the hostile page plus the two ordinary fixture pages
		t.Fatalf("generated links = %+v, want only the three listed wiki pages", index.Links)
	}
	for _, link := range index.Links {
		if strings.HasPrefix(link.Target, "https://") {
			t.Fatalf("frontmatter manufactured an external Markdown link: %+v", link)
		}
	}
	if len(index.Headings) != 6 { // # Wiki, ## Pages, three types, ## Tags
		t.Fatalf("generated headings = %+v, want metadata to remain inside one type heading", index.Headings)
	}
	body := readIndex(t, base)
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img ") {
		t.Fatalf("frontmatter manufactured raw HTML in the generated index:\n%s", body)
	}
	if strings.Count(body, "<!-- >>> fkf managed block") != 1 ||
		strings.Count(body, "<!-- <<< fkf managed block -->") != 1 {
		t.Fatalf("frontmatter manufactured a managed marker:\n%s", body)
	}

	validation, err := services.ValidateMarkdownLayer(t.Context(), base, "wiki", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.OK {
		t.Fatalf("strict validation = %+v, generated literal Markdown must satisfy it", validation.Issues)
	}
	edges, _, err := services.ExtractEdges(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if strings.HasPrefix(edge.Dst, "https://type.example.test") ||
			strings.HasPrefix(edge.Dst, "https://title.example.test") ||
			strings.HasPrefix(edge.Dst, "https://description.example.test") {
			t.Fatalf("frontmatter manufactured an external graph edge: %+v", edge)
		}
	}
}

func TestWikiIndexRefusesUnsafeFrontmatterBeforeWriting(t *testing.T) {
	tests := []struct {
		name, frontmatter, field, codepoint string
	}{
		{"invisible title", `title: "Hidden\u200Btitle"`, "title", "U+200B"},
		{"terminal control description", `description: "Control\u001B[31mred"`, "description", "U+001B"},
		{"multiline type", `type: "line\nbreak"`, "type", "U+000A"},
		{"control in tag", `tags: [safe, "bad\u007Ftag"]`, "tags[1]", "U+007F"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := wikiBase(t)
			frontmatter := "type: decision\ntitle: Safe\ndescription: Safe\ntags: [safe]\n"
			field := strings.SplitN(test.frontmatter, ":", 2)[0]
			lines := strings.Split(frontmatter, "\n")
			for index, line := range lines {
				if strings.HasPrefix(line, field+":") {
					lines[index] = test.frontmatter
				}
			}
			write(t, base, "wiki/unsafe.md", "---\n"+strings.Join(lines, "\n")+"---\n\n# Unsafe metadata\n")
			const curated = "# Wiki\n\nKeep this authored introduction unchanged.\n"
			write(t, base, "wiki/index.md", curated)

			_, err := services.BuildWikiIndex(t.Context(), base, true)
			if err == nil || !strings.Contains(err.Error(), "wiki/unsafe.md") ||
				!strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), test.codepoint) {
				t.Fatalf("BuildWikiIndex() error = %v, want page, field, and %s", err, test.codepoint)
			}
			if after := readIndex(t, base); after != curated {
				t.Fatalf("failed build changed wiki/index.md:\n%s", after)
			}
		})
	}
}
