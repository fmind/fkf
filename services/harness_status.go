package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fmind/fkf/core"
)

// HarnessRegistration is the read-only status of one complete managed integration for this
// base. Registered means its MCP fragment matches exactly.
type HarnessRegistration struct {
	Name       string   `json:"name"`
	Registered bool     `json:"registered"`
	Changes    int      `json:"changes,omitempty"`
	Error      string   `json:"error,omitempty"`
	Cleanup    []string `json:"manual_cleanup,omitempty"`
}

// InspectHarnesses reads user-scope harness files without writing or requiring the base-owned
// assets to exist. A conflict is data in the report rather than a failure of `fkf status`.
func InspectHarnesses(ctx context.Context, baseRoot, home, executable string) ([]HarnessRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := validateHarnessBase(baseRoot)
	if err != nil {
		return nil, err
	}
	home, err = harnessHome(home)
	if err != nil {
		return nil, err
	}
	executable, err = validateHarnessExecutable(executable)
	if err != nil {
		return nil, err
	}
	baseName, err := harnessBaseName(root)
	if err != nil {
		return nil, err
	}
	registrations := make([]HarnessRegistration, 0, len(harnessOrder))
	for _, name := range HarnessNames() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files, inspectErr := preflightHarnessPlans(ctx, home, []*HarnessPlan{buildHarnessPlan(root, baseName, name, executable, "")})
		entry := HarnessRegistration{Name: name}
		entry.Cleanup = inspectUnscopedHarness(home, name)
		if inspectErr != nil {
			entry.Error = fmt.Sprintf("%v", inspectErr)
			registrations = append(registrations, entry)
			continue
		}
		for _, file := range files {
			if file.changed {
				entry.Changes++
			}
		}
		entry.Registered = entry.Changes == 0
		registrations = append(registrations, entry)
	}
	return registrations, nil
}

type unscopedHarnessTarget struct {
	path, selector string
	toml           bool
}

func inspectUnscopedHarness(home, harness string) []string {
	targets := map[string][]unscopedHarnessTarget{
		"claude":      {{path: ".claude.json", selector: "mcpServers.fkf"}, {path: ".claude/settings.json", selector: "hooks.SessionStart"}},
		"codex":       {{path: ".codex/config.toml", toml: true}},
		"gemini":      {{path: ".gemini/settings.json", selector: "mcpServers.fkf"}, {path: ".gemini/settings.json", selector: "hooks.SessionStart"}},
		"copilot":     {{path: ".copilot/mcp-config.json", selector: "mcpServers.fkf"}, {path: ".copilot/hooks/fkf.json", selector: "hooks.sessionStart"}},
		"antigravity": {{path: ".gemini/config/mcp_config.json", selector: "mcpServers.fkf"}, {path: ".gemini/config/hooks.json", selector: "fkf"}},
		"opencode":    {{path: ".config/opencode/opencode.json", selector: "mcp.fkf"}, {path: ".config/opencode/plugins/fkf.js"}},
		"grok":        {{path: ".grok/config.toml", toml: true}},
		"cursor":      {{path: ".cursor/mcp.json", selector: "mcpServers.fkf"}},
		"kiro":        {{path: ".kiro/settings/mcp.json", selector: "mcpServers.fkf"}, {path: ".kiro/hooks/fkf.json", selector: "hooks"}},
		"cline":       {{path: ".cline/data/settings/cline_mcp_settings.json", selector: "mcpServers.fkf"}, {path: ".cline/hooks/TaskStart"}},
	}
	var found []string
	for _, target := range targets[harness] {
		absolute := filepath.Join(home, filepath.FromSlash(target.path))
		body, err := core.ReadFileLimit(absolute, core.MaxControlFileBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			continue
		}
		unscoped := false
		switch {
		case target.toml:
			text := string(body)
			unscoped = strings.Contains(text, "[mcp_servers.fkf]") && strings.Contains(text, "mcp\", \"serve")
		case target.selector == "":
			unscoped = strings.Contains(string(body), "fkf-hook.sh")
		default:
			var root map[string]any
			if json.Unmarshal(body, &root) == nil {
				if value, ok := harnessJSONValue(root, target.selector); ok {
					unscoped = jsonValueManaged(value, "mcp", harness) || findHarnessHookString(value, harness)
				}
			}
		}
		if unscoped {
			location := "~/" + filepath.ToSlash(target.path)
			if target.selector != "" {
				location += "#" + target.selector
			}
			found = append(found, location)
		}
	}
	return found
}

func harnessJSONValue(root map[string]any, selector string) (any, bool) {
	var value any = root
	for _, part := range strings.Split(selector, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return value, true
}
