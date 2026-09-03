package main

import (
	"bytes"
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
