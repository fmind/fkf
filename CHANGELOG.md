# Changelog

All notable changes to `fkf` are documented here. This project follows [Semantic Versioning](https://semver.org/) from its first public release.

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
