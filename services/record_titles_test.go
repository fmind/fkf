package services_test

import (
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestValidateRecordTitlesWarnsOnlyAboveHalf(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-01", `[
  {"id":"a","t":"2026-05-01T09:00:00Z","subject":"Repeated"},
  {"id":"b","t":"2026-05-01T10:00:00Z","subject":"Repeated"},
  {"id":"c","t":"2026-05-01T11:00:00Z","subject":"Distinct"}
]`)

	report, err := services.ValidateRecordTitles(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Warnings != 1 || len(report.Issues) != 1 {
		t.Fatalf("report = %+v, want one non-blocking warning", report)
	}
	issue := report.Issues[0]
	if issue.Source != "synthetic" || issue.Title != "Repeated" || issue.Count != 2 || issue.Records != 3 {
		t.Fatalf("issue = %+v", issue)
	}

	strict, err := services.ValidateRecordTitles(t.Context(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if strict.OK || strict.Errors != 1 || strict.Warnings != 0 {
		t.Fatalf("strict report = %+v, want promoted error", strict)
	}
}

func TestValidateRecordTitlesAcceptsExactlyHalfAndSingletons(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-01", `[
  {"id":"a","t":"2026-05-01T09:00:00Z","subject":"Same"},
  {"id":"b","t":"2026-05-01T10:00:00Z","subject":"Same"},
  {"id":"c","t":"2026-05-01T11:00:00Z","subject":"Third"},
  {"id":"d","t":"2026-05-01T12:00:00Z","subject":"Fourth"}
]`)

	report, err := services.ValidateRecordTitles(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 0 || !report.OK {
		t.Fatalf("report = %+v, want no warning at exactly half", report)
	}
}

func TestValidateRecordTitlesIgnoresHistoricalTitleProjections(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-01", `[
  {"id":"a","t":"2026-05-01T09:00:00Z","subject":"Repeated"},
  {"id":"b","t":"2026-05-01T10:00:00Z","subject":"Repeated"}
]`)
	base.Config.Sources["synthetic"].Fields[core.FieldTitle] = core.FieldPaths{mustFieldPath(t, ".title")}

	report, err := services.ValidateRecordTitles(t.Context(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Errors != 0 || report.Warnings != 0 || report.Records != 0 {
		t.Fatalf("report = %+v, want the superseded historical title projection excluded", report)
	}
}
