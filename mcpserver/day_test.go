package mcpserver_test

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/services"
)

func TestDayAndTimelineToolsAnswerFromStoredEvents(t *testing.T) {
	base := newBase(t)
	session := connect(t, base)
	dayResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "day", Arguments: map[string]any{"date": "2026-05-04", "budget": 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	var day services.TimelineReport
	decodeStructured(t, dayResult, &day)
	if day.Receipt.Records != 1 || len(day.Groups) != 1 || day.Groups[0].Items[0].Title != "Fix FK-412" {
		t.Fatalf("day = %+v", day)
	}
	text, ok := dayResult.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("day content = %T, want compact JSON text", dayResult.Content[0])
	}
	if day.Receipt.Format != services.DigestDeliveryCompactJSON {
		t.Fatalf("day receipt format = %q, want compact MCP JSON", day.Receipt.Format)
	}
	if want := (len(text.Text) + 3) / 4; day.Receipt.UsedTokens != want {
		t.Fatalf("day used_tokens = %d, want %d for %d compact JSON bytes",
			day.Receipt.UsedTokens, want, len(text.Text))
	}
	var compact map[string]any
	if err := json.Unmarshal([]byte(text.Text), &compact); err != nil {
		t.Fatalf("day text content is not compact JSON: %v", err)
	}

	timelineResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "timeline", Arguments: map[string]any{
			"since": "2026-05-01", "until": "2026-05-05",
			"repo": "repo:github.com/fmind/fkf", "person": "person:email/marc@example.test",
			"source": []string{"synthetic"}, "budget": 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var timeline services.TimelineReport
	decodeStructured(t, timelineResult, &timeline)
	if timeline.Receipt.Records != 1 || timeline.Receipt.Repository != "repo:github.com/fmind/fkf" ||
		timeline.Receipt.Person != "person:email/marc@example.test" {
		t.Fatalf("timeline = %+v", timeline)
	}

	aroundResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "timeline", Arguments: map[string]any{
			"uri": "events/2026-05-04/synthetic.json#a1", "around": "2h", "budget": 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decodeStructured(t, aroundResult, &timeline)
	if timeline.Receipt.AroundWindow != "2h0m0s" || timeline.Receipt.Records != 1 {
		t.Fatalf("around timeline = %+v", timeline)
	}
}

func TestTimelineToolRejectsMalformedAroundDuration(t *testing.T) {
	session := connect(t, newBase(t))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "timeline", Arguments: map[string]any{
			"uri": "events/2026-05-04/synthetic.json#a1", "around": "soon", "budget": 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("timeline malformed duration = %+v, want an error result", result)
	}
}
