package services_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const githubSearchHelper = "github-search-json"

type githubSearchRecord struct {
	URL string `json:"url"`
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func githubSearchAPIItem(url string) map[string]any {
	return map[string]any{
		"number":         1,
		"title":          "Synthetic search result",
		"html_url":       url,
		"updated_at":     "2026-05-04T00:00:00Z",
		"repository_url": "https://api.github.example.test/repos/acme/project",
		"state":          "open",
		"user":           map[string]any{"login": "octocat"},
	}
}

func writeGitHubSearchEnvelope(
	t *testing.T,
	path string,
	total int,
	incomplete bool,
	items []map[string]any,
) {
	t.Helper()
	if items == nil {
		items = []map[string]any{}
	}
	// The fake emits the metadata-only envelope produced by gh --jq for one REST page. The
	// helper's external jq then slurps every emitted page before validating the whole result.
	writeJSONFixture(t, path, map[string]any{
		"total_count":        total,
		"incomplete_results": incomplete,
		"items":              items,
	})
}

func writeFakeGitHubCLI(t *testing.T, directory, body string) {
	t.Helper()
	path := filepath.Join(directory, "gh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func githubSearchCommand(t *testing.T, fakeBin, fixtures, calls string, args ...string) *exec.Cmd {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "presets", "bin", githubSearchHelper)
	command := exec.CommandContext(t.Context(), script, args...)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_FIXTURE_DIR="+fixtures,
		"GH_CALL_LOG="+calls,
	)
	return command
}

func TestGitHubSearchJSONSplitsSaturatedWindowsAndDeduplicatesURLs(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "root.json"), 1000, false, nil)
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "left.json"), 2, false, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/pull/1"),
		githubSearchAPIItem("https://github.example.test/acme/project/pull/2"),
	})
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "right.json"), 2, false, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/pull/2"),
		githubSearchAPIItem("https://github.example.test/acme/project/pull/3"),
	})

	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *"q=is:pr author:@me updated:2026-05-04T00:00:00Z..2026-05-04T00:00:03Z"*) file=root.json ;;
  *"q=is:pr author:@me updated:2026-05-04T00:00:00Z..2026-05-04T00:00:01Z"*) file=left.json ;;
  *"q=is:pr author:@me updated:2026-05-04T00:00:02Z..2026-05-04T00:00:03Z"*) file=right.json ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubSearchCommand(t, fakeBin, fixtures, calls,
		"prs", "author", "2026-05-04T00:00:00Z", "2026-05-04T00:00:04Z")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-search-json error = %v\n%s", err, output)
	}
	var records []githubSearchRecord
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode github-search-json output: %v\n%s", err, output)
	}
	want := []string{
		"https://github.example.test/acme/project/pull/1",
		"https://github.example.test/acme/project/pull/2",
		"https://github.example.test/acme/project/pull/3",
	}
	got := make([]string, 0, len(records))
	for _, record := range records {
		got = append(got, record.URL)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("deduplicated URLs = %v, want %v", got, want)
	}

	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 3 {
		t.Fatalf("gh calls = %q, want one saturated query and its two halves", lines)
	}
	for _, expected := range []string{
		"q=is:pr author:@me updated:2026-05-04T00:00:00Z..2026-05-04T00:00:03Z",
		"q=is:pr author:@me updated:2026-05-04T00:00:00Z..2026-05-04T00:00:01Z",
		"q=is:pr author:@me updated:2026-05-04T00:00:02Z..2026-05-04T00:00:03Z",
	} {
		if !strings.Contains(string(log), expected) {
			t.Errorf("gh calls do not contain the half-open window rendered with inclusive end-1s %q:\n%s", expected, log)
		}
	}
	for _, expected := range []string{
		"api --method GET /search/issues",
		"X-GitHub-Api-Version: 2026-03-10",
		"per_page=100",
		"page=1",
		"--jq",
		"total_count,incomplete_results",
	} {
		if !strings.Contains(string(log), expected) {
			t.Errorf("gh calls omit REST completeness argument %q:\n%s", expected, log)
		}
	}
	if strings.Contains(string(log), "--paginate") {
		t.Fatalf("GitHub search still delegates to unbounded --paginate:\n%s", log)
	}
}

func TestGitHubSearchJSONFailsWhenAOneSecondWindowSaturates(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "saturated.json"), 1000, false, nil)
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
cat "$GH_FIXTURE_DIR/saturated.json"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubSearchCommand(t, fakeBin, fixtures, calls,
		"issues", "author", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-search-json accepted a saturated one-second slice")
	}
	if stdout.Len() != 0 {
		t.Fatalf("github-search-json emitted partial stdout before failing: %s", stdout.Bytes())
	}
	if message := stderr.String(); !strings.Contains(message, "one-second") ||
		!strings.Contains(message, "cannot prove completeness") {
		t.Fatalf("stderr = %q, want an actionable completeness failure", message)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "q=is:issue author:@me updated:2026-05-04T00:00:00Z..2026-05-04T00:00:00Z") {
		t.Fatalf("one-second half-open query did not use inclusive end-1s: %s", log)
	}
}

func TestGitHubSearchJSONRejectsIncompleteRESTResults(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "incomplete.json"), 1, true, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/issues/1"),
	})
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
cat "$GH_FIXTURE_DIR/incomplete.json"
`)
	command := githubSearchCommand(t, fakeBin, fixtures, filepath.Join(t.TempDir(), "calls"),
		"issues", "author", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-search-json accepted incomplete_results=true")
	}
	if stdout.Len() != 0 {
		t.Fatalf("incomplete search emitted partial stdout: %s", stdout.Bytes())
	}
	if message := stderr.String(); !strings.Contains(message, "incomplete_results=true") ||
		!strings.Contains(message, "2026-05-04T00:00:00Z") {
		t.Fatalf("incomplete-search error = %q, want the REST signal and affected range", message)
	}
}

func TestGitHubSearchJSONRejectsRetrievedCountBelowTotalCount(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "short.json"), 2, false, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/issues/1"),
	})
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "empty.json"), 2, false, nil)
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
case "$*" in
  *" page=1"*) file=short.json ;;
  *) file=empty.json ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	command := githubSearchCommand(t, fakeBin, fixtures, filepath.Join(t.TempDir(), "calls"),
		"issues", "author", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-search-json accepted fewer items than total_count")
	}
	if stdout.Len() != 0 {
		t.Fatalf("short REST result emitted partial stdout: %s", stdout.Bytes())
	}
	if message := stderr.String(); !strings.Contains(message, "retrieved 1 of 2") ||
		!strings.Contains(message, "cannot prove completeness") {
		t.Fatalf("count-mismatch error = %q, want an actionable completeness failure", message)
	}
}

func TestGitHubSearchJSONFetchesADeclaredSecondPageWithoutUnboundedPaginate(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "first.json"), 2, false, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/issues/1"),
	})
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "second.json"), 2, false, []map[string]any{
		githubSearchAPIItem("https://github.example.test/acme/project/issues/2"),
	})
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *"page=2"*) file=second.json ;;
  *) file=first.json ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubSearchCommand(t, fakeBin, fixtures, calls,
		"issues", "author", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-search-json error = %v\n%s", err, output)
	}
	var records []githubSearchRecord
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode github-search-json output: %v\n%s", err, output)
	}
	if len(records) != 2 {
		t.Fatalf("search records = %s, want both declared pages", output)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "--paginate") || !strings.Contains(string(log), "page=2") {
		t.Fatalf("search pagination is not finite and explicit:\n%s", log)
	}
}

func TestGitHubSearchJSONSelectsEverySupportedSearch(t *testing.T) {
	fixtures := t.TempDir()
	writeGitHubSearchEnvelope(t, filepath.Join(fixtures, "empty.json"), 0, false, nil)
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
cat "$GH_FIXTURE_DIR/empty.json"
`)
	for _, test := range []struct {
		name      string
		kind      string
		mode      string
		qualifier string
	}{
		{name: "authored pull requests", kind: "prs", mode: "author", qualifier: "q=is:pr author:@me"},
		{name: "authored issues", kind: "issues", mode: "author", qualifier: "q=is:issue author:@me"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			command := githubSearchCommand(t, fakeBin, fixtures, calls,
				test.kind, test.mode, "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("github-search-json error = %v\n%s", err, output)
			}
			if string(output) != "[]\n" {
				t.Fatalf("empty search output = %q, want one empty JSON array", output)
			}
			log, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(log), test.qualifier) {
				t.Fatalf("gh call = %q, want %q", log, test.qualifier)
			}
		})
	}
}

func TestGitHubSearchJSONRejectsTheObsoleteReviewedMode(t *testing.T) {
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "gh-called")
	writeFakeGitHubCLI(t, fakeBin, "#!/bin/sh\nprintf called > \"$GH_MARKER\"\n")
	command := githubSearchCommand(t, fakeBin, t.TempDir(), filepath.Join(t.TempDir(), "calls"),
		"prs", "reviewed", "2026-05-04T00:00:00Z", "2026-05-04T00:00:01Z")
	command.Env = append(command.Env, "GH_MARKER="+marker)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-search-json still accepts reviewed pull-request search rows")
	}
	if stdout.Len() != 0 {
		t.Fatalf("obsolete reviewed mode emitted output: %s", stdout.Bytes())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("obsolete reviewed mode reached gh: %v", err)
	}
	if !strings.Contains(stderr.String(), "expected prs author or issues author") {
		t.Fatalf("obsolete-mode error = %q", stderr.String())
	}
}

func TestGitHubSearchSourcesUseTheCompleteCollector(t *testing.T) {
	want := map[string][]string{
		"github-pull-requests": {githubSearchHelper, "prs", "author", "{{start}}", "{{end}}"},
		"github-issues":        {githubSearchHelper, "issues", "author", "{{start}}", "{{end}}"},
	}
	for _, preset := range []string{services.PresetPersonal} {
		t.Run(preset, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "base")
			if _, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Preset: preset, SkipGit: true,
			}, clock); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, core.BaseBinDir, githubSearchHelper)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("disabled %s sources materialized %s: %v", preset, githubSearchHelper, err)
			}
			config, err := core.LoadConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			for name, command := range want {
				source, exists := config.Sources[name]
				if !exists {
					t.Errorf("%s preset does not declare %s", preset, name)
					continue
				}
				if !slices.Equal(source.Run, command) {
					t.Errorf("%s source %s run = %q, want %q", preset, name, source.Run, command)
				}
			}
		})
	}
}
