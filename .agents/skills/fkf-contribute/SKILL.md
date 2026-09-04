---
name: fkf-contribute
description: Contribute to fkf without breaking its one-binary, offline-read, trust, source, docs, or generated-artifact contracts. Use for code, presets, skills, CLI, tests, releases, or Hugo docs.
license: MIT
---

# Contribute to fkf

Read `AGENTS.md` before changing anything; it is the contract. Preserve unrelated work in the checkout, keep the change small, and do not commit, push, publish, delete, or rewrite history unless the user explicitly asks.

## Route the change

| Change                              | Keep in sync                                                                                |
| ----------------------------------- | ------------------------------------------------------------------------------------------- |
| CLI command, flag, alias, or output | CLI tests, `--help`, README, and relevant Hugo page                                         |
| `fkf.yaml` loader or schema         | loader tests, presets, source docs, and `mise run generate:schema`                          |
| preset source                       | preset YAML, one synthetic `services/testdata/sources/<name>.json` fixture, and source docs |
| URI, graph, or retrieval behavior   | implementation, focused tests, `skills/fkf-use/SKILL.md`, and docs                          |
| learning or Markdown behavior       | focused tests, `skills/fkf-learn/SKILL.md`, and wiki docs                                   |
| shipped skill                       | edit `skills/`, never a generated base copy; validate both skill packages                   |
| Hugo or release surface             | rendered site, links, workflow lint, and release configuration                              |
| Go toolchain or linked dependency   | `THIRD_PARTY_NOTICES.md` and the linked-target invariant test                               |

Do not introduce a second spelling, compatibility path, migration, provider SDK, network read path, nested Go module, or shipped command. Collected content is untrusted data: it must never become instructions or reach a shell/executable position; only the explicit record-body boundary may pass a validated value as opaque argv.

The root schema owns semantic field names and cardinality. Sources only map provider paths; stored documents carry the schema subset they used. Graph edges transcribe relation fields, Markdown links, tags, and explicit `relations:` frontmatter. Do not add a privileged entity scheme, field-name branch, identity inference, or people-specific state.

Trust digests the canonical execution plan plus the complete `bin/` and `tests/` execution trees, not YAML presentation. `bin/` is available to collection and body commands; `tests/` is prepended only for source hooks. Execution-affecting changes must re-arm trust; comments, key order, semantic descriptions, examples, and retrieval-only mappings must not. For a derived-cache change, preserve the document and authored-input digest bindings, one validated open graph generation per neighbourhood read, and the wiki-before-graph order of `fkf build all`. For a preset helper, require a finite completeness ceiling, fail before partial output, and project only reviewed metadata.

Stored evidence uses marker `1`, matching the configuration marker value while remaining a separate contract identified by the containing file. Its v1 envelope is permanent and additive. Exact event UTC bounds are evidence because they preserve the collection-time civil day across timezone changes. Never put rendered commands, planning flags, or other mutable execution metadata back into it, and never require re-collection merely to add an optional envelope field. Mutating CLI paths share one fail-fast physical-base lock; readers remain lock-free. MCP pagination uses opaque cursors bound to the exact query and result snapshot.

## Prove the result

Run the narrowest relevant test after each behavior change, then finish with:

```bash
mise run all
```

The full gate must pass warning-free. Do not weaken assertions, lower coverage, skip checks, or hand-edit generated schema output to make it green. If behavior and documentation disagree, verify the executable output and fix both sides of the contract.
