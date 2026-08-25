package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

var ErrFileTooLarge = errors.New("file exceeds size limit")

const (
	MaxConfigBytes         int64 = 1 << 20
	MaxControlFileBytes    int64 = 1 << 20
	MaxSourceDocumentBytes int64 = 64 << 20
	MaxLocalInputBytes     int64 = 64 << 20
	MaxNarrativeBytes      int64 = 4 << 20
)

// OpenRegularFile opens one regular, non-symlink leaf. Lstat rejects a FIFO or device already
// present before os.Open can block on it, while fstat and SameFile bind subsequent reads to the
// inspected inode. As with the store's other path checks, a hostile local writer can still swap
// the path between syscalls; base writers are expected to use fkf's atomic replacement seam.
func OpenRegularFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s must be a regular non-symlink file", ErrUnsafePath, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect opened %s: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s changed before it was opened as a regular file", ErrUnsafePath, path)
	}
	return file, nil
}

// ReadFileLimit reads one regular, non-symlink file while enforcing a hard byte bound.
// The limit is checked both from metadata and while reading so a concurrently growing
// file cannot force an unbounded allocation.
func ReadFileLimit(path string, limit int64) ([]byte, error) {
	return ReadFileLimitContext(context.Background(), path, limit)
}

// ReadFileLimitContext is ReadFileLimit with cooperative cancellation while bytes are read.
// Regular files do not block indefinitely, but a maximum-sized document can still be large
// enough that an agent cancellation must stop the read before the next audit stage begins.
func ReadFileLimitContext(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("read %s: size limit must be positive", path)
	}
	file, err := OpenRegularFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened %s: %w", path, err)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrFileTooLarge, path, info.Size(), limit)
	}
	data, err := io.ReadAll(&contextReader{ctx: ctx, reader: io.LimitReader(file, limit+1)})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s grew beyond %d bytes", ErrFileTooLarge, path, limit)
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
