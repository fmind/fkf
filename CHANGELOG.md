# Changelog

All notable changes to `fkf` are documented here. This project follows [Semantic Versioning](https://semver.org/) from its first public release.

## [v3.0.1](https://github.com/fmind/fkf/releases/tag/v3.0.1) - 2026-09-03

### Fixed

- Make schedule CLI tests select the native fake scheduler and managed-file layout, restoring the hermetic CI contract on macOS without changing runtime behavior.

## [v3.0.0](https://github.com/fmind/fkf/releases/tag/v3.0.0) - 2026-09-03

### Highlights

- Add deterministic `brief`, `day`, `timeline`, `who`, and `eval` workflows, temporal query grammar, declared identity aliases, and compact text and structured retrieval receipts.
- Add digest-bound lexical and constant-time graph caches while keeping durable evidence authoritative, offline reads reproducible, and indexed and fallback retrieval semantically identical.
- Add login-aware opportunistic sync, hourly systemd and launchd scheduling, and idempotent harness integration for Claude Code, Codex, Gemini CLI, Copilot CLI, Antigravity, OpenCode, Grok, Cursor, Kiro, and Cline.
- Add bounded, ignored, manifest-verified body caching with per-source `none`, `cache`, and `sync` policies; first-class meeting-note and local agent-memory sources can prefetch searchable text without copying it into durable evidence.
- Expand and harden the reviewed personal presets, session traces, staged learning workflow, MCP surface, provider pagination, process isolation, trust revalidation, and graph generation consistency.

### Breaking changes

- Every collected record must now project one meaningful, control-free `title`; update custom source schemas and field mappings before the next sync.
- Structured `find` and `context` results omit raw provider records and internal day selections by default; pass `--raw` only when those diagnostic fields are required.

### Upgrade notes

- Existing evidence remains valid and requires no re-collection. Run `fkf build all --base <base>` to create the new derived graph and lexical caches.
- Refresh FKF-owned helpers and harness integrations, review the resulting execution plan, and renew trust before running changed collectors: `fkf config helpers --refresh`, `fkf harness install --all`, then `fkf trust --all`.
- Body caching stays opt-in per source. The default `bodies: none` fetches only on an explicit `read --body`; `cache` retains an explicitly fetched body and `sync` prefetches it after evidence is written.

## [v2.1.0](https://github.com/fmind/fkf/releases/tag/v2.1.0) - 2026-08-30

### Highlights

- Add a dedicated base `tests/` execution tree for source verification hooks, recursively covered by trust and prepended to `PATH` only for `fkf test`; collection and body commands cannot see test fixtures or shadows.
- Report source-hook readiness separately from ordinary `requires:`, disclose `bin/` and `tests/` as distinct trust items, and carry the new layout through init, permissions, schemas, documentation, and bundled skills.
- Preserve v2 compatibility: bases without `tests/` keep their existing trust digest, hooks can still resolve from `bin/`, and an empty optional selection remains a successful 0/0 report. Completion gates should name mandatory sources.

### Fixed

- Restrict repository metadata projected by bundled session, Git, and agent-hook helpers to GitHub remotes, while continuing to strip credentials and reject malformed paths.
- Open Atuin history read-only in batch mode, omit deleted rows and command text, and declare the Git dependency used by the agent-sessions preset.

### Upgrade notes

- A pre-existing base `tests/` directory is now reserved, recursively trust-covered execution material and must contain no symlinks. Move source hooks and their support files there, keep generic repository tests elsewhere, then review and renew trust.

## [v2.0.1](https://github.com/fmind/fkf/releases/tag/v2.0.1) - 2026-08-29

### Fixed

- Make verified release archives the documented v2 installation path and explain that Go's major-version import rules keep the unchanged module path's `go install ...@latest` resolution on v1.

## [v2.0.0](https://github.com/fmind/fkf/releases/tag/v2.0.0) - 2026-08-29

### Highlights

- Add optional, trust-covered source `test:` argv and `fkf test`, with enabled-source defaults, explicit disabled-source selection, bounded timeouts, stable reports, and provider-stderr privacy.
- Publish `graph.meta.json` schema version 2 with separate SHA-256 inputs for events, index, projects, tasks, wiki, and edge-relevant schema semantics, plus a framed aggregate and exact `graph.tsv` output digest.
- Give every bundled shell helper an explicit `.sh` extension and require `.sh` or `.py` when scaffolding a helper, including the interpreter in the generated readiness contract when needed.

### Breaking changes

- Existing derived graph metadata must be rebuilt with `fkf build graph`; collected evidence remains readable and requires no re-collection.
- Base configurations and harness integrations using bundled extensionless helper names must move to the corresponding `.sh` names before refreshing helpers.

## [v1.1.2](https://github.com/fmind/fkf/releases/tag/v1.1.2) - 2026-08-27

### Highlights

- Flatten the documentation sidebar on desktop and mobile so Overview and every guide are peers, while preserving all published routes and enforcing the rendered navigation contract in tests.

## [v1.1.1](https://github.com/fmind/fkf/releases/tag/v1.1.1) - 2026-08-27

### Highlights

- Make failed collection diagnostics actionable with the source, date or window, safe substituted command, neutral working directory, timeout, and exit class while keeping provider stderr and body-derived arguments private.
- Stage release installation beside the destination before an atomic replacement, preserving an existing binary if staging fails; cover every published Linux and macOS architecture tuple hermetically.
- Add a dedicated configuration-schema guide, present every documented agent harness at the same level, refresh vendor hook contracts, simplify root help and contributor instructions, and keep the Overview first in the documentation tree.
- Scope toolchain drift checks to project pins, refresh the embedded usage skill and supported-version policy, and retain a strict, generated, link-checked documentation contract.

## [v1.1.0](https://github.com/fmind/fkf/releases/tag/v1.1.0) - 2026-08-27

### Highlights

- Add `fkf upgrade`, which selects the current platform archive, verifies its published SHA-256 checksum and reported version, and atomically replaces the running executable.
- Send the documentation root directly to the Overview, expose Overview in the navigation, and remove the intermediate "Read the docs" landing page.
- Explain repeated `fkf sync` safety and how coding agents learn from project and wiki content through the read-only MCP server and embedded skills.

## [v1.0.0](https://github.com/fmind/fkf/releases/tag/v1.0.0) - 2026-08-26

The initial Fmind Knowledge Framework release: one Go binary that collects developer activity into an owned, inspectable base of JSON and Markdown, links it as a graph of relative URIs, and gives coding agents a deterministic context pack under a token budget.

### Highlights

- Collect complete daily events and point-in-time indexes from local tools, GitHub, Google Workspace, and Google Cloud presets, plus arbitrary reviewed provider commands.
- Compose each source from direct argv or a trust-digested helper whose shebang selects its interpreter, with curated preset helpers for provider boundaries that need shared pagination, completeness, or privacy handling.
- Declare one root semantic `schema:` with descriptions, cardinality, examples, and relation roles; sources associate those shared fields with provider paths, and stored documents retain the exact schema subset they used.
- Build an open, transcription-only graph: any non-reserved lowercase entity scheme is valid, relation field names are base-defined, and edges come only from declared fields, authored links, tags, and explicit `relations:` frontmatter.
- Read, find, rank, and graph stored knowledge entirely offline; the read-only MCP server cannot collect, write, shell, or fetch bodies.
- Share one base across Claude Code, Codex, Gemini CLI, OpenCode, Copilot CLI, Antigravity, Cursor, Kiro, Cline, and other harnesses through portable Agent Skills and documented hooks.
- Keep execution explicit with a canonical-plan trust digest covering enabled commands, body-bound paths, helper scripts, executable bits, executable search directories, retry, pacing, and collection policy without re-arming on YAML presentation, inherited environment, or retrieval-only metadata.
- Use one explicit `fkf: 1` marker value for strict base configuration and the separate additive evidence envelope.
- Store plain JSON and Markdown in five typed layers, with rebuildable `graph.tsv` and `graph.meta.json` at the base root, strict schemas, bounded and atomic I/O, path confinement, owner-only permissions, and untrusted-content framing.
- Reproduce lexical retrieval under a hard whole-pack budget with a selection receipt that records scores, reasons, counted exclusions, rejected pins, evaluation day, ranking version, and semantic-input digest.
- Ship synthetic demos, personal and team presets, a strict Hugo documentation site, hermetic race-tested Go suites, security scans, reproducible archives, checksums, and build-provenance attestations.

### Supported platforms

Release archives are provided for Linux and macOS on amd64 and arm64. Native Windows is intentionally out of scope; WSL2 uses the Linux archive.
