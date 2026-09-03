package sources_test

import (
	"bytes"
	"errors"
	"log/slog"
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

// Keep interpreter-backed isolation tests bounded without turning cold startup on a loaded CI
// runner into a latency contract.
const interpreterStartupTestTimeout = 5 * time.Second

func mustBuildRunCommand(
	t testing.TB, source *core.Source, env sources.Environment, window sources.Window, timeout time.Duration,
) sources.Command {
	t.Helper()
	command, err := sources.BuildRunCommand(source, env, window, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func mustBuildTestCommand(t testing.TB, source *core.Source, env sources.Environment) sources.Command {
	t.Helper()
	command, err := sources.BuildTestCommand(source, env, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestExecRunnerLogsDeclaredCommandContextWithoutProviderStderr(t *testing.T) {
	const privateStderr = "synthetic-provider-private-stderr"
	t.Setenv("FKF_SYNTHETIC_PRIVATE_STDERR", privateStderr)
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh is not installed: %v", err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	window := sources.Window{
		Date: "2026-08-01", Start: "2026-07-31T22:00:00Z", End: "2026-08-01T22:00:00Z",
	}
	source := &core.Source{
		Name: "github-events", Layer: core.LayerEvents,
		Run: []string{"sh", "-c", `printf '%s' "$FKF_SYNTHETIC_PRIVATE_STDERR" >&2; exit 3`},
	}
	command := mustBuildRunCommand(t, source, sources.Environment{
		Root: t.TempDir(), Env: map[string]string{"PATH": filepath.Dir(shell)},
	}, window, time.Second)
	if _, err := sources.ExecRunner().Run(t.Context(), command); err == nil {
		t.Fatal("ExecRunner.Run() succeeded, want the declared command to fail")
	}

	diagnostic := logs.String()
	if strings.Contains(diagnostic, privateStderr) {
		t.Fatalf("command log leaked provider stderr: %q", diagnostic)
	}
	for _, want := range []string{
		`source=github-events`, `date=2026-08-01`,
		`window_start=2026-07-31T22:00:00Z`, `window_end=2026-08-01T22:00:00Z`,
		`command=`, `sh -c`, `cwd=/`, `timeout=1s`,
		`status=exit`, `diagnostic="command exited with status 3"`,
	} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("command log = %q, want the safe field %q", diagnostic, want)
		}
	}
}

func TestNewEnvironmentExcludesInheritedPathsInsideTheBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
	if _, err := core.WriteTrust(t.Context(), config, time.Now()); err != nil {
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

func TestExecRunnerRevalidatesTrustedExecutableTreeBeforeEachExec(t *testing.T) {
	for _, test := range []struct {
		name, tree string
		build      func(testing.TB, *core.Source, sources.Environment) sources.Command
	}{
		{
			name: "collection helper", tree: core.BaseBinDir,
			build: func(t testing.TB, source *core.Source, env sources.Environment) sources.Command {
				return mustBuildRunCommand(t, source, env, sources.Window{}, time.Minute)
			},
		},
		{
			name: "source test helper", tree: core.BaseTestsDir,
			build: func(t testing.TB, source *core.Source, env sources.Environment) sources.Command {
				return mustBuildTestCommand(t, source, env)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			directory := filepath.Join(root, test.tree)
			if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(directory, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf trusted\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			configBody := `fkf: 1
name: test
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Human label., cardinality: one}
layers: {index: true}
sources:
  source:
    enabled: true
    layer: index
    run: [helper]
    test: [helper]
    fields: {id: .id, title: .id}
`
			if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(configBody), core.BaseFileMode); err != nil {
				t.Fatal(err)
			}
			config, err := core.LoadConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := core.WriteTrust(t.Context(), config, time.Now()); err != nil {
				t.Fatal(err)
			}
			command := test.build(t, config.Sources["source"], sources.NewEnvironment(config))
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf changed\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := sources.ExecRunner().Run(t.Context(), command)
			if !errors.Is(err, core.ErrUntrusted) || output != "" {
				t.Fatalf("ExecRunner.Run() = %q, %v; want executable-tree drift refused before exec", output, err)
			}
		})
	}
}

func TestBuildRunCommandDoesNotRequireAnInstalledShell(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	source := &core.Source{Name: "direct", Layer: core.LayerEvents, Run: []string{"helper", "first | second"}}
	command := mustBuildRunCommand(t, source, sources.Environment{Root: t.TempDir()}, sources.Window{}, time.Minute)
	if !slices.Equal(command.Argv, source.Run) {
		t.Fatalf("argv = %q, want the declared helper and opaque argument with no shell wrapper", command.Argv)
	}
}

func TestBuildTestCommandFailsAHomePlaceholderWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := sources.BuildTestCommand(
		&core.Source{Name: "check", Test: []string{"check", "{{home}}"}},
		sources.Environment{Root: t.TempDir()}, time.Minute,
	)
	if err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("BuildTestCommand() error = %v, want {{home}} planning to fail without HOME", err)
	}

	command, err := sources.BuildRunCommand(
		&core.Source{Name: "no-home", Run: []string{"provider"}},
		sources.Environment{Root: t.TempDir()}, sources.Window{}, time.Minute,
	)
	if err != nil || !slices.Equal(command.Argv, []string{"provider"}) {
		t.Fatalf("BuildRunCommand() = %+v, %v; HOME is irrelevant without {{home}}", command, err)
	}
}

func TestBuildAuthCommandUsesTheDeclaredExecutionBoundary(t *testing.T) {
	config := &core.Config{}
	env := sources.Environment{
		Root: "/base", Env: map[string]string{"PATH": "/usr/bin"}, TrustConfig: config,
	}
	source := &core.Source{
		Name: "provider", Auth: []string{"provider", "auth", "status"}, Timeout: 5 * time.Second,
	}
	command := sources.BuildAuthCommand(source, env, time.Minute)
	if !slices.Equal(command.Argv, source.Auth) {
		t.Fatalf("auth argv = %q, want the literal declared probe %q", command.Argv, source.Auth)
	}
	if command.Dir != core.DeclaredCommandDirectory || command.ForbiddenRoot != env.Root {
		t.Fatalf("auth boundary = dir %q, root %q; want neutral cwd and protected base", command.Dir, command.ForbiddenRoot)
	}
	if command.Timeout != source.Timeout || command.TrustConfig != config {
		t.Fatalf("auth timeout/trust = %s, %p; want %s, %p", command.Timeout, command.TrustConfig, source.Timeout, config)
	}
	if command.Source != "" {
		t.Fatalf("auth command source = %q; probe argv must not enter declared-run diagnostics", command.Source)
	}
	if !command.QuietFailure {
		t.Fatal("auth command failures must not be logged")
	}
	command.Argv[0] = "changed"
	command.Env["PATH"] = "/changed"
	if source.Auth[0] != "provider" || env.Env["PATH"] != "/usr/bin" {
		t.Fatal("BuildAuthCommand returned caller-owned argv or environment storage")
	}
}

func TestExecRunnerKeepsExpectedAuthFailureSilent(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	source := &core.Source{
		Name: "provider", Auth: []string{"sh", "-c", "printf public; printf private >&2; exit 9"},
	}
	command := sources.BuildAuthCommand(source, sources.Environment{
		Root: t.TempDir(), Env: map[string]string{"PATH": filepath.Dir(shell)},
	}, time.Second)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	output, err := sources.ExecRunner().Run(t.Context(), command)
	if err == nil || !strings.Contains(err.Error(), "status 9") {
		t.Fatalf("auth probe error = %v, want the safe exit status", err)
	}
	if output != "" || logs.Len() != 0 {
		t.Fatalf("auth probe exposed output %q or logs %q", output, logs.String())
	}
}

func TestSourceTestPathCannotShadowCollectionCommands(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, core.BaseBinDir)
	tests := filepath.Join(root, core.BaseTestsDir)
	for _, directory := range []string{bin, tests} {
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			t.Fatal(err)
		}
	}
	for path, output := range map[string]string{
		filepath.Join(bin, "helper"):   "bin",
		filepath.Join(tests, "helper"): "tests",
	} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := sources.Environment{Root: root, Env: map[string]string{"PATH": bin}}
	source := &core.Source{Run: []string{"helper"}, Test: []string{"helper"}}
	run := mustBuildRunCommand(t, source, env, sources.Window{}, time.Minute)
	check := mustBuildTestCommand(t, source, env)

	if got := filepath.SplitList(run.Env["PATH"]); !slices.Equal(got, []string{bin}) {
		t.Fatalf("run PATH = %q, want tests/ unreachable during collection", got)
	}
	runner := &fakeRunner{stdout: "body"}
	if _, body, err := sources.FetchBody(t.Context(), runner,
		&core.Source{Body: []string{"helper", "body"}}, sources.Fields{}, env, sources.Record{}, time.Minute,
	); err != nil {
		t.Fatal(err)
	} else if got := filepath.SplitList(body.Env["PATH"]); !slices.Equal(got, []string{bin}) {
		t.Fatalf("body PATH = %q, want tests/ unreachable during body fetching", got)
	}
	if got := filepath.SplitList(check.Env["PATH"]); !slices.Equal(got, []string{tests, bin}) {
		t.Fatalf("test PATH = %q, want only tests/ prepended to the ordinary command path", got)
	}
	executor := sources.ExecRunner()
	if output, err := executor.Run(t.Context(), run); err != nil || output != "bin" {
		t.Fatalf("run helper = %q, %v; want bin/helper", output, err)
	}
	if output, err := executor.Run(t.Context(), check); err != nil || output != "tests" {
		t.Fatalf("test helper = %q, %v; want tests/helper", output, err)
	}
	if err := os.Remove(filepath.Join(tests, "helper")); err != nil {
		t.Fatal(err)
	}
	if output, err := executor.Run(t.Context(), check); err != nil || output != "bin" {
		t.Fatalf("test helper without tests/ entry = %q, %v; want normal bin/ fallback", output, err)
	}
	if env.Env["PATH"] != bin {
		t.Fatalf("BuildTestCommand mutated the shared environment PATH to %q", env.Env["PATH"])
	}
}

func TestSourceHookMayUseATrustedFixtureShadowOnlyInsideTests(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, core.BaseBinDir)
	tests := filepath.Join(root, core.BaseTestsDir)
	for _, directory := range []string{bin, tests} {
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(bin, "git"):         "#!/bin/sh\nprintf real\n",
		filepath.Join(tests, "git"):       "#!/bin/sh\nprintf fixture\n",
		filepath.Join(tests, "git-check"): "#!/bin/sh\nexec git\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := sources.Environment{Root: root, Env: map[string]string{"PATH": bin}}
	run := mustBuildRunCommand(t, &core.Source{Run: []string{"git"}}, env, sources.Window{}, time.Minute)
	check := mustBuildTestCommand(t, &core.Source{Test: []string{"git-check"}}, env)
	executor := sources.ExecRunner()
	if output, err := executor.Run(t.Context(), run); err != nil || output != "real" {
		t.Fatalf("run git = %q, %v; tests/git shadowed collection", output, err)
	}
	if output, err := executor.Run(t.Context(), check); err != nil || output != "fixture" {
		t.Fatalf("test fixture git = %q, %v; want trusted tests/git visible inside the hook", output, err)
	}
}

func TestBuildTestCommandInitializesAnEmptyEnvironment(t *testing.T) {
	root := t.TempDir()
	command := mustBuildTestCommand(t,
		&core.Source{Test: []string{"check"}}, sources.Environment{Root: root},
	)
	if got := filepath.SplitList(command.Env["PATH"]); !slices.Equal(got, []string{filepath.Join(root, core.BaseTestsDir)}) {
		t.Fatalf("test PATH = %q, want the dedicated test tree even without inherited environment", got)
	}
}

func TestBuildRunCommandUsesANeutralWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	command := mustBuildRunCommand(t,
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
	command := mustBuildRunCommand(t,
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
	command := mustBuildRunCommand(t,
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
	command := mustBuildRunCommand(t,
		&core.Source{Run: []string{python, "-c", "print('declared')"}},
		sources.Environment{Root: root, Env: map[string]string{"PATH": os.Getenv("PATH")}},
		sources.Window{},
		interpreterStartupTestTimeout,
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
	command := mustBuildRunCommand(t, source, sources.Environment{Root: root}, sources.Window{}, time.Minute)
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

	if day, err := sources.ParseDayInLocation("2011-12-30", location); err == nil {
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
