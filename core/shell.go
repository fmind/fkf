package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrCLIOutputTooLarge reports a subprocess stream that exceeded the in-memory bound.
var ErrCLIOutputTooLarge = errors.New("CLI output exceeds size limit")

const (
	// MaxCLIOutputBytes bounds each captured subprocess stream.
	MaxCLIOutputBytes = 64 << 20
	// DeclaredCommandDirectory is the neutral cwd for every source command. Base-controlled
	// executables and support files are reached only through the trust-digested bin/ PATH.
	DeclaredCommandDirectory = "/"
	// DeclaredCommandEnvironmentPolicy is both the trust disclosure and the digest input for
	// the fixed child-environment boundary implemented below.
	DeclaredCommandEnvironmentPolicy = "provider environment without runtime startup loaders or base-resolving home/config roots"
	// commandWaitDelay bounds how long Wait may keep draining a cancelled command's pipes.
	// A grandchild that survived the group kill cannot hold `sync` open indefinitely.
	commandWaitDelay = 5 * time.Second
)

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

type commandEnvironmentKey struct{}

type declaredCommandDiagnosticKey struct{}

// DeclaredCommandDiagnostic is the safe context attached only to a source's run: command.
// Body commands are deliberately excluded because their argv may contain a value copied from
// collected provider data. Values maps are also excluded because provider selectors and
// credentials belong in the environment, never in a diagnostic.
type DeclaredCommandDiagnostic struct {
	Source      string
	Date        string
	WindowStart string
	WindowEnd   string
	Command     string
}

type commandEnvironment struct {
	values        map[string]string
	forbiddenRoot string
}

// WithDeclaredCommandDiagnostic binds the base-authored command context that RunCLIBounded may
// safely log on failure. The display is diagnostic-only; execution still receives the original
// argv and never reparses it.
func WithDeclaredCommandDiagnostic(
	ctx context.Context, diagnostic DeclaredCommandDiagnostic,
) context.Context {
	return context.WithValue(ctx, declaredCommandDiagnosticKey{}, diagnostic)
}

// WithCommandEnvironment binds explicit immutable subprocess configuration to one call
// tree. It avoids process-global account switching while keeping provider transports
// independently testable. Values are never included in command diagnostics.
func WithCommandEnvironment(ctx context.Context, values map[string]string) (context.Context, error) {
	return withCommandEnvironment(ctx, values, "")
}

// WithCommandEnvironmentForRoot also names repository content that runtime startup state must
// never select implicitly. Provider commands run from a neutral directory, so this separate
// root keeps absolute HOME/XDG spellings inside the base out of their child environment.
func WithCommandEnvironmentForRoot(
	ctx context.Context, values map[string]string, forbiddenRoot string,
) (context.Context, error) {
	if forbiddenRoot == "" || !filepath.IsAbs(forbiddenRoot) {
		return nil, errors.New("absolute forbidden command root is required")
	}
	return withCommandEnvironment(ctx, values, filepath.Clean(forbiddenRoot))
}

func withCommandEnvironment(
	ctx context.Context, values map[string]string, forbiddenRoot string,
) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("command environment context is required")
	}
	environment := make(map[string]string, len(values))
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("command environment contains an invalid key or value")
		}
		environment[key] = value
	}
	return context.WithValue(ctx, commandEnvironmentKey{}, commandEnvironment{
		values: environment, forbiddenRoot: forbiddenRoot,
	}), nil
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := max(0, buffer.limit-buffer.buffer.Len())
	if remaining < len(data) {
		buffer.truncated = true
		data = data[:remaining]
	}
	_, _ = buffer.buffer.Write(data)
	return written, nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }

// commandFailure keeps provider stderr available only as a retry-matching oracle. Its fields
// are deliberately private, it has no JSON representation, and Error and Diagnostic are
// derived solely from the process status. Provider CLIs routinely put response bodies,
// account identifiers, and occasionally credentials on stderr; none belongs in a sync report
// or log.
type commandFailure struct {
	status     string
	diagnostic string
	exitCode   int
	hasExit    bool
	stderr     string
}

func (failure *commandFailure) Error() string { return failure.diagnostic }

// Format keeps even debug-oriented formatting such as %#v on the safe representation. The
// default struct formatter would otherwise reveal private fields despite Error being clean.
func (failure *commandFailure) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(failure.diagnostic))
}
func (failure *commandFailure) StatusClass() string { return failure.status }
func (failure *commandFailure) Diagnostic() string  { return failure.diagnostic }
func (failure *commandFailure) ExitCode() (int, bool) {
	return failure.exitCode, failure.hasExit
}

// MatchesCommandStderr answers one declared retry condition without exposing the captured
// bytes. It is intentionally an oracle rather than an accessor: callers may decide whether a
// named transient failure occurred, but cannot log or serialize the provider response.
func (failure *commandFailure) MatchesCommandStderr(condition string) bool {
	return condition != "" && strings.Contains(failure.stderr, condition)
}

// NewCommandFailure wraps a runner adapter's failed process without rendering provider stderr.
// ExecRunner uses it below; the exported constructor also lets another hermetic Runner preserve
// the same retry and privacy contract without manufacturing a leaky error string.
func NewCommandFailure(cause error, stderr string) error {
	if cause == nil {
		return nil
	}
	return newCommandFailure(cause, stderr)
}

func newCommandFailure(cause error, stderr string) *commandFailure {
	failure := &commandFailure{
		status: "failure", diagnostic: "command execution failed", exitCode: -1, stderr: stderr,
	}
	var exitErr *exec.ExitError
	if !errors.As(cause, &exitErr) {
		return failure
	}
	if code := exitErr.ExitCode(); code >= 0 {
		failure.status = "exit"
		failure.diagnostic = fmt.Sprintf("command exited with status %d", code)
		failure.exitCode, failure.hasExit = code, true
		return failure
	}
	failure.status = "signal"
	failure.diagnostic = "command terminated by a signal"
	return failure
}

// RunCLI executes a CLI command with a given timeout and working directory.
// The parent context is honored, so an interrupt/SIGTERM cancels in-flight
// subprocesses instead of waiting out the per-call timeout.
func RunCLI(ctx context.Context, cmd []string, cwd string, timeout time.Duration) (string, error) {
	return RunCLIBounded(ctx, cmd, cwd, "", timeout, MaxCLIOutputBytes)
}

// RunCLIStdin executes a CLI command feeding it one in-memory document. It exists for the
// single case that needs it — handing a stored document to the `jq` a `?jq=` URI names — so
// that expression stays one argv element and never reaches a shell.
func RunCLIStdin(ctx context.Context, cmd []string, cwd, stdin string, timeout time.Duration) (string, error) {
	return RunCLIBounded(ctx, cmd, cwd, stdin, timeout, MaxCLIOutputBytes)
}

// RunCLIBounded executes a CLI while applying the caller's tighter limit independently
// to stdout and stderr.
func RunCLIBounded(ctx context.Context, cmd []string, cwd, stdin string, timeout time.Duration, maxOutputBytes int) (string, error) {
	if len(cmd) == 0 {
		return "", errors.New("empty command")
	}
	if maxOutputBytes <= 0 || maxOutputBytes > MaxCLIOutputBytes {
		return "", fmt.Errorf("CLI output limit must be between 1 and %d bytes", MaxCLIOutputBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Resolve against the PATH this command will actually receive. Empty and relative entries
	// are removed before both lookup and exec; otherwise the resolved shell could search them
	// again from Cmd.Dir and reach a file the resolver deliberately ignored.
	inherited, _ := ctx.Value(commandEnvironmentKey{}).(commandEnvironment)
	environment := maps.Clone(inherited.values)
	if environment == nil {
		environment = map[string]string{}
	}
	pathList, supplied := environment["PATH"]
	if !supplied {
		pathList = os.Getenv("PATH")
	}
	pathList = SanitizePathList(pathList, "")
	if pathList == "" {
		return "", fmt.Errorf("executable %q not found on PATH", cmd[0])
	}
	environment["PATH"] = pathList
	name, err := ResolveExecutable(cmd[0], pathList)
	if err != nil {
		return "", err
	}
	args := cmd[1:]

	// #nosec G204 -- running a variable command is the product: a source IS a command. name
	// comes from ResolveExecutable, and the base that declared it must be trusted before this
	// is reached (core.RequireTrust), which is where the review of what may run belongs.
	execCmd := exec.CommandContext(ctx, name, args...)
	// A helper may fork descendants. CommandContext on
	// its own kills only the shell, and because Stdout/Stderr are ordinary writers rather than
	// *os.File, os/exec copies through a pipe whose write end a surviving grandchild still
	// holds — so Wait blocked until the pipeline finished on its own and the declared timeout
	// bounded nothing. Killing the process group reaps the pipeline; WaitDelay is the backstop
	// for whatever a group kill still cannot reach.
	setProcessGroup(execCmd)
	execCmd.WaitDelay = commandWaitDelay
	forbiddenRoot := inherited.forbiddenRoot
	if forbiddenRoot == "" {
		forbiddenRoot = cwd
	}
	execCmd.Env = mergedCommandEnvironment(environment, forbiddenRoot)
	if cwd != "" {
		execCmd.Dir = cwd
	}
	if stdin != "" {
		execCmd.Stdin = strings.NewReader(stdin)
	}

	stdoutBuf := &boundedBuffer{limit: maxOutputBytes}
	stderrBuf := &boundedBuffer{limit: maxOutputBytes}
	execCmd.Stdout = stdoutBuf
	execCmd.Stderr = stderrBuf

	err = execCmd.Run()
	if stdoutBuf.truncated || stderrBuf.truncated {
		return "", fmt.Errorf("%w: command output exceeded %d bytes", ErrCLIOutputTooLarge, maxOutputBytes)
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			if errors.Is(contextErr, context.Canceled) {
				return "", fmt.Errorf("command canceled: %w", contextErr)
			}
			return "", fmt.Errorf("command timed out: %w", contextErr)
		}
		failure := newCommandFailure(err, stderrBuf.String())
		attributes := make([]any, 0, 16)
		if diagnostic, found := ctx.Value(declaredCommandDiagnosticKey{}).(DeclaredCommandDiagnostic); found {
			attributes = append(attributes, "source", diagnostic.Source)
			if diagnostic.Date != "" {
				attributes = append(attributes, "date", diagnostic.Date)
			}
			if diagnostic.WindowStart != "" {
				attributes = append(attributes,
					"window_start", diagnostic.WindowStart, "window_end", diagnostic.WindowEnd)
			}
			attributes = append(attributes,
				"command", diagnostic.Command, "cwd", cwd, "timeout", timeout)
		}
		attributes = append(attributes,
			"status", failure.StatusClass(), "diagnostic", failure.Diagnostic())
		slog.ErrorContext(ctx, "CLI command failed", attributes...)
		return "", failure
	}

	return stdoutBuf.String(), nil
}

func mergedCommandEnvironment(overrides map[string]string, commandDirectory string) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	maps.Copy(values, overrides)
	removeRuntimeStartupEnvironment(values, commandDirectory)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func removeRuntimeStartupEnvironment(values map[string]string, commandDirectory string) {
	// These variables can load code before a declared argv reaches its own entrypoint. In
	// particular, relative values resolve from Cmd.Dir, which is the base root for collectors;
	// inheriting one could execute mutable wiki or collected content outside the trust digest.
	// Provider credentials and profile selectors remain inherited, but runtime startup hooks do
	// not cross the command boundary, including when an internal caller explicitly overrides one.
	for _, key := range []string{
		"BASH_ENV", "ENV", "ZDOTDIR", "fish_function_path",
		"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONUSERBASE", "PYTHONPLATLIBDIR",
		"NODE_OPTIONS", "NODE_PATH", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "GCONV_PATH",
		"R_ENVIRON", "R_ENVIRON_USER", "R_PROFILE", "R_PROFILE_USER",
	} {
		delete(values, key)
	}
	for key := range values {
		if strings.HasPrefix(key, "DYLD_") || strings.HasPrefix(key, "LUA_INIT") {
			delete(values, key)
		}
	}
	// XDG roots and HOME are ordinary provider configuration when they are absolute and
	// machine-local. Refuse only spellings that can resolve into this base, so a Fish startup
	// file cannot hide in wiki/ while GH_CONFIG_DIR and other provider selectors keep working.
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if value, found := values[key]; found && unsafeCommandDirectoryPath(value, commandDirectory) {
			delete(values, key)
		}
	}
}

func unsafeCommandDirectoryPath(value, commandDirectory string) bool {
	if value == "" || !filepath.IsAbs(value) {
		return true
	}
	if commandDirectory == "" {
		return false
	}
	root, err := filepath.Abs(filepath.Clean(commandDirectory))
	if err != nil {
		return true
	}
	candidate, err := filepath.Abs(filepath.Clean(value))
	if err != nil || pathIsWithin(root, candidate) {
		return true
	}
	resolvedRoot, err := resolveExistingPath(root)
	if err != nil {
		return true
	}
	resolvedCandidate, err := resolveExistingPath(candidate)
	return err != nil || pathIsWithin(resolvedRoot, resolvedCandidate)
}
