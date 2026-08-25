package services

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/fmind/fkf/core"
)

// NewKind is what kind of page or entry `fkf new` creates.
type NewKind string

const (
	NewKindTask    NewKind = "task"
	NewKindProject NewKind = "project"
	NewKindWiki    NewKind = "wiki"
	NewKindHelper  NewKind = "helper"
)

// ParseNewKind parses a user string into a NewKind.
func ParseNewKind(value string) (NewKind, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(NewKindTask), "t":
		return NewKindTask, nil
	case string(NewKindProject), "p":
		return NewKindProject, nil
	case string(NewKindWiki), "w":
		return NewKindWiki, nil
	case string(NewKindHelper), "h":
		return NewKindHelper, nil
	default:
		return "", fmt.Errorf("unknown kind %q; expected task, project, wiki, or helper", value)
	}
}

// NewRequest holds parameters for creating a new item.
type NewRequest struct {
	Kind  NewKind
	Slug  string
	Title string
	Type  string
	Tags  []string
	Now   func() time.Time
}

// NewResult is what `fkf new` returns.
type NewResult struct {
	Kind     NewKind  `json:"kind"`
	URI      string   `json:"uri,omitempty"`
	Path     string   `json:"path"`
	Created  bool     `json:"created"`
	Message  string   `json:"message"`
	Run      []string `json:"run,omitempty"`
	Requires []string `json:"requires,omitempty"`
}

// CreateNew creates a task trace, project page, wiki concept, or source helper.
func CreateNew(base *Base, req NewRequest) (*NewResult, error) {
	now := req.Now
	if now == nil {
		now = base.Now
	}
	today := now().Format(time.DateOnly)

	slug := strings.TrimSpace(req.Slug)
	if slug != "" && !slugPattern.MatchString(slug) {
		return nil, fmt.Errorf("%s slug %q must be one flat lowercase name using only letters, digits, dot, underscore, and hyphen",
			req.Kind, slug)
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = titleFromSlug(slug)
	}
	if err := validateNewText("title", title); err != nil {
		return nil, err
	}

	switch req.Kind {
	case NewKindTask:
		return createNewTask(base, slug, title, today)
	case NewKindProject:
		return createNewProject(base, slug, title, req.Tags)
	case NewKindWiki:
		return createNewWiki(base, slug, title, req.Type, req.Tags)
	case NewKindHelper:
		return createNewHelper(base, slug)
	default:
		return nil, fmt.Errorf("unknown new kind %q; want task, project, wiki, or helper", req.Kind)
	}
}

func createNewHelper(base *Base, name string) (*NewResult, error) {
	if name == "" {
		return nil, errors.New("helper name is required (e.g. `fkf new helper collect-prs`)")
	}
	content := helperTemplate(name)
	relPath := filepath.ToSlash(filepath.Join(core.BaseBinDir, name))
	absPath := filepath.Join(base.Root(), filepath.FromSlash(relPath))
	if err := core.ValidateWithinRoot(base.Root(), absPath); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(absPath); err == nil {
		return nil, fmt.Errorf("helper %s already exists", relPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect helper %s: %w", relPath, err)
	}
	// Helpers can inherit provider credentials, so generated executables remain owner-only.
	if err := core.WriteFileAtomicMode(absPath, []byte(content), 0o700); err != nil {
		return nil, err
	}
	return &NewResult{
		Kind: NewKindHelper, Path: absPath, Created: true,
		Message: "created helper at " + relPath,
		Run:     []string{name, "{{start}}", "{{end}}"}, Requires: []string{name},
	}, nil
}

func helperTemplate(name string) string {
	usage := name + " <start> <end>"
	return fmt.Sprintf("#!/bin/sh\nset -eu\n\n[ \"$#\" -eq 2 ] || { echo \"usage: %s\" >&2; exit 2; }\necho \"%s: not implemented\" >&2\nexit 1\n", usage, name)
}

func createNewTask(base *Base, slug, title, today string) (*NewResult, error) {
	if err := base.RequireLayer(core.LayerTasks); err != nil {
		return nil, err
	}
	if slug == "" {
		return nil, errors.New("task slug is required (e.g. `fkf new task my-feature`)")
	}
	relPath := filepath.ToSlash(filepath.Join(string(core.LayerTasks), today, slug, core.TaskTraceFile))
	absPath, err := base.Store.Resolve(relPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err == nil {
		return nil, fmt.Errorf("task trace %s already exists", relPath)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), core.BaseDirMode); err != nil {
		return nil, err
	}
	content := taskTemplate(title)
	if err := core.WriteFileAtomicMode(absPath, []byte(content), core.BaseFileMode); err != nil {
		return nil, err
	}
	return &NewResult{
		Kind:    NewKindTask,
		URI:     relPath,
		Path:    absPath,
		Created: true,
		Message: "created task trace at " + relPath,
	}, nil
}

func createNewProject(base *Base, slug, title string, tags []string) (*NewResult, error) {
	if err := base.RequireLayer(core.LayerProjects); err != nil {
		return nil, err
	}
	if slug == "" {
		return nil, errors.New("project slug is required (e.g. `fkf new project my-project`)")
	}
	relPath := filepath.ToSlash(filepath.Join(string(core.LayerProjects), slug+core.MarkdownExtension))
	absPath, err := base.Store.Resolve(relPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err == nil {
		return nil, fmt.Errorf("project page %s already exists", relPath)
	}
	tags, err = normalizeNewTags(tags)
	if err != nil {
		return nil, err
	}
	content, err := projectTemplate(title, tags)
	if err != nil {
		return nil, err
	}
	if err := validateGeneratedPage(core.LayerProjects, relPath, content, true); err != nil {
		return nil, err
	}
	if err := core.WriteFileAtomicMode(absPath, content, core.BaseFileMode); err != nil {
		return nil, err
	}
	return &NewResult{
		Kind:    NewKindProject,
		URI:     relPath,
		Path:    absPath,
		Created: true,
		Message: "created project page at " + relPath,
	}, nil
}

func createNewWiki(base *Base, slug, title, wikiType string, tags []string) (*NewResult, error) {
	if err := base.RequireLayer(core.LayerWiki); err != nil {
		return nil, err
	}
	if slug == "" {
		return nil, errors.New("wiki slug is required (e.g. `fkf new wiki my-concept`)")
	}
	if wikiType == "" {
		wikiType = "decision"
	}
	wikiType = strings.TrimSpace(wikiType)
	if !tagPattern.MatchString(wikiType) {
		return nil, fmt.Errorf("wiki type %q must be lowercase kebab-case", wikiType)
	}
	relPath := filepath.ToSlash(filepath.Join(string(core.LayerWiki), slug+core.MarkdownExtension))
	absPath, err := base.Store.Resolve(relPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err == nil {
		return nil, fmt.Errorf("wiki page %s already exists", relPath)
	}
	tags, err = normalizeNewTags(tags)
	if err != nil {
		return nil, err
	}
	content, err := wikiTemplate(wikiType, title, tags)
	if err != nil {
		return nil, err
	}
	if err := validateGeneratedPage(core.LayerWiki, relPath, content, false); err != nil {
		return nil, err
	}
	if err := core.WriteFileAtomicMode(absPath, content, core.BaseFileMode); err != nil {
		return nil, err
	}
	return &NewResult{
		Kind:    NewKindWiki,
		URI:     relPath,
		Path:    absPath,
		Created: true,
		Message: "created wiki page at " + relPath,
	}, nil
}

func titleFromSlug(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool {
		return r == '-' || r == '_' || r == '/'
	})
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, " ")
}

func normalizeNewTags(tags []string) ([]string, error) {
	clean := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, tag := range tags {
		for _, part := range strings.Split(tag, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if !tagPattern.MatchString(trimmed) {
				return nil, fmt.Errorf("tag %q must be lowercase kebab-case", trimmed)
			}
			if !seen[trimmed] {
				seen[trimmed] = true
				clean = append(clean, trimmed)
			}
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("at least one tag is required when writing a project or wiki page")
	}
	return clean, nil
}

func validateNewText(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if problem, unsafe := authoredTextProblem(value, false); unsafe {
		return fmt.Errorf("%s %s", field, problem)
	}
	return nil
}

func taskTemplate(title string) string {
	title = markdownLiteralText(title)
	return fmt.Sprintf(`# %s

## 1. %s

- **Request**: %s
- **Trace**:
  1. <!-- Record each completed step and its outcome. -->
- **Files**:
  - <!-- List each changed file, or write none. -->
- **Verification**:
  - <!-- Record each exact command and its result. -->

## Learned

<!-- Add durable lessons as bullets, or leave this section empty. -->
`, title, title, title)
}

type pageFrontmatter struct {
	Type   string   `yaml:"type"`
	Title  string   `yaml:"title"`
	Status string   `yaml:"status,omitempty"`
	Tags   []string `yaml:"tags,flow"`
}

func renderPageTemplate(frontmatter pageFrontmatter, title, body string) ([]byte, error) {
	encoded, err := yaml.Marshal(frontmatter)
	if err != nil {
		return nil, fmt.Errorf("encode page frontmatter: %w", err)
	}
	return []byte("---\n" + string(encoded) + "---\n\n# " + markdownLiteralText(title) + "\n" + body), nil
}

func projectTemplate(title string, tags []string) ([]byte, error) {
	return renderPageTemplate(pageFrontmatter{Type: "project", Title: title, Status: "active", Tags: tags}, title, `

## Intent

## Open questions

## Decisions
`)
}

func wikiTemplate(wikiType, title string, tags []string) ([]byte, error) {
	return renderPageTemplate(pageFrontmatter{Type: wikiType, Title: title, Tags: tags}, title, "")
}

func validateGeneratedPage(layer core.Layer, uri string, content []byte, requireStatus bool) error {
	page, err := ParsePage(uri, content, time.Time{})
	if err != nil {
		return fmt.Errorf("validate generated page: %w", err)
	}
	report := &ValidationReport{Layer: layer, Strict: true, Issues: []Issue{}}
	validatePage(report, page, layer, requireStatus, map[string]string{})
	report.finish()
	if report.OK {
		return nil
	}
	messages := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		messages = append(messages, issue.Message)
	}
	return fmt.Errorf("generated page violates the %s write contract: %s", layer, strings.Join(messages, "; "))
}
