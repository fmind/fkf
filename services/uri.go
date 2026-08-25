package services

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// Every node in a base has one relative URI, and there is one grammar for all of them:
//
//	<path>[?jq=<expr>][#<id|anchor>]     files, documents, records, headings
//	<scheme>:<identity>                  entities, which have no file of their own
//	https:                              external, verbatim
//
// Evaluation is path → fragment → jq, which is why `…json?jq=.headers#18c2a9f` reads as "the
// record 18c2a9f, then its headers". The grammar lives here and in skills/fkf-use/SKILL.md; the
// two must agree, and a test asserts every form in the skill's table parses.

// Scheme names the kind of thing a URI addresses.
type Scheme string

const (
	// SchemeFile addresses a path inside the base. It is the unprefixed form.
	SchemeFile Scheme = "file"
	// SchemeTag addresses a wiki or project tag.
	SchemeTag Scheme = "tag"
	// SchemeExternal addresses an HTTPS resource, kept verbatim.
	SchemeExternal Scheme = "external"
)

// ErrURI marks a malformed or out-of-bounds URI.
var ErrURI = errors.New("invalid URI")

var entitySchemePattern = regexp.MustCompile(`^([a-z][a-z0-9+.-]*):(.*)$`)

// URI is one parsed address.
type URI struct {
	Raw      string `json:"uri"`
	Scheme   Scheme `json:"scheme"`
	Path     string `json:"path,omitempty"`
	Dir      bool   `json:"directory,omitempty"`
	Fragment string `json:"fragment,omitempty"`
	JQ       string `json:"jq,omitempty"`
	Value    string `json:"value,omitempty"`
}

// ParseURI reads one URI in any of the grammar's forms.
func ParseURI(raw string) (URI, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return URI{}, fmt.Errorf("%w: empty", ErrURI)
	}
	lowered := strings.ToLower(trimmed)
	if strings.HasPrefix(lowered, "https://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, raw, err)
		}
		if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
			return URI{}, fmt.Errorf("%w: %q is not an absolute HTTPS URI", ErrURI, raw)
		}
		return URI{Raw: trimmed, Scheme: SchemeExternal, Value: trimmed}, nil
	}
	head, _ := splitURIExtras(trimmed)
	if strings.Contains(head, "://") {
		return URI{}, fmt.Errorf("%w: %q: external URIs must use https", ErrURI, raw)
	}
	if match := entitySchemePattern.FindStringSubmatch(trimmed); match != nil {
		scheme := Scheme(match[1])
		if err := core.ValidateEntityScheme(string(scheme)); err != nil {
			return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, raw, err)
		}
		value := strings.TrimSpace(match[2])
		if value == "" {
			return URI{}, fmt.Errorf("%w: %q names no %s", ErrURI, raw, scheme)
		}
		decoded, err := sources.DecodeFragment(value)
		if err != nil {
			return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, raw, err)
		}
		return newEntityURI(scheme, decoded)
	}
	return parseFileURI(trimmed)
}

// newEntityURI is the only constructor for an entity address. Provider identities are decoded
// values, while URI text is an encoded transport form; keeping that distinction here prevents
// a literal percent sign or control byte in provider data from corrupting graph rows.
func newEntityURI(scheme Scheme, value string) (URI, error) {
	if err := core.ValidateEntityScheme(string(scheme)); err != nil {
		return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, scheme, err)
	}
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return URI{}, fmt.Errorf("%w: names no %s", ErrURI, scheme)
	}
	if !utf8.ValidString(normalized) {
		return URI{}, fmt.Errorf("%w: %s value is not valid UTF-8", ErrURI, scheme)
	}
	uri := URI{Scheme: scheme, Value: normalized}
	uri.Raw = uri.String()
	return uri, nil
}

func entityURIString(scheme Scheme, value string) (string, error) {
	uri, err := newEntityURI(scheme, value)
	if err != nil {
		return "", err
	}
	return uri.String(), nil
}

func parseFileURI(trimmed string) (URI, error) {
	uri := URI{Scheme: SchemeFile}
	rest := trimmed
	// The fragment is what follows the LAST '#': a record identity percent-encodes '#' and a
	// heading anchor cannot hold one, so the only way to reach this split ambiguously is a jq
	// program containing a literal '#', which the grammar asks you to percent-encode.
	if hash := strings.LastIndex(rest, "#"); hash >= 0 {
		if hash == len(rest)-1 {
			return URI{}, fmt.Errorf("%w: %q names no fragment", ErrURI, trimmed)
		}
		fragment, err := sources.DecodeFragment(rest[hash+1:])
		if err != nil {
			return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, trimmed, err)
		}
		uri.Fragment, rest = fragment, rest[:hash]
	}
	if query := strings.Index(rest, "?"); query >= 0 {
		expression := rest[query+1:]
		rest = rest[:query]
		if !strings.HasPrefix(expression, "jq=") {
			return URI{}, fmt.Errorf("%w: %q: the only supported query is ?jq=<expr>", ErrURI, trimmed)
		}
		decoded, err := url.QueryUnescape(strings.TrimPrefix(expression, "jq="))
		if err != nil {
			return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, trimmed, err)
		}
		if strings.TrimSpace(decoded) == "" {
			return URI{}, fmt.Errorf("%w: %q: ?jq= names no expression", ErrURI, trimmed)
		}
		uri.JQ = decoded
	}
	cleaned, err := core.CleanRelative(rest)
	if err != nil {
		return URI{}, fmt.Errorf("%w: %q: %w", ErrURI, trimmed, err)
	}
	if cleaned == "." {
		return URI{}, fmt.Errorf("%w: %q: the base root is not addressable", ErrURI, trimmed)
	}
	uri.Dir = strings.HasSuffix(cleaned, "/")
	uri.Path = strings.TrimSuffix(cleaned, "/")
	if uri.Dir && (uri.Fragment != "" || uri.JQ != "") {
		return URI{}, fmt.Errorf("%w: %q: a directory has no fragment or jq expression", ErrURI, trimmed)
	}
	uri.Raw = uri.String()
	return uri, nil
}

// String renders the canonical form, which parses back to an identical URI.
func (u URI) String() string {
	switch u.Scheme {
	case SchemeExternal:
		return u.Value
	case SchemeFile:
		// Rendered below.
	default:
		return string(u.Scheme) + ":" + sources.EncodeFragment(u.Value)
	}
	rendered := u.Path
	if u.Dir {
		rendered += "/"
	}
	if u.JQ != "" {
		rendered += "?jq=" + url.QueryEscape(u.JQ)
	}
	if u.Fragment != "" {
		rendered += "#" + sources.EncodeFragment(u.Fragment)
	}
	return rendered
}

// IsEntity reports whether the URI names something with no file of its own.
func (u URI) IsEntity() bool {
	return u.Scheme != SchemeFile && u.Scheme != SchemeExternal
}

// NodeURI is the identity this URI has in the graph: the path and, when present, the record or
// heading fragment — but never the jq expression, because a `?jq=` URI is a read form rather
// than a thing that exists. Keeping the fragment is what makes a single RECORD a node, which is
// the whole reason `graph <…>.json#<id>` answers anything.
func (u URI) NodeURI() string {
	if u.Scheme != SchemeFile {
		return u.String()
	}
	rendered := u.Path
	if u.Dir {
		rendered += "/"
	}
	if u.Fragment != "" {
		rendered += "#" + sources.EncodeFragment(u.Fragment)
	}
	return rendered
}

// FileURI drops both the fragment and the jq expression, leaving the file the URI is about.
func (u URI) FileURI() string {
	if u.Scheme != SchemeFile {
		return u.String()
	}
	if u.Dir {
		return u.Path + "/"
	}
	return u.Path
}

// ResolveLink maps a link written inside a Markdown file — where targets are relative to the
// linking file, so editors and GitHub resolve them — to a base-relative URI. A link that
// escapes the base is rejected, never clamped.
func ResolveLink(fromRelative, target string) (URI, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return URI{}, fmt.Errorf("%w: %q is a same-page anchor", ErrURI, target)
	}
	head, tail := splitURIExtras(trimmed)
	// The absolute test looks at the PATH only, never the `?jq=`/`#` tail. A scheme in the
	// fragment is data — an RSS or notification record whose declared id is its own URL — so
	// `../events/<date>/rss.json#https://example.test/post` is an ordinary relative link.
	// Testing the whole string made every such link "absolute", which then failed to parse as
	// one and was reported as escaping the base, dropping the edge and blaming the author.
	if strings.Contains(head, "://") || hasEntityPrefix(head) || strings.HasPrefix(head, "mailto:") {
		return ParseURI(trimmed)
	}
	if strings.HasPrefix(trimmed, "/") {
		return ParseURI(strings.TrimPrefix(trimmed, "/"))
	}
	joined := path.Join(path.Dir(fromRelative), head)
	return ParseURI(joined + tail)
}

func hasEntityPrefix(value string) bool {
	return entitySchemePattern.MatchString(value)
}

// splitURIExtras separates the path from the `?jq=`/`#` tail, so path joining never mangles a
// fragment that legitimately contains a slash.
func splitURIExtras(value string) (head, tail string) {
	cut := len(value)
	if query := strings.Index(value, "?"); query >= 0 && query < cut {
		cut = query
	}
	if hash := strings.Index(value, "#"); hash >= 0 && hash < cut {
		cut = hash
	}
	return value[:cut], value[cut:]
}

// RelativeLink renders a base-relative URI as it should be written inside a Markdown file, so
// the link resolves in an editor and on GitHub as well as through fkf.
func RelativeLink(fromRelative, target string) string {
	head, tail := splitURIExtras(target)
	from := path.Dir(fromRelative)
	if from == "." {
		return head + tail
	}
	relative := relativePath(from, head)
	return relative + tail
}

func relativePath(from, to string) string {
	fromParts := strings.Split(from, "/")
	toParts := strings.Split(to, "/")
	common := 0
	for common < len(fromParts) && common < len(toParts) && fromParts[common] == toParts[common] {
		common++
	}
	parts := make([]string, 0, len(fromParts)-common+len(toParts)-common)
	for range fromParts[common:] {
		parts = append(parts, "..")
	}
	parts = append(parts, toParts[common:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// AnchorSlug renders a heading the way GitHub does, which is what makes a `#heading` fragment
// in a base resolve identically in an editor, on GitHub, and through `fkf read`.
func AnchorSlug(heading string) string {
	var builder strings.Builder
	heading = visibleHeadingText(heading)
	for _, char := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case unicode.IsLetter(char), unicode.IsNumber(char), unicode.IsMark(char), char == '-', char == '_':
			builder.WriteRune(char)
		case char == ' ':
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

// visibleHeadingText removes inline Markdown structure before deriving the fragment. GitHub
// anchors the rendered label of a link or image, not its destination, and ignores HTML tags.
// Generated headings also encode punctuation as numeric entities so untrusted scalar text
// cannot become structure; decode those entities before applying the same visible-text rule.
func visibleHeadingText(heading string) string {
	return renderedInlineMarkdown(heading)
}
