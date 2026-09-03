package main

import (
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func writeTrustText(w *textWriter, report *services.TrustReport) {
	// A re-trust leads with what moved. The full listing is the right answer the first time a
	// base is trusted, and the wrong one every time after: on a base with 46 sources and 15
	// scripts it is a 322-line read over 1,396 lines of shell, so the review that the gate
	// exists to force is the review nobody performs. `--all` is how you ask for it anyway.
	if !report.All && summarizesEverythingThatChanged(report.State.Changes) {
		writeTrustChangesText(w, report)
		return
	}
	w.printf("collection policy\n")
	for _, layer := range core.Layers {
		w.printf("  layer: %s=%t\n", layer, report.Policy.Layers[layer])
	}
	w.printf("  sync:  %d day(s), index stale after %dh, timeout %s, concurrency %d\n\n",
		report.Policy.Days, report.Policy.IndexMaxAgeHours, report.Policy.Timeout, report.Policy.Concurrency)
	w.printf("execution\n  direct argv, cwd %s, %s\n\n",
		report.Policy.WorkingDirectory, report.Policy.Environment)
	// Extra PATH directories apply to every command below without appearing in the run lines.
	if len(report.Bin) > 0 {
		w.printf("applies to every command below\n")
		for _, directory := range report.Bin {
			w.printf("  bin:  %s (on PATH)\n", directory)
		}
		w.line("")
	}
	if len(report.Commands) == 0 {
		w.printf("%s enables no source, so nothing would run.\n", report.Base)
	}
	for _, source := range report.Commands {
		w.printf("%s\n", source.Name)
		writeTrustedSourceText(w, source)
	}
	// Both executable trees are part of what is being approved. Their headings disclose the
	// narrower lookup scope that keeps a tests/git fixture from shadowing collection's real Git.
	writeTrustScriptTree(w, "bin/ (on PATH for every command; first for run: and body:)", report.Scripts)
	writeTrustScriptTree(w, "tests/ (first on PATH for test: hooks only)", report.Tests)
	if report.Recorded {
		w.printf("\ntrusted %s (digest %s)\n", report.Base, short(report.State.Digest))
		return
	}
	state := "NOT trusted"
	if report.State.Trusted {
		state = "trusted since " + report.State.Since
	}
	w.printf("\n%s: %s\n", report.Base, state)
}

func writeTrustScriptTree(w *textWriter, heading string, scripts []core.BinScript) {
	if len(scripts) == 0 {
		return
	}
	w.printf("\n%s\n", heading)
	for _, script := range scripts {
		switch {
		case script.Kind == "symlink":
			w.printf("  %-24s symlink -> %s\n", script.Name, script.Target)
		case !script.Executable:
			w.printf("  %-24s %s (not executable)\n", script.Name, short(script.Digest))
		default:
			w.printf("  %-24s %s\n", script.Name, short(script.Digest))
		}
	}
}

// summarizesEverythingThatChanged decides whether the diff may stand in for the listing. It
// may only when every change is a source or a script, because those are the items whose whole
// reviewable text the diff prints back.
//
// A configuration file that moved with no source or script moving with it is the case that
// must NOT be summarised: `bin:`, `layers:` and every disabled source live in that file and in
// no item, so "modified fkf.yaml" would be the entire review. A base-level PATH-directory
// change is exactly that shape — it can replace every command without appearing in any run line.
func summarizesEverythingThatChanged(changes []core.TrustChange) bool {
	if len(changes) == 0 {
		return false
	}
	for _, change := range changes {
		switch change.Item {
		case core.TrustItemSource, core.TrustItemScript, core.TrustItemTest:
			// The compact renderer prints the complete current disclosure for these items.
		default:
			return false
		}
	}
	return true
}

// writeTrustChangesText is the re-trust review: what differs from what was approved, printed
// with the text that differs. Anything that applies to every command is printed too, however
// small the diff — a reviewer cannot reconstruct it from a filename.
func writeTrustChangesText(w *textWriter, report *services.TrustReport) {
	w.printf("%s changed since it was trusted on %s\n\n", report.Base, report.State.Since)
	if len(report.Bin) > 0 {
		w.printf("applies to every command below\n")
		for _, directory := range report.Bin {
			w.printf("  bin:  %s (on PATH)\n", directory)
		}
		w.line("")
	}
	sources := map[string]services.TrustedSource{}
	for _, source := range report.Commands {
		sources[source.Name] = source
	}
	scripts := map[core.TrustItemKind]map[string]core.BinScript{
		core.TrustItemScript: {},
		core.TrustItemTest:   {},
	}
	for _, script := range report.Scripts {
		scripts[core.TrustItemScript][script.Name] = script
	}
	for _, script := range report.Tests {
		scripts[core.TrustItemTest][script.Name] = script
	}
	for _, change := range report.State.Changes {
		if change.Item == core.TrustItemConfig {
			continue // carried by the source and script lines below it
		}
		w.printf("%s %s %s%s\n", change.Kind, change.Item, change.Name, trustChangeNote(change))
		if source, ok := sources[change.Name]; ok && change.Item == core.TrustItemSource {
			writeTrustedSourceText(w, source)
		}
		if script, ok := scripts[change.Item][change.Name]; ok {
			directory := core.BaseBinDir
			if change.Item == core.TrustItemTest {
				directory = core.BaseTestsDir
			}
			w.printf("  %s/%s  %s\n", directory, change.Name, short(script.Digest))
		}
	}
	if report.Recorded {
		w.printf("\ntrusted %s (digest %s)\n", report.Base, short(report.State.Digest))
		return
	}
	w.printf("\n%d change(s). `fkf trust --all` prints everything; `fkf trust` records.\n",
		len(report.State.Changes))
}

// trustChangeNote spells out the one change whose consequence is not in its own name. A script
// whose contents did not move but which gained the executable bit is a one-line mode diff in a
// pull, and it is exactly what decides whether PATH lookup runs it.
func trustChangeNote(change core.TrustChange) string {
	switch change.Kind {
	case core.TrustArmed:
		return "  (unchanged contents, now executable — this is what PATH picks up)"
	case core.TrustDisarmed:
		return "  (unchanged contents, no longer executable)"
	default:
		return ""
	}
}

// writeTrustedSourceText prints everything about one source that a reviewer is approving. The
// full listing and the change diff both call it, so the two can never show different things
// about the same source.
func writeTrustedSourceText(w *textWriter, source services.TrustedSource) {
	w.printf("  enabled: %t\n", source.Enabled)
	w.printf("  layer:   %s\n", source.Layer)
	if len(source.Auth) > 0 {
		arguments := make([]string, 0, len(source.Auth))
		for _, argument := range source.Auth {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  auth: [%s]\n", strings.Join(arguments, ", "))
	}
	if len(source.Run) > 0 {
		arguments := make([]string, 0, len(source.Run))
		for _, argument := range source.Run {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  run:  [%s]\n", strings.Join(arguments, ", "))
	}
	if len(source.Test) > 0 {
		arguments := make([]string, 0, len(source.Test))
		for _, argument := range source.Test {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  test: [%s]\n", strings.Join(arguments, ", "))
	}
	if len(source.Body) > 0 {
		arguments := make([]string, 0, len(source.Body))
		for _, argument := range source.Body {
			arguments = append(arguments, strconv.QuoteToASCII(argument))
		}
		w.printf("  body: [%s]\n", strings.Join(arguments, ", "))
	}
	for _, name := range source.BodyFields.Names() {
		w.printf("  body field %s: %s\n", name, fieldPathsText(source.BodyFields.Paths(name)))
	}
	if source.Policy != "" {
		w.printf("  how:  %s\n", source.Policy)
	}
}

func fieldPathsText(paths core.FieldPaths) string {
	values := make([]string, 0, len(paths))
	for _, fieldPath := range paths {
		values = append(values, fieldPath.String())
	}
	if len(values) == 1 {
		return values[0]
	}
	return "[" + strings.Join(values, ", ") + "]"
}
