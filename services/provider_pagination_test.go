package services_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestProviderPaginationRoutesThroughBoundedCollectors(t *testing.T) {
	configs := make(map[string]*core.Config, 2)
	roots := make(map[string]string, 2)
	for _, preset := range []string{services.PresetPersonal, services.PresetTeam} {
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
		configs[preset] = config
		roots[preset] = root
	}

	want := map[string]map[string][]string{
		services.PresetPersonal: {
			"github-repositories": {"github-list-json.sh", "user-repositories"},
		},
		services.PresetTeam: {
			"github-repositories": {"github-list-json.sh", "org-repositories", "REPLACE_WITH_ORG"},
		},
	}
	for preset, sources := range want {
		for name, command := range sources {
			if got := configs[preset].Sources[name].Run; !slices.Equal(got, command) {
				t.Errorf("%s/%s run = %q, want bounded collector %q", preset, name, got, command)
			}
		}
	}

	for _, name := range []string{"google-drive-files"} {
		run := configs[services.PresetPersonal].Sources[name].Run
		if !slices.Contains(run, "--page-limit") || !slices.Contains(run, "100") || run[0] != "gws-page-json.sh" {
			t.Errorf("personal/%s has no bounded, token-validated pagination: %s", name, run)
		}
	}
	if _, err := os.Stat(filepath.Join(roots[services.PresetPersonal], core.BaseBinDir, "gws-page-json.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled GWS examples materialized their page validator: %v", err)
	}
}

func TestGitHubListJSONFollowsLinkPagesAndProjectsRepositories(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *" page=1"*)
    printf '%s\n' 'HTTP/2.0 200 OK' 'Link: <https://api.github.test/user/repos?page=2>; rel="next"' ''
    printf '%s\n' '[{"full_name":"acme/one","html_url":"https://github.test/acme/one","updated_at":"2026-08-01T00:00:00Z","archived":false}]'
    ;;
  *" page=2"*)
    printf '%s\n' 'HTTP/2.0 200 OK' ''
    printf '%s\n' '[{"full_name":"acme/two","html_url":"https://github.test/acme/two","updated_at":"2026-08-02T00:00:00Z","archived":true}]'
    ;;
  *) exit 2 ;;
esac
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-list-json.sh"),
		"user-repositories")
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CALL_LOG="+calls,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-list-json.sh error = %v\n%s", err, output)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var names []string
	for {
		var record struct {
			Name string `json:"nameWithOwner"`
		}
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode repository record: %v\n%s", err, output)
		}
		names = append(names, record.Name)
	}
	if strings.Join(names, ",") != "acme/one,acme/two" {
		t.Fatalf("repository names = %v, want both Link pages", names)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--include", "page=1", "page=2", "per_page=100"} {
		if !strings.Contains(string(log), required) {
			t.Errorf("GitHub calls omit %q:\n%s", required, log)
		}
	}
}

func TestGitHubListJSONFailsClosedAtAdvancingPageLimit(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' "$*" >> "$GH_CALL_LOG"
printf '%s\n' 'HTTP/2.0 200 OK' 'Link: <https://api.github.test/notifications?page=next>; rel="next"' '' '[]'
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-list-json.sh"),
		"notifications", "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CALL_LOG="+calls,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-list-json.sh accepted an advancing Link cursor past its page limit")
	}
	if stdout.Len() != 0 {
		t.Fatalf("page-limit failure emitted a partial snapshot: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "100-page safety limit") ||
		!strings.Contains(stderr.String(), "cannot prove completeness") {
		t.Fatalf("page-limit error = %q, want an actionable completeness failure", stderr.String())
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(log)), "\n") + 1; got != 100 {
		t.Fatalf("GitHub calls = %d, want the finite 100-page ceiling", got)
	}
}

func TestGitHubListJSONProjectsNotificationMetadata(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' 'HTTP/2.0 200 OK' ''
printf '%s\n' '[{"id":"n-1","unread":true,"reason":"mention","updated_at":"2026-08-01T12:00:00Z","last_read_at":null,"subject":{"title":"Review","url":"https://api.github.test/repos/acme/repo/issues/1","latest_comment_url":"https://api.github.test/comments/1","type":"Issue","private_body":"forbidden-notification-sentinel"},"repository":{"full_name":"acme/repo","html_url":"https://github.test/acme/repo","owner":{"email":"forbidden-notification-sentinel"}},"private_body":"forbidden-notification-sentinel"}]'
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-list-json.sh"),
		"notifications", "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-list-json.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), "forbidden-notification-sentinel") {
		t.Fatalf("notification projection retained an undeclared provider field: %s", output)
	}
	for _, required := range []string{`"id":"n-1"`, `"reason":"mention"`, `"full_name":"acme/repo"`} {
		if !strings.Contains(string(output), required) {
			t.Errorf("notification projection omits useful metadata %s: %s", required, output)
		}
	}
}

func TestGitHubListJSONFiltersNotificationsToTheExactUpdatedWindow(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' 'HTTP/2.0 200 OK' ''
printf '%s\n' '[{"id":"before","updated_at":"2026-07-31T23:59:59Z"},{"id":"start","updated_at":"2026-08-01T00:00:00Z"},{"id":"inside","updated_at":"2026-08-01T12:00:00Z"},{"id":"end","updated_at":"2026-08-02T00:00:00Z"},{"id":"after","updated_at":"2026-08-03T00:00:00Z"}]'
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-list-json.sh"),
		"notifications", "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-list-json.sh error = %v\n%s", err, output)
	}
	var ids []string
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var record struct {
			ID string `json:"id"`
		}
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode notification record: %v\n%s", err, output)
		}
		ids = append(ids, record.ID)
	}
	if got := strings.Join(ids, ","); got != "start,inside" {
		t.Fatalf("notification ids = %q, want exact half-open updated_at window", got)
	}
}

func TestGitHubListJSONRejectsMalformedNotificationTimesWithoutOutput(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' 'HTTP/2.0 200 OK' ''
printf '%s\n' '[{"id":"valid","updated_at":"2026-08-01T12:00:00Z"},{"id":"malformed","updated_at":"not-a-time"}]'
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-list-json.sh"),
		"notifications", "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-list-json.sh accepted a notification with malformed updated_at")
	}
	if stdout.Len() != 0 {
		t.Fatalf("malformed notification time emitted partial output: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "valid updated_at") {
		t.Fatalf("malformed notification error = %q, want the field contract", stderr.String())
	}
}

func TestGWSPageJSONPreservesACompleteTokenChain(t *testing.T) {
	isolate(t)
	input := strings.Join([]string{
		`{"items":[],"nextPageToken":"cursor-1"}`,
		`{"items":[{"id":"complete"}]}`,
	}, "\n") + "\n"
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-page-json.sh"), "items")
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-page-json.sh error = %v\n%s", err, output)
	}
	if strings.Count(strings.TrimSpace(string(output)), "\n")+1 != 2 ||
		!strings.Contains(string(output), `"id":"complete"`) {
		t.Fatalf("validated GWS pages = %q, want both original envelopes", output)
	}
}

func TestGWSPageJSONRejectsAdvancingEmptyCursorsAtPageLimit(t *testing.T) {
	isolate(t)
	var input strings.Builder
	for page := 1; page <= 100; page++ {
		fmt.Fprintf(&input, "{\"items\":[],\"nextPageToken\":\"cursor-%d\"}\n", page)
	}
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-page-json.sh"), "items")
	command.Stdin = strings.NewReader(input.String())
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("gws-page-json.sh accepted a token after the configured page limit")
	}
	if stdout.Len() != 0 {
		t.Fatalf("GWS page-limit failure emitted partial pages: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "page limit") ||
		!strings.Contains(stderr.String(), "cannot prove completeness") {
		t.Fatalf("GWS page-limit error = %q", stderr.String())
	}
}

func TestGWSPageJSONProjectsChatSpaceMetadata(t *testing.T) {
	isolate(t)
	input := `{"spaces":[{"name":"spaces/one","displayName":"Project","spaceType":"SPACE","spaceThreadingState":"THREADED_MESSAGES","lastActiveTime":"2026-08-01T12:00:00Z","membershipCount":7,"spaceUri":"https://chat.google.test/room/one","spaceDetails":{"description":"forbidden-space-sentinel"},"private_body":"forbidden-space-sentinel"}]}` + "\n"
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-page-json.sh"), "spaces")
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-page-json.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), "forbidden-space-sentinel") {
		t.Fatalf("Chat space projection retained an undeclared provider field: %s", output)
	}
	for _, required := range []string{`"name":"spaces/one"`, `"membershipCount":7`, `"spaceType":"SPACE"`} {
		if !strings.Contains(string(output), required) {
			t.Errorf("Chat space projection omits useful metadata %s: %s", required, output)
		}
	}
}

func TestGWSPageJSONProjectsDriveFileMetadata(t *testing.T) {
	isolate(t)
	input := `{"files":[{"id":"file-1","name":"Plan","mimeType":"application/vnd.google-apps.document","webViewLink":"https://drive.google.test/file-1","modifiedTime":"2026-08-02T12:00:00Z","createdTime":"2026-08-01T12:00:00Z","size":"42","shared":true,"trashed":false,"parents":["folder-1"],"owners":[{"displayName":"Owner","emailAddress":"owner@example.test","photoLink":"forbidden-drive-sentinel"}],"description":"forbidden-drive-sentinel"}],"private_body":"forbidden-drive-sentinel"}` + "\n"
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-page-json.sh"), "files")
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-page-json.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), "forbidden-drive-sentinel") {
		t.Fatalf("Drive file projection retained an undeclared provider field: %s", output)
	}
	for _, required := range []string{`"id":"file-1"`, `"mimeType":"application/vnd.google-apps.document"`, `"displayName":"Owner"`, `"emailAddress":"owner@example.test"`} {
		if !strings.Contains(string(output), required) {
			t.Errorf("Drive file projection omits useful metadata %s: %s", required, output)
		}
	}
}

func TestGWSPageJSONProjectsContactMetadata(t *testing.T) {
	isolate(t)
	input := `{"connections":[{"resourceName":"people/one","etag":"forbidden-contact-sentinel","names":[{"displayName":"Ada Lovelace","displayNameLastFirst":"Lovelace, Ada","unstructuredName":"Ada Lovelace","familyName":"Lovelace","givenName":"Ada","middleName":"Byron","honorificPrefix":"Countess","honorificSuffix":"I","metadata":{"primary":true,"private":"forbidden-contact-sentinel"}}],"emailAddresses":[{"value":"ada@example.test","type":"work","formattedType":"Work","displayName":"Ada","metadata":{"primary":true,"private":"forbidden-contact-sentinel"},"private":"forbidden-contact-sentinel"}],"private_body":"forbidden-contact-sentinel"}]}` + "\n"
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gws-page-json.sh"), "connections")
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-page-json.sh error = %v\n%s", err, output)
	}
	if strings.Contains(string(output), "forbidden-contact-sentinel") {
		t.Fatalf("Contact projection retained an undeclared provider field: %s", output)
	}
	for _, required := range []string{`"resourceName":"people/one"`, `"displayName":"Ada Lovelace"`, `"familyName":"Lovelace"`, `"value":"ada@example.test"`, `"primary":true`} {
		if !strings.Contains(string(output), required) {
			t.Errorf("Contact projection omits useful metadata %s: %s", required, output)
		}
	}
}

func TestGCloudAuditRejectsLimitPlusOneWithoutPartialOutput(t *testing.T) {
	isolate(t)
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gcloud", `jq -cn '[range(0; 10001) | {}]'
`)
	command := exec.CommandContext(t.Context(),
		filepath.Join(repositoryRoot(t), "presets", "bin", "gcloud-audit-json.sh"),
		"2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("gcloud-audit-json.sh accepted the limit-plus-one overflow record")
	}
	if stdout.Len() != 0 {
		t.Fatalf("gcloud audit overflow emitted a partial day: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "10000-item safety limit") {
		t.Fatalf("gcloud audit overflow error = %q", stderr.String())
	}
}
