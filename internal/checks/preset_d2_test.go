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
		"gws-chat-message-body.sh", "kaggle-competitions-json.sh", "kaggle-datasets-json.sh",
		"kaggle-json.sh", "kaggle-kernels-json.sh", "kaggle-models-json.sh",
		"huggingface-repositories-json.sh", "mise-tools-json.sh", "agent-prompts.sh",
		"agent-prompt-body.sh", "writing-source-json.sh", "writing-index.sh",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(repositoryRoot(t), "presets", "bin", name)
			command := exec.CommandContext(t.Context(), "/bin/sh", path, "--version")
			command.Env = []string{"PATH=/nonexistent"}
			output, err := command.CombinedOutput()
			if err != nil || len(strings.TrimSpace(string(output))) == 0 {
				t.Fatalf("%s --version = %q, %v; provider execution must not be required", name, output, err)
			}
		})
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
