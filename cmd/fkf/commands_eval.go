package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newEvalCommand() *cli.Command {
	return &cli.Command{
		Name: "eval", Aliases: []string{"e"}, Category: groupInspect,
		Usage: "Measure retrieval recall at k against evals/queries.yaml.",
		UsageText: usageLines(
			[2]string{"fkf eval", "run the base's deterministic retrieval acceptance set"},
		),
		Description: "Runs every declared question against stored evidence only. Each result names " +
			"the top-k URIs, missing expected URIs, forbidden hits, ranking version, and context input " +
			"digest. A threshold miss exits 1; an invalid suite exits 2.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.Evaluate(ctx, base)
			if err != nil {
				return err
			}
			if err := emitEval(cmd, report); err != nil {
				return err
			}
			if !report.Passed {
				return partialFailure(fmt.Errorf("%d retrieval evaluation(s) failed", report.Failed))
			}
			return nil
		},
	}
}

func emitEval(cmd *cli.Command, report *services.EvalReport) error {
	if cmd.Root().String("format") != formatText {
		return emit(cmd, report, nil)
	}
	for _, query := range report.Queries {
		state := "PASS"
		if !query.Passed {
			state = "FAIL"
		}
		if _, err := fmt.Fprintf(cmd.Root().Writer, "%s %-24s recall@%d %.3f (%d/%d)\n",
			state, query.Name, query.K, query.Recall, query.FoundExpected, query.Expected); err != nil {
			return err
		}
		if len(query.MissingExpected) > 0 {
			if _, err := fmt.Fprintf(cmd.Root().Writer, "  missing: %v\n", query.MissingExpected); err != nil {
				return err
			}
		}
		if len(query.ForbiddenFound) > 0 {
			if _, err := fmt.Fprintf(cmd.Root().Writer, "  forbidden: %v\n", query.ForbiddenFound); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(cmd.Root().Writer, "%d passed, %d failed · threshold %.3f · %s\n",
		report.PassedQueries, report.Failed, report.RecallThreshold, report.Path)
	return err
}
