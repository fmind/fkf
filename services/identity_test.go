package services

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestIdentityResolverMergesPageAliasesTransitively(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime, actor:github.com/maxime]
    kind: person
  owner:
    canonical: person:email/owner@example.com
    aliases: [owner]
    kind: person
    owner: true
`)
	writeIdentityPage(t, base, "wiki/maxime.md", `---
type: person
title: Maxime Cordy
aliases: [actor:github.com/maxime, maxime@work.example]
---
# Maxime Cordy
`)
	writeIdentityPage(t, base, "wiki/maxime-profile.md", `---
type: person
title: MC
aliases: [maxime@work.example]
---
# MC
`)

	resolver, err := LoadIdentityResolver(context.Background(), base)
	if err != nil {
		t.Fatalf("LoadIdentityResolver() error = %v", err)
	}
	for _, alias := range []string{"maxime", "MAXIME@WORK.EXAMPLE", "Maxime Cordy", "wiki/maxime-profile.md"} {
		if got := resolver.Canonical(alias); got != "person:email/maxime@example.com" {
			t.Errorf("Canonical(%q) = %q", alias, got)
		}
	}
	identity, ok := resolver.Exact("Maxime Cordy")
	if !ok || strings.Join(identity.Pages, ",") != "wiki/maxime-profile.md,wiki/maxime.md" {
		t.Fatalf("Exact(Maxime Cordy) = %+v, %v", identity, ok)
	}
	if !resolver.IsOwner("owner") || resolver.IsOwner("maxime") {
		t.Fatalf("owner resolution is wrong")
	}
}

func TestIdentityResolverRejectsAliasesOnOtherPageTypes(t *testing.T) {
	base := identityTestBase(t, "")
	writeIdentityPage(t, base, "wiki/tool.md", "---\ntype: tool\ntitle: Tool\naliases: [tool]\n---\n")
	_, err := LoadIdentityResolver(context.Background(), base)
	if err == nil || !strings.Contains(err.Error(), "aliases require type person or organization") {
		t.Fatalf("LoadIdentityResolver() error = %v", err)
	}
}

func TestBuildGraphCanonicalizesDeclaredIdentityAliases(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [maxime, actor:github.com/maxime]
    kind: person
`)
	writeIdentityPage(t, base, "wiki/maxime.md", "---\ntype: person\ntitle: Maxime Cordy\naliases: [actor:github.com/maxime]\n---\n")
	writeIdentityPage(t, base, "wiki/interaction.md", "---\ntype: note\ntitle: Interaction\nrelations:\n  participant: [actor:github.com/maxime]\n---\n")
	if _, err := BuildGraph(context.Background(), base); err != nil {
		t.Fatalf("BuildGraph() error = %v", err)
	}
	data, err := base.ReadFileContext(t.Context(), core.GraphFile, core.MaxSourceDocumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	var edges []Edge
	if _, err := ScanEdges(context.Background(), bytes.NewReader(data), EdgeQuery{}, func(edge Edge) error {
		edges = append(edges, edge)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []Edge{
		{Src: "wiki/interaction.md", Dst: "person:email/maxime@example.com", Kind: "participant", Via: "frontmatter:relations.participant"},
		{Src: "actor:github.com/maxime", Dst: "person:email/maxime@example.com", Kind: EdgeSameAs, Via: "frontmatter:aliases"},
		{Src: "wiki/maxime.md", Dst: "person:email/maxime@example.com", Kind: EdgeSameAs, Via: "frontmatter:aliases"},
	}
	for _, expected := range want {
		found := false
		for _, edge := range edges {
			if edge.Src == expected.Src && edge.Dst == expected.Dst && edge.Kind == expected.Kind && edge.Via == expected.Via {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing canonical identity edge %+v in %+v", expected, edges)
		}
	}
	listing, err := ListNodes(context.Background(), base, "person", 0)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || listing.Nodes[0].URI != "person:email/maxime@example.com" {
		t.Fatalf("person nodes = %+v, want one canonical node", listing.Nodes)
	}
	neighbours, err := Neighbours(context.Background(), base, GraphQuery{URI: "actor:github.com/maxime", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if neighbours.URI != "person:email/maxime@example.com" {
		t.Fatalf("alias neighbourhood URI = %q", neighbours.URI)
	}
}

func TestFindTreatsIdentityAliasesAsOneExactIdentifier(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [actor:github.com/maxime, maxime@work.example]
    kind: person
`)
	idPath, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	timePath, err := core.ParseFieldPath(".time")
	if err != nil {
		t.Fatal(err)
	}
	participantPath, err := core.ParseFieldPath(".participant")
	if err != nil {
		t.Fatal(err)
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "mail", Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z", Count: 1,
		Schema: core.FieldSchema{
			core.FieldID:   {Description: "Identity.", Cardinality: core.CardinalityOne},
			core.FieldTime: {Description: "Time.", Cardinality: core.CardinalityOne},
			"participant":  {Description: "Person.", Cardinality: core.CardinalityOne, Relation: true},
		},
		Fields:  sources.Fields{core.FieldID: {idPath}, core.FieldTime: {timePath}, "participant": {participantPath}},
		Records: []sources.Record{{"id": "message-1", "time": "2026-09-01T12:00:00Z", "participant": "actor:github.com/maxime"}},
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	result, err := Find(context.Background(), base, FindFilter{Grep: []string{"maxime@work.example"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("Find(alias) records = %+v", result.Records)
	}
	if got := result.Records[0].Fields["participant"]; len(got) != 1 || got[0] != "person:email/maxime@example.com" {
		t.Fatalf("projected participant = %v, want canonical URI", got)
	}
}

func TestContextTreatsAliasAsTheCanonicalIdentifier(t *testing.T) {
	base := identityTestBase(t, `
identities:
  maxime:
    canonical: person:email/maxime@example.com
    aliases: [actor:github.com/maxime, maxime@work.example]
    kind: person
`)
	writeIdentityPage(t, base, "wiki/maxime.md", "---\ntype: person\ntitle: Maxime Cordy\naliases: [actor:github.com/maxime]\n---\n")
	writeIdentityEvent(t, base, "actor:github.com/maxime")
	pack, err := BuildContext(context.Background(), base, ContextRequest{Query: "maxime@work.example", Budget: 2000})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range pack.Items {
		if item.URI != "events/2026-09-01/mail.json#message-1" {
			continue
		}
		found = true
		if got := item.Fields["participant"]; len(got) != 1 || got[0] != "person:email/maxime@example.com" {
			t.Fatalf("context participant = %v", got)
		}
	}
	if !found {
		t.Fatalf("context alias did not retrieve the canonical interaction: %+v", pack.Items)
	}
}

func TestTimelineOmitsOwnerUnlessExplicitlySelected(t *testing.T) {
	base := identityTestBase(t, `
identities:
  owner:
    canonical: person:email/owner@example.com
    aliases: [owner]
    kind: person
    owner: true
`)
	writeIdentityEvent(t, base, "person:email/owner@example.com")
	request := TimelineRequest{Window: Window{Since: "2026-09-01", Until: "2026-09-01"}, Budget: 2000}
	report, err := Timeline(context.Background(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.People) != 0 {
		t.Fatalf("ambient people = %v, want owner omitted", report.People)
	}
	request.Person = "owner"
	report, err = Timeline(context.Background(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.People) != 1 || report.People[0] != "person:email/owner@example.com" {
		t.Fatalf("explicit owner people = %v", report.People)
	}
}

func TestTimelineUsesDeclaredKindsForOpenIdentitySchemes(t *testing.T) {
	base := identityTestBase(t, `
identities:
  alice:
    canonical: slack:users/U123
    aliases: [alice]
    kind: person
  fkf:
    canonical: code:github.com/fmind/fkf
    aliases: [fkf-code]
    kind: repository
`)
	writeIdentityRelationsEvent(t, base, []string{"slack:users/U123", "code:github.com/fmind/fkf"})

	report, err := Timeline(context.Background(), base, TimelineRequest{
		Window: Window{Since: "2026-09-01", Until: "2026-09-01"}, Budget: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.People) != 1 || report.People[0] != "slack:users/U123" ||
		len(report.Repositories) != 1 || report.Repositories[0] != "code:github.com/fmind/fkf" {
		t.Fatalf("timeline entities = people %v repositories %v, want declared open-scheme kinds",
			report.People, report.Repositories)
	}

	for _, test := range []struct {
		name, person, repository, want string
	}{
		{name: "person as repository", repository: "alice", want: "--repo"},
		{name: "repository as person", person: "fkf-code", want: "--person"},
		{name: "conventional person as repository", repository: "person:email/alice@example.test", want: "--repo"},
		{name: "conventional repository as person", person: "repo:github.com/fmind/fkf", want: "--person"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Timeline(context.Background(), base, TimelineRequest{
				Window: Window{Since: "2026-09-01", Until: "2026-09-01"}, Budget: 2000,
				Person: test.person, Repository: test.repository,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("Timeline() error = %v, want %s identity-kind refusal", err, test.want)
			}
		})
	}
}

func TestTimelineInputDigestBindsIdentitySemantics(t *testing.T) {
	base := identityTestBase(t, `
identities:
  alice:
    canonical: slack:users/U123
    aliases: [alice]
    kind: person
`)
	base.Now = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	writeIdentityRelationsEvent(t, base, []string{"slack:users/U123"})
	request := TimelineRequest{Window: Window{Since: "2026-09-01", Until: "2026-09-01"}, Budget: 2000}
	before, err := Timeline(context.Background(), base, request)
	if err != nil {
		t.Fatal(err)
	}

	configPath := base.Config.Path
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(config), "    kind: person\n", "    kind: repository\n", 1)
	if updated == string(config) {
		t.Fatal("identity fixture has no kind to change")
	}
	if err := os.WriteFile(configPath, []byte(updated), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	afterBase, err := Open(base.Root())
	if err != nil {
		t.Fatal(err)
	}
	afterBase.Now = base.Now
	after, err := Timeline(context.Background(), afterBase, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.People) != 1 || len(after.Repositories) != 1 {
		t.Fatalf("identity kind did not change the semantic report: before=%+v after=%+v", before, after)
	}
	if before.Receipt.InputDigest == after.Receipt.InputDigest {
		t.Fatalf("input digest %q did not bind changed identity semantics", before.Receipt.InputDigest)
	}
}

func writeIdentityEvent(t *testing.T, base *Base, participant string) {
	t.Helper()
	idPath, _ := core.ParseFieldPath(".id")
	timePath, _ := core.ParseFieldPath(".time")
	participantPath, _ := core.ParseFieldPath(".participant")
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "mail", Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z", Count: 1,
		Schema: core.FieldSchema{
			core.FieldID:   {Description: "Identity.", Cardinality: core.CardinalityOne},
			core.FieldTime: {Description: "Time.", Cardinality: core.CardinalityOne},
			"participant":  {Description: "Person.", Cardinality: core.CardinalityOne, Relation: true},
		},
		Fields:  sources.Fields{core.FieldID: {idPath}, core.FieldTime: {timePath}, "participant": {participantPath}},
		Records: []sources.Record{{"id": "message-1", "time": "2026-09-01T12:00:00Z", "participant": participant}},
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}

func writeIdentityRelationsEvent(t *testing.T, base *Base, participants []string) {
	t.Helper()
	idPath, err := core.ParseFieldPath(".id")
	if err != nil {
		t.Fatal(err)
	}
	timePath, err := core.ParseFieldPath(".time")
	if err != nil {
		t.Fatal(err)
	}
	participantPath, err := core.ParseFieldPath(".participants[]")
	if err != nil {
		t.Fatal(err)
	}
	values := make([]any, len(participants))
	for index, participant := range participants {
		values[index] = participant
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: "mail", Layer: core.LayerEvents, Date: "2026-09-01",
		CollectedAt: "2026-09-02T00:00:00Z", Count: 1,
		Schema: core.FieldSchema{
			core.FieldID:   {Description: "Identity.", Cardinality: core.CardinalityOne},
			core.FieldTime: {Description: "Time.", Cardinality: core.CardinalityOne},
			"participant":  {Description: "Related entity.", Cardinality: core.CardinalityMany, Relation: true},
		},
		Fields: sources.Fields{
			core.FieldID: {idPath}, core.FieldTime: {timePath}, "participant": {participantPath},
		},
		Records: []sources.Record{{
			"id": "message-1", "time": "2026-09-01T12:00:00Z", "participants": values,
		}},
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
}

func identityTestBase(t *testing.T, identities string) *Base {
	t.Helper()
	root := t.TempDir()
	configText := `fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  participant: {description: Person., cardinality: many, relation: true}
layers:
  events: true
  index: true
  tasks: false
  projects: true
  wiki: true
` + identities + `sources: {}
`
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Base{Config: config, Store: config.Store(), Now: time.Now}
}

func writeIdentityPage(t *testing.T, base *Base, uri, body string) {
	t.Helper()
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
