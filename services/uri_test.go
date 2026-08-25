package services_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// The URI grammar lives in two places by necessity — in this package and in
// skills/fkf-use/SKILL.md, which is what an agent reads. AGENTS.md states they must agree, so the
// agreement is checked rather than remembered: every URI the skill shows has to parse here.

// skillURIExamples reads the code spans out of the skill's URI section. Reading the file rather
// than repeating its table is the point: a form added to the skill and not to the parser fails.
func skillURIExamples(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "skills", "fkf-use", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(data)
	start := strings.Index(section, "\n## URIs\n")
	if start < 0 {
		t.Fatal("skills/fkf-use/SKILL.md has no `## URIs` section; the grammar has to live somewhere an agent reads")
	}
	section = section[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	var examples []string
	for _, span := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(section, -1) {
		candidate := strings.TrimSpace(span[1])
		if looksAddressable(candidate) {
			examples = append(examples, candidate)
		}
	}
	if len(examples) < 15 {
		t.Fatalf("only %d URI example(s) found in the skill; the table is the contract", len(examples))
	}
	return examples
}

// looksAddressable keeps the prose and the placeholder forms out: `<path>[?jq=<expr>]` is the
// grammar being described, not an instance of it.
func looksAddressable(candidate string) bool {
	// A form has to start with a letter and name something: `/` on its own is the sentence
	// "directories end in /", not a URI.
	if len(candidate) < 3 || candidate[0] < 'a' || candidate[0] > 'z' {
		return false
	}
	if strings.ContainsAny(candidate, "<>[] ") || strings.HasPrefix(candidate, "--") {
		return false
	}
	if regexp.MustCompile(`^[a-z][a-z0-9+.-]*:.+`).MatchString(candidate) {
		return true
	}
	return strings.Contains(candidate, "://") ||
		strings.Contains(candidate, ".json") || strings.Contains(candidate, ".md") ||
		strings.Contains(candidate, ".yaml") || strings.Contains(candidate, ".tsv") ||
		strings.HasSuffix(candidate, "/")
}

func TestEveryFormInTheSkillParses(t *testing.T) {
	for _, raw := range skillURIExamples(t) {
		if _, err := services.ParseURI(raw); err != nil {
			t.Fatalf("ParseURI(%q) error = %v; the skill's table and the parser must agree", raw, err)
		}
	}
}

func TestURIRoundTrip(t *testing.T) {
	for _, raw := range skillURIExamples(t) {
		first, err := services.ParseURI(raw)
		if err != nil {
			t.Fatal(err)
		}
		second, err := services.ParseURI(first.String())
		if err != nil {
			t.Fatalf("re-parsing %q (from %q) failed: %v", first.String(), raw, err)
		}
		if second.String() != first.String() {
			t.Fatalf("round trip of %q: %q then %q", raw, first.String(), second.String())
		}
		if second.Scheme != first.Scheme || second.Path != first.Path ||
			second.Fragment != first.Fragment || second.JQ != first.JQ || second.Dir != first.Dir {
			t.Fatalf("round trip of %q changed the parse: %+v then %+v", raw, first, second)
		}
	}
}

func TestParseURIParts(t *testing.T) {
	uri, err := services.ParseURI("events/2026-05-04/google-gmail-emails.json?jq=.payload.headers#18c2a9f")
	if err != nil {
		t.Fatal(err)
	}
	// Evaluation is path → fragment → jq, so this reads as "the record 18c2a9f, then its headers".
	if uri.Path != "events/2026-05-04/google-gmail-emails.json" || uri.Fragment != "18c2a9f" || uri.JQ != ".payload.headers" {
		t.Fatalf("parsed = %+v", uri)
	}
	// A `?jq=` URI is a read form, never a graph node.
	if uri.NodeURI() != "events/2026-05-04/google-gmail-emails.json#18c2a9f" {
		t.Fatalf("NodeURI() = %q, want the record without the jq expression", uri.NodeURI())
	}
	if uri.FileURI() != "events/2026-05-04/google-gmail-emails.json" {
		t.Fatalf("FileURI() = %q", uri.FileURI())
	}
}

// TestFragmentSurvivesASlash is the case percent-encoding exists for: `index/github-repositories.json#fmind/fkf`
// has to address the record whose declared id is literally "fmind/fkf".
func TestFragmentSurvivesASlash(t *testing.T) {
	uri, err := services.ParseURI("index/github-repositories.json#fmind/fkf")
	if err != nil {
		t.Fatal(err)
	}
	if uri.Path != "index/github-repositories.json" || uri.Fragment != "fmind/fkf" {
		t.Fatalf("parsed = %+v", uri)
	}
}

func TestEntitySchemesPreserveBaseChosenIdentitySemantics(t *testing.T) {
	for _, raw := range []string{
		"person:Marc@Example.Test", "repo:Fmind/FKF", "ticket:fk-412", "topic:Architecture",
	} {
		uri, err := services.ParseURI(raw)
		if err != nil {
			t.Fatal(err)
		}
		if uri.String() != raw {
			t.Fatalf("ParseURI(%q) = %q, want the base-chosen identity preserved", raw, uri.String())
		}
		if !uri.IsEntity() {
			t.Fatalf("%q must be an entity", raw)
		}
	}
}

func TestEntityURIRoundTripEncodesLiteralPercentAndControls(t *testing.T) {
	for _, test := range []struct {
		raw, want, value string
	}{
		{"person:Ops%25Team@Example.Test", "person:Ops%25Team@Example.Test", "Ops%Team@Example.Test"},
		{"tag:line%0Abreak", "tag:line%0Abreak", "line\nbreak"},
		{"repo:owner/control%01name", "repo:owner/control%01name", "owner/control\x01name"},
	} {
		first, err := services.ParseURI(test.raw)
		if err != nil {
			t.Fatalf("ParseURI(%q) error = %v", test.raw, err)
		}
		if first.String() != test.want || first.Value != test.value {
			t.Fatalf("ParseURI(%q) = (%q, %q), want (%q, %q)",
				test.raw, first.String(), first.Value, test.want, test.value)
		}
		second, err := services.ParseURI(first.String())
		if err != nil {
			t.Fatalf("re-parse %q: %v", first.String(), err)
		}
		if second.String() != first.String() || second.Value != first.Value {
			t.Fatalf("round trip changed %+v into %+v", first, second)
		}
		if strings.ContainsAny(first.String(), "\n\r\t\x01") {
			t.Fatalf("canonical entity URI %q contains a raw control byte", first.String())
		}
	}
}

func TestParseURIRejects(t *testing.T) {
	cases := []struct{ raw, wantMessage string }{
		{"", "empty"},
		{"../etc/passwd", "escapes the base"},
		{"events/../../etc/passwd", "escapes the base"},
		{"/etc/passwd", "is absolute"},
		{"~/secrets", "home-relative"},
		{"events/x.json?limit=10", "the only supported query is ?jq="},
		{"events/x.json?jq=", "names no expression"},
		{"wiki/x.md#", "names no fragment"},
		{"events/2026-05-04/#id", "a directory has no fragment"},
		{"person:", "names no person"},
		{"file:wiki/x.md", "reserved"},
		{"external:example.test", "reserved"},
		{".", "base root is not addressable"},
		{"./", "base root is not addressable"},
	}
	for _, test := range cases {
		_, err := services.ParseURI(test.raw)
		if err == nil {
			t.Fatalf("ParseURI(%q) succeeded, want a rejection", test.raw)
		}
		if !strings.Contains(err.Error(), test.wantMessage) {
			t.Fatalf("ParseURI(%q) error = %v, want it to mention %q", test.raw, err, test.wantMessage)
		}
	}
}

// TestResolveLinkFollowsTheLinkingFile is what lets a page's links work in an editor, on
// GitHub, and through fkf at the same time.
func TestResolveLinkFollowsTheLinkingFile(t *testing.T) {
	cases := []struct{ from, target, want string }{
		{"wiki/a.md", "b.md", "wiki/b.md"},
		{"wiki/a.md", "../projects/p.md", "projects/p.md"},
		{"wiki/a.md", "../events/2026-05-04/x.json#a1", "events/2026-05-04/x.json#a1"},
		{"tasks/2026-05-04/review/TASKS.md", "../../../projects/fkf.md#decisions", "projects/fkf.md#decisions"},
		{"wiki/a.md", "/wiki/b.md", "wiki/b.md"},
		{"wiki/a.md", "ticket:FK-412", "ticket:FK-412"},
		{"wiki/a.md", "https://example.test/x", "https://example.test/x"},
	}
	for _, test := range cases {
		uri, err := services.ResolveLink(test.from, test.target)
		if err != nil {
			t.Fatalf("ResolveLink(%q, %q) error = %v", test.from, test.target, err)
		}
		if uri.NodeURI() != test.want {
			t.Fatalf("ResolveLink(%q, %q) = %q, want %q", test.from, test.target, uri.NodeURI(), test.want)
		}
	}
}

// TestResolveLinkRejectsAnEscape is the rule that a link leaving the base is refused rather
// than clamped into something plausible.
func TestResolveLinkRejectsAnEscape(t *testing.T) {
	_, err := services.ResolveLink("wiki/a.md", "../../../etc/passwd")
	if !errors.Is(err, core.ErrPathEscapes) {
		t.Fatalf("ResolveLink() error = %v, want an escape refused", err)
	}
}

func TestParseURIRejectsUnpublishedExternalSchemes(t *testing.T) {
	for _, raw := range []string{"http://example.test", "mailto:person@example.test", "ftp://example.test/file"} {
		if _, err := services.ParseURI(raw); err == nil {
			t.Errorf("ParseURI(%q) succeeded; only external https: is in the published grammar", raw)
		}
	}
	if _, err := services.ParseURI("https://example.test/path"); err != nil {
		t.Fatalf("ParseURI(https) error = %v", err)
	}
}

func TestRelativeLinkIsTheInverse(t *testing.T) {
	cases := []struct{ from, target, want string }{
		{"wiki/a.md", "wiki/b.md", "b.md"},
		{"wiki/a.md", "projects/p.md", "../projects/p.md"},
		{"tasks/2026-05-04/review/TASKS.md", "projects/fkf.md#decisions", "../../../projects/fkf.md#decisions"},
	}
	for _, test := range cases {
		if got := services.RelativeLink(test.from, test.target); got != test.want {
			t.Fatalf("RelativeLink(%q, %q) = %q, want %q", test.from, test.target, got, test.want)
		}
		// And it round-trips: what a page writes is what fkf resolves back.
		uri, err := services.ResolveLink(test.from, test.want)
		if err != nil || uri.NodeURI() != test.target {
			t.Fatalf("resolving %q from %q = %q, %v", test.want, test.from, uri.NodeURI(), err)
		}
	}
}

func TestAnchorSlugFollowsGitHubRules(t *testing.T) {
	cases := []struct{ heading, want string }{
		{"Verification", "verification"},
		{"Key Outcomes", "key-outcomes"},
		{"What it protects, and what it does not", "what-it-protects-and-what-it-does-not"},
		{"URIs and the graph", "uris-and-the-graph"},
		{"`fkf init`", "fkf-init"},
		{"[Deployment guide](https://example.test)", "deployment-guide"},
		{"![Launch diagram](diagram.png)", "launch-diagram"},
		{"<span>Inline</span> **markup**", "inline-markup"},
		{"Deploy <!-- internal note -->", "deploy"},
		{"Fix FK&#45;412", "fix-fk-412"},
		{"1&#46; Scope", "1-scope"},
		{"Nebula&#58; Northport&#39;s", "nebula-northports"},
		{"Decision – launch", "decision--launch"},
		{"🚀 Launch", "-launch"},
		{"Привет non-latin 你好", "привет-non-latin-你好"},
		{"Cafe\u0301", "cafe\u0301"},
	}
	for _, test := range cases {
		if got := services.AnchorSlug(test.heading); got != test.want {
			t.Fatalf("AnchorSlug(%q) = %q, want %q", test.heading, got, test.want)
		}
	}
}

// TestResolveLinkKeepsARelativeLinkWhoseFragmentIsAURL pins the split between a target's path
// and its tail. An RSS or notification record's declared id IS its own URL, so a page citing
// one writes `../events/<date>/rss.json#https://…`; testing the whole string for "://" made
// every such link "absolute", which then failed to parse as one and was reported as escaping
// the base — dropping the edge and blaming the author for it.
func TestResolveLinkKeepsARelativeLinkWhoseFragmentIsAURL(t *testing.T) {
	uri, err := services.ResolveLink("wiki/a.md", "../events/2026-08-22/rss.json#https://example.test/post")
	if err != nil {
		t.Fatalf("ResolveLink() error = %v, want a base-relative record URI", err)
	}
	if uri.Path != "events/2026-08-22/rss.json" {
		t.Errorf("path = %q, want events/2026-08-22/rss.json", uri.Path)
	}
	if uri.Fragment != "https://example.test/post" {
		t.Errorf("fragment = %q, want the record id verbatim", uri.Fragment)
	}
}

// TestResolveLinkStillTreatsARealSchemeAsAbsolute keeps the fix from swallowing the case it was
// carved out of: a scheme in the PATH is still an absolute target, and an escape is still refused.
func TestResolveLinkStillTreatsARealSchemeAsAbsolute(t *testing.T) {
	for target, want := range map[string]string{
		"https://example.test/x":   "https://example.test/x",
		"person:marc@example.test": "person:marc@example.test",
	} {
		uri, err := services.ResolveLink("wiki/a.md", target)
		if err != nil {
			t.Fatalf("ResolveLink(%q) error = %v", target, err)
		}
		if uri.String() != want {
			t.Errorf("ResolveLink(%q) = %q, want %q", target, uri.String(), want)
		}
	}
	if _, err := services.ResolveLink("wiki/a.md", "../../etc/passwd"); err == nil {
		t.Error("ResolveLink() accepted a target that escapes the base")
	}
}
