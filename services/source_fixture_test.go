package services_test

import (
	"context"
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

func TestOptionalGCloudAuditHelperUsesBoundedOverflowDetection(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "presets", "bin", "gcloud-audit-json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "--limit=10001") ||
		!strings.Contains(string(script), "length <= 10000") {
		t.Fatalf("gcloud audit helper lacks limit-plus-one overflow detection:\n%s", script)
	}
}

func TestAgentSessionsCollectorRunsOneExactDayAtATime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "personal")
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	source := config.Sources["agent-sessions"]
	if source.Window {
		t.Fatal("agent-sessions declares window: true, so one session envelope is range-dependently bucketed into only one day")
	}
	if !slices.Equal(source.Run, []string{"agent-sessions", "{{start}}", "{{end}}"}) {
		t.Fatalf("agent-sessions run = %q, want exact RFC3339 day bounds", source.Run)
	}
	for _, requirement := range []string{"agent-sessions", "find", "jq", "sqlite3", "touch"} {
		if !slices.Contains(source.Requires, requirement) {
			t.Errorf("agent-sessions requirements = %v, want %q for truthful readiness", source.Requires, requirement)
		}
	}
	memory := config.Sources["agent-memory-files"]
	for _, requirement := range []string{"agent-memory-files", "find", "jq", "stat"} {
		if !slices.Contains(memory.Requires, requirement) {
			t.Errorf("agent-memory-files requirements = %v, want %q for truthful readiness", memory.Requires, requirement)
		}
	}
}

func TestGmailCollectorUsesTheFailFastExactWindowHelper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "personal")
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	source := config.Sources["google-gmail-emails"]
	if !slices.Equal(source.Run, []string{"gmail-json", "{{start}}", "{{end}}"}) {
		t.Fatalf("Gmail run = %q, want the auditable helper and exact RFC3339 bounds", source.Run)
	}
	if !slices.Contains(source.Requires, "gmail-json") {
		t.Fatalf("Gmail requirements = %v, want its bundled helper declared explicitly", source.Requires)
	}
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"set -eu", "fromdateiso8601", "after:", "before:"} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("Gmail helper omits fail-fast exact-window expression %q", required)
		}
	}
	if strings.Contains(string(script), "snippet") {
		t.Fatal("Gmail metadata helper stores the message-body preview; bodies must remain lazy")
	}
}

func TestEveryPresetRunBuildsDirectArgvAfterSubstitution(t *testing.T) {
	window := sources.Window{
		Date: "2026-05-04", Next: "2026-05-05",
		Start: "2026-05-04T00:00:00Z", End: "2026-05-05T00:00:00Z",
	}
	for _, preset := range services.Presets {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatal(err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range config.SourceNames() {
			source := config.Sources[name]
			command := sources.BuildRunCommand(source, sources.Environment{Root: root}, window, time.Minute)
			if len(command.Argv) == 0 || command.Argv[0] != source.Run[0] {
				t.Errorf("%s/%s command = %+v, want the declared executable invoked directly",
					preset, name, command)
			}
		}
	}
}

func TestEveryGWSPageAllCollectorDeclaresAnExplicitPageLimit(t *testing.T) {
	for _, preset := range services.Presets {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatal(err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range config.Sources {
			pageAll := slices.Index(source.Run, "--page-all")
			if pageAll < 0 {
				continue
			}
			pageLimit := slices.Index(source.Run, "--page-limit")
			if pageLimit < 0 || pageLimit+1 >= len(source.Run) || source.Run[pageLimit+1] != "100" {
				t.Errorf("%s/%s uses --page-all without --page-limit 100: %q", preset, source.Name, source.Run)
			}
			if source.Run[0] != "gws-page-json" {
				t.Errorf("%s/%s delegates pagination without the completeness-checking helper: %q",
					preset, source.Name, source.Run)
			}
		}
	}

	for _, path := range []string{
		filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-json"),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-calendar-json"),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-tasks"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			command := strings.SplitN(line, "#", 2)[0]
			bounded := strings.Contains(command, "--page-limit 100") || strings.Contains(command, `--page-limit, "100"`)
			if strings.Contains(command, "--page-all") && !bounded {
				t.Errorf("%s:%d uses gws --page-all without the finite 100-page limit: %s", path, number+1, line)
			}
		}
		if strings.Contains(string(body), "--page-all") &&
			!strings.Contains(string(body), "gws-page-json") {
			t.Errorf("%s delegates a page ceiling to gws without validating the terminal token", path)
		}
	}
}

func TestNoShippedCollectorUsesAnUnboundedPaginationFlag(t *testing.T) {
	bin, err := filepath.Glob(filepath.Join(repositoryRoot(t), "presets", "bin", "*"))
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 2+len(bin))
	paths = append(paths,
		filepath.Join(repositoryRoot(t), "presets", "personal.yaml"),
		filepath.Join(repositoryRoot(t), "presets", "team.yaml"),
	)
	paths = append(paths, bin...)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			command := strings.SplitN(line, "#", 2)[0]
			if strings.Contains(command, "--paginate") {
				t.Errorf("%s:%d delegates an unbounded provider loop: %s", path, number+1, line)
			}
			if strings.Contains(command, "4294967295") {
				t.Errorf("%s:%d uses a nominal rather than operational page bound: %s", path, number+1, line)
			}
		}
	}
}

func TestEveryEnabledPresetHelperCommandIsInstalledIntoTheBase(t *testing.T) {
	for _, preset := range services.Presets {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatal(err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range config.EnabledSources() {
			for _, requirement := range source.Requires {
				if _, err := os.Stat(filepath.Join(repositoryRoot(t), "presets", "bin", requirement)); err != nil {
					continue // The provider CLI, not an fkf-owned helper.
				}
				installed := filepath.Join(root, core.BaseBinDir, requirement)
				info, err := os.Stat(installed)
				if err != nil {
					t.Errorf("%s/%s calls bundled helper %q, but init did not install it: %v", preset, source.Name, requirement, err)
					continue
				}
				if info.Mode().Perm()&0o111 == 0 {
					t.Errorf("%s/%s helper %q is not executable", preset, source.Name, requirement)
				}
			}
		}
	}
}

func TestGitHubRepositorySourcesUseTheBoundedCollectorAndRespectTheBaseScope(t *testing.T) {
	configs := make(map[string]*core.Config, 2)
	for _, preset := range []string{services.PresetPersonal, services.PresetTeam} {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatal(err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		configs[preset] = config
		run := config.Sources["github-repositories"].Run
		if len(run) == 0 || run[0] != "github-list-json" {
			t.Errorf("%s GitHub repository snapshot bypasses the bounded Link collector: %s", preset, run)
		}
	}

	personal := configs[services.PresetPersonal].Sources["github-repositories"].Run
	if !slices.Equal(personal, []string{"github-list-json", "user-repositories"}) {
		t.Fatalf("personal GitHub repository scope = %q, want every repository accessible to the authenticated user", personal)
	}
	team := configs[services.PresetTeam].Sources["github-repositories"].Run
	if !slices.Equal(team, []string{"github-list-json", "org-repositories", "REPLACE_WITH_ORG"}) {
		t.Fatalf("team GitHub repository scope = %q, want one explicit organization and no personal repository spill", team)
	}
}

// "Adding a source is a pull request with no Go in it" (README) is only true if a broken field
// map fails BEFORE a maintainer merges it, not the first time a contributor's own machine runs
// `fkf sync` for real — and until now, nothing checked a source's
// `fields.id`/`fields.time`/`fields.title` paths
// against anything shaped like the real CLI's actual output. `TestEveryPresetSourceProducesAn
// AddressableRecord` is that hermetic gate: testdata/sources/<name>.json is one hand-authored
// sample, as close to the real provider's shape as this repository's own knowledge of it goes —
// nested fields, real timestamp formats (including Gmail's epoch-millis string),
// the works — and it runs through the exact decode-and-verify path `fkf sync` uses, with a fake
// runner standing in for the CLI. A typo (`.number` for a source that actually prints `.id`)
// fails here, for free, with no credential and no network.
//
// A source declared identically in two presets shares one fixture: the same field paths need
// proving only once.
func TestEveryPresetSourceProducesAnAddressableRecord(t *testing.T) {
	seen := map[string]bool{}
	for _, preset := range services.Presets {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatalf("Init(--preset %s) error = %v", preset, err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatalf("the %s preset does not load: %v", preset, err)
		}
		for name, source := range config.Sources {
			if seen[name] {
				continue
			}
			seen[name] = true
			assertFixtureDecodes(t, name, source)
		}
	}
}

func TestEveryShippedPresetArtifactBelongsToADeclaredSource(t *testing.T) {
	declared := map[string]bool{}
	required := map[string]bool{}
	for _, preset := range services.Presets {
		root := filepath.Join(t.TempDir(), preset)
		isolate(t)
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Preset: preset, SkipGit: true,
		}, clock); err != nil {
			t.Fatalf("Init(--preset %s) error = %v", preset, err)
		}
		config, err := core.LoadConfig(root)
		if err != nil {
			t.Fatalf("load %s preset: %v", preset, err)
		}
		for name, source := range config.Sources {
			declared[name] = true
			for _, executable := range source.Requires {
				required[executable] = true
			}
		}
	}

	fixtures, err := filepath.Glob(filepath.Join("testdata", "sources", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		name := strings.TrimSuffix(filepath.Base(fixture), filepath.Ext(fixture))
		if !declared[name] {
			t.Errorf("orphan source fixture %s has no declaration in any shipped preset", fixture)
		}
	}

	helpers, err := filepath.Glob(filepath.Join(repositoryRoot(t), "presets", "bin", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, helper := range helpers {
		name := filepath.Base(helper)
		if name != "fkf-hook" && !required[name] {
			t.Errorf("orphan bundled helper %s is not declared in any preset source's requires", name)
		}
	}
}

func assertFixtureDecodes(t *testing.T, name string, source *core.Source) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		fixturePath := filepath.Join("testdata", "sources", name+".json")
		stdout, err := os.ReadFile(fixturePath)
		if err != nil {
			t.Fatalf("missing %s: every collector needs a fixture proving its field map against "+
				"realistic sample output before it can be trusted without running the real command (%v)",
				fixturePath, err)
		}
		runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
			return string(stdout), nil
		})
		window := sources.Window{}
		if source.Layer == core.LayerEvents {
			day, err := sources.ParseDay("2026-05-04")
			if err != nil {
				t.Fatal(err)
			}
			window = sources.DayWindow(day)
		}
		document, err := sources.Collect(t.Context(), runner, source, sources.Environment{}, window, time.Minute, testClock)
		if err != nil {
			t.Fatalf("fixture failed the real decode-and-verify path fkf sync uses — the field map "+
				"is likely wrong, or the fixture no longer matches the provider's real shape: %v", err)
		}
		if document.Count == 0 {
			t.Fatalf("%s decoded to zero records; the fixture must hold at least one", fixturePath)
		}
	})
}
