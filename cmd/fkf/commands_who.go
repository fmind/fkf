package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newWhoCommand() *cli.Command {
	return &cli.Command{
		Name: "who", Aliases: []string{"w"}, Category: groupAsk,
		Usage:     "Who is this? Resolve exact identities and show their stored interactions.",
		ArgsUsage: "<name|uri>",
		UsageText: usageLines(
			[2]string{`fkf who "Maxime Cordy"`, "the canonical person, pages, aliases, and recent interactions"},
			[2]string{"fkf who actor:github.com/fmind", "resolve an exact declared alias"},
		),
		Description: "Reads declared identities, authored person or organization pages, the validated " +
			"derived graph, and stored records only. It never contacts a provider.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireOneArg(cmd, "fkf who <name|uri>"); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			report, err := services.Who(ctx, base, cmd.Args().First())
			if err != nil || cmd.Root().String("format") != formatText {
				return emit(cmd, report, err)
			}
			return renderWhoText(cmd.Root().Writer, report)
		},
	}
}

func renderWhoText(output io.Writer, report *services.WhoReport) error {
	if len(report.Matches) == 0 {
		_, err := fmt.Fprintf(output, "no identity match for %q\n", report.Query)
		return err
	}
	for index, match := range report.Matches {
		if index > 0 {
			if _, err := fmt.Fprintln(output); err != nil {
				return err
			}
		}
		if err := renderWhoMatch(output, match); err != nil {
			return err
		}
	}
	return nil
}

func renderWhoMatch(output io.Writer, match services.WhoMatch) error {
	if _, err := fmt.Fprintf(output, "%s [%s]\n", match.Canonical, match.Kind); err != nil {
		return err
	}
	for _, line := range []struct {
		label  string
		values []string
	}{{"names", match.Names}, {"aliases", match.Aliases}} {
		if len(line.values) > 0 {
			if _, err := fmt.Fprintf(output, "%s: %s\n", line.label, strings.Join(line.values, ", ")); err != nil {
				return err
			}
		}
	}
	for _, page := range match.Pages {
		if _, err := fmt.Fprintf(output, "page: %s · %s\n", page.URI, page.Title); err != nil {
			return err
		}
	}
	for _, count := range match.Counts {
		if _, err := fmt.Fprintf(output, "source: %s · %d\n", count.Source, count.Count); err != nil {
			return err
		}
	}
	for _, group := range match.Neighbourhood {
		if _, err := fmt.Fprintf(output, "%s: %s\n", group.Kind, strings.Join(group.Nodes, ", ")); err != nil {
			return err
		}
	}
	if match.NeighbourhoodTruncated {
		if _, err := fmt.Fprintln(output, "neighbourhood: truncated at 200 edges"); err != nil {
			return err
		}
	}
	for _, record := range match.Recent {
		if _, err := fmt.Fprintf(output, "%s [%s] %s · %s\n", record.Time, record.Source, record.Title, record.URI); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "total: %d interaction(s)\n", match.Total)
	return err
}
