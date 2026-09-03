# fkf — Fmind Knowledge Framework

Coding agents know your repository, but not the work around it: the meeting that set a constraint, the ticket that explains a decision, the review that rejected an approach, or the page you read last week.

`fkf` keeps that context in a git repository you own. It collects metadata from commands you already trust, stores plain JSON and Markdown, links records and pages through explicit URIs, and gives an agent a small, reproducible context pack with a receipt.

It is one binary with no account, daemon, database, telemetry, or credential store. Stored reads are offline. Network access belongs to the provider CLIs you choose during collection or an explicit body fetch, plus the fixed GitHub release downloads made by `fkf upgrade` when you invoke it.

## Try it on your GitHub pull requests

Start with one real source. The personal preset includes a reviewed GitHub Search helper; enable `github-pull-requests` in the generated `fkf.yaml`, then collect recently updated pull requests assigned to you:

```bash
gh auth status
fkf init ~/brain --preset personal
$EDITOR ~/brain/fkf.yaml # set sources.github-pull-requests.enabled to true
fkf config helpers --refresh --base ~/brain
fkf trust --all --base ~/brain
fkf sync github-pull-requests --days 30 --base ~/brain
fkf find --source github-pull-requests --since 30d --limit 10 --base ~/brain
fkf context "repo:github.com/OWNER/REPOSITORY" --since 30d --budget 2048 --explain --base ~/brain
```

`gh` owns the login and network access. FKF stores the projected metadata as plain JSON, links each pull request to its repository URI, and answers later reads offline. A body remains at GitHub until an explicit, trust-gated `fkf read --body` call. Replace `OWNER/REPOSITORY` with one repository shown by `find`; the exact URI makes the context selection reproducible.

## The model

Four ideas cover most of FKF:

1. **A base is a folder.** It is one git repository with five readable layers: dated events, current indexes, agent task traces, project pages, and durable wiki knowledge. `ls`, `rg`, and `jq` still work.
1. **A source is a command.** A source in `fkf.yaml` runs a reviewed command that prints one JSON document. The named CLI owns its login. Adding GitHub, Google Workspace, Jira, a local database, or another provider does not require a Go adapter.
1. **Relations are explicit URIs.** Records and Markdown pages link to file URIs or base-defined entities such as `repo:github.com/fmind/fkf`. FKF builds `graph.tsv` at the base root from declared relation fields and authored links; it never guesses relationships from prose.
1. **Retrieval is bounded and reproducible.** `find` returns every lexical match. `context` selects the strongest evidence under a token budget and explains the selection in a receipt. There are no embeddings or model calls in the read path.

```text
events/YYYY-MM-DD/     one complete JSON document per event source
index/                 current point-in-time source documents
tasks/                 agent session traces and learned items
projects/              active, paused, or completed efforts
wiki/                  reusable decisions, patterns, tools, and insights
graph.tsv              rebuildable relation cache at the base root
graph.dst.tsv          destination-sorted graph twin
graph.offsets.tsv      byte ranges used by graph walks
graph.meta.json        integrity metadata for that exact graph generation
graph.generation.json  atomic publication state for the graph generation
index/.fkf-index.*     ignored, rebuildable lexical candidate cache
```

This differs from a live connector: FKF preserves history, works offline, and can join local activity with provider metadata. When a coding harness sends a selected context pack to a model, that slice is governed by the harness or model provider's data policy.

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

Set `FKF_INSTALL_DIR` to an absolute directory to change the destination, or `FKF_VERSION=v2.0.0` to pin a release. You can also download an archive and `checksums.txt` directly from the [latest release](https://github.com/fmind/fkf/releases/latest). Each archive is one installation unit containing the binary, license, README, and linked-dependency notices.

The module intentionally remains `github.com/fmind/fkf`. Go's major-version import rules therefore keep `go install github.com/fmind/fkf/cmd/fkf@latest` on the v1 line; use a release archive for v2. Source builds remain available from a tagged checkout.

Once FKF is installed, upgrade the executable that launched it:

```bash
fkf upgrade
```

The command uses `curl` only against fixed `github.com` release endpoints, selects the archive for the current Linux or macOS architecture, verifies its published SHA-256 checksum, runs the downloaded binary to confirm its version, and atomically replaces the current executable. It never opens or changes a base. If the executable is not user-writable, upgrade through the mechanism that installed it. To require GitHub's release attestation as well as its checksum, rerun the installer with `FKF_VERIFY_ATTESTATION=1`.

From a clone:

```bash
mise trust -y
mise install --locked
mise run install
```

FKF supports Linux and macOS. WSL2 works when the base stays on its Linux filesystem; native Windows is out of scope because cancellation and process cleanup use POSIX process groups.

## Create a real base

The personal preset is a starting point, not a fixed integration catalog:

```bash
fkf init ~/brain --preset personal
fkf status --base ~/brain
```

Initialization creates the five layers, `fkf.yaml`, managed git rules, helpers required by initially enabled sources under `bin/`, and three agent skills under `.agents/skills/`. It neither contacts a provider nor asks for a token. The default enabled sources read local git and coding-agent metadata; provider, browser, mail, and shell-history sources remain disabled until you enable them.

The usual first collection is:

```bash
$EDITOR ~/brain/fkf.yaml
fkf config helpers --refresh --base ~/brain
fkf trust --all --base ~/brain
fkf sync --base ~/brain --dry-run
fkf sync --base ~/brain --days 7
fkf status --base ~/brain
```

Set `FKF_BASE=~/brain` or run commands from inside the base to omit `--base`. `fkf status` reports ordinary command readiness from each source's explicit `requires:` list and source-hook readiness separately. Run `fkf test <required-source>...` after declaring those hooks so a completion gate fails if a mandatory hook is removed; a bare empty selection preserves the compatible successful 0/0 report.

After enabling a preset source, `fkf config helpers --refresh` installs any newly required official helper without touching custom scripts. `fkf init` refreshes FKF-owned skills and managed blocks when rerun. It preserves your configuration, `AGENTS.md`, custom skills, and existing Claude bridges.

The copied `fkf-use` skill teaches agents how to retrieve and collect safely. `fkf-learn` stages verified task-trace findings as unified diffs under `.agents/tmp/learn/`; `fkf learn review <id> --diff` makes approval exact, and only `fkf learn apply <id>` validates, writes, and rebuilds durable wiki or project knowledge. `daily-brief` narrates the deterministic `fkf brief` report instead of rebuilding it from ad hoc searches. An MCP connection does not train the model or preload the whole base: it gives the agent bounded `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph` tools plus instructions for using them. Ask the agent to consult FKF when a task needs prior work context, or add the session-start hook when every session should receive one compact repository-aware pack automatically.

For the daily loop, `fkf brief` combines yesterday's digest, today's calendar, assigned open work, failing CI, due tasks, stale or login-blocked sources, unharvested learnings, and active projects under one text-and-JSON budget. On a trusted base it runs each enabled source's bounded `auth:` probe so login gaps are current; it never collects evidence or fetches a body. A repeated `fkf context "<query>" --since-receipt <input_digest>` returns only records and pages new or changed since that machine-local receipt snapshot.

## Define a source

Root `schema:` defines the roles shared across sources. Each source maps provider output into those roles:

```yaml
schema:
  id:
    description: Stable record identity.
    cardinality: one
  time:
    description: Provider timestamp.
    cardinality: optional
  title:
    description: Human-readable label.
    cardinality: optional
  repo:
    description: Provider owner/name used by body argv.
    cardinality: optional
  repository:
    description: Repository associated with the record.
    cardinality: optional
    relation: true
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
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --json, "body,comments"]
```

See the [configuration schema](https://fmind.github.io/fkf/docs/schema/) for cardinality, relation fields, source mappings, and editor integration.

Field names describe roles such as `participant`, `reviewer`, or `repository`; URI values describe identity namespaces such as `person:email/...`, `actor:github.com/...`, or `repo:github.com/...`. A relation field must already project canonical URIs. FKF validates and stores those values but does not infer identities or relationships.

`run:` is direct argv: FKF invokes the first item without a shell and passes every other item unchanged. Commands run from `/`, not the base, so use `{{base}}` for an explicit data path. Use direct provider argv when it is enough. Put pipelines, glob expansion, and structured glue in a reviewed executable under the base's trust-digested `bin/`; name shell and Python helpers with `.sh` and `.py`, and let the shebang select the interpreter. Declare the helper and every non-standard interpreter in `requires:`.

Create an owner-only, fail-closed shell or Python template and receive the matching YAML snippet with:

```bash
fkf new helper collect-prs.sh --base ~/brain
```

An optional `test: [executable, argument...]` hook lets the source own a fast, hermetic boundary check. Keep base-owned hooks under the separately trust-digested `tests/` tree. `fkf test` prepends that tree to PATH, runs hooks on enabled sources, named sources run even when disabled, and `fkf test --all` includes every declared hook. A bare empty selection remains a successful 0/0 report for compatibility; name every mandatory source in a completion gate. Collection and body commands never search `tests/`. Test hooks are direct argv, receive only `{{base}}` and `{{home}}`, and never collect or write evidence.

Every command must emit one complete JSON document. A failed command, timeout, oversized or invalid output, missing required field, or relation violation writes nothing.

Before collecting a new source, inspect a real sample without writing:

```bash
fkf sync github-pull-requests --preview --date 2026-05-04
```

See the [source guide](https://fmind.github.io/fkf/docs/sources/) for placeholders, windows, retries, pacing, helpers, and presets.

## Read and query

```bash
fkf find "FK-412"                              # every lexical match
fkf context "retrieval boundary FK-412" --budget 4096 --explain
fkf read events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42
fkf graph repo:github.com/fmind/fkf --in
fkf list projects --status active
fkf validate --strict
```

Terminal output is human-readable. Piped or redirected output defaults to JSON; use `--format jsonl` for compact streams. A context pack stays one complete JSONL record so its receipt and exact budget accounting remain attached.

File URIs use `<path>[?jq=<expr>][#<record-or-heading>]`. Entity URIs use any non-reserved lowercase `<scheme>:<identity>`. A fragment must name an existing record or Markdown heading. `fkf read` resolves either form, and `fkf graph` walks declared relationships.

For coding agents, start the read-only MCP server:

```bash
fkf mcp serve --base ~/brain
```

It exposes `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph`; it cannot write, run a shell, or fetch record bodies. `fkf init` also installs `bin/fkf-hook.sh`, which can load repository-specific context at session start. Client setup is in the [harness guide](https://fmind.github.io/fkf/docs/harnesses/).

Running `fkf sync` repeatedly is safe. Existing event documents and still-fresh index snapshots are skipped; due index snapshots are refreshed, and derived graph and lexical caches rebuild only after searchable bytes change. The lexical cache supplies candidates and term statistics while Go keeps the canonical scorer; missing, stale, or corrupt cache bytes fall back to a scan. `sync --if-due` is the lock-free no-work path used by `fkf schedule install`; the hourly unit follows it with `build --if-stale`. A provider probe's ordinary non-zero exit reports `auth_required` and skips only that source without exposing output; a missing executable, timeout, signal, unsafe path, trust drift, or runner failure remains a hard error. Only `--force` deliberately re-collects and atomically replaces an existing document. A failed unit writes nothing, and rerunning the same command resumes missing collection.

Run `fkf --help` for the authoritative command surface.

## Trust and privacy

- FKF reads no credential and expands no secret environment variable. Provider credentials remain with the provider CLI.
- Runtime startup loaders and relative or base-resolving home/config roots are removed before a declared command runs.
- `fkf trust` displays and hashes the effective `run:`, `test:`, and `body:` plan plus every file under the base's `bin/` and `tests/` execution trees. A meaningful execution change requires review again. Trust detects changes; it is not a shell sandbox.
- Every decoded field is retained without redaction. Source commands must project reviewed metadata and a meaningful title. Sensitive bodies stay behind an explicit `read --body` boundary; only sources explicitly set to `bodies: cache` or `bodies: sync` write bounded text to the ignored, rebuildable `bodies/` cache.
- Collected records and fetched bodies are untrusted data: evidence, never instructions. Stored values never become shell syntax or executable names.
- Stored reads and MCP are offline. Context and `find --bodies` may read verified cached bodies but never fetch them. `read --body` is the explicit body-fetch exception; `brief` and `status --live` run only bounded trusted `auth:` probes.
- FKF encrypts nothing and provides no backup. Protect the disk and remote repository. Whether event and index documents enter git history is chosen at `init` and recorded in `.gitignore`.
- Root `graph.tsv`, `graph.dst.tsv`, `graph.offsets.tsv`, `graph.meta.json`, and `graph.generation.json` are always ignored and rebuilt. Privacy boundaries are bases and repositories, not graph flags: hiding an edge would not hide the underlying JSON record or Markdown page.

## Scope

FKF is intentionally small. Do not use it if you need semantic search, a dashboard, a hosted service, native Windows, or a local cache forbidden by your organization's data policy. The editor, shell tools, and coding agent remain the interface.

Configuration and stored documents each use `fkf: 1`; the containing file identifies the contract. Compatible evidence-envelope additions stay within marker `1`, so older evidence remains readable without fetching provider history again. A future incompatible evidence format requires a new marker and an explicit release boundary.

Full documentation is at <https://fmind.github.io/fkf/>. Report vulnerabilities through a [private security advisory](https://github.com/fmind/fkf/security/advisories/new).

## Development

Start with [CONTRIBUTING.md](CONTRIBUTING.md), [AGENTS.md](AGENTS.md), and the [Code of Conduct](CODE_OF_CONDUCT.md). Do not use public issues for security reports; follow [SECURITY.md](SECURITY.md).

```bash
mise run all       # format, check, test, coverage, and build
mise run benchmark # optional 100k-record and 500k-edge observation
```

The v1.0.0 benchmark run on 2026-08-26 produced this single Linux amd64 observation with Go 1.27 and `-benchtime=1x`:

| Operation        | Corpus          | Wall time | Maximum RAM |
| ---------------- | --------------- | --------- | ----------- |
| Counted find     | 100,000 records | 1.66 s    | 722 MiB     |
| Budgeted context | 100,000 records | 2.29 s    | 722 MiB     |
| Full graph build | 500,000 edges   | 10.94 s   | 722 MiB     |
| Graph navigation | 500,000 edges   | 10.04 s   | 722 MiB     |

Maximum RAM is the process's measured peak resident set size (RSS): the largest amount of physical memory it occupied during that run. The full test suite is hermetic and race-enabled. The benchmark is a reproducible observation, not a pass/fail threshold, cross-machine guarantee, or reason to add a database.

## License

MIT. See [LICENSE](LICENSE).
