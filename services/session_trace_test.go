package services_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/services"
)

const sessionTraceEventConfig = `name: trace-test
layers: {events: true, tasks: true}
sources:
  agent-session-traces:
    enabled: true
    layer: events
    run: [agent-session-trace.sh, "{{start}}", "{{end}}"]
    window: true
    fields:
      id: .id
      time: .time
      title: .title
      repo: .repo
      repository: .repository_uri
`

const sessionTraceEvent = `[{"id":"codex:abc-123","time":"2026-05-09T10:00:00Z","title":"Implement the session trace","harness":"codex","sid":"abc-123","first_at":"2026-05-09T08:00:00Z","last_at":"2026-05-09T10:00:00Z","repo":"fmind/fkf","repository_uri":"repo:github.com/fmind/fkf","requests":["Implement the session trace."],"files":[" M services/sync.go"],"verification":["go test ./services -run Trace"],"last_assistant":"Implemented it."}]`

func TestSessionTraceSyncWritesOrdinaryEventEvidenceAndNoTaskPage(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"": sessionTraceEvent}}
	base := newBase(t, sessionTraceEventConfig, runner)
	trust(t, base)
	report, err := services.Sync(t.Context(), base, services.SyncRequest{
		Date: "2026-05-09", NoGraph: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Written != 1 || len(report.Units) != 1 || report.Units[0].Count != 1 {
		t.Fatalf("session trace sync = %+v", report)
	}
	uri := "events/2026-05-09/agent-session-traces.json"
	read, err := services.Read(t.Context(), base, uri+"#codex:abc-123", services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Record == nil || read.Record["title"] != "Implement the session trace" {
		t.Fatalf("stored session trace = %+v", read)
	}
	taskRoot := filepath.Join(base.Root(), "tasks")
	entries, err := os.ReadDir(taskRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("session trace sync wrote task pages: %v", entries)
	}
}

func TestSessionTraceEmptyAndMalformedRunsUseOrdinaryAtomicDocuments(t *testing.T) {
	for name, output := range map[string]string{"empty": "[]", "malformed": "["} {
		t.Run(name, func(t *testing.T) {
			base := newBase(t, sessionTraceEventConfig, &fakeRunner{responses: map[string]string{"": output}})
			trust(t, base)
			report, err := services.Sync(t.Context(), base, services.SyncRequest{Date: "2026-05-09", NoGraph: true})
			if err != nil {
				t.Fatal(err)
			}
			uri := filepath.Join(base.Root(), "events", "2026-05-09", "agent-session-traces.json")
			if name == "empty" {
				if !report.Complete || report.Written != 1 {
					t.Fatalf("empty run = %+v", report)
				}
				if _, err := os.Stat(uri); err != nil {
					t.Fatalf("empty event document missing: %v", err)
				}
				return
			}
			if report.Failed != 1 || report.Complete {
				t.Fatalf("malformed run = %+v", report)
			}
			if _, err := os.Stat(uri); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("malformed run wrote evidence: %v", err)
			}
		})
	}
}
