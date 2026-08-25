package main

import (
	"context"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const windowFlagUsage = "YYYY-MM-DD, today, yesterday, or a positive relative window such as 7d."

// The four retrieval commands, each answering exactly one question. `read` is the resolver the
// other three quote: every result they return carries a URI that `read` accepts.

func newContextCommand() *cli.Command {
	return &cli.Command{
		Name: "context", Aliases: []string{"c"}, Category: groupAsk,
		Usage:     "What should an agent know about this? Build a token-budgeted evidence pack.",
		ArgsUsage: "<terms>...",
		UsageText: usageLines(
			[2]string{`fkf context "fmind/fkf retrieval"`, "the pack: paste it, or pipe it into an agent"},
			[2]string{`fkf context "FK-412" --explain`, "the same pack, with why each item scored"},
			[2]string{`fkf context "fmind/fkf" --budget 1500 --expand`, "smaller, plus one graph hop"},
			[2]string{`fkf context "release" --pin projects/release-checklist.md`, "force that exact page in, whatever it scores"},
		),
		Description: "This is the selective half of retrieval: the best few items under a token " +
			"budget. `fkf find` is the other half — every match, unranked and unbudgeted. It ranks " +
			"the windowed records and task traces plus every project and wiki " +
			"concept lexically, then returns the pack plus a receipt: each item's score and " +
			"reasons, bounded drop detail with the full count, rejected pins, graph expansion, and digests that " +
			"make it reproducible. Put the most specific field value first: an exact value from " +
			"any declared field scores as an identifier.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
			&cli.IntFlag{Name: "budget", Value: services.DefaultBudget, Usage: "Hard four-bytes-per-token budget for the complete pack."},
			&cli.StringSliceFlag{Name: "pin", Usage: "Exact wiki/ or projects/ URI admitted first (repeatable, capped at a third of the budget)."},
			&cli.BoolFlag{Name: "expand", Usage: "Add one graph hop from the strongest matches."},
			&cli.BoolFlag{Name: "explain", Usage: "Include the per-reason score breakdown for every selected item."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return showHelp(cmd, "fkf context takes the terms to brief an agent on")
			}
			if err := requirePositiveIntFlag(cmd, "budget"); err != nil {
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
			return render(cmd)(services.BuildContext(ctx, base, services.ContextRequest{
				Query: strings.Join(cmd.Args().Slice(), " "), Window: window, Budget: cmd.Int("budget"),
				Pins: cmd.StringSlice("pin"), Expand: cmd.Bool("expand"), Explain: cmd.Bool("explain"),
			}))
		},
	}
}

func newFindCommand() *cli.Command {
	return &cli.Command{
		Name: "find", Aliases: []string{"f"}, Category: groupAsk,
		Usage:     "Where does this appear? Search every layer, and return every match.",
		ArgsUsage: "[terms]...",
		UsageText: usageLines(
			[2]string{"fkf find", "the last seven populated days of records"},
			[2]string{"fkf find <terms>...", "every match, in wiki/, projects/, tasks/, and the records"},
			[2]string{"fkf find <terms>... --layer wiki", "one layer only"},
			[2]string{"fkf find --where .state=MERGED --since 7d", "records whose provider value matches"},
			[2]string{"fkf find --count", "per-day, per-source volumes instead of items"},
		),
		Description: "This is the exhaustive half of retrieval: every match, flat and filterable. " +
			"`fkf context` is the other half — the best few under a token budget, with a receipt. " +
			"A bare term is matched against record string, numeric, and boolean scalar VALUES, never keys or compounds, and against a page's " +
			"title, tags, and body; it is the positional form of --grep. --limit bounds each half " +
			"separately, so a long record list can never hide a page. With neither a term nor a filter, the last seven " +
			"populated days of records. --format jsonl streams one item per line, so the query " +
			"engine you want is the one you pipe it into.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "layer", Usage: "Restrict a question to events, index, tasks, projects, or wiki (repeatable); use list to browse."},
			&cli.StringSliceFlag{Name: "source", Usage: "Restrict to a declared source (repeatable); records only."},
			&cli.StringFlag{Name: "since", Usage: windowFlagUsage},
			&cli.StringFlag{Name: "until", Usage: windowFlagUsage},
			&cli.StringSliceFlag{Name: "grep", Usage: "Term matched against record string, numeric, or boolean scalar values (repeatable, all must match)."},
			&cli.StringSliceFlag{Name: "where", Usage: "<jq-path>=<value> equality over a record (repeatable); records only."},
			&cli.IntFlag{Name: "limit", Usage: "Maximum items per half (search default all; bare find 200; 0 for no limit)."},
			&cli.BoolFlag{Name: "count", Usage: "Print per-day, per-source volumes instead of items."},
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
			layers, err := parseLayers(cmd.StringSlice("layer"))
			if err != nil {
				return invalidUsage(err)
			}
			filter := services.FindFilter{
				Sources: cmd.StringSlice("source"), Layers: layers, Window: window,
				Grep:  append(cmd.Args().Slice(), cmd.StringSlice("grep")...),
				Limit: findLimit(cmd),
			}
			for _, argument := range cmd.StringSlice("where") {
				clause, err := services.ParseWhere(argument)
				if err != nil {
					return invalidUsage(err)
				}
				filter.Where = append(filter.Where, clause)
			}
			result, err := services.Find(ctx, base, filter, cmd.Bool("count"))
			return emit(cmd, result, err)
		},
	}
}

func newReadCommand() *cli.Command {
	return &cli.Command{
		Name: "read", Aliases: []string{"r"}, Category: groupAsk,
		Usage:     "Open exactly one URI: a document, a record, a heading, a jq selection, or an entity.",
		ArgsUsage: "<uri>",
		Description: "One grammar for everything: <path>[?jq=<expr>][#<id|anchor>], arbitrary " +
			"<scheme>:<identity> entities, and HTTPS. Every other command prints URIs this one accepts.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "body",
				Usage: "Fetch this record's body on demand; it stores nothing and needs a trusted base." + markRun,
			},
			&cli.IntFlag{Name: "limit", Usage: "Bound a directory listing or an entity's neighbourhood."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireOneArg(cmd, "fkf read <uri> [--body]"); err != nil {
				return err
			}
			if err := requireNonNegativeLimit(cmd); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			return render(cmd)(services.Read(ctx, base, cmd.Args().First(), services.ReadOptions{
				Body: cmd.Bool("body"), Limit: cmd.Int("limit"),
			}))
		},
	}
}

// `fkf graph <uri>` walks the neighbourhood, because that is the question asked daily and it
// was two words deep behind an `edges` subcommand whose only job was to hold the argument. The
// bare command keeps answering "what is in this graph", which is what you need before choosing
// a node to walk from. `fkf read` stays the one-URI resolver — walking a neighbourhood and
// opening one thing are different jobs, and folding them together would lose a verb.
func newGraphCommand() *cli.Command {
	return &cli.Command{
		Name: "graph", Aliases: []string{"g"}, Category: groupAsk,
		Usage:     "What is connected to what? Walk the derived edge list from one node.",
		ArgsUsage: "[uri]",
		UsageText: usageLines(
			[2]string{"fkf graph <uri> [--in|--out|--both] [--depth 2]", "the edges around one node"},
			[2]string{"fkf graph", "the edge list at a glance: how many of each kind, and when it was built"},
			[2]string{"fkf graph nodes [--kind customer]", "every node the edge list knows, busiest first"},
		),
		Description: "Edges come from schema fields declared as relations and the links written in " +
			"wiki/, projects/, and tasks/ — transcription, never inference. --in on a page is its " +
			"backlinks; on an entity it is every record or page that names it. --out is what " +
			"the node points at. Authored pages, collected records, and entities become nodes when an edge names them.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "in", Usage: "Follow edges into the node (backlinks)."},
			&cli.BoolFlag{Name: "out", Usage: "Follow edges away from the node."},
			&cli.BoolFlag{Name: "both", Usage: "Follow either side (the default)."},
			&cli.StringFlag{Name: "kind", Usage: "Restrict to an observed edge kind; bare graph lists the vocabulary, including authored frontmatter keys."},
			&cli.IntFlag{Name: "depth", Value: 1, Usage: "Hops to follow, 1 to 3."},
			&cli.IntFlag{Name: "limit", Usage: "Maximum edges to return."},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() > 1 {
				return invalidUsage(errUsage("usage: fkf graph <uri> [--in|--out|--both] [--depth 2]"))
			}
			if err := requirePositiveIntFlagAtMost(cmd, "depth", services.MaxGraphDepth); err != nil {
				return err
			}
			if err := requireNonNegativeLimit(cmd); err != nil {
				return err
			}
			directions := 0
			for _, name := range []string{"in", "out", "both"} {
				if cmd.Bool(name) {
					directions++
				}
			}
			if directions > 1 {
				return invalidUsage(errUsage("choose one of --in, --out, or --both"))
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			// No URI is the other question — the shape of the whole edge list — rather than a
			// missing argument, so it answers instead of correcting.
			if cmd.Args().Len() == 0 {
				return render(cmd)(services.SummarizeGraph(ctx, base))
			}
			direction := services.DirectionBoth
			switch {
			case cmd.Bool("in") && !cmd.Bool("out"):
				direction = services.DirectionIn
			case cmd.Bool("out") && !cmd.Bool("in"):
				direction = services.DirectionOut
			}
			return render(cmd)(services.Neighbours(ctx, base, services.GraphQuery{
				URI: cmd.Args().First(), Direction: direction, Kind: cmd.String("kind"),
				Depth: cmd.Int("depth"), Limit: cmd.Int("limit"),
			}))
		},
		Commands: []*cli.Command{
			{
				Name: "nodes", Aliases: []string{"n"},
				Usage: "Every node the edge list knows, busiest first.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "kind", Usage: "Node kind: an entity scheme, url, event, index, wiki, project, task, derived, or file."},
					&cli.IntFlag{Name: "limit", Usage: "Maximum nodes to return."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := requireNonNegativeLimit(cmd); err != nil {
						return err
					}
					base, err := openBase(cmd)
					if err != nil {
						return err
					}
					return render(cmd)(services.ListNodes(ctx, base, cmd.String("kind"), cmd.Int("limit")))
				},
			},
		},
	}
}

// parseLayers turns --layer into the typed layer list, so an unknown name is a usage error
// naming the five valid ones rather than a silently empty result.
func parseLayers(values []string) ([]core.Layer, error) {
	layers := make([]core.Layer, 0, len(values))
	for _, value := range values {
		layer, err := core.ParseLayer(value)
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// findLimit resolves --limit, whose documented "0 for no limit" collides with urfave/cli's
// zero value for a flag nobody passed. IsSet preserves the explicit escape hatch while leaving
// the service to choose between exhaustive search and bounded bare-discovery defaults.
func findLimit(cmd *cli.Command) int {
	if cmd.IsSet("limit") && cmd.Int("limit") == 0 {
		return services.NoFindLimit
	}
	return cmd.Int("limit")
}
