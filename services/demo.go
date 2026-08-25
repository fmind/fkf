package services

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf init --demo N` writes a base you can read before giving fkf access to anything. Every
// stored value is a pure function of (local evaluation day, source, date, index), so the same
// N on the same local day produces byte-identical content — which is what lets the retrieval
// smoke test assert recall and makes the README's output reproducible.

const demoDailyRecords = 6

var (
	demoIdentities = []string{
		"marc@example.test", "ines@example.test", "tomas@example.test",
		"lea@example.test", "nadia@example.test", "raf@example.test",
		"zoe@example.test", "noah@example.test", "maya@example.test",
		"eli@example.test", "sara@example.test", "luc@example.test",
	}
	demoRepos = []string{
		"fmind/fkf", "fmind/atlas", "acme/ledger", "acme/gateway",
		"acme/search", "acme/console", "example/agents", "example/docs",
	}
	demoTickets = []string{
		"FK-412", "FK-418", "LG-77", "GW-1203", "FK-501",
	}
	demoTopics = []string{
		"retrieval boundary", "token budget receipt", "declarative source runner",
		"graph edge extraction", "trust gate for cloned bases", "lazy body fetching",
		"daily collection window", "typed identity relations", "URI grammar",
		"markdown validation", "index staleness", "quiet source watchdog",
	}
	demoVerbs = []string{"design", "review", "measure", "fix", "document", "revert"}
)

var demoSchema = core.FieldSchema{
	core.FieldID:    {Description: "Stable synthetic record identity.", Cardinality: core.CardinalityOne},
	core.FieldTime:  {Description: "Synthetic event timestamp.", Cardinality: core.CardinalityOne},
	core.FieldTitle: {Description: "Human-readable synthetic event title.", Cardinality: core.CardinalityOptional},
	core.FieldURL:   {Description: "Provider page for the synthetic event.", Cardinality: core.CardinalityOptional, Relation: true},
	"repository":    {Description: "Repository associated with the event.", Cardinality: core.CardinalityOptional, Relation: true},
	"participant":   {Description: "Actors participating in the event, expressed as typed URIs.", Cardinality: core.CardinalityMany, Relation: true},
	"ticket":        {Description: "Work item associated with the event.", Cardinality: core.CardinalityOptional, Relation: true},
}

// DemoSource is one synthetic source and the shape of the records it writes.
type DemoSource struct {
	Name   string
	Layer  core.Layer
	Fields sources.Fields
}

// DemoReport is what `--demo` returns.
type DemoReport struct {
	Base    string   `json:"base"`
	Days    int      `json:"days"`
	Sources []string `json:"sources"`
	Records int      `json:"records"`
	Pages   int      `json:"pages"`
	Since   string   `json:"since"`
	Until   string   `json:"until"`
}

func demoSources() []DemoSource {
	field := func(raw string) core.FieldPath {
		parsed, err := core.ParseFieldPath(raw)
		if err != nil {
			panic("demo field path " + raw + ": " + err.Error())
		}
		return parsed
	}
	common := func(participants ...string) sources.Fields {
		paths := make(core.FieldPaths, 0, len(participants))
		for _, raw := range participants {
			paths = append(paths, field(raw))
		}
		return sources.Fields{
			core.FieldID: {field(".id")}, core.FieldTime: {field(".time")}, core.FieldTitle: {field(".title")},
			core.FieldURL: {field(".url")}, "repository": {field(".repo")},
			"participant": paths, "ticket": {field(".ticket")},
		}
	}
	return []DemoSource{
		{Name: "github-pull-requests", Layer: core.LayerEvents, Fields: common(".author")},
		{Name: "google-calendar-events", Layer: core.LayerEvents, Fields: common(".attendees[]")},
		{Name: "google-gmail-emails", Layer: core.LayerEvents, Fields: common(".from", ".to[]")},
		{Name: "jira-issues", Layer: core.LayerEvents, Fields: common(".assignee")},
		{Name: "git-commits", Layer: core.LayerEvents, Fields: common(".author")},
		{Name: "shell-commands", Layer: core.LayerEvents, Fields: sources.Fields{
			core.FieldID: {field(".id")}, core.FieldTime: {field(".time")}, core.FieldTitle: {field(".title")},
		}},
	}
}

// WriteDemo fills an empty base with synthetic documents and pages. It refuses a base that
// already holds collected content: a demo that quietly mixed into real records would be
// indistinguishable from them a week later.
func WriteDemo(ctx context.Context, base *Base, days int) (*DemoReport, error) {
	if err := validateDemoDays(days); err != nil {
		return nil, err
	}
	if occupied, err := firstDemoLayerEntry(base); err != nil {
		return nil, err
	} else if occupied != "" {
		return nil, fmt.Errorf("%s already holds %s; `--demo` only fills an empty base", base.Root(), occupied)
	}
	report := &DemoReport{Base: base.Root(), Days: days}
	evaluation := base.Now()
	completedDays, err := previousCompletedDays(evaluation, days)
	if err != nil {
		return nil, fmt.Errorf("plan demo days: %w", err)
	}
	// Invocation time and zone are not knowledge. Anchor every generated timestamp to the
	// caller's captured local calendar day in UTC, then give all nested builders that same clock.
	now, err := time.Parse(time.DateOnly, evaluation.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("anchor demo evaluation day: %w", err)
	}
	demoBase := *base
	demoBase.Now = func() time.Time { return now }
	base = &demoBase
	for _, source := range demoSources() {
		report.Sources = append(report.Sources, source.Name)
	}
	for _, day := range completedDays {
		date := day.Format(time.DateOnly)
		if report.Since == "" {
			report.Since = date
		}
		report.Until = date
		for _, source := range demoSources() {
			document := demoDocument(source, date, now)
			if err := base.WriteDocument(document); err != nil {
				return nil, err
			}
			report.Records += document.Count
		}
	}
	pages, err := writeDemoPages(base, now)
	if err != nil {
		return nil, err
	}
	report.Pages = pages
	// A bare build owns the dependency order: wiki/index.md is graph input, so generating only
	// the graph here would leave a brand-new demo stale before its first command.
	if _, err := Build(ctx, base, "", false); err != nil {
		return nil, err
	}
	return report, nil
}

func firstDemoLayerEntry(base *Base) (string, error) {
	for _, layer := range core.Layers {
		if !base.Store.Enabled(layer) {
			continue
		}
		directory, err := base.Store.Dir(layer)
		if err != nil {
			return "", err
		}
		var occupied string
		err = filepath.WalkDir(directory, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if current == directory || entry.IsDir() {
				return nil
			}
			relative, relErr := filepath.Rel(base.Root(), current)
			if relErr != nil {
				return relErr
			}
			occupied = filepath.ToSlash(relative)
			return fs.SkipAll
		})
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect demo layer %s: %w", layer, err)
		}
		if occupied != "" {
			return occupied, nil
		}
	}
	return "", nil
}

func validateDemoDays(days int) error {
	if days < 1 || days > 366 {
		return fmt.Errorf("%w: --demo takes 1..366 days (got %d)", core.ErrConfig, days)
	}
	return nil
}

func demoDocument(source DemoSource, date string, now time.Time) *sources.Document {
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		panic("validated demo date " + date + ": " + err.Error())
	}
	window := sources.DayWindow(day)
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: source.Name, Layer: source.Layer, Date: date,
		WindowStart: window.Start, WindowEnd: window.End,
		CollectedAt: now.UTC().Format(time.RFC3339),
		Schema:      demoSchema.Select(source.Fields), Fields: source.Fields, Records: []sources.Record{},
	}
	for index := range demoDailyRecords {
		document.Records = append(document.Records, demoRecord(source.Name, date, index))
	}
	document.Count = len(document.Records)
	return document
}

// demoRecord derives every field from a stable hash of (source, date, index), so a record for
// the same synthetic slot is identical everywhere and a retrieval assertion is meaningful.
func demoRecord(source, date string, index int) sources.Record {
	seed := demoSeed(source, date, index)
	topic := demoTopics[seed%len(demoTopics)]
	ticket := demoTickets[(seed/3)%len(demoTickets)]
	repoName := demoRepos[(seed/5)%len(demoRepos)]
	repo := "repo:github.com/" + repoName
	author := "person:email/" + demoIdentities[(seed/7)%len(demoIdentities)]
	other := "person:email/" + demoIdentities[(seed/11)%len(demoIdentities)]
	verb := demoVerbs[(seed/13)%len(demoVerbs)]
	id := fmt.Sprintf("%s-%s-%d", source, strings.ReplaceAll(date, "-", ""), index)
	record := sources.Record{
		"id": id,
		// A civil date is the only timestamp that belongs to the same labelled day in every
		// time zone. Synthetic records must stay byte-identical and valid when a demo base is
		// created in UTC+14, UTC-12, or moved between them.
		"time": date,
		"title": fmt.Sprintf("%s %s (%s)",
			strings.ToUpper(verb[:1])+verb[1:], topic, ticket),
	}
	switch source {
	case "shell-commands":
		record["title"] = fmt.Sprintf("fkf find --grep %q", strings.Fields(topic)[0])
		record["cwd"] = "/home/demo/" + path.Base(repoName)
		record["exit"] = 0
		return record
	case "google-calendar-events":
		record["ticket"] = "ticket:" + ticket
		record["url"] = "https://calendar.example.test/event/" + id
		record["repo"] = repo
		record["attendees"] = []any{author, other}
		record["location"] = "remote"
		return record
	case "google-gmail-emails":
		record["ticket"] = "ticket:" + ticket
		record["url"] = "https://mail.example.test/thread/" + id
		record["repo"] = repo
		record["from"] = author
		record["to"] = []any{other}
		record["snippet"] = fmt.Sprintf("Following up on %s for %s — see the thread for the decision.", topic, ticket)
		return record
	case "git-commits":
		record["ticket"] = "ticket:" + ticket
		record["url"] = fmt.Sprintf("https://github.test/%s/commit/%08x", repoName, seed)
		record["repo"] = repo
		record["author"] = author
		return record
	case "jira-issues":
		record["ticket"] = "ticket:" + ticket
		record["id"] = ticket + "-" + strconv.Itoa(index)
		record["url"] = "https://jira.example.test/browse/" + ticket
		record["repo"] = repo
		record["assignee"] = author
		record["status"] = []string{"open", "in progress", "done"}[seed%3]
		return record
	default: // github-pull-requests
		record["ticket"] = "ticket:" + ticket
		record["url"] = fmt.Sprintf("https://github.test/%s/pull/%d", repoName, 100+seed%900)
		record["repo"] = repo
		record["author"] = author
		record["state"] = []string{"OPEN", "MERGED", "CLOSED"}[seed%3]
		return record
	}
}

// demoSeed is a small deterministic hash. It is not cryptographic and does not need to be:
// its only job is to spread twelve topics over N days identically on every machine.
func demoSeed(source, date string, index int) int {
	seed := 2166136261
	for _, char := range source + date + strconv.Itoa(index) {
		seed = (seed ^ int(char)) * 16777619 & 0x7fffffff
	}
	return seed
}

func writeDemoPages(base *Base, now time.Time) (int, error) {
	pages := map[core.Layer]map[string]string{
		core.LayerWiki: {
			"index.md": `# Wiki

The durable knowledge in this demo base.

- [Retrieval boundary](retrieval-boundary.md)
- [Declarative sources](declarative-sources.md)
- [Log](log.md)
`,
			"retrieval-boundary.md": `---
type: decision
title: Retrieval boundary
description: Why retrieval is lexical and reproducible rather than semantic.
tags: [decision, retrieval]
relations:
  related:
    - declarative-sources.md
---

# Retrieval boundary

Ranking is lexical and deterministic so the same query against an unchanged base, with the same
binary and evaluation day, returns the same pack. A receipt that says "cosine 0.83" explains
nothing a reader can check, and a model in the read path makes reproducibility impossible. See
[FK-412](../projects/fkf-rebuild.md).
`,
			"declarative-sources.md": `---
type: pattern
title: Declarative source runner
description: A source is direct argv in the base's own configuration, not Go code.
tags: [pattern, collection]
---

# Declarative source runner

The CLI a source names already holds the login, so fkf reads no credential. Adding a source is
eight lines of YAML and no Go.
`,
			"log.md": fmt.Sprintf(`# Log

## %s

- Demo base generated; every record is synthetic.
`, now.Format(time.DateOnly)),
		},
		core.LayerProjects: {
			"fkf-rebuild.md": `---
type: project
title: fkf rebuild
status: active
tags: [fkf, architecture]
relations:
  related:
    - ../wiki/retrieval-boundary.md
  participant:
    - person:email/marc@example.test
  ticket:
    - ticket:FK-412
---

# fkf rebuild

## Intent

Replace provider packages with declarative commands and make the base the configuration.

## Open questions

- Retrieval boundary for [FK-412](ticket:FK-412), waiting on [Marc](person:email/marc@example.test)

## Decisions

- Sources are commands; the base is the configuration.
`,
			"ledger-migration.md": `---
type: project
title: Ledger migration
status: done
tags: [acme, migration]
---

# Ledger migration

Completed migration of acme/ledger. Kept for the record; see LG-77.
`,
		},
	}
	written := 0
	for layer, files := range pages {
		if !base.Store.Enabled(layer) {
			continue
		}
		directory, err := base.Store.Dir(layer)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			return 0, err
		}
		for name, body := range files {
			target := filepath.Join(directory, name)
			if _, err := os.Stat(target); err == nil {
				continue
			}
			if err := core.WriteFileAtomicMode(target, []byte(body), core.BaseFileMode); err != nil {
				return 0, err
			}
			written++
		}
	}
	return written, nil
}
