package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newBriefCommand() *cli.Command {
	return &cli.Command{
		Name: "brief", Category: groupAsk,
		Usage: "What needs attention today? Build one budgeted daily control surface." + markRun,
		UsageText: usageLines(
			[2]string{"fkf brief", "yesterday, today, open work, failures, due tasks, and source health"},
			[2]string{"fkf brief --budget 800", "the same receipt under a smaller complete-output budget"},
		),
		Description: "Reads stored evidence and authored pages, then runs only the trusted readiness " +
			"probes declared by enabled sources. It never collects evidence or fetches a body. The JSON " +
			"and text forms share one receipt and both fit the requested four-bytes-per-token budget.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "budget", Value: services.DefaultBriefBudget, Usage: "Hard four-bytes-per-token budget for the complete brief."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requirePositiveIntFlagAtMost(cmd, "budget", services.MaxBriefBudget); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.Brief(ctx, base, services.BriefRequest{Budget: cmd.Int("budget")})
			if err != nil || cmd.Root().String("format") != formatText {
				return emit(cmd, report, err)
			}
			_, err = fmt.Fprint(cmd.Root().Writer, services.RenderBriefText(report))
			return err
		},
	}
}
