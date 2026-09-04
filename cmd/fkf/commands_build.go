package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func newBuildCommand() *cli.Command {
	return &cli.Command{
		Name: "build", Aliases: []string{"b"}, Category: groupRun,
		Usage:     "Rebuild derived caches: graph, lexical index, wiki index, body cache, or all." + markWrite,
		ArgsUsage: "[target]",
		UsageText: usageLines(
			[2]string{"fkf build [--check]", "rebuild all derived caches (wiki, graph, and lexical index)"},
			[2]string{"fkf build graph [--check]", "rescan the base and rebuild graph.tsv at the base root"},
			[2]string{"fkf build index [--check]", "rebuild the ignored lexical postings cache"},
			[2]string{"fkf build wiki [--check]", "regenerate the generated block in wiki/index.md"},
			[2]string{"fkf build bodies --prune [--older-than <dur>] [--source <name>]", "prune the ignored record-body cache"},
		),
		Description: "Rebuilds derived caches from base content. Derived files are rebuildable caches " +
			"and never the source of truth. --check reports whether the target cache is stale and writes nothing.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "check", Usage: "Report whether the selected target is stale and write nothing."},
			&cli.BoolFlag{Name: "if-stale", Usage: "Return without taking the writer lock when every selected derived cache is current."},
			&cli.BoolFlag{Name: "prune", Usage: "Confirm that the body-cache prune should run."},
			&cli.StringFlag{Name: "older-than", Usage: "Prune cached bodies older than this duration (e.g. 30d, 720h; used with bodies --prune)."},
			&cli.StringFlag{Name: "source", Usage: "Prune cached bodies for this source name (used with bodies --prune)."},
		},
		Action: runBuild,
	}
}

func parseOlderThan(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		days, err := strconv.Atoi(daysStr)
		if err != nil || days <= 0 || days > int(math.MaxInt64/(24*time.Hour)) {
			return 0, fmt.Errorf("invalid duration days %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	// Kept apart from the parse failure so %w never wraps a nil error and prints %!w(<nil>).
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", raw)
	}
	return d, nil
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
		return invalidUsage(errors.New("`fkf build bodies` requires --prune because it empties or modifies the cache"))
	}
	if target != "bodies" && cmd.Bool("prune") {
		return invalidUsage(errors.New("--prune is supported only by `fkf build bodies`"))
	}
	if cmd.String("older-than") != "" && target != "bodies" {
		return invalidUsage(errors.New("--older-than is supported only by `fkf build bodies --prune`"))
	}
	if source := cmd.String("source"); source != "" {
		if target != "bodies" {
			return invalidUsage(errors.New("--source is supported only by `fkf build bodies --prune`"))
		}
		if err := core.ValidateSourceName(source); err != nil {
			return invalidUsage(err)
		}
	}
	if cmd.Bool("check") && target == "bodies" {
		return invalidUsage(errors.New("--check is not supported by `fkf build bodies`"))
	}
	if cmd.Bool("check") && cmd.Bool("if-stale") {
		return invalidUsage(errors.New("--check cannot be combined with --if-stale"))
	}
	if cmd.Bool("check") && cmd.Bool("prune") {
		return invalidUsage(errors.New("--check cannot be combined with --prune"))
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
	var pruneOlderThan time.Duration
	if raw := cmd.String("older-than"); raw != "" {
		d, err := parseOlderThan(raw)
		if err != nil {
			return invalidUsage(err)
		}
		pruneOlderThan = d
	}
	pruneOptions := services.PruneBodiesOptions{
		OlderThan: pruneOlderThan,
		Source:    cmd.String("source"),
	}

	var report *services.BuildReport
	var err error
	if cmd.Bool("if-stale") {
		report, err = services.BuildIfStale(ctx, base, target)
	} else {
		report, err = services.BuildWithOptions(ctx, base, services.BuildOptions{
			Target:       target,
			Check:        cmd.Bool("check"),
			PruneOptions: pruneOptions,
		})
	}
	if err := emit(cmd, report, err); err != nil {
		return err
	}
	if cmd.Bool("check") {
		var staleURIs []string
		if report.Wiki != nil && report.Wiki.Stale {
			staleURIs = append(staleURIs, report.Wiki.URI)
		}
		if report.Graph != nil && report.Graph.Stale {
			staleURIs = append(staleURIs, report.Graph.URI)
		}
		if report.Index != nil && report.Index.Stale {
			staleURIs = append(staleURIs, report.Index.URI)
		}
		if len(staleURIs) > 0 {
			targetCmd := "build"
			if target != "" && target != "all" {
				targetCmd = "build " + target
			}
			verb := "is"
			if len(staleURIs) > 1 {
				verb = "are"
			}
			return partialFailure(errUsage("%s %s out of date; run `fkf %s`",
				strings.Join(staleURIs, ", "), verb, targetCmd))
		}
	}
	return nil
}
