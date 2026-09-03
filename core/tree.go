package core

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SymlinkPolicy decides whether a tree audit rejects links or ignores them without following.
type SymlinkPolicy uint8

const (
	RejectSymlinks SymlinkPolicy = iota
	SkipSymlinks
)

// WalkOwnedTree visits one real directory tree without following or admitting symlinks.
func WalkOwnedTree(
	ctx context.Context,
	root string,
	visit func(path string, entry fs.DirEntry, info fs.FileInfo) error,
) error {
	return WalkTree(ctx, root, RejectSymlinks, visit)
}

// WalkTree is the shared cancellation, error, metadata, and symlink boundary for physical tree
// walks. A caller may skip links only when their presence is not itself unsafe, such as a
// permission report that deliberately audits owned bytes without following external targets.
func WalkTree(
	ctx context.Context,
	root string,
	symlinks SymlinkPolicy,
	visit func(path string, entry fs.DirEntry, info fs.FileInfo) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if symlinks != RejectSymlinks && symlinks != SkipSymlinks {
		return fmt.Errorf("unsupported symlink policy %d", symlinks)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect tree %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: managed tree %s must be a real directory", ErrUnsafePath, root)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("inspect tree entry %s: %w", path, walkErr)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect tree entry %s: %w", path, err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			if symlinks == SkipSymlinks {
				return nil
			}
			return fmt.Errorf("%w: managed tree entry %s is a symlink", ErrUnsafePath, path)
		}
		return visit(path, entry, entryInfo)
	})
}
