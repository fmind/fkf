package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectHarnessesReportsOnlyCompleteIntegrations(t *testing.T) {
	baseRoot := makeHarnessBase(t)
	home := t.TempDir()
	before, err := InspectHarnesses(t.Context(), baseRoot, home, testHarnessExecutable)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range before {
		if entry.Registered || entry.Changes == 0 {
			t.Fatalf("empty home registration = %+v, want missing integration", entry)
		}
	}
	if _, err := InstallHarnesses(t.Context(), baseRoot, HarnessInstallRequest{
		Names: []string{"claude"}, Home: home, Executable: testHarnessExecutable,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := InspectHarnesses(t.Context(), baseRoot, home, testHarnessExecutable)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range after {
		if entry.Name == "claude" {
			if !entry.Registered || entry.Changes != 0 || entry.Error != "" {
				t.Fatalf("claude registration = %+v, want current", entry)
			}
			return
		}
	}
	t.Fatal("claude is absent from the closed harness vocabulary")
}

func TestInspectHarnessesReportsUnscopedSingletonsWithoutDeletingThem(t *testing.T) {
	baseRoot := makeHarnessBase(t)
	home := t.TempDir()
	unscoped := []byte(`{"mcpServers":{"fkf":{"command":"/usr/local/bin/fkf","args":["mcp","serve","--base","/old/base"]}}}`)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, unscoped, 0o600); err != nil {
		t.Fatal(err)
	}
	registrations, err := InspectHarnesses(t.Context(), baseRoot, home, testHarnessExecutable)
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if registration.Name == "claude" {
			if len(registration.Cleanup) != 1 || registration.Cleanup[0] != "~/.claude.json#mcpServers.fkf" {
				t.Fatalf("manual cleanup = %v", registration.Cleanup)
			}
			current, readErr := os.ReadFile(path)
			if readErr != nil || string(current) != string(unscoped) {
				t.Fatalf("inspection mutated unscoped config: %q, %v", current, readErr)
			}
			return
		}
	}
	t.Fatal("claude registration missing")
}

func TestInspectHarnessesHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectHarnesses(ctx, t.TempDir(), t.TempDir(), testHarnessExecutable); !errors.Is(err, context.Canceled) {
		t.Fatalf("InspectHarnesses() error = %v, want context.Canceled", err)
	}
}
