# AGENTS.md (Project)

This repository builds **`fkf`**: one Go module, one binary, and two embedded skills. A base is a git repository of JSON and Markdown that FKF collects, links as URIs, and retrieves for coding agents. `README.md` explains the product; this file defines the contributor contract.

## Status

The design is implemented. Do not add a compatibility path, migration, or second spelling. Configuration and collected evidence each use `fkf: 1`; the containing file identifies the contract. The evidence envelope is permanent and additive because providers may no longer retain old records. A change that requires forced re-collection is the wrong design.

## Workflow

1. Use `.agents/skills/fkf-contribute/SKILL.md` as the route map. This file is authoritative if they disagree.
1. Run focused tests after behavior changes, then `mise run all`. Never weaken, skip, or hide a failing check.
1. Do not commit, push, publish, rename, delete outside the working tree, or take an irreversible action unless explicitly asked.
1. Keep tests hermetic: use a temporary `HOME`, unset `FKF_BASE`, fake every declared command through `Runner`, and use synthetic fixtures. The narrow exceptions are tests of `core/shell`, the synthetic relative-base helper regression, and invariant or status-policy tests that run local `go list` or `git`.

## Product boundaries

- **One module and binary.** The root module is `github.com/fmind/fkf`; do not add `go.work`, a nested module, plugin, provider SDK, or second shipped command. `docs/go.mod` only pins the Hugo theme.
- **POSIX execution.** FKF supports Linux and macOS; `run:` is direct argv and a helper's shebang selects its interpreter. Cancellation uses POSIX process groups. WSL2 is Linux; native Windows is out of scope.
- **One base boundary.** `<base>/fkf.yaml` plus ignored `fkf.local.yaml` are the only configuration. There is no global config, profile, bundle, visibility, sensitivity, capture, or policy layer.
- **Plain durable data.** Bases contain JSON, Markdown, and the generated graph TSV. Envelope changes under marker `1` are additive; never store rendered commands, planner state, or other mutable execution metadata in evidence. Event documents retain the UTC bounds of their collection-time civil day; older documents without those fields use the reader's current local zone.
- **Published paths only.** Resolve paths through `Store.Resolve`. It admits enabled layers, graph artifacts, `fkf.yaml`, and `AGENTS.md`; it rejects every other root file and any symlink below the base.
- **Small, typed Go.** Keep cancellation explicit, split natural seams rather than line counts, and prefer a pure helper to a framework.

## Trust and execution

- **Stored reads are offline.** `context`, `find`, `graph`, MCP, and `read` without `--body` execute no command, and no package imports `net/http`. In-process gojq has no environment, input, or import access; the final JSON result is bounded. `halt` may succeed; `halt_error` fails.
- **Collected content is untrusted data.** Never turn a stored value into instructions or a shell or executable position. The only exception is trust-gated `read --body`, which may pass one charset-checked, non-option field value as one opaque argv item to the source's current `body:` command.
- **FKF reads no credential.** Credentials and provider-profile selectors belong to the named provider CLI and the process that launches FKF; configuration declares no environment values. Remove interpreter and dynamic-loader startup variables from children, drop relative or base-resolving home/config roots, sanitize `PATH`, and never rely on FKF to redact provider output.
- **Commands stay open.** Prefer direct provider argv, then a helper under `<base>/bin/`, then any reviewed executable suited to structured or stateful work. `requires:` declares bare executable names and non-standard interpreters; readiness never guesses from command text.
- **Commands have a neutral cwd.** Declared commands run from `/`, never the base. Pass data paths explicitly with `{{base}}`; put every executable or interpreted support file under trust-digested `bin/` and invoke it by its bare PATH name.
- **Trust covers execution.** Hash the canonical execution plan from both config files and every entry and executable bit under `<base>/bin/`; refuse every symlink. Enabled state and other execution-affecting values re-arm trust; comments, YAML order, descriptions, examples, `requires:`, and retrieval-only paths do not. Put every base-controlled helper or interpreted support file under `bin/`; never source authored or collected layers. Trust is disclosure and change detection, not a sandbox.
- **PATH stays outside the base.** Extra `bin:` directories must be absolute or `~`-relative, machine-local, and outside the base. Remove empty, relative, and base-resolving inherited `PATH` entries.

## Sources and writes

- Root `schema:` is the base-owned semantic dictionary. Fields declare a description, `one`/`optional`/`many` cardinality, and optionally `relation: true`. Sources map those shared names to provider paths; only `id` and event `time` are structural. Field names express roles, while URI schemes express identity namespaces.
- Placeholders use exact lowercase `{{name}}` syntax. FKF supplies dates and paths as individual argv values; no shell reparses them. Pipelines and expansion belong in a helper whose shebang declares the interpreter.
- Stored documents keep every decoded value, their schema subset, and field map. A `body:` command may use only a max-one field already present in that stored map.
- Collection is all-or-nothing and atomic. Non-zero exit, timeout, excessive output, invalid or multiple JSON documents, or missing required values fails the day. `sync --preview` runs and validates one source once, returns at most three projected samples, and writes nothing.
- A `window: true` source runs once per contiguous missing span. Preset helpers use finite pagination and fail before emitting partial output. `status` revalidates stored documents against current structural rules.
- Every mutating CLI path takes one fail-fast lock per physical base, including symlink aliases. Readers, dry runs, previews, `trust --check`, and `build wiki --check` remain lock-free.

## Derived content and retrieval

- **Generated Markdown stays inside marked blocks.** `build wiki` owns one block in `wiki/index.md` through `services/marked_block.go`. `build` and `build all` update the wiki block before the graph because the exact page bytes are graph input.
- **Files own links; the graph is a cache.** Record edges come from stored `relation: true` fields. Markdown edges come from links, tags, and declared relation fields under `relations:`. Do not infer edges from titles, prose, arbitrary frontmatter, privileged entity kinds, or identity matching.
- **Graph reads bind one generation.** Metadata covers canonical collected documents, every authored graph input, and the TSV bytes. Reads recompute input digests. A neighbourhood walk holds one validated descriptor and revalidates its bytes before returning.
- **Retrieval is lexical and reproducible.** No embeddings, model, or index engine belongs in the read path. The same query, base, binary, and `as_of` date yields the same pack and receipt. The public indented JSON, including its receipt, must fit the exact four-bytes-per-token budget; a smaller request returns a self-consistent minimum, and rejected explicit pins remain named.
- **MCP is read-only and bounded.** It requires `--base`, omits `--body` and the CLI-only Git tracking audit, and pages at 100 items with opaque cursors bound to arguments and an exact snapshot. Text and structured results carry the same compact JSON and stay within 4 MiB. The wiki-index resource keeps its curated body. Client errors expose only base-relative paths and anonymize home and FKF state paths.

## Base contract

Each layer is explicitly enabled in `fkf.yaml`:

| Layer                              | Contract                                                                        |
| ---------------------------------- | ------------------------------------------------------------------------------- |
| `events/YYYY-MM-DD/`               | One complete JSON document per event source.                                    |
| `index/`                           | Current source documents.                                                       |
| `tasks/YYYY-MM-DD/<slug>/TASKS.md` | Session traces and learned items.                                               |
| `projects/<slug>.md`               | Flat, tagged pages with required status.                                        |
| `wiki/<slug>.md`                   | Flat OKF v0.2 pages; writes require type and tags, while reads stay permissive. |

Root `graph.tsv` and `graph.meta.json` are the only graph cache. Managed `.gitignore` and `.gitattributes` blocks decide whether events and index are tracked; there is no configuration key for that policy.

URI grammar lives in `skills/fkf-use/SKILL.md` and must match `services/uri.go`: file and record URIs are `<path>[?jq=<expr>][#<id|anchor>]`; entities are arbitrary non-reserved lowercase `<scheme>:<identity>` values; external HTTPS URLs remain verbatim. Fragments must name an existing record or Markdown heading.

## Code map

| Path         | Responsibility                                                                             |
| ------------ | ------------------------------------------------------------------------------------------ |
| `cmd/fkf/`   | urfave/cli commands, output envelopes, text renderers, and exit codes 0, 1, 2, 3, and 130. |
| `core/`      | Config/schema, store confinement, bounded I/O, atomic writes, processes, and trust.        |
| `sources/`   | Placeholders, execution, decoding, field projection, documents, bodies, retry, and pacing. |
| `services/`  | Sync, retrieval, graph, Markdown, status, build, init, and synthetic preset fixtures.      |
| `mcpserver/` | Read-only tools, resources, cursors, and bounded responses.                                |
| `skills/`    | Embedded `fkf-use` and `fkf-learn`; edit these sources, never a generated base copy.       |
| `presets/`   | Source YAML and small reviewed helpers.                                                    |
| `docs/`      | Hugo site and generated `static/fkf.schema.json`.                                          |

## CLI contract

- Root commands are strict action verbs. `list`, `validate`, and `tags` take layer subcommands; `read` opens any addressable URI. A misplaced listing argument should suggest `read`.
- Listings lead with the URI accepted by `read` and end with the item's subject. `status` is the only whole-base view; bare `read` suggests nearby valid URIs.
- `find` is exhaustive across all layers; `context` selects the best evidence under a budget. Bare `context` prints its help. `graph <uri>` walks a neighbourhood; bare `graph` describes the whole edge list.
- Aliases use the first letter, with the daily command winning collisions; `init`, `trust`, `status`, and `config` have none. Tests own the exact vocabulary.
- Output defaults to text on a terminal and JSON otherwise. Every result has a text renderer; any JSON fallback is reported on stderr.
- Every command, subcommand, and flag that writes or executes a declaration ends its usage line with `markWrite` or `markRun`.

## Verification

`mise run all` runs format, static checks, hermetic race/coverage tests, the coverage ratchet, and the build. Lefthook and CI use the same tasks. Use `mise run generate:schema` after loader changes; do not hand-edit generated schema. A Go toolchain or linked-dependency change also updates `THIRD_PARTY_NOTICES.md`.

Tests enforce the high-risk invariants: one module and binary, no `net/http` or credential-variable source, no legacy or migration identifiers, schema parity, one synthetic fixture per preset, complete third-party notices, and closed-vocabulary plus golden-arithmetic retrieval scoring. `mise run benchmark` is an opt-in 100,000-record/500,000-edge observation, not a threshold or a reason to add a database.
