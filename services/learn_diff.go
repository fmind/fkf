package services

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
)

const (
	maxLearnProposalBytes = 1 << 20
	maxLearnPatchFiles    = 32
	maxLearnPatchHunks    = 256
)

var learnHunkPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: .*)?$`)

type learnPatchLine struct {
	kind byte
	text string
}

type learnPatchHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []learnPatchLine
}

type learnFilePatch struct {
	URI   string
	New   bool
	Hunks []learnPatchHunk
}

func parseLearnDiff(data []byte) ([]learnFilePatch, error) {
	if len(data) == 0 {
		return nil, learnProposalError("diff is empty")
	}
	if len(data) > maxLearnProposalBytes {
		return nil, learnProposalError("diff is %d bytes; limit is %d", len(data), maxLearnProposalBytes)
	}
	if !utf8.Valid(data) {
		return nil, learnProposalError("diff is not valid UTF-8")
	}
	if strings.ContainsRune(string(data), '\x00') || strings.ContainsRune(string(data), '\r') {
		return nil, learnProposalError("diff must use UTF-8 and LF line endings without NUL bytes")
	}
	if data[len(data)-1] != '\n' {
		return nil, learnProposalError("diff must end with a newline")
	}

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	patches := make([]learnFilePatch, 0, 1)
	seen := map[string]bool{}
	for cursor := 0; cursor < len(lines); {
		if lines[cursor] == "" {
			cursor++
			continue
		}
		patch, next, err := parseLearnFile(lines, cursor)
		if err != nil {
			return nil, err
		}
		if seen[patch.URI] {
			return nil, learnProposalError("diff repeats target %s", patch.URI)
		}
		seen[patch.URI] = true
		patches = append(patches, patch)
		if len(patches) > maxLearnPatchFiles {
			return nil, learnProposalError("diff changes more than %d files", maxLearnPatchFiles)
		}
		cursor = next
	}
	if len(patches) == 0 {
		return nil, learnProposalError("diff changes no files")
	}
	return patches, nil
}

func parseLearnFile(lines []string, cursor int) (learnFilePatch, int, error) {
	if !strings.HasPrefix(lines[cursor], "--- ") {
		return learnFilePatch{}, cursor, learnProposalError("line %d: expected an old-file header beginning `--- `", cursor+1)
	}
	oldPath, err := learnHeaderPath(lines[cursor], "--- ")
	if err != nil {
		return learnFilePatch{}, cursor, learnProposalError("line %d: %v", cursor+1, err)
	}
	cursor++
	if cursor >= len(lines) || !strings.HasPrefix(lines[cursor], "+++ ") {
		return learnFilePatch{}, cursor, learnProposalError("line %d: expected a new-file header beginning `+++ `", cursor+1)
	}
	newPath, err := learnHeaderPath(lines[cursor], "+++ ")
	if err != nil {
		return learnFilePatch{}, cursor, learnProposalError("line %d: %v", cursor+1, err)
	}
	uri, create, err := learnPatchTarget(oldPath, newPath)
	if err != nil {
		return learnFilePatch{}, cursor, err
	}
	patch := learnFilePatch{URI: uri, New: create}
	for cursor++; cursor < len(lines) && strings.HasPrefix(lines[cursor], "@@ "); {
		hunk, next, err := parseLearnHunk(lines, cursor)
		if err != nil {
			return learnFilePatch{}, cursor, err
		}
		patch.Hunks = append(patch.Hunks, hunk)
		if len(patch.Hunks) > maxLearnPatchHunks {
			return learnFilePatch{}, cursor, learnProposalError("%s has more than %d hunks", uri, maxLearnPatchHunks)
		}
		cursor = next
	}
	if len(patch.Hunks) == 0 {
		return learnFilePatch{}, cursor, learnProposalError("%s has no hunks", uri)
	}
	return patch, cursor, nil
}

func learnHeaderPath(line, prefix string) (string, error) {
	value := strings.TrimPrefix(line, prefix)
	if index := strings.IndexAny(value, "\t "); index >= 0 {
		value = value[:index]
	}
	if value == "" {
		return "", errors.New("file header has no path")
	}
	return value, nil
}

func learnPatchTarget(oldPath, newPath string) (string, bool, error) {
	if newPath == "/dev/null" {
		return "", false, learnProposalError("page deletion is not supported")
	}
	newURI, err := normalizeLearnPatchPath(newPath, "b/")
	if err != nil {
		return "", false, err
	}
	create := oldPath == "/dev/null"
	if !create {
		oldURI, err := normalizeLearnPatchPath(oldPath, "a/")
		if err != nil {
			return "", false, err
		}
		if oldURI != newURI {
			return "", false, learnProposalError("renames are not supported: %s becomes %s", oldURI, newURI)
		}
	}
	parts := strings.Split(newURI, "/")
	if len(parts) != 2 || (parts[0] != string(core.LayerWiki) && parts[0] != string(core.LayerProjects)) ||
		!strings.HasSuffix(parts[1], core.MarkdownExtension) {
		return "", false, learnProposalError("target %q must be one flat wiki/*.md or projects/*.md page", newURI)
	}
	return newURI, create, nil
}

func normalizeLearnPatchPath(value, prefix string) (string, error) {
	if !strings.HasPrefix(value, prefix) {
		return "", learnProposalError("path %q must begin %s", value, prefix)
	}
	value = strings.TrimPrefix(value, prefix)
	cleaned, err := core.CleanRelative(value)
	if err != nil {
		return "", learnProposalError("path %q: %v", value, err)
	}
	if cleaned != value || strings.ContainsRune(value, '\\') {
		return "", learnProposalError("path %q is not canonical", value)
	}
	return value, nil
}

func parseLearnHunk(lines []string, cursor int) (learnPatchHunk, int, error) {
	match := learnHunkPattern.FindStringSubmatch(lines[cursor])
	if match == nil {
		return learnPatchHunk{}, cursor, learnProposalError("line %d: malformed hunk header", cursor+1)
	}
	oldStart, _ := strconv.Atoi(match[1])
	newStart, _ := strconv.Atoi(match[3])
	oldCount, err := learnHunkCount(match[2])
	if err != nil {
		return learnPatchHunk{}, cursor, learnProposalError("line %d: old hunk count: %v", cursor+1, err)
	}
	newCount, err := learnHunkCount(match[4])
	if err != nil {
		return learnPatchHunk{}, cursor, learnProposalError("line %d: new hunk count: %v", cursor+1, err)
	}
	if match[2] == "" {
		oldCount = 1
	}
	if match[4] == "" {
		newCount = 1
	}
	hunk := learnPatchHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	oldSeen, newSeen := 0, 0
	for cursor++; cursor < len(lines) && (oldSeen < oldCount || newSeen < newCount); cursor++ {
		line := lines[cursor]
		if line == "" {
			return learnPatchHunk{}, cursor, learnProposalError("line %d: hunk line has no prefix", cursor+1)
		}
		kind := line[0]
		switch kind {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
		case '+':
			newSeen++
		default:
			return learnPatchHunk{}, cursor, learnProposalError("line %d: hunk line must begin space, +, or -", cursor+1)
		}
		if oldSeen > oldCount || newSeen > newCount {
			return learnPatchHunk{}, cursor, learnProposalError("line %d: hunk exceeds its declared counts", cursor+1)
		}
		hunk.lines = append(hunk.lines, learnPatchLine{kind: kind, text: line[1:]})
	}
	if oldSeen != oldCount || newSeen != newCount {
		return learnPatchHunk{}, cursor, learnProposalError("hunk declares -%d,+%d lines but contains -%d,+%d", oldCount, newCount, oldSeen, newSeen)
	}
	return hunk, cursor, nil
}

func learnHunkCount(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0, errors.New("not a non-negative integer")
	}
	return count, nil
}

func applyLearnFilePatch(original []byte, patch learnFilePatch) ([]byte, error) {
	old, err := learnTextLines(original)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", patch.URI, err)
	}
	result := make([]string, 0, len(old))
	cursor := 0
	for index, hunk := range patch.Hunks {
		oldPosition := hunk.oldStart - 1
		if hunk.oldCount == 0 {
			oldPosition = hunk.oldStart
		}
		newPosition := hunk.newStart - 1
		if hunk.newCount == 0 {
			newPosition = hunk.newStart
		}
		if oldPosition < cursor || oldPosition > len(old) {
			return nil, learnProposalError("%s hunk %d old position %d is outside or overlaps the file", patch.URI, index+1, hunk.oldStart)
		}
		result = append(result, old[cursor:oldPosition]...)
		if len(result) != newPosition {
			return nil, learnProposalError("%s hunk %d new position %d does not follow the preceding hunks", patch.URI, index+1, hunk.newStart)
		}
		position := oldPosition
		for _, line := range hunk.lines {
			switch line.kind {
			case ' ':
				if position >= len(old) || old[position] != line.text {
					return nil, learnProposalError("%s hunk %d context does not match line %d", patch.URI, index+1, position+1)
				}
				result = append(result, line.text)
				position++
			case '-':
				if position >= len(old) || old[position] != line.text {
					return nil, learnProposalError("%s hunk %d removal does not match line %d", patch.URI, index+1, position+1)
				}
				position++
			case '+':
				result = append(result, line.text)
			}
		}
		cursor = position
	}
	result = append(result, old[cursor:]...)
	if len(result) == 0 {
		return []byte{}, nil
	}
	return []byte(strings.Join(result, "\n") + "\n"), nil
}

func learnTextLines(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if !utf8.Valid(data) {
		return nil, errors.New("page is not valid UTF-8")
	}
	if data[len(data)-1] != '\n' {
		return nil, errors.New("page must end with a newline before a unified diff can update it")
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

func renderLearnDiff(uri string, oldData, newData []byte) ([]byte, error) {
	oldLines, err := learnTextLines(oldData)
	if err != nil {
		return nil, err
	}
	newLines, err := learnTextLines(newData)
	if err != nil {
		return nil, err
	}
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	if prefix == len(oldLines) && prefix == len(newLines) {
		return nil, nil
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	start := max(0, prefix-3)
	oldEnd := min(len(oldLines), len(oldLines)-suffix+3)
	newEnd := min(len(newLines), len(newLines)-suffix+3)
	oldCount, newCount := oldEnd-start, newEnd-start
	oldStart, newStart := start+1, start+1
	if oldCount == 0 {
		oldStart = start
	}
	if newCount == 0 {
		newStart = start
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", uri, uri, oldStart, oldCount, newStart, newCount)
	for _, line := range oldLines[start:prefix] {
		fmt.Fprintf(&builder, " %s\n", line)
	}
	for _, line := range oldLines[prefix : len(oldLines)-suffix] {
		fmt.Fprintf(&builder, "-%s\n", line)
	}
	for _, line := range newLines[prefix : len(newLines)-suffix] {
		fmt.Fprintf(&builder, "+%s\n", line)
	}
	for _, line := range oldLines[len(oldLines)-suffix : oldEnd] {
		fmt.Fprintf(&builder, " %s\n", line)
	}
	data := []byte(builder.String())
	if len(data) > maxLearnProposalBytes {
		return nil, learnProposalError("generated diff is %d bytes; limit is %d", len(data), maxLearnProposalBytes)
	}
	return data, nil
}

func learnProposalError(format string, args ...any) error {
	return fmt.Errorf("%w: learn proposal: %s", core.ErrConfig, fmt.Sprintf(format, args...))
}
