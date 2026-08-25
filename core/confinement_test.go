package core

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidatePathConfinementRejectsLinksAndNonDirectoryComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink confinement contract is covered on Linux and macOS")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{link, filepath.Join(link, "child"), dangling, filepath.Join(regular, "child")} {
		if err := ValidateDirectoryConfinement(path); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("path %s error = %v, want ErrUnsafePath", path, err)
		}
	}
	if err := ValidateDirectoryConfinement(filepath.Join(real, "missing", "child")); err != nil {
		t.Fatalf("safe missing suffix = %v", err)
	}
}

func TestValidatePathConfinementAcceptsMacOSSystemTempAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS root aliases require a macOS filesystem")
	}
	path := t.TempDir()
	if err := ValidateDirectoryConfinement(path); err != nil {
		t.Fatalf("system temporary directory %s: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectoryConfinement(resolved); err != nil {
		t.Fatalf("resolved temporary directory %s: %v", resolved, err)
	}
}

func TestValidatePathConfinementAcceptsMissingRootSuffix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("root path syntax differs on Windows")
	}
	path := filepath.Join(string(filepath.Separator), "fkf-confinement-path-that-does-not-exist", "child")
	if err := ValidatePathConfinement(path); err != nil {
		t.Fatalf("missing root suffix %s: %v", path, err)
	}
}
