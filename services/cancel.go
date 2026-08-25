package services

import (
	"context"
	"io"
)

// checkContext is the cheap cancellation boundary used inside deterministic file scans. A
// stored document or Markdown page is already size-bounded, so checking before each one keeps
// cancellation latency bounded without making every pure record projection context-aware.
func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(data []byte) (int, error) {
	if err := checkContext(reader.ctx); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}
