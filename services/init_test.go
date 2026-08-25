package services_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func clock() time.Time { return testClock }

func TestBaseAgentsTemplateRoutesToSkillsWithoutRepeatingThem(t *testing.T) {
	content := services.BaseAgentsTemplate("brain")
	for _, required := range []string{
		".agents/skills/fkf-use/SKILL.md",
		".agents/skills/fkf-learn/SKILL.md",
		"untrusted data",
		"fkf trust",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("base AGENTS.md template omits %q", required)
		}
	}
	for _, duplicated := range []string{"## Layout", "## URIs", "## Task traces", "graph.tsv"} {
		if strings.Contains(content, duplicated) {
			t.Errorf("base AGENTS.md template repeats skill content %q", duplicated)
		}
	}
	if lines := strings.Count(content, "\n"); lines > 20 {
		t.Errorf("base AGENTS.md template is %d lines; want at most 20", lines)
	}
}

func TestInitCreatesACompleteTrustedBase(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	report, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if !report.Created || report.Name != "brain" {
		t.Fatalf("report = %+v", report)
	}
	// The preset only decides the initial fkf.yaml; everything else is the same for all of them.
	if report.Declared < 10 || report.Enabled < 1 {
		t.Fatalf("report = %+v, want the personal preset to declare many sources and enable a few", report)
	}
	for _, expected := range []string{
		core.ConfigFileName, ".gitignore", ".gitattributes", core.BaseAgentsFile, "CLAUDE.md",
		filepath.FromSlash(core.BaseSkillsDir + "/fkf-use/SKILL.md"),
		filepath.FromSlash(core.BaseSkillsDir + "/fkf-learn/SKILL.md"),
		filepath.Join(core.BaseBinDir, "git-log-json"),
		filepath.Join(core.BaseBinDir, "agent-sessions"),
		filepath.Join(core.BaseBinDir, "agent-memory-files"),
		filepath.Join(core.BaseBinDir, "fkf-hook"),
	} {
		if _, err := os.Stat(filepath.Join(root, expected)); err != nil {
			t.Fatalf("init did not write %s: %v", expected, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, ".claude", "skills")); err != nil || target != "../.agents/skills" {
		t.Fatalf(".claude/skills target = %q, %v; want ../.agents/skills", target, err)
	}
	if got := mustRead(t, filepath.Join(root, "CLAUDE.md")); got != "@AGENTS.md\n" {
		t.Fatalf("CLAUDE.md = %q, want the one canonical instruction bridge", got)
	}
	if ignore := mustRead(t, filepath.Join(root, ".gitignore")); !strings.Contains(ignore, ".agents/skills/local-*/") {
		t.Fatal("the managed ignore block does not keep machine-local skills out of history")
	}
	for _, layer := range core.Layers {
		if _, err := os.Stat(filepath.Join(root, string(layer))); err != nil {
			t.Fatalf("layer %s was not scaffolded: %v", layer, err)
		}
	}
	// A base fkf just wrote is trusted here, so `fkf sync` works without a second command.
	if err := core.RequireTrust(t.Context(), root); err != nil {
		t.Fatalf("RequireTrust(t.Context(), ) = %v, want init to trust what it wrote", err)
	}
	// The file it wrote has to be one this build accepts, or the preset is a lie.
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatalf("the generated configuration does not load: %v", err)
	}
	if !strings.Contains(mustRead(t, filepath.Join(root, core.ConfigFileName)), core.SchemaURL) {
		t.Fatal("the generated file must point an editor at the published schema")
	}
	if config.Sync != core.DefaultSync() {
		t.Fatalf("sync = %+v, want every default spelled out and unchanged", config.Sync)
	}
	if len(report.Next) != 7 {
		t.Fatalf("next = %v, want the seven printed steps", report.Next)
	}
	quotedRoot := "'" + root + "'"
	status := "fkf status --base " + quotedRoot
	helpers := "fkf config helpers --refresh --base " + quotedRoot
	trustAfterEdit := "fkf trust --all --base " + quotedRoot
	sync := "fkf sync --base " + quotedRoot + " --days 7"
	joined := strings.Join(report.Next, "\n")
	for _, command := range []string{status, helpers, trustAfterEdit, sync} {
		if !strings.Contains(joined, command) {
			t.Fatalf("next = %v, want the path-qualified command %q", report.Next, command)
		}
	}
	if strings.Index(joined, "$EDITOR") >= strings.Index(joined, helpers) ||
		strings.Index(joined, helpers) >= strings.Index(joined, trustAfterEdit) ||
		strings.Index(joined, trustAfterEdit) >= strings.Index(joined, sync) {
		t.Fatalf("next = %v, want edit, helper refresh, re-trust, then sync", report.Next)
	}
}

func TestInitDefaultsToMinimalAndDemoUsesTheSameConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		demo int
	}{
		{name: "ordinary"},
		{name: "demo", demo: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "brain")
			report, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Demo: test.demo, SkipGit: true,
			}, clock)
			if err != nil {
				t.Fatal(err)
			}
			if report.Preset != services.PresetMinimal || report.Declared != 0 || report.Enabled != 0 {
				t.Fatalf("report = %+v, want the minimal zero-source configuration", report)
			}
			if test.demo > 0 && (report.Demo == nil || report.Demo.Days != test.demo) {
				t.Fatalf("demo = %+v, want %d synthetic days", report.Demo, test.demo)
			}
		})
	}
}

func TestPublicPresetVocabularyExcludesDemo(t *testing.T) {
	if got := strings.Join(services.Presets, ","); got != "minimal,personal,team" {
		t.Fatalf("Presets = %q, want only minimal, personal, and team", got)
	}
}

func TestInitDoesNotRunGitFromBaseBin(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	marker := installBaseGitSubstitute(t, root)

	report, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetMinimal,
	}, clock)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init executed the untrusted %s substitute: %v", filepath.Join(core.BaseBinDir, "git"), err)
	}
	if !core.NewStore(root, nil).Versioned() {
		t.Fatal("init did not run the host Git executable")
	}
	if report.Trusted {
		t.Fatal("init trusted a pre-existing base/bin/git substitute")
	}
}

func TestInitMakesARelativeBaseAbsolute(t *testing.T) {
	isolate(t)
	parent := t.TempDir()
	t.Chdir(parent)
	report, err := services.Init(t.Context(), services.InitRequest{
		Path: "brain", Preset: services.PresetPersonal, SkipGit: true,
	}, clock)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	want := filepath.Join(parent, "brain")
	if report.Base != want {
		t.Fatalf("report base = %q, want absolute %q", report.Base, want)
	}
	base, err := services.Open("brain")
	if err != nil {
		t.Fatalf("Open(relative base) error = %v", err)
	}
	if base.Root() != want {
		t.Fatalf("opened root = %q, want %q", base.Root(), want)
	}
	if helper, found := base.Env.LookPath("agent-memory-files"); !found || helper != filepath.Join(want, core.BaseBinDir, "agent-memory-files") {
		t.Fatalf("relative base helper = %q, %v; want its absolute trusted bin path", helper, found)
	}
}

func TestInitFailureLeavesCreationRetryable(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 47\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal,
	}, clock)
	if err == nil || !strings.Contains(err.Error(), "git init") {
		t.Fatalf("Init() error = %v, want the forced mid-create git failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, core.ConfigFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed init left %s behind (%v), so an ordinary retry would be misclassified as a refresh",
			core.ConfigFileName, statErr)
	}

	report, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock)
	if err != nil {
		t.Fatalf("retry Init() error = %v", err)
	}
	if !report.Created || !report.Trusted {
		t.Fatalf("retry report = %+v, want a complete newly created and trusted base", report)
	}
	for _, expected := range []string{
		core.BaseAgentsFile,
		filepath.Join(core.BaseBinDir, "git-log-json"),
		filepath.FromSlash(core.BaseSkillsDir + "/fkf-use/SKILL.md"),
	} {
		if _, statErr := os.Stat(filepath.Join(root, expected)); statErr != nil {
			t.Fatalf("retry did not complete %s: %v", expected, statErr)
		}
	}
	if err := core.RequireTrust(t.Context(), root); err != nil {
		t.Fatalf("retry left the base untrusted: %v", err)
	}
}

func TestInitFailureBeforeCompletionLeavesNoTrustRecord(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	// A directory at a generated-file target passes the scaffold directory preflight but makes
	// the demo's final atomic graph replacement fail. That puts the failure after every helper
	// has been installed and immediately before the trust step.
	if err := os.MkdirAll(filepath.Join(root, core.GraphFile), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	_, err := services.Init(t.Context(), services.InitRequest{
		Path: root, SkipGit: true, Demo: 1,
	}, clock)
	if err == nil {
		t.Fatal("Init() succeeded despite a directory occupying graph.tsv")
	}
	trustDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "fkf", "trust")
	entries, readErr := os.ReadDir(trustDir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed init left %d trust record(s); trust must be the final mutation", len(entries))
	}
	if _, statErr := os.Stat(filepath.Join(root, core.ConfigFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed init left its creation marker: %v", statErr)
	}
}

func TestInitDoesNotAutoTrustExecutionInputsItDidNotCreate(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(filepath.Join(root, core.BaseBinDir), 0o700); err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(root, core.BaseBinDir, "shadow")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\necho pre-existing\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, core.LocalConfigName), []byte("bin: [~/tools]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetMinimal, SkipGit: true,
	}, clock)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	state, err := core.ReadTrust(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Trusted || state.Trusted {
		t.Fatal("init auto-trusted a local overlay or script that existed before the base")
	}
	if len(report.Next) == 0 || !strings.Contains(report.Next[0], "fkf trust --all") {
		t.Fatalf("next = %v, want review to precede sync", report.Next)
	}
	if got := mustRead(t, shadow); !strings.Contains(got, "pre-existing") {
		t.Fatalf("pre-existing script was overwritten: %q", got)
	}
}

// assertSourcesAreComplete holds every preset entry to the one source shape: a collection
// command and the fields.id its records are addressed by.
func assertSourcesAreComplete(t *testing.T, preset string, config *core.Config) {
	t.Helper()
	for name, source := range config.Sources {
		if source.Fields.Path(core.FieldID).IsZero() {
			t.Fatalf("%s collector %s declares no fields.id: %+v", preset, name, source)
		}
	}
}

func TestEveryPresetProducesALoadableConfiguration(t *testing.T) {
	for _, preset := range services.Presets {
		t.Run(preset, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "base")
			if _, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Preset: preset, SkipGit: true,
			}, clock); err != nil {
				t.Fatalf("Init(--preset %s) error = %v", preset, err)
			}
			config, err := core.LoadConfig(root)
			if err != nil {
				t.Fatalf("the %s preset does not load: %v", preset, err)
			}
			assertSourcesAreComplete(t, preset, config)
			if preset == services.PresetPersonal {
				history := config.Sources["shell-commands"]
				if history == nil || !history.Fields.Path(core.FieldTitle).IsZero() || slices.Contains(history.Run, "command") {
					t.Fatalf("personal shell history must project activity metadata without raw command text: %+v", history)
				}
			}
			if preset == services.PresetTeam {
				for name, source := range config.Sources {
					if source.Enabled {
						t.Fatalf("the team preset enables %s; enabling a source must be a reviewed commit", name)
					}
				}
			}
		})
	}
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: filepath.Join(t.TempDir(), "b"), Preset: "invented", SkipGit: true,
	}, clock); err == nil {
		t.Fatal("Init() must refuse an unknown preset and list the real ones")
	}
}

// TestInitTwiceIsANoOpDiff is what makes a refresh safe on a base that has been lived in.
func TestInitTwiceIsANoOpDiff(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	request := services.InitRequest{Path: root, Preset: services.PresetPersonal, SkipGit: true}
	if _, err := services.Init(t.Context(), request, clock); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	staleSkillFile := filepath.Join(root, filepath.FromSlash(core.BaseSkillsDir), "fkf-use", "removed-upstream.md")
	if err := os.WriteFile(staleSkillFile, []byte("obsolete bundled resource\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Something the base's humans own, which init must never touch.
	if err := os.WriteFile(filepath.Join(root, core.BaseAgentsFile), []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"),
		[]byte("my-own-rule\n\n"+mustRead(t, filepath.Join(root, ".gitignore"))), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := services.Init(t.Context(), request, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Refreshed || report.Created {
		t.Fatalf("report = %+v, want a refresh", report)
	}
	after := snapshot(t, root)
	if _, present := after[filepath.ToSlash(filepath.Join(core.BaseSkillsDir, "fkf-use", "removed-upstream.md"))]; present {
		t.Fatal("init left a file removed from the bundled fkf-owned skill")
	}
	if after[core.ConfigFileName] != before[core.ConfigFileName] {
		t.Fatal("init rewrote the base's own configuration")
	}
	if after[core.BaseAgentsFile] != "# mine\n" {
		t.Fatal("init overwrote the base's own AGENTS.md")
	}
	if !strings.Contains(after[".gitignore"], "my-own-rule") {
		t.Fatal("the refresh dropped a rule the owner added outside the managed block")
	}
	if !strings.Contains(after[".gitignore"], "events/") {
		t.Fatal("the refresh dropped the managed block itself")
	}
	// The skills are fkf's, so they come back byte-identical.
	for _, name := range services.BundledSkills {
		path := filepath.FromSlash(core.BaseSkillsDir + "/" + name + "/SKILL.md")
		if after[path] != before[path] {
			t.Fatalf("the refresh changed %s", path)
		}
	}
	for _, step := range report.Steps {
		if step.Item == core.ConfigFileName && step.Changed {
			t.Fatal("the report claims it rewrote the configuration")
		}
	}
}

func TestInitRefreshRefusesSymlinkedOwnedPaths(t *testing.T) {
	for _, test := range []struct {
		name  string
		link  string
		seed  map[string]string
		clear string
	}{
		{
			name: "skill directory", link: filepath.FromSlash(core.BaseSkillsDir + "/fkf-use"),
			clear: filepath.FromSlash(core.BaseSkillsDir + "/fkf-use"),
			seed:  map[string]string{"SKILL.md": "outside skill\n"},
		},
		{
			name: "skills parent", link: ".agents",
			clear: ".agents",
			seed: map[string]string{
				"skills/fkf-use/SKILL.md":   "outside use skill\n",
				"skills/fkf-learn/SKILL.md": "outside learn skill\n",
			},
		},
		{name: "bin directory", link: core.BaseBinDir, clear: core.BaseBinDir},
		{name: "graph file", link: core.GraphFile, clear: core.GraphFile, seed: map[string]string{"sentinel": "outside\n"}},
		{name: "git metadata", link: ".git", clear: ".git", seed: map[string]string{"HEAD": "ref: refs/heads/main\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "brain")
			request := services.InitRequest{Path: root, Preset: services.PresetMinimal, SkipGit: true}
			if _, err := services.Init(t.Context(), request, clock); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			for relative, body := range test.seed {
				absolute := filepath.Join(outside, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshot(t, outside)
			if err := os.RemoveAll(filepath.Join(root, test.clear)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, test.link)); err != nil {
				t.Skipf("symlinks are unavailable: %v", err)
			}

			if _, err := services.Init(t.Context(), request, clock); !errors.Is(err, core.ErrUnsafePath) {
				t.Fatalf("Init() error = %v, want the symlinked owned path refused", err)
			}
			if after := snapshot(t, outside); !maps.Equal(before, after) {
				t.Fatalf("init wrote through %s: before=%v after=%v", test.link, before, after)
			}
		})
	}
}

func TestInitTrackCollectedFlipsTheManagedBlock(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetMinimal, TrackCollected: true, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	ignore := mustRead(t, filepath.Join(root, ".gitignore"))
	if strings.Contains(ignore, "\nevents/\n") {
		t.Fatal("--track-collected must not ignore the collected layers")
	}
	if !strings.Contains(ignore, "append-only") {
		t.Fatal("the block must say that committing cannot be undone")
	}
	// .gitignore is the truth; there is no configuration key to disagree with it.
	tracks, err := services.TracksCollected(root)
	if err != nil || !tracks {
		t.Fatalf("TracksCollected() = %v, %v", tracks, err)
	}
}

func TestInitDemoFillsAnEmptyBaseAndRefusesAFullOne(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "demo")
	report, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Demo: 5, SkipGit: true,
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if report.Demo == nil || report.Demo.Days != 5 || report.Demo.Records == 0 {
		t.Fatalf("demo = %+v", report.Demo)
	}
	base := openBase(t, root, nil)
	listing, err := services.ListEvents(t.Context(), base, services.Window{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Days) != 5 {
		t.Fatalf("days = %d, want 5", len(listing.Days))
	}
	wiki, err := services.BuildWikiIndex(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if wiki.Stale {
		t.Fatal("fresh demo left wiki/index.md stale; init must finish every derived cache")
	}
	if _, err := services.WriteDemo(t.Context(), base, 5); err == nil {
		t.Fatal("--demo must refuse a base that already holds collected days")
	}
}

func TestInitRejectsDemoWithAnyPresetBeforeCreatingTheTarget(t *testing.T) {
	for _, preset := range []string{services.PresetMinimal, services.PresetPersonal} {
		t.Run(preset, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "mixed")
			_, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Preset: preset, Demo: 1, SkipGit: true,
			}, clock)
			if err == nil || !errors.Is(err, core.ErrConfig) {
				t.Fatalf("Init(%s + demo) error = %v, want a configuration refusal", preset, err)
			}
			if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("refused mixed demo created %s: %v", root, statErr)
			}
		})
	}
}

func TestWriteDemoRefusesAuthoredContentWithoutCollectedDays(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/owner-authored.md", "# Owner authored\n")
	if _, err := services.WriteDemo(t.Context(), base, 1); err == nil ||
		!strings.Contains(err.Error(), "wiki/owner-authored.md") {
		t.Fatalf("WriteDemo() error = %v, want the occupied authored page named", err)
	}
}

func TestInitRejectsInvalidDemoDaysBeforeCreatingTheTarget(t *testing.T) {
	isolate(t)
	for _, days := range []int{-1, 367} {
		t.Run(fmt.Sprintf("days_%d", days), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			_, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Demo: days,
			}, clock)
			if !errors.Is(err, core.ErrConfig) {
				t.Fatalf("Init(Demo: %d) error = %v, want ErrConfig", days, err)
			}
			if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid --demo created target %s: %v", root, statErr)
			}
		})
	}
}

func TestThirtyDayDemoSupportsTheDocumentedContextExpansion(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Demo: 30, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	base := openBase(t, root, nil)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-418", Budget: 700, Expand: true, Explain: true,
	})
	if err != nil {
		t.Fatalf("documented demo context expansion failed: %v", err)
	}
	foundExpansion := false
	for _, item := range pack.Items {
		for _, reason := range item.Reasons {
			if reason.Reason == "join-expansion" {
				foundExpansion = true
			}
		}
	}
	if !foundExpansion {
		t.Fatalf("expanded demo pack has no join-expansion reason: %+v", pack.Items)
	}
}

func TestThirtyDayDemoKeepsAnOversizedPinAuditable(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Demo: 30, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	base := openBase(t, root, nil)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Budget: 550, Pins: []string{"projects/fkf-rebuild.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if item.Pinned {
			t.Fatalf("oversized pin was unexpectedly admitted: %+v", item)
		}
	}
	if len(pack.Receipt.RejectedPins) != 1 || pack.Receipt.RejectedPins[0] != "projects/fkf-rebuild.md" {
		t.Fatalf("rejected_pins = %v, want the requested pin named", pack.Receipt.RejectedPins)
	}
	if len(pack.Receipt.Dropped) == 0 || pack.Receipt.Dropped[0].URI != "projects/fkf-rebuild.md" ||
		!pack.Receipt.Dropped[0].Pinned || pack.Receipt.Dropped[0].Reason != "budget" {
		t.Fatalf("first drop = %+v, want the requested pin preserved ahead of other budget drops",
			pack.Receipt.Dropped)
	}
	tiny, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Budget: 256, Pins: []string{"projects/fkf-rebuild.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tiny.Receipt.Dropped) != 0 || len(tiny.Receipt.RejectedPins) != 1 ||
		tiny.Receipt.RejectedPins[0] != "projects/fkf-rebuild.md" {
		t.Fatalf("tiny receipt dropped=%+v rejected_pins=%v; the pin must remain auditable without detail",
			tiny.Receipt.Dropped, tiny.Receipt.RejectedPins)
	}
}

// TestDemoIsByteIdenticalOnTheSameLocalDay is what lets the README quote its output and the
// retrieval smoke test assert against it without the invocation time leaking into metadata.
func TestDemoIsByteIdenticalOnTheSameLocalDay(t *testing.T) {
	render := func(now time.Time) map[string]string {
		isolate(t)
		root := filepath.Join(t.TempDir(), "demo")
		if _, err := services.Init(t.Context(), services.InitRequest{
			Path: root, Demo: 3, SkipGit: true,
		}, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
		return snapshot(t, root)
	}
	first := render(time.Date(2026, 5, 10, 1, 2, 3, 0, time.FixedZone("east", 14*60*60)))
	second := render(time.Date(2026, 5, 10, 23, 59, 58, 0, time.FixedZone("west", -10*60*60)))
	if !maps.Equal(first, second) {
		t.Fatal("`--demo` is not reproducible; the same N on the same local day changed base bytes")
	}
}

func TestDemoCapturesTheEvaluationClockOnce(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	instants := []time.Time{
		time.Date(2026, 5, 10, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
	}
	calls := 0
	base.Now = func() time.Time {
		instant := instants[min(calls, len(instants)-1)]
		calls++
		return instant
	}

	if _, err := services.WriteDemo(t.Context(), base, 1); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("demo clock sampled %d times; a midnight rollover can mix evaluation days", calls)
	}
}

func TestDemoIsValidAtBothEndsOfTheTimezoneRange(t *testing.T) {
	for _, zone := range []string{"Pacific/Apia", "Etc/GMT+12"} {
		t.Run(zone, func(t *testing.T) {
			isolate(t)
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			previous := time.Local
			time.Local = location
			t.Cleanup(func() { time.Local = previous })

			root := filepath.Join(t.TempDir(), "demo")
			now := time.Date(2026, 8, 25, 12, 0, 0, 0, location)
			if _, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Demo: 1, SkipGit: true,
			}, func() time.Time { return now }); err != nil {
				t.Fatalf("Init(--demo) in %s: %v", zone, err)
			}

			base := openBase(t, root, nil)
			if report, err := services.Verify(t.Context(), base); err != nil || len(report.Findings) != 0 {
				t.Fatalf("Verify() in %s = %+v, %v; want a valid portable demo", zone, report, err)
			}
		})
	}
}

func TestDemoBuildsDerivedFilesWithoutTheIndexLayer(t *testing.T) {
	config := strings.Replace(baseConfig, "  index: true", "  index: false", 1)
	base := newBase(t, config, nil)
	if _, err := services.WriteDemo(t.Context(), base, 2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{core.GraphFile, core.GraphMetaFile} {
		if _, err := os.Stat(filepath.Join(base.Root(), name)); err != nil {
			t.Fatalf("demo did not build root %s without index/: %v", name, err)
		}
	}
}

func TestTrustPrintsTheCommandsBeforeRecordingThem(t *testing.T) {
	config := baseConfig + `
  dormant:
    enabled: false
    layer: events
    run: [dormant, --json]
    fields:
      id: .id
      time: .t
`
	base := newBase(t, config, nil)
	report, err := services.Trust(t.Context(), base, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Commands) != 2 || len(report.Commands[0].Run) == 0 || len(report.Commands[1].Body) == 0 {
		t.Fatalf("commands = %+v, want every declared run:/body: line", report.Commands)
	}
	if report.Commands[0].Name != "dormant" || report.Commands[0].Enabled || report.Commands[1].Name != "synthetic" || !report.Commands[1].Enabled {
		t.Fatalf("commands = %+v, want disabled and enabled state disclosed in stable name order", report.Commands)
	}
	// --check reports and records nothing, so a hook can ask without deciding.
	if report.Recorded || report.State.Trusted {
		t.Fatalf("report = %+v, want nothing recorded", report)
	}
	recorded, err := services.Trust(t.Context(), base, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.Recorded || !recorded.State.Trusted {
		t.Fatalf("report = %+v, want the digest recorded", recorded)
	}
}

func TestTrustRecordsTheExactPlanItDiscloses(t *testing.T) {
	openedConfig := strings.Replace(baseConfig, "run: [cli, --since", "run: [reviewed-cli, --since", 1)
	base := newBase(t, openedConfig, nil)

	diskConfig := strings.Replace(openedConfig, "run: [reviewed-cli, --since", "run: [changed-cli, --since", 1)
	if err := os.WriteFile(filepath.Join(base.Root(), core.ConfigFileName), []byte(diskConfig), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	report, err := services.Trust(t.Context(), base, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Commands) != 1 || !slices.Contains(report.Commands[0].Run, "reviewed-cli") {
		t.Fatalf("commands = %+v, want the opened plan that was disclosed", report.Commands)
	}
	if err := core.RequireTrustConfig(t.Context(), base.Config); err != nil {
		t.Fatalf("disclosed plan was not trusted: %v", err)
	}
	if err := core.RequireTrust(t.Context(), base.Root()); !errors.Is(err, core.ErrUntrusted) {
		t.Fatalf("changed disk plan error = %v, want it to remain untrusted", err)
	}
}

// --- helpers ------------------------------------------------------------------------------

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			files[relative] = "symlink:" + target
		} else {
			files[relative] = mustRead(t, current)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestRefreshWritesTheHookButNeverAScriptThatExists: the session-start hook belongs to every
// base, so a refresh adds it to a base scaffolded before it existed — and, like every script,
// once it is there it is the owner's and is never rewritten.
func TestRefreshWritesTheHookButNeverAScriptThatExists(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	request := services.InitRequest{Path: root, Preset: services.PresetPersonal, SkipGit: true}
	if _, err := services.Init(t.Context(), request, clock); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	hook := filepath.Join(root, core.BaseBinDir, "fkf-hook")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(root, core.BaseBinDir, "git-log-json")
	if err := os.WriteFile(edited, []byte("#!/bin/sh\necho mine\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := services.Init(t.Context(), services.InitRequest{Path: root, SkipGit: true}, clock)
	if err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	if report.Created {
		t.Fatalf("report = %+v, want a refresh of the existing base", report)
	}
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("refresh did not write the missing hook: %v", err)
	}
	if got := mustRead(t, edited); got != "#!/bin/sh\necho mine\n" {
		t.Fatalf("refresh rewrote an existing script:\n%s", got)
	}
}
