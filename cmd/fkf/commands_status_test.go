package main

import (
	"strings"
	"testing"
)

// `--all` was declared, parsed, and passed to the service, which never read it: the flag
// changed neither the text nor the JSON report. An advertised flag that does nothing is worse
// than an absent one, so it is gone and the CLI now says so.
func TestStatusRejectsTheRemovedAllFlag(t *testing.T) {
	for _, flag := range newStatusCommand().Flags {
		for _, name := range flag.Names() {
			if name == "all" {
				t.Fatalf("status still declares --all: %v", flag.Names())
			}
		}
	}

	root := demoBase(t)
	got := invoke(t, "--format", "json", "--base", root, "status", "--all")
	if got.code != ExitInvalidUsage {
		t.Fatalf("status --all exited %d, want %d: %s%s", got.code, ExitInvalidUsage, got.stdout, got.stderr)
	}
	if !strings.Contains(got.stderr, "all") {
		t.Errorf("status --all diagnostic does not name the flag: %q", got.stderr)
	}
}
