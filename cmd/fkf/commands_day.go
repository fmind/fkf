package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newDayCommand() *cli.Command {
	return &cli.Command{
		Name: "day", Aliases: []string{"d"}, Category: groupAsk,
		Usage:     "What happened on one day? Render a chronological, budgeted digest.",
		ArgsUsage: "[date|today|yesterday]",
		UsageText: usageLines(
			[2]string{"fkf day yesterday", "yesterday grouped by source, noisy streams summarized"},
			[2]string{"fkf day 2026-08-28 --all", "one absolute day with noisy rows expanded"},
		),
		Description: "Reads stored event documents only. Identical titles within a source collapse " +
			"to one counted line; people and repositories come from exact typed field values. " +
			"The receipt accounts for every record, the selected delivery budget, and exact JSON and text sizes.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "budget", Value: services.DefaultDigestBudget, Usage: "Hard four-bytes-per-token budget for the complete digest."},
			&cli.BoolFlag{Name: "all", Usage: "Expand noisy sources instead of representing each by one count."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireAtMostOneArg(cmd, "fkf day [date|today|yesterday] [--budget N] [--all]"); err != nil {
				return err
			}
			if err := requirePositiveIntFlagAtMost(cmd, "budget", services.MaxDigestBudget); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.Day(ctx, base, services.DayRequest{
				Date: cmd.Args().First(), Budget: cmd.Int("budget"), All: cmd.Bool("all"),
				DeliveryFormat: cmd.Root().String("format"),
			})
			return emitTimeline(cmd, report, err)
		},
	}
}

func newTimelineCommand() *cli.Command {
	return &cli.Command{
		Name: "timeline", Category: groupAsk,
		Usage:     "What happened in a range or around one record? Render a filtered digest.",
		ArgsUsage: "[record-uri]",
		UsageText: usageLines(
			[2]string{"fkf timeline --since 7d", "the recent range grouped chronologically by source"},
			[2]string{"fkf timeline --since 7d --repo repo:github.com/fmind/fkf", "records with that exact repository URI"},
			[2]string{"fkf timeline <record-uri> --around 2h", "nearby records across every source"},
		),
		Description: "Range filters read only the events layer. --repo and --person match exact typed " +
			"field values. The record form resolves its stored event time and admits records inside " +
			"the inclusive duration on either side; its default is two hours.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
			&cli.StringSliceFlag{Name: "source", Usage: "Restrict to a declared source (repeatable)."},
			&cli.StringFlag{Name: "repo", Usage: "Exact repository entity URI, such as repo:github.com/fmind/fkf."},
			&cli.StringFlag{Name: "person", Usage: "Exact person or actor entity URI."},
			&cli.DurationFlag{Name: "around", Usage: "Duration on either side of the record URI (default 2h)."},
			&cli.IntFlag{Name: "budget", Value: services.DefaultDigestBudget, Usage: "Hard four-bytes-per-token budget for the complete digest."},
			&cli.BoolFlag{Name: "all", Usage: "Expand noisy sources instead of representing each by one count."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireAtMostOneArg(cmd, "fkf timeline --since <window> [--until <window>] | fkf timeline <record-uri> [--around 2h]"); err != nil {
				return err
			}
			if err := requirePositiveIntFlagAtMost(cmd, "budget", services.MaxDigestBudget); err != nil {
				return err
			}
			if cmd.IsSet("around") && cmd.Duration("around") <= 0 {
				return invalidUsage(fmt.Errorf("--around must be a positive duration"))
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.Timeline(ctx, base, services.TimelineRequest{
				Window:  services.Window{Since: cmd.String("since"), Until: cmd.String("until")},
				Sources: cmd.StringSlice("source"), Repository: cmd.String("repo"), Person: cmd.String("person"),
				AroundURI: cmd.Args().First(), Around: cmd.Duration("around"),
				Budget: cmd.Int("budget"), All: cmd.Bool("all"), DeliveryFormat: cmd.Root().String("format"),
			})
			return emitTimeline(cmd, report, err)
		},
	}
}

func emitTimeline(cmd *cli.Command, report *services.TimelineReport, err error) error {
	if err != nil || cmd.Root().String("format") != formatText {
		return emit(cmd, report, err)
	}
	_, err = fmt.Fprint(cmd.Root().Writer, services.RenderTimelineText(report))
	return err
}
