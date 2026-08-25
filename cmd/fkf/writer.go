package main

import (
	"context"
	"errors"

	"github.com/fmind/fkf/core"
)

// withWriterLock serializes one complete CLI mutation while leaving every reader lock-free.
// The outer action owns the lock so nested sync/build/init helpers cannot deadlock by trying to
// acquire it again.
func withWriterLock(ctx context.Context, root string, action func() error) (returnErr error) {
	lock, err := core.AcquireWriterLock(ctx, root)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	return action()
}
