package services

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
	"go.yaml.in/yaml/v3"

	"github.com/fmind/fkf/core"
)

// One Markdown stack serves wiki/, projects/, and tasks/. They differ only in which
// frontmatter fields are required, so parsing and link extraction remain shared.
//
// Reading is permissive and writing is strict, which is what OKF asks for: a page written by
// hand years ago still parses, while a page fkf or a skill writes today has to be complete.

var (
	tagPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	isoDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	markdownParser = goldmark.DefaultParser()
)

// invisibleRunes are characters that render as nothing but change what a human believes a
// page says. They are refused on the write path — a page carrying one is either a copy-paste
// accident or a prompt-injection attempt, and neither belongs in approved knowledge.
var invisibleRunes = map[rune]string{
	'\u00ad': "soft hyphen", '\u200b': "zero-width space", '\u200c': "zero-width non-joiner",
	'\u200d': "zero-width joiner", '\u200e': "left-to-right mark", '\u200f': "right-to-left mark",
	'\u202a': "left-to-right embedding", '\u202b': "right-to-left embedding",
	'\u202c': "pop directional formatting", '\u202d': "left-to-right override",
	'\u202e': "right-to-left override", '\u2060': "word joiner", '\u2066': "left-to-right isolate",
	'\u2067': "right-to-left isolate", '\u2068': "first strong isolate", '\u2069': "pop directional isolate",
	'\ufeff': "zero-width no-break space",
}

// Link is one authored Markdown link, with the extractor that found it.
type Link struct {
	Target string `json:"target"`
	Line   int    `json:"line"`
	Via    string `json:"via"`
	Title  string `json:"title,omitempty"`
}

// Heading is one Markdown heading and the anchor it answers to.
type Heading struct {
	Level  int    `json:"level"`
	Text   string `json:"text"`
	Anchor string `json:"anchor"`
	Line   int    `json:"line"`
}

// Page is one parsed Markdown file in a base.
type Page struct {
	URI         string              `json:"uri"`
	Slug        string              `json:"slug"`
	Type        string              `json:"type,omitempty"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Status      string              `json:"status,omitempty"`
	Date        string              `json:"date,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Relations   map[string][]string `json:"relations,omitempty"`
	Frontmatter map[string]any      `json:"frontmatter,omitempty"`
	Body        string              `json:"-"`
	Headings    []Heading           `json:"headings,omitempty"`
	Links       []Link              `json:"links,omitempty"`
	Updated     string              `json:"updated,omitempty"`
	Bytes       int                 `json:"bytes"`
}

// ParsePage reads one Markdown file into a Page. Unknown frontmatter is preserved verbatim so
// a field fkf does not understand survives every read, write, and validation.
func ParsePage(uri string, data []byte, modified time.Time) (Page, error) {
	page := Page{
		URI: uri, Slug: strings.TrimSuffix(path.Base(uri), core.MarkdownExtension),
		Bytes: len(data), Frontmatter: map[string]any{},
	}
	if !modified.IsZero() {
		page.Updated = modified.UTC().Format(time.RFC3339)
	}
	frontmatter, body, bodyLine, err := splitFrontmatter(data)
	if err != nil {
		return page, fmt.Errorf("%s: %w", uri, err)
	}
	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, &page.Frontmatter); err != nil {
			return page, fmt.Errorf("%s: parse YAML frontmatter: %w", uri, err)
		}
	}
	if page.Frontmatter == nil {
		page.Frontmatter = map[string]any{}
	}
	page.Type = frontmatterString(page.Frontmatter, "type")
	page.Title = frontmatterString(page.Frontmatter, "title")
	page.Description = frontmatterString(page.Frontmatter, "description")
	page.Status = frontmatterString(page.Frontmatter, "status")
	page.Date = frontmatterString(page.Frontmatter, "date")
	page.Tags = frontmatterStrings(page.Frontmatter, "tags")
	page.Relations, err = frontmatterRelations(page.Frontmatter)
	if err != nil {
		return page, fmt.Errorf("%s: %w", uri, err)
	}
	page.Body = string(body)
	page.Headings, page.Links = extractMarkdown(page.Body, bodyLine)
	if page.Title == "" && len(page.Headings) > 0 {
		page.Title = page.Headings[0].Text
	}
	return page, nil
}

func frontmatterRelations(frontmatter map[string]any) (map[string][]string, error) {
	raw, ok := frontmatter["relations"]
	if !ok {
		return nil, nil
	}
	mapping, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("frontmatter relations must be a mapping of field names to URI lists")
	}
	relations := make(map[string][]string, len(mapping))
	for _, name := range sortedKeys(mapping) {
		items, ok := mapping[name].([]any)
		if !ok {
			return nil, fmt.Errorf("frontmatter relations.%s must be a URI list", name)
		}
		values := make([]string, 0, len(items))
		for _, item := range items {
			value, ok := core.ScalarString(item)
			if !ok || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("frontmatter relations.%s must contain only non-empty URI strings", name)
			}
			values = append(values, value)
		}
		relations[name] = values
	}
	return relations, nil
}

// ReadPage loads and parses one page from a base.
func ReadPage(base *Base, uri string) (Page, error) {
	return ReadPageContext(context.Background(), base, uri)
}

// ReadPageContext loads and parses one page from a base with cooperative cancellation.
func ReadPageContext(ctx context.Context, base *Base, uri string) (Page, error) {
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		return Page{}, err
	}
	data, err := core.ReadFileLimitContext(ctx, absolute, core.MaxNarrativeBytes)
	if err != nil {
		return Page{}, err
	}
	var modified time.Time
	if info, statErr := os.Stat(absolute); statErr == nil {
		modified = info.ModTime()
	}
	if err := checkContext(ctx); err != nil {
		return Page{}, err
	}
	page, err := ParsePage(uri, data, modified)
	if err != nil {
		return Page{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Page{}, err
	}
	return page, nil
}

func splitFrontmatter(data []byte) (frontmatter, body []byte, bodyLine int, err error) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, data, 1, nil
	}
	lines := strings.Split(text, "\n")
	for index := 1; index < len(lines); index++ {
		if strings.TrimRight(lines[index], "\r") != "---" {
			continue
		}
		return []byte(strings.Join(lines[1:index], "\n")),
			[]byte(strings.Join(lines[index+1:], "\n")), index + 2, nil
	}
	return nil, nil, 1, errors.New("frontmatter opening delimiter has no closing delimiter")
}

func frontmatterString(frontmatter map[string]any, key string) string {
	value, ok := frontmatter[key]
	if !ok {
		return ""
	}
	if rendered, ok := core.ScalarString(value); ok {
		return rendered
	}
	return ""
}

func frontmatterStrings(frontmatter map[string]any, key string) []string {
	value, ok := frontmatter[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if rendered, ok := core.ScalarString(item); ok {
				values = append(values, rendered)
			}
		}
		return values
	case string:
		return strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == ' ' })
	default:
		return nil
	}
}

func extractHeadings(body string, firstLine int) []Heading {
	headings, _ := extractMarkdown(body, firstLine)
	return headings
}

// extractMarkdown asks a standards-compliant CommonMark parser what the document renders as.
// Graph edges and addressable headings therefore come only from real AST nodes: code, raw HTML,
// escaped syntax, and malformed constructs can never be mistaken for authored structure.
func extractMarkdown(body string, firstLine int) ([]Heading, []Link) {
	source := []byte(body)
	document := markdownParser.Parse(gmtext.NewReader(source))
	lineStarts := markdownLineStarts(source)
	headings, links := []Heading{}, []Link{}
	usedAnchors := map[string]bool{}
	_ = gast.Walk(document, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		line := markdownSourceLine(lineStarts, firstLine, node.Pos())
		switch typed := node.(type) {
		case *gast.Heading:
			text := strings.TrimSpace(renderedMarkdownText(typed, source))
			base, anchor := AnchorSlug(text), AnchorSlug(text)
			for suffix := 1; usedAnchors[anchor]; suffix++ {
				anchor = fmt.Sprintf("%s-%d", base, suffix)
			}
			usedAnchors[anchor] = true
			headings = append(headings, Heading{Level: typed.Level, Text: text, Anchor: anchor, Line: line})
		case *gast.Link:
			// A reference definition is the single source of truth for reference links. Skipping
			// each use avoids duplicate edges whose only difference would be its occurrence line.
			if typed.Reference == nil {
				links = appendMarkdownLink(links, typed.Destination, typed.Title, line, "markdown-inline")
			}
		case *gast.Image:
			if typed.Reference == nil {
				links = appendMarkdownLink(links, typed.Destination, typed.Title, line, "markdown-inline")
			}
		case *gast.AutoLink:
			// CommonMark also renders <https://...> as a link node. Email autolinks deliberately
			// stay out: mailto is not part of fkf's published URI grammar, while identities use
			// an explicit entity scheme.
			if typed.AutoLinkType == gast.AutoLinkURL {
				links = appendMarkdownLink(links, typed.URL(source), nil, line, "markdown-autolink")
			}
		case *gast.LinkReferenceDefinition:
			target := renderedMarkdownValue(typed.Destination)
			if target != "" {
				links = append(links, Link{Target: target, Line: line, Via: "markdown-reference"})
			}
		}
		return gast.WalkContinue, nil
	})
	return headings, links
}

func appendMarkdownLink(links []Link, destination, title []byte, line int, via string) []Link {
	target := renderedMarkdownValue(destination)
	renderedTitle := renderedMarkdownValue(title)
	if target != "" {
		links = append(links, Link{Target: target, Title: renderedTitle, Line: line, Via: via})
	}
	return links
}

func renderedMarkdownValue(value []byte) string {
	return html.UnescapeString(markdownUnescape(strings.TrimSpace(string(value))))
}

func renderedMarkdownText(node gast.Node, source []byte) string {
	var rendered strings.Builder
	var appendNode func(gast.Node, bool)
	appendNode = func(current gast.Node, literal bool) {
		switch typed := current.(type) {
		case *gast.RawHTML:
			return
		case *gast.CodeSpan:
			for child := current.FirstChild(); child != nil; child = child.NextSibling() {
				appendNode(child, true)
			}
			return
		case *gast.Text:
			value := string(typed.Segment.Value(source))
			if !literal {
				value = html.UnescapeString(markdownUnescape(value))
			}
			rendered.WriteString(value)
			if typed.SoftLineBreak() || typed.HardLineBreak() {
				rendered.WriteByte(' ')
			}
			return
		case *gast.String:
			value := string(typed.Value)
			if !literal && !typed.IsCode() {
				value = html.UnescapeString(markdownUnescape(value))
			}
			rendered.WriteString(value)
			return
		case *gast.AutoLink:
			rendered.Write(typed.Label(source))
			return
		}
		for child := current.FirstChild(); child != nil; child = child.NextSibling() {
			appendNode(child, literal)
		}
	}
	appendNode(node, false)
	return rendered.String()
}

func renderedInlineMarkdown(value string) string {
	source := []byte("# " + value + "\n")
	document := markdownParser.Parse(gmtext.NewReader(source))
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		if heading, ok := node.(*gast.Heading); ok {
			return renderedMarkdownText(heading, source)
		}
	}
	return value
}

func markdownLineStarts(source []byte) []int {
	starts := []int{0}
	for index, value := range source {
		if value == '\n' && index+1 < len(source) {
			starts = append(starts, index+1)
		}
	}
	return starts
}

func markdownSourceLine(starts []int, firstLine, position int) int {
	if position < 0 {
		position = 0
	}
	index := sort.Search(len(starts), func(index int) bool { return starts[index] > position }) - 1
	if index < 0 {
		index = 0
	}
	return firstLine + index
}

func markdownUnescape(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for cursor := 0; cursor < len(value); cursor++ {
		if value[cursor] == '\\' && cursor+1 < len(value) && isASCIIPunctuation(value[cursor+1]) {
			cursor++
		}
		result.WriteByte(value[cursor])
	}
	return result.String()
}

func isASCIIPunctuation(value byte) bool {
	return value >= '!' && value <= '/' || value >= ':' && value <= '@' ||
		value >= '[' && value <= '`' || value >= '{' && value <= '~'
}

// FindInvisible reports the first invisible character in a value, if any.
func FindInvisible(value string) (rune, string, bool) {
	for _, char := range value {
		if name, invisible := invisibleRunes[char]; invisible {
			return char, name, true
		}
	}
	return 0, "", false
}

// authoredTextProblem finds bytes that can change terminal state or make authored knowledge
// look different from what a reviewer sees. Markdown bodies may use their three layout
// controls; frontmatter scalars are single-line metadata and may use none. Reading remains
// permissive, while validation and every generator that writes these values fail closed.
func authoredTextProblem(value string, allowLayout bool) (string, bool) {
	for _, char := range value {
		if problem, unsafe := authoredRuneProblem(char, allowLayout); unsafe {
			return problem, true
		}
	}
	return "", false
}

func authoredRuneProblem(char rune, allowLayout bool) (string, bool) {
	if name, invisible := invisibleRunes[char]; invisible {
		return fmt.Sprintf("contains invisible character U+%04X (%s)", char, name), true
	}
	layout := char == '\n' || char == '\r' || char == '\t'
	if unicode.IsControl(char) && (!allowLayout || !layout) {
		return fmt.Sprintf("contains control character U+%04X", char), true
	}
	return "", false
}

// markdownLiteralText serializes an authored scalar as one line of literal Markdown text.
// Frontmatter may come from an agent summarising untrusted collected content, so displaying it
// must not let newlines, raw HTML, links, headings, or autolinks become authored structure.
// Numeric entities preserve the rendered value while keeping Markdown-significant bytes out
// of the generated source. The Page itself retains the original parsed scalar.
func markdownLiteralText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	var builder strings.Builder
	for _, char := range value {
		if _, unsafe := authoredRuneProblem(char, false); unsafe {
			// Callers refuse unsafe authored values before writing. Keep this helper defensive so a
			// future display-only caller still renders the code point visibly rather than emitting it.
			fmt.Fprintf(&builder, "U&#43;%04X", char)
			continue
		}
		switch char {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '<', '>', '(', ')', '#', '+', '-', '.',
			'!', '|', '~', '&', ':', '@':
			fmt.Fprintf(&builder, "&#%d;", char)
		default:
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// markdownCodeText keeps a tag inside its one-line code span. Character references are not
// decoded inside code spans, so this deliberately preserves the valid lowercase kebab-case
// vocabulary and makes only invalid delimiter/control bytes visible. The build path refuses
// those values first; this is the display helper's defensive fallback.
func markdownCodeText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	var builder strings.Builder
	for _, char := range value {
		if _, unsafe := authoredRuneProblem(char, false); unsafe {
			fmt.Fprintf(&builder, "U+%04X", char)
			continue
		}
		switch char {
		case '`':
			builder.WriteString("&#96;")
		case '<':
			builder.WriteString("&lt;")
		default:
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// --- validation --------------------------------------------------------------------------

// Severity separates what blocks a write from what a reader should merely know.
type Severity string

const (
	// SeverityError is a page that is wrong: unparseable, nested, colliding, escaping.
	SeverityError Severity = "error"
	// SeverityWarning is a page that is incomplete: untagged, unknown tag, missing title.
	SeverityWarning Severity = "warning"
)

// Issue is one validation finding.
type Issue struct {
	URI      string   `json:"uri"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Line     int      `json:"line,omitempty"`
}

// ValidationReport is what `fkf validate wiki` and `fkf validate projects` return.
type ValidationReport struct {
	Layer    core.Layer `json:"layer"`
	Pages    int        `json:"pages"`
	Strict   bool       `json:"strict"`
	Errors   int        `json:"errors"`
	Warnings int        `json:"warnings"`
	Issues   []Issue    `json:"issues"`
	OK       bool       `json:"ok"`
}

// warn records something incomplete rather than wrong. Under --strict it is promoted, which is
// the whole difference between the two modes: reading stays permissive, writing does not.
func (r *ValidationReport) warn(uri string, line int, format string, args ...any) {
	severity := SeverityWarning
	if r.Strict {
		severity = SeverityError
	}
	r.Issues = append(r.Issues, Issue{URI: uri, Severity: severity, Line: line, Message: fmt.Sprintf(format, args...)})
}

func (r *ValidationReport) fail(uri string, line int, format string, args ...any) {
	r.Issues = append(r.Issues, Issue{URI: uri, Severity: SeverityError, Line: line, Message: fmt.Sprintf(format, args...)})
}

func (r *ValidationReport) finish() {
	sort.SliceStable(r.Issues, func(i, j int) bool {
		if r.Issues[i].URI != r.Issues[j].URI {
			return r.Issues[i].URI < r.Issues[j].URI
		}
		return r.Issues[i].Line < r.Issues[j].Line
	})
	for _, issue := range r.Issues {
		if issue.Severity == SeverityError {
			r.Errors++
		} else {
			r.Warnings++
		}
	}
	r.OK = r.Errors == 0
}

// ValidateMarkdownLayer applies the shared rules to one flat Markdown layer. `requireStatus`
// is what makes projects/ different from wiki/: a project with no status is not a project,
// it is a page nobody can act on.
func ValidateMarkdownLayer(ctx context.Context, base *Base, layer core.Layer, requireStatus, strict bool) (*ValidationReport, error) {
	if err := base.RequireLayer(layer); err != nil {
		return nil, err
	}
	report := &ValidationReport{Layer: layer, Strict: strict, Issues: []Issue{}}
	pages, nested, err := loadMarkdownLayer(ctx, base, layer)
	if err != nil {
		return nil, err
	}
	for _, entry := range nested {
		report.fail(entry, 0, "the %s layer is flat: move this page to %s/<slug>.md and classify it with tags", layer, layer)
	}
	seen := map[string]string{}
	for _, page := range pages {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		validatePage(report, page, layer, requireStatus, seen)
		validatePageRelations(base, report, page)
		validatePageLinks(ctx, base, report, page)
	}
	report.Pages = len(pages)
	report.finish()
	return report, nil
}

// validatePageRelations gives authored graph declarations the same early feedback as links.
// Graph construction remains the final cache boundary, but a page should not pass `validate`
// when its relation vocabulary or cardinality already contradicts the base schema.
func validatePageRelations(base *Base, report *ValidationReport, page Page) {
	names := make([]string, 0, len(page.Relations))
	for name := range page.Relations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition, declared := base.Config.Schema[name]
		if !declared {
			report.fail(page.URI, 0, "frontmatter relations.%s is not declared in fkf.yaml schema", name)
			continue
		}
		if !definition.Relation {
			report.fail(page.URI, 0, "frontmatter relations.%s is not declared as a relation", name)
			continue
		}
		values := page.Relations[name]
		if !definition.Cardinality.Allows(len(values)) {
			report.fail(page.URI, 0, "frontmatter relations.%s has %d values; cardinality %s does not allow that count",
				name, len(values), definition.Cardinality)
			continue
		}
		for _, candidate := range values {
			resolved, err := resolveAddressablePageLink(base, page.URI, candidate)
			if err != nil {
				report.fail(page.URI, 0, "frontmatter relations.%s URI %q: %v", name, candidate, err)
				continue
			}
			if err := core.ValidateRelationValue(resolved.NodeURI()); err != nil {
				report.fail(page.URI, 0, "frontmatter relations.%s URI %q: %v", name, candidate, err)
			}
		}
	}
}

func validatePage(report *ValidationReport, page Page, layer core.Layer, requireStatus bool, seen map[string]string) {
	if previous, collision := seen[strings.ToLower(page.Slug)]; collision {
		report.fail(page.URI, 0, "slug collides with %s; slugs are unique per layer", previous)
	}
	seen[strings.ToLower(page.Slug)] = page.URI

	// index.md and log.md are the bundle's structure rather than concepts in it: one is the
	// entry point and one is a dated stream, and neither is classified or tagged.
	structural := layer == core.LayerWiki && (page.Slug == "index" || page.Slug == "log")
	if page.Type == "" && !structural {
		report.warn(page.URI, 0, "frontmatter `type` is required on write (OKF v0.2); reading stays permissive")
	}
	if page.Title == "" {
		report.warn(page.URI, 0, "no title: add frontmatter `title` or a level-one heading")
	}
	if requireStatus && !isProjectStatus(page.Status) {
		report.fail(page.URI, 0, "frontmatter `status` is required and must be active, paused, or done (got %q)", page.Status)
	}
	if len(page.Tags) == 0 && !structural {
		report.warn(page.URI, 0, "no tags: the page is absent from tag-filtered navigation and harder to discover")
	}
	for _, tag := range page.Tags {
		if !tagPattern.MatchString(tag) {
			report.warn(page.URI, 0, "tag %q must be lowercase kebab-case", tag)
		}
	}
	if !slugPattern.MatchString(page.Slug) {
		report.fail(page.URI, 0, "slug %q must be lowercase letters, digits, dot, underscore, and hyphen", page.Slug)
	}
	if problem, unsafe := authoredTextProblem(page.Body, true); unsafe {
		report.fail(page.URI, 0, "body %s; remove it before writing", problem)
	}
	for _, heading := range page.Headings {
		if AnchorSlug(heading.Text) == "" {
			report.fail(page.URI, heading.Line, "heading has no addressable anchor; add a letter or number")
		}
	}
	validateFrontmatterText(report, page)
	if layer == core.LayerWiki {
		validateWikiSpecialPages(report, page)
	}
}

func validateFrontmatterText(report *ValidationReport, page Page) {
	var inspect func(string, any)
	inspect = func(field string, value any) {
		switch typed := value.(type) {
		case string:
			if problem, unsafe := authoredTextProblem(typed, false); unsafe {
				report.fail(page.URI, 0, "frontmatter %s %s; remove it before writing", field, problem)
			}
		case []any:
			for index, item := range typed {
				inspect(fmt.Sprintf("%s[%d]", field, index), item)
			}
		case []string:
			for index, item := range typed {
				inspect(fmt.Sprintf("%s[%d]", field, index), item)
			}
		case map[string]any:
			for _, key := range sortedKeys(typed) {
				if problem, unsafe := authoredTextProblem(key, false); unsafe {
					report.fail(page.URI, 0, "frontmatter key %q %s; remove it before writing", key, problem)
				}
				inspect(field+"."+key, typed[key])
			}
		}
	}
	for _, field := range sortedKeys(page.Frontmatter) {
		if problem, unsafe := authoredTextProblem(field, false); unsafe {
			report.fail(page.URI, 0, "frontmatter key %q %s; remove it before writing", field, problem)
		}
		inspect(field, page.Frontmatter[field])
	}
}

func validatePageLinks(ctx context.Context, base *Base, report *ValidationReport, page Page) {
	for _, link := range page.Links {
		if target := strings.TrimSpace(link.Target); target == "" {
			continue
		}
		resolved, err := resolveAddressablePageLink(base, page.URI, link.Target)
		if err != nil {
			report.fail(page.URI, link.Line, "link %q: %v", link.Target, err)
			continue
		}
		if resolved.Scheme != SchemeFile {
			continue
		}
		if !base.Exists(resolved.Path) {
			report.warn(page.URI, link.Line, "link %q points at %s, which does not exist", link.Target, resolved.Path)
			continue
		}
		if resolved.Fragment != "" || resolved.JQ != "" {
			// A file plus an invented child is not an address. Resolve it through the same
			// non-executing read boundary as `fkf read`, so Markdown headings, collected
			// record IDs, derived person IDs, and jq selections cannot drift into separate
			// validators.
			if _, err := resolveRead(ctx, base, resolved.String(), ReadOptions{}); err != nil {
				report.warn(page.URI, link.Line, "link %q is not addressable: %v", link.Target, err)
			}
		}
	}
}

// resolveAddressablePageLink keeps validation and graph construction on the same URI
// boundary. ParseURI closes the entity vocabulary; Store.Resolve closes the file grammar and
// layer activation. Existence remains a validation warning so forward links can be graphed.
func resolveAddressablePageLink(base *Base, from, target string) (URI, error) {
	trimmed := strings.TrimSpace(target)
	var (
		resolved URI
		err      error
	)
	if strings.HasPrefix(trimmed, "#") {
		resolved, err = ParseURI(from + trimmed)
	} else {
		resolved, err = ResolveLink(from, trimmed)
	}
	if err != nil {
		return URI{}, err
	}
	if resolved.Scheme == SchemeFile {
		if _, err := base.Store.Resolve(resolved.Path); err != nil {
			return URI{}, err
		}
	}
	return resolved, nil
}

// validateWikiSpecialPages enforces the two conventions that make a bundle navigable: the
// index is a page with a heading, and the log is a reverse-chronological list of ISO dates.
func validateWikiSpecialPages(report *ValidationReport, page Page) {
	switch page.Slug {
	case "index":
		if len(page.Headings) == 0 || page.Headings[0].Level != 1 {
			report.fail(page.URI, 0, "wiki/index.md must start with a level-one heading")
		}
	case "log":
		previous, seen := "", map[string]int{}
		for _, heading := range page.Headings {
			if heading.Level != 2 {
				continue
			}
			if !isoDatePattern.MatchString(heading.Text) {
				report.fail(page.URI, heading.Line, "wiki/log.md level-two headings are ISO dates; got %q", heading.Text)
				continue
			}
			if first, duplicate := seen[heading.Text]; duplicate {
				report.fail(page.URI, heading.Line, "wiki/log.md repeats the date %s, first seen at line %d", heading.Text, first)
			}
			seen[heading.Text] = heading.Line
			if previous != "" && heading.Text > previous {
				report.fail(page.URI, heading.Line, "wiki/log.md is newest first; %s follows %s", heading.Text, previous)
			}
			previous = heading.Text
		}
	}
}

func isProjectStatus(status string) bool {
	switch status {
	case "active", "paused", "done":
		return true
	default:
		return false
	}
}

// ProjectStatuses is the closed set a project page may declare.
var ProjectStatuses = []string{"active", "paused", "done"}

// Exists reports whether a base-relative path is present. It resolves through the store, so
// a path that escapes or names a disabled layer is absent rather than an error.
func (b *Base) Exists(relative string) bool {
	absolute, err := b.Store.Resolve(relative)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(absolute)
	return statErr == nil
}

// loadMarkdownLayer reads every page directly under a flat layer, and separately reports the
// nested ones so the validator can name them instead of silently skipping them.
func loadMarkdownLayer(ctx context.Context, base *Base, layer core.Layer) (pages []Page, nested []string, err error) {
	if err := checkContext(ctx); err != nil {
		return nil, nil, err
	}
	directory, err := base.Store.Dir(layer)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", directory, err)
	}
	for _, entry := range entries {
		if err := checkContext(ctx); err != nil {
			return nil, nil, err
		}
		if entry.IsDir() {
			nested = append(nested, path.Join(string(layer), entry.Name())+"/")
			continue
		}
		if !strings.HasSuffix(entry.Name(), core.MarkdownExtension) {
			continue
		}
		page, parseErr := ReadPageContext(ctx, base, path.Join(string(layer), entry.Name()))
		if parseErr != nil {
			return nil, nil, parseErr
		}
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].URI < pages[j].URI })
	return pages, nested, nil
}
