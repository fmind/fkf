package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// The inspection commands browse the layers, validate Markdown, and inspect the tag vocabulary.
// Reading any item uses universal `fkf read <uri>`, and searching uses `fkf find <terms> --layer <layer>`.

func newListCommand() *cli.Command {
	return &cli.Command{
		Name: "list", Aliases: []string{"l"}, Category: groupInspect,
		Usage: "List documents, traces, or pages in an enabled layer.",
		UsageText: usageLines(
			[2]string{"fkf list events [options]", "what happened: one collected document per source per day"},
			[2]string{"fkf list index", "what exists now: point-in-time documents"},
			[2]string{"fkf list tasks [options]", "what an agent did: one trace per session"},
			[2]string{"fkf list projects [options]", "what you are trying to achieve: intent pages"},
			[2]string{"fkf list wiki [options]", "what is durable and reusable: concept pages"},
		),
		Commands: []*cli.Command{
			newEventsListSubcommand(),
			newIndexListSubcommand(),
			newTasksListSubcommand(),
			newProjectsListSubcommand(),
			newWikiListSubcommand(),
		},
	}
}

func newEventsListSubcommand() *cli.Command {
	return &cli.Command{
		Name: "events", Aliases: []string{"e"},
		Usage: "What happened: one collected document per source per day.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "source", Usage: "Restrict to one declared source."},
			&cli.IntFlag{Name: "limit", Usage: "Maximum days to return."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireNonNegativeLimit(cmd); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			window, err := services.ParseWindow(cmd.String("since"), cmd.String("until"), base.Now())
			if err != nil {
				return invalidUsage(err)
			}
			return render(cmd)(services.ListEvents(ctx, base, window, cmd.String("source"), cmd.Int("limit")))
		},
	}
}

func newIndexListSubcommand() *cli.Command {
	return &cli.Command{
		Name: "index", Aliases: []string{"i"},
		Usage: "The things you have: one point-in-time document per source.",
		Description: "Where events/ records what happened on a day, index/ records what " +
			"exists now: your repositories, your files, your labels, your toolchain. A source " +
			"files here by declaring `layer: index`, and each sync rewrites its document " +
			"whole. The graph fkf derives lives in `graph.tsv` at the base root and is read with `fkf graph`.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			return render(cmd)(services.ListIndex(ctx, base, 0))
		},
	}
}

func newTasksListSubcommand() *cli.Command {
	return &cli.Command{
		Name: "tasks", Aliases: []string{"t"},
		Usage: "What an agent did: one trace per session, newest first.",
		UsageText: usageLines(
			[2]string{"fkf list tasks [options]", "the traces in the window, newest first"},
			[2]string{"fkf list tasks learned --unharvested", "every `## Learned` bullet, and which are still a backlog"},
		),
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
			&cli.IntFlag{Name: "limit", Usage: "Maximum traces to return."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireNonNegativeLimit(cmd); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			window, err := services.ParseWindow(cmd.String("since"), cmd.String("until"), base.Now())
			if err != nil {
				return invalidUsage(err)
			}
			return render(cmd)(services.ListTasks(ctx, base, window, cmd.Int("limit")))
		},
		Commands: []*cli.Command{
			{
				Name: "learned", Aliases: []string{"l"},
				Usage: "Every `## Learned` bullet in the window, and whether some wiki or projects page has already promoted it.",
				Description: "A deterministic scan, not a proposal: a bullet is `harvested` when some wiki or " +
					"projects page's `sources:` frontmatter already cites the trace it came from — the fkf-learn " +
					"skill's own convention — and `unharvested` otherwise. Nothing is written.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
					&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
					&cli.BoolFlag{Name: "unharvested", Usage: "Only the bullets no page has cited yet — the backlog."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					window, err := services.ParseWindow(cmd.String("since"), cmd.String("until"), base.Now())
					if err != nil {
						return invalidUsage(err)
					}
					return render(cmd)(services.ListLearned(ctx, base, window, cmd.Bool("unharvested")))
				},
			},
		},
	}
}

func newProjectsListSubcommand() *cli.Command {
	return &cli.Command{
		Name: "projects", Aliases: []string{"p"},
		Usage: "What you are trying to achieve: the status-bearing intent pages.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "status", Usage: "active, paused, or done."},
			&cli.StringSliceFlag{Name: "tag", Usage: "Require this tag (repeatable, all must match)."},
			&cli.IntFlag{Name: "limit", Usage: "Maximum pages to return."},
		},
		Action: listPagesAction(core.LayerProjects),
	}
}

func newWikiListSubcommand() *cli.Command {
	return &cli.Command{
		Name: "wiki", Aliases: []string{"w"},
		Usage: "What is true and reusable: the durable knowledge bundle, flat and tagged.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "tag", Usage: "Require this tag (repeatable, all must match)."},
			&cli.StringFlag{Name: "type", Usage: "decision, pattern, tool, insight, person, …"},
			&cli.IntFlag{Name: "limit", Usage: "Maximum pages to return."},
		},
		Action: listPagesAction(core.LayerWiki),
	}
}

func newValidateCommand() *cli.Command {
	return &cli.Command{
		Name: "validate", Aliases: []string{"v"}, Category: groupInspect,
		Usage: "Check frontmatter, the flat rule, slugs, and links.",
		UsageText: usageLines(
			[2]string{"fkf validate [--strict]", "validate enabled wiki and project pages"},
			[2]string{"fkf validate wiki [--strict]", "frontmatter, flat rule, slugs, links"},
			[2]string{"fkf validate projects [--strict]", "frontmatter, required status, slugs, links"},
		),
		Flags: []cli.Flag{&cli.BoolFlag{Name: "strict", Usage: "Promote every warning to an error."}},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return validateAll(ctx, cmd)
		},
		Commands: []*cli.Command{
			{
				Name: "wiki", Aliases: []string{"w"},
				Usage: "Check wiki frontmatter, the flat rule, slugs, and links.",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "strict", Usage: "Promote every warning to an error."}},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return validateLayer(ctx, cmd, core.LayerWiki, false)
				},
			},
			{
				Name: "projects", Aliases: []string{"p"},
				Usage: "Check projects frontmatter, required status, slugs, and links.",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "strict", Usage: "Promote every warning to an error."}},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return validateLayer(ctx, cmd, core.LayerProjects, true)
				},
			},
		},
	}
}

func newTagsCommand() *cli.Command {
	return &cli.Command{
		Name: "tags", Aliases: []string{"t"}, Category: groupInspect,
		Usage: "Tag vocabulary with its usage, most-used first.",
		UsageText: usageLines(
			[2]string{"fkf tags", "wiki tag vocabulary"},
			[2]string{"fkf tags wiki", "wiki tag vocabulary"},
			[2]string{"fkf tags projects", "projects tag vocabulary"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			return render(cmd)(services.BuildTagVocabulary(ctx, base, core.LayerWiki))
		},
		Commands: []*cli.Command{
			{
				Name: "wiki", Aliases: []string{"w"},
				Usage: "Wiki tag vocabulary with its usage, most-used first.",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return render(cmd)(services.BuildTagVocabulary(ctx, base, core.LayerWiki))
				},
			},
			{
				Name: "projects", Aliases: []string{"p"},
				Usage: "Projects tag vocabulary with its usage, most-used first.",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return render(cmd)(services.BuildTagVocabulary(ctx, base, core.LayerProjects))
				},
			},
		},
	}
}

func validateAll(ctx context.Context, cmd *cli.Command) error {
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	strict := cmd.Bool("strict")
	hasErrors := false

	if base.Config.Layers[core.LayerWiki] {
		report, err := services.ValidateMarkdownLayer(ctx, base, core.LayerWiki, false, strict)
		if err != nil {
			return err
		}
		if err := emit(cmd, report, nil); err != nil {
			return err
		}
		if !report.OK {
			hasErrors = true
		}
	}

	if base.Config.Layers[core.LayerProjects] {
		report, err := services.ValidateMarkdownLayer(ctx, base, core.LayerProjects, true, strict)
		if err != nil {
			return err
		}
		if err := emit(cmd, report, nil); err != nil {
			return err
		}
		if !report.OK {
			hasErrors = true
		}
	}

	if hasErrors {
		return partialFailure(errUsage("validation found errors"))
	}
	return nil
}

func listPagesAction(layer core.Layer) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if err := requireNonNegativeLimit(cmd); err != nil {
			return err
		}
		base, err := openBase(cmd)
		if err != nil {
			return err
		}
		return render(cmd)(services.ListPages(ctx, base, layer, services.PageFilter{
			Tags: cmd.StringSlice("tag"), Status: cmd.String("status"),
			Type: cmd.String("type"), Limit: cmd.Int("limit"),
		}))
	}
}

func validateLayer(ctx context.Context, cmd *cli.Command, layer core.Layer, requireStatus bool) error {
	base, err := openBase(cmd)
	if err != nil {
		return err
	}
	report, err := services.ValidateMarkdownLayer(ctx, base, layer, requireStatus, cmd.Bool("strict"))
	if err := emit(cmd, report, err); err != nil {
		return err
	}
	if !report.OK {
		return partialFailure(errUsage("validation found %d error(s) in %s/", report.Errors, string(layer)))
	}
	return nil
}

func usageLines(forms ...[2]string) string {
	width := 0
	for _, form := range forms {
		width = max(width, len(form[0]))
	}
	var builder strings.Builder
	for index, form := range forms {
		if index > 0 {
			builder.WriteString("\n")
		}
		fmt.Fprintf(&builder, "%-*s  %s", width, form[0], form[1])
	}
	return builder.String()
}
