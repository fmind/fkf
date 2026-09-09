---
title: Getting started
description: "Install FKF, explore a synthetic base, collect local activity, and connect a coding agent."
---

# Getting started

FKF is one Python command and one base: a git repository of plain JSON and Markdown. There is no service, database, provider SDK, or credential store. Sources are commands, and the CLI each command names owns its login.

## Install

FKF requires Python 3.14. Install the package and a persistent `fkf` launcher with [uv](https://docs.astral.sh/uv/):

```bash
uv tool install fkf
```

For one command without a persistent installation, let `uvx` create an isolated environment:

```bash
uvx fkf --version
uvx fkf init ~/demo --demo 30
```

Use the persistent installation for `harness install` and `schedule install`. Those integrations pin the current launcher and deliberately reject an ephemeral `uvx` process whose environment disappears after the command.

Upgrade through the same package manager:

```bash
uv tool upgrade fkf
```

Wheel and source distributions are published to PyPI and attached to the [matching GitHub release](https://github.com/fmind/fkf/releases/latest) with build-provenance attestations. Configuration and evidence compatibility is independent of the package installation mechanism.

To install from a clone with the repository's pinned toolchain:

```bash
git clone https://github.com/fmind/fkf.git
cd fkf
mise trust -y
mise install --locked
mise run install
```

Run checkout commands as `uv run fkf ...`. `mise run build` creates and verifies the wheel and source distribution under `dist/`.

## Explore without connecting a provider

Create a synthetic base:

```bash
fkf init ~/demo --demo 30
fkf status --base ~/demo
fkf find --base ~/demo retrieval
fkf context --base ~/demo "retrieval boundary" --budget 1024 --explain
```

The minimal configuration declares no source. `--demo` adds six synthetic sources with deterministic local documents, pages, and explicit relation fields without reading machine state or running a provider command. It refuses to mix synthetic data into an existing collected base.

Terminal output defaults to text. Pipes and redirects default to indented JSON; use `--format text|json|jsonl` to override it.

The important retrieval split is:

- `find` returns every lexical match and is ideal for exhaustive questions;
- `context` selects the strongest evidence under a hard token budget and returns a reproducible receipt;
- `read` opens one URI;
- `graph` follows only declared relationships and authored links.

## Create your base

```bash
fkf init ~/brain --preset personal
export FKF_BASE=~/brain
fkf status
```

The personal preset declares a small supported set of local and provider sources, and four local event sources start enabled — `fkf status` names them. Git commits, coding-agent session metadata, and touched agent-memory file metadata omit prompts and responses. The memory-file source also prefetches each full file into the ignored, manifest-verified body cache under its declared `bodies: sync` policy; the text does not enter the stored document. The fourth source, `agent-session-traces`, stores bounded request and assistant excerpts from completed normalized sessions as ordinary untrusted JSON evidence. It makes no model call and reads no changed file content; [Agent harnesses](harnesses.md) describes that store. Collection never creates a task page. Together the enabled helpers use `python3` and `git` on Linux and macOS. Shell-history metadata, repository facts, and every network source start disabled. Enable only the sources whose data boundary and prerequisites you have reviewed.

Initialization creates:

- `fkf.yaml`, with `fkf: 1`, a root semantic schema, all source defaults, and no secrets;
- five enabled layers: `events/`, `index/`, `tasks/`, `projects/`, and `wiki/`;
- managed blocks in `.gitignore` and `.gitattributes`;
- a minimal base-specific `AGENTS.md` and the copied `fkf-use`, `fkf-learn`, and `daily-brief` skills;
- non-overwriting Claude bridges;
- helpers required by initially enabled sources and the session-start hook under trust-digested `sources/`;
- `checks/queries.yaml`, the owner-controlled retrieval acceptance set `fkf eval` runs;
- a git repository with owner-only files.

Running `fkf init ~/brain` again refreshes FKF-owned skills and managed blocks. It preserves `fkf.yaml`, `AGENTS.md`, custom skills, existing bridges, and existing helpers. After enabling a preset source, run `fkf config helpers --refresh` to install any newly required official helper. `fkf config helpers` compares official helpers with the running binary, and refresh leaves custom scripts untouched.

## Setup checklist

1. Initialize a base with a unique `--name`, then select it explicitly with `--base` when more than one base is connected.
1. Review `fkf.yaml`; enable only explicit sources and choose provider accounts in the launching process, never in the base.
1. Refresh official helpers and run the named fake-backed hooks for the sources you enabled.
1. Inspect `fkf sync --dry-run`, then `fkf trust --check` and the complete `fkf trust --all` disclosure before allowing execution.
1. Preview each provider source, collect the intended window, and inspect the stored JSON.
1. Run `fkf build`, `fkf eval`, and the base's validation gate before relying on retrieval.
1. Inspect `fkf harness install --all --dry-run`, install that base's MCP entries, then pass `--workspace` explicitly when adding supported context hooks.

## Enable and collect one source

Open `fkf.yaml`, enable one source whose provider and data boundary you understand, then inspect every executable step before collecting:

```bash
$EDITOR "$FKF_BASE/fkf.yaml"
fkf config helpers --refresh
fkf sync --dry-run
fkf trust --check
fkf trust --all
fkf sync github-pull-requests --preview --date 2026-08-24
fkf sync --days 7
```

Sources are direct argv, not shell strings. The root [configuration schema](schema.md) defines shared field meanings; the [source guide](sources.md) covers helpers, requirements, windows, bodies, and failure behavior; [privacy and security](privacy.md) explains the trust digest. `--preview` executes and validates one source once but writes nothing. Normal sync is resumable and all-or-nothing per source/day.

After collection:

```bash
fkf status
fkf find --since 7d --count
fkf find retrieval --since 7d
fkf find --where .state=MERGED --source github-pull-requests
fkf read events/2026-08-24/github-pull-requests.json#https://github.com/fmind/fkf/pull/42
fkf graph repo:github.com/fmind/fkf --in
```

Every result prints a URI accepted by `read`. Generic `--grep` and `--where` replace type-specific filters; graph entities may use any base-defined, non-reserved lowercase scheme.

## Connect an agent

Install the managed harness bridges for the first compact repository-scoped pack and the read-only MCP server for later questions:

```bash
fkf --base ~/brain harness print claude
fkf --base ~/brain harness install --all
```

`print` lets you inspect the exact integration first. `install` pins the current executable and absolute base in every managed entry, and wraps base-owned hook execution in a trust check. The server exposes bounded `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph` operations. It cannot write, collect, or fetch record bodies. Pageable calls return opaque cursors bound to the normalized effective query and result snapshot. `--base` is required so the launch command states the disclosure boundary.

Keep the base's `AGENTS.md` minimal and specific to that base. FKF instructions belong in the copied skills, and reusable custom workflows belong in their own `.agents/skills/<name>/` packages.

## Share one team base

Use one designated collector. Other team members pull the reviewed JSON and Markdown through Git and keep collection disabled locally; FKF's base lock coordinates processes on one machine, not collectors on different machines.

```bash
fkf init ~/team-brain --name team --preset team --track-collected
$EDITOR ~/team-brain/fkf.yaml # replace one GitHub repository and Jira project/site/filter
fkf --base ~/team-brain config helpers --refresh
fkf --base ~/team-brain test github-issues github-pull-requests jira-issues
fkf --base ~/team-brain sync --dry-run
fkf --base ~/team-brain trust --check
fkf --base ~/team-brain trust --all
GH_CONFIG_DIR=~/.config/gh-team fkf --base ~/team-brain sync jira-issues --preview
```

Select the Jira site in ACLI's machine-local configuration with `acli jira auth switch`; keep its credentials and GitHub's `GH_CONFIG_DIR` out of the base. Preview and collect each enabled source deliberately. Then inspect `git status`, the projected JSON, and `git diff` before a separately authorized commit and push.

`--track-collected` is the durable sharing decision: `git check-ignore events index` should report neither layer, and `git ls-files events index` names collected documents after they are explicitly added. `fkf.local.yaml`, `bodies/`, and `index/.fkf-index.*` stay ignored because they contain machine-local configuration or rebuildable caches. A second clone can run `fkf validate records`, `fkf build`, and offline reads without provider access.

## Next

- [The base and fkf.yaml](base.md) — configuration, schema, discovery, and trust inputs.
- [Configuration schema](schema.md) — semantic field declarations, provider mappings, and editor validation.
- [Sources are commands](sources.md) — command composition, requirements, cardinality, storage, and bodies.
- [URIs and the graph](uris-graph.md) — open entity schemes and transcription-only edges.
- [Command reference](commands.md) — the compact CLI surface.
- [Context packs](context.md) — ranking, budget, expansion, and receipt.
- [MCP server](mcp.md) and [Agent harnesses](harnesses.md) — agent integration.
- [Privacy and security](privacy.md) — the exact trust and data boundary.
- [The wiki format](okf.md) — authored knowledge and explicit relations.
