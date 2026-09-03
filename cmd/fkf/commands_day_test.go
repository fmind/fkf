package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestDayCommandRendersTheCanonicalTextAndJSON(t *testing.T) {
	root := commandDigestBase(t)
	for _, test := range []struct {
		format string
		check  func(*testing.T, string)
	}{
		{format: "text", check: func(t *testing.T, output string) {
			if !strings.Contains(output, "[shell-commands] 6 records summarized") ||
				!strings.Contains(output, "receipt: 2026-05-09..2026-05-09") {
				t.Fatalf("day text = %q", output)
			}
			assertTextDigestAccounting(t, output, 600)
		}},
		{format: "json", check: func(t *testing.T, output string) {
			report := decodeDigestReport(t, output)
			if report.Receipt.Records != 36 || report.Receipt.Budget != 600 {
				t.Fatalf("day receipt = %+v", report.Receipt)
			}
			assertStructuredDigestAccounting(t, output, report, services.DigestDeliveryJSON, 600)
		}},
		{format: "jsonl", check: func(t *testing.T, output string) {
			report := decodeDigestReport(t, output)
			if strings.Count(output, "\n") != 1 {
				t.Fatalf("day JSONL = %q, want one compact report line", output)
			}
			assertStructuredDigestAccounting(t, output, report, services.DigestDeliveryJSONL, 600)
		}},
	} {
		t.Run(test.format, func(t *testing.T) {
			stdout, stderr, err := runDigestCommand(t, root,
				"--format", test.format, "day", "2026-05-09", "--budget", "600")
			if err != nil {
				t.Fatalf("day command: %v; stderr=%q", err, stderr)
			}
			test.check(t, stdout)
		})
	}
}

func decodeDigestReport(t *testing.T, output string) services.TimelineReport {
	t.Helper()
	var report services.TimelineReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("day JSON = %q: %v", output, err)
	}
	return report
}

func assertStructuredDigestAccounting(
	t *testing.T,
	output string,
	report services.TimelineReport,
	format string,
	budget int,
) {
	t.Helper()
	if report.Receipt.Format != format {
		t.Fatalf("receipt format = %q, want %q", report.Receipt.Format, format)
	}
	if len(output) > budget*4 {
		t.Fatalf("%s output = %d bytes, over %d-byte budget", format, len(output), budget*4)
	}
	if want := (len(output) + 3) / 4; report.Receipt.UsedTokens != want {
		t.Fatalf("%s used_tokens = %d, want %d for %d delivered bytes",
			format, report.Receipt.UsedTokens, want, len(output))
	}
}

func assertTextDigestAccounting(t *testing.T, output string, budget int) {
	t.Helper()
	if len(output) > budget*4 {
		t.Fatalf("text output = %d bytes, over %d-byte budget", len(output), budget*4)
	}
	needle := "receipt: budget 600 · used "
	start := strings.Index(output, needle)
	if start < 0 {
		t.Fatalf("text output has no accounting receipt: %q", output)
	}
	var used int
	if _, err := fmt.Sscanf(output[start:], "receipt: budget 600 · used %d", &used); err != nil {
		t.Fatalf("parse text accounting receipt: %v", err)
	}
	if want := (len(output) + 3) / 4; used != want {
		t.Fatalf("text used_tokens = %d, want %d for %d delivered bytes", used, want, len(output))
	}
}

func TestTimelineCommandAcceptsRangeFiltersAndAround(t *testing.T) {
	root := commandDigestBase(t)
	stdout, stderr, err := runDigestCommand(t, root, "--format", "json", "timeline",
		"--since", "2026-05-08", "--until", "2026-05-09",
		"--source", "git-commits", "--repo", "repo:github.com/fmind/fkf", "--all", "--budget", "1200")
	if err != nil {
		t.Fatalf("timeline range: %v; stderr=%q", err, stderr)
	}
	var report services.TimelineReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) == 0 || report.Receipt.Repository != "repo:github.com/fmind/fkf" {
		t.Fatalf("timeline = %+v", report)
	}
	uri := report.Groups[0].Items[0].URI
	stdout, stderr, err = runDigestCommand(t, root, "--format", "json", "timeline", uri, "--around", "2h", "--budget", "1200")
	if err != nil {
		t.Fatalf("timeline around: %v; stderr=%q", err, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Receipt.Around != uri || report.Receipt.AroundWindow != "2h0m0s" {
		t.Fatalf("around receipt = %+v", report.Receipt)
	}
}

func runDigestCommand(t *testing.T, root string, arguments ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := newApp(&stdout, &stderr)
	app.Commands = append(app.Commands, newDayCommand(), newTimelineCommand())
	args := append([]string{"fkf", "--base", root}, arguments...)
	err := app.Run(context.Background(), args)
	return stdout.String(), stderr.String(), err
}

func commandDigestBase(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := filepath.Join(t.TempDir(), "brain")
	now := func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }
	if _, err := services.Init(t.Context(), services.InitRequest{Path: root, Demo: 2, SkipGit: true}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, core.ConfigFileName)); err != nil {
		t.Fatal(err)
	}
	return root
}
