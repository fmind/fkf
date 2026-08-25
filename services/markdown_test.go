package services_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestParsePagePreservesUnknownFrontmatter(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte(`---
type: decision
title: A decision
tags: [architecture, retrieval]
invented_by_a_teammate: keep me
---

# A decision

Body text with a [link](../projects/p.md) and an ![image](img.png).
`), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if page.Type != "decision" || page.Title != "A decision" || page.Slug != "a" {
		t.Fatalf("page = %+v", page)
	}
	if strings.Join(page.Tags, ",") != "architecture,retrieval" {
		t.Fatalf("tags = %v", page.Tags)
	}
	// A field fkf does not understand has to survive every read, or a teammate's convention
	// silently disappears the first time an agent rewrites a page.
	if page.Frontmatter["invented_by_a_teammate"] != "keep me" {
		t.Fatalf("frontmatter = %v, want the unknown key preserved", page.Frontmatter)
	}
	if len(page.Links) != 2 {
		t.Fatalf("links = %v, want the inline link and the image", page.Links)
	}
}

// TestLinksInCodeAreSkipped is the line between an illustration and an assertion: a link in a
// code sample must not become a graph edge nobody claimed.
func TestLinksInCodeAreSkipped(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte("# A\n\n"+
		"A real [link](real.md).\n\n"+
		"```markdown\n[fenced](fenced.md)\n```\n\n"+
		"~~~\n[tilde-fenced](tilde.md)\n~~~\n\n"+
		"Inline `[code](code.md)` stays out too.\n"+
		"A double-backtick span ``[double](double.md)`` is literal too.\n"+
		"A double-backtick span `` `[nested](nested.md)` `` is literal too.\n"+
		"An escaped opening bracket \\[escaped](escaped.md) is not a link.\n\n"+
		"    [indented code](indented.md)\n\n"+
		"   \t[tab-expanded code](tab-expanded.md)\n\n"+
		"````markdown\n[four-backtick fence](four.md)\n``` trailing text\n[still fenced](still-fenced.md)\n````\n\n"+
		"A multiline `code span starts\n[multiline code](multiline.md)\nand ends here`.\n\n"+
		"<!--\n[HTML comment](comment.md)\n-->\n\n"+
		"<pre>\n[raw HTML](raw-html.md)\n</pre>\n"), testClock)
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]string, 0, len(page.Links))
	for _, link := range page.Links {
		targets = append(targets, link.Target)
	}
	if strings.Join(targets, ",") != "real.md" {
		t.Fatalf("links = %v, want only the one outside code", targets)
	}
}

func TestSetextHeadingsAreAddressable(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte("Rendered title\n==============\n"), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Headings) != 1 || page.Headings[0].Level != 1 ||
		page.Headings[0].Text != "Rendered title" || page.Headings[0].Anchor != "rendered-title" ||
		page.Title != "Rendered title" {
		t.Fatalf("page = %+v, want the setext heading to supply title and addressable anchor", page)
	}
}

func TestHeadingsInCodeBlocksAreSkipped(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte("# Real\n\n"+
		"    # Indented code heading\n\n"+
		"````markdown\n# Fenced heading\n``` trailing text\n# Still fenced\n````\n"), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Headings) != 1 || page.Headings[0].Text != "Real" {
		t.Fatalf("headings = %+v, want only the rendered prose heading", page.Headings)
	}
}

// TestLinkTitleIsOnlyMetadata keeps ordinary Markdown semantics honest: a tooltip is not a
// hidden second graph assertion, even when its prose happens to look like an FKF URI.
func TestLinkTitleIsOnlyMetadata(t *testing.T) {
	page, err := services.ParsePage("projects/p.md",
		[]byte(`# P

- [FK-412](https://acme/browse/FK-412 "../events/2026-08-20/jira-issues.json#FK-412")
`), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Links) != 1 || page.Links[0].Target != "https://acme/browse/FK-412" ||
		page.Links[0].Title != "../events/2026-08-20/jira-issues.json#FK-412" ||
		page.Links[0].Via != "markdown-inline" {
		t.Fatalf("links = %+v, want one visible link whose title remains metadata", page.Links)
	}
}

// TestOrdinaryLinkTitleIsMetadata keeps standard Markdown title attributes usable. Tooltip
// prose belongs to the visible link as metadata and is never resolved as a base path.
func TestOrdinaryLinkTitleIsMetadata(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/example.md", `---
type: insight
title: Example
tags: [markdown]
---

# Example

[Example](https://example.test "ordinary tooltip")
`)

	report, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Errors != 0 {
		t.Fatalf("strict validation = %+v, want an ordinary Markdown tooltip accepted", report)
	}
	page, err := services.ReadPage(base, "wiki/example.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Links) != 1 || page.Links[0].Target != "https://example.test" ||
		page.Links[0].Title != "ordinary tooltip" || page.Links[0].Via != "markdown-inline" {
		t.Fatalf("links = %+v, want one visible link carrying its ordinary title as metadata", page.Links)
	}
}

func TestInlineLinkDestinationsPreserveBalancedAndEscapedParentheses(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte(`# Links

[Balanced](https://en.wikipedia.org/wiki/Foo_(bar))
[Escaped](https://example.test/Foo_\(bar\))
`), testClock)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://en.wikipedia.org/wiki/Foo_(bar)",
		"https://example.test/Foo_(bar)",
	}
	if len(page.Links) != len(want) {
		t.Fatalf("links = %+v, want %d exact destinations", page.Links, len(want))
	}
	for index, target := range want {
		if page.Links[index].Target != target {
			t.Errorf("link %d target = %q, want %q", index, page.Links[index].Target, target)
		}
	}
}

func TestReferenceLinksAreExtracted(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte("# A\n\nSee [the page][ref].\n\n[ref]: ../projects/p.md\n"), testClock)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, link := range page.Links {
		if link.Via == "markdown-reference" && link.Target == "../projects/p.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("links = %+v, want the reference definition", page.Links)
	}
}

func TestReferenceLinkDestinationUsesRenderedMarkdownValue(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte(
		"# A\n\nSee [the page][ref].\n\n[ref]: https://example.test/Foo_\\(bar\\)?a=1&amp;b=2\n",
	), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Links) != 1 || page.Links[0].Target != "https://example.test/Foo_(bar)?a=1&b=2" {
		t.Fatalf("links = %+v, want the exact rendered reference destination", page.Links)
	}
}

func TestURLAutolinksAreExtractedWithoutInventingEmailURIs(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte(
		"# A\n\n<https://example.test/Foo_\\(bar\\)?a=1&amp;b=2> <person@example.test>\n",
	), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Links) != 1 || page.Links[0].Target != "https://example.test/Foo_(bar)?a=1&b=2" ||
		page.Links[0].Via != "markdown-autolink" {
		t.Fatalf("links = %+v, want the rendered URL autolink only", page.Links)
	}
}

func TestHeadingsCarryGitHubAnchors(t *testing.T) {
	page, err := services.ParsePage("wiki/a.md", []byte("# Title\n\n## Key Outcomes\n\n### Sub\n\n## Key Outcomes\n\n## Key Outcomes-1\n"), testClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Headings) != 5 || page.Headings[1].Anchor != "key-outcomes" || page.Headings[2].Level != 3 ||
		page.Headings[3].Anchor != "key-outcomes-1" || page.Headings[4].Anchor != "key-outcomes-1-1" {
		t.Fatalf("headings = %+v", page.Headings)
	}
}

func TestParsePageRejectsUnclosedFrontmatter(t *testing.T) {
	_, err := services.ParsePage("wiki/a.md", []byte("---\ntype: decision\n\n# A\n"), testClock)
	if err == nil || !strings.Contains(err.Error(), "closing delimiter") {
		t.Fatalf("ParsePage() error = %v, want the unclosed frontmatter named", err)
	}
}

func TestFindInvisible(t *testing.T) {
	if _, _, found := services.FindInvisible("ordinary text"); found {
		t.Fatal("FindInvisible() flagged ordinary text")
	}
	char, name, found := services.FindInvisible("hidden\u200binstruction")
	if !found || char != '\u200b' || name != "zero-width space" {
		t.Fatalf("FindInvisible() = %q, %q, %v", char, name, found)
	}
}

func TestValidateWiki(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/index.md", "# Wiki\n\n- [Good](good.md)\n")
	write(t, base, "wiki/good.md", "---\ntype: decision\ntitle: Good\ntags: [architecture]\n---\n\n# Good\n\nText.\n")
	write(t, base, "wiki/untagged.md", "---\ntype: insight\ntitle: Untagged\n---\n\n# Untagged\n")
	write(t, base, "wiki/log.md", "# Log\n\n## 2026-05-10\n\n- a\n\n## 2026-05-09\n\n- b\n")

	report, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Errors != 0 {
		t.Fatalf("report = %+v, want a clean bundle with warnings only", report)
	}
	if report.Warnings == 0 {
		t.Fatal("the untagged concept must be a warning: a flat layer is navigated by tags")
	}
	// index.md and log.md are the bundle's structure rather than concepts in it, so they are
	// not asked for a type or tags.
	for _, issue := range report.Issues {
		if issue.URI == "wiki/index.md" || issue.URI == "wiki/log.md" {
			t.Fatalf("structural page flagged: %+v", issue)
		}
	}

	strict, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if strict.OK || strict.Errors == 0 {
		t.Fatalf("--strict must promote the untagged warning to an error, got %+v", strict)
	}
}

func TestValidateWikiChecksEveryAddressableFileFragment(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"One","who":"marc@example.test"}]`)
	write(t, base, "wiki/target.md", `---
type: reference
title: Target
tags: [validation]
---

# Target

## Repeated

First.

## Repeated

Second.
`)
	write(t, base, "wiki/links.md", `---
type: reference
title: Links
tags: [validation]
---

# Links

- [valid same-page heading](#details)
- [missing same-page heading](#missing-self)
- [valid duplicate heading](target.md#repeated-1)
- [missing cross-page heading](target.md#missing-page)
- [valid record](../events/2026-05-04/synthetic.json#a1)
- [missing record](../events/2026-05-04/synthetic.json#missing-record)
- [valid jq selection](../events/2026-05-04/synthetic.json?jq=.records[0].id)
- [invalid jq selection](../events/2026-05-04/synthetic.json?jq=%28)
- [typed entity](person:email/marc@example.test)

## Details

Present.
`)

	permissive, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !permissive.OK || permissive.Warnings != 4 {
		t.Fatalf("permissive report = %+v, want three missing-fragment warnings and one invalid jq warning", permissive)
	}
	strict, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if strict.OK || strict.Errors != 4 {
		t.Fatalf("strict report = %+v, want three missing-fragment errors and one invalid jq error", strict)
	}
	var messages strings.Builder
	for _, issue := range strict.Issues {
		messages.WriteString(issue.Message + "\n")
	}
	for _, wanted := range []string{"missing-self", "missing-page", "missing-record", "?jq=%28"} {
		if !strings.Contains(messages.String(), wanted) {
			t.Errorf("issues omit missing fragment %q:\n%s", wanted, messages.String())
		}
	}
}

func TestValidateWikiFindsRealProblems(t *testing.T) {
	cases := []struct {
		name, path, body, wantMessage string
	}{
		{
			"an escaping link", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\n---\n\n# B\n\n[out](../../../etc/passwd)\n",
			"escapes the base",
		},
		{
			"a malformed URI", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\n---\n\n# B\n\n[bad](target.md?other=value)\n",
			"invalid URI",
		},
		{
			"an undeclared relation", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\nrelations:\n  invented: [topic:x]\n---\n\n# B\n",
			"relations.invented is not declared",
		},
		{
			"a semantic field that is not a relation", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\nrelations:\n  topic: [topic:x]\n---\n\n# B\n",
			"relations.topic is not declared as a relation",
		},
		{
			"a relation violating cardinality", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\nrelations:\n  ticket: [ticket:one, ticket:two]\n---\n\n# B\n",
			"cardinality optional does not allow",
		},
		{
			"an invisible character", "wiki/hidden.md",
			"---\ntype: decision\ntitle: H\ntags: [x]\n---\n\n# H\n\nnormal\u200bhidden\n",
			"invisible character",
		},
		{
			"an invisible frontmatter scalar", "wiki/hidden.md",
			"---\ntype: decision\ntitle: \"Hidden\\u200Btitle\"\ntags: [x]\n---\n\n# H\n",
			"frontmatter title contains invisible character",
		},
		{
			"a terminal control in frontmatter", "wiki/control.md",
			"---\ntype: decision\ntitle: \"Control\\u001B[31mred\"\ntags: [x]\n---\n\n# C\n",
			"frontmatter title contains control character",
		},
		{
			"a terminal control in the body", "wiki/control.md",
			"---\ntype: decision\ntitle: Control\ntags: [x]\n---\n\n# C\n\nControl\u001b[31mred\n",
			"body contains control character",
		},
		{
			"a log out of order", "wiki/log.md",
			"# Log\n\n## 2026-05-09\n\n- a\n\n## 2026-05-10\n\n- b\n",
			"newest first",
		},
		{
			"a log with a repeated date", "wiki/log.md",
			"# Log\n\n## 2026-05-10\n\n- a\n\n## 2026-05-10\n\n- b\n",
			"repeats the date",
		},
		{
			"an index with no heading", "wiki/index.md",
			"Just prose, no heading.\n",
			"level-one heading",
		},
		{
			"a heading with no addressable anchor", "wiki/bad.md",
			"---\ntype: decision\ntitle: B\ntags: [x]\n---\n\n# B\n\n## !!!\n",
			"heading has no addressable anchor",
		},
		{
			"a nested page", "wiki/nested/deep.md",
			"---\ntype: decision\ntitle: D\ntags: [x]\n---\n\n# D\n",
			"layer is flat",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			base := newBase(t, baseConfig, nil)
			if strings.Contains(test.path, "/nested/") {
				// Store.Resolve correctly refuses a nested flat-layer URI. Write this malformed
				// fixture beneath the base directly so the validator can prove it still reports
				// neighbour files that an editor or git checkout placed there.
				absolute := filepath.Join(base.Root(), filepath.FromSlash(test.path))
				if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(absolute, []byte(test.body), core.BaseFileMode); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, base, test.path, test.body)
			}
			report, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if report.OK {
				t.Fatalf("report = %+v, want an error", report)
			}
			var joined strings.Builder
			for _, issue := range report.Issues {
				joined.WriteString(issue.Message + "\n")
			}
			if !strings.Contains(joined.String(), test.wantMessage) {
				t.Fatalf("issues = %s, want one mentioning %q", joined.String(), test.wantMessage)
			}
		})
	}
}

// TestValidateProjectsRequiresStatus is the one thing that makes projects/ different from
// wiki/: a project with no status is not a project, it is a page nobody can act on.
func TestValidateProjectsRequiresStatus(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "projects/ok.md", "---\ntype: project\ntitle: OK\nstatus: active\ntags: [x]\n---\n\n# OK\n")
	write(t, base, "projects/nostatus.md", "---\ntype: project\ntitle: No\ntags: [x]\n---\n\n# No\n")
	write(t, base, "projects/badstatus.md", "---\ntype: project\ntitle: Bad\nstatus: someday\ntags: [x]\n---\n\n# Bad\n")

	report, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerProjects, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 2 {
		t.Fatalf("errors = %d, want exactly the two pages with no usable status: %+v", report.Errors, report.Issues)
	}
	for _, issue := range report.Issues {
		if !strings.Contains(issue.Message, "active, paused, or done") {
			t.Fatalf("issue = %+v, want the closed set named", issue)
		}
	}
}

func TestValidateRefusesADisabledLayer(t *testing.T) {
	base := newBase(t, strings.Replace(baseConfig, "  wiki: true", "  wiki: false", 1), nil)
	_, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerWiki, false, false)
	if !errorsAsLayerDisabled(err) {
		t.Fatalf("error = %v, want a disabled layer refused rather than reported empty", err)
	}
}

func errorsAsLayerDisabled(err error) bool {
	var disabled core.ErrLayerDisabled
	return errors.As(err, &disabled)
}
