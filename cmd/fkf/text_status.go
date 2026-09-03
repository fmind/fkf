package main

import (
	"fmt"
	"strings"

	"github.com/fmind/fkf/services"
)

func writeStatusText(w *textWriter, status *services.Status) {
	trust := "untrusted"
	if status.Trust.Trusted {
		trust = "trusted"
	}
	w.printf("%s  %s  (%s, %s)\n\n", status.Name, status.Base, status.Origin, trust)
	writeStatusLayers(w, status)
	writeStatusSources(w, status)
	writeStatusHarnesses(w, status)
	writeStatusFindings(w, status)
	writeStatusNext(w, status)
}

func writeStatusLayers(w *textWriter, status *services.Status) {
	for _, layer := range status.Layers {
		if !layer.Enabled {
			w.printf("%-10s %s\n", layer.Layer, "off")
			continue
		}
		w.printf("%s\n", strings.TrimRight(fmt.Sprintf("%-10s %6d %-9s %s",
			layer.Layer, layer.Count, plural(layer.Unit, layer.Count), overviewDetail(layer)), " "))
	}
	if status.Graph != nil {
		w.printf("%-10s %6d %-9s over %d nodes, built %s\n",
			"graph", status.Graph.Edges, plural("edge", status.Graph.Edges),
			status.Graph.Nodes, status.Graph.GeneratedAt)
	} else {
		w.printf("%-10s %s\n", "graph", "not built")
	}
}

func writeStatusSources(w *textWriter, status *services.Status) {
	if len(status.Sources) == 0 {
		return
	}
	w.printf("\nsources\n")
	for _, source := range status.Sources {
		writeSourceStatusText(w, source)
	}
	w.printf("\n%d enabled, %d missing requirement(s), %d missing source test hook(s), %d quiet\n",
		status.Enabled, status.Missing, status.MissingTests, status.Quiet)
	if len(status.AuthRequired) > 0 {
		w.printf("auth required: %s\n", strings.Join(status.AuthRequired, ", "))
	}
}

func writeStatusHarnesses(w *textWriter, status *services.Status) {
	if len(status.Harnesses) == 0 {
		return
	}
	registered := make([]string, 0, len(status.Harnesses))
	for _, harness := range status.Harnesses {
		if harness.Registered {
			registered = append(registered, harness.Name)
		}
	}
	w.printf("\nharnesses\n  registered for this base: %s\n", orDash(strings.Join(registered, ", ")))
	for _, harness := range status.Harnesses {
		if harness.Error != "" {
			w.printf("  %-12s conflict: %s\n", harness.Name, harness.Error)
		}
	}
}

func writeStatusFindings(w *textWriter, status *services.Status) {
	if len(status.Findings) == 0 {
		return
	}
	w.printf("\nfindings\n")
	for _, finding := range status.Findings {
		w.printf("  [%s] %-20s %s\n", finding.Severity, finding.Check, finding.Message)
		for _, path := range finding.Paths {
			w.printf("         %s\n", path)
		}
		if finding.Fix != "" {
			w.printf("         fix: %s\n", finding.Fix)
		}
	}
	w.printf("\n%d error(s), %d warning(s)\n", status.Errors, status.Warnings)
}

func writeStatusNext(w *textWriter, status *services.Status) {
	if len(status.Next) == 0 {
		return
	}
	w.printf("\nnext\n")
	for _, line := range status.Next {
		w.printf("  %s\n", line)
	}
}

func writeSourceStatusText(w *textWriter, source services.SourceStatus) {
	state := "off"
	switch {
	case source.Undeclared:
		state = "gone"
	case source.Enabled:
		state = "on"
	}
	requirements := make([]string, 0, len(source.Requires))
	missing := false
	for _, requirement := range source.Requires {
		name := requirement.Name
		if !requirement.OnPath {
			name += " (missing)"
			missing = true
		}
		requirements = append(requirements, name)
	}
	if source.Test != nil {
		name := "test:" + source.Test.Name
		if !source.Test.OnPath {
			name += " (missing)"
		}
		requirements = append(requirements, name)
	}
	w.printf("%-24s %-4s %-22s %-12s %6d", source.Name, state,
		orDash(strings.Join(requirements, ", ")), orDash(source.LastDate), source.LastCount)
	if source.Quiet {
		w.printf("  quiet: %s", source.QuietReason)
	}
	if source.AuthRequired {
		w.printf("  auth-required")
	}
	w.line("")
	if missing && source.Enabled && source.Install != "" {
		w.printf("%26s install: %s\n", "", source.Install)
	}
}
