package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/mcpserver"
	"github.com/fmind/fkf/services"
)

func errUsage(format string, args ...any) error { return fmt.Errorf(format, args...) }

func newUpgradeCommand() *cli.Command {
	return &cli.Command{
		Name: "upgrade", Aliases: []string{"u"}, Category: groupRun,
		Usage: "Install the latest verified release over this executable.  [replaces the fkf binary]",
		Description: "Uses curl only for fixed GitHub release endpoints, verifies the selected archive " +
			"against checksums.txt, runs its fkf --version, and atomically replaces this executable. " +
			"It reads no base and runs no source command.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate current FKF executable: %w", err)
			}
			return render(cmd)(services.Upgrade(ctx, executable))
		},
	}
}

func newInitCommand() *cli.Command {
	return &cli.Command{
		Name: "init", Category: groupRun,
		Usage:     "Create a base, or refresh the parts of one that fkf owns." + markWrite,
		ArgsUsage: "[path]",
		Description: "On a new path this writes the chosen preset's fkf.yaml, the enabled layers, " +
			"managed git blocks, AGENTS.md, two skills, helpers required by enabled sources, and agent " +
			"bridges. It records trust only when no execution input predated init. On an existing base " +
			"it refreshes the skills and managed blocks, creates missing agent bridges, and preserves " +
			"fkf.yaml, AGENTS.md, and helpers; `fkf config helpers --refresh` installs newly required helpers.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "preset", Value: services.PresetMinimal, Usage: "minimal, personal, or team."},
			&cli.StringFlag{Name: "name", Usage: "Base name (default: the directory name)."},
			&cli.BoolFlag{Name: "track-collected", Usage: "Track events/ and index/ in git. History is append-only."},
			&cli.IntFlag{Name: "demo", Usage: "Fill an empty base with 1 to 366 days of synthetic documents."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireAtMostOneArg(cmd, "fkf init [path] [--preset minimal|personal|team]"); err != nil {
				return err
			}
			if err := requirePositiveIntFlagAtMost(cmd, "demo", 366); err != nil {
				return err
			}
			target := cmd.Args().First()
			if target == "" {
				target = cmd.Root().String("base")
			}
			if target == "" {
				return invalidUsage(errUsage("usage: fkf init <path> [--preset minimal|personal|team]"))
			}
			preset := cmd.String("preset")
			if cmd.Int("demo") > 0 && !cmd.IsSet("preset") {
				// An omitted preset and --demo intentionally select the same minimal contract.
				preset = ""
			}
			return withWriterLock(ctx, target, func() error {
				return render(cmd)(services.Init(ctx, services.InitRequest{
					Path: target, Preset: preset, Name: cmd.String("name"),
					TrackCollected: cmd.Bool("track-collected"), Demo: cmd.Int("demo"),
				}, time.Now))
			})
		},
	}
}

func newTrustCommand() *cli.Command {
	return &cli.Command{
		Name: "trust", Category: groupRun,
		Usage: "Review every declared run:, test:, and body: command, body-bound field path, enabled state, " +
			"invocation policy, bin: PATH directories, and every helper or hook under bin/ and tests/; " +
			"then record trust." + markWrite,
		Description: "Reading the commands is the act of trusting them, so the listing is part of " +
			"this command rather than something you are told to do first. --check prints the state " +
			"and records nothing. A base that was trusted before and has changed leads with what " +
			"changed, because a 300-line re-read is a review nobody performs; --all prints it all.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "check", Usage: "Report the trust state without recording it."},
			&cli.BoolFlag{Name: "all", Usage: "Print the complete disclosure: bin:, every source command, body-bound field path and policy, and every helper or hook in both execution trees, even when only a few changed."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			run := func() error {
				report, err := services.Trust(ctx, base, !cmd.Bool("check"), cmd.Bool("all"))
				if err := emit(cmd, report, err); err != nil {
					return err
				}
				if cmd.Bool("check") && !report.State.Trusted {
					return cli.Exit(base.RequireTrust(ctx), ExitUntrusted)
				}
				return nil
			}
			if cmd.Bool("check") {
				return run()
			}
			return withWriterLock(ctx, base.Root(), run)
		},
	}
}

func newTestCommand() *cli.Command {
	return &cli.Command{
		Name: "test", Category: groupRun,
		Usage:     "Run trusted source verification hooks." + markRun,
		ArgsUsage: "[source...]",
		Description: "With no arguments, runs hooks declared by enabled sources in stable name order. " +
			"Named sources run even when disabled; --all includes every source that declares a hook. " +
			"An empty selection is a successful 0/0 report. Hooks are direct argv, receive only {{base}} and {{home}}, " +
			"search the trusted tests/ tree first, and never collect or write evidence.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "Test every source that declares a hook, including disabled sources."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Bool("all") && cmd.Args().Len() > 0 {
				return invalidUsage(errUsage("fkf test --all cannot be combined with source names"))
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.TestSources(ctx, base, services.SourceTestRequest{
				Targets: cmd.Args().Slice(), All: cmd.Bool("all"),
			})
			if err := emit(cmd, report, err); err != nil {
				return err
			}
			if !report.Complete {
				return partialFailure(fmt.Errorf("%d source test(s) failed:\n%s", report.Failed, report.FailureSummary()))
			}
			return nil
		},
	}
}

func newSyncCommand() *cli.Command {
	return &cli.Command{
		Name: "sync", Aliases: []string{"s"}, Category: groupRun,
		Usage:     "Collect the completed days that are missing." + markWrite + markRun,
		ArgsUsage: "[source...]",
		Description: "Today is never collected: a day is complete or absent, never partial. " +
			"A base you did not create on this machine must be trusted once, with `fkf trust`, " +
			"before any declared command runs.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "days", Usage: "Completed local days to cover, 1 to 366 (default: sync.days)."},
			&cli.StringFlag{Name: "date", Usage: "Collect exactly this completed day (YYYY-MM-DD)."},
			&cli.BoolFlag{Name: "force", Usage: "Re-collect days that already have a document."},
			&cli.BoolFlag{Name: "dry-run", Usage: "Print every substituted command and execute none."},
			&cli.BoolFlag{Name: "preview", Usage: "Run and validate exactly one source, show up to three projected records, and write nothing." + markRun},
			&cli.BoolFlag{Name: "no-graph", Usage: "Skip the derived edge-list rebuild."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requirePositiveIntFlagAtMost(cmd, "days", 366); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			run := func() error {
				report, err := services.Sync(ctx, base, services.SyncRequest{
					Targets: cmd.Args().Slice(), Days: cmd.Int("days"), Date: cmd.String("date"),
					Force: cmd.Bool("force"), DryRun: cmd.Bool("dry-run"), NoGraph: cmd.Bool("no-graph"),
					Preview: cmd.Bool("preview"),
				})
				if err := emit(cmd, report, err); err != nil {
					return err
				}
				if !report.Complete {
					return partialFailure(fmt.Errorf("%d unit(s) failed:\n%s", report.Failed, report.FailureSummary()))
				}
				return nil
			}
			if cmd.Bool("dry-run") || cmd.Bool("preview") {
				return run()
			}
			return withWriterLock(ctx, base.Root(), run)
		},
	}
}

func newStatusCommand() *cli.Command {
	return &cli.Command{
		Name: "status", Category: groupRun,
		Usage: "Inspect base overview, collector status, and repository health.",
		Description: "Unifies the whole-base overview, source collector status, and health audits: " +
			"git tracked files, permissions, skill drift, and JSON document schema verification.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "max-age-hours", Usage: fmt.Sprintf("Exit 1 when any enabled source is missing or older than this, 1 to %d.", core.MaxFreshnessAgeHours)},
			&cli.BoolFlag{Name: "all", Usage: "Include all sources in full detail."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requirePositiveIntFlagAtMost(cmd, "max-age-hours", core.MaxFreshnessAgeHours); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			run := func() error {
				status, err := services.Report(ctx, base, services.StatusRequest{
					MaxAgeHours: cmd.Int("max-age-hours"), All: cmd.Bool("all"),
				})
				if err := emit(cmd, status, err); err != nil {
					return err
				}
				if status.Stale {
					return partialFailure(errUsage("one or more enabled sources are missing or older than --max-age-hours %d",
						cmd.Int("max-age-hours")))
				}
				if !status.OK {
					return partialFailure(errUsage("status found %d error(s) in %s", status.Errors, base.Root()))
				}
				return nil
			}
			return run()
		},
	}
}

func newBuildCommand() *cli.Command {
	return &cli.Command{
		Name: "build", Aliases: []string{"b"}, Category: groupRun,
		Usage:     "Rebuild derived caches: graph, wiki index, or all." + markWrite,
		ArgsUsage: "[target]",
		UsageText: usageLines(
			[2]string{"fkf build", "rebuild all derived caches (graph and wiki index)"},
			[2]string{"fkf build graph", "rescan the base and rebuild graph.tsv at the base root"},
			[2]string{"fkf build wiki [--check]", "regenerate the generated block in wiki/index.md"},
		),
		Description: "Rebuilds derived caches from base content. Derived files are rebuildable caches " +
			"and never the source of truth. --check with wiki reports whether wiki/index.md is stale and writes nothing.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "check", Usage: "Report whether the target is stale and write nothing (wiki only)."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireAtMostOneArg(cmd, "fkf build [graph|wiki|all] [--check]"); err != nil {
				return err
			}
			target := cmd.Args().First()
			if target != "" && target != "graph" && target != "wiki" && target != "all" {
				return invalidUsage(fmt.Errorf("unknown build target %q; expected graph, wiki, or all", target))
			}
			if cmd.Bool("check") && target != "wiki" {
				return invalidUsage(errors.New("--check is supported only by `fkf build wiki`; graph checks are not implemented"))
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			run := func() error {
				report, err := services.Build(ctx, base, target, cmd.Bool("check"))
				if err := emit(cmd, report, err); err != nil {
					return err
				}
				if report.Wiki != nil && report.Wiki.Stale && cmd.Bool("check") {
					return partialFailure(errUsage("%s is out of date; run `fkf build wiki`", report.Wiki.URI))
				}
				return nil
			}
			if cmd.Bool("check") {
				return run()
			}
			return withWriterLock(ctx, base.Root(), run)
		},
	}
}

func newNewCommand() *cli.Command {
	return &cli.Command{
		Name: "new", Aliases: []string{"n"}, Category: groupRun,
		Usage: "Scaffold a task trace, project page, wiki concept, or source helper." + markWrite,
		Commands: []*cli.Command{
			{
				Name: "task", Aliases: []string{"t"},
				Usage:     "Create a new task trace in tasks/YYYY-MM-DD/<slug>/TASKS.md." + markWrite,
				ArgsUsage: "<slug>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "title", Usage: "Human title (default derived from slug)."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := requireOneArg(cmd, "fkf new task <slug> [--title \"...\"]"); err != nil {
						return err
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return withWriterLock(ctx, base.Root(), func() error {
						return render(cmd)(services.CreateNew(base, services.NewRequest{
							Kind: services.NewKindTask, Slug: cmd.Args().First(), Title: cmd.String("title"),
						}))
					})
				},
			},
			{
				Name: "project", Aliases: []string{"p"},
				Usage:     "Create a new project page in projects/<slug>.md; requires at least one --tag." + markWrite,
				ArgsUsage: "<slug>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "title", Usage: "Human title (default derived from slug)."},
					&cli.StringSliceFlag{Name: "tag", Usage: "Required frontmatter tag (repeatable)."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := requireOneArg(cmd, "fkf new project <slug> [--title \"...\"] [--tag ...]"); err != nil {
						return err
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return withWriterLock(ctx, base.Root(), func() error {
						return render(cmd)(services.CreateNew(base, services.NewRequest{
							Kind: services.NewKindProject, Slug: cmd.Args().First(),
							Title: cmd.String("title"), Tags: cmd.StringSlice("tag"),
						}))
					})
				},
			},
			{
				Name: "helper", Aliases: []string{"h"},
				Usage:     "Create a portable, fail-closed /bin/sh source helper in bin/." + markWrite,
				ArgsUsage: "<name>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := requireOneArg(cmd, "fkf new helper <name>"); err != nil {
						return err
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return withWriterLock(ctx, base.Root(), func() error {
						return render(cmd)(services.CreateNew(base, services.NewRequest{
							Kind: services.NewKindHelper, Slug: cmd.Args().First(),
						}))
					})
				},
			},
			{
				Name: "wiki", Aliases: []string{"w"},
				Usage:     "Create a new wiki concept in wiki/<slug>.md; requires at least one --tag." + markWrite,
				ArgsUsage: "<slug>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "type", Value: "decision", Usage: "decision, pattern, tool, insight, person, …"},
					&cli.StringFlag{Name: "title", Usage: "Human title (default derived from slug)."},
					&cli.StringSliceFlag{Name: "tag", Usage: "Required frontmatter tag (repeatable)."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := requireOneArg(cmd, "fkf new wiki <slug> [--type ...] [--title \"...\"] [--tag ...]"); err != nil {
						return err
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return withWriterLock(ctx, base.Root(), func() error {
						return render(cmd)(services.CreateNew(base, services.NewRequest{
							Kind: services.NewKindWiki, Slug: cmd.Args().First(),
							Type: cmd.String("type"), Title: cmd.String("title"), Tags: cmd.StringSlice("tag"),
						}))
					})
				},
			},
		},
	}
}

func newConfigCommand() *cli.Command {
	return &cli.Command{
		Name: "config", Category: groupRun,
		Usage: "Print the resolved configuration, or the JSON Schema fkf.yaml is validated against.",
		Action: func(_ context.Context, cmd *cli.Command) error {
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			return emit(cmd, base.Config, nil)
		},
		Commands: []*cli.Command{
			{
				Name: "helpers", Aliases: []string{"h"},
				Usage: "Compare official helpers with this binary, or explicitly refresh them.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "refresh", Usage: "Restore drifted installed helpers and missing official helpers required by this base, replacing each file atomically." + markWrite},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					run := func() error {
						return render(cmd)(services.InspectHelpers(ctx, base, cmd.Bool("refresh")))
					}
					if cmd.Bool("refresh") {
						return withWriterLock(ctx, base.Root(), run)
					}
					return run()
				},
			},
			{
				Name: "schema", Aliases: []string{"s"},
				Usage: "Print the JSON Schema for fkf.yaml, for editor completion.",
				Action: func(_ context.Context, cmd *cli.Command) error {
					encoded, err := core.EncodeConfigSchema()
					if err != nil {
						return err
					}
					_, err = cmd.Root().Writer.Write(encoded)
					return err
				},
			},
		},
	}
}

func newMCPCommand() *cli.Command {
	return &cli.Command{
		Name: "mcp", Aliases: []string{"m"}, Category: groupRun,
		Usage: "Serve this base to an agent over MCP, read-only.",
		Commands: []*cli.Command{
			{
				Name: "serve", Aliases: []string{"s"},
				Usage: "Run the read-only stdio server. --base is required.",
				Description: "Exposes context, find, list, read, and graph, plus four resources. " +
					"It cannot write, shell, or fetch, and it never exposes `read --body`.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: "base", Aliases: []string{"b"}, Required: true, Local: true,
						Usage: "Base directory. Required so a launch line always says what the agent can see.",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := services.Open(cmd.String("base"))
					if err != nil {
						return err
					}
					return mcpserver.Serve(ctx, base)
				},
			},
			{
				Name: "instructions", Aliases: []string{"i"},
				Usage: "Print the instructions this base would send to a connecting client.",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					instructions, err := mcpserver.Instructions(ctx, base)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(cmd.Root().Writer, instructions)
					return err
				},
			},
		},
	}
}
