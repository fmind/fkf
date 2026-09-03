package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
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

const (
	demoDailyRecords = 6
	demoHelperName   = "fkf-demo-json.sh"
)

const demoHelper = `#!/bin/sh
set -eu

[ "$#" -eq 2 ] || { echo "usage: fkf-demo-json.sh <source> <date>" >&2; exit 2; }
source=$1
date=$2
case "$source" in
  github-pull-requests|google-calendar-events|google-gmail-emails|jira-issues|git-commits|shell-commands) ;;
  *) echo "unknown synthetic source: $source" >&2; exit 2 ;;
esac
case "$date" in
  ????-??-??) ;;
  *) echo "date must be YYYY-MM-DD" >&2; exit 2 ;;
esac
case "$date" in
  *[!0-9-]*) echo "date must be YYYY-MM-DD" >&2; exit 2 ;;
esac

printf '[{"id":"%s-%s-0","time":"%s","title":"Synthetic %s activity","url":"https://example.test/demo/%s/%s","repo":"repo:github.com/fmind/fkf","author":"person:email/demo@example.test","attendees":["person:email/demo@example.test"],"from":"person:email/demo@example.test","to":["person:email/reader@example.test"],"assignee":"person:email/demo@example.test","ticket":"ticket:DEMO-1"}]\n' \
  "$source" "$date" "$date" "$source" "$source" "$date"
`

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
	created []initCreatedFile
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

func demoConfigBlock() string {
	var builder strings.Builder
	builder.WriteString("# The demo documents are generated locally during init. These matching declarations stay\n")
	builder.WriteString("# disabled until the owner explicitly chooses to continue the synthetic timeline.\n")
	builder.WriteString("sources:\n")
	for _, source := range demoSources() {
		fmt.Fprintf(&builder, "  %s:\n", source.Name)
		builder.WriteString("    enabled: false\n")
		fmt.Fprintf(&builder, "    layer: %s\n", source.Layer)
		fmt.Fprintf(&builder, "    requires: [%s]\n", demoHelperName)
		fmt.Fprintf(&builder, "    run: [%s, %s, \"{{date}}\"]\n", demoHelperName, source.Name)
		builder.WriteString("    fields:\n")
		for _, name := range source.Fields.Names() {
			paths := source.Fields.Paths(name)
			fmt.Fprintf(&builder, "      %s: ", name)
			if len(paths) == 1 {
				builder.WriteString(strconv.Quote(paths[0].String()))
			} else {
				builder.WriteByte('[')
				for index, fieldPath := range paths {
					if index > 0 {
						builder.WriteString(", ")
					}
					builder.WriteString(strconv.Quote(fieldPath.String()))
				}
				builder.WriteByte(']')
			}
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func installDemoHelper(root string) (bool, error) {
	target := filepath.Join(root, core.BaseBinDir, demoHelperName)
	if _, err := os.Lstat(target); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect demo helper: %w", err)
	}
	if err := core.WriteFileAtomicMode(target, []byte(demoHelper), 0o700); err != nil {
		return false, fmt.Errorf("write demo helper: %w", err)
	}
	return true, nil
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
	staged, cleanup, err := newDemoStageBase(base)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	report, err := writeDemoInPlace(ctx, staged, days)
	if err != nil {
		return nil, err
	}
	created, err := publishDemoArtifacts(ctx, staged, base)
	if err != nil {
		return nil, err
	}
	report.Base = base.Root()
	report.created = created
	return report, nil
}

func writeDemoInPlace(ctx context.Context, base *Base, days int) (*DemoReport, error) {
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
	// Generate wiki/index.md first because it is graph and lexical input. The demo fixture then
	// anchors every searchable input before either cache captures stat metadata.
	if _, err := Build(ctx, base, "wiki", false); err != nil {
		return nil, err
	}
	if err := anchorDemoLexicalInputTimes(ctx, base, now); err != nil {
		return nil, err
	}
	if _, err := Build(ctx, base, "graph", false); err != nil {
		return nil, err
	}
	if _, err := BuildLexicalIndex(ctx, base); err != nil {
		return nil, err
	}
	return report, nil
}

func newDemoStageBase(base *Base) (*Base, func(), error) {
	stageParent := filepath.Join(base.Root(), ".agents", "tmp")
	if err := core.ValidateDirectoryConfinement(stageParent); err != nil {
		return nil, nil, err
	}
	_, statErr := os.Lstat(stageParent)
	parentCreated := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !parentCreated {
		return nil, nil, fmt.Errorf("inspect demo staging directory: %w", statErr)
	}
	if err := os.MkdirAll(stageParent, core.BaseDirMode); err != nil {
		return nil, nil, fmt.Errorf("create demo staging directory: %w", err)
	}
	stageRoot, err := os.MkdirTemp(stageParent, "init-demo-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create demo staging base: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(stageRoot)
		if parentCreated {
			_ = os.Remove(stageParent)
		}
	}
	configBytes, err := core.ReadFileLimit(base.Config.Path, core.MaxControlFileBytes)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read demo base configuration: %w", err)
	}
	stageConfig := *base.Config
	stageConfig.Path = filepath.Join(stageRoot, core.ConfigFileName)
	stageConfig.LocalPath = ""
	if err := core.WriteFileAtomicMode(stageConfig.Path, configBytes, core.BaseFileMode); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("stage demo base configuration: %w", err)
	}
	staged := *base
	staged.Config = &stageConfig
	staged.Store = stageConfig.Store()
	staged.Env = sources.NewEnvironment(&stageConfig)
	return &staged, cleanup, nil
}

type demoArtifact struct {
	relative string
	source   string
	target   string
}

func publishDemoArtifacts(ctx context.Context, staged, target *Base) ([]initCreatedFile, error) {
	artifacts, err := collectDemoArtifacts(ctx, staged, target)
	if err != nil {
		return nil, err
	}
	created := make([]initCreatedFile, 0, len(artifacts))
	rollback := func(cause error) ([]initCreatedFile, error) {
		return nil, errors.Join(cause, rollbackInitCreatedFiles(created))
	}
	for _, artifact := range artifacts {
		if err := checkContext(ctx); err != nil {
			return rollback(err)
		}
		published, err := publishDemoArtifact(target.Root(), artifact)
		if published.path != "" {
			created = append(created, published)
		}
		if err != nil {
			return rollback(err)
		}
	}
	return created, nil
}

func collectDemoArtifacts(ctx context.Context, staged, target *Base) ([]demoArtifact, error) {
	artifacts := []demoArtifact{}
	err := filepath.WalkDir(staged.Root(), func(current string, entry fs.DirEntry, walkErr error) error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: staged demo entry %s is not a regular file", core.ErrUnsafePath, current)
		}
		relative, err := filepath.Rel(staged.Root(), current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == core.ConfigFileName {
			return nil
		}
		targetPath, err := demoArtifactTarget(target, relative)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(targetPath); err == nil {
			return fmt.Errorf("%s already holds %s; `--demo` only fills an empty base", target.Root(), relative)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect demo target %s: %w", relative, err)
		}
		artifacts = append(artifacts, demoArtifact{relative: relative, source: current, target: targetPath})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].relative < artifacts[j].relative })
	return artifacts, nil
}

func demoArtifactTarget(base *Base, relative string) (string, error) {
	if relative != LexicalIndexPath && relative != lexicalIndexMetaPath {
		return base.Store.Resolve(relative)
	}
	target := filepath.Join(base.Root(), filepath.FromSlash(relative))
	if err := core.ValidateWithinRoot(base.Root(), target); err != nil {
		return "", err
	}
	return target, nil
}

func publishDemoArtifact(root string, artifact demoArtifact) (initCreatedFile, error) {
	// Revalidate after the publication preflight so a newly introduced parent symlink cannot
	// redirect the target-directory temporary file outside the base.
	if err := core.ValidateWithinRoot(root, artifact.target); err != nil {
		return initCreatedFile{}, err
	}
	input, err := os.Open(artifact.source)
	if err != nil {
		return initCreatedFile{}, fmt.Errorf("open staged demo artifact %s: %w", artifact.relative, err)
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return initCreatedFile{}, fmt.Errorf("inspect staged demo artifact %s: %w", artifact.relative, err)
	}
	if !info.Mode().IsRegular() {
		return initCreatedFile{}, fmt.Errorf("%w: staged demo artifact %s is not a regular file",
			core.ErrUnsafePath, artifact.relative)
	}
	directory := filepath.Dir(artifact.target)
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		return initCreatedFile{}, fmt.Errorf("create demo artifact directory for %s: %w", artifact.relative, err)
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(artifact.target)+".*.tmp")
	if err != nil {
		return initCreatedFile{}, fmt.Errorf("stage demo artifact %s: %w", artifact.relative, err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	copied, copyErr := io.Copy(temporary, input)
	if copyErr != nil || copied != info.Size() {
		_ = temporary.Close()
		if copyErr != nil {
			return initCreatedFile{}, fmt.Errorf("copy demo artifact %s: %w", artifact.relative, copyErr)
		}
		return initCreatedFile{}, fmt.Errorf("copy demo artifact %s: copied %d bytes, expected %d",
			artifact.relative, copied, info.Size())
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return initCreatedFile{}, fmt.Errorf("set demo artifact %s mode: %w", artifact.relative, err)
	}
	if err := os.Chtimes(temporaryPath, info.ModTime(), info.ModTime()); err != nil {
		_ = temporary.Close()
		return initCreatedFile{}, fmt.Errorf("set demo artifact %s time: %w", artifact.relative, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return initCreatedFile{}, fmt.Errorf("sync demo artifact %s: %w", artifact.relative, err)
	}
	if err := temporary.Close(); err != nil {
		return initCreatedFile{}, fmt.Errorf("close demo artifact %s: %w", artifact.relative, err)
	}
	temporaryInfo, err := os.Lstat(temporaryPath)
	if err != nil {
		return initCreatedFile{}, fmt.Errorf("inspect staged publication for %s: %w", artifact.relative, err)
	}
	if err := os.Link(temporaryPath, artifact.target); err != nil {
		return initCreatedFile{}, fmt.Errorf("publish demo artifact %s: %w", artifact.relative, err)
	}
	created := initCreatedFile{path: artifact.target, info: temporaryInfo}
	if err := os.Remove(temporaryPath); err != nil {
		return created, fmt.Errorf("remove staged publication for %s: %w", artifact.relative, err)
	}
	if err := core.SyncDirectory(directory); err != nil {
		return created, fmt.Errorf("sync demo artifact directory for %s: %w", artifact.relative, err)
	}
	return created, nil
}

func anchorDemoLexicalInputTimes(ctx context.Context, base *Base, now time.Time) error {
	paths, err := lexicalInputPaths(ctx, base)
	if err != nil {
		return err
	}
	for _, relative := range paths {
		absolute, err := lexicalInputAbsolute(base, relative)
		if err != nil {
			return err
		}
		if err := os.Chtimes(absolute, now, now); err != nil {
			return fmt.Errorf("anchor demo input %s: %w", relative, err)
		}
	}
	return nil
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
