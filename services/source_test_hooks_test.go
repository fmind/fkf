package services_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
)

const sourceTestConfig = `name: test-hooks
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  active:
    enabled: true
    layer: index
    run: [collect-active.sh]
    test: [check-active.sh, --fixture, "{{base}}/fixtures"]
    timeout: 7s
    fields: {id: .id}
  dormant:
    enabled: false
    layer: index
    run: [collect-dormant.sh]
    test: [check-dormant.sh, --test]
    fields: {id: .id}
  untested:
    enabled: true
    layer: index
    run: [collect-untested.sh]
    fields: {id: .id}
`

func TestSourceTestsSelectEnabledDeclaredHooksByDefault(t *testing.T) {
	runner := &fakeRunner{}
	base := newBase(t, sourceTestConfig, runner)
	trust(t, base)
	report, err := services.TestSources(t.Context(), base, services.SourceTestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Passed != 1 || report.Failed != 0 || len(report.Sources) != 1 {
		t.Fatalf("report = %+v, want one passing enabled hook", report)
	}
	if report.Sources[0].Source != "active" || report.Sources[0].Outcome != services.SourceTestPassed {
		t.Fatalf("source result = %+v, want active passed", report.Sources[0])
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0].Argv,
		[]string{"check-active.sh", "--fixture", base.Root() + "/fixtures"}) {
		t.Fatalf("runner calls = %+v, want the exact substituted test argv", runner.calls)
	}
	if runner.calls[0].Timeout != 7*time.Second {
		t.Fatalf("test timeout = %s, want the source timeout", runner.calls[0].Timeout)
	}
}

func TestSourceTestsSelectAllOrExplicitDisabledHooks(t *testing.T) {
	for _, test := range []struct {
		name    string
		request services.SourceTestRequest
		want    []string
	}{
		{name: "all", request: services.SourceTestRequest{All: true}, want: []string{"active", "dormant"}},
		{name: "named disabled", request: services.SourceTestRequest{Targets: []string{"dormant"}}, want: []string{"dormant"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			base := newBase(t, sourceTestConfig, runner)
			trust(t, base)
			report, err := services.TestSources(t.Context(), base, test.request)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(report.Sources))
			for _, result := range report.Sources {
				got = append(got, result.Source)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("tested sources = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSourceTestsReportSafeFailuresAndContinue(t *testing.T) {
	runner := &fakeRunner{err: errors.New("synthetic safe failure")}
	base := newBase(t, sourceTestConfig, runner)
	trust(t, base)
	report, err := services.TestSources(t.Context(), base, services.SourceTestRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || report.Failed != 2 || report.Passed != 0 || len(report.Sources) != 2 {
		t.Fatalf("report = %+v, want both hooks to fail without stopping", report)
	}
	summary := report.FailureSummary()
	for _, want := range []string{"active", "check-active.sh", "dormant", "check-dormant.sh", "synthetic safe failure"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("failure summary = %q, want %q", summary, want)
		}
	}
}
