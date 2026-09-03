package checks_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type gitLogRecord struct {
	Hash     string `json:"hash"`
	Message  string `json:"message"`
	RepoFull string `json:"repo_full"`
	Time     string `json:"time"`
	UID      string `json:"uid"`
}

func syntheticGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func commitSyntheticFile(t *testing.T, env []string, repo, name, contents, when string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	syntheticGit(t, env, repo, "add", name)
	commitEnv := append(append([]string{}, env...), "GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when)
	syntheticGit(t, commitEnv, repo, "commit", "-m", strings.TrimSuffix(contents, "\n"))
	return syntheticGit(t, env, repo, "rev-parse", "HEAD")
}

func newSyntheticRepository(t *testing.T, env []string, path, remote string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	syntheticGit(t, env, path, "init", "--initial-branch=main")
	syntheticGit(t, env, path, "config", "user.name", "Test Author")
	syntheticGit(t, env, path, "config", "user.email", "author@example.test")
	syntheticGit(t, env, path, "remote", "add", "origin", remote)
	return commitSyntheticFile(t, env, path, "main.txt", "main", "2026-08-23T10:00:00Z")
}

func runGitLogJSON(t *testing.T, env []string, roots string) []gitLogRecord {
	t.Helper()
	return runGitLogJSONWindow(t, env, "2026-08-23", "2026-08-25", roots)
}

func runGitLogJSONWindow(t *testing.T, env []string, since, until, roots string) []gitLogRecord {
	t.Helper()
	return runGitLogJSONWindowForAuthors(t, env, since, until, roots, "author@example.test")
}

func runGitLogJSONWindowForAuthors(t *testing.T, env []string, since, until, roots string, authors ...string) []gitLogRecord {
	t.Helper()
	output := runGitLogJSONWindowForAuthorsRaw(t, env, since, until, roots, authors...)
	var records []gitLogRecord
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode git-log-json.sh output: %v\n%s", err, output)
	}
	return records
}

func runGitLogJSONWindowForAuthorsRaw(t *testing.T, env []string, since, until, roots string, authors ...string) []byte {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "git-log-json.sh")
	arguments := append([]string{since, until, roots}, authors...)
	command := exec.CommandContext(t.Context(), script, arguments...)
	command.Dir = string(filepath.Separator)
	command.Env = append(os.Environ(), env...)
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("git-log-json.sh: %v\n%s", err, exitError.Stderr)
		}
		t.Fatalf("git-log-json.sh: %v", err)
	}
	return output
}

func TestGitLogJSONProjectsOnlySafeTwoSegmentRepositoryNames(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	cases := []struct {
		name   string
		remote string
		want   string
	}{
		{name: "credential-url", remote: "https://secret-user:secret-password@github.com/example/credential.git", want: "example/credential"},
		{name: "github-scp", remote: "git@github.com:example/scp.git", want: "example/scp"},
		{name: "gitlab-url", remote: "https://gitlab.com/example/project.git", want: ""},
		{name: "gitlab-scp", remote: "git@gitlab.com:example/project.git", want: ""},
		{name: "single-segment", remote: "single", want: ""},
		{name: "missing-path", remote: "https://leaky-user:leaky-password@github.com", want: ""},
		{name: "extra-segment", remote: "https://github.com/example/project/extra.git", want: ""},
	}
	wantByHash := make(map[string]string, len(cases))
	for _, testCase := range cases {
		repo := filepath.Join(root, testCase.name)
		newSyntheticRepository(t, env, repo, testCase.remote)
		hash := commitSyntheticFile(t, env, repo, testCase.name+".txt", testCase.name,
			"2026-08-24T10:00:00Z")
		wantByHash[hash] = testCase.want
	}

	output := runGitLogJSONWindowForAuthorsRaw(t, env,
		"2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z", root, "author@example.test")
	for _, forbidden := range []string{"secret-user", "secret-password", "leaky-user", "leaky-password"} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("git-log-json.sh leaked remote userinfo %q to stdout: %s", forbidden, output)
		}
	}
	var records []gitLogRecord
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode git-log-json.sh output: %v\n%s", err, output)
	}
	if len(records) != len(wantByHash) {
		t.Fatalf("git-log-json.sh records = %+v, want one per synthetic repository", records)
	}
	for _, record := range records {
		want, ok := wantByHash[record.Hash]
		if !ok {
			t.Fatalf("unexpected git-log-json.sh record %+v", record)
		}
		if record.RepoFull != want {
			t.Fatalf("repo_full for %q = %q, want %q", record.Message, record.RepoFull, want)
		}
		delete(wantByHash, record.Hash)
	}
	if len(wantByHash) != 0 {
		t.Fatalf("git-log-json.sh omitted repositories: %v", wantByHash)
	}
}

func TestGitLogJSONTreatsAuthorIdentitiesAsLiteralStrings(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	repo := filepath.Join(root, "project")
	newSyntheticRepository(t, env, repo, "https://github.com/example/project.git")
	syntheticGit(t, env, repo, "config", "user.email", "me.one@example.com")
	wanted := commitSyntheticFile(t, env, repo, "literal.txt", "literal", "2026-08-24T10:00:00Z")
	syntheticGit(t, env, repo, "config", "user.email", "meXone@exampleYcom")
	commitSyntheticFile(t, env, repo, "regex-match.txt", "regex match", "2026-08-24T11:00:00Z")

	records := runGitLogJSONWindowForAuthors(t, env,
		"2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z", root, "me.one@example.com")
	assertGitRecords(t, records, wanted)
}

func TestGitLogJSONRetainsSeparatorBytesInARealCommitSubject(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	repo := filepath.Join(root, "project")
	newSyntheticRepository(t, env, repo, "https://github.com/example/project.git")
	const subject = "retain record\x1e separator and field\x1f separator"
	hash := commitSyntheticFile(t, env, repo, "controls.txt", subject, "2026-08-24T10:00:00Z")

	records := runGitLogJSONWindowForAuthors(t, env,
		"2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z", root, "author@example.test")
	if len(records) != 1 || records[0].Hash != hash || records[0].Message != subject {
		t.Fatalf("records = %+v, want one lossless subject %q for %s", records, subject, hash)
	}
}

func TestGitLogJSONUsesAHalfOpenWindow(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	repo := filepath.Join(root, "project")
	inWindow := newSyntheticRepository(t, env, repo, "https://github.com/example/project.git")
	commitSyntheticFile(t, env, repo, "boundary.txt", "exclusive end", "2026-08-25T00:00:00Z")

	records := runGitLogJSONWindow(t, env,
		"2026-08-23T00:00:00Z", "2026-08-25T00:00:00Z", root)
	assertGitRecords(t, records, inWindow)
}

func TestGitLogJSONAllowsARepositoryWithNoMatchingCommit(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	matching := filepath.Join(root, "matching")
	newSyntheticRepository(t, env, matching, "https://github.com/example/project.git")
	wanted := commitSyntheticFile(t, env, matching, "inside.txt", "inside", "2026-08-24T10:00:00Z")
	newSyntheticRepository(t, env, filepath.Join(root, "empty-window"), "https://github.com/example/old.git")

	records := runGitLogJSONWindow(t, env,
		"2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z", root)
	assertGitRecords(t, records, wanted)
}

func assertGitRecords(t *testing.T, records []gitLogRecord, hashes ...string) {
	t.Helper()
	const repo = "example/project"
	if len(records) != len(hashes) {
		t.Fatalf("records = %+v, want one record for each of %v", records, hashes)
	}
	want := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		want[hash] = true
	}
	for _, record := range records {
		if !want[record.Hash] {
			t.Fatalf("unexpected record %+v; want hashes %v", record, hashes)
		}
		if record.RepoFull != repo || record.UID != repo+"@"+record.Hash {
			t.Fatalf("record identity = %+v, want repo@hash identity for %s", record, repo)
		}
		delete(want, record.Hash)
	}
	if len(want) != 0 {
		t.Fatalf("missing hashes %v from records %+v", want, records)
	}
}

func TestGitLogJSONIncludesCommitsReachableFromEveryRef(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	repo := filepath.Join(root, "project")
	mainHash := newSyntheticRepository(t, env, repo, "https://github.com/example/project.git")
	syntheticGit(t, env, repo, "checkout", "-b", "unmerged")
	unmergedHash := commitSyntheticFile(t, env, repo, "branch.txt", "unmerged", "2026-08-24T10:00:00Z")
	syntheticGit(t, env, repo, "checkout", "main")

	records := runGitLogJSON(t, env, root)
	assertGitRecords(t, records, mainHash, unmergedHash)
}

func TestGitLogJSONDiscoversLinkedWorktreesOnce(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := t.TempDir()
	// Spaces exercise the path boundary while the .git file remains data handled by git.
	mainRoot := filepath.Join(root, "main clones")
	linkedRoot := filepath.Join(root, "linked clones")
	repo := filepath.Join(mainRoot, "project")
	linked := filepath.Join(linkedRoot, "project-topic")
	mainHash := newSyntheticRepository(t, env, repo, "git@github.com:example/project.git")
	if err := os.MkdirAll(linkedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	syntheticGit(t, env, repo, "worktree", "add", "-b", "topic", linked)
	linkedHash := commitSyntheticFile(t, env, linked, "topic.txt", "topic", "2026-08-24T11:00:00Z")

	// A scan rooted only at the linked checkout proves that a .git file is discovered.
	assertGitRecords(t, runGitLogJSON(t, env, linkedRoot), mainHash, linkedHash)
	// Scanning both checkout roots must not emit each reachable commit twice.
	assertGitRecords(t, runGitLogJSON(t, env, mainRoot+":"+linkedRoot), mainHash, linkedHash)
}

func TestGitLogJSONEmitsTheSameCommitterClockItFilters(t *testing.T) {
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	root := filepath.Join(t.TempDir(), "clones")
	repo := filepath.Join(root, "project")
	newSyntheticRepository(t, env, repo, "https://github.com/example/project.git")
	if err := os.WriteFile(filepath.Join(repo, "rebased.txt"), []byte("rebased"), 0o600); err != nil {
		t.Fatal(err)
	}
	syntheticGit(t, env, repo, "add", "rebased.txt")
	commitEnv := append(append([]string{}, env...),
		"GIT_AUTHOR_DATE=2026-08-20T10:00:00Z",
		"GIT_COMMITTER_DATE=2026-08-24T12:00:00-07:00",
	)
	syntheticGit(t, commitEnv, repo, "commit", "-m", "rebased commit")
	hash := syntheticGit(t, env, repo, "rev-parse", "HEAD")

	records := runGitLogJSON(t, env, root)
	var found *gitLogRecord
	for index := range records {
		if records[index].Hash == hash {
			found = &records[index]
			break
		}
	}
	if found == nil {
		t.Fatalf("rebased commit %s missing from %+v", hash, records)
	}
	if found.Time != "2026-08-24T19:00:00Z" {
		t.Fatalf("record time = %q, want the in-window committer time", found.Time)
	}
}
