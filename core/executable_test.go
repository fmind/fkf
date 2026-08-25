package core

import (
	"os"
	"path/filepath"
	"testing"
)

// One resolver, two callers, one answer. `fkf status` and the runner used to scan different
// PATHs, so status could report a helper present that collection could not execute.
func TestLookPathInResolvesAgainstTheGivenPath(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, found := LookPathIn("helper", dir)
	if !found || resolved != script {
		t.Fatalf("LookPathIn() = %q, %v; want the script in the supplied PATH", resolved, found)
	}
	if _, found := LookPathIn("helper", t.TempDir()); found {
		t.Fatal("LookPathIn() found a script that is not on the supplied PATH")
	}
}

// TestLookPathInIgnoresRelativePATHEntries closes the bare-name version of the cwd mismatch.
// Resolving a relative PATH entry before Cmd.Dir changes would approve a file in fkf's cwd and
// then ask the child to execute the same spelling in the base. Base and configured bin paths
// are absolute, so a relative inherited entry can be ignored safely.
func TestLookPathInIgnoresRelativePATHEntries(t *testing.T) {
	processDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "helper"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(processDirectory, directory)
	if err != nil || filepath.IsAbs(relative) {
		t.Fatalf("relative PATH entry = %q, %v", relative, err)
	}
	if resolved, found := LookPathIn("helper", relative); found {
		t.Fatalf("LookPathIn() accepted relative PATH entry as %q", resolved)
	}
}

// A non-executable file is not a command, whatever its name.
func TestLookPathInIgnoresANonExecutableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helper"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found := LookPathIn("helper", dir); found {
		t.Fatal("LookPathIn() accepted a file with no executable bit")
	}
}

// A name carrying a separator is a path already, not something to search for.
func TestLookPathInTreatsASeparatorNameAsAPath(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if resolved, found := LookPathIn(script, ""); !found || resolved != script {
		t.Fatalf("LookPathIn(%q) = %q, %v", script, resolved, found)
	}
}

func TestResolveExecutableNamesWhatItCouldNotFind(t *testing.T) {
	_, err := ResolveExecutable("fkf-no-such-binary", t.TempDir())
	if err == nil {
		t.Fatal("ResolveExecutable() succeeded for a missing binary")
	}
	if got := err.Error(); got == "" {
		t.Fatal("ResolveExecutable() error is empty")
	}
}
