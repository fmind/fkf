package services

import (
	"fmt"
	"strings"
	"testing"
)

func TestContextTextFitsFourteenKagglathonItemsUnderNineHundredTokens(t *testing.T) {
	sources := []string{
		"git-commits", "git-commits", "git-commits", "git-commits", "git-commits",
		"github-pull-requests", "github-pull-requests", "github-pull-requests",
		"agent-session-traces", "agent-session-traces", "agent-session-traces",
		"jira-issues", "jira-issues", "project",
	}
	candidates := make([]*ContextItem, 0, len(sources))
	for index, source := range sources {
		kind := "record"
		uri := fmt.Sprintf("events/2026-09-02/%s.json#kagglathon-%02d", source, index+1)
		if source == "project" {
			kind, uri = "projects", "projects/kagglathon.md"
		}
		candidates = append(candidates, &ContextItem{
			URI: uri, Kind: kind, Source: source, Date: "2026-09-02",
			Title: fmt.Sprintf("Kagglathon change %02d", index+1), Score: 200 - index,
			Fields: map[string][]string{
				"repository": {"repo:github.com/fmind/kagglathon"},
				"ticket":     {"ticket:KAG-123"},
			},
		})
	}
	request := ContextRequest{Query: "kagglathon", Budget: 900, DeliveryFormat: ContextDeliveryText}
	pack := &ContextPack{
		Query: request.Query,
		Receipt: Receipt{
			Query: request.Query, Window: Window{Since: "2026-09-02", Until: "2026-09-02"},
			Budget: request.Budget, Format: request.DeliveryFormat, Candidates: len(candidates),
			Terms: []string{"kagglathon"}, Dropped: []DroppedItem{}, Floor: relevanceFloor,
			InputDigest: "0123456789abcdef", AsOf: "2026-09-03", RankingVersion: RankingVersion,
			ToolVersion: "test", Notice: ContextNotice,
		},
	}
	selectWithinBudget(pack, candidates, request)
	if err := fitContextTextBudget(pack, request.Budget, candidates, request); err != nil {
		t.Fatal(err)
	}
	if pack.Receipt.Selected != 14 || len(pack.Items) != 14 {
		t.Fatalf("selected = %d/%d, want the complete 14-item kagglathon pack",
			pack.Receipt.Selected, pack.Receipt.Candidates)
	}
	delivered := []byte(RenderContextText(pack))
	if actual := (len(delivered) + 3) / 4; actual != pack.Receipt.EncodedTokens || actual > request.Budget || actual < 700 {
		t.Fatalf("encoded_tokens = %d, delivered = %d, budget = %d\n%s",
			pack.Receipt.EncodedTokens, actual, request.Budget, delivered)
	}
}

func TestContextTextBackfillsASmallerLaterCandidate(t *testing.T) {
	selected := ContextItem{
		URI: "wiki/large-existing.md", Kind: "wiki", Title: strings.Repeat("x", 2500), Score: 100,
	}
	large := &ContextItem{
		URI: "events/2026-09-02/tasks.json#large", Kind: "record", Source: "tasks",
		Date: "2026-09-02", Title: strings.Repeat("L", 800), Score: 90,
	}
	small := &ContextItem{
		URI: "events/2026-09-02/commits.json#small", Kind: "record", Source: "commits",
		Date: "2026-09-02", Title: "Small kagglathon change", Score: 80,
	}
	request := ContextRequest{Query: "kagglathon", Budget: 900, DeliveryFormat: ContextDeliveryText}
	selected.Tokens = contextSelectionTokens(&selected, request)
	large.Tokens = contextSelectionTokens(large, request)
	small.Tokens = contextSelectionTokens(small, request)
	pack := &ContextPack{
		Query: request.Query,
		Items: []ContextItem{selected},
		Receipt: Receipt{
			Query: request.Query, Budget: request.Budget, Format: request.DeliveryFormat,
			UsedTokens: selected.Tokens, Candidates: 3, Selected: 1, Terms: []string{"kagglathon"},
			Dropped: []DroppedItem{
				{URI: large.URI, Reason: "budget", Score: large.Score, Tokens: large.Tokens},
				{URI: small.URI, Reason: "budget", Score: small.Score, Tokens: small.Tokens},
			},
			Floor: relevanceFloor, InputDigest: "0123456789abcdef", AsOf: "2026-09-03",
			RankingVersion: RankingVersion, ToolVersion: "test", Notice: ContextNotice,
		},
	}
	stabilizeContextTextTokens(pack)
	backfillContextText(pack, []*ContextItem{&selected, large, small}, request, request.Budget)
	if len(pack.Items) != 2 || pack.Items[1].URI != small.URI {
		t.Fatalf("items = %+v, want the oversized higher-ranked candidate skipped and the smaller one backfilled", pack.Items)
	}
	if actual := (len(RenderContextText(pack)) + 3) / 4; actual > request.Budget || actual != pack.Receipt.EncodedTokens {
		t.Fatalf("backfilled delivery = %d, receipt = %d, budget = %d", actual, pack.Receipt.EncodedTokens, request.Budget)
	}
}
