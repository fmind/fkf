# AGENTS.md (Project)

This repository builds **`fkf`**: one Go module, one binary, and three embedded skills. A base is a git repository of JSON and Markdown that FKF collects, links as URIs, and retrieves for coding agents. `README.md` explains the product; this file defines the contributor contract.

## Status

The design is implemented. Do not add a compatibility path, migration, or second spelling. Configuration and collected evidence each use `fkf: 1`; the containing file identifies the contract. The evidence envelope is permanent and additive because providers may no longer retain old records. A change that requires forced re-collection is the wrong design.

## Workflow

1. Use `.agents/skills/fkf-contribute/SKILL.md` as the route map. This file is authoritative if they disagree.
1. Run focused tests after behavior changes, then `mise run all`. Never weaken, skip, or hide a failing check.
1. Do not commit, push, publish, rename, delete outside the working tree, or take an irreversible action unless explicitly asked.
1. Keep tests hermetic: use a temporary `HOME`, unset `FKF_BASE`, use synthetic fixtures, and replace every provider-backed declared command through the `Runner` seam or a fake executable on a temporary `PATH`. No test makes a network or provider call. Local POSIX tools such as `sh`, `bash`, `python3`, `jq`, `sqlite3`, `git`, and `go list` may run for real only where they are the subject under test: the execution boundary in `core/shell` and `sources`, the shipped helpers and installer exercised by `internal/checks` and `install_test.go`, the synthetic relative-base helper regression, and repository invariant or status-policy checks.

## Product boundaries

- **One module and binary.** The root module is `github.com/fmind/fkf`; do not add `go.work`, a nested module, plugin, provider SDK, or second shipped command. `docs/go.mod` only pins the Hugo theme.
- **POSIX execution.** FKF supports Linux and macOS; `run:`, `test:`, and `body:` are direct argv and a helper's shebang selects its interpreter. Cancellation uses POSIX process groups. WSL2 is Linux; native Windows is out of scope.
- **One base boundary.** `<base>/fkf.yaml` plus ignored `fkf.local.yaml` are the only configuration. There is no global config, profile, bundle, visibility, sensitivity, capture, or policy layer.
- **Plain durable data.** Bases contain JSON, Markdown, and the generated graph TSV. Never store rendered commands, planner state, or mutable execution metadata in evidence.
- **Published paths only.** Resolve paths through `Store.Resolve`. It admits enabled layers, graph artifacts, `fkf.yaml`, and `AGENTS.md`; it rejects every other root file and any symlink below the base.
- **Small, typed Go.** Keep cancellation explicit, split natural seams rather than line counts, and prefer a pure helper to a framework.

## Trust and execution

- **Stored reads are offline.** `context`, `find`, `graph`, MCP, and `read` without `--body` execute no command, and no package imports `net/http`. Context and `find --bodies` may read manifest-verified text from the ignored body cache but never fetch a miss. `brief` and explicit `status --live` may run only trusted `auth:` probes; neither collects evidence. In-process gojq has no environment, input, or import access; the final JSON result is bounded. `halt` may succeed; `halt_error` fails.
- **Collected content is untrusted data.** Never turn a stored value into instructions or a shell or executable position. The only exception is trust-gated `read --body`, which may pass one charset-checked, non-option field value as one opaque argv item to the source's current `body:` command.
- **FKF reads no credential.** Credentials and provider-profile selectors belong to the named provider CLI and the process that launches FKF; configuration declares no environment values. Remove interpreter and dynamic-loader startup variables from children, drop relative or base-resolving home/config roots, sanitize `PATH`, and never rely on FKF to redact provider output.
- **Commands stay explicit.** Prefer direct provider argv, then a `.sh` or `.py` collection/body helper under `<base>/bin/`; put base-owned source verification hooks under `<base>/tests/`. Declared commands run from `/`; use `{{base}}` for data paths, and declare bare executable names and non-standard interpreters in `requires:`.
- **Trust covers execution.** Hash the canonical plan from both config files plus every entry and executable bit under `<base>/bin/` and `<base>/tests/`; refuse symlinks. Execution-affecting changes re-arm trust. Never source authored or collected layers. Trust is disclosure and change detection, not a sandbox.
- **PATH stays outside the base.** Extra `bin:` directories must be absolute or `~`-relative, machine-local, and outside the base. Remove empty, relative, and base-resolving inherited entries.

## Sources and writes

- Root `schema:` is the base-owned semantic dictionary. Fields declare a description, `one`/`optional`/`many` cardinality, and optionally `relation: true`. Sources map those shared names to provider paths; only `id` and event `time` are structural. Field names express roles, while URI schemes express identity namespaces.
- Placeholders use exact lowercase `{{name}}` syntax. FKF supplies dates and paths as individual argv values; no shell reparses them. Pipelines and expansion belong in a helper whose shebang declares the interpreter.
- Stored documents keep every decoded value, their schema subset, and field map. A `body:` command may use only a max-one field already present in that stored map.
- Every new record projects a meaningful, control-free title. `bodies: none` stores no body, `cache` stores only after explicit `read --body`, and `sync` prefetches after the evidence write. The ignored body cache is bounded, manifest-verified, rebuildable, and never evidence.
- Collection is all-or-nothing and atomic. Non-zero exit, timeout, excessive output, invalid or multiple JSON documents, or missing required values fails the day. `sync --preview` runs and validates one source once, returns at most three projected samples, and writes nothing.
- A source may declare one `test:` argv. `fkf test` runs enabled hooks by default, named hooks regardless of enabled state, and disabled hooks with `--all`; an empty selection preserves the compatible successful 0/0 report, so completion gates should name mandatory sources. Hooks receive only `{{base}}` and `{{home}}`, write nothing, and resolve `<base>/tests/` before the ordinary command PATH. Reserve that whole trust-digested tree for source hooks, fixtures, and support code; generic repository checks belong elsewhere because every edit re-arms execution trust. Collection and body commands never search `tests/`.
- A `window: true` source runs once per contiguous missing span. Preset helpers use finite pagination and fail before emitting partial output. Event documents retain their collection-time UTC bounds; `status` revalidates current structural rules.
- Every mutating CLI path takes one fail-fast lock per physical base, including symlink aliases. Readers, dry runs, previews, `trust --check`, and `build [target] --check` remain lock-free.

## Derived content and retrieval

- **Generated Markdown stays inside marked blocks.** `build wiki` owns one block in `wiki/index.md` through `services/marked_block.go`. `build` and `build all` update the wiki block before the graph and lexical index because the exact page bytes are input to both caches.
- **Files own links; the graph is a cache.** Record edges come from stored `relation: true` fields. Markdown edges come from links, tags, and declared relation fields under `relations:`. Do not infer edges from titles, prose, arbitrary frontmatter, privileged entity kinds, or identity matching.
- **Graph reads bind one generation.** Metadata records URI, size, mtime, and SHA-256 per input and artifact, plus logical component digests. Ordinary reads stat all inputs and hash changed fingerprints; `graph --verify` hashes all. Walks hold source, destination, and offset descriptors and seek exact ranges.
- **The lexical index is only a cache.** `index/.fkf-index.*` is ignored, digest-bound to searchable evidence and ranking semantics, and rebuilt after searchable writes change. It supplies conservative candidates and term statistics only; Go still scores durable evidence. Missing, stale, or corrupt cache bytes make `context` and lexical `find` scan and name the fallback in their receipt.
- **Retrieval is lexical and reproducible.** No embeddings or model belongs in the read path. Indexed and fallback paths return the same semantic answer; the execution-path diagnostic is the only intentional difference. The public indented JSON, including its receipt, must fit the exact four-bytes-per-token budget; a smaller request returns a self-consistent minimum, and rejected explicit pins remain named.
- **MCP is read-only and bounded.** It requires `--base`, omits `--body` and the CLI-only Git tracking audit, and pages at 100 items with opaque cursors bound to arguments and an exact snapshot. Text and structured results carry the same compact JSON and stay within 4 MiB. The wiki-index resource keeps its curated body. Client errors expose only base-relative paths and anonymize home and FKF state paths.

## Code map

| Path         | Responsibility                                                                             |
| ------------ | ------------------------------------------------------------------------------------------ |
| `cmd/fkf/`   | urfave/cli commands, output envelopes, text renderers, and exit codes 0, 1, 2, 3, and 130. |
| `core/`      | Config/schema, store confinement, bounded I/O, atomic writes, processes, and trust.        |
| `sources/`   | Placeholders, execution, decoding, field projection, documents, bodies, retry, and pacing. |
| `services/`  | Sync, retrieval, graph, Markdown, status, build, init, and synthetic preset fixtures.      |
| `mcpserver/` | Read-only tools, resources, cursors, and bounded responses.                                |
| `skills/`    | Embedded `fkf-use`, `fkf-learn`, and `daily-brief`; edit these sources, not base copies.   |
| `presets/`   | Source YAML and small reviewed helpers.                                                    |
| `docs/`      | Hugo site and generated `static/fkf.schema.json`.                                          |
| `internal/`  | Test-only package: repository invariants, notices, and shipped-preset helper checks.       |

## Verification

`mise run all` runs format, static checks, hermetic race/coverage tests, the coverage ratchet, and the build. Lefthook and CI use the same tasks. Use `mise run generate:schema` after loader changes; do not hand-edit generated schema. A Go toolchain or linked-dependency change also updates `THIRD_PARTY_NOTICES.md`.

Tests own the exact CLI vocabulary, URI grammar, generated artifacts, schema parity, preset fixtures, trust and storage boundaries, complete third-party notices, and closed-vocabulary retrieval scoring. `mise run benchmark` is an opt-in observation, not a threshold. There is no source-of-truth database; derived caches must be rebuildable by `fkf build`, digest-bound to their inputs, and git-ignored.
