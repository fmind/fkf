package services

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

func checkGit(ctx context.Context, base *Base, status *Status) error {
	if !base.Store.Versioned() {
		status.addFinding("git", SeverityWarning,
			"this base is not a git working tree, so nothing versions the wiki, the projects, or the task traces",
			"git init "+shellArg(base.Root()))
		return nil
	}
	tracked, err := trackedPaths(ctx, base.Root())
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		status.addFinding("uncommitted", SeverityWarning,
			"this base is a git tree with no commit, so nothing here is versioned and every audit "+
				"of what git tracks passes by having nothing to look at",
			"git -C "+shellArg(base.Root())+" add -A && git -C "+shellArg(base.Root())+
				" commit -m 'chore: first snapshot'")
		return nil
	}
	var credentials, collected []string
	for _, entry := range tracked {
		if matchesAnyPattern(entry, credentialPatterns) {
			credentials = append(credentials, entry)
		}
		if !status.TrackCollected && matchesAnyLayer(entry, collectedLayers) {
			collected = append(collected, entry)
		}
	}
	if len(credentials) > 0 {
		status.addFinding("tracked-credentials", SeverityError,
			"git tracks files whose whole purpose is to hold a secret; adding a pattern to .gitignore does not untrack them",
			"git rm --cached <path> and rotate the credential", credentials...)
	}
	if len(collected) > 0 {
		status.addFinding("tracked-collected", SeverityError,
			"collected content is ignored by the managed block but is still tracked, so it keeps entering history",
			"git rm -r --cached events index", collected...)
	}
	return nil
}

func trackedPaths(ctx context.Context, root string) ([]string, error) {
	output, err := runGit(ctx, root, gitTimeout,
		"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "ls-files")
	if err != nil {
		return nil, fmt.Errorf("ask git what %s tracks: %w", root, err)
	}
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	return paths, nil
}

func matchesAnyPattern(entry string, patterns []string) bool {
	name := filepath.Base(entry)
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(entry, pattern) || strings.Contains(entry, "/"+pattern) {
				return true
			}
			continue
		}
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func matchesAnyLayer(entry string, layers []string) bool {
	for _, layer := range layers {
		if strings.HasPrefix(entry, layer) {
			return true
		}
	}
	return false
}
