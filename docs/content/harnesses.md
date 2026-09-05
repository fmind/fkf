---
title: Agent harnesses
weight: 10
description: "Install named FKF bases into supported coding agents with scoped read-only MCP and optional workspace context hooks."
---

`fkf harness` registers one named base without claiming the global `fkf` key. Every adapter uses `fkf-<base-name>`, so independent bases coexist in one user profile and each launch carries an explicit absolute `--base`.

All ten adapters support read-only MCP. Automatic context hooks are available only for Claude, Codex, Gemini, and Kiro, and only when the caller supplies an explicit workspace. The other adapters remain MCP-only because their current passive hooks either do not consume output or cannot be scoped reliably per base.

## Install

Inspect one adapter, then install selected names or all adapters:

```bash
fkf --base /absolute/path/to/brain harness print codex
fkf --base /absolute/path/to/brain harness install claude codex
fkf --base /absolute/path/to/brain harness install --all
```

These commands install MCP only. Add automatic context for one canonical workspace explicitly:

```bash
fkf --base /absolute/path/to/brain harness print codex --workspace /absolute/path/to/work
fkf --base /absolute/path/to/brain harness install claude codex gemini kiro --workspace /absolute/path/to/work
```

Preview and check use the same selection:

```bash
fkf --base /absolute/path/to/brain harness install --all --dry-run
fkf --base /absolute/path/to/brain harness install --all --check
fkf --base /absolute/path/to/brain status --live
```

The installer preserves unrelated entries, preflights every target before writing, writes atomically, and saves the immediately previous file as `<path>.fkf.bak`. Reinstalling the same base and workspace is byte-idempotent. It refuses a scoped key owned by another physical base, an unmanaged command, and overlapping automatic-hook workspaces. A same-named second base must be renamed explicitly in `fkf.yaml`; FKF does not invent suffixes.

An MCP-only reinstall preserves existing workspace hooks. Changing a hook's workspace checks every other base, including Kiro's separate hook files; base names that share a prefix remain independent.

`status --live` reports old singleton `fkf` registrations as manual cleanup candidates. Installation does not delete them.

## Supported harnesses

| Name          | MCP configuration                                       | Automatic context with `--workspace` |
| ------------- | ------------------------------------------------------- | ------------------------------------ |
| `claude`      | `~/.claude.json`                                        | `SessionStart`                       |
| `codex`       | `~/.codex/config.toml`                                  | `SessionStart`                       |
| `gemini`      | `~/.gemini/settings.json`                               | `SessionStart`                       |
| `copilot`     | `~/.copilot/mcp-config.json`                            | MCP-only; lifecycle output ignored   |
| `antigravity` | `~/.gemini/config/mcp_config.json`                      | MCP-only; passive output ignored     |
| `opencode`    | `~/.config/opencode/opencode.json`                      | MCP-only; no stable passive seam     |
| `grok`        | `~/.grok/config.toml`                                   | MCP-only; passive output ignored     |
| `cursor`      | `~/.cursor/mcp.json`                                    | MCP-only; no per-base user hook      |
| `kiro`        | `~/.kiro/settings/mcp.json`, `~/.kiro/hooks/<key>.json` | `SessionStart`                       |
| `cline`       | `~/.cline/data/settings/cline_mcp_settings.json`        | MCP-only; one global hook filename   |

No adapter creates a user-scope link to one base's embedded skills. The three skills remain under `<base>/.agents/skills/`. If a harness needs shared discovery, install a neutral FKF skill separately and require it to select the base by name; it must not infer a base from the skill's own path.

Provider account selection belongs to the process that launches collection. For example, a team collector may export `GH_CONFIG_DIR=~/.config/gh-team` before `fkf sync`; ACLI keeps its selected Jira site in its own machine-local configuration. Neither value belongs in `fkf.yaml`, an MCP registration, or a workspace hook.

## Read-only MCP boundary

A base named `brain` registers this shape under `fkf-brain`:

```json
{
  "command": "/absolute/path/to/fkf",
  "args": ["mcp", "serve", "--base", "/absolute/path/to/brain"]
}
```

The server title, instructions, result metadata, resources, and delivery receipts name the selected base. Stored item URIs remain relative JSON values, while model-facing text qualifies citations as `fkf://brain/<relative-uri>`. The server exposes no sync, body fetch, shell, mutation, or Git audit.

## Context-hook boundary

The managed hook command pins the FKF executable, physical base, and physical workspace. It checks execution trust before dispatching `<base>/bin/fkf-hook.sh`. The hook accepts only the host event's cwd or workspace field, resolves it physically, and emits nothing unless it is the configured workspace or a descendant. Missing or malformed input, sibling-prefix paths, and symlink escapes produce the host's empty envelope.

On startup it reads yesterday with 600 tokens and repository context with 850 tokens. Claude compact starts skip yesterday and use a 600-token repository reminder. The repository query is the exact `repo:github.com/owner/name` identity projected from the GitHub origin. Branch names do not become retrieval terms; without a valid repository identity, the hook omits repository context. Every FKF call includes `--base`; the hook never collects, fetches a body, or uses ambient cwd as session identity.

Workspace scope prevents accidental context injection into another checkout. It is not an execution sandbox. Overlapping scopes are rejected because the host cannot reliably distinguish which base should inject context.

Repository tests exercise exact envelopes, the explicit workspace boundary, physical path escapes, closed `PATH`, pinned executable, repository-name projection, coexistence, conflict handling, rollback, and idempotence. They do not launch a harness; after an upgrade, inspect the harness's own MCP and hook diagnostics.

## Collected local metadata

When enabled, the bundled `agent-sessions.sh` and `agent-memory-files.sh` collectors read metadata from supported local stores and skip absent products. They never collect prompts or responses.

| Harness         | Session metadata store                            | Memory metadata store                  |
| --------------- | ------------------------------------------------- | -------------------------------------- |
| Claude Code     | `~/.claude/projects/<cwd>/*.jsonl`                | `~/.claude/projects/<cwd>/memory/*.md` |
| Codex           | `~/.codex/sessions/**/rollout-*.jsonl`            | `~/.codex/memories/**/*.md`            |
| Gemini CLI      | `~/.gemini/tmp/<project>/chats/session-*.json[l]` | `~/.gemini/tmp/<project>/memory/*.md`  |
| OpenCode        | `~/.local/share/opencode/opencode.db`             | none                                   |
| Copilot CLI     | `~/.copilot/session-store.db`                     | remote, not collected                  |
| Antigravity CLI | `~/.gemini/antigravity-cli/history.jsonl`         | none                                   |

A session record contains its id, first activity time inside the collected day, harness, working directory, branch, canonical repository identifier when available, and harness-authored title. A file timestamp is never substituted for missing activity evidence.

The separate `agent-session-traces` source reads only `~/.agents/sessions/v1`, the normalized append-only store shared across harnesses. For each newest complete session generation in the requested window it projects bounded user requests, changed paths from `git status`, verification-looking lines from the last assistant message, harness, and model into ordinary JSON event records. It makes no model call, reads no changed file content, and refuses links in the store. Collection never creates or overwrites `tasks/` pages. The personal preset enables this source; the team preset leaves it disabled because session prose may cross a shared-base privacy boundary.

A nightly learning routine belongs to an owner-scheduled agent, not to FKF. That agent may sync, inspect authored task traces, JSON session evidence, and cached memory bodies, and stage `.agents/tmp/learn/*.diff`; it must stop at `fkf learn review <id> --diff` until the owner approves or rejects the exact diff.
