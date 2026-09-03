package services_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestPresetRemoteSourcesDeclareProviderReadinessProbes(t *testing.T) {
	expected := map[string]map[string][]string{
		services.PresetPersonal: {
			"google-calendar-agenda":   {"gws", "auth", "status"},
			"github-commits":           {"gh", "auth", "status"},
			"github-events":            {"gh", "auth", "status"},
			"github-runs":              {"gh", "auth", "status"},
			"github-stars":             {"gh", "auth", "status"},
			"github-gists":             {"gh", "auth", "status"},
			"github-pull-requests":     {"gh", "auth", "status"},
			"github-issues":            {"gh", "auth", "status"},
			"github-reviews":           {"gh", "auth", "status"},
			"github-notifications":     {"gh", "auth", "status"},
			"github-repositories":      {"gh", "auth", "status"},
			"google-calendar-events":   {"gws", "auth", "status"},
			"google-calendars":         {"gws", "auth", "status"},
			"google-gmail-emails":      {"gws", "auth", "status"},
			"google-chat-messages":     {"gws", "auth", "status"},
			"google-chat-spaces":       {"gws", "auth", "status"},
			"google-docs":              {"gws", "auth", "status"},
			"google-meet-calls":        {"gws", "auth", "status"},
			"google-tasks-items":       {"gws", "auth", "status"},
			"google-drive-files":       {"gws", "auth", "status"},
			"google-contacts-people":   {"gws", "auth", "status"},
			"meeting-notes":            {"gws", "auth", "status"},
			"gcloud-audit-logs":        {"gcloud-auth-ready.sh"},
			"gcloud-projects":          {"gcloud-auth-ready.sh"},
			"kaggle-competitions":      {"kaggle", "config", "view"},
			"kaggle-datasets":          {"kaggle", "config", "view"},
			"kaggle-kernels":           {"kaggle", "config", "view"},
			"kaggle-models":            {"kaggle", "config", "view"},
			"huggingface-repositories": {"hf", "auth", "whoami"},
		},
		services.PresetTeam: {
			"github-repositories": {"gh", "auth", "status"},
		},
		services.PresetMinimal: {},
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
			want := expected[preset][name]
			if !reflect.DeepEqual(config.Sources[name].Auth, want) {
				t.Errorf("%s/%s auth = %q, want %q", preset, name, config.Sources[name].Auth, want)
			}
		}
	}
}
