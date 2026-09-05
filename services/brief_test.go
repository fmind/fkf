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
  owner: {description: Assigned owner., cardinality: many, relation: true}
  repository: {description: Repository., cardinality: optional, relation: true}
identities:
  owner:
    canonical: person:email/owner@example.test
    aliases: [actor:code.example/owner]
    owner: true
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  schedule-events:
    enabled: true
    layer: events
    run: [provider, schedule]
    auth: [provider, login]
    fields: {id: .id, time: .time, title: .title}
  work-items:
    enabled: true
    layer: events
    run: [provider, work]
    fields: {id: .id, time: .updated, title: .summary, owner: [".owners[]"]}
  stale-feed:
    enabled: true
    layer: index
    run: [provider, stale]
    max_age_hours: 12
    fields: {id: .id, time: .time, title: .title}
`

func TestBriefIsOfflineEvenForATrustedBaseWithAuthCommands(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	collectBriefSource(t, base, "schedule-events", "2026-05-10",
		`[{"id":"today","time":"2026-05-10T14:00:00Z","title":"Current planning"}]`)
	trust(t, base)
	runner := &fakeRunner{err: authExitFailure{}}
	base.Runner = runner

	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("brief executed %d provider command(s); readiness belongs to status --live", len(runner.calls))
	}
	section := briefSection(t, report, "today")
	if section.Total != 1 || section.Items[0].Title != "Current planning" {
		t.Fatalf("today = %+v, want stored evidence without a provider call", section)
	}
}

func TestBriefReturnsEmptyRecentSectionsWithoutEventEvidence(t *testing.T) {
	const config = `fkf: 1
name: empty-brief
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
	for _, name := range []string{"today", "yesterday"} {
		section := briefSection(t, report, name)
		if section.Total != 0 || len(section.Items) != 0 {
			t.Fatalf("%s section = %+v, want a valid empty section", name, section)
		}
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
		t.Fatalf("clock reads = %d receipt=%+v; want one shared evaluation instant", clockReads, report.Receipt)
	}
}

func TestBriefComposesGenericEvidenceAuthoredWorkAndReceipt(t *testing.T) {
	base := newBase(t, briefConfig, &fakeRunner{})
	collectBriefSource(t, base, "schedule-events", "2026-05-10",
		`[{"id":"meeting","time":"2026-05-10T09:00:00Z","title":"Daily planning"}]`)
	collectBriefSource(t, base, "work-items", "2026-05-09",
		`[{"id":"work-7","updated":"2026-05-09T10:00:00Z","summary":"Finish bounded delivery","owners":["actor:code.example/owner"]}]`)
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
	runner := &fakeRunner{err: authExitFailure{}}
	base.Runner = runner

	report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for name, minimum := range map[string]int{
		"attention": 1, "today": 1, "tasks_due": 1, "yesterday": 1, "active_projects": 1,
	} {
		section := briefSection(t, report, name)
		if section.Total < minimum || len(section.Items) < minimum {
			t.Fatalf("section %s = %+v, want at least %d complete item(s)", name, section, minimum)
		}
	}
	if len(runner.calls) != 0 || report.Receipt.Owner != "person:email/owner@example.test" ||
		report.Receipt.Unharvested != 1 || report.Receipt.InputDigest == "" || report.Receipt.BriefVersion != 2 {
		t.Fatalf("runner calls=%d receipt=%+v", len(runner.calls), report.Receipt)
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

func TestBriefRecentEvidenceIsProviderNeutral(t *testing.T) {
	config := func(name string) string {
		return strings.ReplaceAll(briefConfig, "schedule-events", name)
	}
	for _, name := range []string{"alpha-stream", "beta-stream"} {
		t.Run(name, func(t *testing.T) {
			base := newBase(t, config(name), &fakeRunner{})
			collectBriefSource(t, base, name, "2026-05-10",
				`[{"id":"one","time":"2026-05-10T09:30:00Z","title":"Equivalent planning fact"}]`)
			report, err := services.Brief(t.Context(), base, services.BriefRequest{Budget: 4096})
			if err != nil {
				t.Fatal(err)
			}
			section := briefSection(t, report, "today")
			if section.Total != 1 || section.Items[0].Title != "Equivalent planning fact" ||
				section.Items[0].Time != "2026-05-10T09:30:00Z" || section.Items[0].Detail != name {
				t.Fatalf("today = %+v, want projected evidence grouped by its declared source", section)
			}
		})
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
