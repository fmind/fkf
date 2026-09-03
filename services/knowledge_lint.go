package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

// DefaultProjectStaleDays is the lint horizon for an active or paused project page.
const DefaultProjectStaleDays = 90

// ValidateKnowledgeLint performs advisory cross-page checks that need the complete authored
// corpus rather than one page in isolation.
func ValidateKnowledgeLint(
	ctx context.Context, base *Base, strict bool, staleDays int,
) (*ValidationReport, error) {
	if staleDays < 1 {
		return nil, fmt.Errorf("stale project horizon must be positive")
	}
	report := &ValidationReport{Layer: core.Layer("lint"), Strict: strict, Issues: []Issue{}}
	pages, err := lintPages(ctx, base)
	if err != nil {
		return nil, err
	}
	report.Pages = len(pages)
	known := make(map[string]Page, len(pages))
	for _, page := range pages {
		known[page.URI] = page
	}
	inbound := make(map[string]int)
	for _, page := range pages {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		lintRelativeDates(report, page)
		lintProjectFreshness(base, report, page, staleDays)
		if err := lintPageTargets(ctx, base, report, page, known, inbound); err != nil {
			return nil, err
		}
	}
	for _, page := range pages {
		if strings.HasPrefix(page.URI, string(core.LayerWiki)+"/") &&
			page.Slug != "index" && page.Slug != "log" && inbound[page.URI] == 0 {
			report.warn(page.URI, 0, "orphan wiki page has no explicit inbound link or relation outside the generated index")
		}
	}
	report.finish()
	return report, nil
}

func lintPages(ctx context.Context, base *Base) ([]Page, error) {
	var pages []Page
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if !base.Store.Enabled(layer) {
			continue
		}
		loaded, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		pages = append(pages, loaded...)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].URI < pages[j].URI })
	return pages, nil
}

func lintRelativeDates(report *ValidationReport, page Page) {
	for _, key := range sortedKeys(page.Frontmatter) {
		if !dateLikeFrontmatterKey(key) {
			continue
		}
		value, ok := frontmatterScalarString(page.Frontmatter[key])
		if ok && isRelativeAuthoredDate(value) {
			report.warn(page.URI, 0, "frontmatter `%s` uses relative date %q; write an absolute YYYY-MM-DD date", key, value)
		}
	}
}

func dateLikeFrontmatterKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "date" || key == "due" || key == "start" || key == "end" ||
		key == "valid_from" || key == "valid_until" || strings.HasSuffix(key, "_date") ||
		strings.HasSuffix(key, "_at")
}

func isRelativeAuthoredDate(value string) bool {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	if value == "today" || value == "yesterday" || value == "tomorrow" {
		return true
	}
	return strings.HasPrefix(value, "last ") || strings.HasPrefix(value, "next ") ||
		strings.HasPrefix(value, "this ") || strings.HasSuffix(value, " ago") ||
		strings.HasSuffix(value, " from now")
}

func lintProjectFreshness(base *Base, report *ValidationReport, page Page, staleDays int) {
	if !strings.HasPrefix(page.URI, string(core.LayerProjects)+"/") || page.Status == "done" {
		return
	}
	nowDate := base.Now().Format(time.DateOnly)
	if page.Status == "active" && page.ValidUntil != "" && page.ValidUntil < nowDate {
		report.warn(page.URI, 0, "active project valid_until %s is in the past", page.ValidUntil)
	}
	updated, err := time.Parse(time.RFC3339, page.Updated)
	if err != nil {
		return
	}
	age := int(base.Now().Sub(updated).Hours() / 24)
	if age > staleDays {
		report.warn(page.URI, 0, "project page is untouched for %d days; review or close it", age)
	}
}

func lintPageTargets(
	ctx context.Context, base *Base, report *ValidationReport, page Page,
	known map[string]Page, inbound map[string]int,
) error {
	if page.URI == "wiki/log.md" {
		return nil
	}
	links := page.Links
	if page.URI == indexPageURI() {
		var err error
		links, err = lintIndexLinks(page)
		if err != nil {
			return fmt.Errorf("inspect authored links in %s: %w", page.URI, err)
		}
	}
	for _, link := range links {
		lintPageTarget(ctx, base, report, page, link.Target, "link", false, known, inbound)
	}
	for name, values := range page.Relations {
		for _, value := range values {
			lintPageTarget(ctx, base, report, page, value, "relation "+name, name == "supersedes", known, inbound)
		}
	}
	return nil
}

func lintIndexLinks(page Page) ([]Link, error) {
	markers := markedBlockMarkers{
		begin: indexBlockBegin, beginPrefix: blockMarkerPrefix,
		end: blockEndMarker, endPrefix: blockEndMarker,
	}
	region, err := parseMarkedBlockRegion(page.Body, markers)
	if err != nil || !region.present {
		return page.Links, err
	}
	// The generated listing proves only that a page exists; it cannot prove that an author
	// intentionally connected that page. Parse the two authored regions together so curated
	// links on either side of the managed block retain their ordinary inbound semantics.
	authored := page.Body[:region.begin] + page.Body[region.end:]
	_, links := extractMarkdown(authored, 1)
	return links, nil
}

func lintPageTarget(
	ctx context.Context, base *Base, report *ValidationReport, page Page, target, via string, supersedes bool,
	known map[string]Page, inbound map[string]int,
) {
	resolved, err := resolveAddressablePageLink(base, page.URI, target)
	if err != nil {
		if supersedes {
			report.warn(page.URI, 0, "supersedes target %q is not an existing addressable page: %v", target, err)
		}
		return // The ordinary validator owns other malformed URI diagnostics.
	}
	if resolved.Scheme != SchemeFile {
		if supersedes {
			report.warn(page.URI, 0, "supersedes target %q must be a wiki or project page URI", target)
		}
		return
	}
	if resolved.Fragment != "" || resolved.JQ != "" {
		if supersedes {
			report.warn(page.URI, 0, "supersedes target %q must name a whole page", target)
			return
		}
		if !base.Exists(resolved.Path) {
			report.warn(page.URI, 0, "%s target %q does not exist", via, target)
			return
		}
		// A file existing does not prove that its heading, record, or jq child exists. Use
		// the same offline address resolver as read and Markdown-link validation.
		if _, err := resolveRead(ctx, base, resolved.String(), ReadOptions{}); err != nil {
			report.warn(page.URI, 0, "%s target %q is not addressable: %v", via, target, err)
			return
		}
		if _, exists := known[resolved.Path]; exists && resolved.Path != page.URI {
			inbound[resolved.Path]++
		}
		return
	}
	if _, exists := known[resolved.Path]; exists {
		if resolved.Path != page.URI {
			inbound[resolved.Path]++
		}
		return
	}
	if supersedes {
		report.warn(page.URI, 0, "supersedes target %q does not exist", target)
		return
	}
	if !base.Exists(resolved.Path) {
		report.warn(page.URI, 0, "%s target %q does not exist", via, target)
	}
}
