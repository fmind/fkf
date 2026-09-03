package checks_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestGitHubGistsFallbackToAStableMeaningfulTitle(t *testing.T) {
	bin := t.TempDir()
	writeTitleTestExecutable(t, bin, "github-generic-list-json.sh", `#!/bin/sh
set -eu
printf '%s\n' '[{"id":"deadbeef","description":"","html_url":"https://gist.github.com/example/deadbeef","updated_at":"2026-05-04T09:00:00Z","public":true,"files":{"notes.md":{}}}]'
`)
	output := runTitleHelper(t, bin, "github-gists-json.sh")
	record := decodeTitleObject(t, output)
	if record["description"] != "Gist deadbeef" {
		t.Fatalf("gist description = %q, want a stable fallback title", record["description"])
	}
	assertPersonalOutputCollects(t, "github-gists", output)
}

func TestGitHubStarsRemoveFormatCharactersFromTitles(t *testing.T) {
	bin := t.TempDir()
	writeTitleTestExecutable(t, bin, "github-generic-list-json.sh", `#!/bin/sh
set -eu
printf '%s\n' '[{"starred_at":"2026-05-04T09:00:00Z","repo":{"full_name":"example/project","html_url":"https://github.com/example/project","description":"Soft\u00adware","language":"Go","topics":[],"stargazers_count":7,"archived":false}}]'
`)
	output := runTitleHelper(t, bin, "github-stars-json.sh")
	record := decodeTitleObject(t, output)
	if record["title"] != "example/project: Software" {
		t.Fatalf("star title = %q, want the invisible soft hyphen removed", record["title"])
	}
	assertPersonalOutputCollects(t, "github-stars", output)
}

func TestGWSCalendarsFallbackForAnEmptySummary(t *testing.T) {
	bin := t.TempDir()
	writeTitleTestExecutable(t, bin, "gws", `#!/bin/sh
set -eu
case "$*" in
  *"calendarList list"*)
    printf '%s\n' '{"items":[{"id":"calendar@example.test","summary":"Calendar","accessRole":"owner"}]}'
    ;;
  *"events list"*)
    printf '%s\n' '{"items":[{"id":"empty-summary","summary":"   ","status":"confirmed","eventType":"default","start":{"dateTime":"2026-05-04T09:00:00Z"},"end":{"dateTime":"2026-05-04T10:00:00Z"}}]}'
    ;;
  *) exit 2 ;;
esac
`)
	output := runTitleHelper(t, bin, "gws-calendars-json.sh",
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", "2026-05-04", "2026-05-05")
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode calendar records: %v\n%s", err, output)
	}
	if len(records) != 1 || records[0]["summary"] != "Calendar event empty-summary" {
		t.Fatalf("calendar records = %s, want a stable fallback for the empty summary", output)
	}
	assertPersonalOutputCollects(t, "google-calendar-agenda", output)
	assertPersonalOutputCollects(t, "google-calendar-events", output)
}

func TestRSSCollectorRemovesFormatCharactersFromTitles(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "feed.xml")
	xml := "<rss version=\"2.0\"><channel><title>Exa\u200bmple</title><link>https://example.test</link><item><guid>post-1</guid><title>Po\u200bst</title><link>https://example.test/post</link><pubDate>Mon, 04 May 2026 09:00:00 +0000</pubDate></item></channel></rss>"
	if err := os.WriteFile(fixture, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeTitleTestExecutable(t, bin, "curl", `#!/bin/sh
set -eu
output=
while [ "$#" -gt 0 ]; do
  case "$1" in --output) output=$2; shift 2 ;; *) shift ;; esac
done
cp "$RSS_TITLE_FIXTURE" "$output"
`)
	output := runTitleHelperWithEnv(t, bin, []string{"RSS_TITLE_FIXTURE=" + fixture},
		"rss-json.sh", "https://example.test/feed.xml")
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode RSS records: %v\n%s", err, output)
	}
	if len(records) != 2 || records[0]["title"] != "Example" || records[1]["title"] != "Post" {
		t.Fatalf("RSS records = %s, want invisible format characters removed from both titles", output)
	}
	assertPersonalOutputCollects(t, "rss-items", output)
}

func TestGmailCollectorRemovesFormatCharactersFromTheSubject(t *testing.T) {
	bin := t.TempDir()
	writeTitleTestExecutable(t, bin, "gws", `#!/bin/sh
set -eu
case "$*" in
  *"users messages list"*)
    printf '%s\n' '{"messages":[{"id":"message-1"}]}'
    ;;
  *'"id":"message-1"'*)
    printf '%s\n' '{"id":"message-1","threadId":"thread-1","internalDate":"1777885200000","payload":{"headers":[{"name":"Subject","value":"Zero\u200dWidth"}]}}'
    ;;
  *) exit 2 ;;
esac
`)
	output := runTitleHelper(t, bin, "gmail-json.sh", "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	record := decodeTitleObject(t, output)
	if record["subject"] != "ZeroWidth" {
		t.Fatalf("Gmail subject = %q, want the zero-width joiner removed", record["subject"])
	}
	assertPersonalOutputCollects(t, "google-gmail-emails", output)
}

func TestPersonalChatSpacesUseTheStableAPIResourceNameAsTitle(t *testing.T) {
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
	path := config.Sources["google-chat-spaces"].Fields.Path(core.FieldTitle).String()
	if path != ".name" {
		t.Fatalf("google-chat-spaces title path = %q, want the always-present API resource name", path)
	}
	assertPersonalOutputCollects(t, "google-chat-spaces", []byte(
		`{"spaces":[{"name":"spaces/AAAA1234","displayName":"   ","spaceType":"SPACE"}]}`,
	))
}

func runTitleHelper(t *testing.T, bin, helper string, arguments ...string) []byte {
	t.Helper()
	return runTitleHelperWithEnv(t, bin, nil, helper, arguments...)
}

func runTitleHelperWithEnv(t *testing.T, bin string, extraEnv []string, helper string, arguments ...string) []byte {
	t.Helper()
	helperBin := filepath.Join(repositoryRoot(t), "presets", "bin")
	commandArguments := append([]string{filepath.Join(helperBin, helper)}, arguments...)
	command := exec.CommandContext(t.Context(), "/bin/sh", commandArguments...)
	command.Env = append(os.Environ(), extraEnv...)
	command.Env = append(command.Env, "PATH="+strings.Join(
		[]string{bin, helperBin, os.Getenv("PATH")}, string(os.PathListSeparator)))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s error = %v\n%s", helper, err, output)
	}
	return output
}

func writeTitleTestExecutable(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func decodeTitleObject(t *testing.T, output []byte) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(output, &record); err != nil {
		t.Fatalf("decode helper record: %v\n%s", err, output)
	}
	return record
}

func assertPersonalOutputCollects(t *testing.T, name string, output []byte) {
	t.Helper()
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
	source := config.Sources[name]
	window := sources.Window{}
	if source.Layer == core.LayerEvents {
		day, err := sources.ParseDay("2026-05-04")
		if err != nil {
			t.Fatal(err)
		}
		window = sources.DayWindow(day)
	}
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		return string(output), nil
	})
	if _, err := sources.Collect(
		t.Context(), runner, source, sources.Environment{}, window, time.Minute, testClock,
	); err != nil {
		t.Fatalf("%s helper output failed the real collection validator: %v\n%s", name, err, output)
	}
}
