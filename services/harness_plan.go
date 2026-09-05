package services

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
)

func buildHarnessPlan(baseRoot, baseName, name, executable, workspace string) *HarnessPlan {
	key := harnessRegistrationKey(baseName)
	hook := filepath.Join(baseRoot, core.BaseBinDir, "fkf-hook.sh")
	hookCommand := guardedHookCommand(baseRoot, key, workspace, hook, name, executable)
	mcpArgs := []any{"mcp", "serve", "--base", baseRoot}
	stdioMCP := map[string]any{"command": executable, "args": mcpArgs}
	plan := &HarnessPlan{Name: name, Base: baseRoot, BaseName: baseName, Workspace: workspace}

	switch name {
	case "claude":
		claudeMCP := cloneMap(stdioMCP)
		claudeMCP["type"] = "stdio"
		claudeMCP["env"] = map[string]any{}
		plan.Fragments = append(plan.Fragments, jsonFragment("~/.claude.json", "mcpServers."+key, claudeMCP, false, "mcp", baseRoot, key, ""))
		if workspace != "" {
			plan.Fragments = append(plan.Fragments,
				jsonFragment("~/.claude/settings.json", "hooks.SessionStart", hookGroup("startup|compact", hookCommand, 20), true, "hook", baseRoot, key, workspace))
		}
	case "codex":
		lines := []string{
			"# base: " + strconv.Quote(baseRoot),
			"[mcp_servers." + key + "]", "command = " + strconv.Quote(executable),
			"args = [\"mcp\", \"serve\", \"--base\", " + strconv.Quote(baseRoot) + "]",
		}
		if workspace != "" {
			lines = append(lines, "", "[[hooks.SessionStart]]", `matcher = "startup|compact"`, "", "[[hooks.SessionStart.hooks]]",
				`type = "command"`, "command = "+strconv.Quote(hookCommand), "timeout = 20",
				`statusMessage = "Loading FKF context"`)
		}
		block := managedTOMLBlock(name, key, strings.Join(lines, "\n"))
		plan.Fragments = append(plan.Fragments, tomlFragment("~/.codex/config.toml", block, baseRoot, key, workspace))
	case "gemini":
		plan.Fragments = append(plan.Fragments, jsonFragment("~/.gemini/settings.json", "mcpServers."+key, stdioMCP, false, "mcp", baseRoot, key, ""))
		if workspace != "" {
			plan.Fragments = append(plan.Fragments,
				jsonFragment("~/.gemini/settings.json", "hooks.SessionStart", hookGroup("startup|compact", hookCommand, 20000), true, "hook", baseRoot, key, workspace))
		}
	case "copilot":
		copilotMCP := cloneMap(stdioMCP)
		copilotMCP["type"] = "local"
		copilotMCP["tools"] = []any{"*"}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.copilot/mcp-config.json", "mcpServers."+key, copilotMCP, false, "mcp", baseRoot, key, ""))
		plan.Notes = append(plan.Notes, "Copilot CLI ignores command output from sessionStart; this adapter is MCP-only.")
	case "antigravity":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.gemini/config/mcp_config.json", "mcpServers."+key, stdioMCP, false, "mcp", baseRoot, key, ""))
		plan.Notes = append(plan.Notes, "Antigravity ignores PreInvocation output; this adapter is MCP-only.")
	case "opencode":
		command := []any{executable, "mcp", "serve", "--base", baseRoot}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.config/opencode/opencode.json", "mcp."+key, map[string]any{
				"type": "local", "command": command, "enabled": true,
			}, false, "mcp", baseRoot, key, ""),
		)
		plan.Notes = append(plan.Notes, "OpenCode's current plugin contract has no stable passive session-context transform; this adapter is MCP-only.")
	case "grok":
		block := managedTOMLBlock(name, key, strings.Join([]string{
			"# base: " + strconv.Quote(baseRoot),
			"[mcp_servers." + key + "]", "command = " + strconv.Quote(executable),
			"args = [\"mcp\", \"serve\", \"--base\", " + strconv.Quote(baseRoot) + "]", "enabled = true",
		}, "\n"))
		plan.Fragments = append(plan.Fragments, tomlFragment("~/.grok/config.toml", block, baseRoot, key, ""))
		plan.Notes = append(plan.Notes, "Grok runs SessionStart but ignores passive-hook output; this adapter is MCP-only.")
	case "cursor":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.cursor/mcp.json", "mcpServers."+key, stdioMCP, false, "mcp", baseRoot, key, ""))
		plan.Notes = append(plan.Notes, "Cursor has no verified per-base user hook filename contract; this adapter is MCP-only.")
	case "kiro":
		kiroMCP := cloneMap(stdioMCP)
		kiroMCP["disabled"] = false
		kiroMCP["autoApprove"] = []any{}
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.kiro/settings/mcp.json", "mcpServers."+key, kiroMCP, false, "mcp", baseRoot, key, ""))
		if workspace != "" {
			hookPath := "~/.kiro/hooks/" + key + ".json"
			plan.Fragments = append(plan.Fragments,
				jsonFragment(hookPath, "version", "v1", false, "scalar", baseRoot, key, workspace),
				jsonFragment(hookPath, "hooks", map[string]any{
					"name": "FKF context", "trigger": "SessionStart",
					"action":  map[string]any{"type": "command", "command": hookCommand},
					"timeout": 20, "enabled": true,
				}, true, "hook", baseRoot, key, workspace))
		}
	case "cline":
		plan.Fragments = append(plan.Fragments,
			jsonFragment("~/.cline/data/settings/cline_mcp_settings.json", "mcpServers."+key, stdioMCP, false, "mcp", baseRoot, key, ""),
		)
		plan.Notes = append(plan.Notes, "Cline exposes one global TaskStart filename rather than per-base files; this adapter is MCP-only.")
	}
	plan.Notes = append(plan.Notes, "Skills remain base-local. Install neutral shared FKF skills separately if the harness needs user-scope discovery.")
	return plan
}

func harnessRegistrationKey(baseName string) string { return "fkf-" + baseName }

func hookGroup(matcher, command string, timeout int) map[string]any {
	return map[string]any{
		"matcher": matcher,
		"hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": timeout,
			"statusMessage": "Loading FKF context",
		}},
	}
}

func jsonFragment(path, selector string, value any, array bool, managedKind string, metadata ...string) HarnessFragment {
	baseRoot, key, workspace := "", "", ""
	if len(metadata) > 0 {
		baseRoot = metadata[0]
	}
	if len(metadata) > 1 {
		key = metadata[1]
	}
	if len(metadata) > 2 {
		workspace = metadata[2]
	}
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return HarnessFragment{
		Path: path, Kind: HarnessFragmentJSON, Selector: selector, Content: string(encoded),
		value: normalizeJSON(value), array: array, managedKind: managedKind,
		managedBase: baseRoot, managedKey: key, workspace: workspace, mode: 0o600,
	}
}

func tomlFragment(path, block string, metadata ...string) HarnessFragment {
	fragment := HarnessFragment{Path: path, Kind: HarnessFragmentTOML, Content: block, mode: 0o600}
	if len(metadata) > 0 {
		fragment.managedBase = metadata[0]
	}
	if len(metadata) > 1 {
		fragment.managedKey = metadata[1]
	}
	if len(metadata) > 2 {
		fragment.workspace = metadata[2]
	}
	return fragment
}

func managedTOMLBlock(name, key, content string) string {
	marker := name + " " + key
	return harnessManagedStart + marker + "\n" + strings.TrimSpace(content) + "\n" + harnessManagedEnd + marker + "\n"
}

func guardedHookCommand(baseRoot, key, workspace, hook, harness, executable string) string {
	if workspace == "" {
		return ""
	}
	// Harness configuration lives outside the trust digest, so it must verify the current
	// base plan before dispatching a base-owned executable hook.
	marker := ": fkf-key=" + url.PathEscape(key) + " fkf-base=" + url.PathEscape(baseRoot) +
		" fkf-workspace=" + url.PathEscape(workspace)
	check := shellQuote(executable) + " trust --check --base " + shellQuote(baseRoot) + " >/dev/null 2>&1"
	dispatch := shellQuote(hook) + " " + shellQuote(harness) + " " + shellQuote(executable) + " " + shellQuote(workspace)
	return marker + "; " + check + " && " + dispatch + " || " + emptyHarnessCommand(harness)
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
