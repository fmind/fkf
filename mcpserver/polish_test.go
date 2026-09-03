package mcpserver_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/mcpserver"
)

func TestEveryMCPStringInputIsBoundedAndReadPublishesItsSchema(t *testing.T) {
	session := connect(t, newBase(t))
	for _, want := range []struct {
		tool   string
		fields []string
	}{
		{"find", []string{"since", "until", "cursor"}},
		{"context", []string{"query", "since", "until"}},
		{"day", []string{"date"}},
		{"timeline", []string{"since", "until", "repo", "person", "uri", "around"}},
		{"list", []string{"layer", "since", "until", "source", "status", "type", "cursor"}},
		{"read", []string{"uri", "cursor"}},
		{"graph", []string{"uri", "direction", "kind", "cursor"}},
	} {
		tool := listedTool(t, session, want.tool)
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				MaxLength *int `json:"maxLength"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		for _, field := range want.fields {
			property, ok := schema.Properties[field]
			if !ok || property.MaxLength == nil || *property.MaxLength <= 0 {
				t.Errorf("%s.%s schema = %+v, want a positive maxLength", want.tool, field, property)
			}
		}
	}
}

func TestMCPToolDescriptionsNameDefaultsAndAnExample(t *testing.T) {
	session := connect(t, newBase(t))
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		for _, marker := range []string{"Default:", "Example:"} {
			if !strings.Contains(tool.Description, marker) {
				t.Errorf("%s description omits %q: %s", tool.Name, marker, tool.Description)
			}
		}
		hint, ok := tool.Meta[mcpserver.ResultSizeMetaKey].(map[string]any)
		if !ok || hint["maxBytes"] == nil {
			t.Errorf("%s _meta[%q] = %#v, want a maximum result-size hint", tool.Name, mcpserver.ResultSizeMetaKey, hint)
		}
	}
}

func TestMCPToolResultCarriesItsActualSizeHint(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hint, ok := result.Meta[mcpserver.ResultSizeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("result _meta[%q] = %#v", mcpserver.ResultSizeMetaKey, result.Meta)
	}
	if hint["bytes"] == nil || hint["items"] == nil || hint["maxBytes"] == nil {
		t.Fatalf("result-size hint = %#v, want bytes, items, and maxBytes", hint)
	}
}

func TestGraphDerivedResultsPublishTheirValidatedGenerationWithoutAToolCallTTL(t *testing.T) {
	session := connect(t, newBase(t))
	validatedGeneration := ""
	for _, call := range []*mcp.CallToolParams{
		{Name: "graph", Arguments: map[string]any{"uri": "ticket:FK-412"}},
		{Name: "read", Arguments: map[string]any{"uri": "ticket:FK-412"}},
		{Name: "context", Arguments: map[string]any{"query": "FK-412", "expand": true}},
	} {
		result, err := session.CallTool(t.Context(), call)
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := result.Meta[mcpserver.GraphGenerationMetaKey].(string)
		if len(generation) != 64 {
			t.Errorf("%s graph generation = %q, want a SHA-256 digest", call.Name, generation)
		}
		if validatedGeneration == "" {
			validatedGeneration = generation
		} else if generation != validatedGeneration {
			t.Errorf("%s graph generation = %q, want the shared validated generation %q",
				call.Name, generation, validatedGeneration)
		}
		if _, exists := result.Meta["ttlMs"]; exists {
			t.Errorf("%s tools/call result invents unsupported ttlMs metadata: %#v", call.Name, result.Meta)
		}
	}

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools.TTLMs != 0 {
		t.Fatalf("tools/list ttlMs = %d, want immediately stale because graph generation changes are not notified", tools.TTLMs)
	}
}
