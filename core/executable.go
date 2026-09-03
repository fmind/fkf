package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// There is one executable resolver, and it resolves against the PATH the command will actually
// be given. There used to be two: this file scanned the process PATH (with an undocumented
// `mise which` fallback), while sources.Environment.LookPath scanned the base's PATH — the
// base's own bin/ first, then its declared bin: entries. They disagreed in the direction that
// matters: `fkf status` reported a helper script as present while `sync` or `read --body` then
// could not execute it, and a base could ship a bin/git that sync ran but status never saw.

// LookPathIn resolves a command name against an explicit PATH list, returning the absolute path
// and whether it was found. An empty pathList means the process PATH, which is what the few
// callers with no base in hand — resolving the shell itself — need.
func LookPathIn(name, pathList string) (string, bool) {
	if name == "" {
		return "", false
	}
	// A name carrying a separator is an explicit path, not something to search for. Relative
	// paths are refused: stat would otherwise resolve them from fkf's process directory while
	// exec resolves them after changing to the command's cwd, so the reviewed file and the
	// executed file could differ. Base helpers belong on the trusted PATH and use bare names.
	if strings.ContainsRune(name, os.PathSeparator) {
		if !filepath.IsAbs(name) {
			return "", false
		}
		return name, isExecutableFile(name)
	}
	if strings.TrimSpace(pathList) == "" {
		pathList = os.Getenv("PATH")
	}
	for _, directory := range filepath.SplitList(pathList) {
		// A relative PATH entry has the same split-brain semantics as a relative argv[0]:
		// resolution happens before the child's working directory is applied. Base and declared
		// bin directories are absolute, so ignoring an inherited relative entry is both safer
		// and consistent with the absolute path this function promises to return.
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// SanitizePathList keeps unique absolute entries outside forbiddenRoot.
func SanitizePathList(pathList, forbiddenRoot string) string {
	var root, resolvedRoot string
	if forbiddenRoot != "" {
		root, _ = filepath.Abs(filepath.Clean(forbiddenRoot))
		resolvedRoot, _ = resolveExistingPath(root)
	}
	seen := map[string]bool{}
	directories := make([]string, 0, len(filepath.SplitList(pathList)))
	for _, entry := range filepath.SplitList(pathList) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		directory := filepath.Clean(entry)
		if root != "" {
			absolute, err := filepath.Abs(directory)
			if err != nil || pathIsWithin(root, absolute) {
				continue
			}
			resolved, err := resolveExistingPath(absolute)
			if err != nil || (resolvedRoot != "" && pathIsWithin(resolvedRoot, resolved)) {
				continue
			}
		}
		if !seen[directory] {
			seen[directory] = true
			directories = append(directories, directory)
		}
	}
	return strings.Join(directories, string(os.PathListSeparator))
}

// ResolveExecutable is LookPathIn with the diagnostic a caller about to exec needs.
func ResolveExecutable(name, pathList string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) && !filepath.IsAbs(name) {
		return "", fmt.Errorf("relative executable path %q is not allowed; use a bare name from PATH or an absolute path", name)
	}
	if resolved, found := LookPathIn(name, pathList); found {
		return resolved, nil
	}
	return "", fmt.Errorf("executable %q not found on PATH", name)
}

func isExecutableFile(path string) bool {
	// #nosec G703 -- the path is a PATH entry joined with a command name, and this only Stats
	// it to answer "is there an executable here". Deciding that is the whole job; refusing to
	// look would mean resolving nothing.
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
