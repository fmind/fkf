---
title: Command reference
weight: 6
description: "The compact FKF CLI: retrieve, browse, collect, validate, and serve one base."
---

FKF has one binary and verb-first root commands. Terminal output defaults to text; piped output defaults to indented JSON. Override with global `--format text|json|jsonl` and select a base with `--base`.

The CLI help is authoritative and versioned with the binary:

```bash
fkf --help
fkf <command> --help
```

Stable exit codes are `0` success, `1` partial or operational failure, `2` configuration or usage, `3` untrusted base, and `130` cancellation.

Every mutating path takes one fail-fast cross-process writer lock for the physical base. A concurrent writer exits operationally instead of waiting or interleaving changes. Read-only commands, `sync --dry-run`, `sync --preview`, `trust --check`, and `build wiki --check` stay lock-free.

## Ask the base

### `context`

Build a ranked evidence pack under an exact token budget:

```bash
fkf context "retrieval boundary" --budget 4096 --explain
fkf context "repo:github.com/fmind/fkf" --expand
fkf context "collection" --pin wiki/explicit-sync-boundary.md
```

Use `--since` and `--until` for the dated window, repeat `--pin` for approved wiki or project pages, `--expand` for one shared-entity join, and `--explain` for the integer score breakdown. See [Context packs](../context/).

### `find`

Search every enabled layer exhaustively:

```bash
fkf find retrieval
fkf find "explicit sync" --layer wiki --layer projects
fkf find --where .state=MERGED --source github-pull-requests --since 7d
fkf find --since 7d --count
fkf find retrieval --format jsonl | jq -r .uri
```

Bare terms are the positional form of repeatable `--grep`; all terms must match. Record searches inspect scalar leaf values, never keys or container renderings. Repeatable `--where <jq-path>=<value>` tests any provider path in records and does not apply to pages. `--limit` bounds records and pages separately, so a long record list cannot hide authored pages.

With no term or filter, find lists records from the last seven populated days. `--count` reports per-day, per-source volume instead.

### `read`

Open exactly one published URI:

```bash
fkf read wiki/retrieval-boundary.md
fkf read events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42
fkf read 'events/2026-05-04/github-pull-requests.json?jq=.records|length'
fkf read person:email/marc@example.test
```

`--limit` bounds directory listings and entity neighbourhoods. `--body` is the explicit execution path for a record URI: it runs that source's trusted body argv, stores nothing, and is unavailable over MCP. Entity reads are always offline graph views.

A bare `read` suggests the closest addressable URIs rather than guessing.

### `graph`

Inspect the edge cache or walk one neighbourhood:

```bash
fkf graph
fkf graph ticket:jira/FKF-412 --in
fkf graph wiki/retrieval-boundary.md --in
fkf graph actor:github.com/marc --both --depth 2 --kind reviewer
fkf graph nodes --kind person
```

`--in` follows backlinks, `--out` follows destinations, and `--both` is the default; choose at most one. Depth is 1 to 3. On a neighbourhood, `--kind` accepts any observed **edge kind**, including base-defined relation fields such as `reviewer`. On `graph nodes`, `--kind` filters **node kinds** such as the `person` entity scheme. `graph nodes` lists every known node busiest first; there is no people-specific cache or subcommand.

## Inspect and explore

### `list`

```bash
fkf list events --since 7d
fkf list index
fkf list tasks --since 7d
fkf list tasks learned --unharvested
fkf list projects --status active
fkf list wiki --tag architecture
```

The fixed layer vocabulary is `events`, `index`, `tasks`, `projects`, and `wiki`. Every listing leads with the URI accepted by `read`. Run the relevant subcommand's help for layer-specific filters.

### `validate`

```bash
fkf validate
fkf validate wiki --strict
fkf validate projects --strict
```

Validation checks enabled wiki and project pages: their flat layout, slugs, required frontmatter, invisible characters, Markdown links, explicit `relations:` declarations, and fragment targets. `--strict` promotes every warning to an error. Task traces remain permissive evidence pages and are not part of this validator.

### `tags`

```bash
fkf tags
fkf tags wiki
fkf tags projects
```

Tags are listed with usage counts, most-used first. The bare command is the wiki vocabulary.

## Run and set up

### `init`

```bash
fkf init ~/brain
fkf init ~/team-brain --preset team --track-collected
fkf init /tmp/fkf-demo --demo 30
```

On a new path, init writes the configuration, layers, managed git blocks, base instructions, embedded skills, the helpers required by enabled sources, agent bridges, and optionally synthetic data. On an existing base it refreshes only FKF-owned skills and marked blocks, creates missing bridges, and preserves `fkf.yaml`, `AGENTS.md`, and existing helpers. Use `config helpers` for an explicit helper diff or refresh.

The default `minimal` preset starts with no source. `personal` and `team` are opt-in starting points; `--demo` adds synthetic records to the minimal configuration. `--track-collected` decides repository policy through the managed `.gitignore` block, not a configuration key.

### `trust`

```bash
fkf trust --check
fkf trust
fkf trust --all
```

Trust prints the base's executable plan and records its canonical digest. A changed base leads with per-item differences; `--all` prints the complete disclosure. `--check` records nothing. Comments, YAML order, descriptions, examples, and retrieval-only mappings do not re-arm an unchanged execution plan.

### `sync`

```bash
fkf sync --dry-run
fkf sync github-pull-requests --preview --date 2026-08-24
fkf sync --days 7
fkf sync --date 2026-08-24
fkf sync github-pull-requests --date 2026-08-24 --force
fkf sync --no-graph
```

Sync collects completed days that are missing and refreshes due index sources. `--force` replaces an existing document atomically. `--dry-run` prints rendered commands without execution. `--preview` executes and validates exactly one source once, shows its count and up to three projected records, and writes nothing; it cannot be combined with `--days`, `--force`, `--dry-run`, or `--no-graph`. `--no-graph` defers the derived graph rebuild. A `window: true` source runs once per contiguous missing date span rather than crossing an already collected gap.

### `status`

```bash
fkf status
fkf status --all
fkf status --max-age-hours 48
```

Status is the whole-base view: layers, evidence-envelope integrity, explicitly declared executable requirements, collector volume, trust, graph integrity, repository tracking policy, permissions, official-helper drift, managed skills, links, and unharvested task learning. It locates required executables but never runs a probe or declared source command. Findings include exact repair commands, but status never mutates the base. With `--max-age-hours`, every enabled source must have evidence within the requested age.

### `build`

```bash
fkf build
fkf build graph
fkf build wiki
fkf build wiki --check
```

The bare command writes the managed wiki-index block first, then rebuilds the graph from the bytes left on disk. `wiki --check` reports drift without writing. Derived outputs are caches.

### `new`

```bash
fkf new task open-graph-v1
fkf new project fkf --tag fkf
fkf new wiki retrieval-boundary --tag retrieval
```

The subcommands scaffold the strict write shape for task traces, projects, and wiki concepts without overwriting existing files. Project and wiki pages require at least one `--tag`; repeat the flag to add more.

### `config`

```bash
fkf config
fkf config helpers
fkf config helpers --refresh
fkf config schema
```

The bare command prints the resolved configuration plus override origins. `helpers` compares installed official helpers with the running binary; `--refresh` restores drifted installed helpers and missing official helpers required by the base, writing each replacement atomically while leaving custom scripts untouched. `schema` prints the generated JSON Schema for editor completion and validation.

### `mcp`

```bash
fkf mcp instructions --base ~/brain
fkf mcp serve --base ~/brain
```

The stdio server is read-only and requires an explicit base. It exposes bounded `context`, `find`, `list`, `read`, and `graph` operations but no body execution. Pageable tools return strict opaque cursors bound to their arguments and exact result snapshot. See [MCP server](../mcp/).

## Aliases and time bounds

One-letter aliases are the first letter, assigned to the command typed most often when two collide. Root `init`, `trust`, `status`, and `config` therefore have no alias. Subcommand aliases follow the same fixed vocabulary shown in `fkf --help`.

`--since` and `--until` accept `YYYY-MM-DD`, `today`, `yesterday`, or a positive relative window such as `7d`, `6w`, `3m`, or `1y`. Day keywords name one absolute day; use the same keyword on both bounds to select exactly that day.
