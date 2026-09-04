package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateExposesCollectedRecordTitleChecks(t *testing.T) {
	command := newValidateCommand()
	records := command.Command("records")
	if records == nil {
		t.Fatal("validate records subcommand is missing")
	}
	if records.Action == nil {
		t.Fatal("validate records has no action")
	}
}

// Bare `fkf validate` runs three or four checks, and a caller that pipes it must receive one
// JSON document, not a concatenated stream that only a stream-tolerant reader like jq accepts.
func TestValidateEmitsOneJSONDocumentForEveryLayerItChecks(t *testing.T) {
	root := demoBase(t)

	for _, test := range []struct {
		name     string
		args     []string
		wantLint bool
	}{
		{name: "bare", args: []string{"--format", "json", "--base", root, "validate"}},
		{name: "lint", args: []string{"--format", "json", "--base", root, "validate", "--lint"}, wantLint: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := invoke(t, test.args...)
			if got.code != ExitSuccess {
				t.Fatalf("validate exited %d: %s%s", got.code, got.stdout, got.stderr)
			}
			decoder := json.NewDecoder(strings.NewReader(got.stdout))
			var report validateReport
			if err := decoder.Decode(&report); err != nil {
				t.Fatalf("decode validate report: %v\n%s", err, got.stdout)
			}
			if decoder.More() {
				t.Fatalf("validate emitted more than one JSON document:\n%s", got.stdout)
			}
			if report.Wiki == nil || report.Wiki.Layer != "wiki" {
				t.Errorf("wiki report = %+v, want the wiki layer", report.Wiki)
			}
			if report.Projects == nil || report.Projects.Layer != "projects" {
				t.Errorf("projects report = %+v, want the projects layer", report.Projects)
			}
			if report.Records == nil {
				t.Error("records report is missing")
			}
			if test.wantLint != (report.Lint != nil) {
				t.Errorf("lint report = %+v, want present=%v", report.Lint, test.wantLint)
			}
			if !report.OK {
				t.Errorf("validate report ok = false on a clean demo base:\n%s", got.stdout)
			}
		})
	}
}

// JSONL has no natural collection to stream here: four heterogeneous reports are one record.
// The contract that matters is that it emits something, because the previous shape printed
// nothing at all and a clean run was byte-identical to a crash.
func TestValidateJSONLEmitsTheWholeReportOnOneLine(t *testing.T) {
	root := demoBase(t)
	got := invoke(t, "--format", "jsonl", "--base", root, "validate")
	if got.code != ExitSuccess {
		t.Fatalf("validate exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if strings.Count(strings.TrimSpace(got.stdout), "\n") != 0 {
		t.Fatalf("JSONL validate = %q, want one line", got.stdout)
	}
	var report validateReport
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("decode JSONL validate report: %v\n%s", err, got.stdout)
	}
	if report.Wiki == nil || report.Records == nil {
		t.Fatalf("JSONL validate report = %+v, want every checked layer", report)
	}
}

// The text rendering carried no layer label, so three near-identical summary lines were
// indistinguishable and a fourth --lint block looked like a repeat.
func TestValidateTextNamesEveryLayerItReports(t *testing.T) {
	root := demoBase(t)
	got := invoke(t, "--format", "text", "--base", root, "validate", "--lint")
	if got.code != ExitSuccess {
		t.Fatalf("validate exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	for _, want := range []string{"\nwiki\n", "\nprojects\n", "\nrecords\n", "\nlint\n", "page(s):", "record(s):"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("validate text omits %q:\n%s", want, got.stdout)
		}
	}
}
