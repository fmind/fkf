package checks_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runJiraHelper(t *testing.T, payload string, exitCode int) ([]byte, string, error) {
	t.Helper()
	fakeBin := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "jira.json")
	if err := os.WriteFile(fixture, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$ACLI_CALL_LOG"
cat "$ACLI_FIXTURE"
exit "${ACLI_EXIT_CODE:-0}"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "acli"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(t.TempDir(), "call")
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "jira-issues-json.sh"),
		"team.atlassian.net", "TEAM", "statusCategory != Done")
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ACLI_FIXTURE="+fixture, "ACLI_CALL_LOG="+callLog,
		"ACLI_EXIT_CODE="+string(rune('0'+exitCode)))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if log, readErr := os.ReadFile(callLog); readErr == nil && exitCode == 0 {
		for _, want := range []string{
			`project = "TEAM" AND (statusCategory != Done) ORDER BY key ASC`,
			"--fields key,summary,status,assignee,url", "--limit 10001", "--json",
		} {
			if !strings.Contains(string(log), want) {
				t.Errorf("acli call omits %q: %s", want, log)
			}
		}
	}
	return stdout.Bytes(), stderr.String(), err
}

func jiraIssue(key string) map[string]any {
	return map[string]any{"key": key, "fields": map[string]any{
		"summary": "Shared issue", "status": map[string]any{"name": "In Progress"},
		"assignee": map[string]any{"displayName": "Example Owner"},
		"url":      "https://team.atlassian.net/browse/" + key,
	}}
}

func TestJiraIssuesJSONAcceptsBoundedAggregatedPagesAndEmptyProjects(t *testing.T) {
	for _, issues := range [][]map[string]any{{jiraIssue("TEAM-1"), jiraIssue("TEAM-2")}, {}} {
		payload, err := json.Marshal(map[string]any{"issues": issues})
		if err != nil {
			t.Fatal(err)
		}
		output, stderr, runErr := runJiraHelper(t, string(payload), 0)
		if runErr != nil {
			t.Fatalf("helper error = %v\n%s", runErr, stderr)
		}
		var records []map[string]any
		if err := json.Unmarshal(output, &records); err != nil {
			t.Fatalf("decode: %v\n%s", err, output)
		}
		if len(records) != len(issues) {
			t.Fatalf("records = %d, want %d", len(records), len(issues))
		}
	}
}

func TestJiraIssuesJSONFailsBeforeOutputOnProviderAndBoundaryErrors(t *testing.T) {
	cases := map[string]struct {
		payload string
		exit    int
	}{
		"access denied":   {`{"error":"denied"}`, 1},
		"malformed":       {`{"issues":"wrong"}`, 0},
		"out of scope":    {`[{"key":"OTHER-1","summary":"wrong","url":"https://team.atlassian.net/browse/OTHER-1"}]`, 0},
		"repeated cursor": {`{"issues":[],"nextPageToken":"same"}`, 0},
		"duplicate":       {`[{"key":"TEAM-1","summary":"one","url":"https://team.atlassian.net/browse/TEAM-1"},{"key":"TEAM-1","summary":"two","url":"https://team.atlassian.net/browse/TEAM-1"}]`, 0},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			output, _, err := runJiraHelper(t, test.payload, test.exit)
			if err == nil {
				t.Fatal("helper accepted unsafe or incomplete results")
			}
			if len(output) != 0 {
				t.Fatalf("helper emitted partial output: %s", output)
			}
		})
	}
}

func TestJiraIssuesJSONRejectsCompletenessCeiling(t *testing.T) {
	issues := make([]map[string]any, 10001)
	for index := range issues {
		issues[index] = jiraIssue("TEAM-" + string(rune('1'+index%9)))
	}
	payload, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	output, _, runErr := runJiraHelper(t, string(payload), 0)
	if runErr == nil {
		t.Fatal("helper accepted the limit-plus-one sentinel")
	}
	if len(output) != 0 {
		t.Fatalf("helper emitted partial output: %s", output)
	}
}
