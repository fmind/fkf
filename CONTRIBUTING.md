# Contributing to fkf

Thanks for helping make `fkf` a small, dependable knowledge layer for developers and coding agents. Focused fixes, source presets, tests, and documentation improvements are welcome.

## Before you start

- Read [AGENTS.md](AGENTS.md). It defines the design invariants and the required workflow for human and agent contributors.
- Search existing issues before proposing a substantial feature. `fkf` keeps its v1 contracts narrow; an incompatible change needs an explicit new contract rather than an implicit migration path.
- Never include credentials, personal data, customer names, or records from a real base in an issue, fixture, test, or commit. Use synthetic data only.

## Set up the checkout

Install [mise](https://mise.jdx.dev/), then use the repository-pinned toolchain:

```bash
mise trust -y
mise install --locked
mise run install
```

Run `mise run docs:watch` for the documentation site. The canonical task vocabulary is `install`, `format`, `check`, `test`, `build`, and `all`; local hooks and CI delegate to those same tasks.

`mise run benchmark` is opt-in. It observes find, context, graph build, and graph navigation over a reproducible 100,000-record/500,000-edge corpus; it is not a pass/fail gate or a database requirement.

## Make a focused change

Keep the affected contracts together:

- A CLI change includes focused tests, help output, and the matching README or Hugo documentation.
- A loader change includes `mise run generate:schema`; never edit the published schema by hand.
- A Go toolchain or linked-dependency change updates `THIRD_PARTY_NOTICES.md`; the invariant test rejects missing runtime and module entries.
- A preset source includes one small, synthetic fixture under `services/testdata/sources/` that matches the provider CLI's real JSON shape and exercises its open `fields:` map. Keep a short command inline; put longer glue in a shell helper under the base `bin/` template.
- A URI, retrieval, or learning change keeps `skills/fkf-use/SKILL.md` or `skills/fkf-learn/SKILL.md` aligned with the executable behavior.

Tests must be hermetic: temporary homes, no real base discovery, no provider call, and every provider-backed declared command replaced through the runner seam. The relative-base CLI regression may execute only its deterministic helper created inside the temporary base; AGENTS.md records the other narrow local-tool exceptions.

## Verify it

Run a focused test while developing, then the complete gate before asking for review:

```bash
mise run all
```

Do not skip or weaken a check, suppress a warning, or lower the coverage floor. For documentation-only changes, still run the repository gate; its strict Hugo build, rendered link check, workflow lint, and security checks are part of the product contract.

## Open the pull request

Explain what changed, why it is the smallest complete fix, and the exact commands that passed. Link the issue when one exists. Keep unrelated cleanup out of the patch and use a [Conventional Commits](https://www.conventionalcommits.org/) title such as `fix:`, `feat:`, `docs:`, or `chore:`.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security reports belong in the private channel described by [SECURITY.md](SECURITY.md), never in a public issue.
