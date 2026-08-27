---
title: Agent harnesses
weight: 9
description: "Connect one FKF base to an agent with read-only MCP or a small, tested context hook."
---

There are two useful integrations:

- **MCP on demand** gives an agent bounded `context`, `find`, `list`, `read`, and `graph` tools over stdio.
- **A context hook** asks FKF for a small repository-aware pack at session start.

Start with MCP. Add a hook only when every session should receive context before the agent chooses a tool. Both surfaces are read-only and offline; the source commands in `fkf.yaml` never run.

MCP does not train the model or copy the whole base into its prompt. It gives the connected agent five bounded retrieval tools and concise server instructions; the agent decides when to call them. Ask the coding agent to consult FKF for prior decisions, project state, or supporting evidence when you want retrieval to be explicit. Add the hook only when one compact repository-and-branch-aware context pack should arrive automatically at session start.

## Proof boundary

The repository hermetically tests every `bin/fkf-hook` output envelope, empty response, first-call gate, repository-name parser, closed `PATH`, and unknown-harness failure. Those tests prove FKF's adapter bytes. They do not prove that a particular installed or hosted harness still consumes the bytes, so the setup recipes below link to the vendor contract they depend on.

The adapter table is the complete supported hook vocabulary:

| Argument      | Output envelope            | Intended event                                |
| ------------- | -------------------------- | --------------------------------------------- |
| `claude`      | plain text                 | Claude Code `SessionStart`                    |
| `codex`       | plain text                 | Codex `SessionStart`                          |
| `kiro`        | plain text                 | Kiro `SessionStart`                           |
| `copilot`     | `additionalContext` JSON   | Copilot CLI `sessionStart`                    |
| `gemini`      | `hookSpecificOutput` JSON  | Gemini CLI `SessionStart`                     |
| `cursor`      | `additional_context` JSON  | Cursor `sessionStart`                         |
| `antigravity` | `injectSteps` JSON         | Antigravity `PreInvocation`, first call only  |
| `cline`       | `contextModification` JSON | Cline `TaskStart`                             |
| `devin`       | `hookSpecificOutput` JSON  | Provisional Devin Local compatibility adapter |

`devin` has no stable public hook contract. Its peer section below is intentionally a warning rather than a setup recipe; retest it against the installed product before use.

## Read-only MCP

Every stdio client needs the same command and arguments:

```json
{
  "command": "fkf",
  "args": ["mcp", "serve", "--base", "/absolute/path/to/brain"]
}
```

Use an absolute base path unless the client explicitly documents shell or home expansion. The launch line is the disclosure boundary: it says exactly which base that client may read. The server exposes no `sync`, body fetch, shell, mutation, or Git audit.

Common registration commands:

```bash
codex mcp add fkf -- fkf mcp serve --base /absolute/path/to/brain
claude mcp add --transport stdio --scope user fkf -- fkf mcp serve --base /absolute/path/to/brain
```

Codex's stdio form and shared MCP configuration are documented in the [official OpenAI MCP guide](https://developers.openai.com/codex/extend/mcp). For other clients, translate the same command into that client's local-MCP configuration; the FKF side does not change.

## The tested context hook

`fkf init` writes `bin/fkf-hook` for optional session integration. The hook resolves its base from its own location, reads the current repository's strict `owner/name` and branch, and runs:

```bash
fkf context --base <hook-parent> --budget 1500 --format text -- "<owner/name> <branch>"
```

Three properties are deliberate:

- The hook ignores the caller's `PATH` and searches only conventional user and OS-managed locations. A repository-local executable cannot impersonate `git`, `jq`, or `fkf`.
- It prints the target harness's empty envelope and exits successfully when no repository, FKF binary, or usable base is available. Context loading must not block the agent.
- Its output is evidence, not instructions. Every context pack carries that notice even when the harness has not loaded the embedded skill or MCP instructions.

Keep the default 1,500-token budget for session start. Re-running the hook on every prompt repeatedly spends that context budget.

## Documented setup recipes

The following recipes are backed by current public vendor documentation and FKF's adapter tests. After a product update, re-open the linked contract before assuming its paths or event schema are unchanged.

### Claude Code

Claude Code documents `SessionStart`, its `startup|resume|clear|compact|fork` sources, and plain stdout as added context in the [hooks reference](https://code.claude.com/docs/en/hooks).

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact|fork",
        "hooks": [
          {
            "type": "command",
            "command": "/absolute/path/to/brain/bin/fkf-hook claude",
            "timeout": 20,
            "statusMessage": "Loading FKF context"
          }
        ]
      }
    ]
  }
}
```

Put this in the documented user or project settings file. `fkf init` also creates `CLAUDE.md` and `.claude/skills` bridges when those paths are absent; it never overwrites existing harness-specific files.

### Codex

Codex documents inline hook tables, `SessionStart`, plain stdout as developer context, and hash-based review in the [official OpenAI hooks guide](https://developers.openai.com/codex/hooks).

```toml
[[hooks.SessionStart]]
matcher = "startup|resume|clear|compact"

[[hooks.SessionStart.hooks]]
type = "command"
command = "/absolute/path/to/brain/bin/fkf-hook codex"
timeout = 20
statusMessage = "Loading FKF context"
```

Add it to `~/.codex/config.toml` or a trusted project's `.codex/config.toml`, then review the exact hook through `/hooks`. Codex reads `AGENTS.md` and `.agents/skills/` natively.

### Gemini CLI

Add `/absolute/path/to/brain/bin/fkf-hook gemini` as a `SessionStart` command in the user or project `settings.json`. FKF returns Gemini's `hookSpecificOutput.additionalContext` JSON. The exact hook object and millisecond timeout are defined in the [Gemini CLI hooks reference](https://geminicli.com/docs/hooks/reference/).

### Copilot CLI

Add `/absolute/path/to/brain/bin/fkf-hook copilot` to a `sessionStart` command entry under `~/.copilot/hooks/*.json`, or another scope documented by GitHub. FKF returns `additionalContext` JSON. See the [Copilot hooks reference](https://docs.github.com/en/copilot/reference/hooks-reference) for the versioned file shape and scope order.

### Kiro

Create a version `v1` hook under `.kiro/hooks/*.json` with trigger `SessionStart` and command `/absolute/path/to/brain/bin/fkf-hook kiro`. Kiro adds successful command stdout to agent context. See [Kiro hooks](https://kiro.dev/docs/hooks/) for the current standalone file schema.

### Cursor

Register `/absolute/path/to/brain/bin/fkf-hook cursor` for `sessionStart` in the user or project `hooks.json`. FKF returns `additional_context` JSON. Cursor treats this event as fire-and-forget, so test the installed client after changing the hook. See the [Cursor hooks reference](https://prod.cursor.com/docs/hooks).

### Antigravity

Register `/absolute/path/to/brain/bin/fkf-hook antigravity` as a `PreInvocation` command in the workspace `.agents/hooks.json` or global `~/.gemini/config/hooks.json`. FKF returns one `injectSteps` ephemeral message only for invocation zero, so later model calls do not repeatedly spend the session-start budget. See the [Antigravity hooks reference](https://www.antigravity.google/docs/hooks/).

### Cline

Create the executable `TaskStart` hook at `.clinerules/hooks/TaskStart` or the user-level location and have it execute `/absolute/path/to/brain/bin/fkf-hook cline`. FKF returns `cancel: false` with `contextModification`. See the [Cline hooks reference](https://docs.cline.bot/customization/hooks) for enablement and platform-specific executable rules.

### Devin Local (experimental)

The `devin` adapter emits provisional `hookSpecificOutput` JSON, but Devin has no stable public hook contract that FKF can cite or turn into a reliable setup recipe. Do not deploy it from this page; inspect the installed product and retest the exact bytes first.

Only the adapter bytes are repository-tested. Follow each linked vendor page for the surrounding configuration, trust prompt, timeout unit, and supported scope.

## Collected local metadata

When enabled, the bundled `agent-sessions` and `agent-memory-files` collectors read metadata from supported local stores and skip absent products. They never collect prompts or responses.

| Harness         | Session metadata store                            | Memory metadata store                  |
| --------------- | ------------------------------------------------- | -------------------------------------- |
| Claude Code     | `~/.claude/projects/<cwd>/*.jsonl`                | `~/.claude/projects/<cwd>/memory/*.md` |
| Codex           | `~/.codex/sessions/**/rollout-*.jsonl`            | `~/.codex/memories/**/*.md`            |
| Gemini CLI      | `~/.gemini/tmp/<project>/chats/session-*.json[l]` | `~/.gemini/tmp/<project>/memory/*.md`  |
| OpenCode        | `~/.local/share/opencode/opencode.db`             | none                                   |
| Copilot CLI     | `~/.copilot/session-store.db`                     | remote, not collected                  |
| Antigravity CLI | `~/.gemini/antigravity-cli/history.jsonl`         | none                                   |

A session record contains its id, first activity time inside the collected day, harness, working directory, branch, canonical repository identifier when available, and harness-authored title. A file timestamp is never substituted for missing activity evidence. The collectors are ordinary reviewed helpers under the base's `bin/`; users can extend or replace them through the same open command boundary as every other source.
