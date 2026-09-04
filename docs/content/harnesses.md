---
title: Agent harnesses
weight: 10
description: "Install one FKF base into supported coding agents with read-only MCP, a small context hook, and the embedded skills."
---

`fkf harness` keeps the integration for one base explicit and reproducible. It configures three surfaces:

- **Read-only MCP** exposes bounded `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph` tools over stdio.
- **Session context** asks FKF for one small repository-and-branch-aware pack.
- **Skills** bridge the base's `fkf-use`, `fkf-learn`, and `daily-brief` skills into the harness's user skill directory.

Installation runs no source command. MCP and the hook read already collected evidence offline. Every managed MCP and hook entry pins both the current FKF executable and the base by absolute path, so an older binary earlier on `PATH` cannot serve an incompatible base. Before a managed hook dispatches base-owned code, that pinned binary verifies the base's current execution trust; drift produces only the harness's empty envelope.

## Install

List the closed supported vocabulary, inspect one harness's exact fragments, then install either selected names or all of them:

```bash
fkf harness list
fkf --base /absolute/path/to/brain harness print codex
fkf --base /absolute/path/to/brain harness install claude codex opencode
fkf --base /absolute/path/to/brain harness install --all
```

`print` is intended for dotfile templates. It emits the same managed fragments and link targets that `install` applies, including the current executable path; regenerate the fragment if the binary moves. It never reads or writes the user configuration.

Preview or verify without writing:

```bash
fkf --base /absolute/path/to/brain harness install --all --dry-run
fkf --base /absolute/path/to/brain harness install --all --check
```

`--check` exits 1 when any selected entry or skills link is missing or points at another base. A current configuration exits 0.

The installer:

- preserves fields, hook entries, and TOML outside the FKF entry;
- pins the binary that performed the install instead of resolving `fkf` through a client-specific `PATH`;
- preflights every selected harness before the first write;
- refuses an existing `fkf` MCP entry or hook that FKF cannot identify as managed;
- writes atomically and saves the immediately previous file as `<path>.fkf.bak`;
- is idempotent: a second current install reports no changes;
- refuses regular files or unmanaged symlinks at a skills-bridge target.

It does not invoke a harness CLI, start an MCP server, or touch a live session. Restart the harness after installation and use its MCP or hooks view to confirm that it loaded the user configuration.

## Supported harnesses

| Name          | Managed user configuration                                              | Context event or seam                  | Hook output                        |
| ------------- | ----------------------------------------------------------------------- | -------------------------------------- | ---------------------------------- |
| `claude`      | `~/.claude.json`, `~/.claude/settings.json`, `~/.claude/skills/`        | `SessionStart`, `startup\|compact`     | plain context                      |
| `codex`       | `~/.codex/config.toml`, `~/.agents/skills/`                             | `SessionStart`, `startup\|compact`     | `hookSpecificOutput` JSON          |
| `gemini`      | `~/.gemini/settings.json`, `~/.gemini/skills/`                          | `SessionStart`, `startup\|compact`     | `hookSpecificOutput` JSON          |
| `copilot`     | `~/.copilot/mcp-config.json`, `~/.copilot/hooks/`, `~/.copilot/skills/` | `sessionStart`                         | ignored; see limitation below      |
| `antigravity` | `~/.gemini/config/{mcp_config,hooks}.json`, Antigravity CLI skills      | `PreInvocation`, invocation zero       | `injectSteps` JSON                 |
| `opencode`    | `~/.config/opencode/{opencode.json,plugins/}`, `~/.agents/skills/`      | first system transform in each session | plain context consumed by plugin   |
| `grok`        | `~/.grok/{config.toml,hooks/,skills/}`                                  | `SessionStart`, `startup\|compact`     | plain output; see limitation below |
| `cursor`      | `~/.cursor/{mcp.json,hooks.json,skills/}`                               | `sessionStart`                         | `additional_context` JSON          |
| `kiro`        | `~/.kiro/{settings/mcp.json,hooks/,skills/}`                            | `SessionStart`                         | plain context                      |
| `cline`       | `~/.cline/{data/settings,skills/,hooks/}`                               | `TaskStart`                            | `contextModification` JSON         |

On `startup`, Claude receives yesterday's digest with a 600-token budget plus a repository pack with an 850-token budget. A `compact` start skips yesterday and receives only a 600-token repository reminder because the harness already carries its own conversation summary.

Antigravity has no session-start hook. Its documented `PreInvocation` integration runs before model calls, so the FKF adapter emits context only when `invocationNum` is zero. OpenCode similarly has no session-start context-output event: the managed local plugin calls the same adapter once per session from its documented system-transform hook.

Copilot CLI runs the personal `sessionStart` hook, but that event ignores command output. FKF therefore installs it as a visible lifecycle integration without claiming context injection; MCP and the three skills are the supported retrieval paths.

### Grok limitation

Grok 1.0.5 discovers and runs the installed `SessionStart` hook, but its installed hooks guide says passive-hook stdout is ignored. The adapter emits a tested plain pack, but that Grok release does not add it to the model context. The installed MCP server remains the supported retrieval path. This is a harness limitation, not evidence that automatic context injection was verified.

## Read-only MCP boundary

Every harness ultimately launches the same stdio server:

```json
{
  "command": "/absolute/path/to/fkf",
  "args": ["mcp", "serve", "--base", "/absolute/path/to/brain"]
}
```

The launch line is the disclosure boundary: it says exactly which base that client may read. The server exposes no `sync`, body fetch, shell, mutation, or Git audit. MCP does not train the model or copy the whole base into its prompt; the agent calls bounded retrieval tools when needed.

## Context-hook boundary

`fkf init` owns `bin/fkf-hook.sh`. The installer writes a fail-open guard that asks the pinned binary to run `trust --check` before pointing the harness at that absolute script and passing the binary as its second argument. A symlinked hook is refused during installation. The script's parent directory determines the base. The hook reads the harness event on stdin, derives a strict GitHub `owner/name` plus branch from the working repository, then combines these bounded offline reads on startup:

```bash
/absolute/path/to/fkf day yesterday --base <hook-parent> --budget 600 --format text
/absolute/path/to/fkf context --base <hook-parent> --budget 850 --format text -- "<owner/name> <branch>"
```

Compact starts omit the day read and use a 600-token repository budget.

The hook is deliberately fail-open:

- It dispatches no base-owned executable when execution trust is missing or stale.
- It ignores the inherited `PATH` and searches only conventional user and OS-managed locations.
- It prints the harness's empty envelope and exits successfully when no repository, pinned FKF binary, or usable pack is available.
- It never runs source collection or body commands.
- Its result is evidence, not instructions.

Repository tests pin every supported output envelope, the empty responses, Claude's compact budget, the Antigravity first-call gate, the repository-name parser, the closed `PATH`, and unknown-harness failure. These tests prove FKF's adapter bytes. They do not prove that a newly released harness still consumes those bytes; use the harness's own MCP and hook diagnostics after an upgrade.

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

The separate `agent-session-traces` source reads only `~/.agents/sessions/v1`, the normalized append-only store shared across harnesses. For each newest complete session generation in the requested window it imports bounded user requests, changed paths from `git status`, verification-looking lines from the last assistant message, harness, and model into one task skeleton. It makes no model call, reads no changed file content, refuses links in the store, and never overwrites an existing trace. The personal preset enables it; the team preset leaves it disabled because session prose may cross a shared-base privacy boundary.

A nightly learning routine belongs to an owner-scheduled agent, not to FKF. That agent may sync, inspect the day's task traces and cached memory bodies, and stage `.agents/tmp/learn/*.diff`; it must stop at `fkf learn review <id> --diff` until the owner approves or rejects the exact diff.
