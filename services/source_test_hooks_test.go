package services_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
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
    fields: {id: .id, title: .id}
  dormant:
    enabled: false
    layer: index
    run: [collect-dormant.sh]
    test: [check-dormant.sh, --test]
    fields: {id: .id, title: .id}
  untested:
    enabled: true
    layer: index
    run: [collect-untested.sh]
    fields: {id: .id, title: .id}
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

func TestSourceTestsKeepAnEmptySelectionSuccessful(t *testing.T) {
	base := newBase(t, strings.ReplaceAll(sourceTestConfig,
		"    test: [check-active.sh, --fixture, \"{{base}}/fixtures\"]\n", ""), &fakeRunner{})
	report, err := services.TestSources(t.Context(), base, services.SourceTestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Passed != 0 || report.Failed != 0 || len(report.Sources) != 0 {
		t.Fatalf("report = %+v, want the compatible successful 0/0 result", report)
	}
}

func TestSourceTestsResolveBaseHooksFromTheDedicatedTestsDirectory(t *testing.T) {
	base := newBase(t, sourceTestConfig, nil)
	base.Runner = sources.ExecRunner()
	for directory, status := range map[string]int{
		filepath.Join(base.Root(), core.BaseBinDir): 47,
		filepath.Join(base.Root(), "tests"):         0,
	} {
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(directory, "check-active.sh")
		body := fmt.Sprintf("#!/bin/sh\nexit %d\n", status)
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	trust(t, base)

	report, err := services.TestSources(t.Context(), base, services.SourceTestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Passed != 1 || report.Failed != 0 {
		t.Fatalf("report = %+v, want the tests/ hook to win only for fkf test", report)
	}
}

func TestSourceTestsRefuseAnUnsafeTestsTreeBeforeRunning(t *testing.T) {
	runner := &fakeRunner{}
	base := newBase(t, sourceTestConfig, runner)
	trust(t, base)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "check-active.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base.Root(), core.BaseTestsDir)); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := services.TestSources(t.Context(), base, services.SourceTestRequest{}); !errors.Is(err, core.ErrUnsafePath) {
		t.Fatalf("TestSources() error = %v, want unsafe tests/ refused by trust", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unsafe tests/ executed %d command(s)", len(runner.calls))
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
