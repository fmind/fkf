---
title: Command reference
weight: 8
description: "The compact FKF CLI: retrieve, browse, collect, validate, and serve one base."
---

FKF has one binary and verb-first root commands. Terminal output defaults to text; piped output defaults to indented JSON. Override with global `--format text|json|jsonl` and select a base with `--base`.

The CLI help is authoritative and versioned with the binary:

```bash
fkf --help
fkf <command> --help
```

Stable exit codes are `0` success, `1` partial or operational failure, `2` configuration or usage, `3` untrusted base, and `130` cancellation.

Every mutating path takes one fail-fast cross-process writer lock for the physical base. A concurrent writer exits operationally instead of waiting or interleaving changes. Read-only commands, `sync --dry-run`, `sync --preview`, `learn propose --dry-run`, `learn review`, `trust --check`, and `build [target] --check` stay lock-free.

## Ask the base

### `context`

Build a ranked evidence pack under an exact token budget:

```bash
fkf context "retrieval boundary" --budget 4096 --explain
fkf context "repo:github.com/fmind/fkf" --expand
fkf context "collection" --pin wiki/explicit-sync-boundary.md
fkf context "What did I do yesterday?"
fkf context "last meeting notes"
fkf context "retrieval boundary" --since-receipt 0123456789abcdef
```

Use `--since` and `--until` for an explicit dated window, or put one supported temporal expression at the start or end of the query. Repeat `--pin` for approved wiki or project pages, use `--expand` for one shared-entity join, and `--explain` for the integer score breakdown. See [Context packs](../context/).

`--since-receipt` compares the current semantic candidates with the owner-only machine-local snapshot saved for an earlier `input_digest`. Only new or changed records and pages remain. The same query must be reused; a missing or expired snapshot fails with the command needed to seed it rather than treating every item as new.

### `brief`

Build the daily control surface under one complete-output budget:

```bash
fkf brief
fkf brief --budget 800
```

The fixed sections cover attention, today's calendar, due tasks, failing CI, open GitHub items assigned to the declared owner, yesterday's digest, and active projects touched this week. Attention includes failed source login probes, sources missing or older than 24 hours, and unharvested learning bullets. Brief reads stored evidence and runs only the enabled sources' trusted `auth:` probes; it never collects or fetches a body. Text and JSON share one receipt and both fit `--budget`.

### `day` and `timeline`

Render deterministic chronological activity without asking ranking to reconstruct a diary:

```bash
fkf day yesterday --budget 600
fkf day 2026-08-28 --all
fkf timeline --since 7d --repo repo:github.com/fmind/fkf
fkf timeline events/2026-08-28/meeting-notes.json#doc-id --around 2h
```

Day groups records by source, collapses repeated titles, and summarizes noisy sources unless `--all` is set. Timeline accepts source, repository, and person filters or centers a bounded range on one record. Both are stored, offline reads with reproducible receipts and equivalent MCP tools.

The digest budget measures only the representation actually delivered: terminal text, indented JSON, compact JSONL, or compact MCP JSON. `receipt.used_tokens` is that complete payload's byte length divided by four and rounded up; `json_tokens` and `text_tokens` remain comparison diagnostics. A budget below the smallest honest receipt fails and reports the format-specific minimum to retry.

### `who`

```bash
fkf who "Maxime Cordy"
fkf who actor:github.com/maxime
```

Who resolves only declared identity aliases. It returns the canonical node, matching authored pages, aliases, neighbourhood by kind, and the latest ten stored interactions. An interaction also admits directly linked stored records, such as the meeting notes attached to a matching calendar event, but the join never continues through those linked records. A neighbourhood over 200 edges is explicitly marked truncated while the newest interactions remain available. It never infers that two identities belong together and excludes owner identities from ordinary people summaries.

### `find`

Search every enabled layer exhaustively:

```bash
fkf find retrieval
fkf find "explicit sync" --layer wiki --layer projects
fkf find --where .state=MERGED --source github-pull-requests --since 7d
fkf find --since 7d --count
fkf find retrieval --format jsonl | jq -r .uri
fkf find "decision phrase" --bodies
```

Bare terms are the positional form of repeatable `--grep`; all terms must match. Record searches inspect scalar leaf values, never keys or container renderings. `--bodies` additionally searches only manifest-verified cached text and requires a term; it never invokes a body command. Repeatable `--where <jq-path>=<value>` tests any provider path in records and does not apply to pages. `--limit` bounds records and pages separately, so a long record list cannot hide authored pages.

Structured results omit the raw provider `record` and internal `days` selection by default. Add `--raw` when diagnosing a collector and you explicitly need those payloads; ordinary results keep the URI and projected fields needed to cite and filter the match.

With no term or filter, find lists records from the last seven populated days. `--count` reports per-day, per-source volumes instead of items; text output totals each source over the window.

### `read`

Open exactly one published URI:

```bash
fkf read wiki/retrieval-boundary.md
fkf read events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42
fkf read 'events/2026-05-04/github-pull-requests.json?jq=.records|length'
fkf read person:email/marc@example.test
```

`--limit` bounds directory listings and entity neighbourhoods. `--body` is the explicit execution path for a record URI and is unavailable over MCP. With the default `bodies: none`, it runs the trusted body argv and stores nothing. `cache` and `sync` sources reuse a valid cached copy; a miss runs the argv and writes a bounded manifest-verified cache entry. Entity reads are always offline graph views.

A bare `read` suggests the closest addressable URIs rather than guessing.

### `graph`

Inspect the edge cache or walk one neighbourhood:

```bash
fkf graph
fkf graph --verify
fkf graph ticket:jira/FKF-412 --in
fkf graph wiki/retrieval-boundary.md --in
fkf graph actor:github.com/marc --both --depth 2 --kind reviewer
fkf graph nodes --kind person
```

`--in` follows backlinks, `--out` follows destinations, and `--both` is the default; choose at most one. Depth is 1 to 3. On a neighbourhood, `--kind` accepts any observed **edge kind**, including base-defined relation fields such as `reviewer`. On `graph nodes`, `--kind` filters **node kinds** such as the `person` entity scheme. `graph nodes` lists every known node busiest first; there is no people-specific cache or subcommand. Bare `graph --verify` hashes every input and generated artifact without writing; it accepts no URI, walk flag, or subcommand.

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
fkf validate records --strict
fkf validate --lint --stale-days 90
```

Validation checks enabled wiki and project pages: their flat layout, slugs, required frontmatter, invisible characters, Markdown links, explicit `relations:` declarations, and fragment targets. It also checks whether one subject line is shared by more than half of any collected source. `--lint` adds orphan wiki pages, dangling authored URIs, relative frontmatter dates, expired validity windows, missing `supersedes` targets, and open projects untouched for the configurable horizon. `--strict` promotes every warning to an error. Task traces remain permissive evidence pages and are not part of the authored-page validator.

### `tags`

```bash
fkf tags
fkf tags wiki
fkf tags projects
```

Tags are listed with usage counts, most-used first. The bare command is the wiki vocabulary.

### `eval`

```bash
fkf eval
```

`evals/queries.yaml` is the base-owned retrieval acceptance set. It declares `k`, a recall threshold, and questions with a window, expected URIs, and forbidden URIs. `fkf init` creates one runnable entry-point check plus commented target-journey prompts, then leaves the file entirely owner-controlled on refresh. Replace or extend that baseline with exact URIs from the base. Evaluation reads stored evidence only and reports each context input digest and ranking version. A missed threshold or forbidden top-k result exits `1`; an invalid suite exits `2`.

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

Trust prints the base's executable plan, the ordinary `bin/` tree, and the test-only `tests/` tree, then records their canonical digest. A changed base leads with per-item differences; `--all` prints the complete disclosure. `--check` records nothing. Comments, YAML order, descriptions, examples, and retrieval-only mappings do not re-arm an unchanged execution plan.

### `test`

```bash
fkf test
fkf test github-pull-requests
fkf test --all
```

With no arguments, test runs hooks declared by enabled sources in stable name order. Naming sources includes disabled ones; `--all` runs every declared hook and cannot be combined with names. An empty selection preserves the compatible successful 0/0 report, so completion gates should name every mandatory source. A base-owned hook lives under the fully trust-digested `tests/` tree, which is prepended only for source tests; collection and body commands keep `bin/` first and never search `tests/`. Hooks run sequentially under the source timeout, capture no printable provider output, write no evidence, and continue after an ordinary failure so the report names every failed source. An untrusted base exits `3`; one or more hook failures exit `1`.

### `sync`

```bash
fkf sync --dry-run
fkf sync github-pull-requests --preview --date 2026-08-24
fkf sync --days 7
fkf sync --date 2026-08-24
fkf sync github-pull-requests --date 2026-08-24 --force
fkf sync --if-due
fkf sync --no-graph
```

Sync collects completed days that are missing and refreshes due index sources. A source's optional trust-covered `auth:` command runs once only when that source has due work; an ordinary provider exit skips the source as `auth-required` without failing other collection or exposing probe output. Missing executables, timeouts, signals, unsafe paths, trust drift, and runner failures remain hard errors. `--force` replaces an existing document atomically. `--dry-run` prints rendered commands without execution. `--preview` executes the auth probe and validates exactly one source once, shows its count and up to three projected records, and writes nothing; it cannot be combined with `--days`, `--force`, `--dry-run`, or `--no-graph`. `--if-due` cannot be combined with `--force`, `--dry-run`, or `--preview`; it checks for work without taking the writer lock and returns a compact success when nothing is due. `--no-graph` defers the derived graph rebuild. A `window: true` source runs once per contiguous missing date span rather than crossing an already collected gap.

### `learn`

```bash
fkf learn propose --dry-run
fkf learn propose
fkf learn review
fkf learn review <proposal> --diff
fkf learn apply <proposal>
fkf learn reject <proposal>
```

Learn is the approval boundary between session evidence and durable knowledge. `propose --dry-run` lists unharvested log candidates with their task-trace citations and writes nothing; without it, FKF stages one deterministic `wiki/log.md` unified diff under `.agents/tmp/learn/`. `review` is a bounded, lock-free read of the active queue.

An agent may also stage a canonical unified diff there for a concept or project change. Its filename is the diff's full lowercase SHA-256 digest, which binds an approval to the exact bytes reviewed. The diff may target only flat `wiki/*.md` and `projects/*.md` pages; deletion, rename, nesting, and every other path are rejected. `apply` rechecks that digest and current file context, writes atomically, runs the existing strict validators for each affected layer, rebuilds derived caches, and archives the accepted diff. Any failure restores the exact authored and cache bytes from before the attempt. `reject` archives the diff without touching knowledge. Repeating an accepted or rejected action is idempotent.

### `schedule`

```bash
fkf schedule install
fkf schedule status
fkf schedule remove
fkf schedule install --dry-run
```

Schedule manages one hourly user unit for the selected base: a systemd user service and timer on Linux, or a launchd agent on macOS. It pins the current FKF executable and absolute base path, exports explicit `HOME` and a sanitized absolute `PATH`, runs `sync --if-due`, then `build --if-stale`. Status and removal inspect the manager independently from the files, so an orphaned active unit remains visible and removable. Dry runs write no file and perform no scheduler mutation; they may run read-only manager status probes.

### `status`

```bash
fkf status
fkf status --max-age-hours 48
fkf status --live
```

Status is the whole-base view: layers, evidence-envelope integrity, explicitly declared ordinary command requirements, separately checked source-hook entrypoints, collector volume, trust, graph integrity, lexical-index cache health, repository tracking policy, permissions, official-helper drift, managed skills, links, and unharvested task learning. It locates executables but never runs a probe or declared source command. The JSON summary keeps `missing_requirements` and `missing_test_hooks` distinct. Findings include exact repair commands, but status never mutates the base. With `--max-age-hours`, every enabled source must have evidence within the requested age.

`--live` additionally runs each enabled source's trusted `auth:` probe and inspects user-scope harness registrations. Probe output is discarded; the report exposes only the source names that require login. Because this crosses the declared execution boundary, ordinary `status` remains the offline default.

### `build`

```bash
fkf build [--check]
fkf build graph [--check]
fkf build index [--check]
fkf build wiki [--check]
fkf build --if-stale
fkf build bodies --prune [--older-than <dur>] [--source <name>]
```

The bare command, spelled explicitly as `fkf build all`, writes the managed wiki-index block first, rebuilds the graph from the bytes left on disk, then rebuilds the ignored lexical index. `build index` rebuilds only that lexical cache. `--check` reports whether the selected target cache is stale and writes nothing. `--if-stale` checks every selected cache without a writer lock and rebuilds only after detecting drift; it cannot be combined with `--check`. `bodies --prune` requires the explicit flag and prunes the ignored body cache (with optional `--older-than <dur>` and `--source <name>` filters); no evidence record or provider link changes. Derived outputs are caches.

### `new`

```bash
fkf new task open-graph-v1
fkf new project fkf --tag fkf
fkf new wiki retrieval-boundary --tag retrieval
fkf new helper collect-prs.sh
fkf new helper collect-prs.py
```

The subcommands scaffold the strict write shape for task traces, projects, wiki concepts, and owner-only helpers without overwriting existing files. Project and wiki pages require at least one `--tag`; repeat the flag to add more. Helpers require an explicit `.sh` or `.py` extension; the generated `requires:` list includes `python3` for a Python helper.

`new helper` creates collection or body support under `bin/`. Source verification hooks belong under the base's `tests/` tree and are declared directly in `test:`.

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

The stdio server is read-only and requires an explicit base. It exposes bounded `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph` operations but no body execution. Pageable tools return strict opaque cursors bound to their arguments and exact result snapshot. See [MCP server](../mcp/).

### `harness`

```bash
fkf harness list
fkf harness print claude
fkf harness install claude codex
fkf harness install --all --dry-run
fkf harness install --all --check
```

Harness manages one base's MCP, session-start hook, and skill integrations at user scope. `list` prints the closed supported vocabulary; `print` emits the exact managed fragments without writing; `install` preserves unrelated configuration and refuses an FKF key owned by something else. Use `--dry-run` to inspect changes and `--check` to fail when an installed bridge is missing or drifted. Every managed launch pins the executable and base by absolute path. See [Agent harnesses](../harnesses/).

### `upgrade`

```bash
fkf upgrade
```

Upgrade resolves the latest stable GitHub release, downloads the archive and `checksums.txt` for the current Linux or macOS architecture through `curl`, verifies the SHA-256 checksum, executes the staged binary to confirm its version, and atomically replaces the executable that launched the command. It reads no base and runs no source declaration. Use the installation method instead when that executable is managed elsewhere or not user-writable.

## Aliases and time bounds

One-letter aliases are the command's first letter, assigned to the command typed most often when commands collide; built-in `help` keeps `h`. A command without the available first letter is typed in full. Subcommand aliases follow the same fixed vocabulary shown in `fkf --help`.

`--since` and `--until` accept `YYYY-MM-DD`, `today`, `yesterday`, or a positive relative window such as `7d`, `6w`, `3m`, or `1y`. Day keywords name one absolute day; use the same keyword on both bounds to select exactly that day.
