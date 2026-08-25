package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFileLimit(path, 5)
	if err != nil || string(data) != "12345" {
		t.Fatalf("ReadFileLimit = %q, %v", data, err)
	}
	if _, err := ReadFileLimit(path, 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversized read error = %v, want ErrFileTooLarge", err)
	}
}

func TestReadFileLimitContextStopsBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadFileLimitContext(ctx, path, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v, want context.Canceled", err)
	}
}

func TestReadFileLimitRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileLimit(link, 1024); err == nil {
		t.Fatal("expected symlink rejection")
	}
}
