package services_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestImportSessionTracesRejectsMalformedHelperOutput(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	window := sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"}
	tooMany := make([]services.SessionTraceInput, 1025)
	for index := range tooMany {
		tooMany[index] = testSessionTraceInput()
		tooMany[index].ID += string(rune('a' + index%26))
	}
	cases := []struct {
		name   string
		stdout string
		want   string
	}{
		{"empty", " \n", "printed nothing"},
		{"not an array", `{}`, "cannot unmarshal object"},
		{"unknown field", `[{"unknown":true}]`, "unknown field"},
		{"too many", encodeSessionTraceInputs(t, tooMany...), "limit is 1024"},
		{"second document", `[] []`, "more than one document"},
		{"invalid trailing JSON", `[] {`, "invalid trailing JSON"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := services.ImportSessionTraces(t.Context(), base, source, test.stdout, window)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ImportSessionTraces() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestImportSessionTracesRejectsInvalidFieldsBeforeWriting(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	validWindow := sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"}
	valid := testSessionTraceInput()
	requests := make([]string, 21)
	files := make([]string, 201)
	commands := make([]string, 21)
	for index := range requests {
		requests[index] = "request"
	}
	for index := range files {
		files[index] = "file"
	}
	for index := range commands {
		commands[index] = "command"
	}
	cases := []struct {
		name   string
		mutate func(*services.SessionTraceInput)
		window sources.Window
		want   string
	}{
		{"harness", func(input *services.SessionTraceInput) { input.Harness = "Codex" }, validWindow, "short lowercase name"},
		{"empty sid", func(input *services.SessionTraceInput) { input.SID = "" }, validWindow, "sid is empty"},
		{"sid layout", func(input *services.SessionTraceInput) { input.SID = "bad\tsid" }, validWindow, "layout control"},
		{"first time", func(input *services.SessionTraceInput) { input.FirstAt = "yesterday" }, validWindow, "first_at"},
		{"last time", func(input *services.SessionTraceInput) { input.LastAt = "today" }, validWindow, "last_at"},
		{"window", func(_ *services.SessionTraceInput) {}, sources.Window{Start: "bad", End: "also-bad"}, "window is not RFC3339"},
		{"reverse time", func(input *services.SessionTraceInput) { input.FirstAt = "2026-05-09T11:00:00Z" }, validWindow, "first_at is after"},
		{"outside window", func(input *services.SessionTraceInput) { input.LastAt = "2026-05-10T00:00:00Z" }, validWindow, "outside requested window"},
		{"relative cwd", func(input *services.SessionTraceInput) { input.CWD = "relative" }, validWindow, "cwd must be absolute"},
		{"long cwd", func(input *services.SessionTraceInput) { input.CWD = "/" + strings.Repeat("x", 4097) }, validWindow, "cwd is"},
		{"repository", func(input *services.SessionTraceInput) { input.Repo = "three/part/name" }, validWindow, "exact owner/name"},
		{"model layout", func(input *services.SessionTraceInput) { input.Model = "bad\nmodel" }, validWindow, "model contains layout"},
		{"no requests", func(input *services.SessionTraceInput) { input.Requests = nil }, validWindow, "expected 1..20"},
		{"too many requests", func(input *services.SessionTraceInput) { input.Requests = requests }, validWindow, "requests has 21 entries"},
		{"long request", func(input *services.SessionTraceInput) { input.Requests = []string{strings.Repeat("x", 6001)} }, validWindow, "requests[0] is"},
		{"unsafe request", func(input *services.SessionTraceInput) { input.Requests = []string{"unsafe\x00request"} }, validWindow, "requests[0]"},
		{"too many files", func(input *services.SessionTraceInput) { input.Files = files }, validWindow, "files has 201 entries"},
		{"empty file", func(input *services.SessionTraceInput) { input.Files = []string{""} }, validWindow, "files[0] is empty"},
		{"too many commands", func(input *services.SessionTraceInput) { input.Verification = commands }, validWindow, "verification has 21 entries"},
		{"command layout", func(input *services.SessionTraceInput) { input.Verification = []string{"go test\nrm -rf"} }, validWindow, "verification[0] contains layout"},
		{"assistant excerpt", func(input *services.SessionTraceInput) { input.LastAssistant = strings.Repeat("x", 6001) }, validWindow, "last_assistant is"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			_, err := services.ImportSessionTraces(t.Context(), base, source, encodeSessionTraceInputs(t, input), test.window)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ImportSessionTraces() error = %v, want %q", err, test.want)
			}
		})
	}
	if entries, err := os.ReadDir(filepath.Join(base.Root(), "tasks")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("invalid inputs wrote task entries: %v", entries)
	}
}

func TestImportSessionTracesRejectsWrongLayerDuplicatesCancellationAndUnsafeTargets(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	window := sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"}
	input := testSessionTraceInput()

	wrongLayer := *source
	wrongLayer.Layer = core.LayerEvents
	if _, err := services.ImportSessionTraces(t.Context(), base, &wrongLayer, encodeSessionTraceInputs(t, input), window); err == nil ||
		!strings.Contains(err.Error(), "not tasks") {
		t.Fatalf("wrong-layer import error = %v", err)
	}
	if _, err := services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, input, input), window); err == nil || !strings.Contains(err.Error(), "map to the same task trace") {
		t.Fatalf("duplicate import error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := services.ImportSessionTraces(canceled, base, source, encodeSessionTraceInputs(t, input), window); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled import error = %v", err)
	}

	for _, fixture := range []struct {
		name string
		make func(string) error
		want string
	}{
		{"symlink", func(target string) error { return os.Symlink("elsewhere", target) }, "is a symlink inside the base"},
		{"directory", func(target string) error { return os.Mkdir(target, core.BaseDirMode) }, "not a regular file"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fixtureBase := newBase(t, sessionTraceConfig, nil)
			fixtureSource, err := fixtureBase.Source("agent-session-traces")
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(fixtureBase.Root(), "tasks", "2026-05-09", "kagglathon-codex-abc-123", "TASKS.md")
			if err := os.MkdirAll(filepath.Dir(target), core.BaseDirMode); err != nil {
				t.Fatal(err)
			}
			if err := fixture.make(target); err != nil {
				t.Fatal(err)
			}
			_, err = services.ImportSessionTraces(t.Context(), fixtureBase, fixtureSource, encodeSessionTraceInputs(t, input), window)
			if err == nil || !strings.Contains(err.Error(), fixture.want) {
				t.Fatalf("unsafe target error = %v, want %q", err, fixture.want)
			}
		})
	}
}

func TestImportSessionTracesPreflightsEveryTargetBeforePublishing(t *testing.T) {
	base := newBase(t, sessionTraceConfig, nil)
	source, err := base.Source("agent-session-traces")
	if err != nil {
		t.Fatal(err)
	}
	first := testSessionTraceInput()
	first.ID, first.SID = "codex:aaa", "aaa"
	blocked := testSessionTraceInput()
	blocked.ID, blocked.SID = "codex:zzz", "zzz"
	blockedTarget := filepath.Join(base.Root(), "tasks", "2026-05-09", "kagglathon-codex-zzz", "TASKS.md")
	if err := os.MkdirAll(blockedTarget, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	_, err = services.ImportSessionTraces(t.Context(), base, source,
		encodeSessionTraceInputs(t, first, blocked),
		sources.Window{Start: "2026-05-09T00:00:00Z", End: "2026-05-10T00:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("unsafe batch error = %v, want target preflight failure", err)
	}
	firstTarget := filepath.Join(base.Root(), "tasks", "2026-05-09", "kagglathon-codex-aaa", "TASKS.md")
	if _, statErr := os.Lstat(firstTarget); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("batch published its first trace before rejecting a later target: %v", statErr)
	}
}
