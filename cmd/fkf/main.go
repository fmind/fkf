// Command fkf is the whole program: one binary over a folder of plain JSON and Markdown.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
)

func main() {
	core.ConfigureLogging()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	app := newApp(stdout, stderr)
	// Keep format auto-detection on the original writer, then put one cancellation boundary in
	// front of every renderer. An exhaustive query can produce a large result after its scan;
	// once SIGINT cancels ctx, no later JSON, JSONL, text, or help write may keep draining it.
	app.Writer = contextWriter{ctx: ctx, writer: stdout}
	if err := app.Run(ctx, args); err != nil {
		writeCLIError(stderr, err)
		return exitCodeFor(err)
	}
	return ExitSuccess
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (writer contextWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	return writer.writer.Write(data)
}

func writeCLIError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintln(stderr, "fkf: "+err.Error())
}

// Commands are grouped by the question you came with, because that is what a reader of `--help`
// is looking for. What an invocation may DO is the other thing worth knowing, so the two groups
// that only read say so in their heading and the individual commands that write the base or run
// a declared command carry markWrite or markRun at the end of their usage line. Trust is a
// property of the leaf — `build graph` writes inside a group that otherwise does not — and a
// marker on the leaf is the only way to say that without a paragraph of prose.
const (
	groupAsk     = "Ask the base — retrieval, reads only"
	groupInspect = "Inspect and explore — reads only"
	groupRun     = "Run and set up"

	markWrite = "  [writes the base]"
	markRun   = "  [runs the commands you trusted]"
)

func newApp(stdout, stderr io.Writer) *cli.Command {
	app := &cli.Command{
		Name:  "fkf",
		Usage: "Fmind Knowledge Framework — a local, offline record of your work, for your agent.",
		Description: "A base is one git repository of plain JSON and Markdown, addressed by relative URIs.\n" +
			"\n" +
			"  fkf init ~/brain --demo 30       create a base you can explore safely\n" +
			"  fkf status                       inspect an existing base and its collectors\n" +
			"  fkf sync --days 7                collect completed days that are missing\n" +
			"  fkf context \"FK-412 retrieval\"   select the best evidence under a budget\n" +
			"  fkf read wiki/retrieval.md       open exactly one URI\n" +
			"\n" +
			"find and context are the two halves of retrieval: find is exhaustive, context fits a\n" +
			"budget. read opens one thing, graph says what it connects to.\n" +
			"\n" +
			"Run `fkf <command> --help` for that command's arguments, examples, and safety boundary.\n" +
			"\n" +
			"Exit codes are stable: 0 success, 1 partial or operational failure, 2 invalid\n" +
			"configuration or usage, 3 an untrusted base, 130 cancellation.",
		Version:               core.Version,
		Reader:                os.Stdin,
		Writer:                stdout,
		ErrWriter:             stderr,
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "base", Aliases: []string{"b"},
				Usage: "Base directory. Falls back to $FKF_BASE, then the nearest ancestor holding fkf.yaml.",
			},
			&cli.StringFlag{
				Name: "format", Aliases: []string{"f"}, Value: defaultFormat(stdout),
				DefaultText: "text at a terminal, json when piped",
				Usage:       "Output format: json, jsonl, or text.",
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if err := validateFormat(cmd.String("format")); err != nil {
				return ctx, invalidUsage(err)
			}
			// An unknown command is a usage error, and saying so here is what makes it exit 2.
			// Left to the framework it falls through to help and exits with the wrong code.
			if first := cmd.Args().First(); first != "" && cmd.Command(first) == nil {
				return ctx, invalidUsage(fmt.Errorf("unknown command %q; run `fkf --help` for the command list", first))
			}
			return ctx, nil
		},
		// Returning the error keeps construction testable: run owns diagnostics and the exit
		// code, so urfave/cli never calls os.Exit from inside an action.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
	}
	usageError := func(_ context.Context, _ *cli.Command, err error, _ bool) error { return invalidUsage(err) }
	app.OnUsageError = usageError
	app.Commands = []*cli.Command{
		// Ask the base.
		newContextCommand(), newFindCommand(), newReadCommand(), newGraphCommand(),
		// Inspect and explore.
		newListCommand(), newValidateCommand(), newTagsCommand(),
		// Run and set up.
		newInitCommand(), newTrustCommand(), newTestCommand(), newSyncCommand(), newStatusCommand(),
		newBuildCommand(), newNewCommand(), newConfigCommand(), newMCPCommand(), newUpgradeCommand(),
	}
	// urfave/cli consults the failing command's own hook rather than walking up to the root, so
	// a mistyped flag on a subcommand would otherwise exit 1 — "partial failure" — telling a
	// script that some work succeeded when none was attempted.
	applyUsageErrorHandler(app.Commands, usageError)
	applyParentAction(app.Commands)
	applyArityGuard(app.Commands)
	return app
}

func applyUsageErrorHandler(commands []*cli.Command, handler cli.OnUsageErrorFunc) {
	for _, command := range commands {
		command.OnUsageError = handler
		applyUsageErrorHandler(command.Commands, handler)
	}
}

// applyArityGuard refuses a stray argument on every command whose grammar declares none.
// urfave/cli discards extra arguments silently, which is merely untidy on a listing and unsafe
// on the commands that act: `fkf trust check` — a plausible typo, since `build graph` and `build
// wiki` are real commands — recorded this machine's trust in every declared command and
// exited 0, and `build graph STRAY` and `build wiki STRAY` wrote the base.
//
// It lives here rather than in fourteen actions for the reason the usage-error handler does:
// a rule applied per command is a rule the fifteenth command forgets. ArgsUsage is the
// declaration — a command that takes an argument names it there — so the guard needs no list
// to maintain, and the suggestion is derived from the command's own layer grammar rather
// than restated beside it.
func applyArityGuard(commands []*cli.Command) {
	for _, command := range commands {
		applyArityGuard(command.Commands)
		if command.ArgsUsage != "" || command.Action == nil {
			continue
		}
		action, suggestion := command.Action, strayArgumentSuggestion(command)
		command.Action = func(ctx context.Context, cmd *cli.Command) error {
			if err := requireNoArgs(cmd, suggestion); err != nil {
				return err
			}
			return action(ctx, cmd)
		}
	}
}

// applyParentAction gives every command that is only a container for subcommands an action of
// its own. Without one, urfave/cli falls through to its help printer, which answers an unknown
// subcommand with an error carrying its OWN hardcoded exit code 3 — the code fkf documents as
// "an untrusted base". `fkf mcp bogus` therefore told a script that a base had not been trusted
// when the only thing wrong was a typo, and no mapping in exitCodeFor could tell the two apart,
// because by then both are just an ExitCoder holding 3.
func applyParentAction(commands []*cli.Command) {
	for _, command := range commands {
		applyParentAction(command.Commands)
		if len(command.Commands) == 0 || command.Action != nil {
			continue
		}
		command.Action = func(_ context.Context, cmd *cli.Command) error {
			if stray := cmd.Args().First(); stray != "" {
				return invalidUsage(fmt.Errorf("%s has no subcommand %q; run `%s --help` for the list",
					cmd.FullName(), stray, cmd.FullName()))
			}
			// A bare invocation is somebody asking what the command is for, which its own help
			// answers and a one-line usage string does not.
			return showHelp(cmd, "name a subcommand")
		}
	}
}

// strayArgumentSuggestion names the read a stray argument probably meant. A layer command IS
// its listing, so the argument is almost always the target wanted; suggesting universal fkf read
// guides the user to the unified address space.
func strayArgumentSuggestion(command *cli.Command) string {
	switch command.Name {
	case "wiki", "projects":
		return fmt.Sprintf("fkf read %s/<slug>.md", command.Name)
	case "tasks":
		return "fkf read tasks/<date>/<slug>/TASKS.md"
	case "events":
		return "fkf read events/<date>/<source>.json"
	case "index":
		return "fkf read index/<name>.json"
	default:
		return ""
	}
}
