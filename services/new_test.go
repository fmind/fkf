package services_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func fixedTime() time.Time {
	t, _ := time.Parse(time.DateOnly, "2026-08-24")
	return t
}

func TestParseNewKind(t *testing.T) {
	cases := []struct {
		input string
		want  services.NewKind
		err   bool
	}{
		{"task", services.NewKindTask, false},
		{"t", services.NewKindTask, false},
		{"project", services.NewKindProject, false},
		{"p", services.NewKindProject, false},
		{"wiki", services.NewKindWiki, false},
		{"w", services.NewKindWiki, false},
		{"unknown", "", true},
	}
	for _, tc := range cases {
		got, err := services.ParseNewKind(tc.input)
		if (err != nil) != tc.err {
			t.Errorf("ParseNewKind(%q) err = %v, wantErr = %v", tc.input, err, tc.err)
		}
		if got != tc.want {
			t.Errorf("ParseNewKind(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCreateNewTask(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindTask,
		Slug: "agent-eval",
		Now:  fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantURI := "tasks/2026-08-24/agent-eval/TASKS.md"
	if result.URI != wantURI {
		t.Fatalf("URI = %q, want %q", result.URI, wantURI)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "# Agent Eval") || !strings.Contains(content, "## Learned") {
		t.Fatalf("content = %q, want task template", content)
	}
	for lineNumber, line := range strings.Split(content, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Fatalf("line %d has trailing whitespace: %q", lineNumber+1, line)
		}
	}
	for _, marker := range []string{"- **Trace**:", "- **Files**:", "- **Verification**:", "## Learned"} {
		if !strings.Contains(content, marker) {
			t.Fatalf("content = %q, want task scaffold marker %q", content, marker)
		}
	}

	// Duplicate
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindTask,
		Slug: "agent-eval",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error creating duplicate task")
	}

	// Empty slug
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindTask,
		Slug: "",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error with empty slug")
	}
}

func TestCreateNewProject(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{
		Kind:  services.NewKindProject,
		Slug:  "my-project",
		Title: "Custom Title",
		Tags:  []string{"fkf", "agent"},
		Now:   fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantURI := "projects/my-project.md"
	if result.URI != wantURI {
		t.Fatalf("URI = %q, want %q", result.URI, wantURI)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "title: Custom Title") || !strings.Contains(content, "tags: [fkf, agent]") {
		t.Fatalf("content = %q, want project template", content)
	}

	// Duplicate
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindProject,
		Slug: "my-project",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error on duplicate project")
	}

	// Empty slug
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindProject,
		Slug: "",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error on empty slug")
	}
}

func TestCreateNewProjectEncodesFrontmatterScalars(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	title := `Project Nebula: Northport's [chapter](https://injected.example.test) "launch"`
	result, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindProject, Slug: "nebula-northport", Title: title, Tags: []string{"nebula"}, Now: fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := services.ReadPage(base, result.URI)
	if err != nil {
		t.Fatalf("ReadPage() error = %v, generated frontmatter must parse", err)
	}
	if page.Title != title {
		t.Fatalf("title = %q, want %q", page.Title, title)
	}
	if len(page.Links) != 0 {
		t.Fatalf("generated heading turned title metadata into Markdown links: %+v", page.Links)
	}
	report, err := services.ValidateMarkdownLayer(t.Context(), base, core.LayerProjects, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("strict validation = %+v, generated page must satisfy the write contract", report)
	}
}

func TestCreateNewPageHeadingFragmentRoundTripsAfterLiteralEncoding(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindProject, Slug: "fix-fk-412", Title: "Fix FK-412: Northport's launch",
		Tags: []string{"nebula"}, Now: fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "fix-fk-412-northports-launch"
	page, err := services.ReadPage(base, result.URI)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Headings) == 0 || page.Headings[0].Anchor != anchor {
		t.Fatalf("headings = %+v, want the rendered GitHub anchor %q", page.Headings, anchor)
	}
	section, err := services.Read(t.Context(), base, result.URI+"#"+anchor, services.ReadOptions{})
	if err != nil {
		t.Fatalf("Read(generated fragment) error = %v", err)
	}
	if section.Kind != "section" {
		t.Fatalf("kind = %q, want section", section.Kind)
	}
}

func TestCreateNewRejectsUnsafeOrInvalidMetadataBeforeWriting(t *testing.T) {
	for _, test := range []struct {
		name string
		req  services.NewRequest
	}{
		{name: "nested slug", req: services.NewRequest{Kind: services.NewKindProject, Slug: "nebula/northport", Tags: []string{"nebula"}}},
		{name: "parent slug", req: services.NewRequest{Kind: services.NewKindWiki, Slug: "../outside", Tags: []string{"nebula"}}},
		{name: "newline title", req: services.NewRequest{Kind: services.NewKindProject, Slug: "nebula", Title: "Nebula\nInjected", Tags: []string{"nebula"}}},
		{name: "invalid tag", req: services.NewRequest{Kind: services.NewKindProject, Slug: "nebula", Tags: []string{"Nebula Northport"}}},
		{name: "missing tags", req: services.NewRequest{Kind: services.NewKindWiki, Slug: "nebula"}},
		{name: "invalid type", req: services.NewRequest{Kind: services.NewKindWiki, Slug: "nebula", Type: "bad: type", Tags: []string{"nebula"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newBase(t, baseConfig, nil)
			if _, err := services.CreateNew(base, test.req); err == nil {
				t.Fatal("CreateNew() succeeded, want invalid metadata refused")
			}
			projects, _ := services.ListPages(t.Context(), base, core.LayerProjects, services.PageFilter{})
			wiki, _ := services.ListPages(t.Context(), base, core.LayerWiki, services.PageFilter{})
			if (projects != nil && projects.Total != 0) || (wiki != nil && wiki.Total != 0) {
				t.Fatal("CreateNew() wrote a page before rejecting invalid metadata")
			}
		})
	}
}

func TestCreateNewWiki(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{
		Kind:  services.NewKindWiki,
		Slug:  "decision-rule",
		Type:  "decision",
		Title: "Decision Rule",
		Tags:  []string{"arch"},
		Now:   fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantURI := "wiki/decision-rule.md"
	if result.URI != wantURI {
		t.Fatalf("URI = %q, want %q", result.URI, wantURI)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "type: decision") || !strings.Contains(content, "title: Decision Rule") {
		t.Fatalf("content = %q, want wiki template", content)
	}

	// Duplicate
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindWiki,
		Slug: "decision-rule",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error on duplicate wiki page")
	}

	// Empty slug
	if _, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindWiki,
		Slug: "",
		Now:  fixedTime,
	}); err == nil {
		t.Fatal("expected error on empty wiki slug")
	}
}

func TestCreateNewHelperUsesPortablePrivateScaffold(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindHelper,
		Slug: "collect-prs.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URI != "" {
		t.Fatalf("URI = %q, want no read URI for an executable helper", result.URI)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"uri"`) {
		t.Fatalf("JSON = %s, want unavailable helper URI omitted", encoded)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#!/bin/sh\nset -eu\n") {
		t.Fatalf("helper = %q, want portable fail-closed /bin/sh scaffold", data)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("helper mode = %04o, want 0700", got)
	}
	if len(result.Requires) != 1 || result.Requires[0] != "collect-prs.sh" {
		t.Fatalf("requires = %q, want helper executable only", result.Requires)
	}
	if _, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindHelper, Slug: "collect-prs.sh"}); err == nil {
		t.Fatal("CreateNew() succeeded for an existing helper")
	}
	if _, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindHelper, Slug: "collect-prs"}); err == nil {
		t.Fatal("CreateNew() accepted a helper without an explicit .sh or .py extension")
	}
}

func TestCreateNewPythonHelperUsesExplicitInterpreterContract(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	result, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindHelper, Slug: "collect-prs.py"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#!/usr/bin/env python3\n") {
		t.Fatalf("python helper = %q, want explicit python3 shebang", data)
	}
	if !slices.Equal(result.Requires, []string{"collect-prs.py", "python3"}) {
		t.Fatalf("requires = %q, want helper and its non-standard interpreter", result.Requires)
	}
}

func TestCreateNewDisabledLayer(t *testing.T) {
	const config = `name: brain
layers: {events: true, index: true, tasks: false, projects: false, wiki: false}
`
	base := newBase(t, config, nil)
	if _, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindTask, Slug: "a"}); err == nil {
		t.Fatal("expected error on disabled tasks layer")
	}
	if _, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindProject, Slug: "a"}); err == nil {
		t.Fatal("expected error on disabled projects layer")
	}
	if _, err := services.CreateNew(base, services.NewRequest{Kind: services.NewKindWiki, Slug: "a"}); err == nil {
		t.Fatal("expected error on disabled wiki layer")
	}
	if _, err := services.CreateNew(base, services.NewRequest{Kind: "invalid", Slug: "a"}); err == nil {
		t.Fatal("expected error on invalid kind")
	}
}
