package sources_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestNewEnvironmentExcludesInheritedPathsInsideTheBase(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(filepath.Join(root, core.BaseBinDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName),
		[]byte("fkf: 1\nname: brain\nschema:\n  id: {description: Stable record identity., cardinality: one}\n"+
			"layers: {events: true}\nsources: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "helper"), []byte("#!/bin/sh\nprintf unreviewed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(root, core.BaseBinDir, "trusted-helper")
	if err := os.WriteFile(trusted, []byte("#!/bin/sh\nprintf trusted\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "base-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{root, link, filepath.Dir(shell)}, string(os.PathListSeparator)))
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	environment := sources.NewEnvironment(config)
	for _, entry := range filepath.SplitList(environment.Env["PATH"]) {
		if entry == root || entry == link {
			t.Fatalf("child PATH %q retains inherited base-controlled entry %q", environment.Env["PATH"], entry)
		}
	}

	runner := sources.ExecRunner()
	output, err := runner.Run(t.Context(), sources.Command{
		Argv: []string{"sh", "-c", "helper"}, Dir: root, Env: environment.Env, Timeout: time.Second,
	})
	if err == nil || output != "" {
		t.Fatalf("unreviewed base-root helper = %q, %v; want it unreachable", output, err)
	}
	output, err = runner.Run(t.Context(), sources.Command{
		Argv: []string{"sh", "-c", "trusted-helper"}, Dir: root, Env: environment.Env, Timeout: time.Second,
	})
	if err != nil || output != "trusted" {
		t.Fatalf("canonical base/bin helper = %q, %v; want the trusted helper reachable", output, err)
	}
}

func TestBuildRunCommandDoesNotRequireAnInstalledShell(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	source := &core.Source{Name: "direct", Layer: core.LayerEvents, Run: []string{"helper", "first | second"}}
	command := sources.BuildRunCommand(source, sources.Environment{Root: t.TempDir()}, sources.Window{}, time.Minute)
	if !slices.Equal(command.Argv, source.Run) {
		t.Fatalf("argv = %q, want the declared helper and opaque argument with no shell wrapper", command.Argv)
	}
}

func TestBuildRunCommandUsesANeutralWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	command := sources.BuildRunCommand(
		&core.Source{Run: []string{"provider"}},
		sources.Environment{Root: root},
		sources.Window{},
		time.Second,
	)
	if command.Dir != string(filepath.Separator) {
		t.Fatalf("command directory = %q, want the neutral filesystem root", command.Dir)
	}
	if command.ForbiddenRoot != root {
		t.Fatalf("forbidden root = %q, want base %q kept out of startup paths", command.ForbiddenRoot, root)
	}
}

func TestExecRunnerRemovesAnAbsoluteBaseConfigRootFromTheNeutralDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "wiki"))
	command := sources.BuildRunCommand(
		&core.Source{Run: []string{"sh", "-c", `printf '%s' "${XDG_CONFIG_HOME-unset}"`}},
		sources.Environment{Root: root, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		sources.Window{},
		time.Second,
	)
	output, err := sources.ExecRunner().Run(t.Context(), command)
	if err != nil || output != "unset" {
		t.Fatalf("base config root = %q, %v; want it removed before neutral-cwd execution", output, err)
	}
}

func TestBuildRunCommandCannotExecuteRelativeAuthoredSupport(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not installed: %v", err)
	}
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	if err := os.Mkdir(wiki, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wiki, "payload.py"), []byte("print('sourced')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := sources.BuildRunCommand(
		&core.Source{Run: []string{python, "wiki/payload.py"}},
		sources.Environment{Root: root, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		sources.Window{},
		time.Second,
	)
	output, err := sources.ExecRunner().Run(t.Context(), command)
	if err == nil || output != "" {
		t.Fatalf("relative authored support = %q, %v; want it unreachable outside trusted bin/", output, err)
	}
}

func TestBuildRunCommandDoesNotImportPythonFromTheBaseRoot(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not installed: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sitecustomize.py"), []byte("print('sourced')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := sources.BuildRunCommand(
		&core.Source{Run: []string{python, "-c", "print('declared')"}},
		sources.Environment{Root: root, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		sources.Window{},
		time.Second,
	)
	output, err := sources.ExecRunner().Run(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if output != "declared\n" {
		t.Fatalf("Python output = %q; base-root startup code ran outside the trusted bin/ digest", output)
	}
}

func TestExecRunnerDelegatesInterpreterChoiceToTheHelperShebang(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(bin, "literal-argument")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -eu\nprintf '%s' \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argument := `space ; $(printf injected) | wildcard*`
	output, err := sources.ExecRunner().Run(t.Context(), sources.Command{
		Argv:    []string{"literal-argument", argument},
		Dir:     root,
		Env:     map[string]string{"PATH": bin},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != argument {
		t.Fatalf("output = %q, want the opaque argument unchanged", output)
	}
}

func TestExecRunnerDoesNotSourceInheritedShellStartupFiles(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is required by run: sources: %v", err)
	}
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	if err := os.WriteFile(payload, []byte("printf sourced"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A relative BASH_ENV is resolved from Cmd.Dir by non-interactive Bash. If the inherited
	// value reaches the child, mutable base content runs before the trusted command string.
	t.Setenv("BASH_ENV", "payload")
	t.Setenv("ENV", "payload")
	output, err := sources.ExecRunner().Run(t.Context(), sources.Command{
		Argv: []string{bash, "-c", `printf 'declared|%s|%s' "${BASH_ENV-unset}" "${ENV-unset}"`},
		Dir:  root, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "declared|unset|unset" {
		t.Fatalf("child output = %q; inherited shell startup state reached the trusted command", output)
	}
}

func TestBuildRunCommandEscapesControlCharactersInItsDisplay(t *testing.T) {
	t.Setenv("HOME", "/tmp/home\nsecond\x1bline")
	root := "/tmp/base\nnext\x1bline"
	source := &core.Source{Name: "display", Layer: core.LayerEvents, Run: []string{"cli", "{{base}}", "{{home}}"}}
	command := sources.BuildRunCommand(source, sources.Environment{Root: root}, sources.Window{}, time.Minute)
	display := command.Display()
	if strings.ContainsAny(display, "\n\x1b") || !strings.Contains(display, `\n`) || !strings.Contains(display, `\x1b`) {
		t.Fatalf("display = %q, want terminal control characters escaped", display)
	}
	if command.Argv[1] != root {
		t.Fatalf("execution base = %q, want the exact raw path passed as argv data", command.Argv[1])
	}
}

// TestDayWindowSurvivesAMidnightThatDoesNotExist is the regression test for the day that
// silently filed under its predecessor. Several real zones move their clocks at local
// midnight rather than 02:00 — Brazil did through 2018 — so requesting "midnight" on the
// spring-forward day asks for a wall-clock instant that never happened. Go's time.Date does
// not error on that: it normalizes to a different, earlier, valid instant, and that instant
// formats as the PREVIOUS calendar day. A day boundary built directly from Y/M/D reproduces
// exactly that misfile; DayWindow is built from a noon anchor instead, precisely to avoid ever
// reconstructing a midnight that might not exist.
//
// This loads a real IANA zone rather than fabricating one, because the failure is a property
// of actual tzdata transition tables, not of anything this test could construct by hand.
func TestDayWindowSurvivesAMidnightThatDoesNotExist(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata for America/Sao_Paulo on this system: %v", err)
	}
	// Confirmed against the zone's own transition table: local midnight on each of these dates
	// falls inside the spring-forward gap and does not exist.
	for _, date := range []string{"2015-10-18", "2016-10-16", "2017-10-15", "2018-11-04"} {
		t.Run(date, func(t *testing.T) {
			noon := parseNoonIn(t, date, loc)
			window := sources.DayWindow(noon)
			if window.Date != date {
				t.Fatalf("Date = %q, want %q; a day whose midnight does not exist must not be "+
					"filed under the previous day", window.Date, date)
			}
			wantNext := addCivilDay(t, date)
			if window.Next != wantNext {
				t.Fatalf("Next = %q, want %q", window.Next, wantNext)
			}
			start, end := mustParseRFC3339(t, window.Start), mustParseRFC3339(t, window.End)
			if !start.Before(end) {
				t.Fatalf("Start %s is not before End %s", window.Start, window.End)
			}
			// The gap this zone's transition opens is one hour, so the civil day is honestly
			// 23 hours long rather than the claimed-but-wrong 24.
			if span := end.Sub(start); span != 23*time.Hour {
				t.Fatalf("span = %s, want the honest 23h a spring-forward day actually has", span)
			}
		})
	}
}

// TestDayWindowSurvivesAMidnightThatIsAmbiguous is the fall-back counterpart: the same zone's
// clocks fall BACK at local midnight too, so 00:00 on that day is ambiguous (it happens twice)
// rather than absent. The day is honestly 25 hours, and the boundary still has to name the
// right calendar date on both sides.
func TestDayWindowSurvivesAMidnightThatIsAmbiguous(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("no tzdata for America/Sao_Paulo on this system: %v", err)
	}
	for _, date := range []string{"2015-02-21", "2016-02-20", "2017-02-18", "2018-02-17"} {
		t.Run(date, func(t *testing.T) {
			noon := parseNoonIn(t, date, loc)
			window := sources.DayWindow(noon)
			if window.Date != date {
				t.Fatalf("Date = %q, want %q", window.Date, date)
			}
			wantNext := addCivilDay(t, date)
			if window.Next != wantNext {
				t.Fatalf("Next = %q, want %q", window.Next, wantNext)
			}
			start, end := mustParseRFC3339(t, window.Start), mustParseRFC3339(t, window.End)
			if span := end.Sub(start); span != 25*time.Hour {
				t.Fatalf("span = %s, want the honest 25h a fall-back day actually has", span)
			}
		})
	}
}

// TestDayWindowOnAnOrdinaryDayIsUnchanged pins the common case: a 24-hour day, midnight to
// midnight, exactly as before this fix.
func TestDayWindowOnAnOrdinaryDayIsUnchanged(t *testing.T) {
	day, err := sources.ParseDay("2026-08-20")
	if err != nil {
		t.Fatal(err)
	}
	window := sources.DayWindow(day)
	if window.Date != "2026-08-20" || window.Next != "2026-08-21" {
		t.Fatalf("window = %+v, want an ordinary day unchanged", window)
	}
	start, end := mustParseRFC3339(t, window.Start), mustParseRFC3339(t, window.End)
	if span := end.Sub(start); span != 24*time.Hour {
		t.Fatalf("span = %s, want the ordinary 24h", span)
	}
}

// TestParseDayIsNoonAnchoredButLooksLikeMidnightToItsCallers is what makes the fix safe for
// the two existing callers, both of which reduce ParseDay's result to a date label or hand it
// straight to DayWindow: the noon shift must never leak into a caller comparing calendar dates.
func TestParseDayIsNoonAnchoredButLooksLikeMidnightToItsCallers(t *testing.T) {
	day, err := sources.ParseDay("2026-08-20")
	if err != nil {
		t.Fatal(err)
	}
	if got := day.Format(time.DateOnly); got != "2026-08-20" {
		t.Fatalf("Format(DateOnly) = %q, want 2026-08-20", got)
	}
	// The DAY-LABEL comparison services/sync.go makes ("today or later") must still work: a
	// noon-anchored today must compare equal to itself, not slip a day either way.
	todayLabel := time.Now().Format(time.DateOnly)
	today, err := sources.ParseDay(todayLabel)
	if err != nil {
		t.Fatal(err)
	}
	if today.Format(time.DateOnly) != todayLabel {
		t.Fatalf("today's label = %q, want it to match the wall clock's own date %q",
			today.Format(time.DateOnly), todayLabel)
	}
}

func TestParseDayRejectsACivilDateThatDidNotExist(t *testing.T) {
	location, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = location
	t.Cleanup(func() { time.Local = previous })

	if day, err := sources.ParseDay("2011-12-30"); err == nil {
		t.Fatalf("ParseDay() = %s, nil; Pacific/Apia skipped 2011-12-30 entirely", day.Format(time.RFC3339))
	}
}

func parseNoonIn(t *testing.T, date string, loc *time.Location) time.Time {
	t.Helper()
	noon, err := time.ParseInLocation("2006-01-02 15:04:05", date+" 12:00:00", loc)
	if err != nil {
		t.Fatalf("parse %s: %v", date, err)
	}
	return noon
}

func addCivilDay(t *testing.T, date string) string {
	t.Helper()
	day, err := time.ParseInLocation(time.DateOnly, date, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return day.AddDate(0, 0, 1).Format(time.DateOnly)
}

func mustParseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}
