package services_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const githubReviewsHelper = "github-reviews-json.sh"

func githubReviewContribution(
	nodeID string,
	databaseID int,
	submittedAt, repository string,
	pullRequestNumber int,
) map[string]any {
	pullRequestURL := "https://github.example.test/" + repository + "/pull/" + strconv.Itoa(pullRequestNumber)
	return map[string]any{
		"occurredAt": submittedAt,
		"pullRequestReview": map[string]any{
			"id": nodeID, "fullDatabaseId": strconv.Itoa(databaseID), "submittedAt": submittedAt,
			"state": "APPROVED", "url": pullRequestURL + "#pullrequestreview-" + nodeID,
		},
		"pullRequest": map[string]any{
			"number": pullRequestNumber, "title": "Review exact collection windows",
			"url": pullRequestURL,
		},
		"repository": map[string]any{"nameWithOwner": repository},
		"user":       map[string]any{"login": "reviewer"},
	}
}

func githubReviewPage(nodes []map[string]any, total int, hasNext bool, cursor any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"viewer": map[string]any{
				"contributionsCollection": map[string]any{
					"pullRequestReviewContributions": map[string]any{
						"nodes": nodes, "totalCount": total,
						"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
					},
				},
			},
		},
	}
}

func githubReviewsCommand(t *testing.T, fakeBin, fixtures, calls string, args ...string) *exec.Cmd {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "presets", "bin", githubReviewsHelper)
	command := exec.CommandContext(t.Context(), script, args...)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_FIXTURE_DIR="+fixtures,
		"GH_CALL_LOG="+calls,
	)
	return command
}

func TestGitHubReviewsJSONPaginatesActualReviewContributionsWithinTheHalfOpenWindow(t *testing.T) {
	fixtures := t.TempDir()
	writeJSONFixture(t, filepath.Join(fixtures, "first.json"), githubReviewPage([]map[string]any{
		githubReviewContribution("before", 100, "2026-05-03T23:59:59Z", "acme/widgets", 42),
		githubReviewContribution("at-start", 101, "2026-05-04T00:00:00Z", "acme/widgets", 42),
		githubReviewContribution("at-end", 102, "2026-05-05T00:00:00Z", "acme/widgets", 42),
	}, 4, true, "cursor-1"))
	writeJSONFixture(t, filepath.Join(fixtures, "second.json"), githubReviewPage([]map[string]any{
		githubReviewContribution("inside", 103, "2026-05-04T12:34:56Z", "acme/other", 7),
	}, 4, false, nil))

	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *cursor=cursor-1*) file=second.json ;;
  *) file=first.json ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubReviewsCommand(t, fakeBin, fixtures, calls,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-reviews-json.sh error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode github-reviews-json.sh output: %v\n%s", err, output)
	}
	if len(records) != 2 {
		t.Fatalf("review records = %s, want the lower bound and inside record only", output)
	}
	wantIDs := []string{
		"repos/acme/widgets/pulls/42/reviews/101",
		"repos/acme/other/pulls/7/reviews/103",
	}
	gotIDs := make([]string, len(records))
	for index, record := range records {
		id, ok := record["id"].(string)
		if !ok {
			t.Fatalf("review record %d id = %#v, want a string", index, record["id"])
		}
		gotIDs[index] = id
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("review ids = %v, want stable REST API locators %v", gotIDs, wantIDs)
	}
	for _, record := range records {
		for _, field := range []string{"reviewId", "occurredAt", "submittedAt", "state", "url", "title", "pullRequest", "repo", "reviewer"} {
			if _, exists := record[field]; !exists {
				t.Errorf("review record omits metadata field %q: %#v", field, record)
			}
		}
		if _, exists := record["body"]; exists {
			t.Errorf("review collector stored a body: %#v", record)
		}
	}

	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"api graphql", "from=2026-05-04T00:00:00Z", "to=2026-05-05T00:00:00Z", "cursor=cursor-1",
	} {
		if !strings.Contains(string(log), expected) {
			t.Errorf("gh calls omit %q:\n%s", expected, log)
		}
	}
	if got := strings.Count(string(log), "api graphql"); got != 2 {
		t.Fatalf("gh GraphQL calls = %d, want both pages:\n%s", got, log)
	}
}

func TestGitHubReviewsJSONFailsWithoutPartialOutputWhenPaginationIsIncomplete(t *testing.T) {
	fixtures := t.TempDir()
	writeJSONFixture(t, filepath.Join(fixtures, "first.json"), githubReviewPage([]map[string]any{
		githubReviewContribution("first", 201, "2026-05-04T01:00:00Z", "acme/widgets", 42),
	}, 2, true, "cursor-1"))
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
case "$*" in
  *cursor=cursor-1*) exit 42 ;;
  *) cat "$GH_FIXTURE_DIR/first.json" ;;
esac
`)
	command := githubReviewsCommand(t, fakeBin, fixtures, filepath.Join(t.TempDir(), "calls"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-reviews-json.sh accepted a failed second page")
	}
	if stdout.Len() != 0 {
		t.Fatalf("github-reviews-json.sh emitted a partial first page: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "GraphQL page") {
		t.Fatalf("pagination failure = %q, want an actionable GraphQL page error", stderr.String())
	}
}

func TestGitHubReviewsJSONRejectsAFalseCompleteTotalWithoutOutput(t *testing.T) {
	fixtures := t.TempDir()
	writeJSONFixture(t, filepath.Join(fixtures, "short.json"), githubReviewPage([]map[string]any{
		githubReviewContribution("only", 301, "2026-05-04T01:00:00Z", "acme/widgets", 42),
	}, 2, false, nil))
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, "#!/bin/sh\nset -eu\ncat \"$GH_FIXTURE_DIR/short.json\"\n")
	command := githubReviewsCommand(t, fakeBin, fixtures, filepath.Join(t.TempDir(), "calls"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-reviews-json.sh accepted fewer nodes than totalCount")
	}
	if stdout.Len() != 0 {
		t.Fatalf("github-reviews-json.sh emitted an incomplete result: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "totalCount") {
		t.Fatalf("completeness failure = %q, want totalCount evidence", stderr.String())
	}
}

func TestGitHubReviewsJSONRejectsDuplicateNodesAcrossPages(t *testing.T) {
	fixtures := t.TempDir()
	duplicate := githubReviewContribution("duplicate", 401, "2026-05-04T01:00:00Z", "acme/widgets", 42)
	writeJSONFixture(t, filepath.Join(fixtures, "first.json"), githubReviewPage(
		[]map[string]any{duplicate}, 2, true, "cursor-1"))
	writeJSONFixture(t, filepath.Join(fixtures, "second.json"), githubReviewPage(
		[]map[string]any{duplicate}, 2, false, nil))
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
case "$*" in
  *cursor=cursor-1*) file=second.json ;;
  *) file=first.json ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	command := githubReviewsCommand(t, fakeBin, fixtures, filepath.Join(t.TempDir(), "calls"),
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-reviews-json.sh silently de-duplicated overlapping GraphQL pages")
	}
	if stdout.Len() != 0 {
		t.Fatalf("github-reviews-json.sh emitted a duplicate traversal: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "duplicate") {
		t.Fatalf("duplicate-page error = %q", stderr.String())
	}
}

func TestGitHubReviewsJSONRejectsAnAdvancingCursorWithAnEmptyPage(t *testing.T) {
	fixtures := t.TempDir()
	writeJSONFixture(t, filepath.Join(fixtures, "first.json"), githubReviewPage([]map[string]any{}, 1, true, "cursor-1"))
	writeJSONFixture(t, filepath.Join(fixtures, "second.json"), githubReviewPage([]map[string]any{}, 1, true, "cursor-2"))
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *cursor=cursor-2*) exit 42 ;;
  *cursor=cursor-1*) file=second.json ;;
  *) file=first.json ;;
esac
cat "$GH_FIXTURE_DIR/$file"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubReviewsCommand(t, fakeBin, fixtures, calls,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-reviews-json.sh accepted an empty page with a continuing cursor")
	}
	if stdout.Len() != 0 {
		t.Fatalf("empty GraphQL page emitted a partial snapshot: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "empty page") ||
		!strings.Contains(stderr.String(), "cannot prove completeness") {
		t.Fatalf("empty-page error = %q, want an actionable completeness failure", stderr.String())
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "api graphql"); got != 1 {
		t.Fatalf("GraphQL calls = %d, want fail-fast on the first empty page", got)
	}
}

func TestGitHubReviewsJSONRejectsTotalBeyondItsFinitePageBudget(t *testing.T) {
	fixtures := t.TempDir()
	writeJSONFixture(t, filepath.Join(fixtures, "large.json"), githubReviewPage([]map[string]any{
		githubReviewContribution("first", 501, "2026-05-04T01:00:00Z", "acme/widgets", 42),
	}, 10001, true, "cursor-1"))
	fakeBin := t.TempDir()
	writeFakeGitHubCLI(t, fakeBin, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALL_LOG"
cat "$GH_FIXTURE_DIR/large.json"
`)
	calls := filepath.Join(t.TempDir(), "calls")
	command := githubReviewsCommand(t, fakeBin, fixtures, calls,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-reviews-json.sh accepted totalCount beyond its finite page budget")
	}
	if stdout.Len() != 0 {
		t.Fatalf("GraphQL safety-limit failure emitted output: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "100-page safety limit") {
		t.Fatalf("GraphQL safety-limit error = %q", stderr.String())
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "api graphql"); got != 1 {
		t.Fatalf("GraphQL calls = %d, want count preflight failure after one page", got)
	}
}

func TestGitHubReviewsJSONUsesTheCurrentFullDatabaseID(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "presets", "bin", githubReviewsHelper))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "fullDatabaseId") {
		t.Fatal("github-reviews-json.sh does not request GitHub's 64-bit review identifier")
	}
	if strings.Contains(string(script), "databaseId") {
		t.Fatal("github-reviews-json.sh still requests GitHub's deprecated 32-bit databaseId")
	}
}

func TestGitHubReviewSourcesUseTheContributionCollectorWithLazyBodies(t *testing.T) {
	wantBody := []string{"gh", "api", "{{id}}"}
	for _, preset := range []string{services.PresetPersonal} {
		t.Run(preset, func(t *testing.T) {
			isolate(t)
			root := filepath.Join(t.TempDir(), "base")
			if _, err := services.Init(t.Context(), services.InitRequest{
				Path: root, Preset: preset, SkipGit: true,
			}, clock); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, core.BaseBinDir, githubReviewsHelper)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("disabled %s source materialized %s: %v", preset, githubReviewsHelper, err)
			}
			config, err := core.LoadConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			source := config.Sources["github-reviews"]
			if !slices.Equal(source.Run, []string{githubReviewsHelper, "{{start}}", "{{end}}"}) {
				t.Fatalf("github-reviews run = %q, want the contribution collector", source.Run)
			}
			if source.Fields.Path(core.FieldID).String() != ".id" ||
				source.Fields.Path(core.FieldTime).String() != ".submittedAt" ||
				source.Fields.Path("repository").String() != ".repository_uri" {
				t.Fatalf("github-reviews field map = %+v", source.Fields)
			}
			if !reflect.DeepEqual(source.Body, wantBody) {
				t.Fatalf("github-reviews body = %#v, want safe argv-only REST lookup %#v", source.Body, wantBody)
			}
		})
	}
}
