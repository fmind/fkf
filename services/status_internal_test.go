package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestCheckHelpersUsesTheBoundedBaseReader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := t.TempDir()
	config := internalTestContract + `name: brain
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources: {}
`
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, core.BaseBinDir), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, core.BaseBinDir, hookScript)
	if err := os.WriteFile(hook, make([]byte, core.MaxControlFileBytes+1), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHelpers(t.Context(), base, &Status{}); !errors.Is(err, core.ErrFileTooLarge) {
		t.Fatalf("checkHelpers() error = %v, want core.ErrFileTooLarge", err)
	}
}

func TestVolumeHistoryHonoursCancellation(t *testing.T) {
	base := statusInternalBase(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := volumeHistory(ctx, base); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled volume history error = %v, want context.Canceled", err)
	}
}

func statusInternalBase(t *testing.T) *Base {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := t.TempDir()
	config := internalTestContract + "name: brain\nlayers: {events: true}\nsources: {}\n"
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return base
}
