---
title: Getting started
weight: 2
description: "Install FKF, explore a synthetic base, collect local activity, and connect a coding agent."
---

FKF is one Go binary and one base: a git repository of plain JSON and Markdown. There is no service, database, provider SDK, or credential store. Sources are commands, and the CLI each command names owns its login.

## Install

The installer selects the latest Linux or macOS archive for amd64 or arm64, verifies its checksum and binary, and atomically writes it to `~/.local/bin` without `sudo`. An existing installation remains intact if staging fails:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | sh
```

For cryptographic release-provenance verification, authenticate the GitHub CLI and require the published attestation before installation:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | FKF_VERIFY_ATTESTATION=1 sh
```

Set `FKF_INSTALL_DIR` to an absolute directory to change the destination, or `FKF_VERSION=vX.Y.Z` to pin a release. You can also download an archive and `checksums.txt` directly from the [latest release](https://github.com/fmind/fkf/releases/latest). Each archive contains the binary, project license, README, and checked-in notices for the linked Go runtime and dependencies.

The module intentionally remains `github.com/fmind/fkf`. Go's major-version import rules therefore keep `go install github.com/fmind/fkf/cmd/fkf@latest` on the v1 line; use a release archive for v2 and later. Source builds remain available from a tagged checkout.

Once FKF is installed, upgrade the executable that launched it:

```bash
fkf upgrade
```

The command uses `curl` only against fixed `github.com` release endpoints, selects the archive for the current Linux or macOS architecture, verifies its published SHA-256 checksum, runs the downloaded binary to confirm its version, and atomically replaces the current executable. It never opens or changes a base. If the executable is not user-writable, upgrade through the mechanism that installed it. To require GitHub's release attestation as well as its checksum, rerun the installer with `FKF_VERIFY_ATTESTATION=1`.

To install from a clone with the repository's pinned toolchain:

```bash
git clone https://github.com/fmind/fkf.git
cd fkf
mise trust -y
mise install --locked
mise run install
```

A bare `go build ./cmd/fkf` writes a temporary `./fkf` by Go convention. It is ignored; `bin/fkf` is the project artifact and release archives put `fkf` at their root.

## Explore without connecting a provider

Create a synthetic base:

```bash
fkf init ~/demo --demo 30
fkf status --base ~/demo
fkf find --base ~/demo retrieval
fkf context --base ~/demo "retrieval boundary" --budget 1024 --explain
```

The minimal configuration declares no source. `--demo` adds deterministic local documents, pages, and explicit relation fields without reading machine state or running a provider command. It refuses to mix synthetic data into an existing collected base.

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

The personal preset declares a small supported set of local and provider sources, and four of them start enabled — `fkf status` names them. Three write metadata-only evidence to `events/`: git commits, coding-agent session metadata without prompts or responses, and touched agent-memory file metadata. The memory-file source also prefetches each full file into the ignored, manifest-verified body cache under its declared `bodies: sync` policy; the text does not enter the stored document. The fourth, `agent-session-traces`, writes one bounded task skeleton per completed session into `tasks/`: your requests and the last assistant message as inert code blocks, plus changed paths from `git status`. It makes no model call and reads no changed file content; [Agent harnesses](../harnesses/) describes that store. Together they use `git`, `jq`, `sqlite3`, and the standard `find`, `stat`, `touch`, and `xargs` utilities on supported Linux and macOS systems. Shell-history metadata and every network source start disabled. Enable only the sources whose data boundary and prerequisites you have reviewed.

Initialization creates:

- `fkf.yaml`, with `fkf: 1`, a root semantic schema, all source defaults, and no secrets;
- five enabled layers: `events/`, `index/`, `tasks/`, `projects/`, and `wiki/`;
- managed blocks in `.gitignore` and `.gitattributes`;
- a minimal base-specific `AGENTS.md` and the copied `fkf-use`, `fkf-learn`, and `daily-brief` skills;
- non-overwriting Claude bridges;
- helpers required by initially enabled sources and the session-start hook under trust-digested `bin/`;
- `evals/queries.yaml`, the owner-controlled retrieval acceptance set `fkf eval` runs;
- a git repository with owner-only files.

Running `fkf init ~/brain` again refreshes FKF-owned skills and managed blocks. It preserves `fkf.yaml`, `AGENTS.md`, custom skills, existing bridges, and existing helpers. After enabling a preset source, run `fkf config helpers --refresh` to install any newly required official helper. `fkf config helpers` compares official helpers with the running binary, and refresh leaves custom scripts untouched.

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

Sources are direct argv, not shell strings. The root [configuration schema](../schema/) defines shared field meanings; the [source guide](../sources/) covers helpers, requirements, windows, bodies, and failure behavior; [privacy and security](../privacy/) explains the trust digest. `--preview` executes and validates one source once but writes nothing. Normal sync is resumable and all-or-nothing per source/day.

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

## Next

- [The base and fkf.yaml](../base/) — configuration, schema, discovery, and trust inputs.
- [Configuration schema](../schema/) — semantic field declarations, provider mappings, and editor validation.
- [Sources are commands](../sources/) — command composition, requirements, cardinality, storage, and bodies.
- [URIs and the graph](../uris-graph/) — open entity schemes and transcription-only edges.
- [Command reference](../commands/) — the compact CLI surface.
- [Context packs](../context/) — ranking, budget, expansion, and receipt.
- [MCP server](../mcp/) and [Agent harnesses](../harnesses/) — agent integration.
- [Privacy and security](../privacy/) — the exact trust and data boundary.
- [The wiki format](../okf/) — authored knowledge and explicit relations.
