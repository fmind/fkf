package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

// Exit codes are part of the contract: a timer reads them, and so does a shell script.
const (
	ExitSuccess      = 0
	ExitPartial      = 1
	ExitInvalidUsage = 2
	ExitUntrusted    = 3
	ExitCanceled     = 130
)

const (
	formatJSON  = "json"
	formatJSONL = "jsonl"
	formatText  = "text"
)

func invalidUsage(err error) error { return cli.Exit(err, ExitInvalidUsage) }

func partialFailure(err error) error { return cli.Exit(err, ExitPartial) }

// exitCodeFor maps an error to its documented code. Configuration and usage problems, and an
// untrusted base, get their own codes so a caller can tell "you asked wrong" from "it broke".
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return ExitCanceled
	case errors.Is(err, core.ErrUntrusted):
		return ExitUntrusted
	case errors.Is(err, core.ErrConfig), errors.Is(err, core.ErrNoBase),
		errors.Is(err, core.ErrPathEscapes), errors.Is(err, services.ErrURI),
		errors.Is(err, core.ErrNotAddressable), errors.Is(err, services.ErrJQExpression),
		errors.Is(err, services.ErrContextBudgetTooSmall):
		return ExitInvalidUsage
	}
	var disabled core.ErrLayerDisabled
	if errors.As(err, &disabled) {
		return ExitInvalidUsage
	}
	var coder cli.ExitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode()
	}
	return ExitPartial
}

// defaultFormat fits the output to the caller instead of asking them to say so: a person at a
// terminal gets the human rendering, and anything piped, redirected, or launched from a hook
// gets JSON. A character device is the whole question, so it is answered with os.Stat rather
// than a terminal library.
func defaultFormat(writer io.Writer) string {
	file, ok := writer.(*os.File)
	if !ok {
		return formatJSON
	}
	info, err := file.Stat()
	if err != nil {
		return formatJSON
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return formatText
	}
	return formatJSON
}

func validateFormat(value string) error {
	switch value {
	case formatJSON, formatJSONL, formatText:
		return nil
	default:
		return fmt.Errorf("invalid format %q; expected json, jsonl, or text", value)
	}
}

// openBase resolves and loads the base once per command.
func openBase(cmd *cli.Command) (*services.Base, error) {
	return services.Open(cmd.Root().String("base"))
}

// requireNoArgs guards every command whose grammar has no argument. urfave/cli discards extra
// arguments silently, which is harmless on a listing and dangerous on the three commands that
// act: `fkf trust check` — a plausible typo, since `build graph` and `wiki tags` are real
// subcommands — recorded this machine's trust in every declared command and exited 0.
//
// suggestion names the read that was probably meant, because a stray argument on a listing is
// almost always a slug. It is empty for a command that has no such neighbour, where "takes no
// arguments" is the whole of the answer.
func requireNoArgs(cmd *cli.Command, suggestion string) error {
	if cmd.Args().Len() == 0 {
		return nil
	}
	stray := cmd.Args().First()
	switch {
	case suggestion != "":
		return invalidUsage(fmt.Errorf("%s takes no arguments; did you mean `%s`?",
			cmd.FullName(), suggestion))
	case hasSubcommands(cmd):
		// A command that has subcommands was almost certainly given a misspelt one, not an
		// argument, so the error names the vocabulary rather than the arity.
		return invalidUsage(fmt.Errorf("%s has no subcommand %q; run `%s --help` for the list",
			cmd.FullName(), stray, cmd.FullName()))
	default:
		return invalidUsage(fmt.Errorf("%s takes no arguments, and %q is not one",
			cmd.FullName(), stray))
	}
}

// hasSubcommands ignores the help command urfave/cli adds to everything: counting it would
// make every leaf look like a parent, and answer `fkf status STRAY` with "no subcommand".
func hasSubcommands(cmd *cli.Command) bool {
	for _, sub := range cmd.Commands {
		if sub.Name != "help" {
			return true
		}
	}
	return false
}

// showHelp answers with the command's own help — the same text `--help` prints — plus one
// line naming what was missing, and exits 2. A one-line usage string is the right answer to a
// malformed argument; it is the wrong answer to a bare invocation, because a bare invocation
// is somebody asking what the command is for, and replying with its own grammar tells them
// nothing they did not already fail to guess.
func showHelp(cmd *cli.Command, reason string) error {
	template := cli.CommandHelpTemplate
	if len(cmd.VisibleCommands()) > 0 {
		template = cli.SubcommandHelpTemplate
	}
	cli.HelpPrinter(cmd.Root().Writer, template, cmd)
	return invalidUsage(errors.New(reason))
}

// requireOneArg turns an arity mistake into a usage error naming the correct form.
func requireOneArg(cmd *cli.Command, usage string) error {
	if cmd.Args().Len() != 1 {
		return invalidUsage(errors.New("usage: " + usage))
	}
	return nil
}

// requireAtMostOneArg protects optional singular arguments. ArgsUsage documents the grammar,
// but urfave/cli does not enforce it and silently leaves extra values for an action to ignore.
// A command that writes must reject that ambiguity before resolving or touching its target.
func requireAtMostOneArg(cmd *cli.Command, usage string) error {
	if cmd.Args().Len() > 1 {
		return invalidUsage(errors.New("usage: " + usage))
	}
	return nil
}

// Integer flags use zero as urfave/cli's "not supplied" value, so their domain cannot live in
// a flag default. Validate an explicitly supplied value before opening a base or doing work;
// this keeps a typo a usage failure and preserves zero only where the help publishes it.
func requirePositiveIntFlagAtMost(cmd *cli.Command, name string, maximum int) error {
	if !cmd.IsSet(name) {
		return nil
	}
	value := cmd.Int(name)
	if value < 1 || value > maximum {
		return invalidUsage(fmt.Errorf("--%s is %d; expected 1..%d", name, value, maximum))
	}
	return nil
}

func requirePositiveIntFlag(cmd *cli.Command, name string) error {
	if !cmd.IsSet(name) || cmd.Int(name) > 0 {
		return nil
	}
	return invalidUsage(fmt.Errorf("--%s is %d; expected a positive integer", name, cmd.Int(name)))
}

func requireNonNegativeLimit(cmd *cli.Command) error {
	if !cmd.IsSet("limit") || cmd.Int("limit") >= 0 {
		return nil
	}
	return invalidUsage(fmt.Errorf("--limit is %d; expected zero or a positive integer", cmd.Int("limit")))
}

// emit renders a result in the requested format. Text rendering is explicit per type: a
// --format text that silently fell back to JSON for most commands looked like it worked, and
// a flag that appears to work is harder to diagnose than one that says it did not.
func emit(cmd *cli.Command, result any, err error) error {
	if err != nil {
		return err
	}
	writer := cmd.Root().Writer
	switch cmd.Root().String("format") {
	case formatJSONL:
		return writeJSONLines(writer, result)
	case formatText:
		if rendered, ok := writeText(writer, result); ok {
			return rendered
		}
		_, _ = fmt.Fprintln(cmd.Root().ErrWriter, "fkf: no text rendering for this result; showing JSON")
		return writeJSON(writer, result)
	default:
		return writeJSON(writer, result)
	}
}

func writeJSON(writer io.Writer, result any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}

// writeJSONLines streams the natural collection inside an envelope one item per line, which is
// what makes `fkf find --format jsonl | duckdb` work. A result with no obvious collection
// stays on one compact line rather than being silently dropped.
func writeJSONLines(writer io.Writer, result any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	// Context is one budgeted report, like day and timeline. Keeping its items and receipt in
	// one compact record makes the bytes the service accounted for the bytes actually emitted.
	if _, ok := result.(*services.ContextPack); ok {
		return encoder.Encode(result)
	}
	if stream, ok := streamOf(result); ok {
		for index := range stream.Len() {
			if err := encoder.Encode(stream.Index(index).Interface()); err != nil {
				return fmt.Errorf("encode JSONL item %d: %w", index, err)
			}
		}
		return nil
	}
	return encoder.Encode(result)
}

// streamOf finds the one collection a result is really about, so jsonl streams records rather
// than a single envelope holding them.
func streamOf(result any) (reflect.Value, bool) {
	if stream, ok := readStreamOf(result); ok {
		return stream, true
	}
	if stream, ok := operationStreamOf(result); ok {
		return stream, true
	}
	value := reflect.ValueOf(result)
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
		return value, true
	}
	return reflect.Value{}, false
}

func readStreamOf(result any) (reflect.Value, bool) {
	switch typed := result.(type) {
	case *services.FindResult:
		// A --count result is about the volumes; anything else is about the items, and a
		// searching find has two kinds of them. They are streamed through one []any in the
		// order the text rendering prints, so `--format jsonl` never silently drops the page
		// half of a result the help says it streams.
		if len(typed.Pages) > 0 {
			items := make([]any, 0, len(typed.Pages)+len(typed.Records))
			for _, page := range typed.Pages {
				items = append(items, page)
			}
			for _, record := range typed.Records {
				items = append(items, record)
			}
			return reflect.ValueOf(items), true
		}
		if len(typed.Records) > 0 || len(typed.Volumes) == 0 {
			return reflect.ValueOf(typed.Records), true
		}
		return reflect.ValueOf(typed.Volumes), true
	case *services.Neighbourhood:
		return reflect.ValueOf(typed.Edges), true
	case *services.NodeListing:
		return reflect.ValueOf(typed.Nodes), true
	case *sources.Document:
		return reflect.ValueOf(typed.Records), true
	default:
		return reflect.Value{}, false
	}
}

func operationStreamOf(result any) (reflect.Value, bool) {
	switch typed := result.(type) {
	case *services.PageListing:
		return reflect.ValueOf(typed.Pages), true
	case *services.EventListing:
		return reflect.ValueOf(typed.Days), true
	case *services.IndexListing:
		return reflect.ValueOf(typed.Entries), true
	case *services.TaskListing:
		return reflect.ValueOf(typed.Traces), true
	case *services.SearchResult:
		return reflect.ValueOf(typed.Hits), true
	case *services.SyncReport:
		return reflect.ValueOf(typed.Units), true
	case *services.SourceTestReport:
		return reflect.ValueOf(typed.Sources), true
	case *services.ValidationReport:
		return reflect.ValueOf(typed.Issues), true
	case *services.RecordTitleReport:
		return reflect.ValueOf(typed.Issues), true
	case *services.Status:
		if len(typed.Findings) > 0 {
			return reflect.ValueOf(typed.Findings), true
		}
		return reflect.ValueOf(typed.Sources), true
	default:
		return reflect.Value{}, false
	}
}

// render is the two-value form of emit: `return render(cmd)(services.ListEvents(...))`. It
// exists because Go forbids mixing a fixed argument with a multi-value call, and spelling every
// read as two statements added a line of noise to twenty commands.
func render(cmd *cli.Command) func(any, error) error {
	return func(result any, err error) error { return emit(cmd, result, err) }
}
