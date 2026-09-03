package checks_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentMemoryBodyReadsOnlyRegularFilesBelowReviewedRoots(t *testing.T) {
	home := t.TempDir()
	memory := filepath.Join(home, ".codex", "memories", "rollout_summaries")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(memory, "session.md")
	if err := os.WriteFile(allowed, []byte("reviewed memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "agent-memory-body.sh")
	run := func(path string) ([]byte, error) {
		command := exec.CommandContext(t.Context(), "/bin/sh", script, path)
		command.Env = append(os.Environ(), "HOME="+home)
		return command.CombinedOutput()
	}
	output, err := run(allowed)
	if err != nil || string(output) != "reviewed memory\n" {
		t.Fatalf("allowed memory body = %q, %v", output, err)
	}
	grokMemory := filepath.Join(home, ".grok", "memory", "project")
	if err := os.MkdirAll(grokMemory, 0o700); err != nil {
		t.Fatal(err)
	}
	grok := filepath.Join(grokMemory, "session.md")
	if err := os.WriteFile(grok, []byte("reviewed Grok memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = run(grok)
	if err != nil || string(output) != "reviewed Grok memory\n" {
		t.Fatalf("allowed Grok memory body = %q, %v", output, err)
	}

	outside := filepath.Join(home, "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(memory, "linked.md")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{outside, linked} {
		output, err := run(refused)
		if err == nil || !strings.Contains(string(output), "outside the reviewed") &&
			!strings.Contains(string(output), "absent, linked") {
			t.Fatalf("memory body %q = %q, %v; want confinement refusal", refused, output, err)
		}
	}
}

func TestAgentMemoryBodyRefusesOversizedFilesBeforePrinting(t *testing.T) {
	home := t.TempDir()
	memory := filepath.Join(home, ".codex", "memories")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(memory, "oversized.md")
	file, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4<<20 + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "agent-memory-body.sh"), oversized)
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "exceeds the 4194304-byte body limit") {
		t.Fatalf("oversized memory body = %q, %v; want size refusal", output, err)
	}
}

func TestGitHubCommitHelperNormalizesNoreplyAuthorsToActors(t *testing.T) {
	bin := t.TempDir()
	fakeGH := `#!/bin/sh
cat <<'JSON'
[{"sha":"abc","url":"https://github.com/fmind/fkf/commit/abc","repository":{"fullName":"fmind/fkf"},"commit":{"author":{"date":"2026-05-04T09:00:00Z","email":"12345+Fmind@users.noreply.github.com"},"message":"Subject"}}]
JSON
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-commits-json.sh"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-commits-json.sh error = %v\n%s", err, output)
	}
	var records []struct {
		Participants []string `json:"participant_uris"`
	}
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode GitHub commits: %v\n%s", err, output)
	}
	if len(records) != 1 || len(records[0].Participants) != 1 ||
		records[0].Participants[0] != "actor:github.com/fmind" {
		t.Fatalf("GitHub commit participants = %+v", records)
	}
}
