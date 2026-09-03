package checks_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestPersonalPresetDeclaresMeetingNotesWithLazyBodyAndRelations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "personal")
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	source := config.Sources["meeting-notes"]
	if source == nil || source.Layer != core.LayerEvents || !source.Window ||
		!slices.Equal(source.Body, []string{"gws-doc-text.sh", "{{id}}"}) ||
		!slices.Contains(source.Run, "{{base}}") {
		t.Fatalf("meeting-notes = %#v, want a windowed event source with reviewed body helper", source)
	}
	for _, field := range []string{"title", "owner", "attachment", "meeting"} {
		if len(source.Fields.Paths(field)) == 0 {
			t.Errorf("meeting-notes omits field %q", field)
		}
	}
	calendar := config.Sources["google-calendar-events"]
	if calendar == nil || len(calendar.Fields.Paths("attachment")) == 0 {
		t.Fatalf("google-calendar-events = %#v, want attachment relation projection", calendar)
	}
}

func TestMeetingNotesTimelineKeepsTheCalendarRecordAndAttendeeAddressable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "personal")
	isolate(t)
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	base, err := services.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	base.Now = clock
	day, err := sources.ParseDay("2026-05-04")
	if err != nil {
		t.Fatal(err)
	}
	collect := func(name, records string) {
		t.Helper()
		source, sourceErr := base.Source(name)
		if sourceErr != nil {
			t.Fatal(sourceErr)
		}
		document, collectErr := sources.Collect(t.Context(),
			sources.RunnerFunc(func(context.Context, sources.Command) (string, error) { return records, nil }),
			source, base.Env, sources.DayWindow(day), time.Minute, testClock)
		if collectErr != nil {
			t.Fatal(collectErr)
		}
		if writeErr := base.WriteDocument(document); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	collect("google-calendar-events", `[{
		"uid":"owner@example.test~event-1","at":"2026-05-04T09:00:00Z","summary":"Attached Review",
		"participant_uris":["person:email/attendee@example.test"]
	}]`)
	calendarURI := "events/2026-05-04/google-calendar-events.json#owner@example.test%7Eevent-1"
	collect("meeting-notes", `[{"id":"doc-1","at":"2026-05-04T09:05:00Z","title":"Attached Review - Notes by Gemini","meeting_uris":["`+calendarURI+`"]}]`)

	report, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		AroundURI: "events/2026-05-04/meeting-notes.json#doc-1", Around: 2 * time.Hour,
		Budget: services.MaxDigestBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourcesSeen := make([]string, 0, len(report.Groups))
	for _, group := range report.Groups {
		sourcesSeen = append(sourcesSeen, group.Source)
	}
	if !slices.Contains(sourcesSeen, "google-calendar-events") ||
		!slices.Contains(report.People, "person:email/attendee@example.test") {
		t.Fatalf("timeline groups = %v, people = %v; want the durable calendar event and attendee", sourcesSeen, report.People)
	}
}

func TestMeetingNotesHelperJoinsAttachmentsAndTitlePrefixes(t *testing.T) {
	bin := t.TempDir()
	fakeGWS := `#!/bin/sh
set -eu
case "$*" in
*"files list"*) cat <<'JSON'
{"files":[
  {"id":"doc-1","name":"Attached Review - Notes by Gemini","createdTime":"2026-05-04T09:00:00Z","modifiedTime":"2026-05-04T09:30:00Z","webViewLink":"https://docs.google.com/document/d/doc-1/edit","owners":[{"emailAddress":"owner@example.test","me":true}]},
  {"id":"doc-2","name":"Owner Sync - Fmind Notes","createdTime":"2026-05-04T11:00:00Z","modifiedTime":"2026-05-04T11:30:00Z","webViewLink":"https://docs.google.com/document/d/doc-2/edit","owners":[{"emailAddress":"owner@example.test","me":true}]},
  {"id":"doc-3","name":"All-day Review - Notes by Gemini","createdTime":"2026-05-05T09:00:00Z","modifiedTime":"2026-05-05T09:30:00Z","webViewLink":"https://docs.google.com/document/d/doc-3/edit","owners":[{"emailAddress":"owner@example.test","me":true}]}
]}
JSON
;;
*"calendarList list"*) printf '%s\n' '{"items":[{"id":"owner@example.test","summary":"Primary","primary":true}]}' ;;
	*"events list"*) cat <<'JSON'
	{"items":[
	  {"id":"event-1","summary":"Attached Review","start":{"dateTime":"2026-05-04T09:00:00Z"},"end":{"dateTime":"2026-05-04T10:00:00Z"},"attachments":[{"fileId":"doc-1","fileUrl":"https://docs.google.com/document/d/doc-1/edit","title":"Attached Review - Notes by Gemini"}]},
	  {"id":"event-old","summary":"Owner Sync","start":{"dateTime":"2026-05-04T08:00:00Z"},"end":{"dateTime":"2026-05-04T08:30:00Z"}},
	  {"id":"event-2","summary":"Owner Sync","start":{"dateTime":"2026-05-04T11:00:00Z"},"end":{"dateTime":"2026-05-04T12:00:00Z"}}
	  ,{"id":"event-3","summary":"All-day Review","start":{"date":"2026-05-05"},"end":{"date":"2026-05-06"},"attachments":[{"fileId":"doc-3","fileUrl":"https://docs.google.com/document/d/doc-3/edit","title":"All-day Review - Notes by Gemini"}]}
	]}
JSON
;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	helperBin := filepath.Join(repositoryRoot(t), "presets", "bin")
	base := t.TempDir()
	run := func() ([]byte, error) {
		command := exec.CommandContext(t.Context(), "sh", filepath.Join(helperBin, "gws-meeting-notes-json.sh"),
			"2026-05-01T00:00:00Z", "2026-05-06T00:00:00Z", "2026-05-01", "2026-05-06", "Fmind", base)
		command.Env = append(os.Environ(),
			"TZ=UTC",
			"PATH="+strings.Join([]string{bin, helperBin, os.Getenv("PATH")}, string(os.PathListSeparator)),
		)
		return command.CombinedOutput()
	}
	output, err := run()
	if err == nil || !strings.Contains(string(output), "enable and sync google-calendar-events") {
		t.Fatalf("helper without durable calendar evidence = %v, %q; want a fail-closed dependency error", err, output)
	}
	calendarRecords := map[string]string{
		"2026-05-04": `{"fkf":1,"source":"google-calendar-events","fields":{"id":".uid"},"records":[{"uid":"owner@example.test~event-1"},{"uid":"owner@example.test~event-2"}]}`,
		"2026-05-05": `{"fkf":1,"source":"google-calendar-events","fields":{"id":".uid"},"records":[{"uid":"owner@example.test~event-3"}]}`,
	}
	for date, document := range calendarRecords {
		directory := filepath.Join(base, "events", date)
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "google-calendar-events.json"), []byte(document+"\n"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
	}
	output, err = run()
	if err != nil {
		t.Fatalf("gws-meeting-notes-json.sh error = %v\n%s", err, output)
	}
	var records []struct {
		ID             string   `json:"id"`
		OwnerURIs      []string `json:"owner_uris"`
		AttachmentURIs []string `json:"attachment_uris"`
		MeetingURIs    []string `json:"meeting_uris"`
	}
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode meeting notes: %v\n%s", err, output)
	}
	if len(records) != 3 || records[0].ID != "doc-1" || records[1].ID != "doc-2" || records[2].ID != "doc-3" {
		t.Fatalf("meeting notes = %s, want all three ordered documents", output)
	}
	for index, event := range []string{"event-1", "event-2", "event-3"} {
		date := "2026-05-04"
		if event == "event-3" {
			date = "2026-05-05"
		}
		want := "events/" + date + "/google-calendar-events.json#owner@example.test%7E" + event
		if !slices.Equal(records[index].MeetingURIs, []string{want}) ||
			!slices.Equal(records[index].OwnerURIs, []string{"person:email/owner@example.test"}) ||
			len(records[index].AttachmentURIs) != 1 {
			t.Fatalf("meeting note %d = %+v, want canonical owner, document, and calendar relations", index, records[index])
		}
	}
}

func TestGoogleDocBodyHelperPrintsVisibleTextInDocumentOrder(t *testing.T) {
	bin := t.TempDir()
	fakeGWS := `#!/bin/sh
set -eu
cat <<'JSON'
{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"First paragraph.\n"}}]}},{"table":{"tableRows":[{"tableCells":[{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Table cell.\n"}}]}}]}]}]}}]}}
JSON
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-doc-text.sh")
	command := exec.CommandContext(t.Context(), "sh", script, "document-1")
	command.Env = append(os.Environ(), "PATH="+strings.Join([]string{bin, os.Getenv("PATH")}, string(os.PathListSeparator)))
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "First paragraph.\nTable cell.\n\n" {
		t.Fatalf("gws-doc-text.sh = %q, %v; want plain visible text in order", output, err)
	}
}

func TestMeetingHelpersAnswerVersionWithoutProviderExecution(t *testing.T) {
	for _, name := range []string{"gws-meeting-notes-json.sh", "gws-doc-text.sh"} {
		command := exec.CommandContext(t.Context(), "sh", filepath.Join(repositoryRoot(t), "presets", "bin", name), "--version")
		command.Env = []string{"PATH=/nonexistent"}
		output, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) == "" {
			t.Fatalf("%s --version = %q, %v", name, output, err)
		}
	}
}
