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

type markedBlockMarkers struct {
	begin, beginPrefix string
	end, endPrefix     string
}

type markedBlockRegion struct {
	begin   int
	end     int
	present bool
}

func parseMarkedBlockRegion(content string, markers markedBlockMarkers) (markedBlockRegion, error) {
	var begins, ends []int
	noncanonicalBegin, noncanonicalEnd := false, false
	offset := 0
	for _, chunk := range strings.SplitAfter(content, "\n") {
		line := strings.TrimSuffix(chunk, "\n")
		lineEnd := offset + len(line)
		if strings.Contains(line, markers.beginPrefix) {
			if line == markers.begin {
				begins = append(begins, offset)
			} else {
				noncanonicalBegin = true
			}
		}
		if strings.Contains(line, markers.endPrefix) {
			if line == markers.end {
				ends = append(ends, lineEnd)
			} else {
				noncanonicalEnd = true
			}
		}
		offset += len(chunk)
	}
	switch {
	case noncanonicalBegin:
		return markedBlockRegion{}, fmt.Errorf("managed block has a non-canonical begin marker; replace it with %q", markers.begin)
	case noncanonicalEnd:
		return markedBlockRegion{}, fmt.Errorf("managed block has a non-canonical end marker; replace it with %q", markers.end)
	case len(begins) > 1:
		return markedBlockRegion{}, errors.New("managed block has more than one canonical begin marker")
	case len(ends) > 1:
		return markedBlockRegion{}, errors.New("managed block has more than one canonical end marker")
	case len(begins) == 0 && len(ends) == 1:
		return markedBlockRegion{}, fmt.Errorf("managed block end marker %q has no matching begin marker", markers.end)
	case len(begins) == 0:
		return markedBlockRegion{}, nil
	case len(ends) == 0:
		return markedBlockRegion{}, fmt.Errorf("managed block begin marker has no matching end marker %q", markers.end)
	case ends[0] < begins[0]+len(markers.begin):
		return markedBlockRegion{}, fmt.Errorf("managed block end marker %q has no matching begin marker before it", markers.end)
	default:
		return markedBlockRegion{begin: begins[0], end: ends[0], present: true}, nil
	}
}

// replaceMarkedBlock swaps the exact canonical region carried by block and leaves everything
// else exactly as written. Accepting a marker by prefix would silently preserve an obsolete
// command spelling, contrary to the one-generation base contract.
func replaceMarkedBlock(existing, block, defaultHeading string) (string, error) {
	beginMarker, _, ok := strings.Cut(block, "\n")
	if !ok || !strings.HasPrefix(beginMarker, blockMarkerPrefix) {
		return "", errors.New("generated block has no canonical begin marker")
	}
	markers := markedBlockMarkers{
		begin: beginMarker, beginPrefix: blockMarkerPrefix,
		end: blockEndMarker, endPrefix: blockEndMarker,
	}
	generated, err := parseMarkedBlockRegion(block, markers)
	if err != nil || !generated.present || generated.begin != 0 || strings.TrimSpace(block[generated.end:]) != "" {
		return "", errors.New("generated block does not contain exactly one canonical marker pair")
	}
	region, err := parseMarkedBlockRegion(existing, markers)
	if err != nil {
		return "", err
	}
	if !region.present {
		if strings.TrimSpace(existing) == "" {
			existing = defaultHeading
		}
		if !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		return existing + "\n" + block, nil
	}
	tail := strings.TrimPrefix(existing[region.end:], "\n")
	return existing[:region.begin] + block + tail, nil
}
