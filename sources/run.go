// Package sources runs the commands a base declares and files what they print.
//
// There is no provider code in fkf. A source is an entry in the base's own fkf.yaml: a
// command that prints JSON, and jq-subset paths naming the fields worth joining on. That
// choice is what removes credentials from this program entirely — the CLI a source names
// already holds the login — and it is why adding a source is a YAML pull request.
//
// `run:`, `test:`, and `body:` are direct argv. A base helper's shebang chooses its interpreter;
// fkf never reparses arguments as shell syntax. `run:` and `test:` receive only values fkf
// computes, while `body:` may receive charset-checked values from one stored record.
package sources

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

// ErrCivilDateDoesNotExist reports a date label skipped entirely by the local timezone. It is
// distinct from malformed input so planners can step over the missing label and continue to the
// previous completed civil day.
var ErrCivilDateDoesNotExist = errors.New("civil date does not exist in the local timezone")

// Command is one execution request. It is a value so a test can assert exactly what would
// have run, which is what makes `--dry-run` verifiable rather than merely plausible.
type Command struct {
	Argv          []string          `json:"argv"`
	Dir           string            `json:"dir"`
	ForbiddenRoot string            `json:"-"`
	Stdin         string            `json:"-"`
	Env           map[string]string `json:"env,omitempty"`
	Timeout       time.Duration     `json:"timeout"`
	Source        string            `json:"-"`
	Window        Window            `json:"-"`
}

// Display renders argv as a copyable diagnostic. Execution never reparses this string.
func (c Command) Display() string {
	arguments := make([]string, 0, len(c.Argv))
	for _, argument := range c.Argv {
		arguments = append(arguments, displayArg(argument))
	}
	return strings.Join(arguments, " ")
}

// Runner executes one command and returns its stdout. It is an interface so every test in
// this repository can inject a fake: the suite must never be able to reach a provider.
type Runner interface {
	Run(ctx context.Context, cmd Command) (string, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, cmd Command) (string, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, cmd Command) (string, error) { return f(ctx, cmd) }

// ExecRunner is the real runner: bounded output, honoured cancellation, and a per-command timeout.
func ExecRunner() Runner {
	return RunnerFunc(func(ctx context.Context, cmd Command) (string, error) {
		// Only BuildRunCommand sets Source. A body command may contain collected values in argv,
		// so its zero Source keeps the generic failure log free of command parameters.
		if cmd.Source != "" {
			ctx = core.WithDeclaredCommandDiagnostic(ctx, core.DeclaredCommandDiagnostic{
				Source: cmd.Source, Date: cmd.Window.Date,
				WindowStart: cmd.Window.Start, WindowEnd: cmd.Window.End,
				Command: cmd.Display(),
			})
		}
		if len(cmd.Env) > 0 || cmd.ForbiddenRoot != "" {
			var (
				withEnv context.Context
				err     error
			)
			if cmd.ForbiddenRoot == "" {
				withEnv, err = core.WithCommandEnvironment(ctx, cmd.Env)
			} else {
				withEnv, err = core.WithCommandEnvironmentForRoot(ctx, cmd.Env, cmd.ForbiddenRoot)
			}
			if err != nil {
				return "", err
			}
			ctx = withEnv
		}
		return core.RunCLIStdin(ctx, cmd.Argv, cmd.Dir, cmd.Stdin, cmd.Timeout)
	})
}

// Window is the local day an events source collects, and the values its placeholders expand to.
// The bounds are half-open in UTC so a source that filters on timestamps and one that filters
// on dates select the same records.
type Window struct {
	Date  string
	Next  string
	Start string
	End   string
}

// DayWindow builds the window for one local day. day is expected to be NOON-anchored — what
// ParseDay returns — because reconstructing literal midnight directly from a Y/M/D is exactly
// the operation that fails silently on a day whose midnight does not exist.
//
// A civil day is not always 24 hours: a spring-forward transition at local midnight (several
// zones move their clocks at 00:00, not 02:00 — Brazil did through 2018) makes that day 23
// hours long, and `time.Date` given a wall-clock time inside the gap does not error. It
// normalizes to a DIFFERENT, earlier, valid instant — one that formats as the PREVIOUS
// calendar day. Reconstructing "midnight" from day.Year/Month/Day would reproduce the same
// non-existent request and the same silent misfile, so the boundary is found instead: the
// earliest local instant whose calendar date actually equals the one asked for.
func DayWindow(day time.Time) Window {
	year, month, date := day.Date()
	loc := day.Location()
	start := startOfCivilDay(year, month, date, loc)
	// The next day's own boundary is found the same way, from a noon anchor of that day
	// rather than by adding a wall-clock day to `start` — `start` may itself sit inside an
	// unusual offset, and AddDate on it can drift by exactly the gap it was found to avoid.
	nextNoon := time.Date(year, month, date, 12, 0, 0, 0, loc).AddDate(0, 0, 1)
	nextYear, nextMonth, nextDate := nextNoon.Date()
	end := startOfCivilDay(nextYear, nextMonth, nextDate, loc)
	return Window{
		Date:  start.Format(time.DateOnly),
		Next:  end.Format(time.DateOnly),
		Start: start.UTC().Format(time.RFC3339),
		End:   end.UTC().Format(time.RFC3339),
	}
}

// startOfCivilDay finds the earliest instant whose local calendar date is (year, month, date).
// The direct construction is correct on every ordinary day and is tried first, at zero extra
// cost; the search only ever runs on the rare day a zone transitions at midnight, and any real
// transition is well under the four-hour ceiling.
func startOfCivilDay(year int, month time.Month, date int, loc *time.Location) time.Time {
	candidate := time.Date(year, month, date, 0, 0, 0, 0, loc)
	if sameCivilDay(candidate, year, month, date) {
		return candidate
	}
	for step := time.Minute; step <= 4*time.Hour; step += time.Minute {
		probe := candidate.Add(step)
		if sameCivilDay(probe, year, month, date) {
			return probe
		}
	}
	// No real IANA zone jumps more than a few hours at once, so this is unreachable in
	// practice; returning the direct construction keeps the function total rather than
	// panicking on a day it could not resolve.
	return candidate
}

func sameCivilDay(t time.Time, year int, month time.Month, date int) bool {
	y, m, d := t.Date()
	return y == year && m == month && d == date
}

// ParseDay reads a YYYY-MM-DD day in the local zone, anchored at noon rather than midnight.
// No real transition falls at noon, so the returned time's calendar date is always exactly
// the one requested — which a direct midnight parse cannot promise, since a nonexistent local
// midnight is what DayWindow's own fix exists to route around. Every caller already reduces
// this to a date label or hands it straight to DayWindow, so the shifted hour is invisible.
func ParseDay(value string) (time.Time, error) {
	label := strings.TrimSpace(value)
	noon, err := time.ParseInLocation("2006-01-02 15:04:05", label+" 12:00:00", time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
	}
	if noon.Format(time.DateOnly) != label {
		return time.Time{}, fmt.Errorf("%w: %s in %s", ErrCivilDateDoesNotExist, label, time.Local)
	}
	return noon, nil
}

// Environment is the machine-local context a base's commands run in.
type Environment struct {
	Root string
	Bin  []string
	Env  map[string]string
}

// NewEnvironment resolves the execution context from a base's configuration. The base's own
// bin/ comes first on PATH so a preset's helper script is reachable without installation, and
// the declared bin: entries follow — both ahead of the inherited PATH, because a base is
// allowed to pin the tool its own sources expect. Inherited entries below the base, including
// symlinks back into it, are removed first: only the separately prepended, trust-digested bin/
// may make repository content executable.
func NewEnvironment(config *core.Config) Environment {
	store := config.Store()
	inherited := core.SanitizePathList(os.Getenv("PATH"), store.Root())
	directories := make([]string, 0, len(config.Bin)+1+len(filepath.SplitList(inherited)))
	directories = append(directories, store.BinDir())
	for _, entry := range config.Bin {
		directories = append(directories, core.ExpandHome(entry))
	}
	directories = append(directories, filepath.SplitList(inherited)...)
	env := map[string]string{"PATH": strings.Join(directories, string(os.PathListSeparator))}
	return Environment{Root: store.Root(), Bin: directories, Env: env}
}

// LookPath resolves a command name against the PATH this environment will actually give the
// subprocess — the base's own bin/ first, then the declared bin: entries, then the inherited
// PATH. It delegates to core so `fkf status` and the runner cannot answer differently.
func (e Environment) LookPath(name string) (string, bool) {
	return core.LookPathIn(name, e.Env["PATH"])
}

// BuildRunCommand substitutes fkf-owned placeholders in each declared argument and returns
// exactly what exec receives. No shell parses the result.
func BuildRunCommand(source *core.Source, env Environment, window Window, timeout time.Duration) Command {
	home := homeDirectory()
	values := map[string]string{
		"date": window.Date, "next_date": window.Next,
		"start": window.Start, "end": window.End,
		"base": env.Root, "home": home,
	}
	argv := make([]string, 0, len(source.Run))
	for _, argument := range source.Run {
		argv = append(argv, substitute(argument, values))
	}
	if source.Timeout > 0 {
		timeout = source.Timeout
	}
	return Command{
		Argv: argv,
		Dir:  core.DeclaredCommandDirectory, ForbiddenRoot: env.Root,
		Env: maps.Clone(env.Env), Timeout: timeout, Source: source.Name, Window: window,
	}
}

// BuildTestCommand substitutes only stable base and home paths into a source's verification
// hook. Test hooks receive no collection window or stored value and execute as direct argv.
func BuildTestCommand(source *core.Source, env Environment, timeout time.Duration) Command {
	values := map[string]string{"base": env.Root, "home": homeDirectory()}
	argv := make([]string, 0, len(source.Test))
	for _, argument := range source.Test {
		argv = append(argv, substitute(argument, values))
	}
	if source.Timeout > 0 {
		timeout = source.Timeout
	}
	return Command{
		Argv: argv,
		Dir:  core.DeclaredCommandDirectory, ForbiddenRoot: env.Root,
		Env: maps.Clone(env.Env), Timeout: timeout, Source: source.Name,
	}
}

func displayArg(value string) string {
	if value != "" && strings.IndexFunc(value, displayArgNeedsQuoting) == -1 {
		return value
	}
	if strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		var quoted strings.Builder
		quoted.WriteString("$'")
		for _, char := range []byte(value) {
			switch char {
			case '\\', '\'':
				quoted.WriteByte('\\')
				quoted.WriteByte(char)
			case '\n':
				quoted.WriteString(`\n`)
			case '\r':
				quoted.WriteString(`\r`)
			case '\t':
				quoted.WriteString(`\t`)
			default:
				if char < 0x20 || char == 0x7f {
					_, _ = fmt.Fprintf(&quoted, `\x%02x`, char)
				} else {
					quoted.WriteByte(char)
				}
			}
		}
		quoted.WriteByte('\'')
		return quoted.String()
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func displayArgNeedsQuoting(char rune) bool {
	switch {
	case char >= 'a' && char <= 'z':
		return false
	case char >= 'A' && char <= 'Z':
		return false
	case char >= '0' && char <= '9':
		return false
	default:
		return !strings.ContainsRune("_@%+=:,./-", char)
	}
}

func substitute(template string, values map[string]string) string {
	replacements := make([]string, 0, len(values)*2)
	for name, value := range values {
		replacements = append(replacements, "{{"+name+"}}", value)
	}
	return strings.NewReplacer(replacements...).Replace(template)
}

func homeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// EnsureBinDir creates the base's script directory so a `run:` line that calls a helper does
// not fail on a base scaffolded before that preset existed.
func EnsureBinDir(root string) error {
	directory := filepath.Join(root, core.BaseBinDir)
	if err := core.ValidateDirectoryConfinement(directory); err != nil {
		return err
	}
	return os.MkdirAll(directory, core.BaseDirMode)
}
