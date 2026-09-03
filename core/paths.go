package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ErrPathEscapes reports a base-relative path that leaves the base root. Escaping paths are
// rejected rather than clamped: silently rewriting `../../etc/passwd` into `etc/passwd`
// turns a hostile link into a plausible one, and a reader cannot tell the difference.
var ErrPathEscapes = errors.New("path escapes the base")

// ExpandHome resolves a leading `~` against the current user's home directory. Every path
// fkf accepts from configuration or the command line goes through it, so `--base ~/brain`
// works in a shell that does not expand tildes (an exec'd argv, an MCP launch line).
func ExpandHome(value string) string {
	value = strings.TrimSpace(value)
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return value
	}
	if value == "~" {
		return home
	}
	return filepath.Join(home, value[2:])
}

// ResolveAbsolutePath expands the one supported home-relative spelling and anchors a relative
// path to the caller's current directory. It deliberately does not evaluate symlinks: the
// chosen root spelling is part of trust identity, while confinement checks inspect symlinks
// separately at the boundary where they matter.
func ResolveAbsolutePath(value string) (string, error) {
	expanded := ExpandHome(value)
	if expanded == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(filepath.Clean(expanded))
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	return absolute, nil
}

// ResolvePhysicalPath gives aliases of the same existing path one stable identity. Missing
// suffixes are preserved so callers can also key state for a path they are about to create.
func ResolvePhysicalPath(value string) (string, error) {
	absolute, err := ResolveAbsolutePath(value)
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("resolve physical path %s: %w", value, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve physical path %s: no existing ancestor", value)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// CleanRelative normalizes one base-relative slash path and refuses anything that could
// address a file outside the base. Backslashes and NUL are rejected outright rather than
// normalized, because both mean the caller is not describing a base-relative URI.
func CleanRelative(relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", fmt.Errorf("%w: path is empty", ErrPathEscapes)
	}
	if strings.ContainsAny(relative, "\x00\\") {
		return "", fmt.Errorf("%w: %q contains an unsafe character", ErrPathEscapes, relative)
	}
	if path.IsAbs(relative) {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathEscapes, relative)
	}
	if strings.HasPrefix(relative, "~") {
		return "", fmt.Errorf("%w: %q is home-relative", ErrPathEscapes, relative)
	}
	trailingSlash := strings.HasSuffix(relative, "/")
	cleaned := path.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q", ErrPathEscapes, relative)
	}
	if cleaned == "." {
		return ".", nil
	}
	if !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("%w: %q is not a valid path", ErrPathEscapes, relative)
	}
	if trailingSlash {
		cleaned += "/"
	}
	return cleaned, nil
}

// ValidateDate checks the YYYY-MM-DD form every dated command and layer path uses.
func ValidateDate(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, value); err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD: %w", err)
	}
	return nil
}
