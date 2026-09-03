package main

import (
	"context"
	"errors"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newLearnCommand() *cli.Command {
	return &cli.Command{
		Name: "learn", Category: groupRun,
		Usage: "Stage, review, apply, or reject reviewable knowledge diffs.",
		Description: "Keeps agent-authored changes out of durable knowledge until a person reviews an exact " +
			"unified diff. Active proposals live under .agents/tmp/learn and may target only flat wiki or projects pages.",
		Commands: []*cli.Command{
			{
				Name: "propose", Aliases: []string{"p"}, Usage: "Stage unharvested task lessons as a wiki log diff." + markWrite,
				Description: "Builds one deterministic proposal for wiki/log.md and cites every task trace in sources frontmatter. " +
					"--dry-run lists the candidate bullets and creates no queue directory.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "dry-run", Usage: "List candidate log bullets and write nothing."},
				},
				Action: runLearnPropose,
			},
			{
				Name: "review", Aliases: []string{"v"}, Usage: "List queued proposals or inspect one exact diff.", ArgsUsage: "[proposal]",
				Description: "With no proposal, lists the active queue. With one proposal and --diff, prints the bounded " +
					"unified diff that apply would validate.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "diff", Usage: "Include the exact unified diff; requires one proposal id."},
				},
				Action: runLearnReview,
			},
			{
				Name: "apply", Aliases: []string{"a"}, Usage: "Validate, apply, and archive one reviewed proposal." + markWrite,
				ArgsUsage: "<proposal>",
				Description: "Accepts only unified diffs against flat wiki/*.md and projects/*.md pages. It rolls back on " +
					"patch mismatch, strict validation failure, rebuild failure, or archive failure.",
				Action: runLearnApply,
			},
			{
				Name: "reject", Aliases: []string{"r"}, Usage: "Archive one proposal without changing knowledge." + markWrite,
				ArgsUsage: "<proposal>",
				Action:    runLearnReject,
			},
		},
	}
}

func runLearnPropose(ctx context.Context, cmd *cli.Command) error {
	if err := requireNoArgs(cmd, ""); err != nil {
		return err
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	run := func() error {
		report, err := services.ProposeLearn(ctx, base, cmd.Bool("dry-run"))
		return emitLearn(cmd, report, err)
	}
	if cmd.Bool("dry-run") {
		return run()
	}
	return withWriterLock(ctx, base.Root(), run)
}

func runLearnReview(ctx context.Context, cmd *cli.Command) error {
	if err := requireAtMostOneArg(cmd, "fkf learn review [proposal] [--diff]"); err != nil {
		return err
	}
	id := cmd.Args().First()
	if cmd.Bool("diff") && id == "" {
		return invalidUsage(errors.New("usage: fkf learn review <proposal> --diff"))
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	review, err := services.ReviewLearn(ctx, base, id, cmd.Bool("diff"))
	return emitLearn(cmd, review, err)
}

func runLearnApply(ctx context.Context, cmd *cli.Command) error {
	if err := requireOneArg(cmd, "fkf learn apply <proposal>"); err != nil {
		return err
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	return withWriterLock(ctx, base.Root(), func() error {
		report, err := services.ApplyLearn(ctx, base, cmd.Args().First())
		return emitLearn(cmd, report, err)
	})
}

func runLearnReject(ctx context.Context, cmd *cli.Command) error {
	if err := requireOneArg(cmd, "fkf learn reject <proposal>"); err != nil {
		return err
	}
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	return withWriterLock(ctx, base.Root(), func() error {
		report, err := services.RejectLearn(ctx, base, cmd.Args().First())
		return emitLearn(cmd, report, err)
	})
}

func emitLearn(cmd *cli.Command, result any, err error) error {
	if err != nil {
		return err
	}
	switch cmd.Root().String("format") {
	case formatJSONL:
		return writeJSONLines(cmd.Root().Writer, result)
	case formatText:
		writer := &textWriter{out: cmd.Root().Writer}
		writeLearnText(writer, result)
		return writer.err
	default:
		return writeJSON(cmd.Root().Writer, result)
	}
}

func writeLearnText(writer *textWriter, result any) {
	switch typed := result.(type) {
	case *services.LearnProposalReport:
		if typed.Nothing {
			writer.line("nothing to propose")
			return
		}
		if typed.DryRun {
			for _, candidate := range typed.Candidates {
				writer.printf("- %s · %s#learned -> %s\n", candidate.Text, candidate.Trace, candidate.Target)
			}
			writer.printf("\n%d candidate log bullet(s); nothing written\n", len(typed.Candidates))
			return
		}
		state := "staged"
		if typed.Existing {
			state = "already staged"
		}
		writer.printf("%s %s · %d bytes · %s\n", state, typed.Proposal.ID, typed.Proposal.Bytes, typed.Proposal.Path)
		writer.printf("review: fkf learn review %s --diff\n", typed.Proposal.ID)
	case *services.LearnReview:
		if len(typed.Proposals) == 0 {
			writer.line("no active learn proposals")
			return
		}
		for _, proposal := range typed.Proposals {
			if proposal.Diff != "" {
				writer.printf("%s", proposal.Diff)
				continue
			}
			writer.printf("%s · %d bytes · %s · %s\n", proposal.ID, proposal.Bytes,
				proposal.Path, strings.Join(proposal.Files, ", "))
		}
	case *services.LearnActionReport:
		writer.printf("%s %s · %s\n", typed.Status, typed.ID, typed.Path)
		if len(typed.Files) > 0 {
			writer.printf("files: %s\n", strings.Join(typed.Files, ", "))
		}
	}
}
