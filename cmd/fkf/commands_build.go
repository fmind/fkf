package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newBuildCommand() *cli.Command {
	return &cli.Command{
		Name: "build", Aliases: []string{"b"}, Category: groupRun,
		Usage:     "Rebuild derived caches: graph, lexical index, wiki index, body cache, or all." + markWrite,
		ArgsUsage: "[target]",
		UsageText: usageLines(
			[2]string{"fkf build", "rebuild all derived caches (wiki, graph, and lexical index)"},
			[2]string{"fkf build graph", "rescan the base and rebuild graph.tsv at the base root"},
			[2]string{"fkf build index", "rebuild the ignored lexical postings cache"},
			[2]string{"fkf build wiki [--check]", "regenerate the generated block in wiki/index.md"},
			[2]string{"fkf build bodies --prune", "empty the ignored, rebuildable record-body cache"},
		),
		Description: "Rebuilds derived caches from base content. Derived files are rebuildable caches " +
			"and never the source of truth. --check with wiki reports whether wiki/index.md is stale and writes nothing.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "check", Usage: "Report whether the target is stale and write nothing (wiki only)."},
			&cli.BoolFlag{Name: "if-stale", Usage: "Return without taking the writer lock when every selected derived cache is current."},
			&cli.BoolFlag{Name: "prune", Usage: "Confirm that `fkf build bodies` should empty the body cache."},
		},
		Action: runBuild,
	}
}

func runBuild(ctx context.Context, cmd *cli.Command) error {
	if err := requireAtMostOneArg(cmd, "fkf build [bodies|graph|index|wiki|all] [--check]"); err != nil {
		return err
	}
	target := cmd.Args().First()
	if target != "" && target != "bodies" && target != "graph" && target != "index" && target != "wiki" && target != "all" {
		return invalidUsage(fmt.Errorf("unknown build target %q; expected bodies, graph, index, wiki, or all", target))
	}
	if target == "bodies" && !cmd.Bool("prune") {
		return invalidUsage(errors.New("`fkf build bodies` requires --prune because it empties the cache"))
	}
	if target != "bodies" && cmd.Bool("prune") {
		return invalidUsage(errors.New("--prune is supported only by `fkf build bodies`"))
	}
	if cmd.Bool("check") && target != "wiki" {
		return invalidUsage(errors.New("--check is supported only by `fkf build wiki`; graph checks are not implemented"))
	}
	if cmd.Bool("check") && cmd.Bool("if-stale") {
		return invalidUsage(errors.New("--check cannot be combined with --if-stale"))
	}
	if target == "bodies" && cmd.Bool("if-stale") {
		return invalidUsage(errors.New("--if-stale is not supported by `fkf build bodies --prune`"))
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	run := func() error { return renderBuild(ctx, cmd, base, target) }
	if cmd.Bool("check") {
		return run()
	}
	if cmd.Bool("if-stale") {
		stale, err := services.BuildStale(ctx, base, target)
		if err != nil {
			return err
		}
		if !stale {
			return render(cmd)(&services.BuildReport{NothingStale: true}, nil)
		}
	}
	return withWriterLock(ctx, base.Root(), run)
}

func renderBuild(ctx context.Context, cmd *cli.Command, base *services.Base, target string) error {
	var report *services.BuildReport
	var err error
	if cmd.Bool("if-stale") {
		report, err = services.BuildIfStale(ctx, base, target)
	} else {
		report, err = services.Build(ctx, base, target, cmd.Bool("check"))
	}
	if err := emit(cmd, report, err); err != nil {
		return err
	}
	if report.Wiki != nil && report.Wiki.Stale && cmd.Bool("check") {
		return partialFailure(errUsage("%s is out of date; run `fkf build wiki`", report.Wiki.URI))
	}
	return nil
}
