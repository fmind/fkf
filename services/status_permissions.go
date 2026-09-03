package services

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/fmind/fkf/core"
)

func checkPermissions(ctx context.Context, base *Base, status *Status) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	walkRoot, err := filepath.EvalSymlinks(base.Root())
	if err != nil {
		return fmt.Errorf("resolve base for permission audit: %w", err)
	}
	executionDirs := []string{
		filepath.Join(walkRoot, core.BaseBinDir),
		filepath.Join(walkRoot, core.BaseTestsDir),
	}
	var wrongMode []string
	err = core.WalkTree(ctx, walkRoot, core.SkipSymlinks, func(current string, entry fs.DirEntry, info fs.FileInfo) error {
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		want := wantFileMode(current, executionDirs, entry.IsDir(), info.Mode())
		if info.Mode().Perm() != want {
			relative, err := filepath.Rel(walkRoot, current)
			if err != nil {
				return err
			}
			wrongMode = append(wrongMode, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(wrongMode) > 0 {
		status.addFinding("permissions", SeverityWarning,
			"files or directories are not owner-only; a base can hold mail and shell-activity metadata",
			permissionRepairCommand(walkRoot), wrongMode...)
	}
	return nil
}

func permissionRepairCommand(root string) string {
	quotedRoot := shellArg(root)
	quotedGit := shellArg(filepath.Join(root, ".git"))
	quotedBin := shellArg(filepath.Join(root, core.BaseBinDir))
	quotedTests := shellArg(filepath.Join(root, core.BaseTestsDir))
	return "chmod 700 " + quotedRoot +
		" && find " + quotedRoot + " -path " + quotedGit + " -prune -o -type d -exec chmod 700 {} +" +
		" && find " + quotedRoot + " -path " + quotedGit + " -prune -o -path " + quotedBin +
		" -prune -o -path " + quotedTests +
		" -prune -o -type f -exec chmod 600 {} +" +
		" && if [ -d " + quotedBin + " ]; then find " + quotedBin +
		` -type f -exec sh -c 'for file do if [ -x "$file" ]; then chmod 700 "$file"; ` +
		`else chmod 600 "$file"; fi; done' sh {} +; fi` +
		" && if [ -d " + quotedTests + " ]; then find " + quotedTests +
		` -type f -exec sh -c 'for file do if [ -x "$file" ]; then chmod 700 "$file"; ` +
		`else chmod 600 "$file"; fi; done' sh {} +; fi`
}

func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func wantFileMode(current string, executionDirs []string, isDir bool, currentMode fs.FileMode) fs.FileMode {
	if isDir {
		return core.BaseDirMode
	}
	for _, directory := range executionDirs {
		if pathBelow(current, directory) && currentMode.Perm()&0o111 != 0 {
			return 0o700
		}
	}
	return core.BaseFileMode
}

func pathBelow(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
