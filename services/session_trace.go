package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

const (
	// A busy multi-harness base can produce hundreds of sessions in one completed day.
	// The subprocess 64 MiB ceiling remains the authoritative aggregate byte bound.
	maxSessionTracesPerRun  = 1024
	maxSessionTraceRequests = 20
	maxSessionTraceFiles    = 200
	maxSessionTraceCommands = 20
	maxSessionTraceExcerpt  = 6000
	maxSessionTracePath     = 4096
	maxSessionTraceScalar   = 256
	taskTraceSyncRelative   = ".agents/tmp/sync/tasks"
)

var (
	sessionHarnessPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	sessionRepoPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// SessionTraceInput is the closed, bounded output contract of agent-session-trace.sh. Unlike
// ordinary sources it is not durable provider JSON: sync renders it immediately into the tasks
// layer, and only the generated Markdown becomes base evidence.
type SessionTraceInput struct {
	ID            string   `json:"id"`
	Harness       string   `json:"harness"`
	SID           string   `json:"sid"`
	FirstAt       string   `json:"first_at"`
	LastAt        string   `json:"last_at"`
	CWD           string   `json:"cwd,omitempty"`
	Repo          string   `json:"repo,omitempty"`
	Model         string   `json:"model,omitempty"`
	Requests      []string `json:"requests"`
	Files         []string `json:"files"`
	Verification  []string `json:"verification"`
	LastAssistant string   `json:"last_assistant,omitempty"`
}

// SessionTraceWrite reports how many task skeletons were created and how many existing traces
// were left untouched. A generated skeleton is a starting point the owner may annotate, so a
// later store snapshot never overwrites it.
type SessionTraceWrite struct {
	Written  int      `json:"written"`
	Existing int      `json:"existing"`
	Paths    []string `json:"paths"`
	created  map[string][sha256.Size]byte
}

// ImportSessionTraces validates one helper result completely before writing any skeleton.
func ImportSessionTraces(
	ctx context.Context, base *Base, source *core.Source, stdout string, window sources.Window,
) (*SessionTraceWrite, error) {
	if source.Layer != core.LayerTasks {
		return nil, fmt.Errorf("source %s is %s, not tasks", source.Name, source.Layer)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	inputs, err := decodeSessionTraceInputs(stdout)
	if err != nil {
		return nil, fmt.Errorf("source %s session traces: %w", source.Name, err)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	prepared := make([]preparedSessionTrace, 0, len(inputs))
	seen := make(map[string]string, len(inputs))
	for index, input := range inputs {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		trace, err := prepareSessionTrace(base, input, window)
		if err != nil {
			return nil, fmt.Errorf("source %s session trace %d: %w", source.Name, index, err)
		}
		if previous, duplicate := seen[trace.URI]; duplicate {
			return nil, fmt.Errorf("source %s sessions %s and %s map to the same task trace %s",
				source.Name, previous, input.ID, trace.URI)
		}
		seen[trace.URI] = input.ID
		prepared = append(prepared, trace)
	}
	slices.SortFunc(prepared, func(left, right preparedSessionTrace) int {
		return strings.Compare(left.URI, right.URI)
	})

	result := &SessionTraceWrite{Paths: []string{}, created: make(map[string][sha256.Size]byte)}
	targets := make([]sessionTraceTarget, 0, len(prepared))
	for _, trace := range prepared {
		absolute, err := base.Store.Resolve(trace.URI)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(absolute)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("inspect task trace %s: %w", trace.URI, err)
		case info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("refusing task-trace symlink %s", trace.URI)
		case !info.Mode().IsRegular():
			return nil, fmt.Errorf("task trace %s is not a regular file", trace.URI)
		default:
			result.Existing++
			continue
		}
		targets = append(targets, sessionTraceTarget{trace: trace, absolute: absolute})
	}
	if err := publishSessionTraceBatch(ctx, targets); err != nil {
		return nil, err
	}
	result.Written = len(targets)
	for _, target := range targets {
		result.Paths = append(result.Paths, target.trace.URI)
		result.created[target.trace.URI] = sha256.Sum256(target.trace.Content)
	}
	return result, nil
}

// taskTraceRangeDue keeps opportunistic hourly sync cheap even when a completed day contained
// no agent session. The marker is ignored planner state under .agents/tmp, not evidence: deleting
// it merely re-runs the trust-gated helper, whose trace writes remain idempotent.
func taskTraceRangeDue(base *Base, source string, dates []string) (bool, error) {
	return taskTraceRangeDueAt(base, source, dates, base.Now().Location())
}

func taskTraceRangeDueAt(base *Base, source string, dates []string, location *time.Location) (bool, error) {
	if len(dates) == 0 {
		return false, nil
	}
	directory := filepath.Join(base.Root(), filepath.FromSlash(taskTraceSyncRelative), source)
	if err := core.ValidateWithinRoot(base.Root(), directory); err != nil {
		return false, err
	}
	for _, date := range dates {
		marker := filepath.Join(directory, date+".done")
		expected, err := taskTraceMarkerContent(source, date, location)
		if err != nil {
			return false, err
		}
		info, err := os.Lstat(marker)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("inspect tasks sync marker for %s/%s: %w", source, date, err)
		case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
			return false, fmt.Errorf("tasks sync marker for %s/%s is not a regular non-symlink file", source, date)
		}
		content, err := core.ReadFileLimit(marker, core.MaxControlFileBytes)
		if err != nil {
			return false, fmt.Errorf("read tasks sync marker for %s/%s: %w", source, date, err)
		}
		if !bytes.Equal(content, expected) {
			return true, nil
		}
	}
	return false, nil
}

type taskTraceMarker struct {
	Source string `json:"source"`
	Date   string `json:"date"`
	Start  string `json:"start"`
	End    string `json:"end"`
}

func taskTraceMarkerContent(source, date string, location *time.Location) ([]byte, error) {
	day, err := sources.ParseDayInLocation(date, location)
	if err != nil {
		return nil, err
	}
	window := sources.DayWindow(day)
	content, err := json.Marshal(taskTraceMarker{
		Source: source, Date: date, Start: window.Start, End: window.End,
	})
	if err != nil {
		return nil, fmt.Errorf("encode tasks sync marker for %s/%s: %w", source, date, err)
	}
	return append(content, '\n'), nil
}

type taskTraceMarkerSnapshot struct {
	path   string
	exists bool
	data   []byte
	mode   os.FileMode
}

func markTaskTraceRange(
	ctx context.Context, base *Base, source string, dates []string, location *time.Location,
) error {
	contents := make([][]byte, len(dates))
	for index, date := range dates {
		if err := checkContext(ctx); err != nil {
			return err
		}
		content, err := taskTraceMarkerContent(source, date, location)
		if err != nil {
			return err
		}
		contents[index] = content
	}
	directory := base.Root()
	for _, component := range []string{".agents", "tmp", "sync", "tasks", source} {
		if err := checkContext(ctx); err != nil {
			return err
		}
		directory = filepath.Join(directory, component)
		if err := ensureSessionTraceDirectory(base.Root(), directory); err != nil {
			return err
		}
	}
	snapshots := make([]taskTraceMarkerSnapshot, 0, len(dates))
	for _, date := range dates {
		marker := filepath.Join(directory, date+".done")
		snapshot, err := snapshotTaskTraceMarker(marker, source, date)
		if err != nil {
			return err
		}
		snapshots = append(snapshots, snapshot)
	}
	for index, date := range dates {
		if err := checkContext(ctx); err != nil {
			return errors.Join(err, restoreTaskTraceMarkers(snapshots))
		}
		marker := snapshots[index].path
		if err := core.WriteFileAtomicMode(marker, contents[index], core.BaseFileMode); err != nil {
			cause := fmt.Errorf("write tasks sync marker for %s/%s: %w", source, date, err)
			return errors.Join(cause, restoreTaskTraceMarkers(snapshots))
		}
	}
	return nil
}

func snapshotTaskTraceMarker(marker, source, date string) (taskTraceMarkerSnapshot, error) {
	snapshot := taskTraceMarkerSnapshot{path: marker, mode: core.BaseFileMode}
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("inspect tasks sync marker for %s/%s: %w", source, date, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return snapshot, fmt.Errorf("tasks sync marker for %s/%s is not a regular non-symlink file", source, date)
	}
	data, err := core.ReadFileLimit(marker, core.MaxControlFileBytes)
	if err != nil {
		return snapshot, fmt.Errorf("read tasks sync marker for %s/%s: %w", source, date, err)
	}
	snapshot.exists, snapshot.data, snapshot.mode = true, data, info.Mode().Perm()
	return snapshot, nil
}

func restoreTaskTraceMarkers(snapshots []taskTraceMarkerSnapshot) error {
	var problems []error
	directories := map[string]struct{}{}
	for _, snapshot := range snapshots {
		directories[filepath.Dir(snapshot.path)] = struct{}{}
		if snapshot.exists {
			if err := core.WriteFileAtomicMode(snapshot.path, snapshot.data, snapshot.mode); err != nil {
				problems = append(problems, fmt.Errorf("restore tasks sync marker %s: %w", snapshot.path, err))
			}
			continue
		}
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove tasks sync marker %s: %w", snapshot.path, err))
		}
	}
	for directory := range directories {
		if err := core.SyncDirectory(directory); err != nil {
			problems = append(problems, fmt.Errorf("sync restored tasks marker directory %s: %w", directory, err))
		}
	}
	return errors.Join(problems...)
}

func rollbackImportedSessionTraces(base *Base, result *SessionTraceWrite) error {
	if result == nil || result.Written == 0 {
		return nil
	}
	var problems []error
	directories := map[string]struct{}{}
	for _, uri := range result.Paths {
		expected, created := result.created[uri]
		absolute, err := base.Store.Resolve(uri)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		data, err := core.ReadFileLimit(absolute, core.MaxNarrativeBytes)
		if err != nil {
			problems = append(problems, fmt.Errorf("re-read imported task trace %s for rollback: %w", uri, err))
			continue
		}
		if !created || sha256.Sum256(data) != expected {
			problems = append(problems, fmt.Errorf("refuse to roll back changed task trace %s", uri))
			continue
		}
		if err := os.Remove(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("roll back imported task trace %s: %w", uri, err))
			continue
		}
		directories[filepath.Dir(absolute)] = struct{}{}
	}
	for directory := range directories {
		if err := core.SyncDirectory(directory); err != nil {
			problems = append(problems, fmt.Errorf("sync rolled-back task trace directory %s: %w", directory, err))
		}
	}
	return errors.Join(problems...)
}

func ensureSessionTraceDirectory(baseRoot, directory string) error {
	if err := core.ValidateWithinRoot(baseRoot, directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(directory, core.BaseDirMode); err != nil {
			return fmt.Errorf("create tasks sync state directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("tasks sync state path is not a real directory below the base")
	}
	return nil
}

type preparedSessionTrace struct {
	URI     string
	Content []byte
}

type sessionTraceTarget struct {
	trace    preparedSessionTrace
	absolute string
}

type stagedSessionTrace struct {
	target    sessionTraceTarget
	temporary string
}

func publishSessionTraceBatch(ctx context.Context, targets []sessionTraceTarget) error {
	staged, err := stageSessionTraceBatch(ctx, targets)
	if err != nil {
		return err
	}
	published, err := linkSessionTraceBatch(ctx, staged)
	if err != nil {
		return rollbackSessionTraceBatch(err, staged, published)
	}
	if err := removeStagedSessionTraces(staged); err != nil {
		return rollbackSessionTraceBatch(err, staged, published)
	}
	if err := syncSessionTraceDirectories(staged); err != nil {
		return rollbackSessionTraceBatch(err, staged, published)
	}
	return nil
}

func stageSessionTraceBatch(ctx context.Context, targets []sessionTraceTarget) ([]stagedSessionTrace, error) {
	staged := make([]stagedSessionTrace, 0, len(targets))
	for _, target := range targets {
		if err := checkContext(ctx); err != nil {
			return nil, rollbackSessionTraceBatch(err, staged, nil)
		}
		directory := filepath.Dir(target.absolute)
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			cause := fmt.Errorf("create task-trace directory for %s: %w", target.trace.URI, err)
			return nil, rollbackSessionTraceBatch(cause, staged, nil)
		}
		temporary, err := stageSessionTrace(directory, target)
		if err != nil {
			return nil, rollbackSessionTraceBatch(err, staged, nil)
		}
		staged = append(staged, stagedSessionTrace{target: target, temporary: temporary})
	}
	return staged, nil
}

func linkSessionTraceBatch(ctx context.Context, staged []stagedSessionTrace) ([]string, error) {
	published := make([]string, 0, len(staged))
	for _, file := range staged {
		if err := checkContext(ctx); err != nil {
			return published, err
		}
		// Link publishes without replacing a trace that appeared after preflight.
		if err := os.Link(file.temporary, file.target.absolute); err != nil {
			return published, fmt.Errorf("publish task trace %s: %w", file.target.trace.URI, err)
		}
		published = append(published, file.target.absolute)
	}
	return published, nil
}

func removeStagedSessionTraces(staged []stagedSessionTrace) error {
	for _, file := range staged {
		if err := os.Remove(file.temporary); err != nil {
			return fmt.Errorf("remove staged task trace %s: %w", file.target.trace.URI, err)
		}
	}
	return nil
}

func syncSessionTraceDirectories(staged []stagedSessionTrace) error {
	directories := make(map[string]struct{}, len(staged))
	for _, file := range staged {
		directories[filepath.Dir(file.target.absolute)] = struct{}{}
	}
	for directory := range directories {
		if err := core.SyncDirectory(directory); err != nil {
			return fmt.Errorf("sync task-trace directory %s: %w", directory, err)
		}
	}
	return nil
}

func rollbackSessionTraceBatch(cause error, staged []stagedSessionTrace, published []string) error {
	problems := []error{cause}
	directories := make(map[string]struct{}, len(published))
	expected := make(map[string][sha256.Size]byte, len(staged))
	for _, file := range staged {
		expected[file.target.absolute] = sha256.Sum256(file.target.trace.Content)
	}
	for _, absolute := range published {
		directories[filepath.Dir(absolute)] = struct{}{}
		info, err := os.Lstat(absolute)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("inspect task trace before rollback %s: %w", absolute, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			problems = append(problems, fmt.Errorf("refuse to roll back changed task trace %s", absolute))
			continue
		}
		data, err := core.ReadFileLimit(absolute, core.MaxNarrativeBytes)
		want, found := expected[absolute]
		if err != nil || !found || sha256.Sum256(data) != want {
			problems = append(problems, fmt.Errorf("refuse to roll back changed task trace %s", absolute))
			continue
		}
		if err := os.Remove(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("roll back task trace %s: %w", absolute, err))
		}
	}
	for _, file := range staged {
		if err := os.Remove(file.temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove staged task trace %s: %w", file.temporary, err))
		}
	}
	for directory := range directories {
		if err := core.SyncDirectory(directory); err != nil {
			problems = append(problems, fmt.Errorf("sync rolled-back task-trace directory %s: %w", directory, err))
		}
	}
	return errors.Join(problems...)
}

func stageSessionTrace(directory string, target sessionTraceTarget) (string, error) {
	file, err := os.CreateTemp(directory, "."+filepath.Base(target.absolute)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("stage task trace %s: %w", target.trace.URI, err)
	}
	temporary := file.Name()
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(target.trace.Content); err != nil {
		return "", fmt.Errorf("write staged task trace %s: %w", target.trace.URI, err)
	}
	if err := file.Chmod(core.BaseFileMode); err != nil {
		return "", fmt.Errorf("chmod staged task trace %s: %w", target.trace.URI, err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync staged task trace %s: %w", target.trace.URI, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close staged task trace %s: %w", target.trace.URI, err)
	}
	complete = true
	return temporary, nil
}

func decodeSessionTraceInputs(stdout string) ([]SessionTraceInput, error) {
	if strings.TrimSpace(stdout) == "" {
		return nil, errors.New("command exited zero but printed nothing; a tasks source prints [] when no session completed")
	}
	if !utf8.ValidString(stdout) {
		return nil, errors.New("command output is not valid UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	var inputs []SessionTraceInput
	if err := decoder.Decode(&inputs); err != nil {
		return nil, fmt.Errorf("decode JSON array: %w", err)
	}
	if inputs == nil {
		return nil, errors.New("expected a JSON array; null is not a completed empty import")
	}
	if len(inputs) > maxSessionTracesPerRun {
		return nil, fmt.Errorf("helper returned %d sessions; limit is %d", len(inputs), maxSessionTracesPerRun)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("trailing JSON holds more than one document")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return inputs, nil
}

func prepareSessionTrace(base *Base, input SessionTraceInput, window sources.Window) (preparedSessionTrace, error) {
	if !sessionHarnessPattern.MatchString(input.Harness) {
		return preparedSessionTrace{}, fmt.Errorf("harness %q must be a short lowercase name", input.Harness)
	}
	if err := validateSessionTraceScalar("sid", input.SID, maxSessionTraceScalar); err != nil {
		return preparedSessionTrace{}, err
	}
	if input.ID != input.Harness+":"+input.SID {
		return preparedSessionTrace{}, fmt.Errorf("id %q does not match harness and sid", input.ID)
	}
	first, err := time.Parse(time.RFC3339Nano, input.FirstAt)
	if err != nil {
		return preparedSessionTrace{}, fmt.Errorf("first_at %q is not RFC3339: %w", input.FirstAt, err)
	}
	last, err := time.Parse(time.RFC3339Nano, input.LastAt)
	if err != nil {
		return preparedSessionTrace{}, fmt.Errorf("last_at %q is not RFC3339: %w", input.LastAt, err)
	}
	start, startErr := time.Parse(time.RFC3339, window.Start)
	end, endErr := time.Parse(time.RFC3339, window.End)
	if startErr != nil || endErr != nil {
		return preparedSessionTrace{}, errors.New("collection window is not RFC3339")
	}
	if first.After(last) {
		return preparedSessionTrace{}, errors.New("first_at is after last_at")
	}
	if last.Before(start) || !last.Before(end) {
		return preparedSessionTrace{}, fmt.Errorf("last_at %s is outside requested window [%s, %s)", input.LastAt, window.Start, window.End)
	}
	if input.CWD != "" {
		if !filepath.IsAbs(input.CWD) {
			return preparedSessionTrace{}, errors.New("cwd must be absolute when present")
		}
		if err := validateSessionTraceScalar("cwd", input.CWD, maxSessionTracePath); err != nil {
			return preparedSessionTrace{}, err
		}
	}
	if input.Repo != "" && !sessionRepoPattern.MatchString(input.Repo) {
		return preparedSessionTrace{}, fmt.Errorf("repo %q must be an exact owner/name", input.Repo)
	}
	if err := validateSessionTraceScalar("model", input.Model, maxSessionTraceScalar); err != nil && input.Model != "" {
		return preparedSessionTrace{}, err
	}
	if len(input.Requests) == 0 || len(input.Requests) > maxSessionTraceRequests {
		return preparedSessionTrace{}, fmt.Errorf("requests has %d entries; expected 1..%d", len(input.Requests), maxSessionTraceRequests)
	}
	if err := validateSessionTraceList("requests", input.Requests, maxSessionTraceRequests, maxSessionTraceExcerpt, true); err != nil {
		return preparedSessionTrace{}, err
	}
	if err := validateSessionTraceList("files", input.Files, maxSessionTraceFiles, maxSessionTracePath, false); err != nil {
		return preparedSessionTrace{}, err
	}
	if err := validateSessionTraceList("verification", input.Verification, maxSessionTraceCommands, maxSessionTraceScalar, false); err != nil {
		return preparedSessionTrace{}, err
	}
	if err := validateSessionTraceLayout("last_assistant", input.LastAssistant, maxSessionTraceExcerpt); err != nil && input.LastAssistant != "" {
		return preparedSessionTrace{}, err
	}

	// Collection and completion use last_at, so the trace path must use the same local day.
	// Otherwise a cross-midnight session is hidden under a day the completed-range marker has
	// already skipped.
	date := last.In(base.Now().Location()).Format(time.DateOnly)
	slug := sessionTraceSlug(input)
	uri := path.Join(string(core.LayerTasks), date, slug, core.TaskTraceFile)
	content := renderSessionTrace(input)
	if int64(len(content)) > core.MaxNarrativeBytes {
		return preparedSessionTrace{}, fmt.Errorf("rendered task trace is %d bytes; limit is %d", len(content), core.MaxNarrativeBytes)
	}
	if _, err := ParsePage(uri, content, time.Time{}); err != nil {
		return preparedSessionTrace{}, fmt.Errorf("validate rendered task trace: %w", err)
	}
	return preparedSessionTrace{URI: uri, Content: content}, nil
}

func validateSessionTraceList(name string, values []string, maximum, bytesLimit int, allowLayout bool) error {
	if len(values) > maximum {
		return fmt.Errorf("%s has %d entries; limit is %d", name, len(values), maximum)
	}
	for index, value := range values {
		var err error
		if allowLayout {
			err = validateSessionTraceLayout(fmt.Sprintf("%s[%d]", name, index), value, bytesLimit)
		} else {
			err = validateSessionTraceScalar(fmt.Sprintf("%s[%d]", name, index), value, bytesLimit)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func validateSessionTraceScalar(name, value string, bytesLimit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("%s contains layout control characters", name)
	}
	return validateSessionTraceLayout(name, value, bytesLimit)
}

func validateSessionTraceLayout(name, value string, bytesLimit int) error {
	if !utf8ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if len(value) > bytesLimit {
		return fmt.Errorf("%s is %d bytes; limit is %d", name, len(value), bytesLimit)
	}
	if problem, unsafe := authoredTextProblem(value, true); unsafe {
		return fmt.Errorf("%s %s", name, problem)
	}
	return nil
}

func utf8ValidString(value string) bool {
	for len(value) > 0 {
		char, size := utf8.DecodeRuneInString(value)
		if char == unicode.ReplacementChar && size == 1 {
			return false
		}
		value = value[size:]
	}
	return true
}

func sessionTraceSlug(input SessionTraceInput) string {
	repository := input.Harness
	if input.Repo != "" {
		repository = path.Base(input.Repo)
	} else if input.CWD != "" {
		repository = filepath.Base(filepath.Clean(input.CWD))
	}
	repository = traceSlugPart(repository, 48)
	harness := traceSlugPart(input.Harness, 32)
	session := traceSlugPart(input.SID, 64)
	digest := sha256.Sum256([]byte(input.Harness + "\x00" + input.SID))
	if session == "session" || session != input.SID || len(input.SID) > 64 {
		// A transformed component is deliberately longer than the 64-byte literal namespace.
		// Otherwise a distinct literal SID could equal the normalized spelling and lose evidence.
		session = strings.Trim(traceSlugPart(input.SID, 48), "-._") + "-" + hex.EncodeToString(digest[:])
	}
	return strings.Trim(repository+"-"+harness+"-"+session, "-._")
}

func traceSlugPart(value string, maximum int) string {
	var builder strings.Builder
	lastSeparator := false
	for _, char := range strings.ToLower(value) {
		valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-'
		if valid {
			builder.WriteRune(char)
			lastSeparator = false
		} else if !lastSeparator {
			builder.WriteByte('-')
			lastSeparator = true
		}
		if builder.Len() >= maximum {
			break
		}
	}
	part := strings.Trim(builder.String(), "-._")
	if part == "" || !slugPattern.MatchString(part) {
		return "session"
	}
	return part
}

func renderSessionTrace(input SessionTraceInput) []byte {
	label := input.Harness
	if input.Repo != "" {
		label = input.Repo
	} else if input.CWD != "" {
		label = filepath.Base(filepath.Clean(input.CWD))
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Agent session in %s\n\n", markdownLiteralText(label))
	builder.WriteString("## Session\n\n")
	fmt.Fprintf(&builder, "- **Harness**: %s\n", markdownLiteralText(input.Harness))
	if input.Model != "" {
		fmt.Fprintf(&builder, "- **Model**: %s\n", markdownLiteralText(input.Model))
	}
	fmt.Fprintf(&builder, "- **Session**: %s\n", markdownLiteralText(input.SID))
	fmt.Fprintf(&builder, "- **Started**: %s\n- **Completed**: %s\n", input.FirstAt, input.LastAt)
	if input.Repo != "" {
		fmt.Fprintf(&builder, "- **Repository**: %s\n", markdownLiteralText(input.Repo))
	}
	if input.CWD != "" {
		fmt.Fprintf(&builder, "- **Working directory**: %s\n", markdownLiteralText(input.CWD))
	}

	for index, request := range input.Requests {
		fmt.Fprintf(&builder, "\n## %d. %s\n\n", index+1, markdownLiteralText(sessionRequestTitle(request)))
		// Keep captured text outside list items and inside a collision-safe fence so
		// formatters preserve the evidence while its Markdown remains inert.
		builder.WriteString("**Request**:\n\n")
		writeFencedTraceText(&builder, request)
		builder.WriteString("\n**Trace**:\n\n1. Imported from the harness-independent session store without a model call.\n")
	}

	builder.WriteString("\n## Session evidence\n\n**Files changed from git at collection time**:\n\n")
	writeFencedTraceList(&builder, input.Files, "none reported")
	builder.WriteString("\n**Verification commands seen in the last assistant message**:\n\n")
	writeFencedTraceList(&builder, input.Verification, "none reported; this trace does not infer execution")
	builder.WriteString("\n**Last assistant message**:\n\n")
	if input.LastAssistant == "" {
		writeFencedTraceText(&builder, "none recorded")
	} else {
		writeFencedTraceText(&builder, input.LastAssistant)
	}
	builder.WriteString("\n## Learned\n\n<!-- Add reviewed durable lessons as bullets; `fkf learn propose` stages them as a diff. -->\n")
	return []byte(builder.String())
}

func sessionRequestTitle(request string) string {
	line, _, _ := strings.Cut(request, "\n")
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "Request"
	}
	runes := []rune(line)
	if len(runes) > 96 {
		line = string(runes[:96]) + "…"
	}
	return line
}

func writeFencedTraceList(builder *strings.Builder, values []string, empty string) {
	if len(values) == 0 {
		writeFencedTraceText(builder, empty)
		return
	}
	writeFencedTraceText(builder, strings.Join(values, "\n"))
}

func writeFencedTraceText(builder *strings.Builder, value string) {
	fenceLength := 3
	run := 0
	for index := range len(value) {
		if value[index] == '`' {
			run++
			if run >= fenceLength {
				fenceLength = run + 1
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", fenceLength)
	builder.WriteString(fence)
	// Captured prose is inert text, and the language marker keeps generated Markdown valid
	// under the same fenced-block contract as authored pages.
	builder.WriteString("text\n")
	builder.WriteString(value)
	if !strings.HasSuffix(value, "\n") {
		builder.WriteByte('\n')
	}
	builder.WriteString(fence)
	builder.WriteByte('\n')
}
