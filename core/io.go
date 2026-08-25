package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// WriteDataToJSON writes any to file (indent 2) or stdout if path is empty.
// Files are written atomically (temp + rename) so interrupted runs and concurrent
// readers never observe a torn file, with owner-only permissions (0700/0600) because
// the workspace concentrates personal data (emails, chat, shell history) on disk.
func WriteDataToJSON(data any, path string) error {
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	bytes = append(bytes, '\n')

	if path == "" {
		if _, err := os.Stdout.Write(bytes); err != nil {
			return fmt.Errorf("failed to write JSON to stdout: %w", err)
		}
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, baseDirMode); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}
	return writeFileAtomic(dir, path, bytes)
}

// writeFileAtomic writes bytes owner-only, the mode every collected record uses.
func writeFileAtomic(dir, path string, bytes []byte) error {
	return writeFileAtomicMode(dir, path, bytes, baseFileMode)
}

// writeFileAtomicMode writes bytes to a temp file in dir then renames it over path.
// Rename is atomic on the same filesystem, so a reader sees either the old or the
// new complete file, and a crash mid-write never leaves a partial file that
// skip-if-exists would later treat as complete. The mode comes from the target layer's
// visibility so a shared working tree is not written owner-only.
func writeFileAtomicMode(dir, path string, bytes []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed; cleans up on any error

	if _, err := tmp.Write(bytes); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync temp file data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	if err := SyncDirectory(dir); err != nil {
		return fmt.Errorf("failed to sync containing directory after replacing %s: %w", path, err)
	}
	return nil
}

// WriteFileAtomicMode atomically replaces one explicitly named file with the requested
// mode, including file-data and containing-directory synchronization.
func WriteFileAtomicMode(path string, bytes []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, baseDirMode); err != nil {
		return fmt.Errorf("create containing directory: %w", err)
	}
	return writeFileAtomicMode(dir, path, bytes, mode)
}

// SyncDirectory persists a directory-entry change where the platform supports directory
// fsync. Windows does not expose the same primitive through os.File.Sync.
func SyncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	return handle.Sync()
}
