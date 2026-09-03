---
title: Getting started
weight: 2
description: "Install FKF, explore a synthetic base, collect local activity, and connect a coding agent."
---

FKF is one Go binary and one base: a git repository of plain JSON and Markdown. There is no service, database, provider SDK, or credential store. Sources are commands, and the CLI each command names owns its login.

## Install

The installer selects the latest Linux or macOS release archive for amd64 or arm64, verifies it against the published checksums, and writes to `~/.local/bin` without `sudo`:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | sh
```

The installer selects the published Linux or macOS archive for the current architecture, verifies its checksum, validates the binary, and atomically replaces the destination. An existing installation remains intact if staging fails.

For cryptographic release-provenance verification, authenticate the GitHub CLI and require the published attestation before installation:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | FKF_VERIFY_ATTESTATION=1 sh
```

Set `FKF_INSTALL_DIR` to an absolute directory to change the destination, or `FKF_VERSION=v3.0.2` to pin a release. You can also download an archive and `checksums.txt` directly from the [latest release](https://github.com/fmind/fkf/releases/latest). Each archive contains the binary, project license, README, and checked-in notices for the linked Go runtime and dependencies.

The module intentionally remains `github.com/fmind/fkf`. Go's major-version import rules therefore keep `go install github.com/fmind/fkf/cmd/fkf@latest` on the v1 line; use a release archive for v2 and later. Source builds remain available from a tagged checkout.

Once FKF is installed, upgrade the executable that launched it:

```bash
fkf upgrade
```

The command uses `curl` only against fixed `github.com` release endpoints, selects the archive for the current Linux or macOS architecture, verifies its published SHA-256 checksum, runs the downloaded binary to confirm its version, and atomically replaces the current executable. It never opens or changes a base. If the executable is not user-writable, upgrade through the mechanism that installed it. To require GitHub's release attestation as well as its checksum, rerun the installer with `FKF_VERIFY_ATTESTATION=1`.

From a clone, the project gate builds the canonical repository artifact at `bin/fkf`:

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

The personal preset declares a small supported set of local and provider sources. Three local metadata collectors start enabled: git commits, coding-agent session metadata without prompts or responses, and touched agent-memory files without bodies. They use `git`, `jq`, `sqlite3`, and the standard `find`, `stat`, and `touch` utilities on supported Linux and macOS systems. Shell-history metadata and every network source start disabled. Enable only the sources whose data boundary and prerequisites you have reviewed.

Initialization creates:

- `fkf.yaml`, with `fkf: 1`, a root semantic schema, all source defaults, and no secrets;
- five enabled layers: `events/`, `index/`, `tasks/`, `projects/`, and `wiki/`;
- managed blocks in `.gitignore` and `.gitattributes`;
- a minimal base-specific `AGENTS.md` and the copied `fkf-use`, `fkf-learn`, and `daily-brief` skills;
- non-overwriting Claude bridges;
- helpers required by initially enabled sources and the session-start hook under trust-digested `bin/`;
- a git repository with owner-only files.

Running `fkf init ~/brain` again refreshes FKF-owned skills and managed blocks. It preserves `fkf.yaml`, `AGENTS.md`, custom skills, existing bridges, and existing helpers. After enabling a preset source, run `fkf config helpers --refresh` to install any newly required official helper. `fkf config helpers` compares official helpers with the running binary, and refresh leaves custom scripts untouched.

## Understand fields before enabling sources

The root schema defines shared meanings. Each source only maps provider paths to those names:

```yaml
fkf: 1
schema:
  id: { description: Stable record identity., cardinality: one }
  time: { description: Record timestamp when the provider exposes one., cardinality: optional }
  title: { description: Human-readable record label., cardinality: optional }
  repo: { description: Raw repository value used by body argv., cardinality: optional }
  repository:
    description: Repository associated with the record.
    cardinality: optional
    relation: true
    examples: [repo:github.com/fmind/fkf]
  participant:
    description: Person or account involved in the record.
    cardinality: many
    relation: true
    examples: [person:email/user@example.test, actor:github.com/login]
  owner:
    description: Person or account assigned to the record.
    cardinality: many
    relation: true

sources:
  github-pull-requests:
    enabled: true
    layer: events
    requires: [github-search-json.sh, gh, jq]
    window: true
    run: [github-search-json.sh, prs, assignee, "{{start}}", "{{end}}"]
    fields:
      id: .url
      time: .updatedAt
      title: .title
      repo: .repository.nameWithOwner
      repository: .repository_uri
      owner: [".assignee_uris[]"]
      participant: [".participant_uris[]"]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --json, "body,comments"]
```

The field name states the role; the URI states the identity namespace. `participant` is unambiguous where `people` is not, while values can distinguish an email person from a GitHub actor. FKF validates and transcribes those choices without merging identities or inferring relationships.

Every collected source maps `title`, and every new record must produce a non-empty, control-free value. A metadata source uses the provider's natural subject; a content-first helper derives one stable line, at most 160 characters, while leaving the full text behind `body:`.

Keep direct provider argv in `run:`. Move pipelines and expansion to a `.sh` or `.py` helper under `bin/`; use Python when structured or stateful logic would be clearer. The helper's shebang selects its interpreter, so the same base works on a Mac whose interactive shell is Zsh and on a Linux host using Bash or Fish.

Declare each ordinary collection/body dependency and every external tool a hook invokes explicitly in `requires:`. `status` locates those bare names without parsing command text or executing a version probe. Put the base-owned `test[0]` entrypoint under `tests/`; status reports it separately on the test-only PATH.

## Review execution, then collect

Changing a command or helper invalidates local trust:

```bash
fkf config helpers --refresh
fkf sync --dry-run
fkf sync github-pull-requests --preview --date 2026-08-24
fkf trust --check
fkf trust --all
fkf sync --days 7
```

`trust` prints the executable plan before recording its canonical digest. Command, policy, body-bound path, executable path, and `bin/` or `tests/` changes re-arm trust. Comments, YAML order, descriptions, examples, `requires:`, retrieval-only mappings, and the inherited process environment do not.

`--preview` performs one real trusted execution and every decode, projection, cardinality, relation, and completeness check for exactly one source, shows at most three projected records, and writes nothing. Normal sync never collects today. A completed day is complete or absent: failed commands, invalid or oversized output, missing identities or times, cardinality errors, and invalid relation URIs write nothing. Existing documents are skipped unless `--force` is used; a `window: true` source runs once per contiguous missing span instead of crossing an existing day. The graph cache is rebuilt unless `--no-graph` is supplied.

This makes normal sync safe to run repeatedly: existing event documents and still-fresh index snapshots are skipped, due index snapshots are refreshed, and a failed unit remains absent. Rerunning the same command resumes missing collection and retries a failed derived rebuild. Only `--force` deliberately re-collects and atomically replaces existing documents.

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
