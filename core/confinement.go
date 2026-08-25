package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePath identifies a symlink, reparse-like entry, or non-directory component in
// a path that fkf may create beneath. Callers preflight all targets before writing so a
// later failure cannot leave a partially initialized store.
var ErrUnsafePath = errors.New("unsafe filesystem path")

// ValidatePathConfinement accepts an operating-system-managed root alias, then rejects
// every deeper existing symlink and every non-directory intermediate component. A
// missing suffix is valid because MkdirAll may create it after all targets have passed
// the same preflight.
func ValidatePathConfinement(path string) error {
	return validatePathConfinement(path, false)
}

// ValidateDirectoryConfinement additionally requires the leaf to be a real directory
// when it already exists.
func ValidateDirectoryConfinement(path string) error {
	return validatePathConfinement(path, true)
}

func validatePathConfinement(path string, directoryLeaf bool) error {
	path = filepath.Clean(ExpandHome(path))
	if path == "" || path == "." {
		return fmt.Errorf("%w: path is empty", ErrUnsafePath)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: resolve %s: %w", ErrUnsafePath, path, err)
	}
	abs, err = resolveConfinementRootAlias(abs)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	remainder := abs[len(volume):]
	current := volume + string(filepath.Separator)
	for _, component := range splitPathComponents(remainder) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("%w: inspect %s: %w", ErrUnsafePath, current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrUnsafePath, current)
		}
		if (current != abs || directoryLeaf) && !info.IsDir() {
			return fmt.Errorf("%w: intermediate component %s is not a directory", ErrUnsafePath, current)
		}
	}
	return nil
}

// ValidateWithinRoot rejects a symlink on any component strictly between a base's root and
// the leaf it is about to open. It deliberately does not inspect the root itself or anything
// above it: a base legitimately lives behind a symlinked path (`~/brain -> /data/brain`, or
// macOS's /tmp), and the owner chose that. What must not happen is a path *inside* the base —
// committed to git and carried by every clone — pointing back out of it.
//
// The check races a symlink swapped between here and the open that follows. Closing that
// needs every caller to hold an *os.Root, which is a larger change than this seam; the race
// requires write access to the base, whereas the escape this refuses requires only a `git
// pull`.
func ValidateWithinRoot(root, absolute string) error {
	root = filepath.Clean(root)
	absolute = filepath.Clean(absolute)
	if root == absolute {
		return nil
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is outside %s", ErrPathEscapes, absolute, root)
	}
	current := root
	for _, component := range splitPathComponents(relative) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			// A path fkf is about to create. Nothing below an absent component exists yet,
			// so there is nothing left to inspect.
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: inspect %s: %w", ErrUnsafePath, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink inside the base; a base addresses only its own files",
				ErrUnsafePath, current)
		}
	}
	return nil
}

func resolveConfinementRootAlias(abs string) (string, error) {
	volume := filepath.VolumeName(abs)
	components := splitPathComponents(abs[len(volume):])
	if len(components) == 0 {
		return abs, nil
	}
	rootEntry := filepath.Join(volume+string(filepath.Separator), components[0])
	info, err := os.Lstat(rootEntry)
	if errors.Is(err, os.ErrNotExist) {
		return abs, nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: inspect %s: %w", ErrUnsafePath, rootEntry, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return abs, nil
	}
	// Root-level aliases such as macOS /var -> /private/var are controlled by
	// the operating system. Resolve only that privileged component; every
	// caller-controlled component below it is still checked with Lstat.
	resolved, err := filepath.EvalSymlinks(rootEntry)
	if err != nil {
		return "", fmt.Errorf("%w: resolve system root alias %s: %w", ErrUnsafePath, rootEntry, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("%w: system root alias %s is not absolute", ErrUnsafePath, rootEntry)
	}
	return filepath.Join(append([]string{resolved}, components[1:]...)...), nil
}

func splitPathComponents(path string) []string {
	components := make([]string, 0)
	for path != "" && path != string(filepath.Separator) && path != "." {
		dir, base := filepath.Split(path)
		if base != "" {
			components = append(components, base)
		}
		path = filepath.Clean(dir)
	}
	for left, right := 0, len(components)-1; left < right; left, right = left+1, right-1 {
		components[left], components[right] = components[right], components[left]
	}
	return components
}
