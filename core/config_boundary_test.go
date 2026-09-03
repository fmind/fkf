package core

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestEveryConfigReaderRefusesUnsafeLeaves keeps the single configuration boundary closed. A
// symlink can swap command bytes outside the base, while a FIFO can block a read forever.
func TestEveryConfigReaderRefusesUnsafeLeaves(t *testing.T) {
	for _, name := range []string{ConfigFileName, LocalConfigName} {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				assertUnsafeConfigReaders(t, unsafeConfigBase(t, name, kind))
			})
		}
	}
}

func unsafeConfigBase(t *testing.T, name, kind string) string {
	t.Helper()
	root := writeBase(t, minimalConfig, nil)
	leaf := filepath.Join(root, name)
	if name == ConfigFileName {
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
	}
	switch kind {
	case "symlink":
		outside := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(outside, []byte(minimalConfig), BaseFileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, leaf); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
	case "fifo":
		if err := syscall.Mkfifo(leaf, uint32(BaseFileMode)); err != nil {
			t.Skipf("FIFOs are unavailable: %v", err)
		}
	}
	return root
}

func assertUnsafeConfigReaders(t *testing.T, root string) {
	t.Helper()
	if _, err := LoadConfig(root); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("LoadConfig() error = %v, want ErrUnsafePath", err)
	}
}

// TestConfigReadersPreserveASymlinkSpelledBaseRoot distinguishes an unsafe link inside a
// base from the root spelling the operator deliberately chose. Trust identity and config
// origins retain that spelling; only leaves below it must be real files.
func TestConfigReadersPreserveASymlinkSpelledBaseRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	machineBin := t.TempDir()
	realRoot := writeBase(t, minimalConfig, map[string]string{
		LocalConfigName: "bin: [" + machineBin + "]\n",
	})
	linkRoot := filepath.Join(t.TempDir(), "brain")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	config, err := LoadConfig(linkRoot)
	if err != nil {
		t.Fatalf("LoadConfig() through a symlink-spelled root: %v", err)
	}
	if config.Path != filepath.Join(linkRoot, ConfigFileName) ||
		config.LocalPath != filepath.Join(linkRoot, LocalConfigName) {
		t.Fatalf("config paths = %q and %q, want the chosen root spelling %q",
			config.Path, config.LocalPath, linkRoot)
	}
	if _, err := ConfigDigest(t.Context(), config); err != nil {
		t.Fatalf("ConfigDigest(t.Context(), ) through a symlink-spelled root: %v", err)
	}
	if _, err := TrustItems(t.Context(), config); err != nil {
		t.Fatalf("TrustItems(t.Context(), ) through a symlink-spelled root: %v", err)
	}
	if _, err := WriteTrust(t.Context(), config, time.Now()); err != nil {
		t.Fatalf("WriteTrust(t.Context(), ) through a symlink-spelled root: %v", err)
	}
	if err := RequireTrust(t.Context(), config); err != nil {
		t.Fatalf("RequireTrust(t.Context(), ) through a symlink-spelled root: %v", err)
	}
}
