package services

import (
	"errors"
	"fmt"
	"strings"
)

// A generated Markdown page splits into a hand-curated half and a generated half: one
// comment-delimited block in wiki/index.md regenerates from the base itself, and everything
// outside it belongs to whoever wrote it — the way the `.gitignore` block works.

const (
	blockMarkerPrefix = "<!-- >>> fkf managed block"
	blockEndMarker    = "<!-- <<< fkf managed block -->"
)

// blockBeginMarker names the command a reader should run to regenerate the block, so the
// comment doubles as instructions rather than only a boundary.
func blockBeginMarker(command string) string {
	return blockMarkerPrefix + " — regenerate with `" + command + "`; edits between the markers are lost -->"
}

// replaceMarkedBlock swaps the exact canonical region carried by block and leaves everything
// else exactly as written. Accepting a marker by prefix would silently preserve an obsolete
// command spelling, contrary to the one-generation base contract.
func replaceMarkedBlock(existing, block, defaultHeading string) (string, error) {
	beginMarker, _, ok := strings.Cut(block, "\n")
	if !ok || !strings.HasPrefix(beginMarker, blockMarkerPrefix) {
		return "", errors.New("generated block has no canonical begin marker")
	}
	if strings.Count(block, beginMarker) != 1 || strings.Count(block, blockEndMarker) != 1 {
		return "", errors.New("generated block does not contain exactly one canonical marker pair")
	}
	canonicalBegins := strings.Count(existing, beginMarker)
	allBegins := strings.Count(existing, blockMarkerPrefix)
	ends := strings.Count(existing, blockEndMarker)
	switch {
	case canonicalBegins > 1:
		return "", errors.New("managed block has more than one canonical begin marker")
	case allBegins > canonicalBegins:
		return "", fmt.Errorf("managed block has a non-canonical begin marker; replace it with %q", beginMarker)
	case canonicalBegins == 0 && ends > 0:
		return "", fmt.Errorf("managed block end marker %q has no matching begin marker", blockEndMarker)
	case canonicalBegins == 0:
		if strings.TrimSpace(existing) == "" {
			existing = defaultHeading
		}
		if !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		return existing + "\n" + block, nil
	case ends == 0:
		// With no closing boundary there is no honest way to tell generated text from the
		// owner's narrative. Refuse instead of guessing and deleting everything to EOF.
		return "", fmt.Errorf("managed block begin marker has no matching end marker %q; restore the end marker before regenerating", blockEndMarker)
	case ends > 1:
		return "", errors.New("managed block has more than one end marker")
	}
	begin := strings.Index(existing, beginMarker)
	end := strings.Index(existing, blockEndMarker)
	if end < begin+len(beginMarker) {
		return "", fmt.Errorf("managed block end marker %q has no matching begin marker before it", blockEndMarker)
	}
	tail := strings.TrimPrefix(existing[end+len(blockEndMarker):], "\n")
	return existing[:begin] + block + tail, nil
}
