package checks_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentSessionTraceSelectsTheLatestCompleteGenerationAndOnlyPathEvidence(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage")
	for generation, manifest := range map[string]string{
		"generation-a": `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T09:00:00Z","high_water_mark":"2026-05-04T08:30:00Z","record_count":2}`,
		"generation-b": `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":4}`,
	} {
		directory := filepath.Join(store, generation)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wrongDepthManifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"wrong-depth","completeness":"complete","ingested_at":"2026-05-04T11:00:00Z","high_water_mark":"2026-05-04T10:30:00Z","record_count":1}`
	for _, path := range []string{
		filepath.Join(store, "manifest.json"),
		filepath.Join(store, "generation-c", "nested", "manifest.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(wrongDepthManifest+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	worktree := filepath.Join(home, "work", "fkf")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"ts":"2026-05-04T08:00:00Z","agent":"codex","sid":"session-1","role":"user","content":"<system-reminder>private harness chrome</system-reminder>Implement session traces.","cwd":"` + worktree + `","model":"gpt-test"}`,
		`{"ts":"2026-05-04T08:01:00Z","agent":"codex","sid":"session-1","role":"user","content":"# AGENTS.md instructions for /untrusted","cwd":"` + worktree + `","model":"gpt-test"}`,
		`{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"session-1","role":"assistant","content":"Implemented it.\ngo test ./services -run SessionTrace\nrm -rf /must-not-be-classified","cwd":"` + worktree + `","model":"gpt-test"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(store, "generation-b", "transcript.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeGit := `#!/bin/sh
set -eu
case " $* " in
  *" rev-parse --is-inside-work-tree "*) printf '%s\n' true ;;
  *" rev-parse --verify HEAD "*) printf '%s\n' deadbeef ;;
  *" hash-object --stdin "*) printf '%s\n' 0123456789abcdef0123456789abcdef01234567 ;;
  *" status --short --untracked-files=normal "*) printf '%s\n' ' M services/session_trace.go' '?? services/session_trace_test.go' ;;
  *" config --get remote.origin.url "*) printf '%s\n' 'git@github.com:fmind/fkf.git' ;;
  *" log --since="*) printf '%s\n' 'services/committed.go' 'services/session_trace.go' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o700); err != nil {
		t.Fatal(err)
	}
	output := runAgentSessionTrace(t, home, bin, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var traces []struct {
		ID            string   `json:"id"`
		Harness       string   `json:"harness"`
		Repo          string   `json:"repo"`
		Requests      []string `json:"requests"`
		Files         []string `json:"files"`
		Verification  []string `json:"verification"`
		LastAssistant string   `json:"last_assistant"`
	}
	if err := json.Unmarshal(output, &traces); err != nil {
		t.Fatalf("decode session traces: %v\n%s", err, output)
	}
	if len(traces) != 1 || traces[0].ID != "codex:session-1" || traces[0].Harness != "codex" ||
		traces[0].Repo != "fmind/fkf" {
		t.Fatalf("session traces = %+v, want one latest complete normalized session", traces)
	}
	if len(traces[0].Requests) != 1 || traces[0].Requests[0] != "Implement session traces." ||
		strings.Contains(strings.Join(traces[0].Requests, "\n"), "AGENTS.md") {
		t.Fatalf("requests = %q, want harness chrome filtered before storage", traces[0].Requests)
	}
	if strings.Join(traces[0].Files, "\n") !=
		" M services/session_trace.go\n?? services/session_trace_test.go\nservices/committed.go\nservices/session_trace.go" {
		t.Fatalf("files = %q, want unique status and in-session commit paths", traces[0].Files)
	}
	if len(traces[0].Verification) != 1 || traces[0].Verification[0] != "go test ./services -run SessionTrace" ||
		!strings.Contains(traces[0].LastAssistant, "rm -rf /must-not-be-classified") {
		t.Fatalf("assistant evidence = commands %q message %q", traces[0].Verification, traces[0].LastAssistant)
	}

	excluded := runAgentSessionTrace(t, home, bin, "2026-05-05T00:00:00Z", "2026-05-06T00:00:00Z")
	if strings.TrimSpace(string(excluded)) != "[]" {
		t.Fatalf("outside-window trace output = %s, want []", excluded)
	}
}

func TestAgentSessionTraceUsesPortableFindSyntax(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agents", "sessions", "v1"), 0o700); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeFind := `#!/bin/sh
set -eu
case " $* " in
  *" -mindepth "* | *" -maxdepth "*) exit 64 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte(fakeFind), 0o700); err != nil {
		t.Fatal(err)
	}

	output := runAgentSessionTrace(t, home, bin, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if strings.TrimSpace(string(output)) != "[]" {
		t.Fatalf("empty trace output = %s, want []", output)
	}
}

func TestAgentSessionTraceAppliesWindowAfterSelectingLatestGeneration(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage")
	for generation, fixture := range map[string]struct {
		manifest   string
		transcript string
	}{
		"generation-old": {
			manifest:   `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1}`,
			transcript: `{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"session-1","role":"user","content":"Old snapshot must not become a trace."}` + "\n",
		},
		"generation-new": {
			manifest:   `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-05T10:00:00Z","high_water_mark":"2026-05-05T09:30:00Z","record_count":2}`,
			transcript: `{"ts":"2026-05-05T09:00:00Z","agent":"codex","sid":"session-1","role":"user","content":"Newest complete session snapshot."}` + "\n",
		},
	} {
		directory := filepath.Join(store, generation)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte(fixture.manifest+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "transcript.jsonl"), []byte(fixture.transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	earlier := runAgentSessionTrace(t, home, t.TempDir(), "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if strings.TrimSpace(string(earlier)) != "[]" {
		t.Fatalf("superseded generation output = %s, want []", earlier)
	}

	latest := runAgentSessionTrace(t, home, t.TempDir(), "2026-05-05T00:00:00Z", "2026-05-06T00:00:00Z")
	var traces []struct {
		Requests []string `json:"requests"`
	}
	if err := json.Unmarshal(latest, &traces); err != nil {
		t.Fatalf("decode latest session trace: %v\n%s", err, latest)
	}
	if len(traces) != 1 || len(traces[0].Requests) != 1 ||
		traces[0].Requests[0] != "Newest complete session snapshot." {
		t.Fatalf("latest session traces = %+v, want only the newest complete generation", traces)
	}
}

func TestAgentSessionTraceDoesNotExpireWhenTheStoreHasManyGenerations(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1}`
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := `{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"session-1","role":"user","content":"Keep an append-only store collectible."}` + "\n"
	if err := os.WriteFile(filepath.Join(generation, "transcript.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeFind := `#!/bin/sh
set -eu
case " $* " in
  *" -type f -name manifest.json -print0 "*)
    manifest=$HOME/.agents/sessions/v1/codex/lineage/generation/manifest.json
    count=0
    while [ "$count" -lt 8193 ]; do
      printf '%s\0' "$manifest"
      count=$((count + 1))
    done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte(fakeFind), 0o700); err != nil {
		t.Fatal(err)
	}

	output := runAgentSessionTrace(t, home, bin, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var traces []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &traces); err != nil {
		t.Fatalf("decode session traces: %v\n%s", err, output)
	}
	if len(traces) != 1 || traces[0].ID != "codex:session-1" {
		t.Fatalf("session traces = %+v, want one latest session after more than 8192 generations", traces)
	}
}

func TestAgentSessionTraceBoundsDistinctCandidatesWhileStreaming(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeXargs := `#!/bin/sh
set -eu
cat >/dev/null
count=0
while [ "$count" -le 8192 ]; do
  printf '{"agent":"codex","session_id":"session-%s","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1,"ingested":1777888800,"last":1777887000,"path":"/generation"}\n' "$count"
  count=$((count + 1))
done
`
	if err := os.WriteFile(filepath.Join(bin, "xargs"), []byte(fakeXargs), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-session-trace.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "more than 8192 completed sessions") {
		t.Fatalf("unbounded distinct candidates = error %v, output %q", err, output)
	}
}

func TestAgentSessionTraceHistoricalWindowIgnoresLaterDistinctSessions(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := `{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"in-window","role":"user","content":"Keep historical collection available."}` + "\n"
	if err := os.WriteFile(filepath.Join(generation, "transcript.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeXargs := `#!/bin/sh
set -eu
cat >/dev/null
printf '{"agent":"codex","session_id":"in-window","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1,"ingested":1777888800,"last":1777887000,"path":"%s"}\n' "$HOME/.agents/sessions/v1/codex/lineage/generation"
count=0
while [ "$count" -le 8192 ]; do
  printf '{"agent":"codex","session_id":"later-%s","ingested_at":"2026-05-06T10:00:00Z","high_water_mark":"2026-05-06T09:30:00Z","record_count":1,"ingested":1778061600,"last":1778059800,"path":"/later"}\n' "$count"
  count=$((count + 1))
done
`
	if err := os.WriteFile(filepath.Join(bin, "xargs"), []byte(fakeXargs), 0o700); err != nil {
		t.Fatal(err)
	}

	output := runAgentSessionTrace(t, home, bin, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var traces []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &traces); err != nil {
		t.Fatalf("decode historical session traces: %v\n%s", err, output)
	}
	if len(traces) != 1 || traces[0].ID != "codex:in-window" {
		t.Fatalf("historical session traces = %+v, want the in-window session after later store growth", traces)
	}
}

func TestAgentSessionTraceRejectsAPartialSecondCandidateScan(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1}`
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generation, "transcript.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	realFind, err := exec.LookPath("find")
	if err != nil {
		t.Skip("find is unavailable")
	}
	bin := t.TempDir()
	if err := os.Symlink(realFind, filepath.Join(bin, "real-find")); err != nil {
		t.Fatal(err)
	}
	fakeFind := `#!/bin/sh
set -eu
real_find=${0%/*}/real-find
case " $* " in
  *" -type f -name manifest.json -print0 "*)
    count_file=$HOME/find-print0-count
    count=0
    [ ! -f "$count_file" ] || count=$(cat "$count_file")
    count=$((count + 1))
    printf '%s\n' "$count" >"$count_file"
    "$real_find" "$@"
    [ "$count" -ne 2 ] || exit 7
    exit 0
    ;;
  *) exec "$real_find" "$@" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte(fakeFind), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-session-trace.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "could not enumerate session manifests") {
		t.Fatalf("partial second candidate scan = error %v, output %q", err, output)
	}
}

func TestAgentSessionTraceBoundsMultibyteExcerptsByUTF8Bytes(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":2}`
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := strings.Repeat("é", 2000) + "\u200d" + strings.Repeat("é", 2000)
	assistant := strings.Repeat("🙂", 1000) + "\u00ad" + strings.Repeat("🙂", 1000)
	transcript := strings.Join([]string{
		`{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"session-1","role":"user","content":` +
			mustMarshalJSON(t, request) + `}`,
		`{"ts":"2026-05-04T09:01:00Z","agent":"codex","sid":"session-1","role":"assistant","content":` +
			mustMarshalJSON(t, assistant) + `}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(generation, "transcript.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	output := runAgentSessionTrace(t, home, t.TempDir(), "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var traces []struct {
		Requests      []string `json:"requests"`
		LastAssistant string   `json:"last_assistant"`
	}
	if err := json.Unmarshal(output, &traces); err != nil {
		t.Fatalf("decode session traces: %v\n%s", err, output)
	}
	if len(traces) != 1 || len(traces[0].Requests) != 1 {
		t.Fatalf("session traces = %+v, want one trace with one request", traces)
	}
	if got := len([]byte(traces[0].Requests[0])); got > 6000 {
		t.Fatalf("request UTF-8 bytes = %d, want at most 6000", got)
	}
	if got := len([]byte(traces[0].LastAssistant)); got > 6000 {
		t.Fatalf("assistant UTF-8 bytes = %d, want at most 6000", got)
	}
	if strings.ContainsAny(traces[0].Requests[0]+traces[0].LastAssistant, "\u200d\u00ad") {
		t.Fatal("session excerpts retained invisible Unicode format characters")
	}
}

func TestAgentSessionTraceRejectsATranscriptPastEightMiB(t *testing.T) {
	home := t.TempDir()
	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":1}`
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(generation, "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(transcript, 8*1024*1024+1); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-session-trace.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "session transcript exceeds 8 MiB") {
		t.Fatalf("oversized transcript = error %v, output %q", err, output)
	}
}

func mustMarshalJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestAgentSessionTraceRefusesAnySessionStoreSymlink(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".agents", "sessions", "v1")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "outside")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store, "linked-harness")); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-session-trace.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "session store contains a symlink") {
		t.Fatalf("linked session store = error %v, output %q", err, output)
	}
}

func TestAgentSessionTraceDisablesRepositoryConfiguredFSMonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	home := t.TempDir()
	repo := filepath.Join(home, "work")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	git("init", "--quiet")
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	marker := filepath.Join(home, "FS_MONITOR_RAN")
	hook := filepath.Join(home, "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > '"+marker+"'\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git("config", "core.fsmonitor", hook)
	if err := os.WriteFile(tracked, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	generation := filepath.Join(home, ".agents", "sessions", "v1", "codex", "lineage", "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"parser_version":"1","agent":"codex","session_id":"session-1","completeness":"complete","ingested_at":"2026-05-04T10:00:00Z","high_water_mark":"2026-05-04T09:30:00Z","record_count":2}`
	if err := os.WriteFile(filepath.Join(generation, "manifest.json"), []byte(manifest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := `{"ts":"2026-05-04T09:00:00Z","agent":"codex","sid":"session-1","role":"user","content":"Test Git isolation.","cwd":"` + repo + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(generation, "transcript.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	runAgentSessionTrace(t, home, t.TempDir(), "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("agent session trace executed repository core.fsmonitor: %v", err)
	}
}

func runAgentSessionTrace(t *testing.T, home, bin, start, end string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-session-trace.sh"), start, end)
	command.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("agent-session-trace.sh error = %v\n%s", err, output)
	}
	return output
}
