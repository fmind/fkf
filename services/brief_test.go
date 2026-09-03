package services_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

const briefConfig = `fkf: 1
name: brief-test
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human title., cardinality: optional}
  url: {description: Provider URL., cardinality: optional}
  owner: {description: Assigned owner., cardinality: many, relation: true}
  repository: {description: Repository., cardinality: optional, relation: true}
identities:
  owner:
    canonical: person:email/owner@example.test
    aliases: [actor:github.com/owner]
    owner: true
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  google-calendar-agenda:
    enabled: true
    layer: index
    run: [provider, agenda, "{{start}}", "{{end}}", "{{date}}", "{{next_date}}"]
    fields: {id: .id, time: .time, title: .title}
  google-calendar-events:
    enabled: true
    layer: events
    run: [provider, calendar]
    auth: [provider, login]
    fields: {id: .id, time: .time, title: .title}
  github-pull-requests:
    enabled: true
    layer: events
    run: [provider, prs]
    fields: {id: .url, time: .time, title: .title, url: .url, owner: [".assignee_uris[]"]}
  github-issues:
    enabled: true
    layer: events
    run: [provider, issues]
    fields: {id: .url, time: .time, title: .title, url: .url, owner: [".assignee_uris[]"]}
  github-runs:
    enabled: true
    layer: events
    run: [provider, runs]
    fields: {id: .url, time: .time, title: .title, url: .url, repository: .repository_uri}
  google-tasks-items:
    enabled: true
    layer: events
    run: [provider, tasks]
    fields: {id: .uid, time: .updated, title: .title, url: .webViewLink}
  stale-feed:
    enabled: true
    layer: events
    run: [provider, stale]
    fields: {id: .id, time: .time, title: .title}
`

func TestSyncPopulatesTodaysCalendarFromTheCurrentAgendaSnapshot(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"provider agenda": `[{"id":"today","time":"2026-05-10T14:00:00Z","title":"Current agenda"}]`,
	}}
	base := newBase(t, briefConfig, runner)
	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{
		Targets: []string{"google-calendar-agenda"}, Days: 1, NoGraph: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 1 || len(runner.calls) != 1 {
		t.Fatalf("agenda sync = %+v, calls=%d; want one current snapshot", report, len(runner.calls))
	}
	wantWindow := "2026-05-10T00:00:00Z 2026-05-11T00:00:00Z 2026-05-10 2026-05-11"
	if !strings.Contains(runner.calls[0].Display(), wantWindow) {
		t.Fatalf("agenda command = %q, want current local-day placeholders %q", runner.calls[0].Display(), wantWindow)
	}

	brief, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	section := briefSection(t, brief, "today_calendar")
	if section.Total != 1 || section.Items[0].Title != "Current agenda" {
		t.Fatalf("today calendar = %+v, want the synced agenda snapshot", section)
	}
}

func TestBriefReturnsAnEmptyCalendarWhenNoCalendarSourceIsDeclared(t *testing.T) {
	const config = `fkf: 1
name: no-calendar
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
layers: {events: true, index: true}
sources: {}
`
	base := newBase(t, config, &fakeRunner{})
	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	section := briefSection(t, report, "today_calendar")
	if section.Total != 0 || len(section.Items) != 0 {
		t.Fatalf("calendar section = %+v, want a valid empty section", section)
	}
}

func TestBriefBindsEverySectionToOneEvaluationInstant(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	clockReads := 0
	base.Now = func() time.Time {
		clockReads++
		return testClock.AddDate(0, 0, clockReads-1)
	}
	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if clockReads != 1 || report.Receipt.AsOf != "2026-05-10" {
		t.Fatalf("clock reads = %d, receipt = %+v; want one shared evaluation instant", clockReads, report.Receipt)
	}
}

func TestBriefComposesTheDailyControlSurfaceAndReceipt(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	collectBriefSource(t, base, "google-calendar-events", "2026-05-10",
		`[{"id":"meeting","time":"2026-05-10T09:00:00Z","title":"Daily planning"}]`)
	collectBriefSource(t, base, "github-pull-requests", "2026-05-09",
		`[{"url":"https://github.com/fmind/fkf/pull/7","time":"2026-05-09T10:00:00Z","title":"Finish delta packs","state":"OPEN","assignee_uris":["actor:github.com/owner"]}]`)
	collectBriefSource(t, base, "github-issues", "2026-05-09",
		`[{"url":"https://github.com/fmind/fkf/issues/8","time":"2026-05-09T11:00:00Z","title":"Already done","state":"CLOSED","assignee_uris":["actor:github.com/owner"]}]`)
	collectBriefSource(t, base, "github-runs", "2026-05-09",
		`[{"url":"https://github.com/fmind/fkf/actions/runs/9","time":"2026-05-09T12:00:00Z","title":"test","workflowName":"test","conclusion":"failure","repository_uri":"repo:github.com/fmind/fkf"}]`)
	write(t, base, "tasks/2026-05-10/delta/TASKS.md", `---
title: Finish the daily brief
status: active
due: 2026-05-10
---

# Finish the daily brief

## Learned

- Keep one receipt for both output formats.
`)
	write(t, base, "projects/fkf.md", `---
type: project
title: FKF
status: active
tags: [fkf]
---

# FKF
`)
	projectTouched := testClock.AddDate(0, 0, -2)
	if err := os.Chtimes(mustResolve(t, base, "projects/fkf.md"), projectTouched, projectTouched); err != nil {
		t.Fatal(err)
	}
	trust(t, base)
	base.Runner = &fakeRunner{err: authExitFailure{}}

	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for name, minimum := range map[string]int{
		"attention": 3, "today_calendar": 1, "tasks_due": 1, "failing_ci": 1,
		"open_items": 1, "yesterday": 1, "active_projects": 1,
	} {
		section := briefSection(t, report, name)
		if section.Total < minimum || len(section.Items) < minimum {
			t.Fatalf("section %s = %+v, want at least %d complete item(s)", name, section, minimum)
		}
	}
	if report.Receipt.Owner != "person:email/owner@example.test" || !report.Receipt.AuthChecked ||
		!strings.Contains(strings.Join(report.Receipt.AuthRequired, " "), "google-calendar-events") ||
		report.Receipt.Unharvested != 1 || report.Receipt.InputDigest == "" {
		t.Fatalf("receipt = %+v", report.Receipt)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > report.Receipt.Budget*4 || len(services.RenderBriefText(report)) > report.Receipt.Budget*4 {
		t.Fatalf("brief exceeds budget: json=%d text=%d limit=%d", len(encoded)+1,
			len(services.RenderBriefText(report)), report.Receipt.Budget*4)
	}
}

func TestBriefIncludesANewProjectTouchedThisWeek(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	created, err := services.CreateNew(base, services.NewRequest{
		Kind: services.NewKindProject, Slug: "fresh-project", Title: "Fresh project",
		Tags: []string{"fresh"}, Now: func() time.Time { return testClock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(created.Path, testClock, testClock); err != nil {
		t.Fatal(err)
	}

	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	section := briefSection(t, report, "active_projects")
	if section.Total != 1 || section.Items[0].URI != "projects/fresh-project.md" ||
		section.Items[0].Detail != "touched 2026-05-10" {
		t.Fatalf("active projects = %+v, want the newly written project", section)
	}
}

func TestBriefReportsARetryableMinimumBudget(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	trust(t, base)
	_, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 1})
	var budgetErr *services.BriefBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Minimum <= 1 {
		t.Fatalf("Brief() error = %v, want an exact minimum", err)
	}
	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: budgetErr.Minimum})
	if err != nil {
		t.Fatalf("retry at minimum %d: %v", budgetErr.Minimum, err)
	}
	if report.Receipt.UsedTokens > budgetErr.Minimum {
		t.Fatalf("retry used %d tokens of %d", report.Receipt.UsedTokens, budgetErr.Minimum)
	}
}

func TestBriefTasksDueIncludesLatestCollectedGoogleTaskState(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	collectBriefSource(t, base, "google-tasks-items", "2026-05-09", `[
		{"uid":"list~closed","updated":"2026-05-09T08:00:00Z","title":"Old open state","due":"2026-05-10T00:00:00Z","status":"needsAction","webViewLink":"https://tasks.example/closed"},
		{"uid":"list~active","updated":"2026-05-09T09:00:00Z","title":"Ship FKF","due":"2026-05-10T00:00:00Z","status":"needsAction","webViewLink":"https://tasks.example/active"},
		{"uid":"list~future","updated":"2026-05-09T10:00:00Z","title":"Future task","due":"2026-05-11T00:00:00Z","status":"needsAction","webViewLink":"https://tasks.example/future"}
	]`)
	collectBriefSource(t, base, "google-tasks-items", "2026-05-10", `[
		{"uid":"list~closed","updated":"2026-05-10T07:00:00Z","title":"Old open state","due":"2026-05-10T00:00:00Z","status":"completed","webViewLink":"https://tasks.example/closed"}
	]`)
	trust(t, base)

	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	section := briefSection(t, report, "tasks_due")
	if section.Total != 1 || section.Items[0].Title != "Ship FKF" ||
		!strings.Contains(section.Items[0].Detail, "google-tasks-items") {
		t.Fatalf("tasks due = %+v, want only the active collected task due today", section)
	}
}

func briefSection(t *testing.T, report *services.BriefReport, name string) services.BriefSection {
	t.Helper()
	for _, section := range report.Sections {
		if section.Name == name {
			return section
		}
	}
	t.Fatalf("brief has no %s section: %+v", name, report.Sections)
	return services.BriefSection{}
}

func collectBriefSource(t *testing.T, base *services.Base, name, date, records string) {
	t.Helper()
	source, err := base.Source(name)
	if err != nil {
		t.Fatal(err)
	}
	day, err := sources.ParseDay(date)
	if err != nil {
		t.Fatal(err)
	}
	document, err := sources.Collect(t.Context(), &fakeRunner{responses: map[string]string{"": records}},
		source, base.Env, sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatalf("collect %s: %v", name, err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}
