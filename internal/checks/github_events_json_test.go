package checks_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubEventsStopsAtTheDocumentedThreeHundredEventBoundary(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-48 * time.Hour).Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	calls := filepath.Join(t.TempDir(), "calls")
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gh", `printf '%s\n' "$*" >> "$GH_CALL_LOG"
case "$*" in
  *" /user --jq .login"*) printf '%s\n' fmind ;;
  *" page=1"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-1-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *" page=2"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-2-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *" page=3"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-3-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *) exit 42 ;;
esac
`)
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-events-json.sh"),
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+filepath.Join(repositoryRoot(t), "presets", "bin")+
			string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CALL_LOG="+calls,
		"GH_EVENT_TIME="+start.Add(12*time.Hour).Format(time.RFC3339),
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("github-events-json.sh accepted a window hidden by the 300-event cap")
	}
	if stdout.Len() != 0 {
		t.Fatalf("saturated GitHub events emitted a partial snapshot: %s", stdout.Bytes())
	}
	if !strings.Contains(stderr.String(), "300-event feed cuts off") ||
		!strings.Contains(stderr.String(), "completeness cannot be proved") {
		t.Fatalf("saturated GitHub events error = %q", stderr.String())
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "page=4") {
		t.Fatalf("GitHub events requested the unsupported fourth page:\n%s", log)
	}
}

func TestGitHubEventsCollectsAnUnsaturatedFeed(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-48 * time.Hour).Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	fakeBin := t.TempDir()
	writePresetFake(t, fakeBin, "gh", `case "$*" in
  *" /user --jq .login"*) printf '%s\n' fmind ;;
  *" page=1"*) jq -cn --arg time "$GH_EVENT_TIME" '[range(0; 100) | {id:("page-1-" + tostring),type:"PushEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *" page=2"*) jq -cn --arg time "$GH_EVENT_TIME" '[{id:"page-2",type:"IssuesEvent",created_at:$time,public:true,actor:{login:"fmind"},repo:{name:"fmind/fkf"},org:null}]' ;;
  *) exit 42 ;;
esac
`)
	command := exec.CommandContext(t.Context(), "/bin/sh",
		filepath.Join(repositoryRoot(t), "presets", "bin", "github-events-json.sh"),
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_EVENT_TIME="+start.Add(12*time.Hour).Format(time.RFC3339),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("github-events-json.sh error = %v\n%s", err, output)
	}
	if got := strings.Count(string(output), `"id"`); got != 101 {
		t.Fatalf("GitHub event count = %d, want all 101 events", got)
	}
}
