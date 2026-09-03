package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newHarnessCommand() *cli.Command {
	return &cli.Command{
		Name: "harness", Category: groupRun,
		Usage: "Print or install one base's MCP, context-hook, and skill integrations.",
		Description: "Managed user-scope entries preserve unrelated config and refuse an FKF key " +
			"that another command already owns. Every MCP launch pins this executable and base by absolute path.",
		Commands: []*cli.Command{
			{
				Name: "list", Aliases: []string{"l"}, Usage: "List the closed supported harness vocabulary.",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := requireNoArgs(cmd, ""); err != nil {
						return err
					}
					return emitHarnessList(cmd, services.HarnessNames())
				},
			},
			{
				Name: "print", Aliases: []string{"p"}, Usage: "Print the exact managed fragments for a dotfile template.",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := requireOneArg(cmd, "fkf harness print <name>"); err != nil {
						return err
					}
					if !isHarnessName(cmd.Args().First()) {
						return invalidUsage(fmt.Errorf("unknown harness %q; expected %s",
							cmd.Args().First(), strings.Join(services.HarnessNames(), ", ")))
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					executable, err := os.Executable()
					if err != nil {
						return fmt.Errorf("locate current FKF executable: %w", err)
					}
					plan, err := services.HarnessPlanFor(base.Root(), cmd.Args().First(), executable)
					if err != nil {
						return err
					}
					return emitHarnessPlan(cmd, plan)
				},
			},
			{
				Name: "install", Aliases: []string{"i"}, Usage: "Install or verify managed user-scope integrations.",
				ArgsUsage: "<name>...",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "Select every supported harness."},
					&cli.BoolFlag{Name: "dry-run", Usage: "Print exact changes without writing."},
					&cli.BoolFlag{Name: "check", Usage: "Exit 1 when a selected integration is missing or drifted; write nothing."},
				},
				Action: installHarnesses,
			},
		},
	}
}

func installHarnesses(ctx context.Context, cmd *cli.Command) error {
	names := cmd.Args().Slice()
	if cmd.Bool("all") && len(names) > 0 {
		return invalidUsage(errors.New("fkf harness install --all cannot be combined with harness names"))
	}
	if !cmd.Bool("all") && len(names) == 0 {
		return invalidUsage(errors.New("usage: fkf harness install <name>... | --all"))
	}
	if cmd.Bool("check") && cmd.Bool("dry-run") {
		return invalidUsage(errors.New("fkf harness install --check cannot be combined with --dry-run"))
	}
	for _, name := range names {
		if !isHarnessName(name) {
			return invalidUsage(fmt.Errorf("unknown harness %q; expected %s",
				name, strings.Join(services.HarnessNames(), ", ")))
		}
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current FKF executable: %w", err)
	}
	run := func() error {
		report, err := services.InstallHarnesses(ctx, base.Root(), services.HarnessInstallRequest{
			Names: names, All: cmd.Bool("all"), DryRun: cmd.Bool("dry-run"), Check: cmd.Bool("check"),
			Home: os.Getenv("HOME"), Executable: executable,
		})
		if err := emitHarnessInstall(cmd, report, err); err != nil {
			return err
		}
		if cmd.Bool("check") && !report.Complete {
			return partialFailure(fmt.Errorf("%d harness integration change(s) required", len(report.Changes)))
		}
		return nil
	}
	if cmd.Bool("check") || cmd.Bool("dry-run") {
		return run()
	}
	return withWriterLock(ctx, base.Root(), run)
}

func isHarnessName(name string) bool {
	for _, candidate := range services.HarnessNames() {
		if name == candidate {
			return true
		}
	}
	return false
}

func emitHarnessList(cmd *cli.Command, names []string) error {
	if cmd.Root().String("format") != formatText {
		return emit(cmd, map[string]any{"harnesses": names}, nil)
	}
	for _, name := range names {
		if _, err := fmt.Fprintln(cmd.Root().Writer, name); err != nil {
			return err
		}
	}
	return nil
}

func emitHarnessPlan(cmd *cli.Command, plan *services.HarnessPlan) error {
	if cmd.Root().String("format") != formatText {
		return emit(cmd, plan, nil)
	}
	for index, fragment := range plan.Fragments {
		if index > 0 {
			if _, err := fmt.Fprintln(cmd.Root().Writer); err != nil {
				return err
			}
		}
		heading := "# " + fragment.Path + " (" + fragment.Kind
		if fragment.Selector != "" {
			heading += ": " + fragment.Selector
		}
		heading += ")"
		if _, err := fmt.Fprintln(cmd.Root().Writer, heading); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.Root().Writer, fragment.Content); err != nil {
			return err
		}
	}
	for _, note := range plan.Notes {
		if _, err := fmt.Fprintln(cmd.Root().Writer, "\n# Note: "+note); err != nil {
			return err
		}
	}
	return nil
}

func emitHarnessInstall(cmd *cli.Command, report *services.HarnessInstallReport, err error) error {
	if err != nil {
		return err
	}
	if cmd.Root().String("format") != formatText {
		return emit(cmd, report, nil)
	}
	if len(report.Changes) == 0 {
		_, err := fmt.Fprintf(cmd.Root().Writer, "harness %s: current\n", report.Mode)
		return err
	}
	for _, change := range report.Changes {
		if change.Backup != "" {
			if _, err := fmt.Fprintf(cmd.Root().Writer, "backup %s -> %s\n", change.Path, change.Backup); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(cmd.Root().Writer, "%s %s [%s]\n", change.Action, change.Path, change.Harness); err != nil {
			return err
		}
	}
	return nil
}
