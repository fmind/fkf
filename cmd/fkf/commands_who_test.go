package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestWhoCommandAndTextRenderer(t *testing.T) {
	command := newWhoCommand()
	if command.Name != "who" || command.ArgsUsage != "<name|uri>" || command.Action == nil {
		t.Fatalf("newWhoCommand() = %+v", command)
	}
	report := &services.WhoReport{Query: "maxime", Matches: []services.WhoMatch{{
		Canonical: "person:email/maxime@example.com", Kind: "person",
		Names: []string{"Maxime Cordy"}, Aliases: []string{"actor:github.com/maxime"},
		Counts:                 []services.SourceCount{{Source: "mail", Count: 2}},
		NeighbourhoodTruncated: true,
		Recent: []services.FindRecord{{
			URI: "events/2026-09-01/mail.json#1", Source: "mail", Time: "2026-09-01T12:00:00Z", Title: "Hello",
		}},
		Total: 2,
	}}}
	var output bytes.Buffer
	if err := renderWhoText(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"person:email/maxime@example.com [person]", "names: Maxime Cordy", "source: mail · 2",
		"neighbourhood: truncated at 200 edges", "events/2026-09-01/mail.json#1", "total: 2 interaction(s)",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("text output %q does not contain %q", output.String(), expected)
		}
	}
}

// A well-connected identity carries hundreds of record neighbours in one kind, and the renderer
// used to join every one of them onto a single line. The line is now bounded and says how many
// it did not name.
func TestWhoTextBoundsOneKindsNeighbourhoodLine(t *testing.T) {
	nodes := make([]string, 0, 200)
	for index := range 200 {
		nodes = append(nodes, fmt.Sprintf("events/2026-09-01/github-issues.json#%d", index))
	}
	report := &services.WhoReport{Query: "busy", Matches: []services.WhoMatch{{
		Canonical: "person:email/busy@example.com", Kind: "person",
		Neighbourhood: []services.WhoNeighbourGroup{
			{Kind: "event", Nodes: nodes},
			{Kind: "wiki", Nodes: []string{"wiki/one.md", "wiki/two.md"}},
		},
		Total: 200,
	}}}
	var output bytes.Buffer
	if err := renderWhoText(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if len(line) > 512 {
			t.Fatalf("who text line is %d bytes, want a bounded rendering:\n%s", len(line), line)
		}
	}
	for _, want := range []string{"+192 more", "wiki: wiki/one.md, wiki/two.md"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("who text %q does not contain %q", output.String(), want)
		}
	}
	if strings.Contains(output.String(), "github-issues.json#199") {
		t.Error("who text still renders every neighbour")
	}
}
