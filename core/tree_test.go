package core

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkOwnedTreeRefusesLinksAndHonoursCancellation(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := WalkOwnedTree(t.Context(), root, func(string, fs.DirEntry, fs.FileInfo) error {
		return nil
	}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("WalkOwnedTree() error = %v, want ErrUnsafePath", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := WalkOwnedTree(cancelled, root, func(string, fs.DirEntry, fs.FileInfo) error {
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled WalkOwnedTree() error = %v, want context.Canceled", err)
	}
}

func TestWalkTreeCanSkipLinksWithoutFollowingThem(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside"), BaseFileMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	var visited []string
	if err := WalkTree(t.Context(), root, SkipSymlinks, func(path string, _ fs.DirEntry, _ fs.FileInfo) error {
		visited = append(visited, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range visited {
		if path == link {
			t.Fatalf("skipped link %s was visited", link)
		}
	}
}
