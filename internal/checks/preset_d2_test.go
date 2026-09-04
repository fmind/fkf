package checks_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestReviewedPresetHelpersAnswerVersionWithoutProviderExecution(t *testing.T) {
	for _, name := range []string{
		"rss-json.sh", "chrome-bookmarks.sh", "gh-runs.sh", "github-commits-json.sh",
		"github-events-json.sh", "github-generic-list-json.sh", "github-gists-json.sh",
		"github-stars-json.sh", "gws-calendars-json.sh", "gws-chat-messages.sh",
		"gws-calendar-body.sh", "gws-chat-message-body.sh", "kaggle-competitions-json.sh", "kaggle-datasets-json.sh",
		"kaggle-json.sh", "kaggle-kernels-json.sh", "kaggle-models-json.sh",
		"huggingface-repositories-json.sh", "mise-tools-json.sh", "agent-prompts.sh",
		"agent-prompt-body.sh", "writing-source-json.sh", "writing-index.sh",
		"gmail-body.py",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(repositoryRoot(t), "presets", "bin", name)
			// The child PATH is cleared to prove no provider binary is consulted, so the
			// interpreter a helper's shebang names is resolved here instead. Running the Python
			// helper compiles it, which is the only gate this repository has on its syntax.
			interpreter := "/bin/sh"
			if filepath.Ext(name) == ".py" {
				resolved, err := exec.LookPath("python3")
				if err != nil {
					t.Fatalf("find python3 for %s: %v", name, err)
				}
				interpreter = resolved
			}
			command := exec.CommandContext(t.Context(), interpreter, path, "--version")
			command.Env = []string{"PATH=/nonexistent"}
			output, err := command.CombinedOutput()
			if err != nil || len(strings.TrimSpace(string(output))) == 0 {
				t.Fatalf("%s --version = %q, %v; provider execution must not be required", name, output, err)
			}
		})
	}
}

func TestGmailPresetInstallHintProvidesEveryNonSystemRuntime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "personal")
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	source, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	gmail := source.Sources["google-gmail-emails"]
	if gmail == nil || !slices.Contains(gmail.Requires, "python3") ||
		!strings.Contains(gmail.Install, "python@latest") {
		t.Fatalf("Gmail source = %#v, want python3 required and supplied by its install hint", gmail)
	}
}

func TestSessionTracePresetsDeclareFIFORequirement(t *testing.T) {
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
		source := config.Sources["agent-session-traces"]
		if source == nil || !slices.Contains(source.Requires, "mkfifo") {
			t.Errorf("%s session trace source = %#v, want mkfifo declared", preset, source)
		}
	}
}

func TestPersonalPresetIncludesReviewedBrainCollectors(t *testing.T) {
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
	for _, name := range []string{
		"agent-prompts", "chrome-bookmarks", "github-commits", "github-events",
		"github-runs", "github-stars", "github-gists", "google-calendars",
		"google-chat-messages", "google-docs", "google-meet-calls",
		"kaggle-competitions", "kaggle-datasets", "kaggle-kernels", "kaggle-models",
		"huggingface-repositories", "mise-tools", "rss-items", "writing-documents",
	} {
		if config.Sources[name] == nil {
			t.Errorf("personal preset is missing reviewed collector %q", name)
		}
	}

	calendar := config.Sources["google-calendar-events"]
	if calendar == nil || !slices.Equal(calendar.Run,
		[]string{"gws-calendars-json.sh", "{{start}}", "{{end}}", "{{date}}", "{{next_date}}"}) {
		t.Fatalf("calendar collector = %#v, want the all-calendars helper", calendar)
	}
	if calendar.Timeout != 5*time.Minute {
		t.Errorf("all-calendar timeout = %s, want 5m", calendar.Timeout)
	}
}

func TestReviewedPresetDefectsStayClosed(t *testing.T) {
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
	if config.Sources["google-chat-spaces"].Layer != core.LayerIndex {
		t.Errorf("Chat spaces layer = %s, want index", config.Sources["google-chat-spaces"].Layer)
	}
	if config.Sources["google-gmail-emails"].Timeout != 15*time.Minute {
		t.Errorf("Gmail timeout = %s, want 15m", config.Sources["google-gmail-emails"].Timeout)
	}
	drive := strings.Join(config.Sources["google-drive-files"].Run, " ")
	if !strings.Contains(drive, "nextPageToken") {
		t.Errorf("Drive argv omits nextPageToken: %s", drive)
	}
	for _, name := range []string{"github-pull-requests", "github-issues", "github-commits"} {
		source := config.Sources[name]
		if source == nil || source.Retry.Attempts != 3 ||
			!slices.Contains(source.Retry.On, "API rate limit exceeded") ||
			!slices.Contains(source.Retry.On, "secondary rate limit") {
			t.Errorf("%s retry policy = %#v, want bounded GitHub search rate handling", name, source)
		}
	}
	chromium, err := os.ReadFile(filepath.Join(repositoryRoot(t), "presets", "bin", "chromium-pages.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chromium), "SQLite online backup") ||
		!strings.Contains(string(chromium), "live WAL") {
		t.Fatal("Chromium helper must explain that its online backup includes committed live-WAL rows")
	}
}

// TestThePrivateFeedListIsIgnoredByTheManagedBlock ties the personal preset's optional OPML to
// the .gitignore block `fkf init` writes. A base is a git repository, and rss-json.sh opaques
// the endpoints that file holds before they reach evidence — work the first commit would undo.
func TestThePrivateFeedListIsIgnoredByTheManagedBlock(t *testing.T) {
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
	feeds := config.Sources["rss-items"]
	if feeds == nil {
		t.Fatal("personal preset is missing the rss-items collector")
	}
	var private string
	for index, value := range feeds.Run {
		if value == "--optional-opml" && index+1 < len(feeds.Run) {
			private = filepath.Base(feeds.Run[index+1])
		}
	}
	if private == "" {
		t.Fatalf("rss-items argv = %v, want it to name an optional private OPML", feeds.Run)
	}
	if !slices.Contains(strings.Split(services.ManagedIgnoreBlock(false), "\n"), private) {
		t.Errorf("the managed .gitignore block does not ignore %s, the private feed list the preset names", private)
	}
}
