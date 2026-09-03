package checks_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writePresetFake(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func linkPresetTool(t *testing.T, bin, name string) {
	t.Helper()
	target, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("find %s for preset test: %v", name, err)
	}
	if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
}

func runPresetScript(
	t *testing.T,
	name, home, bin string,
	extraEnv []string,
	args ...string,
) (string, string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", name), args...)
	command.Env = append([]string{"HOME=" + home, "PATH=" + bin}, extraEnv...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func requirePresetFailureWithoutOutput(t *testing.T, name, stdout, stderr string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted a failed producer; stdout=%q stderr=%q", name, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("%s emitted partial stdout after a producer failed: %q", name, stdout)
	}
}

func TestAgentSessionsFailsWithoutOutputWhenSQLiteFails(t *testing.T) {
	home := t.TempDir()
	database := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"jq", "mktemp", "rm", "sed", "touch"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", "exit 0\n")
	writePresetFake(t, bin, "sqlite3", `printf '%s\n' '[{"id":"partial","time_created":1777885200000,"title":"partial","directory":null}]'
exit 37
`)

	stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	requirePresetFailureWithoutOutput(t, "agent-sessions.sh", stdout, stderr, err)
}

func TestAgentSessionsFailsWithoutOutputWhenFindFails(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"jq", "mktemp", "rm", "sed", "touch"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", "exit 38\n")

	stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	requirePresetFailureWithoutOutput(t, "agent-sessions.sh", stdout, stderr, err)
}

func TestAgentSessionsQualifiesProviderIDsByHarness(t *testing.T) {
	home := t.TempDir()
	database := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(home, ".gemini", "antigravity-cli", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(history), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history,
		[]byte("{\"conversationId\":\"shared\",\"timestamp\":1777885200000}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"jq", "mktemp", "rm", "sed", "touch"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", "exit 0\n")
	writePresetFake(t, bin, "sqlite3", `printf '%s\n' '[{"id":"shared","activity_time":1777885200000,"title":"OpenCode","directory":null}]'
`)

	stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if err != nil {
		t.Fatalf("agent-sessions.sh failed: %v; stderr=%q", err, stderr)
	}
	var records []struct {
		ID    string `json:"id"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("decode agent-sessions.sh output: %v; stdout=%q", err, stdout)
	}
	want := map[string]string{
		"antigravity:shared": "antigravity",
		"opencode:shared":    "opencode",
	}
	if len(records) != len(want) {
		t.Fatalf("agent-sessions.sh records = %+v, want one per harness", records)
	}
	for _, record := range records {
		if want[record.ID] != record.Agent {
			t.Fatalf("agent-sessions.sh record = %+v, want a harness-qualified id", record)
		}
		delete(want, record.ID)
	}
	if len(want) != 0 {
		t.Fatalf("agent-sessions.sh missing qualified ids: %v", want)
	}
}

func TestAgentSessionsCollectsPrimaryGrokSessions(t *testing.T) {
	home := t.TempDir()
	session := filepath.Join(home, ".grok", "sessions", "workspace", "session-1")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "events.jsonl"), []byte(
		"{\"ts\":\"2026-05-04T08:00:00Z\",\"session_id\":\"grok-session\",\"session_relationship\":\"primary\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "summary.json"), []byte(
		`{"info":{"id":"grok-session","cwd":"/missing/workspace"},"head_branch":"main","git_remotes":["https://github.com/example/project.git"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"find", "jq", "mktemp", "rm", "sed", "touch"} {
		linkPresetTool(t, bin, name)
	}

	stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if err != nil {
		t.Fatalf("agent-sessions.sh failed: %v; stderr=%q", err, stderr)
	}
	var records []struct {
		ID            string `json:"id"`
		Agent         string `json:"agent"`
		Repo          string `json:"repo"`
		RepositoryURI string `json:"repository_uri"`
	}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("decode agent-sessions.sh output: %v; stdout=%q", err, stdout)
	}
	if len(records) != 1 || records[0].ID != "grok:grok-session" || records[0].Agent != "grok" ||
		records[0].Repo != "example/project" || records[0].RepositoryURI != "repo:github.com/example/project" {
		t.Fatalf("agent-sessions.sh records = %+v, want one repository-qualified Grok session", records)
	}
}

func TestAgentSessionsProjectsOnlyGitHubCopilotRepositoriesWithoutUserinfo(t *testing.T) {
	home := t.TempDir()
	database := filepath.Join(home, ".copilot", "session-store.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "sqlite3", database, `
CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, branch TEXT, summary TEXT,
  created_at TEXT, updated_at TEXT);
CREATE TABLE turns (session_id TEXT NOT NULL, timestamp TEXT);
INSERT INTO sessions VALUES ('safe', NULL,
  'https://secret-user:secret-password@github.com/example/project.git', NULL, 'safe',
  '2026-05-04T08:00:00Z', '2026-05-04T08:00:00Z');
INSERT INTO sessions VALUES ('single', NULL, 'single', NULL, 'single',
  '2026-05-04T09:00:00Z', '2026-05-04T09:00:00Z');
INSERT INTO sessions VALUES ('malformed', NULL,
  'https://leaky-user:leaky-password@github.com', NULL, 'malformed',
  '2026-05-04T10:00:00Z', '2026-05-04T10:00:00Z');
INSERT INTO sessions VALUES ('github-scp', NULL,
  'git@github.com:example/scp.git', NULL, 'github-scp',
  '2026-05-04T11:00:00Z', '2026-05-04T11:00:00Z');
INSERT INTO sessions VALUES ('gitlab-url', NULL,
  'https://gitlab-user:gitlab-password@gitlab.com/example/project.git', NULL, 'gitlab-url',
  '2026-05-04T12:00:00Z', '2026-05-04T12:00:00Z');
INSERT INTO sessions VALUES ('gitlab-scp', NULL,
  'git@gitlab.com:example/project.git', NULL, 'gitlab-scp',
  '2026-05-04T13:00:00Z', '2026-05-04T13:00:00Z');
`)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create Copilot fixture: %v; output=%q", err, output)
	}
	bin := t.TempDir()
	for _, name := range []string{"find", "jq", "mktemp", "rm", "sqlite3", "touch"} {
		linkPresetTool(t, bin, name)
	}

	stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	if err != nil {
		t.Fatalf("agent-sessions.sh failed: %v; stderr=%q", err, stderr)
	}
	for _, forbidden := range []string{
		"secret-user", "secret-password", "leaky-user", "leaky-password", "gitlab-user", "gitlab-password",
	} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("agent-sessions.sh leaked Copilot repository userinfo %q to stdout: %s", forbidden, stdout)
		}
	}
	var records []struct {
		ID   string `json:"id"`
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("decode agent-sessions.sh output: %v; stdout=%q", err, stdout)
	}
	want := map[string]string{
		"copilot:safe":       "example/project",
		"copilot:single":     "",
		"copilot:malformed":  "",
		"copilot:github-scp": "example/scp",
		"copilot:gitlab-url": "",
		"copilot:gitlab-scp": "",
	}
	if len(records) != len(want) {
		t.Fatalf("agent-sessions.sh records = %+v, want one per Copilot fixture", records)
	}
	for _, record := range records {
		if record.Repo != want[record.ID] {
			t.Fatalf("agent-sessions.sh record = %+v, want repo %q", record, want[record.ID])
		}
		delete(want, record.ID)
	}
	if len(want) != 0 {
		t.Fatalf("agent-sessions.sh omitted Copilot fixtures: %v", want)
	}
}

func TestFKFHookProjectsOnlySafeRepositoryNames(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "dirname", "jq", "sed"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "git", `case "$*" in
  *"remote get-url origin") printf '%s\n' "$FAKE_REMOTE" ;;
  *"branch --show-current") printf '%s\n' main ;;
  *) exit 2 ;;
esac
`)
	writePresetFake(t, bin, "fkf", `printf '%s\n' "$*"
`)
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{name: "credential-url", remote: "https://secret-user:secret-password@github.com/example/project.git", want: "example/project main"},
		{name: "github-scp", remote: "git@github.com:example/project.git", want: "example/project main"},
		{name: "gitlab-url", remote: "https://gitlab.com/example/project.git", want: "main"},
		{name: "gitlab-scp", remote: "git@gitlab.com:example/project.git", want: "main"},
		{name: "single-segment", remote: "single", want: "main"},
		{name: "malformed", remote: "https://leaky-user:leaky-password@github.com", want: "main"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			script := filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh")
			command := exec.CommandContext(t.Context(), script, "codex")
			command.Env = []string{
				"HOME=" + home,
				"PATH=" + bin,
				"PWD=" + t.TempDir(),
				"FAKE_REMOTE=" + testCase.remote,
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("fkf-hook.sh failed: %v; output=%q", err, output)
			}
			for _, forbidden := range []string{"secret-user", "secret-password", "leaky-user", "leaky-password"} {
				if strings.Contains(string(output), forbidden) {
					t.Fatalf("fkf-hook.sh leaked remote userinfo %q: %s", forbidden, output)
				}
			}
			parts := strings.SplitN(strings.TrimSpace(codexHookContext(t, string(output))), " -- ", 2)
			if len(parts) != 2 || parts[1] != testCase.want {
				t.Fatalf("fkf-hook.sh query = %q, want %q", output, testCase.want)
			}
		})
	}
}

type hookRunner func(t *testing.T, harness, input, pack, temporary string) (string, string, error)

func publishedHarnessRunner(home, bin, script string) hookRunner {
	return func(t *testing.T, harness, input, pack, temporary string) (string, string, error) {
		t.Helper()
		command := exec.CommandContext(t.Context(), script, harness)
		command.Dir = t.TempDir()
		command.Env = []string{
			"HOME=" + home,
			"PATH=" + bin,
			"PWD=" + command.Dir,
			"TMPDIR=" + temporary,
			"FAKE_PACK=" + pack,
		}
		command.Stdin = strings.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
}

func decodeHookEnvelope(t *testing.T, output string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode hook envelope %q: %v", output, err)
	}
	return value
}

func codexHookContext(t *testing.T, output string) string {
	t.Helper()
	envelope := decodeHookEnvelope(t, output)
	hook, ok := envelope["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("Codex hook envelope = %#v, want hookSpecificOutput", envelope)
	}
	context, ok := hook["additionalContext"].(string)
	if !ok {
		t.Fatalf("Codex hook output = %#v, want additionalContext", hook)
	}
	return context
}

func assertPublishedHookEnvelopes(t *testing.T, run hookRunner, pack string) {
	t.Helper()
	combined := "Yesterday:\n" + pack + "\n\nRepository:\n" + pack
	for _, testCase := range []struct {
		harness string
		input   string
		want    map[string]any
		plain   bool
	}{
		{harness: "claude", plain: true},
		{harness: "codex", want: map[string]any{
			"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": combined},
		}},
		{harness: "opencode", plain: true},
		{harness: "grok", plain: true},
		{harness: "kiro", plain: true},
		{harness: "copilot", want: map[string]any{}},
		{harness: "gemini", input: `{"hook_event_name":"BeforeAgent"}`, want: map[string]any{
			"hookSpecificOutput": map[string]any{"hookEventName": "BeforeAgent", "additionalContext": combined},
		}},
		{harness: "cursor", want: map[string]any{"additional_context": combined}},
		{harness: "antigravity", input: `{"invocationNum":0}`, want: map[string]any{
			"injectSteps": []any{map[string]any{"ephemeralMessage": combined}},
		}},
		{harness: "cline", input: `{"taskId":"matrix-first"}`, want: map[string]any{
			"cancel": false, "contextModification": combined,
		}},
	} {
		t.Run(testCase.harness, func(t *testing.T) {
			output, stderr, err := run(t, testCase.harness, testCase.input, pack, t.TempDir())
			if err != nil {
				t.Fatalf("fkf-hook.sh failed: %v; stderr=%q", err, stderr)
			}
			if testCase.plain {
				if output != combined+"\n" {
					t.Fatalf("plain output = %q, want %q", output, combined+"\n")
				}
				return
			}
			if got := decodeHookEnvelope(t, output); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("envelope = %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func assertEmptyHookEnvelopes(t *testing.T, run hookRunner) {
	t.Helper()
	for _, harness := range []string{"claude", "codex", "gemini", "copilot", "antigravity", "opencode", "grok", "cursor", "kiro", "cline"} {
		t.Run(harness+"-empty", func(t *testing.T) {
			input := "{}"
			if harness == "cline" {
				input = `{"taskId":"matrix-empty"}`
			}
			output, stderr, err := run(t, harness, input, "", t.TempDir())
			if err != nil {
				t.Fatalf("empty hook failed: %v; stderr=%q", err, stderr)
			}
			var want string
			switch harness {
			case "claude", "opencode", "grok", "kiro":
				want = ""
			case "cline":
				want = "{\"cancel\":false}\n"
			default:
				want = "{}\n"
			}
			if output != want {
				t.Fatalf("empty output = %q, want %q", output, want)
			}
		})
	}
}

func assertHookEventGates(t *testing.T, run hookRunner, pack string) {
	t.Helper()
	combined := "Yesterday:\n" + pack + "\n\nRepository:\n" + pack
	if output, stderr, err := run(t, "antigravity", `{"invocationNum":1}`, pack, t.TempDir()); err != nil || output != "{}\n" {
		t.Fatalf("later Antigravity call = %q, %v; stderr=%q", output, err, stderr)
	}
	clineTemporary := t.TempDir()
	output, stderr, err := run(t, "cline", `{"taskId":"matrix-repeat"}`, pack, clineTemporary)
	if err != nil || !reflect.DeepEqual(decodeHookEnvelope(t, output), map[string]any{"cancel": false, "contextModification": combined}) {
		t.Fatalf("Cline TaskStart call = %q, %v; stderr=%q", output, err, stderr)
	}
	entries, err := os.ReadDir(clineTemporary)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Cline TaskStart left temporary state: %v", entries)
	}
}

func TestFKFHookMatchesEveryPublishedHarnessEnvelope(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "jq", "sed"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "git", `case "$*" in
  *"remote get-url origin") printf '%s\n' https://github.com/example/project.git ;;
  *"branch --show-current") printf '%s\n' main ;;
  *) exit 2 ;;
esac
`)
	writePresetFake(t, bin, "fkf", `printf '%s' "${FAKE_PACK-}"
`)
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh")
	run := publishedHarnessRunner(home, bin, script)
	const pack = "trusted pack"
	assertPublishedHookEnvelopes(t, run, pack)
	assertEmptyHookEnvelopes(t, run)
	t.Run("event gates", func(t *testing.T) {
		assertHookEventGates(t, run, pack)
	})

	if output, stderr, err := run(t, "unknown", "{}", pack, t.TempDir()); err == nil || output != "" || !strings.Contains(stderr, "unknown harness unknown") {
		t.Fatalf("unknown harness = stdout %q, stderr %q, error %v", output, stderr, err)
	}
}

func TestFKFHookUsesTheSmallerClaudeCompactBudget(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "jq", "sed"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "git", `case "$*" in
  *"remote get-url origin") printf '%s\n' https://github.com/example/project.git ;;
  *"branch --show-current") printf '%s\n' main ;;
  *) exit 2 ;;
esac
`)
	writePresetFake(t, bin, "fkf", `printf '%s' "$*"
`)
	run := publishedHarnessRunner(home, bin, filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh"))
	for _, testCase := range []struct {
		name, input string
		contains    []string
		forbidden   string
	}{
		{name: "startup", input: `{"source":"startup"}`, contains: []string{"day yesterday", "--budget 600", "context", "--budget 850"}},
		{name: "compact", input: `{"source":"compact"}`, contains: []string{"context", "--budget 600"}, forbidden: "day yesterday"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			output, stderr, err := run(t, "claude", testCase.input, "", t.TempDir())
			if err != nil {
				t.Fatalf("hook failed: %v; stderr=%q", err, stderr)
			}
			for _, value := range testCase.contains {
				if !strings.Contains(output, value) {
					t.Fatalf("hook argv = %q, want %q", output, value)
				}
			}
			if testCase.forbidden != "" && strings.Contains(output, testCase.forbidden) {
				t.Fatalf("hook argv = %q, must omit %q", output, testCase.forbidden)
			}
		})
	}
}

func TestFKFHookStartupKeepsYesterdayWithoutARepository(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "jq", "sed"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "git", `exit 0
`)
	writePresetFake(t, bin, "fkf", `case "$1" in
  day) printf '%s' 'stored yesterday' ;;
  context) printf '%s' 'unexpected repository pack' ;;
esac
`)
	run := publishedHarnessRunner(home, bin, filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh"))
	output, stderr, err := run(t, "codex", `{}`, "", t.TempDir())
	if err != nil || codexHookContext(t, output) != "Yesterday:\nstored yesterday" {
		t.Fatalf("startup without repository = %q, %v; stderr=%q", output, err, stderr)
	}
}

func TestFKFHookUsesThePinnedExecutableInsteadOfPATH(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "jq", "sed"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "git", "exit 0\n")
	shadowLog := filepath.Join(t.TempDir(), "shadow.log")
	writePresetFake(t, bin, "fkf", `printf '%s\n' path >> "$SHADOW_LOG"
`)
	pinnedDirectory := t.TempDir()
	writePresetFake(t, pinnedDirectory, "candidate", `printf '%s' 'pinned pack'
`)
	pinned := filepath.Join(pinnedDirectory, "candidate")
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh"), "codex", pinned)
	command.Dir = t.TempDir()
	command.Env = []string{"HOME=" + home, "PATH=" + bin, "PWD=" + command.Dir, "SHADOW_LOG=" + shadowLog}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned hook failed: %v; output=%q", err, output)
	}
	if got := codexHookContext(t, string(output)); got != "Yesterday:\npinned pack" {
		t.Fatalf("pinned hook output = %q", got)
	}
	if shadowed, err := os.ReadFile(shadowLog); err == nil {
		t.Fatalf("hook resolved fkf through PATH: %s", shadowed)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read shadow log: %v", err)
	}
}

func TestFKFHookRejectsInheritedPATHEntriesBeforeAnyCommand(t *testing.T) {
	home := t.TempDir()
	trustedBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(trustedBin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "dirname", "jq", "sed"} {
		linkPresetTool(t, trustedBin, name)
	}
	writePresetFake(t, trustedBin, "git", `case "$*" in
  *"remote get-url origin") printf '%s\n' https://github.com/example/project.git ;;
  *"branch --show-current") printf '%s\n' main ;;
  *) exit 2 ;;
esac
`)
	writePresetFake(t, trustedBin, "fkf", `printf '%s\n' 'trusted pack'
`)
	realDirname, err := exec.LookPath("dirname")
	if err != nil {
		t.Fatalf("find dirname for hook test: %v", err)
	}

	for _, testCase := range []struct {
		name string
		kind string
	}{
		{name: "empty", kind: "empty"},
		{name: "dot", kind: "dot"},
		{name: "relative-directory", kind: "relative"},
		{name: "absolute-workspace", kind: "absolute"},
		{name: "symlink-to-workspace", kind: "symlink"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			work := t.TempDir()
			shadowDir := work
			pathPrefix := work
			switch testCase.kind {
			case "empty":
				pathPrefix = ""
			case "dot":
				pathPrefix = "."
			case "relative":
				pathPrefix = "relative-bin"
				shadowDir = filepath.Join(work, pathPrefix)
				if err := os.Mkdir(shadowDir, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				pathPrefix = filepath.Join(t.TempDir(), "workspace-link")
				if err := os.Symlink(work, pathPrefix); err != nil {
					t.Fatal(err)
				}
			}
			shadowLog := filepath.Join(t.TempDir(), "shadow.log")
			writePresetFake(t, shadowDir, "dirname", `printf '%s\n' dirname >> "$SHADOW_LOG"
exec "$REAL_DIRNAME" "$@"
`)
			writePresetFake(t, shadowDir, "git", `printf '%s\n' git >> "$SHADOW_LOG"
case "$*" in
  *"remote get-url origin") printf '%s\n' https://github.com/shadow/project.git ;;
  *"branch --show-current") printf '%s\n' shadow ;;
esac
`)
			writePresetFake(t, shadowDir, "fkf", `printf '%s\n' fkf >> "$SHADOW_LOG"
printf '%s\n' 'shadow pack'
`)

			script := filepath.Join(repositoryRoot(t), "presets", "bin", "fkf-hook.sh")
			command := exec.CommandContext(t.Context(), script, "codex")
			command.Dir = work
			command.Env = []string{
				"HOME=" + home,
				"PATH=" + pathPrefix + ":" + trustedBin,
				"PWD=" + work,
				"REAL_DIRNAME=" + realDirname,
				"SHADOW_LOG=" + shadowLog,
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("fkf-hook.sh failed: %v; output=%q", err, output)
			}
			want := "Yesterday:\ntrusted pack\n\nRepository:\ntrusted pack"
			if codexHookContext(t, string(output)) != want {
				t.Fatalf("fkf-hook.sh output = %q, want %q", output, want)
			}
			if shadowed, err := os.ReadFile(shadowLog); err == nil {
				t.Fatalf("fkf-hook.sh executed commands through an inherited PATH entry: %s", shadowed)
			} else if !os.IsNotExist(err) {
				t.Fatalf("read shadow command log: %v", err)
			}
		})
	}
}

// TestAgentSessionsRequiresRecordedActivityInsideTheExactDay prevents a session envelope from
// becoming invented daily activity. The May 4 sessions record another event on May 6 but have
// no evidence for May 5. Fractional timestamps immediately inside and outside May 6 exercise
// parsed RFC3339 comparisons; lexical comparison gets both boundaries wrong. The Claude file's
// mtime equals the inclusive start so candidate discovery cannot use a strict lower bound.
func TestAgentSessionsRequiresRecordedActivityInsideTheExactDay(t *testing.T) {
	home := t.TempDir()
	may4 := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	may6Start := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	may6 := time.Date(2026, 5, 6, 9, 30, 0, 0, time.UTC)
	claudeDirectory := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(claudeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeAtStart := filepath.Join(claudeDirectory, "start.jsonl")
	if err := os.WriteFile(claudeAtStart, []byte(
		`{"type":"user","timestamp":"2026-05-06T00:00:00.001Z","sessionId":"start","cwd":null}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(claudeAtStart, may6Start, may6Start); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDirectory, "after-end.jsonl"), []byte(
		`{"type":"user","timestamp":"2026-05-07T00:00:00.001Z","sessionId":"after-end","cwd":null}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subagent := filepath.Join(claudeDirectory, "start", "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(subagent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subagent, []byte(
		`{"type":"user","timestamp":"2026-05-06T00:00:00.005Z","sessionId":"child","cwd":null}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	codex := filepath.Join(home, ".codex", "sessions", "2026", "05", "04", "rollout-gap.jsonl")
	if err := os.MkdirAll(filepath.Dir(codex), 0o700); err != nil {
		t.Fatal(err)
	}
	codexTranscript := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"gap","timestamp":"2026-05-04T08:00:00Z","cwd":null,"parent_thread_id":null}}`,
		`{"type":"event_msg","timestamp":"2026-05-04T08:05:00Z","payload":{"type":"metadata-only-fixture"}}`,
		`{"type":"event_msg","timestamp":"2026-05-06T00:00:00.002Z","payload":{"type":"metadata-only-fixture"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(codex, []byte(codexTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	geminiDirectory := filepath.Join(home, ".gemini", "tmp", "project", "chats")
	if err := os.MkdirAll(geminiDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	geminiTranscript := strings.Join([]string{
		`{"sessionId":"gap","startTime":"2026-05-04T10:00:00Z","lastUpdated":"2026-05-06T11:00:00Z","kind":"main"}`,
		`{"id":"one","timestamp":"2026-05-04T10:05:00Z","type":"user","content":"not inspected"}`,
		`{"$set":{"lastUpdated":"2026-05-06T11:00:00Z"}}`,
		`{"id":"two","timestamp":"2026-05-06T00:00:00.003Z","type":"gemini","content":"not inspected"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(geminiDirectory, "session-gap.jsonl"), []byte(geminiTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	geminiDocument := `{
  "sessionId": "gap-json",
  "startTime": "2026-05-04T12:00:00Z",
  "lastUpdated": "2026-05-06T12:30:00Z",
  "kind": "main",
  "messages": [
    {"id":"one","timestamp":"2026-05-04T12:05:00Z","type":"user","content":"not inspected"},
	    {"id":"two","timestamp":"2026-05-06T00:00:00.004Z","type":"gemini","content":"not inspected"}
  ]
}
`
	if err := os.WriteFile(filepath.Join(geminiDirectory, "session-gap-json.json"), []byte(geminiDocument), 0o600); err != nil {
		t.Fatal(err)
	}

	createDatabase := func(path, schema string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "sqlite3", path, schema)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create synthetic database %s: %v; output=%q", path, err, output)
		}
	}
	createDatabase(filepath.Join(home, ".local", "share", "opencode", "opencode.db"), fmt.Sprintf(`
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, title TEXT, directory TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
INSERT INTO session VALUES ('gap', NULL, 'OpenCode gap', NULL, %d, %d);
INSERT INTO message VALUES ('message', 'gap', %d, %d);
`, may4.UnixMilli(), may6.UnixMilli(), may6.UnixMilli(), may6.UnixMilli()))
	createDatabase(filepath.Join(home, ".copilot", "session-store.db"), `
CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, branch TEXT, summary TEXT,
  created_at TEXT, updated_at TEXT);
CREATE TABLE turns (session_id TEXT NOT NULL, timestamp TEXT);
INSERT INTO sessions VALUES ('gap', NULL, NULL, NULL, 'Copilot gap',
  '2026-05-04T08:30:00Z', '2026-05-06T10:00:00Z');
INSERT INTO turns VALUES ('gap', '2026-05-06T10:30:00Z');
`)

	bin := t.TempDir()
	for _, name := range []string{"cat", "dirname", "head", "jq", "mktemp", "rm", "sed", "sqlite3", "tail", "touch"} {
		linkPresetTool(t, bin, name)
	}
	// Candidate discovery is a deterministic fake: the collector's timestamp filtering is
	// exercised by the fixture contents, not by whichever find implementation hosts the test.
	writePresetFake(t, bin, "find", `case "$1" in
  "$TEST_HOME/.claude/projects")
    printf '%s\n' "$TEST_HOME/.claude/projects/project/start.jsonl"
    printf '%s\n' "$TEST_HOME/.claude/projects/project/after-end.jsonl"
    printf '%s\n' "$TEST_HOME/.claude/projects/project/start/subagents/agent-child.jsonl"
    ;;
  "$TEST_HOME/.codex/sessions")
    printf '%s\n' "$TEST_HOME/.codex/sessions/2026/05/04/rollout-gap.jsonl"
    ;;
  "$TEST_HOME/.gemini/tmp")
    printf '%s\n' "$TEST_HOME/.gemini/tmp/project/chats/session-gap.jsonl"
    printf '%s\n' "$TEST_HOME/.gemini/tmp/project/chats/session-gap-json.json"
    ;;
esac
`)

	assertDay := func(start, end string) []struct {
		ID   string `json:"id"`
		Time string `json:"time"`
	} {
		t.Helper()
		stdout, stderr, err := runPresetScript(t, "agent-sessions.sh", home, bin,
			[]string{"TEST_HOME=" + home}, start, end)
		if err != nil {
			t.Fatalf("agent-sessions.sh %s failed: %v; stderr=%q", start, err, stderr)
		}
		var records []struct {
			ID   string `json:"id"`
			Time string `json:"time"`
		}
		if err := json.Unmarshal([]byte(stdout), &records); err != nil {
			t.Fatalf("decode agent-sessions.sh %s: %v; stdout=%q", start, err, stdout)
		}
		return records
	}

	if records := assertDay("2026-05-05T00:00:00Z", "2026-05-06T00:00:00Z"); len(records) != 0 {
		t.Fatalf("idle middle day = %+v, want no invented session activity", records)
	}
	records := assertDay("2026-05-06T00:00:00Z", "2026-05-07T00:00:00Z")
	want := map[string]string{
		"claude:start":    "2026-05-06T00:00:00.001Z",
		"codex:gap":       "2026-05-06T00:00:00.002Z",
		"copilot:gap":     "2026-05-06T10:30:00Z",
		"gemini:gap":      "2026-05-06T00:00:00.003Z",
		"gemini:gap-json": "2026-05-06T00:00:00.004Z",
		"opencode:gap":    "2026-05-06T09:30:00Z",
	}
	if len(records) != len(want) {
		t.Fatalf("May 6 records = %+v, want one exact activity record per harness", records)
	}
	for _, record := range records {
		if want[record.ID] != record.Time {
			t.Fatalf("May 6 record = %+v, want recorded activity time %q", record, want[record.ID])
		}
		delete(want, record.ID)
	}
	if len(want) != 0 {
		t.Fatalf("May 6 output omitted exact-day sessions: %v", want)
	}
}

func TestAgentMemoryFilesFailsWithoutOutputWhenFindFails(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex", "memories"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"jq", "mktemp", "rm"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", "exit 39\n")

	stdout, stderr, err := runPresetScript(t, "agent-memory-files.sh", home, bin, nil,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	requirePresetFailureWithoutOutput(t, "agent-memory-files.sh", stdout, stderr, err)
}

func TestAgentMemoryFilesFailsWhenStatCannotReadACandidate(t *testing.T) {
	home := t.TempDir()
	memory := filepath.Join(home, ".codex", "memories")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"awk", "jq", "mktemp", "rm"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", `printf '%s\n' "$TEST_FILE"`)
	writePresetFake(t, bin, "stat", "exit 41\n")

	stdout, stderr, err := runPresetScript(t, "agent-memory-files.sh", home, bin,
		[]string{"TEST_FILE=" + filepath.Join(memory, "unreadable.md")},
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	requirePresetFailureWithoutOutput(t, "agent-memory-files.sh", stdout, stderr, err)
	if !strings.Contains(stderr, "stat could not read") {
		t.Fatalf("agent-memory-files.sh error = %q, want a clear stat failure", stderr)
	}
}

func TestAgentMemoryFilesUsesHalfOpenWindowAndBoundedTitlePreview(t *testing.T) {
	home := t.TempDir()
	memory := filepath.Join(home, ".codex", "memories")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	files := []struct {
		name     string
		body     string
		modified time.Time
	}{
		{name: "start.md", body: "# At start\n", modified: start},
		{name: "bounded.md", body: strings.Repeat("plain metadata line\n", 4000) + "# Too late\n", modified: start.Add(time.Hour)},
		{name: "end.md", body: "# At end\n", modified: end},
	}
	for _, file := range files {
		path := filepath.Join(memory, file.name)
		if err := os.WriteFile(path, []byte(file.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, file.modified, file.modified); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	for _, name := range []string{"awk", "basename", "dd", "jq", "mktemp", "rm"} {
		linkPresetTool(t, bin, name)
	}
	// Fake POSIX find and BSD stat so the same regression proves the macOS path on Linux.
	writePresetFake(t, bin, "find", `printf '%s\n' "$START_FILE" "$BOUNDED_FILE" "$END_FILE"
`)
	writePresetFake(t, bin, "stat", `case "$1" in
  -c) exit 1 ;;
  -f)
    case "$3" in
      "$START_FILE") printf '%s\n' "$START_EPOCH" ;;
      "$BOUNDED_FILE") printf '%s\n' "$BOUNDED_EPOCH" ;;
      "$END_FILE") printf '%s\n' "$END_EPOCH" ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)

	stdout, stderr, err := runPresetScript(t, "agent-memory-files.sh", home, bin, []string{
		"START_FILE=" + filepath.Join(memory, "start.md"),
		"START_EPOCH=" + strconv.FormatInt(start.Unix(), 10),
		"BOUNDED_FILE=" + filepath.Join(memory, "bounded.md"),
		"BOUNDED_EPOCH=" + strconv.FormatInt(start.Add(time.Hour).Unix(), 10),
		"END_FILE=" + filepath.Join(memory, "end.md"),
		"END_EPOCH=" + strconv.FormatInt(end.Unix(), 10),
	},
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("agent-memory-files.sh failed: %v; stderr=%q", err, stderr)
	}
	var records []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("decode agent-memory-files.sh output: %v; stdout=%q", err, stdout)
	}
	want := map[string]string{
		filepath.Join(memory, "start.md"):   "At start",
		filepath.Join(memory, "bounded.md"): "bounded",
	}
	if len(records) != len(want) {
		t.Fatalf("agent-memory-files.sh records = %+v, want start-inclusive/end-exclusive records", records)
	}
	for _, record := range records {
		if want[record.ID] != record.Title {
			t.Fatalf("agent-memory-files.sh record = %+v, want title %q", record, want[record.ID])
		}
		delete(want, record.ID)
	}
	if len(want) != 0 {
		t.Fatalf("agent-memory-files.sh missing records: %v", want)
	}
}

func TestGitLogJSONFailsWithoutOutputWhenFindFails(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	bin := t.TempDir()
	for _, name := range []string{"jq", "mktemp", "rm", "sort"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", "exit 40\n")

	stdout, stderr, err := runPresetScript(t, "git-log-json.sh", home, bin, nil,
		"2026-05-04", "2026-05-05", root, "author@example.test")
	requirePresetFailureWithoutOutput(t, "git-log-json.sh", stdout, stderr, err)
}

func TestGitLogJSONFailsWithoutOutputWhenGitLogFails(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"basename", "dirname", "jq", "mktemp", "rm", "sed", "sort"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "find", `printf '%s\n' "$TEST_ROOT/repo/.git"
`)
	writePresetFake(t, bin, "git", `case "$*" in
  *"rev-parse --path-format=absolute --git-common-dir"*)
    printf '%s\n' "$TEST_ROOT/repo/.git"
    ;;
  *"config --get remote.origin.url"*)
    printf '%s\n' 'https://github.com/example/repo.git'
    ;;
  *" log --all "*)
    printf 'abc\0372026-05-04T10:00:00Z\037author@example.test\037partial\036'
    exit 41
    ;;
  *) exit 42 ;;
esac
`)

	stdout, stderr, err := runPresetScript(t, "git-log-json.sh", home, bin,
		[]string{"TEST_ROOT=" + root},
		"2026-05-04", "2026-05-05", root, "author@example.test")
	requirePresetFailureWithoutOutput(t, "git-log-json.sh", stdout, stderr, err)
}
