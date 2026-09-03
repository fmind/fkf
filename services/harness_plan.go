package services

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
)

func buildHarnessPlan(baseRoot, name, executable string) *HarnessPlan {
	hook := filepath.Join(baseRoot, core.BaseBinDir, "fkf-hook.sh")
	hookCommand := guardedHookCommand(baseRoot, hook, name, executable)
	mcpArgs := []any{"mcp", "serve", "--base", baseRoot}
	stdioMCP := map[string]any{"command": executable, "args": mcpArgs}
	plan := &HarnessPlan{Name: name, Base: baseRoot}

	switch name {
	case "claude":
		claudeMCP := cloneMap(stdioMCP)
		claudeMCP["type"] = "stdio"
		claudeMCP["env"] = map[string]any{}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.claude.json", "mcpServers.fkf", claudeMCP, false, "mcp"),
			jsonFragment("~/.claude/settings.json", "hooks.SessionStart", hookGroup("startup|compact", hookCommand, 20), true, "hook"),
		)
	case "codex":
		block := managedTOMLBlock(name, strings.Join([]string{
			"[mcp_servers.fkf]", "command = " + strconv.Quote(executable),
			"args = [\"mcp\", \"serve\", \"--base\", " + strconv.Quote(baseRoot) + "]", "",
			"[[hooks.SessionStart]]", `matcher = "startup|compact"`, "", "[[hooks.SessionStart.hooks]]",
			`type = "command"`, "command = " + strconv.Quote(hookCommand), "timeout = 20",
			`statusMessage = "Loading FKF context"`,
		}, "\n"))
		plan.Fragments = append(plan.Fragments, tomlFragment("~/.codex/config.toml", block))
	case "gemini":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.gemini/settings.json", "mcpServers.fkf", stdioMCP, false, "mcp"),
			jsonFragment("~/.gemini/settings.json", "hooks.SessionStart", hookGroup("startup|compact", hookCommand, 20000), true, "hook"),
		)
	case "copilot":
		copilotMCP := cloneMap(stdioMCP)
		copilotMCP["type"] = "local"
		copilotMCP["tools"] = []any{"*"}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.copilot/mcp-config.json", "mcpServers.fkf", copilotMCP, false, "mcp"),
			jsonFragment("~/.copilot/hooks/fkf.json", "version", 1, false, "scalar"),
			jsonFragment("~/.copilot/hooks/fkf.json", "hooks.sessionStart", map[string]any{
				"type": "command", "bash": hookCommand, "timeoutSec": 20,
			}, true, "hook"),
		)
		plan.Notes = append(plan.Notes, "Copilot CLI runs sessionStart hooks but ignores their output; MCP and skills remain fully usable.")
	case "antigravity":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.gemini/config/mcp_config.json", "mcpServers.fkf", stdioMCP, false, "mcp"),
			jsonFragment("~/.gemini/config/hooks.json", "fkf", map[string]any{
				"enabled": true,
				"PreInvocation": []any{map[string]any{
					"type": "command", "command": hookCommand, "timeout": 20,
				}},
			}, false, "hook"),
		)
	case "opencode":
		command := []any{executable, "mcp", "serve", "--base", baseRoot}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.config/opencode/opencode.json", "mcp.fkf", map[string]any{
				"type": "local", "command": command, "enabled": true,
			}, false, "mcp"),
			fileFragment("~/.config/opencode/plugins/fkf.js", openCodePlugin(baseRoot, hook, executable), 0o600),
		)
	case "grok":
		block := managedTOMLBlock(name, strings.Join([]string{
			"[mcp_servers.fkf]", "command = " + strconv.Quote(executable),
			"args = [\"mcp\", \"serve\", \"--base\", " + strconv.Quote(baseRoot) + "]", "enabled = true",
		}, "\n"))
		plan.Fragments = append(plan.Fragments,
			tomlFragment("~/.grok/config.toml", block),
			jsonFragment("~/.grok/hooks/fkf.json", "hooks.SessionStart", hookGroup("startup|compact", hookCommand, 20), true, "hook"),
		)
		plan.Notes = append(plan.Notes, "Grok 1.0.5 runs SessionStart but ignores passive-hook stdout; MCP remains fully usable.")
	case "cursor":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.cursor/mcp.json", "mcpServers.fkf", stdioMCP, false, "mcp"),
			jsonFragment("~/.cursor/hooks.json", "version", 1, false, "scalar"),
			jsonFragment("~/.cursor/hooks.json", "hooks.sessionStart", map[string]any{
				"command": hookCommand,
			}, true, "hook"),
		)
	case "kiro":
		kiroMCP := cloneMap(stdioMCP)
		kiroMCP["disabled"] = false
		kiroMCP["autoApprove"] = []any{}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.kiro/settings/mcp.json", "mcpServers.fkf", kiroMCP, false, "mcp"),
			jsonFragment("~/.kiro/hooks/fkf.json", "version", "v1", false, "scalar"),
			jsonFragment("~/.kiro/hooks/fkf.json", "hooks", map[string]any{
				"name": "FKF context", "trigger": "SessionStart",
				"action":  map[string]any{"type": "command", "command": hookCommand},
				"timeout": 20, "enabled": true,
			}, true, "hook"),
		)
	case "cline":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.cline/data/settings/cline_mcp_settings.json", "mcpServers.fkf", stdioMCP, false, "mcp"),
			fileFragment("~/.cline/hooks/TaskStart", clineHook(hookCommand), 0o700),
		)
	}

	bridgeRoot := map[string]string{
		"claude": "~/.claude/skills", "codex": "~/.agents/skills", "gemini": "~/.gemini/skills",
		"copilot": "~/.copilot/skills", "antigravity": "~/.gemini/antigravity-cli/skills", "opencode": "~/.agents/skills",
		"grok": "~/.grok/skills", "cursor": "~/.cursor/skills", "kiro": "~/.kiro/skills", "cline": "~/.cline/skills",
	}[name]
	for _, skill := range BundledSkills {
		plan.Fragments = append(plan.Fragments, HarnessFragment{
			Path: bridgeRoot + "/" + skill, Kind: HarnessFragmentLink,
			Content: filepath.Join(baseRoot, core.BaseSkillsDir, skill),
		})
	}
	return plan
}

func hookGroup(matcher, command string, timeout int) map[string]any {
	return map[string]any{
		"matcher": matcher,
		"hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": timeout,
			"statusMessage": "Loading FKF context",
		}},
	}
}

func jsonFragment(path, selector string, value any, array bool, managedKind string) HarnessFragment {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return HarnessFragment{
		Path: path, Kind: HarnessFragmentJSON, Selector: selector, Content: string(encoded),
		value: normalizeJSON(value), array: array, managedKind: managedKind, mode: 0o600,
	}
}

func tomlFragment(path, block string) HarnessFragment {
	return HarnessFragment{Path: path, Kind: HarnessFragmentTOML, Content: block, mode: 0o600}
}

func fileFragment(path, content string, mode os.FileMode) HarnessFragment {
	return HarnessFragment{Path: path, Kind: HarnessFragmentFile, Content: content, mode: mode}
}

func managedTOMLBlock(name, content string) string {
	return harnessManagedStart + name + "\n" + strings.TrimSpace(content) + "\n" + harnessManagedEnd + name + "\n"
}

func openCodePlugin(baseRoot, hook, executable string) string {
	return `// Managed by fkf harness install: opencode
// OpenCode has no SessionStart context hook. Inject once per session at its documented
// system-transform seam, preserving the hook's fail-open behavior. Verify the base's
// execution trust before dispatch because the hook itself is base-owned executable code.
const seen = new Set()

export const Fkf = async ({ directory }) => ({
  "experimental.chat.system.transform": async (input, output) => {
    if (seen.has(input.sessionID)) return
    seen.add(input.sessionID)
    const trust = Bun.spawn([` + strconv.Quote(executable) + `, "trust", "--check", "--base", ` + strconv.Quote(baseRoot) + `], {
      cwd: directory,
      stdout: "ignore",
      stderr: "ignore",
    })
    if ((await trust.exited) !== 0) return
    const child = Bun.spawn([` + strconv.Quote(hook) + `, "opencode", ` + strconv.Quote(executable) + `], {
      cwd: directory,
      stdin: "pipe",
      stdout: "pipe",
      stderr: "ignore",
    })
    child.stdin.write(JSON.stringify({ cwd: directory }))
    child.stdin.end()
    const text = await new Response(child.stdout).text()
    if ((await child.exited) === 0 && text.trim()) output.system.push(text.trimEnd())
  },
})
`
}

func clineHook(command string) string {
	return "#!/bin/sh\n# Managed by fkf harness install: cline\n" + command + "\n"
}

func guardedHookCommand(baseRoot, hook, harness, executable string) string {
	// Harness configuration lives outside the trust digest, so it must verify the current
	// base plan before dispatching a base-owned executable hook.
	check := shellQuote(executable) + " trust --check --base " + shellQuote(baseRoot) + " >/dev/null 2>&1"
	dispatch := shellQuote(hook) + " " + shellQuote(harness) + " " + shellQuote(executable)
	return check + " && " + dispatch + " || " + emptyHarnessCommand(harness)
}

func emptyHarnessCommand(harness string) string {
	switch harness {
	case "claude", "opencode", "grok", "kiro":
		return ":"
	case "cline":
		return "printf '%s\\n' '{\"cancel\":false}'"
	default:
		return "printf '%s\\n' '{}'"
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func normalizeJSON(value any) any {
	encoded, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	_ = decoder.Decode(&normalized)
	return normalized
}
