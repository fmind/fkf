package checks_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCalendarBodyHelperSplitsTheLastSeparator(t *testing.T) {
	bin := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gws-call")
	fakeGWS := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$GWS_CALL_LOG"
printf '%s\n' '{"id":"event-1","description":"Agenda","location":"Room 7","hangoutLink":"https://meet.example.test/one"}'
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-calendar-body.sh")
	command := exec.CommandContext(t.Context(), "sh", script, "team~archive@example.test~event-1")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GWS_CALL_LOG="+callLog,
	)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "Agenda\n\nLocation: Room 7\n\nConference: https://meet.example.test/one\n" {
		t.Fatalf("gws-calendar-body.sh = %q, %v", output, err)
	}
	call, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"calendarId":"team~archive@example.test"`, `"eventId":"event-1"`} {
		if !strings.Contains(string(call), want) {
			t.Errorf("provider call = %q, want %s", call, want)
		}
	}
}

func TestCalendarBodyHelperDoesNotRelayProviderDiagnostics(t *testing.T) {
	bin := t.TempDir()
	fakeGWS := "#!/bin/sh\nprintf '%s\\n' 'private provider diagnostic' >&2\nexit 7\n"
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-calendar-body.sh")
	command := exec.CommandContext(t.Context(), "sh", script, "owner@example.test~event-1")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "cannot fetch the calendar event") ||
		strings.Contains(string(output), "private provider diagnostic") {
		t.Fatalf("provider failure = %q, %v; want only the helper's bounded diagnostic", output, err)
	}
}

func TestCalendarBodyHelperBoundsProviderOutputWhileReading(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to exercise a stubborn provider")
	}
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "gws.pid")
	fakeGWS := `#!` + python + `
import os
import signal
import time

with open(os.environ["GWS_PID_FILE"], "w", encoding="ascii") as stream:
    stream.write(str(os.getpid()))
signal.signal(signal.SIGTERM, signal.SIG_IGN)
signal.signal(signal.SIGPIPE, signal.SIG_IGN)
chunk = b"x" * (1024 * 1024)
while True:
    try:
        os.write(1, chunk)
    except BrokenPipeError:
        while True:
            time.sleep(60)
`
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gws-calendar-body.sh")
	command := exec.CommandContext(ctx, "sh", script, "owner@example.test~event-1")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GWS_PID_FILE="+pidFile,
	)
	output, runErr := command.CombinedOutput()
	pidData, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(pidData))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	provider, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	providerAlive := provider.Signal(syscall.Signal(0)) == nil
	if providerAlive {
		_ = provider.Kill()
	}
	if ctx.Err() != nil {
		t.Fatalf("calendar body helper did not stop the oversized provider: %v", ctx.Err())
	}
	if runErr == nil || !strings.Contains(string(output), "provider response exceeds FKF's 64 MiB command bound") {
		t.Fatalf("oversized provider = %q, %v; want the bounded diagnostic", output, runErr)
	}
	if providerAlive {
		t.Fatal("calendar body helper left the oversized provider running")
	}
}

func TestGmailBodyHelperPrintsPreferredText(t *testing.T) {
	message := "From: sender@example.test\r\nTo: owner@example.test\r\nSubject: Example\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nHello from Gmail.\r\n"
	raw := base64.RawURLEncoding.EncodeToString([]byte(message))
	bin := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "gws-call")
	fakeGWS := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" > "$GWS_CALL_LOG"
printf '%%s\n' '{"id":"message-1","raw":%q}'
`, raw)
	if err := os.WriteFile(filepath.Join(bin, "gws"), []byte(fakeGWS), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-body.py")
	command := exec.CommandContext(t.Context(), "python3", script, "message-1")
	command.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GWS_CALL_LOG="+callLog,
	)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "Hello from Gmail.\n" {
		t.Fatalf("gmail-body.py = %q, %v", output, err)
	}
	call, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(call), `"id":"message-1"`) || !strings.Contains(string(call), `"format":"raw"`) {
		t.Fatalf("provider call = %q, want the exact raw-message request", call)
	}
}

// The provider limit has to be applied while the child is running. Capturing all stdout and
// checking len afterward turns a nominal 64 MiB boundary into unbounded helper memory.
func TestGmailBodyHelperBoundsProviderOutputWhileReading(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "presets", "bin", "gmail-body.py"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if strings.Contains(text, "subprocess.run(") ||
		!strings.Contains(text, ".read(MAX_PROVIDER_BYTES + 1)") {
		t.Fatal("gmail-body.py must read at most the provider limit plus one byte while gws runs")
	}
}
