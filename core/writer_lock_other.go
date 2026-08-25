//go:build !unix

package core

import (
	"errors"
	"os"
)

var errWriterUnsupported = errors.New("writer locking is unsupported on this platform")

func lockWriterFile(*os.File) error   { return errWriterUnsupported }
func unlockWriterFile(*os.File) error { return nil }
