package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBaseBusy reports that another fkf process is already mutating the same physical base.
// Readers never acquire this lock; it serializes only whole CLI write workflows.
var ErrBaseBusy = errors.New("base has an active writer")

var errWriterLocked = errors.New("writer lock is held")

// WriterLock is one process's exclusive advisory lock for a physical base.
//
// The empty state file is deliberately persistent. Removing it after unlock creates an inode
// race: a waiter can hold the old inode while a third process locks a newly created file with
// the same name. The kernel lock itself disappears when the descriptor closes or the process
// exits, so a leftover file is never a stale lock.
type WriterLock struct {
	file *os.File
}

// AcquireWriterLock takes the one fail-fast writer lock for root. Its identity follows
// symlinks, including through the deepest existing ancestor of a not-yet-created init target,
// so two spellings of the same base cannot acquire independent locks.
func AcquireWriterLock(ctx context.Context, root string) (*WriterLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, err := physicalWriterRoot(root)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Join(StateDir(), "locks")
	if err := os.MkdirAll(directory, BaseDirMode); err != nil {
		return nil, fmt.Errorf("create writer-lock state: %w", err)
	}
	// State directories can survive upgrades and restored backups with broader permissions.
	// Tighten the leaf on every acquisition, just as the persistent lock inode is tightened.
	if err := os.Chmod(directory, BaseDirMode); err != nil {
		return nil, fmt.Errorf("secure writer-lock state: %w", err)
	}
	digest := sha256.Sum256([]byte(identity))
	lockPath := filepath.Join(directory, hex.EncodeToString(digest[:])+".lock")
	// #nosec G304 G703 -- the filename is a fixed SHA-256 encoding under fkf's state directory.
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, BaseFileMode)
	if err != nil {
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	// An existing state file may predate owner-only modes. Tightening it does not affect the
	// advisory lock and keeps machine-local state from disclosing that a base exists.
	if err := file.Chmod(BaseFileMode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure writer lock: %w", err)
	}
	if err := lockWriterFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errWriterLocked) {
			return nil, fmt.Errorf("%w for %s; retry after the other fkf command finishes", ErrBaseBusy, root)
		}
		return nil, fmt.Errorf("acquire writer lock for %s: %w", root, err)
	}
	return &WriterLock{file: file}, nil
}

// Close releases the advisory lock. It is safe to call more than once.
func (lock *WriterLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	unlockErr := unlockWriterFile(file)
	closeErr := file.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return fmt.Errorf("release writer lock: %w", err)
	}
	return nil
}

func physicalWriterRoot(root string) (string, error) {
	absolute, err := ResolveAbsolutePath(root)
	if err != nil {
		return "", fmt.Errorf("resolve writer-lock base: %w", err)
	}
	current := absolute
	missing := make([]string, 0, 2)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve writer-lock base %s: %w", root, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve writer-lock base %s: no existing ancestor", root)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
