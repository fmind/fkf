package main

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func writeUpgradeText(w *textWriter, report *services.UpgradeReport) {
	if report.Updated {
		w.printf("updated: %s -> %s\n", report.Previous, report.Current)
	} else {
		w.printf("up to date: %s\n", report.Current)
	}
	w.printf("path: %s\n", report.Path)
	if report.PrecededBy != "" {
		w.printf("warning: %s precedes %s on PATH; invoke %s or reorder PATH\n",
			report.PrecededBy, report.Path, report.Path)
	}
}

// writeConfigText answers "what does this base collect, and what did I actually get" — the
// resolved values plus the file each one came from, which is the whole reason to ask.
func writeConfigText(w *textWriter, config *core.Config) {
	w.printf("%s  %s\n", config.Name, config.Path)
	if config.LocalPath != "" {
		w.printf("overlay: %s\n", config.LocalPath)
	}
	layers := make([]string, 0, len(config.Layers))
	for _, layer := range slices.Sorted(maps.Keys(config.Layers)) {
		if config.Layers[layer] {
			layers = append(layers, string(layer))
		}
	}
	w.printf("layers:  %s\n", strings.Join(layers, " "))
	w.printf("sync:    %d day(s), timeout %s, concurrency %d, index stale after %dh\n",
		config.Sync.Days, config.Sync.Timeout, config.Sync.Concurrency, config.Sync.IndexMaxAgeHours)
	if len(config.Bin) > 0 {
		w.printf("bin:     %s\n", strings.Join(config.Bin, " "))
	}
	names := config.SourceNames()
	w.printf("\n%d source(s), %d enabled\n", len(names), len(config.EnabledSources()))
	for _, name := range names {
		source := config.Sources[name]
		state := "off"
		if source.Enabled {
			state = "on"
		}
		w.printf("%-24s %-4s %-8s %s\n", name, state, source.Layer, strings.Join(source.Requires, ", "))
	}
	if len(config.Origins) > 0 {
		w.line("")
		for _, key := range slices.Sorted(maps.Keys(config.Origins)) {
			w.printf("%-24s from %s\n", key, config.Origins[key])
		}
	}
}

func writeSyncText(w *textWriter, report *services.SyncReport) {
	if report.NothingDue {
		w.printf("nothing due (%s)\n", report.Elapsed)
		return
	}
	if report.Preview != nil {
		preview := report.Preview
		when := preview.Date
		if when == "" {
			when = "snapshot"
		}
		w.printf("preview %s (%s, %s): %d record(s)\n", preview.Source, preview.Kind, when, preview.Count)
		for _, record := range preview.Sample {
			w.printf("  %s\n", record.URI)
			if record.Title != "" {
				w.printf("      %s\n", inline(record.Title))
			}
		}
		w.printf("\nvalidated in %s; nothing written\n", report.Elapsed)
		return
	}
	for _, unit := range report.Units {
		label := unit.Source
		if unit.Date != "" {
			label = unit.Date + " " + unit.Source
		}
		w.printf("%-36s %-16s", label, unit.Outcome)
		switch {
		case unit.Outcome == services.OutcomeWritten:
			w.printf(" %5d record(s)  %s", unit.Count, unit.Elapsed)
		case unit.Outcome == services.OutcomePlanned:
			w.printf(" %s", unit.Command)
		case unit.Error != "":
			w.printf(" %s", unit.Error)
		}
		if unit.BodiesCached > 0 {
			w.printf("  %d body/bodies cached", unit.BodiesCached)
		}
		if unit.BodyFailures > 0 {
			w.printf("  %s", unit.BodyError)
		}
		w.line("")
	}
	w.printf("\n%d written, %d skipped, %d failed, %d record(s), %d body/bodies cached, %d body failure(s) in %s\n",
		report.Written, report.Skipped, report.Failed, report.Records, report.BodiesCached, report.BodyFailed, report.Elapsed)
	if len(report.AuthRequired) > 0 {
		w.printf("auth required: %s\n", strings.Join(report.AuthRequired, ", "))
	}
	if report.Graph != nil {
		w.printf("graph: %d edge(s) (%s)\n", report.Graph.Edges, report.Graph.Mode)
	}
	if report.Index != nil {
		writeLexicalIndexBuildText(w, report.Index)
	}
}

func writeSourceTestText(w *textWriter, report *services.SourceTestReport) {
	for _, result := range report.Sources {
		w.printf("%-24s %-7s %s", result.Source, result.Outcome, result.Elapsed)
		if result.Error != "" {
			w.printf("  %s", result.Error)
		}
		w.line("")
	}
	w.printf("\n%d passed, %d failed in %s\n", report.Passed, report.Failed, report.Elapsed)
}

func writeInitText(w *textWriter, report *services.InitReport) {
	verb := "refreshed"
	if report.Created {
		verb = "created"
	}
	w.printf("%s %s\n", verb, report.Base)
	for _, step := range report.Steps {
		marker := " "
		if step.Changed {
			marker = "+"
		}
		w.printf(" %s %-18s %s\n", marker, step.Item, step.Detail)
	}
	if len(report.Next) > 0 {
		w.line("\nnext")
		for index, step := range report.Next {
			w.printf("  %d. %s\n", index+1, step)
		}
	}
}

func writeHelpersText(w *textWriter, report *services.HelperReport) {
	for _, helper := range report.Helpers {
		detail := string(helper.State)
		if helper.Required {
			detail += ", required"
		}
		if helper.Refreshed {
			detail += ", refreshed"
		}
		w.printf("%-28s %s\n", helper.Path, detail)
		if helper.State != services.HelperCurrent {
			w.printf("  current: %s\n  shipped: %s\n", orDash(short(helper.CurrentSHA256)), short(helper.ShippedSHA256))
		}
	}
	w.printf("\n%d current, %d drifted, %d missing, %d refreshed\n",
		report.Current, report.Drifted, report.Missing, report.Refreshed)
}

func writeBuildText(w *textWriter, report *services.BuildReport) {
	if report.NothingStale {
		w.line("nothing stale")
		return
	}
	if report.Graph != nil {
		writeGraphBuildText(w, report.Graph)
	}
	if report.Wiki != nil {
		writeWikiIndexText(w, report.Wiki)
	}
	if report.Bodies != nil {
		w.printf("%s: pruned %d cached body or bodies (%d bytes)\n", report.Bodies.Message, report.Bodies.Pruned, report.Bodies.Bytes)
	}
	if report.Index != nil {
		writeLexicalIndexBuildText(w, report.Index)
	}
}

func writeLexicalIndexBuildText(w *textWriter, report *services.LexicalIndexBuild) {
	w.printf("%s  %d entries, %d postings, %d bytes in %s (%s)\n",
		report.URI, report.Entries, report.Postings, report.Bytes, report.Elapsed, report.Mode)
}

func writeGraphBuildText(w *textWriter, typed *services.GraphBuild) {
	w.printf("%s  %d edges from %d document(s) and %d page(s) in %s (%s)\n",
		typed.URI, typed.Edges, typed.Documents, typed.Pages, typed.Elapsed, typed.Mode)
}

func writeNewText(w *textWriter, result *services.NewResult) {
	if result.URI == "" {
		w.line(result.Message)
	} else {
		w.printf("%s (%s)\n", result.Message, result.URI)
	}
	if len(result.Run) > 0 {
		w.printf("run: [%s]\n", yamlFlowStrings(result.Run))
	}
	if len(result.Requires) > 0 {
		w.printf("requires: [%s]\n", yamlFlowStrings(result.Requires))
	}
}

func yamlFlowStrings(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		// JSON string literals are valid YAML and keep placeholders from becoming flow mappings.
		quoted[index] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}

func writeValidationText(w *textWriter, report *services.ValidationReport) {
	for _, issue := range report.Issues {
		location := issue.URI
		if issue.Line > 0 {
			location = fmt.Sprintf("%s:%d", issue.URI, issue.Line)
		}
		w.printf("%-7s %s  %s\n", issue.Severity, location, issue.Message)
	}
	w.printf("\n%d page(s): %d error(s), %d warning(s)\n", report.Pages, report.Errors, report.Warnings)
}

func writeRecordTitleValidationText(w *textWriter, report *services.RecordTitleReport) {
	for _, issue := range report.Issues {
		w.printf("%-7s source:%s  %s: %q\n", issue.Severity, issue.Source, issue.Message, issue.Title)
	}
	w.printf("\n%d source(s), %d document(s), %d record(s): %d error(s), %d warning(s)\n",
		report.Sources, report.Documents, report.Records, report.Errors, report.Warnings)
}

func writeWikiIndexText(w *textWriter, report *services.WikiIndexReport) {
	state := "unchanged"
	switch {
	case report.Stale:
		state = "STALE — run `fkf build wiki`"
	case report.Created:
		state = "created"
	case report.Changed:
		state = "rewritten"
	}
	w.printf("%s  %s\n%d page(s) across %d type(s), %d tag(s)\n",
		report.URI, state, report.Pages, report.Types, report.Tags)
}
