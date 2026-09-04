package mcpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/mcpserver"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

var testClock = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

const testConfig = `fkf: 1
name: brain
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
  repository: {description: Repository associated with the record., cardinality: optional, relation: true}
  participant: {description: Actors associated with the record., cardinality: many, relation: true}
  ticket: {description: Work item associated with the record., cardinality: optional, relation: true}
layers:
  events: true
  index: true
  tasks: true
  projects: true
  wiki: true
sources:
  synthetic:
    enabled: true
    layer: events
    run: [cli, --since, "{{date}}"]
    fields:
      id: .id
      time: .t
      title: .subject
      repository: .repository_uri
      participant: ".participant_uris[]"
      ticket: .ticket_uri
    body: [cli, view, "{{id}}"]
`

// newBase builds a small, populated base. The suite is hermetic: HOME and XDG_STATE_HOME are
// redirected and nothing here can execute a declared command.
func newBase(t *testing.T) *services.Base {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(core.BaseEnvVar, "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := services.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	base.Now = func() time.Time { return testClock }
	base.Runner = sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		t.Fatal("an MCP handler executed a command; the server is read-only by construction")
		return "", nil
	})

	source, _ := base.Source("synthetic")
	day, _ := sources.ParseDay("2026-05-04")
	document, err := sources.Collect(t.Context(),
		sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
			return `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Fix FK-412","repo":"fmind/fkf","who":"marc@example.test","repository_uri":"repo:github.com/fmind/fkf","participant_uris":["person:email/marc@example.test"],"ticket_uri":"ticket:FK-412"}]`, nil
		}), source, base.Env, sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, "wiki/index.md", "# Wiki\n\n- [A decision](a-decision.md)\n")
	writeFile(t, base, "wiki/a-decision.md", "---\ntype: decision\ntitle: A decision\ntags: [architecture]\n---\n\n# A decision\n")
	writeFile(t, base, "projects/p.md", "---\ntype: project\ntitle: P\nstatus: active\ntags: [x]\n---\n\n# P\n")
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	return base
}

func writeFile(t *testing.T, base *services.Base, relative, body string) {
	t.Helper()
	absolute, err := base.Store.Resolve(relative)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(body), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}

func collectSynthetic(t *testing.T, base *services.Base, date, records string) {
	t.Helper()
	source, err := base.Source("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	day, err := sources.ParseDay(date)
	if err != nil {
		t.Fatal(err)
	}
	document, err := sources.Collect(t.Context(),
		sources.RunnerFunc(func(context.Context, sources.Command) (string, error) { return records, nil }),
		source, base.Env, sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}

func TestServerStartupAndInstructionsRefuseAPreCancelledContext(t *testing.T) {
	base := newBase(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := mcpserver.Instructions(ctx, base); !errors.Is(err, context.Canceled) {
		t.Fatalf("Instructions error = %v, want context.Canceled", err)
	}
	if _, err := mcpserver.New(ctx, base); !errors.Is(err, context.Canceled) {
		t.Fatalf("New error = %v, want context.Canceled", err)
	}
}

// connect wires a client to the server over an in-memory transport, which is how the tools are
// exercised the way a real client would.
func connect(t *testing.T, base *services.Base) *mcp.ClientSession {
	t.Helper()
	server, err := mcpserver.New(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() {
		if err := server.Run(context.Background(), serverTransport); err != nil {
			return
		}
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool returned an error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatal(err)
	}
}

func errorText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	text, _ := result.Content[0].(*mcp.TextContent)
	if text == nil {
		return ""
	}
	return text.Text
}

func TestServerExposesExactlySevenToolsAndFourResources(t *testing.T) {
	session := connect(t, newBase(t))
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Fatalf("tool %s has no description", tool.Name)
		}
		// The read-only thesis has to be machine-readable, not just a package doc comment: a
		// client that trusts annotations can only auto-approve a call it can see is safe.
		annotations := tool.Annotations
		if annotations == nil || !annotations.ReadOnlyHint || !annotations.IdempotentHint ||
			annotations.DestructiveHint == nil || *annotations.DestructiveHint ||
			annotations.OpenWorldHint == nil || *annotations.OpenWorldHint {
			t.Fatalf("tool %s annotations = %+v, want read-only, idempotent, non-destructive, closed-world",
				tool.Name, annotations)
		}
	}
	if strings.Join(names, ",") != "context,day,find,graph,list,read,timeline" {
		t.Fatalf("tools = %v, want exactly context, day, find, graph, list, read, timeline", names)
	}
	resources, err := session.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 4 {
		t.Fatalf("resources = %d, want four", len(resources.Resources))
	}
	for _, resource := range resources.Resources {
		if !strings.HasPrefix(resource.URI, "fkf://brain/") {
			t.Fatalf("resource %q must live under the base's own authority", resource.URI)
		}
	}
}

// TestNoToolCanFetchABody is the read-only guarantee, checked structurally: `--body` is the one
// read that runs a command, and no tool takes a parameter that could reach it.
func TestNoToolCanFetchABody(t *testing.T) {
	session := connect(t, newBase(t))
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"body", "sync", "write", "trust", "shell", "exec", "command"} {
			if strings.Contains(strings.ToLower(string(encoded)), `"`+forbidden+`"`) {
				t.Fatalf("tool %s takes a %q parameter; nothing here may write, shell, or fetch",
					tool.Name, forbidden)
			}
		}
	}
	// And a client that sends the argument anyway gets a read with no body: the parameter does
	// not exist, so the handler has nothing to set. The base's runner fails the test if any
	// handler executes a command, which is the other half of the same guarantee.
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": "events/2026-05-04/synthetic.json#a1", "body": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"body"`) || strings.Contains(string(encoded), "body_state") {
		t.Fatalf("read returned a body over MCP: %s", encoded)
	}
}

func TestOnlyPageableToolsPublishACursor(t *testing.T) {
	session := connect(t, newBase(t))
	for _, name := range []string{"find", "list", "read", "graph", "context", "day", "timeline"} {
		tool := listedTool(t, session, name)
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		_, hasCursor := schema.Properties["cursor"]
		if want := name != "context" && name != "day" && name != "timeline"; hasCursor != want {
			t.Fatalf("%s cursor schema = %t, want %t", name, hasCursor, want)
		}
	}
}

func TestToolsAnswerFromTheBase(t *testing.T) {
	session := connect(t, newBase(t))
	cases := []struct {
		tool      string
		arguments map[string]any
		want      string
	}{
		{"find", map[string]any{"grep": []string{"FK-412"}}, "events/2026-05-04/synthetic.json#a1"},
		{"find", map[string]any{"count": true}, `"date":"2026-05-04"`},
		{"context", map[string]any{"query": "FK-412", "explain": true}, `"input_digest"`},
		// The pack's own trust framing has to survive the MCP transport unchanged: a client
		// that reads the tool result directly, never the server's one-time Instructions, still
		// needs to see what a record is and is not.
		{"context", map[string]any{"query": "FK-412"}, `"notice":"Records`},
		{"list", map[string]any{"layer": "wiki"}, "a-decision"},
		{"list", map[string]any{"layer": "events"}, "2026-05-04"},
		{"read", map[string]any{"uri": "wiki/a-decision.md"}, "A decision"},
		{"read", map[string]any{"uri": "ticket:FK-412"}, `"scheme":"ticket"`},
		{"graph", map[string]any{"uri": "ticket:FK-412", "direction": "in"}, "synthetic.json#a1"},
	}
	for _, test := range cases {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: test.tool, Arguments: test.arguments})
		if err != nil {
			t.Fatalf("%s(%v) error = %v", test.tool, test.arguments, err)
		}
		if result.IsError {
			t.Fatalf("%s(%v) returned an error result: %+v", test.tool, test.arguments, result.Content)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), test.want) {
			t.Fatalf("%s(%v) = %s, want it to contain %q", test.tool, test.arguments, encoded, test.want)
		}
	}
}

func TestContextResolvesRelativeWindowAndAsOfFromOneClockRead(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	clockReads := 0
	base.Now = func() time.Time {
		clockReads++
		return testClock.AddDate(0, 0, clockReads-1)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "context", Arguments: map[string]any{
			"query": "FK-412", "since": "today", "until": "today",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var pack services.ContextPack
	decodeStructured(t, result, &pack)
	// The MCP wrapper samples duration before and after the handler. The one clock read between
	// them must bind both relative bounds and as_of, even when every sample crosses midnight.
	if clockReads != 3 || pack.Receipt.AsOf != "2026-05-11" ||
		pack.Receipt.Window.Since != pack.Receipt.AsOf || pack.Receipt.Window.Until != pack.Receipt.AsOf {
		t.Fatalf("clock reads = %d, receipt = %+v; want one shared evaluation instant", clockReads, pack.Receipt)
	}
}

func TestFindAcceptsEveryRepeatableCLIRecordFilter(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find",
		Arguments: map[string]any{
			"source": []string{"synthetic"},
			"layer":  []string{"events", "index"},
			"grep":   []string{"Fix", "FK-412"},
			"where":  []string{".subject=Fix FK-412"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("find with repeatable filters returned an error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var found services.FindResult
	if err := json.Unmarshal(encoded, &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Records) != 1 || found.Records[0].URI != "events/2026-05-04/synthetic.json#a1" {
		t.Fatalf("find records = %+v, want only the record satisfying every repeated filter", found.Records)
	}
}

func TestFindWhereIsNotSilentlyIgnored(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{"where": []string{".subject=not-present"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("find with a valid where clause returned an error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var found services.FindResult
	if err := json.Unmarshal(encoded, &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Records) != 0 {
		t.Fatalf("find records = %+v, want no record matching the where clause", found.Records)
	}
}

func TestListUsesBothWindowBounds(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list",
		Arguments: map[string]any{
			"layer": "events", "since": "2026-05-01", "until": "2026-05-03",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list with a valid closed window returned an error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var listing services.EventListing
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Days) != 0 {
		t.Fatalf("list days = %+v, want the until bound to exclude 2026-05-04", listing.Days)
	}
}

func TestListAppliesEachLayerSpecificFilter(t *testing.T) {
	base := newBase(t)
	writeFile(t, base, "wiki/a-decision.md", "---\ntype: decision\ntitle: A decision\ntags: [architecture, retrieval]\n---\n\n# A decision\n")
	session := connect(t, base)
	for _, test := range []struct {
		name      string
		arguments map[string]any
		wantEmpty bool
		want      string
	}{
		{
			name: "events source", arguments: map[string]any{"layer": "events", "source": "synthetic"},
			want: "2026-05-04",
		},
		{
			name: "wiki repeated tags", arguments: map[string]any{
				"layer": "wiki", "tag": []string{"architecture", "retrieval"},
			},
			want: "a-decision",
		},
		{
			name: "wiki type", arguments: map[string]any{"layer": "wiki", "type": "pattern"},
			wantEmpty: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "list", Arguments: test.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("list(%v) returned an error: %+v", test.arguments, result.Content)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantEmpty && (strings.Contains(string(encoded), "2026-05-04") || strings.Contains(string(encoded), "a-decision")) {
				t.Fatalf("list(%v) = %s, want the layer-specific filter to exclude the fixture", test.arguments, encoded)
			}
			if test.want != "" && !strings.Contains(string(encoded), test.want) {
				t.Fatalf("list(%v) = %s, want %q", test.arguments, encoded, test.want)
			}
		})
	}
}

// TestListPagesTheTasksLayerWithinItsWindow is the tasks layer's only successful list: every
// other case in this file either filters the layer out or is rejected before the dispatch, so
// without it the whole tasks arm — window parse, snapshot binding, page copy — never runs.
func TestListPagesTheTasksLayerWithinItsWindow(t *testing.T) {
	base := newBase(t)
	writeFile(t, base, "tasks/2026-05-04/a-session/"+core.TaskTraceFile, "# A session\n\nRequest.\n")
	writeFile(t, base, "tasks/2026-05-06/later-session/"+core.TaskTraceFile, "# Later session\n\nRequest.\n")
	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{
			"layer": "tasks", "since": "2026-05-04", "until": "2026-05-04",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list(tasks) returned an error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var listing services.TaskListing
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Traces) != 1 {
		t.Fatalf("list(tasks) traces = %+v, want only the trace inside the window", listing.Traces)
	}
	trace := listing.Traces[0]
	if trace.URI != "tasks/2026-05-04/a-session/"+core.TaskTraceFile || trace.Title != "A session" {
		t.Fatalf("trace = %+v, want the windowed trace's URI and heading", trace)
	}
	if listing.Window.Since != "2026-05-04" || listing.Window.Until != "2026-05-04" {
		t.Fatalf("window = %+v, want the bounds the call asked for", listing.Window)
	}
}

func TestListRejectsFiltersThatDoNotApplyToTheLayer(t *testing.T) {
	session := connect(t, newBase(t))
	for _, test := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "events tag", arguments: map[string]any{"layer": "events", "tag": []string{"architecture"}}},
		{name: "events type", arguments: map[string]any{"layer": "events", "type": "decision"}},
		{name: "events status", arguments: map[string]any{"layer": "events", "status": "active"}},
		{name: "tasks status", arguments: map[string]any{"layer": "tasks", "status": "active"}},
		{name: "tasks source", arguments: map[string]any{"layer": "tasks", "source": "synthetic"}},
		{name: "index since", arguments: map[string]any{"layer": "index", "since": "7d"}},
		{name: "index tag", arguments: map[string]any{"layer": "index", "tag": []string{"architecture"}}},
		{name: "index type", arguments: map[string]any{"layer": "index", "type": "decision"}},
		{name: "index source", arguments: map[string]any{"layer": "index", "source": "synthetic"}},
		{name: "wiki status", arguments: map[string]any{"layer": "wiki", "status": "active"}},
		{name: "wiki source", arguments: map[string]any{"layer": "wiki", "source": "synthetic"}},
		{name: "projects until", arguments: map[string]any{"layer": "projects", "until": "today"}},
		{name: "projects type", arguments: map[string]any{"layer": "projects", "type": "project"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "list", Arguments: test.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("list(%v) succeeded, want an incompatible-filter error", test.arguments)
			}
		})
	}
}

func TestNumericToolInputsPublishAndEnforceTheirDomains(t *testing.T) {
	session := connect(t, newBase(t))
	type bound struct {
		tool, field string
		minimum     float64
		maximum     *float64
	}
	three := float64(services.MaxGraphDepth)
	maxBudget := float64(core.MaxNarrativeBytes / 4)
	for _, want := range []bound{
		{tool: "find", field: "limit", minimum: 0},
		{tool: "list", field: "limit", minimum: 0},
		{tool: "context", field: "budget", minimum: 1, maximum: &maxBudget},
		{tool: "day", field: "budget", minimum: 1, maximum: &maxBudget},
		{tool: "timeline", field: "budget", minimum: 1, maximum: &maxBudget},
		{tool: "graph", field: "depth", minimum: 1, maximum: &three},
		{tool: "graph", field: "limit", minimum: 0},
	} {
		tool := listedTool(t, session, want.tool)
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Minimum *float64 `json:"minimum"`
				Maximum *float64 `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		property := schema.Properties[want.field]
		if property.Minimum == nil || *property.Minimum != want.minimum {
			t.Fatalf("%s.%s minimum = %v, want %v", want.tool, want.field, property.Minimum, want.minimum)
		}
		if want.maximum != nil && (property.Maximum == nil || *property.Maximum != *want.maximum) {
			t.Fatalf("%s.%s maximum = %v, want %v", want.tool, want.field, property.Maximum, *want.maximum)
		}
	}

	for _, call := range []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{name: "find negative limit", tool: "find", arguments: map[string]any{"limit": -1}},
		{name: "list negative limit", tool: "list", arguments: map[string]any{"layer": "wiki", "limit": -1}},
		{name: "context negative budget", tool: "context", arguments: map[string]any{"query": "FK-412", "budget": -1}},
		{name: "context zero budget", tool: "context", arguments: map[string]any{"query": "FK-412", "budget": 0}},
		{name: "graph negative depth", tool: "graph", arguments: map[string]any{"uri": "ticket:FK-412", "depth": -1}},
		{name: "graph zero depth", tool: "graph", arguments: map[string]any{"uri": "ticket:FK-412", "depth": 0}},
		{name: "graph excessive depth", tool: "graph", arguments: map[string]any{"uri": "ticket:FK-412", "depth": services.MaxGraphDepth + 1}},
		{name: "graph negative limit", tool: "graph", arguments: map[string]any{"uri": "ticket:FK-412", "limit": -1}},
	} {
		t.Run(call.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: call.tool, Arguments: call.arguments})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("%s(%v) succeeded, want schema validation failure", call.tool, call.arguments)
			}
		})
	}
}

func TestToolInputsPublishAndEnforceTextAndRepeatedFilterBounds(t *testing.T) {
	const (
		maxText     = 4096
		maxRepeated = 64
	)
	session := connect(t, newBase(t))
	for _, want := range []struct {
		tool, field string
		maxLength   int
		maxItems    int
		itemLength  int
	}{
		{tool: "context", field: "query", maxLength: maxText},
		{tool: "context", field: "pin", maxItems: maxRepeated, itemLength: maxText},
		{tool: "find", field: "grep", maxItems: maxRepeated, itemLength: maxText},
		{tool: "find", field: "where", maxItems: maxRepeated, itemLength: maxText},
		{tool: "list", field: "tag", maxItems: maxRepeated, itemLength: maxText},
	} {
		tool := listedTool(t, session, want.tool)
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				MaxLength *int `json:"maxLength"`
				MaxItems  *int `json:"maxItems"`
				Items     *struct {
					MaxLength *int `json:"maxLength"`
				} `json:"items"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		property := schema.Properties[want.field]
		if want.maxLength > 0 && (property.MaxLength == nil || *property.MaxLength != want.maxLength) {
			t.Fatalf("%s.%s maxLength = %v, want %d", want.tool, want.field, property.MaxLength, want.maxLength)
		}
		if want.maxItems > 0 && (property.MaxItems == nil || *property.MaxItems != want.maxItems) {
			t.Fatalf("%s.%s maxItems = %v, want %d", want.tool, want.field, property.MaxItems, want.maxItems)
		}
		if want.itemLength > 0 && (property.Items == nil || property.Items.MaxLength == nil ||
			*property.Items.MaxLength != want.itemLength) {
			t.Fatalf("%s.%s items.maxLength = %+v, want %d", want.tool, want.field, property.Items, want.itemLength)
		}
	}

	tooMany := make([]string, maxRepeated+1)
	for index := range tooMany {
		tooMany[index] = "term"
	}
	for _, call := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "long query", arguments: map[string]any{"query": strings.Repeat("q", maxText+1)}},
		{name: "long filter", arguments: map[string]any{"query": "q", "pin": []string{strings.Repeat("p", maxText+1)}}},
		{name: "too many pins", arguments: map[string]any{"query": "q", "pin": tooMany}},
		{name: "excessive budget", arguments: map[string]any{"query": "q", "budget": int(core.MaxNarrativeBytes/4) + 1}},
	} {
		t.Run(call.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "context", Arguments: call.arguments})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("context(%v) succeeded, want schema validation failure", call.arguments)
			}
		})
	}
}

func TestWindowToolInputsPublishTheSharedGrammar(t *testing.T) {
	session := connect(t, newBase(t))
	for _, toolName := range []string{"find", "context", "list", "timeline"} {
		tool := listedTool(t, session, toolName)
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		for _, bound := range []string{"since", "until"} {
			description := schema.Properties[bound].Description
			for _, form := range []string{"YYYY-MM-DD", "today", "yesterday", "7d"} {
				if !strings.Contains(description, form) {
					t.Fatalf("%s.%s description = %q, want shared window form %q", toolName, bound, description, form)
				}
			}
		}
	}
}

func listedTool(t *testing.T, session *mcp.ClientSession, name string) *mcp.Tool {
	t.Helper()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return nil
}

func TestResourcesAreReadable(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	for _, name := range []string{"wiki/index", "wiki/tags", "projects", "status"} {
		uri := "fkf://brain/" + name
		result, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("ReadResource(%q) error = %v", uri, err)
		}
		if len(result.Contents) != 1 || result.Contents[0].Text == "" {
			t.Fatalf("ReadResource(%q) = %+v", uri, result.Contents)
		}
		if result.CacheScope != "private" {
			t.Fatalf("ReadResource(%q) cache scope = %q, want private", uri, result.CacheScope)
		}
		var decoded any
		if err := json.Unmarshal([]byte(result.Contents[0].Text), &decoded); err != nil {
			t.Fatalf("ReadResource(%q) is not JSON: %v", uri, err)
		}
	}
}

func TestWikiIndexResourceIncludesTheCuratedBody(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	result, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/wiki/index"})
	if err != nil {
		t.Fatal(err)
	}
	body := result.Contents[0].Text
	if !strings.Contains(body, "A decision") {
		t.Fatalf("wiki index resource omits its curated Markdown body: %s", body)
	}
}

func TestProjectsResourceIsCappedAtTheServerPageSize(t *testing.T) {
	base := newBase(t)
	for index := 0; index < mcpserver.PageSize+10; index++ {
		writeFile(t, base, fmt.Sprintf("projects/generated-%03d.md", index), fmt.Sprintf(`---
type: project
title: Generated %03d
status: active
tags: [generated]
---

# Generated %03d
`, index, index))
	}
	session := connect(t, base)
	result, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/projects"})
	if err != nil {
		t.Fatal(err)
	}
	var listing services.PageListing
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Pages) != mcpserver.PageSize || listing.Total != mcpserver.PageSize+11 {
		t.Fatalf("projects resource = %d returned / %d total, want %d / %d",
			len(listing.Pages), listing.Total, mcpserver.PageSize, mcpserver.PageSize+11)
	}
}

func TestIndexListHonoursTheRequestedAndServerLimits(t *testing.T) {
	base := newBase(t)
	source, _ := base.Source("synthetic")
	for index := 0; index < mcpserver.PageSize+10; index++ {
		name := fmt.Sprintf("generated-%03d", index)
		fields := sources.Fields{core.FieldID: source.Fields[core.FieldID]}
		document := &sources.Document{
			FKF: sources.SchemaVersion, Source: name, Layer: core.LayerIndex,
			CollectedAt: testClock.Format(time.RFC3339),
			Schema:      base.Config.Schema.Select(fields), Fields: fields, Count: 1,
			Records: []sources.Record{{"id": name}},
		}
		if err := base.WriteDocument(document); err != nil {
			t.Fatal(err)
		}
	}
	session := connect(t, base)
	for _, test := range []struct {
		limit int
		want  int
	}{{7, 7}, {mcpserver.PageSize + 1, mcpserver.PageSize}} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "list", Arguments: map[string]any{"layer": "index", "limit": test.limit},
		})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var listing struct {
			Entries []services.IndexEntry `json:"entries"`
			Total   int                   `json:"total"`
			Cursor  string                `json:"next_cursor"`
		}
		if err := json.Unmarshal(encoded, &listing); err != nil {
			t.Fatal(err)
		}
		if len(listing.Entries) != test.want || listing.Total != mcpserver.PageSize+10 {
			t.Fatalf("limit %d returned %d / %d index documents, want %d / %d",
				test.limit, len(listing.Entries), listing.Total, test.want, mcpserver.PageSize+10)
		}
		if listing.Cursor == "" {
			t.Fatalf("limit %d omitted next_cursor with %d documents remaining", test.limit, listing.Total-test.want)
		}
		secondResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "list", Arguments: map[string]any{
				"layer": "index", "limit": test.limit, "cursor": listing.Cursor,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var second struct {
			Entries []services.IndexEntry `json:"entries"`
		}
		decodeStructured(t, secondResult, &second)
		if len(second.Entries) == 0 || second.Entries[0].URI == listing.Entries[0].URI {
			t.Fatalf("second index page = %+v after %+v", second.Entries, listing.Entries)
		}
	}
}

func TestResourceRefusesAnOversizeResponse(t *testing.T) {
	base := newBase(t)
	// Many short tags make the vocabulary JSON much larger than the still-bounded Markdown
	// input: each one expands into a count object and a page list in the resource.
	var page strings.Builder
	page.WriteString("---\ntitle: Wiki\ntags:\n")
	for index := 0; index < 90_000; index++ {
		fmt.Fprintf(&page, "  - t%06d\n", index)
	}
	page.WriteString("---\n\n# Wiki\n")
	if page.Len() >= int(core.MaxNarrativeBytes) {
		t.Fatalf("oversize fixture input = %d, must remain below the narrative read bound", page.Len())
	}
	writeFile(t, base, "wiki/index.md", page.String())
	session := connect(t, base)
	_, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/wiki/tags"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "size limit") {
		t.Fatalf("ReadResource() error = %v, want an oversize refusal", err)
	}
}

func TestResourceRefusesAnEscapeExpandedWireResponse(t *testing.T) {
	base := newBase(t)
	page := "---\ntitle: Wiki\ntags: [wiki]\n---\n\n# Wiki\n\n" + strings.Repeat(`"`, 1_100_000)
	writeFile(t, base, "wiki/index.md", page)

	value, err := services.Read(t.Context(), base, "wiki/index.md", services.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: "fkf://brain/wiki/index", MIMEType: "application/json", Text: string(inner)},
	}}
	wire, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) >= int(core.MaxNarrativeBytes) || len(wire) <= int(core.MaxNarrativeBytes) {
		t.Fatalf("fixture inner = %d and wire = %d; want only JSON-string escaping to cross %d bytes",
			len(inner), len(wire), core.MaxNarrativeBytes)
	}

	session := connect(t, base)
	_, err = session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/wiki/index"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "size limit") {
		t.Fatalf("ReadResource() error = %v, want the oversized escaped wire response refused", err)
	}
}

func TestStatusResourceAndInstructionsNeverInvokeGit(t *testing.T) {
	base := newBase(t)
	if err := os.Mkdir(filepath.Join(base.Root(), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "git-ran")
	bin := t.TempDir()
	script := "#!/bin/sh\ntouch " + sentinel + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := mcpserver.Instructions(t.Context(), base); err != nil {
		t.Fatalf("Instructions() error = %v", err)
	}
	session := connect(t, base)
	uri := "fkf://brain/status"
	resource, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("ReadResource(%q) error = %v", uri, err)
	}
	if len(resource.Contents) != 1 || strings.Contains(resource.Contents[0].Text, base.Root()) {
		t.Fatalf("status resource discloses local base path %q: %+v", base.Root(), resource.Contents)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("the read-only MCP surface invoked git: %v", err)
	}
}

func TestStatusResourceRedactsLocalPathsFromFailureFindings(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	meta, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}

	resource, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/status"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 {
		t.Fatalf("status contents = %d, want one", len(resource.Contents))
	}
	body := resource.Contents[0].Text
	if strings.Contains(body, base.Root()) {
		t.Fatalf("status failure discloses local base root %q: %s", base.Root(), body)
	}
	if !strings.Contains(body, core.GraphMetaFile) {
		t.Fatalf("status failure = %s, want the actionable fkf URI", body)
	}
}

func TestStatusResourceRedactsAnExternalStateDirectoryFromErrors(t *testing.T) {
	base := newBase(t)
	externalState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", externalState)
	if _, err := core.WriteTrust(t.Context(), base.Config, testClock); err != nil {
		t.Fatal(err)
	}
	trustDir := filepath.Join(externalState, "fkf", "trust")
	entries, err := os.ReadDir(trustDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trust records = %v, error = %v; want one external record", entries, err)
	}
	if err := os.WriteFile(filepath.Join(trustDir, entries[0].Name()), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	session := connect(t, base)
	_, err = session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fkf://brain/status"})
	if err == nil {
		t.Fatal("status resource accepted a corrupt trust record")
	}
	if strings.Contains(err.Error(), externalState) || strings.Contains(err.Error(), base.Root()) {
		t.Fatalf("status error discloses a local path: %v", err)
	}
}

func TestToolErrorsRedactTheBaseRootAndKeepTheFKFURI(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	uri := "wiki/oversized.md"
	writeFile(t, base, uri, strings.Repeat("x", int(core.MaxNarrativeBytes)+1))

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": uri},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("oversized page read succeeded")
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	message := string(encoded)
	if strings.Contains(message, base.Root()) {
		t.Fatalf("tool error discloses local base root %q: %s", base.Root(), message)
	}
	if !strings.Contains(message, uri) {
		t.Fatalf("tool error = %s, want actionable fkf URI %q", message, uri)
	}
}

// TestResourcesOmitADisabledLayer is the regression test for a resource that stayed registered
// after its layer was turned off: reading it would fail on every call instead of the resource
// simply not existing, which is the same "an agent discovers the lie a turn late" cost
// Instructions already avoids for the enabled-layers sentence.
func TestResourcesOmitADisabledLayer(t *testing.T) {
	session := connect(t, narrowBase(t))
	resources, err := session.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(resources.Resources))
	for _, resource := range resources.Resources {
		names = append(names, resource.URI)
	}
	if strings.Join(names, ",") != "fkf://brain/projects,fkf://brain/status" {
		t.Fatalf("resources = %v, want only the layers narrowBase enables (projects, status), "+
			"never the wiki ones it turned off", names)
	}
}

// TestInstructionsDescribeTheBaseThatIsOpen is why they are generated: an agent told "the wiki
// is at wiki/" for a base with no wiki layer spends a turn discovering the lie.
func TestInstructionsDescribeTheBaseThatIsOpen(t *testing.T) {
	base := newBase(t)
	instructions, err := mcpserver.Instructions(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`the fkf base "brain"`, "events, index, tasks, projects, wiki",
		"1 source(s) enabled", "fkf://brain/status", mcpserver.UntrustedEvidenceNotice,
		"events/<date>/<source>.json#<id>", "?jq=", "lowercase <scheme>:<identity>",
		"fkf://brain/wiki/index", "fkf://brain/wiki/tags",
	} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions omit %q:\n%s", want, instructions)
		}
	}
	if strings.Contains(instructions, "Last collected day:") {
		t.Fatalf("instructions performed collection-health work reserved for the status resource:\n%s", instructions)
	}
	if strings.Contains(instructions, base.Root()) {
		t.Fatalf("instructions disclose the local base path %q:\n%s", base.Root(), instructions)
	}
	if len(instructions) > 4096 {
		t.Fatalf("instructions are %d bytes; they are prepended to every session", len(instructions))
	}
	// They must not name a layer this base does not enable.
	narrow := narrowBase(t)
	limited, err := mcpserver.Instructions(t.Context(), narrow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(limited, "events, index, tasks, projects, wiki") {
		t.Fatalf("instructions claim layers the base disabled:\n%s", limited)
	}
	if strings.Contains(limited, "/wiki/index") || strings.Contains(limited, "/wiki/tags") {
		t.Fatalf("instructions advertise resources the base does not register:\n%s", limited)
	}
}

// TestInstructionsStayBoundedAtTheLargestValidAuthority pins the interaction between the
// configuration name bound, exact registered resource URIs, and the 4 KiB MCP context budget.
func TestInstructionsStayBoundedAtTheLargestValidAuthority(t *testing.T) {
	root := t.TempDir()
	name := strings.Repeat("x", core.MaxBaseNameLength)
	config := strings.Replace(testConfig, "name: brain", "name: "+name, 1)
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := services.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	base.Now = func() time.Time { return testClock }

	instructions, err := mcpserver.Instructions(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(instructions) > 4096 {
		t.Fatalf("instructions are %d bytes; they are prepended to every session", len(instructions))
	}
	for _, want := range []string{
		mcpserver.UntrustedEvidenceNotice,
		"events/<date>/<source>.json#<id>",
		"?jq=",
		"fkf://" + name + "/wiki/index",
		"fkf://" + name + "/wiki/tags",
	} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("maximum-size authority instructions omit %q:\n%s", want, instructions)
		}
	}
}

func narrowBase(t *testing.T) *services.Base {
	t.Helper()
	root := t.TempDir()
	config := strings.Replace(testConfig, "  wiki: true", "  wiki: false", 1)
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := services.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	base.Now = func() time.Time { return testClock }
	return base
}

// TestLoggingNeverCarriesEvidence is the rule a server log has to obey: it says what was asked
// and how much came back, never what the base holds. A log that quotes records is a second,
// unmanaged copy of the base.
func TestLoggingNeverCarriesEvidence(t *testing.T) {
	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	session := connect(t, newBase(t))
	for _, call := range []struct {
		tool      string
		arguments map[string]any
	}{
		{"find", map[string]any{"grep": []string{"FK-412"}}},
		{"context", map[string]any{"query": "FK-412"}},
		{"read", map[string]any{"uri": "events/2026-05-04/synthetic.json#a1"}},
		{"graph", map[string]any{"uri": "ticket:FK-412"}},
		{"list", map[string]any{"layer": "wiki"}},
	} {
		if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: call.tool, Arguments: call.arguments}); err != nil {
			t.Fatal(err)
		}
	}
	logged := captured.String()
	if !strings.Contains(logged, "fkf mcp call") {
		t.Fatalf("nothing was logged:\n%s", logged)
	}
	// Every one of these appears in the base's records and pages, and none may appear in a log.
	for _, evidence := range []string{
		"Fix FK-412", "marc@example.test", "A decision", "fmind/fkf", "a-decision.md",
	} {
		if strings.Contains(logged, evidence) {
			t.Fatalf("the log carries record content %q:\n%s", evidence, logged)
		}
	}
	for _, wanted := range []string{"tool=find", "items=", "elapsed_ms=", "input_digest=", "bytes="} {
		if !strings.Contains(logged, wanted) {
			t.Fatalf("the log omits %q, so a call cannot be accounted for:\n%s", wanted, logged)
		}
	}
}

func TestListRefusesADisabledLayer(t *testing.T) {
	session := connect(t, narrowBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki"},
	})
	if err == nil && !result.IsError {
		t.Fatal("listing a disabled layer must be refused, not reported empty")
	}
}

// TestReadingAnEntityOverMCPNeverExecutes guards the entity-read boundary. An entity URI is an
// offline graph node on both CLI and MCP; only a collected record may use the CLI-only --body
// path. The base's runner fails the test if anything executes, so this asserts the property
// rather than the plumbing.
func TestReadingAnEntityOverMCPNeverExecutes(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": "person:marc@example.test"},
	})
	if err != nil {
		t.Fatalf("read person: %v", err)
	}
	if result.IsError {
		t.Fatalf("read person returned an error result: %v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"body"`) {
		t.Fatalf("an MCP read must never carry a resolved body, got %s", encoded)
	}
}

// TestAFailedCallLogsAClassNotTheEvidence is the half TestLoggingNeverCarriesEvidence does not
// reach: every call it makes succeeds, and the leak was on the error path. gojq puts the value
// it failed on into its own message, so `?jq=.records[0].title|tonumber` came back as
// `tonumber cannot be applied to "Fix FK-412 ...": invalid number` — a collected record's title,
// written verbatim to the server log by a string the connected agent chose. Slicing the field
// walks any record into the log twenty-four characters at a time.
func TestAFailedCallLogsAClassNotTheEvidence(t *testing.T) {
	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	session := connect(t, newBase(t))
	for _, uri := range []string{
		"events/2026-05-04/synthetic.json?jq=.records[0].title|tonumber",
		"events/2026-05-04/synthetic.json?jq=.records[0].title[0:24]|tonumber",
		"events/2026-05-04/synthetic.json?jq=.records[0]|has(\"x\")|tonumber",
		"../../etc/passwd",
		"events/2026-05-04/missing.json",
	} {
		// The call is expected to fail; what matters is what the failure wrote to the log.
		_, _ = session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "read", Arguments: map[string]any{"uri": uri},
		})
	}
	logged := captured.String()
	if !strings.Contains(logged, "fkf mcp call failed") {
		t.Fatalf("no failure was logged, so this test proves nothing:\n%s", logged)
	}
	for _, evidence := range []string{
		"Fix FK-412", "marc@example.test", "A decision", "tonumber", "cannot be applied",
	} {
		if strings.Contains(logged, evidence) {
			t.Fatalf("a failed call put %q in the log; the log must carry the class, not the evidence:\n%s",
				evidence, logged)
		}
	}
	// And it still accounts for the failure well enough to debug one.
	for _, wanted := range []string{"error=path-escapes", "error=not-found", "input_digest="} {
		if !strings.Contains(logged, wanted) {
			t.Fatalf("the log omits %q, so a failure cannot be accounted for:\n%s", wanted, logged)
		}
	}
}

// TestFindReturnsTheSameOrderOverMCPAsOnTheCLI pins ordering to the service. While `fkf find`
// sorted its own copy of the records and the MCP handler did not, the same query answered two
// clients two ways — and every client of a base that claims "same query, same base, same binary,
// same answer" has to get the same list.
func TestFindReturnsTheSameOrderOverMCPAsOnTheCLI(t *testing.T) {
	base := newBase(t)
	// The shared fixture holds one record, and one record has no order. Collecting a second day
	// with three out-of-order times is what makes the property observable at all.
	source, _ := base.Source("synthetic")
	day, _ := sources.ParseDay("2026-05-05")
	document, err := sources.Collect(t.Context(),
		sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
			return `[{"id":"b1","t":"2026-05-05T09:00:00Z","subject":"FK-412 morning","repo":"fmind/fkf","who":"marc@example.test"},
			         {"id":"b3","t":"2026-05-05T17:00:00Z","subject":"FK-412 evening","repo":"fmind/fkf","who":"marc@example.test"},
			         {"id":"b2","t":"2026-05-05T13:00:00Z","subject":"FK-412 midday","repo":"fmind/fkf","who":"marc@example.test"}]`, nil
		}), source, base.Env, sources.DayWindow(day), time.Minute, testClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}

	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{"grep": []string{"FK-412"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Records []struct {
			Time string `json:"time"`
			URI  string `json:"uri"`
		} `json:"records"`
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Records) < 3 {
		t.Fatalf("the base returned %d record(s); the fixture is not the one this test wrote", len(decoded.Records))
	}
	for index := 1; index < len(decoded.Records); index++ {
		previous, current := decoded.Records[index-1], decoded.Records[index]
		if previous.Time < current.Time ||
			(previous.Time == current.Time && previous.URI > current.URI) {
			t.Fatalf("records are not newest-first over MCP: %+v", decoded.Records)
		}
	}
}

// TestFindCountIsCappedWithoutShorteningTheScan keeps count mode inside the same 100-item
// response contract as every other MCP listing. The returned volumes are bounded, while the
// counters and selected-day metadata still describe the complete requested window.
func TestFindCountIsCappedWithoutShorteningTheScan(t *testing.T) {
	base := newBase(t)
	source, err := base.Source("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for offset := range mcpserver.PageSize + 1 {
		date := first.AddDate(0, 0, offset).Format(time.DateOnly)
		day, err := sources.ParseDay(date)
		if err != nil {
			t.Fatal(err)
		}
		document, err := sources.Collect(t.Context(),
			sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
				return fmt.Sprintf(
					`[{"id":"r-%d","t":"%sT09:00:00Z","subject":"bounded count"}]`,
					offset, date), nil
			}), source, base.Env, sources.DayWindow(day), time.Minute, testClock)
		if err != nil {
			t.Fatal(err)
		}
		if err := base.WriteDocument(document); err != nil {
			t.Fatal(err)
		}
	}

	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{"count": true, "since": "2026-01-01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("find count returned an error: %+v", result.Content)
	}
	var decoded struct {
		services.FindResult
		NextCursor string `json:"next_cursor"`
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	wantDays := mcpserver.PageSize + 2 // The shared fixture already contains 2026-05-04.
	if len(decoded.Volumes) != mcpserver.PageSize || !decoded.Truncated || decoded.NextCursor == "" {
		t.Fatalf("volumes = %d, truncated = %t; want %d and true",
			len(decoded.Volumes), decoded.Truncated, mcpserver.PageSize)
	}
	if decoded.Scanned != wantDays || decoded.Matched != wantDays {
		t.Fatalf("scanned = %d, matched = %d; want the complete %d-day scan",
			decoded.Scanned, decoded.Matched, wantDays)
	}
	if len(decoded.Days) != 0 {
		t.Fatalf("days metadata = %d, want it omitted from the bounded MCP result", len(decoded.Days))
	}
	// A cursor is self-contained rather than tied to one stdio session.
	continuedSession := connect(t, base)
	secondResult, err := continuedSession.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{
			"count": true, "since": "2026-01-01", "cursor": decoded.NextCursor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var second struct {
		services.FindResult
		NextCursor string `json:"next_cursor"`
	}
	decodeStructured(t, secondResult, &second)
	if len(second.Volumes) != 2 || second.Truncated || second.NextCursor != "" ||
		second.Scanned != wantDays || second.Matched != wantDays || len(second.Days) != 0 {
		t.Fatalf("second count page = %+v, want the final two volumes with full counters", second)
	}
}

func TestFindCursorContinuesPagesThenRecordsUnderOnePageCap(t *testing.T) {
	base := newBase(t)
	writeFile(t, base, "wiki/needle-one.md", "---\ntype: note\ntitle: Needle one\ntags: [test]\n---\n\n# Needle one\n")
	writeFile(t, base, "wiki/needle-two.md", "---\ntype: note\ntitle: Needle two\ntags: [test]\n---\n\n# Needle two\n")
	collectSynthetic(t, base, "2026-05-05", `[
		{"id":"b1","t":"2026-05-05T09:00:00Z","subject":"needle one"},
		{"id":"b2","t":"2026-05-05T10:00:00Z","subject":"needle two"}
	]`)
	session := connect(t, base)
	type findPage struct {
		services.FindResult
		NextCursor string `json:"next_cursor"`
	}
	arguments := map[string]any{"grep": []string{"needle"}, "limit": 1}
	var pages, records []string
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("find cursor did not terminate")
		}
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "find", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		var page findPage
		decodeStructured(t, result, &page)
		if len(page.Pages)+len(page.Records) > 1 {
			t.Fatalf("find page returned %d pages and %d records with total limit 1", len(page.Pages), len(page.Records))
		}
		for _, item := range page.Pages {
			pages = append(pages, item.URI)
		}
		for _, item := range page.Records {
			records = append(records, item.URI)
		}
		if page.NextCursor == "" {
			if page.Truncated {
				t.Fatal("final find page is marked truncated")
			}
			break
		}
		if !page.Truncated {
			t.Fatal("find returned next_cursor without truncated")
		}
		arguments = map[string]any{"grep": []string{"needle"}, "limit": 1, "cursor": page.NextCursor}
	}
	if len(pages) != 2 || len(records) != 2 {
		t.Fatalf("continued find returned pages=%v records=%v", pages, records)
	}
	seen := map[string]bool{}
	for _, uri := range append(pages, records...) {
		if seen[uri] {
			t.Fatalf("find repeated %s across pages", uri)
		}
		seen[uri] = true
	}
}

func TestFindRequiresEveryTermForMarkdownPages(t *testing.T) {
	base := newBase(t)
	writeFile(t, base, "wiki/alpha-only.md", "---\ntype: note\ntitle: Alpha only\ntags: [test]\n---\n\n# Alpha\n")
	writeFile(t, base, "wiki/alpha-beta.md", "---\ntype: note\ntitle: Alpha beta\ntags: [test]\n---\n\n# Alpha beta\n")
	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{"grep": []string{"alpha", "beta"}, "limit": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		services.FindResult
		NextCursor string `json:"next_cursor"`
	}
	decodeStructured(t, result, &page)
	if len(page.Pages) != 1 || page.Pages[0].URI != "wiki/alpha-beta.md" || page.NextCursor != "" {
		t.Fatalf("multi-term MCP page = %+v, want only the page matching every term", page)
	}
}

func TestFindRejectsBlankGrepInsteadOfScanningTheWholeBase(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{"grep": []string{" \t "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(errorText(result), "non-whitespace") {
		t.Fatalf("blank-grep result = %+v, want a configuration refusal", result)
	}
}

func TestFindCursorBindsARelativeWindowToItsResolvedDay(t *testing.T) {
	base := newBase(t)
	writeFile(t, base, "wiki/needle-one.md", "---\ntype: note\ntitle: Needle one\ntags: [test]\n---\n\n# Needle one\n")
	writeFile(t, base, "wiki/needle-two.md", "---\ntype: note\ntitle: Needle two\ntags: [test]\n---\n\n# Needle two\n")
	day := testClock
	base.Now = func() time.Time { return day }
	session := connect(t, base)
	firstResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{
			"grep": []string{"needle"}, "since": "today", "limit": 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		services.FindResult
		NextCursor string `json:"next_cursor"`
	}
	decodeStructured(t, firstResult, &first)
	if first.NextCursor == "" {
		t.Fatal("first relative-window find page returned no continuation")
	}

	day = day.AddDate(0, 0, 1)
	continued, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "find", Arguments: map[string]any{
			"grep": []string{"needle"}, "since": "today", "limit": 1, "cursor": first.NextCursor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !continued.IsError || !strings.Contains(errorText(continued), "effective query") {
		t.Fatalf("next-day continuation = %+v, want a resolved-window query mismatch", continued)
	}
}

func TestListCursorIsSnapshotBound(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	type listPage struct {
		services.PageListing
		NextCursor string `json:"next_cursor"`
	}
	firstResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki", "limit": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var first listPage
	decodeStructured(t, firstResult, &first)
	if len(first.Pages) != 1 || first.NextCursor == "" || first.Total != 2 {
		t.Fatalf("first list page = %+v, want one of two pages and a cursor", first)
	}
	repeatResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki", "limit": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var repeat listPage
	decodeStructured(t, repeatResult, &repeat)
	if repeat.NextCursor != first.NextCursor || !reflect.DeepEqual(repeat.PageListing, first.PageListing) {
		t.Fatalf("repeated first page = %+v, want byte-stable data and cursor from %+v", repeat, first)
	}

	secondResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki", "limit": 1, "cursor": first.NextCursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	var second listPage
	decodeStructured(t, secondResult, &second)
	if len(second.Pages) != 1 || second.NextCursor != "" || second.Total != 2 ||
		second.Pages[0].URI == first.Pages[0].URI {
		t.Fatalf("second list page = %+v after first %+v", second, first)
	}

	changedArguments := map[string]any{"layer": "wiki", "limit": 2, "cursor": first.NextCursor}
	changedResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list", Arguments: changedArguments})
	if err != nil {
		t.Fatal(err)
	}
	if !changedResult.IsError || !strings.Contains(errorText(changedResult), "effective query") {
		t.Fatalf("changed-query result = %+v, want a cursor query mismatch", changedResult)
	}

	writeFile(t, base, "wiki/new-page.md", "---\ntype: note\ntitle: New page\ntags: [new]\n---\n\n# New page\n")
	staleResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"layer": "wiki", "limit": 1, "cursor": first.NextCursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !staleResult.IsError || !strings.Contains(errorText(staleResult), "stale") {
		t.Fatalf("changed-snapshot result = %+v, want a stale cursor error", staleResult)
	}
}

func TestReadDirectoryCursorReturnsEveryEntryOnce(t *testing.T) {
	base := newBase(t)
	for index := range mcpserver.PageSize + 1 {
		writeFile(t, base, fmt.Sprintf("wiki/page-%03d.md", index), "# Page\n")
	}
	session := connect(t, base)
	type readPage struct {
		services.ReadResult
		NextCursor string `json:"next_cursor"`
	}
	arguments := map[string]any{"uri": "wiki/"}
	var entries []string
	for calls := 0; ; calls++ {
		if calls > 3 {
			t.Fatal("directory cursor did not terminate")
		}
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "read", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		var page readPage
		decodeStructured(t, result, &page)
		if len(page.Entries) > mcpserver.PageSize {
			t.Fatalf("directory page has %d entries", len(page.Entries))
		}
		entries = append(entries, page.Entries...)
		if page.NextCursor == "" {
			break
		}
		arguments = map[string]any{"uri": "wiki/", "cursor": page.NextCursor}
	}
	want := mcpserver.PageSize + 3 // The shared index and decision pages plus this test's pages.
	if len(entries) != want || !sort.StringsAreSorted(entries) {
		t.Fatalf("directory entries = %d sorted=%t, want %d sorted entries", len(entries), sort.StringsAreSorted(entries), want)
	}
	for index := 1; index < len(entries); index++ {
		if entries[index] == entries[index-1] {
			t.Fatalf("directory repeated %s across pages", entries[index])
		}
	}
}

func TestReadRejectsACursorForAnUnpagedResult(t *testing.T) {
	const uri = "wiki/a-decision.md"
	query, err := json.Marshal(mcpserver.ReadInput{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(query)
	payload, err := json.Marshal(map[string]any{
		"v": 1, "tool": "read", "query_sha256": fmt.Sprintf("%x", digest[:]),
		"snapshot_sha256": strings.Repeat("a", 64), "offset": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor := base64.RawURLEncoding.EncodeToString(payload)
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": uri, "cursor": cursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(errorText(result), "only to directories and entities") {
		t.Fatalf("read result = %+v, want the cursor refused for a page", result)
	}
}

type graphCursorPage struct {
	services.Neighbourhood
	NextCursor string `json:"next_cursor"`
}

type entityCursorPage struct {
	services.ReadResult
	NextCursor string `json:"next_cursor"`
}

func newPagedGraphBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t)
	records := make([]map[string]any, 0, mcpserver.PageSize+1)
	for index := range mcpserver.PageSize + 1 {
		records = append(records, map[string]any{
			"id": fmt.Sprintf("repo-%03d", index), "t": "2026-05-05T09:00:00Z",
			"subject":        fmt.Sprintf("Repository fact %03d", index),
			"repository_uri": "repo:github.com/fmind/fkf", "participant_uris": []string{},
		})
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	collectSynthetic(t, base, "2026-05-05", string(encoded))
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	return base
}

func recordGraphCursorPage(
	t *testing.T,
	page graphCursorPage,
	seenEdges map[string]bool,
	seenNodes map[string]bool,
) {
	t.Helper()
	if len(page.Edges) > 7 || len(page.Nodes) > len(page.Edges) {
		t.Fatalf("graph page has %d edges and %d newly discovered nodes", len(page.Edges), len(page.Nodes))
	}
	for _, edge := range page.Edges {
		key := edge.Src + "\x00" + edge.Dst + "\x00" + edge.Kind
		if seenEdges[key] {
			t.Fatalf("graph repeated edge %+v across pages", edge)
		}
		seenEdges[key] = true
	}
	for _, node := range page.Nodes {
		if seenNodes[node] {
			t.Fatalf("graph repeated discovered node %s across pages", node)
		}
		seenNodes[node] = true
	}
}

func walkGraphCursor(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	arguments := map[string]any{
		"uri": "repo:github.com/fmind/fkf", "kind": "repository", "limit": 7,
	}
	seenEdges, seenNodes := map[string]bool{}, map[string]bool{}
	var firstGraphCursor string
	for calls := 0; ; calls++ {
		if calls > 20 {
			t.Fatal("graph cursor did not terminate")
		}
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "graph", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		var page graphCursorPage
		decodeStructured(t, result, &page)
		if calls == 0 {
			firstGraphCursor = page.NextCursor
		}
		recordGraphCursorPage(t, page, seenEdges, seenNodes)
		if page.NextCursor == "" {
			if page.Truncated {
				t.Fatal("final graph page is marked truncated")
			}
			break
		}
		if !page.Truncated {
			t.Fatal("graph returned next_cursor without truncated")
		}
		arguments = map[string]any{
			"uri": "repo:github.com/fmind/fkf", "kind": "repository", "limit": 7,
			"cursor": page.NextCursor,
		}
	}
	wantEdges := mcpserver.PageSize + 2 // The shared fixture contributes one more repository edge.
	if len(seenEdges) != wantEdges || len(seenNodes) != wantEdges {
		t.Fatalf("continued graph has %d edges and %d nodes, want %d of each", len(seenEdges), len(seenNodes), wantEdges)
	}
	return firstGraphCursor
}

func assertEntityCursor(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	firstEntityResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": "repo:github.com/fmind/fkf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var firstEntity entityCursorPage
	decodeStructured(t, firstEntityResult, &firstEntity)
	if firstEntity.Entity == nil || len(firstEntity.Entity.Neighbours) != mcpserver.PageSize ||
		firstEntity.NextCursor == "" || !firstEntity.Entity.NeighbourCap {
		t.Fatalf("first entity page = %+v, want %d neighbours and a cursor", firstEntity, mcpserver.PageSize)
	}
	secondEntityResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{
			"uri": "repo:github.com/fmind/fkf", "cursor": firstEntity.NextCursor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondEntity entityCursorPage
	decodeStructured(t, secondEntityResult, &secondEntity)
	if secondEntity.Entity == nil || len(secondEntity.Entity.Neighbours) != 2 ||
		secondEntity.NextCursor != "" || secondEntity.Entity.NeighbourCap {
		t.Fatalf("second entity page = %+v, want the final two neighbours", secondEntity)
	}
}

func TestGraphAndEntityCursorsUseTheValidatedGraphSnapshot(t *testing.T) {
	base := newPagedGraphBase(t)
	session := connect(t, base)
	firstGraphCursor := walkGraphCursor(t, session)
	assertEntityCursor(t, session)

	writeFile(t, base, "wiki/changes-graph.md", "---\ntype: note\ntitle: Changes graph\ntags: [changed]\n---\n\n# Changes graph\n")
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	staleResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "graph", Arguments: map[string]any{
			"uri": "repo:github.com/fmind/fkf", "kind": "repository", "limit": 7,
			"cursor": firstGraphCursor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !staleResult.IsError || !strings.Contains(errorText(staleResult), "stale") {
		t.Fatalf("rebuilt-graph result = %+v, want a stale cursor error", staleResult)
	}
}

// TestToolResultCarriesTheSameJSONForOldAndNewClients keeps both MCP compatibility paths
// useful. Clients predating structured tool output read TextContent; current clients read
// StructuredContent. Both must receive the complete compact JSON, not a summary in one path.
func TestToolResultCarriesTheSameJSONForOldAndNewClients(t *testing.T) {
	session := connect(t, newBase(t))
	for _, call := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "find", arguments: map[string]any{"grep": []string{"FK-412"}}},
		{name: "context", arguments: map[string]any{"query": "FK-412"}},
		{name: "list", arguments: map[string]any{"layer": "wiki"}},
		{name: "read", arguments: map[string]any{"uri": "wiki/a-decision.md"}},
		{name: "graph", arguments: map[string]any{"uri": "ticket:FK-412"}},
	} {
		t.Run(call.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: call.name, Arguments: call.arguments})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || result.StructuredContent == nil {
				t.Fatalf("result = %+v, want complete StructuredContent", result)
			}
			if len(result.Content) != 1 {
				t.Fatalf("Content = %+v, want exactly one complete JSON text block", result.Content)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("Content[0] = %T, want *mcp.TextContent", result.Content[0])
			}
			var textJSON any
			if err := json.Unmarshal([]byte(text.Text), &textJSON); err != nil {
				t.Fatalf("TextContent is not complete JSON: %v", err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(text.Text)); err != nil || compact.String() != text.Text {
				t.Fatalf("TextContent is not compact JSON: %v", err)
			}
			if !reflect.DeepEqual(textJSON, result.StructuredContent) {
				structured, _ := json.Marshal(result.StructuredContent)
				t.Fatalf("TextContent = %q, StructuredContent = %s; want equivalent complete JSON",
					text.Text, structured)
			}
			wire, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) > int(core.MaxNarrativeBytes) {
				t.Fatalf("CallToolResult is %d bytes, over the combined %d-byte bound", len(wire), core.MaxNarrativeBytes)
			}
		})
	}
}

// TestReadRefusesAnOversizedResponse proves the bound covers both complete JSON representations,
// not just the structured half. The document fits by itself and a direct stored read succeeds;
// only the MCP result containing both client paths crosses four MiB.
func TestReadRefusesAnOversizedResponse(t *testing.T) {
	base := newBase(t)
	const largeRecords = 20000
	huge := make([]map[string]string, 0, largeRecords)
	for i := range largeRecords {
		huge = append(huge, map[string]string{
			"id": fmt.Sprintf("r%d", i), "t": "2026-05-06T09:00:00Z",
			"subject": "padding to push this document past the MCP response bound",
		})
	}
	document := map[string]any{
		"fkf": 1, "source": "synthetic", "layer": "events", "date": "2026-05-06",
		"collected_at": "2026-05-06T09:00:00Z", "command": "synthetic",
		"schema": map[string]any{
			"id":   map[string]any{"description": "Stable record identity.", "cardinality": "one"},
			"time": map[string]any{"description": "Event time.", "cardinality": "one"},
		},
		"fields": map[string]any{"id": ".id", "time": ".t"}, "body": false,
		"count": len(huge), "records": huge,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, "events/2026-05-06/synthetic.json", string(encoded))
	direct, err := services.Read(t.Context(), base, "events/2026-05-06/synthetic.json", services.ReadOptions{})
	if err != nil {
		t.Fatalf("the single stored result must fit before MCP duplicates it: %v", err)
	}
	single, err := json.Marshal(direct)
	if err != nil {
		t.Fatal(err)
	}
	if len(single) >= int(core.MaxNarrativeBytes) || len(single)*2 <= int(core.MaxNarrativeBytes) {
		t.Fatalf("fixture single result = %d bytes; want one copy below and two copies above %d",
			len(single), core.MaxNarrativeBytes)
	}

	session := connect(t, base)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"uri": "events/2026-05-06/synthetic.json"},
	})
	// MCP reports a handler failure in-band (IsError on a normal result), not as a transport-level
	// Go error, so the tool call itself succeeds and the refusal has to be read from the content.
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("CallTool() succeeded on an oversized document, want it refused before crossing the wire")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || (!strings.Contains(text.Text, "jq") && !strings.Contains(text.Text, "byte")) {
		t.Fatalf("error content = %+v, want it to name the bound and how to narrow the read", result.Content)
	}
}

func TestToolErrorsStayWithinResponseBound(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	oversized := strings.Repeat("a", int(core.MaxNarrativeBytes)+1)
	tests := []struct {
		name string
		call *mcp.CallToolParams
	}{
		{"handler error", &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"uri": "x://" + oversized}}},
		{"schema error", &mcp.CallToolParams{Name: "list", Arguments: map[string]any{"layer": "wiki", "limit": oversized}}},
		{"protocol error", &mcp.CallToolParams{Name: oversized, Arguments: map[string]any{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), test.call)
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatal("malformed oversized call did not return a tool error")
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > int(core.MaxNarrativeBytes) {
				t.Fatalf("encoded error result = %d bytes, over the %d-byte MCP response limit",
					len(encoded), core.MaxNarrativeBytes)
			}
			if strings.Contains(string(encoded), oversized) {
				t.Fatal("bounded error echoed the complete oversized input")
			}
		})
	}
}
