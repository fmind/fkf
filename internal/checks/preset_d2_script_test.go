package checks_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChromeBookmarksNamespacesEveryProfile(t *testing.T) {
	home := t.TempDir()
	fixtures := map[string]string{
		".config/chromium/Default/Bookmarks":        `{"roots":{"bookmark_bar":{"type":"folder","name":"Bookmarks","children":[{"type":"url","guid":"one","name":"One","url":"https://one.example.test/path","date_added":"13300000000000000"}]}}}`,
		".config/google-chrome/Profile 1/Bookmarks": `{"roots":{"bookmark_bar":{"type":"folder","name":"Bookmarks","children":[{"type":"url","guid":"two","name":"Two","url":"https://two.example.test/path","date_added":"13300000000000000"}]}}}`,
	}
	for relative, body := range fixtures {
		path := filepath.Join(home, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "chrome-bookmarks.sh"))
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("chrome-bookmarks.sh error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode bookmark records: %v\n%s", err, output)
	}
	if len(records) != 2 || records[0]["uid"] == records[1]["uid"] {
		t.Fatalf("bookmark records = %s, want two profile-qualified identities", output)
	}
}

func TestAgentPromptsReadsOnlyExactWindowFromDurableStore(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, ".agents", "sessions", "v1", "agy", "lineage", "session", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`{"ts":"2026-08-26T23:59:59Z","role":"user","sid":"before","content":"Before.","cwd":"/tmp/work","model":"test"}`,
		`{"ts":"2026-08-27T00:00:00Z","role":"user","sid":"start","content":"At start.","cwd":"/tmp/work","model":"test"}`,
		`{"ts":"2026-08-27T12:00:00Z","role":"user","sid":"inside","content":"Inside.","cwd":"/tmp/work","model":"test"}`,
		`{"ts":"2026-08-28T00:00:00Z","role":"user","sid":"end","content":"At end.","cwd":"/tmp/work","model":"test"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-prompts.sh"),
		"2026-08-27T00:00:00Z", "2026-08-28T00:00:00Z", "0")
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("agent-prompts.sh error = %v\n%s", err, output)
	}
	var records []struct {
		Agent string `json:"agent"`
		SID   string `json:"sid"`
	}
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode prompt records: %v\n%s", err, output)
	}
	if len(records) != 2 || records[0].SID != "start" || records[1].SID != "inside" ||
		records[0].Agent != "antigravity" {
		t.Fatalf("prompt records = %+v, want the exact half-open window and canonical harness", records)
	}
}

func TestRSSCollectorNormalizesACompletePublicFeed(t *testing.T) {
	fixtureDir := t.TempDir()
	feed := filepath.Join(fixtureDir, "feed.xml")
	xml := `<rss version="2.0"><channel><title>Example</title><link>https://example.test</link><item><guid>post-1</guid><title>Post</title><link>https://example.test/post</link><pubDate>Mon, 04 May 2026 09:00:00 +0000</pubDate></item></channel></rss>`
	if err := os.WriteFile(feed, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeCurl := `#!/bin/sh
set -eu
output=
while [ "$#" -gt 0 ]; do
  case "$1" in --output) output=$2; shift 2 ;; *) shift ;; esac
done
cp "$RSS_FIXTURE" "$output"
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "rss-json.sh"),
		"https://example.test/feed.xml")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RSS_FIXTURE="+feed,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("rss-json.sh error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode RSS records: %v\n%s", err, output)
	}
	if len(records) != 2 || records[0]["kind"] != "feed" || records[1]["kind"] != "item" {
		t.Fatalf("RSS records = %s, want one feed and one addressable item", output)
	}
	for _, record := range records {
		if record["visibility"] != "public" {
			t.Fatalf("RSS record = %#v, want explicit public visibility", record)
		}
	}
}

func TestGitHubCommitSearchLeavesRateLimitEvidenceOnStderr(t *testing.T) {
	bin := t.TempDir()
	fakeGH := "#!/bin/sh\nprintf '%s\\n' 'API rate limit exceeded for this account' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-commits-json.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "API rate limit exceeded") {
		t.Fatalf("rate-limited search = %q, %v; retry.on needs the original command-failure evidence", output, err)
	}
}

func TestGitHubGenericListFollowsFiniteNumberedPages(t *testing.T) {
	bin := t.TempDir()
	fakeGH := `#!/bin/sh
set -eu
case "$*" in
  *" page=1"*) jq -cn '[range(0; 100) | {id:("page-1-" + tostring)}]' ;;
  *" page=2"*) printf '%s\n' '[{"id":"page-2"}]' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-generic-list-json.sh"),
		"/user/starred")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-generic-list-json.sh error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode GitHub listing: %v\n%s", err, output)
	}
	if len(records) != 101 || records[100]["id"] != "page-2" {
		t.Fatalf("GitHub listing has %d records, want 101 across two numbered pages", len(records))
	}
}
