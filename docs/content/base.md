---
title: The base and fkf.yaml
weight: 3
description: "The five-layer base, open semantic schema, configuration discovery, local overrides, and generated contract."
---

A base is one git repository of plain JSON and Markdown. Its committed `fkf.yaml` is both configuration and disclosure boundary: it says which layers exist, what semantic fields mean, and which commands may collect data. Two boundaries are two repositories, not two profiles in one file.

## The five layers

| Path                               | Holds                                                      |
| ---------------------------------- | ---------------------------------------------------------- |
| `events/YYYY-MM-DD/<source>.json`  | one complete collected document per source and day         |
| `index/<name>.json`                | one current point-in-time document per index source        |
| `tasks/YYYY-MM-DD/<slug>/TASKS.md` | request, work trace, changed files, evidence, and learning |
| `projects/<slug>.md`               | intent and decisions over weeks; `status` is required      |
| `wiki/<slug>.md`                   | durable approved knowledge, flat [OKF v0.2](../okf/)       |

Root `graph.tsv` and `graph.meta.json` are one rebuildable cache generation. The source of truth is the relation schema stored with collected documents plus authored links, tags, and explicit Markdown `relations:`. `fkf build graph` atomically replaces each file; because two renames cannot be one filesystem operation, readers validate the sidecar digest and fail closed during the brief publication window rather than mix generations.

The sidecar carries separate SHA-256 inputs for events, index, projects, tasks, wiki, and the edge-relevant root schema, plus one aggregate and the exact `graph.tsv` output digest. A stale read therefore names which logical component changed instead of reporting one opaque base-wide mismatch.

FKF keeps this plain-file design until measurements justify another storage layer. `mise run benchmark` builds a reproducible synthetic corpus of exactly 100,000 records and 500,000 edges, then reports wall time and maximum RAM for find, context, graph build, and navigation. Maximum RAM is the measured peak resident set size (RSS): the largest amount of physical memory occupied during that run. It is an optional observation with no pass/fail threshold and no database claim.

Every layer is explicitly enabled:

```yaml
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
```

An absent layer entry is disabled. Disabled layers are not created, listed, served, scanned, or addressable.

## Finding a base

FKF tries, in order:

1. `--base <path>`;
1. `$FKF_BASE`;
1. the nearest ancestor containing `fkf.yaml`.

A leading `~` is expanded by FKF, including in an argv-based MCP launch. Nothing creates a base implicitly; use `fkf init <path>`.

## The configuration contract

Every base configuration declares `fkf: 1`, and every stored document independently declares the same marker value for its distinct evidence envelope. The containing file identifies which contract applies. Compatible evidence additions stay within marker `1`; an incompatible configuration or evidence change requires an explicit new marker and release boundary.

```yaml
fkf: 1
name: brain

schema:
  id: { description: Stable record identity., cardinality: one }
  time: { description: Event timestamp., cardinality: one }
  repository: { description: Related repository., cardinality: optional, relation: true }

layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true

sources:
  git-commits:
    enabled: true
    layer: events
    requires: [git-log-json.sh, git]
    window: true
    run: [git-log-json.sh, "{{start}}", "{{end}}", "{{home}}"]
    fields: { id: .uid, time: .time, repository: .repository_uri }

sync:
  days: 30
  index_max_age_hours: 168
  timeout: 2m0s
  concurrency: 4
```

Root `schema:` is the semantic dictionary shared by every source. The dedicated [Configuration schema](../schema/) guide defines cardinality and relation fields, explains source mappings, and shows how to bind an editor to the generated [`fkf.schema.json`](https://fmind.github.io/fkf/fkf.schema.json). `fkf config schema` prints the same artifact without requiring a base.

## Machine-local overlay

The ignored `fkf.local.yaml` contains execution facts that differ by machine. It may add external `bin:` directories and override `enabled`, `run`, or `timeout` for a source already declared in `fkf.yaml`. It cannot introduce a source, redefine the schema, or declare environment values.

Extra `bin:` entries must be absolute or `~`-relative directories outside the base. Provider accounts and credentials remain process environment owned by the provider CLI; export a selector such as `GH_CONFIG_DIR` before launching FKF, or use a reviewed executable wrapper under `bin/`. FKF reads no env file, expands no `$VAR`, and owns no credential.

The ordinary child command path is composed from `<base>/bin/`, declared external `bin:` directories, and safe inherited absolute entries. Relative entries and inherited entries resolving inside the base are removed. Source tests prepend the separately trust-digested `<base>/tests/` tree to that path; collection and body commands never search it.

`fkf config` prints the merged result and the origin of every local override.

## Trust follows execution

`fkf trust` hashes a canonical execution plan, not YAML bytes. Changes to `run:`, `test:`, `body:`, enabled state, body-bound paths, timeouts, retries, pacing, extra executable directories, or files and executable bits under `bin/` or `tests/` re-arm trust. Comments, YAML key order, schema descriptions and examples, `requires:`, retrieval-only field-path changes, and the inherited process environment do not.

The disclosure printed before trust remains the authority. Trust is local change detection, never a shell sandbox.

Mutating CLI paths share one fail-fast cross-process lock keyed by the physical base, so symlink aliases cannot admit two FKF writers. Read-only commands and write-free checks or previews remain lock-free.

## Managed repository files

`fkf init` refreshes marked blocks in `.gitignore` and `.gitattributes` without touching surrounding content. The ignore block covers local configuration, common secret-bearing files, root `graph.tsv` and `graph.meta.json`, and optional collected data according to the choice made at initialization. The attributes block prevents line-merging complete JSON documents. The graph files need no merge rule because both are rebuilt from source files.

Use `fkf init` again to refresh FKF-owned skills and managed blocks. It does not overwrite base-specific `AGENTS.md`, custom skills, existing helpers under `bin/`, or source hooks under `tests/`. Use `fkf config helpers` to inspect installed official helpers and missing required ones, then `fkf config helpers --refresh` for an explicit refresh whose individual file replacements are atomic; unknown scripts remain user-owned.
