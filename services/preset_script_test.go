package services_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fakeSQLiteBackup = `#!/bin/sh
set -eu
if [ "$3" = -readonly ]; then
  source=$4
  destination=$(printf '%s' "$5" | sed -n "s/^\\.backup '\\(.*\\)'$/\\1/p")
  [ -n "$destination" ] || exit 2
  if [ -f "$source-wal" ]; then
    cp "$source-wal" "$destination"
  else
    cp "$source" "$destination"
  fi
  exit 0
fi
[ "${FAIL_SQLITE_QUERY:-}" != 1 ] || exit 8
cat "$4"
`

func writeFakeSQLite(t *testing.T, directory string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "sqlite3"), []byte(fakeSQLiteBackup), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestChromiumPagesCollectsEveryProfileWithAGlobalIdentity(t *testing.T) {
	home := t.TempDir()
	profiles := map[string]string{
		".config/chromium/Default/History":        `[{"visit":7,"url":"https://private-user:private-password@one.example.test/path?sensitive-test-value#fragment","title":"One","time":"2026-05-04T09:00:00Z"}]`,
		".config/google-chrome/Profile 1/History": `[{"visit":7,"url":"http://localhost/callback?private-test-value","title":"","time":"2026-05-04T10:00:00Z"}]`,
	}
	for relative, body := range profiles {
		absolute := filepath.Join(home, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	writeFakeSQLite(t, bin)

	script := filepath.Join(repositoryRoot(t), "presets", "bin", "chromium-pages")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("chromium-pages error = %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode chromium-pages output: %v\n%s", err, output)
	}
	if len(records) != 2 {
		t.Fatalf("chromium-pages returned %d profile record(s), want 2: %s", len(records), output)
	}
	first, second := records[0]["uid"], records[1]["uid"]
	if first == nil || second == nil || first == second {
		t.Fatalf("profile-qualified uids = %v, %v; want two distinct identities", first, second)
	}
	if records[0]["profile"] == records[1]["profile"] {
		t.Fatalf("profiles = %v, %v; want provenance for each browser profile",
			records[0]["profile"], records[1]["profile"])
	}
	for _, forbidden := range []string{
		"private-user", "private-password", "sensitive-test-value", "private-test-value", "fragment",
	} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("chromium-pages retained URL userinfo, query, or fragment %q: %s", forbidden, output)
		}
	}
	urls := map[any]bool{records[0]["url"]: true, records[1]["url"]: true}
	if !urls["https://one.example.test/path"] || !urls[nil] {
		t.Fatalf("sanitized URLs = %v; want an HTTPS path without query/fragment and null for HTTP", urls)
	}
	if records[1]["title"] != nil {
		t.Fatalf("empty Chromium title = %v, want null so an optional semantic field is absent", records[1]["title"])
	}
}

func TestChromiumPagesUsesSQLiteBackupSoLiveWALVisitsAreIncluded(t *testing.T) {
	home := t.TempDir()
	history := filepath.Join(home, ".config", "chromium", "Default", "History")
	if err := os.MkdirAll(filepath.Dir(history), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	walRecord := `[{"visit":9,"url":"https://wal.example.test","title":"Committed in WAL","time":"2026-05-04T11:00:00Z"}]`
	if err := os.WriteFile(history+"-wal", []byte(walRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFakeSQLite(t, bin)

	script := filepath.Join(repositoryRoot(t), "presets", "bin", "chromium-pages")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("chromium-pages error = %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode chromium-pages output: %v\n%s", err, output)
	}
	if len(records) != 1 || records[0]["title"] != "Committed in WAL" {
		t.Fatalf("records = %s, want the committed visit still in the live WAL", output)
	}
}

func TestChromiumPagesFailsWhenAHistoryQueryFails(t *testing.T) {
	home := t.TempDir()
	history := filepath.Join(home, ".config", "chromium", "Default", "History")
	if err := os.MkdirAll(filepath.Dir(history), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFakeSQLite(t, bin)

	script := filepath.Join(repositoryRoot(t), "presets", "bin", "chromium-pages")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAIL_SQLITE_QUERY=1")
	output, err := command.Output()
	if err == nil {
		t.Fatalf("chromium-pages accepted a failed History query: %s", output)
	}
	if len(output) != 0 {
		t.Fatalf("chromium-pages emitted partial output after a failed History query: %s", output)
	}
}

func TestGmailJSONStopsBeforeProviderWhenBoundConversionFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "jq"), []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "gws-called")
	fakeGWS := "#!/bin/sh\nprintf called > \"$GWS_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-json")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GWS_MARKER="+marker)
	if err := command.Run(); err == nil {
		t.Fatal("gmail-json succeeded after the RFC3339 conversion failed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gws ran after conversion failure: %v", err)
	}
}

func TestGmailJSONUsesAnExactHalfOpenInternalDateWindow(t *testing.T) {
	bin := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gws-calls")
	fakeGWS := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GWS_CALL_LOG"
case "$*" in
  *"users messages list"*) printf '%s\n' '{"messages":[{"id":"before"},{"id":"start"},{"id":"end"}]}' ;;
  *'"id":"before"'*) printf '%s\n' '{"id":"before","threadId":"t","internalDate":"1777852799999","payload":{"headers":[]}}' ;;
  *'"id":"start"'*) printf '%s\n' '{"id":"start","threadId":"t","internalDate":"1777852800000","payload":{"headers":[]}}' ;;
  *'"id":"end"'*) printf '%s\n' '{"id":"end","threadId":"t","internalDate":"1777939200000","payload":{"headers":[]}}' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-json")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GWS_CALL_LOG="+callLog,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gmail-json error = %v\n%s", err, output)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	var records []map[string]any
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode gmail-json output: %v\n%s", err, output)
		}
		records = append(records, record)
	}
	if len(records) != 1 || records[0]["id"] != "start" {
		t.Fatalf("gmail-json records = %s, want the exact lower bound and neither outside record", output)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "after:1777852799 before:1777939200") {
		t.Fatalf("Gmail search did not widen its exclusive lower operator by one second: %s", calls)
	}
}

func TestGWSTasksUsesAnExactHalfOpenWindowAndIncludesEveryTaskState(t *testing.T) {
	bin := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gws-calls")
	fakeGWS := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GWS_CALL_LOG"
case "$*" in
  *"tasks tasklists list"*)
    printf '%s\n' '{"items":[{"id":"list-1","title":"All"}]}'
    ;;
  *"tasks tasks list"*)
    printf '%s\n' '{"items":[{"id":"inside","updated":"2026-05-04T23:59:59.999Z","title":"Inside","notes":"sensitive-task-body","assignmentInfo":{"driveResourceInfo":{"driveFileId":"private-drive-id"}},"links":[{"link":"https://private.example.test"}]},{"id":"boundary","updated":"2026-05-05T00:00:00.000Z","title":"Next day"}]}'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-tasks")
	command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GWS_CALL_LOG="+callLog,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-tasks error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode gws-tasks output: %v\n%s", err, output)
	}
	if len(records) != 1 || records[0]["id"] != "inside" {
		t.Fatalf("gws-tasks records = %s, want only the task strictly before the upper bound", output)
	}
	for _, private := range []string{"sensitive-task-body", "private-drive-id", "private.example"} {
		if strings.Contains(string(output), private) {
			t.Errorf("task metadata projection retained %q: %s", private, output)
		}
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"showCompleted", "showHidden", "showDeleted", "showAssigned"} {
		if !strings.Contains(string(calls), `"`+field+`":true`) {
			t.Errorf("gws task request does not include %s=true: %s", field, calls)
		}
	}
}

func TestGWSCalendarKeepsOnlyInWindowStartsAndProjectsMetadata(t *testing.T) {
	bin := t.TempDir()
	fakeGWS := `#!/bin/sh
set -eu
cat <<'JSON'
{"items":[
  {"id":"timed-spanning","summary":"Started yesterday","start":{"dateTime":"2026-05-03T23:30:00Z"},"end":{"dateTime":"2026-05-04T01:00:00Z"}},
  {"id":"all-day-spanning","summary":"All day overlap","start":{"date":"2026-05-03"},"end":{"date":"2026-05-05"}},
  {"id":"inside","status":"confirmed","eventType":"default","summary":"Inside","htmlLink":"https://calendar.example.test/event/inside","start":{"dateTime":"2026-05-04T11:00:00+02:00","timeZone":"Europe/Paris"},"end":{"dateTime":"2026-05-04T12:00:00+02:00"},"organizer":{"email":"organizer#calendar@example.test","displayName":"Private name"},"attendees":[{"email":"guest@example.test","responseStatus":"accepted","comment":"private response"}],"description":"sensitive-calendar-body","location":"private-location","conferenceData":{"entryPoints":[{"uri":"https://meet.example.test/private"}]},"attachments":[{"fileUrl":"https://drive.example.test/private"}]},
  {"id":"boundary","summary":"Next day","start":{"dateTime":"2026-05-05T00:00:00Z"},"end":{"dateTime":"2026-05-05T01:00:00Z"}}
]}
JSON
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-calendar-json")
	command := exec.CommandContext(t.Context(), "sh", script,
		"2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", "2026-05-04", "2026-05-05")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("gws-calendar-json error = %v\n%s", err, output)
	}
	var records []map[string]any
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode gws-calendar-json output: %v\n%s", err, output)
	}
	if len(records) != 1 || records[0]["id"] != "inside" {
		t.Fatalf("calendar records = %s, want only the event whose start is inside the window", output)
	}
	if !strings.Contains(string(output), "person:email/organizer%23calendar@example.test") {
		t.Fatalf("calendar relation did not canonically percent-encode the email identity: %s", output)
	}
	for _, private := range []string{"sensitive-calendar-body", "private-location", "private response", "meet.example", "drive.example", "Private name"} {
		if strings.Contains(string(output), private) {
			t.Errorf("calendar metadata projection retained %q: %s", private, output)
		}
	}
}

func TestGCloudAuditJSONProjectsMetadataAndFailsClosed(t *testing.T) {
	t.Run("projects metadata", func(t *testing.T) {
		bin := t.TempDir()
		fakeGCloud := `#!/bin/sh
set -eu
cat <<'JSON'
[{"insertId":"insert-1","timestamp":"2026-05-04T09:00:00Z","receiveTimestamp":"2026-05-04T09:00:01Z","severity":"NOTICE","logName":"projects/example/logs/activity","resource":{"type":"audited_resource","labels":{"project_id":"example","location":"europe-west1","private_label":"private-resource"}},"protoPayload":{"serviceName":"example.googleapis.com","methodName":"example.v1.Service.Read","resourceName":"projects/example/resources/one","authenticationInfo":{"principalEmail":"developer@example.test","principalSubject":"user:developer@example.test"},"authorizationInfo":[{"resource":"projects/example/resources/one","permission":"example.resources.get","granted":true}],"status":{"code":0,"message":"private-status"},"request":{"secret":"sensitive-request-body"},"response":{"secret":"sensitive-response-body"},"requestMetadata":{"callerIp":"192.0.2.1"}},"operation":{"id":"operation-1","producer":"example.googleapis.com","first":true,"last":true}}]
JSON
`
		if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(fakeGCloud), 0o700); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(repositoryRoot(t), "presets", "bin", "gcloud-audit-json")
		command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
		command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud-audit-json error = %v\n%s", err, output)
		}
		var records []map[string]any
		if err := json.Unmarshal(output, &records); err != nil {
			t.Fatalf("decode gcloud-audit-json output: %v\n%s", err, output)
		}
		if len(records) != 1 || records[0]["uid"] != "example@insert-1@2026-05-04T09:00:00Z" {
			t.Fatalf("gcloud audit records = %s, want one stable metadata record", output)
		}
		for _, private := range []string{"private-resource", "private-status", "sensitive-request-body", "sensitive-response-body", "192.0.2.1"} {
			if strings.Contains(string(output), private) {
				t.Errorf("Cloud Audit metadata projection retained %q: %s", private, output)
			}
		}
	})

	t.Run("provider failure emits nothing", func(t *testing.T) {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte("#!/bin/sh\nprintf partial\nexit 7\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(repositoryRoot(t), "presets", "bin", "gcloud-audit-json")
		command := exec.CommandContext(t.Context(), "sh", script, "2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z")
		command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		output, err := command.Output()
		if err == nil {
			t.Fatalf("gcloud-audit-json accepted a failed provider: %s", output)
		}
		if len(output) != 0 {
			t.Fatalf("gcloud-audit-json emitted partial output after provider failure: %s", output)
		}
	})
}
