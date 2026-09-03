package services_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func contextBase(t *testing.T) *services.Base {
	t.Helper()
	base := graphBase(t)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestContextReturnsAPackAndAReceipt(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary FK-412", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) == 0 {
		t.Fatal("the pack is empty; the base holds matching records and pages")
	}
	for _, item := range pack.Items {
		if item.URI == "" || item.Tokens <= 0 {
			t.Fatalf("item = %+v, want a uri and a token cost", item)
		}
		// The arithmetic has to be checkable by hand: the reasons add up to the score.
		total := 0
		for _, reason := range item.Reasons {
			total += reason.Points
		}
		if total != item.Score {
			t.Fatalf("item %s scored %d but its reasons sum to %d", item.URI, item.Score, total)
		}
	}
	receipt := pack.Receipt
	if receipt.RankingVersion != services.RankingVersion || receipt.ToolVersion == "" || receipt.InputDigest == "" {
		t.Fatalf("receipt = %+v, want the version and digest fields that make a change visible", receipt)
	}
	if receipt.UsedTokens > receipt.Budget {
		t.Fatalf("used %d tokens of a %d budget", receipt.UsedTokens, receipt.Budget)
	}
	if receipt.Candidates < receipt.Selected {
		t.Fatalf("receipt = %+v, want every selection to come from a candidate", receipt)
	}
}

// TestContextIsByteIdentical is the property the receipt promises: the same query against the
// same base, binary, and evaluation day produces the same pack.
func TestContextIsByteIdentical(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval boundary", Budget: 2048, Explain: true, Expand: true}
	first, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	encodedFirst, _ := json.Marshal(first)
	for range 5 {
		again, err := services.BuildContext(t.Context(), base, request)
		if err != nil {
			t.Fatal(err)
		}
		encodedAgain, _ := json.Marshal(again)
		if string(encodedAgain) != string(encodedFirst) {
			t.Fatal("BuildContext() is not reproducible; the receipt would stop meaning anything")
		}
	}
}

func TestContextResolvesRawRelativeWindowFromItsEvaluationInstant(t *testing.T) {
	base := contextBase(t)
	clockReads := 0
	base.Now = func() time.Time {
		clockReads++
		return testClock.AddDate(0, 0, clockReads-1)
	}
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval", Window: services.Window{Since: "today", Until: "today"}, Budget: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if clockReads != 1 || pack.Receipt.AsOf != "2026-05-10" ||
		pack.Receipt.Window.Since != pack.Receipt.AsOf || pack.Receipt.Window.Until != pack.Receipt.AsOf {
		t.Fatalf("clock reads = %d, receipt = %+v; want one shared evaluation instant", clockReads, pack.Receipt)
	}
}

func TestContextReceiptBindsTheRequestedDeliveryFormat(t *testing.T) {
	base := contextBase(t)
	jsonPack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary", Budget: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	textPack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary", Budget: 2048, DeliveryFormat: services.ContextDeliveryText,
	})
	if err != nil {
		t.Fatal(err)
	}
	if jsonPack.Receipt.Format != services.ContextDeliveryJSON ||
		textPack.Receipt.Format != services.ContextDeliveryText ||
		jsonPack.Receipt.InputDigest == textPack.Receipt.InputDigest {
		t.Fatalf("json receipt = %+v, text receipt = %+v; want format-bound digests",
			jsonPack.Receipt, textPack.Receipt)
	}
	actualTextTokens := (len(services.RenderContextText(textPack)) + 3) / 4
	if textPack.Receipt.EncodedTokens != actualTextTokens || actualTextTokens > textPack.Receipt.Budget {
		t.Fatalf("service text encoded_tokens = %d, delivered = %d, budget = %d",
			textPack.Receipt.EncodedTokens, actualTextTokens, textPack.Receipt.Budget)
	}
}

func TestContextDigestCoversSemanticInputsAndEvaluationDay(t *testing.T) {
	base := contextBase(t)
	request := services.ContextRequest{Query: "retrieval boundary", Budget: 2048, Explain: true}
	first, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.AsOf != "2026-05-10" {
		t.Fatalf("as_of = %q, want the evaluation day", first.Receipt.AsOf)
	}

	pageURI := "wiki/retrieval-boundary.md"
	before, err := base.ReadFileContext(t.Context(), pageURI, core.MaxNarrativeBytes)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(before), "boundary", "boundarz", 1)
	if len(mutated) != len(before) || mutated == string(before) {
		t.Fatal("the fixture mutation must change semantics without changing byte length")
	}
	write(t, base, pageURI, mutated)
	second, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt.InputDigest == first.Receipt.InputDigest {
		t.Fatal("a same-length semantic edit left input_digest unchanged")
	}

	base.Now = func() time.Time { return testClock.AddDate(0, 0, 1) }
	third, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if third.Receipt.AsOf != "2026-05-11" || third.Receipt.InputDigest == second.Receipt.InputDigest {
		t.Fatalf("next-day receipt = %+v, want as_of and input_digest to change", third.Receipt)
	}
}

func TestContextDropsBelowTheFloorWithAReason(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval boundary", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Receipt.Dropped) == 0 {
		t.Fatal("a base with unrelated records must drop some, and say why")
	}
	sawFloor := false
	for _, dropped := range pack.Receipt.Dropped {
		if dropped.Reason == "below-floor" {
			sawFloor = true
		}
		if dropped.Reason != "below-floor" && dropped.Reason != "budget" && dropped.Reason != "duplicate" {
			t.Fatalf("dropped = %+v, want a reason from the documented set", dropped)
		}
		for _, item := range pack.Items {
			if item.URI == dropped.URI {
				t.Fatalf("%s is both selected and dropped", item.URI)
			}
		}
	}
	if !sawFloor {
		t.Fatal("nothing was dropped below the floor; the floor is not doing anything")
	}
}

// TestContextBudgetIsAHardCeiling checks the one thing a budget must never do: overshoot.
func TestContextBudgetIsAHardCeiling(t *testing.T) {
	base := contextBase(t)
	succeeded := false
	tooSmall := false
	for _, budget := range []int{1, 10, 50, 200, 256, 300, 400, 512, 700, 1024, 4096} {
		pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval boundary FK-412", Budget: budget})
		if err != nil {
			if !errors.Is(err, services.ErrContextBudgetTooSmall) {
				t.Fatalf("budget %d: BuildContext() error = %v", budget, err)
			}
			tooSmall = true
			continue
		}
		succeeded = true
		total := 0
		for _, item := range pack.Items {
			total += item.Tokens
		}
		if total > budget {
			t.Fatalf("budget %d produced a %d-token pack", budget, total)
		}
		if total != pack.Receipt.UsedTokens {
			t.Fatalf("the receipt claims %d tokens, the items cost %d", pack.Receipt.UsedTokens, total)
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(pack); err != nil {
			t.Fatal(err)
		}
		actual := (encoded.Len() + 3) / 4
		if actual != pack.Receipt.EncodedTokens {
			t.Fatalf("budget %d: encoded_tokens = %d, actual delivered size = %d", budget, pack.Receipt.EncodedTokens, actual)
		}
		if actual > budget {
			t.Fatalf("budget %d delivered a %d-token pack", budget, actual)
		}
	}
	if !succeeded || !tooSmall {
		t.Fatalf("sampled budgets succeeded=%t too-small=%t, want both outcomes", succeeded, tooSmall)
	}
}

func TestContextEncodedTokensMatchesThePublicJSONEncoder(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "<&> retrieval boundary", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var delivered bytes.Buffer
	encoder := json.NewEncoder(&delivered)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(pack); err != nil {
		t.Fatal(err)
	}
	actual := (delivered.Len() + 3) / 4
	if pack.Receipt.EncodedTokens != actual || actual > pack.Receipt.Budget {
		t.Fatalf("encoded_tokens = %d, delivered = %d, budget = %d",
			pack.Receipt.EncodedTokens, actual, pack.Receipt.Budget)
	}
}

func TestContextTooSmallMinimumSucceedsOnRetry(t *testing.T) {
	base := contextBase(t)
	query := "<&> retrieval boundary FK-412"
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: query, Budget: 1})
	var budgetError *services.ContextBudgetError
	if !errors.As(err, &budgetError) {
		t.Fatalf("BuildContext(budget 1) error = %v, want ContextBudgetError", err)
	}
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: query, Budget: budgetError.Minimum,
	})
	if err != nil {
		t.Fatalf("retry at reported minimum %d failed: %v", budgetError.Minimum, err)
	}
	if pack.Receipt.EncodedTokens > budgetError.Minimum {
		t.Fatalf("retry at %d delivered %d tokens", budgetError.Minimum, pack.Receipt.EncodedTokens)
	}
	if budgetError.Minimum > 1 {
		_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
			Query: query, Budget: budgetError.Minimum - 1,
		})
		if !errors.Is(err, services.ErrContextBudgetTooSmall) {
			t.Fatalf("budget %d error = %v, want the reported minimum to be exact",
				budgetError.Minimum-1, err)
		}
	}
}

// TestContextPinIsAdmittedFirstButCapped keeps a pin from crowding out the answer it was
// supposed to accompany.
func TestContextPinIsAdmittedFirstButCapped(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "something entirely unrelated", Budget: 4096, Pins: []string{"projects/fkf-rebuild.md"}, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) == 0 || !pack.Items[0].Pinned || pack.Items[0].URI != "projects/fkf-rebuild.md" {
		t.Fatalf("items = %+v, want the pin admitted first even with no term match", pack.Items)
	}
	tiny, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "unrelated", Budget: 300, Pins: []string{"projects/fkf-rebuild.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The pin's third of a small valid budget cannot hold this page, even though a compact
	// matching record may still use the rest of the pack.
	for _, item := range tiny.Items {
		if item.Pinned {
			t.Fatalf("items = %+v, want the oversized pin capped", tiny.Items)
		}
	}
	if len(tiny.Receipt.RejectedPins) != 1 || tiny.Receipt.RejectedPins[0] != "projects/fkf-rebuild.md" {
		t.Fatalf("rejected_pins = %v, want the requested page named independently of dropped detail",
			tiny.Receipt.RejectedPins)
	}
	foundBudgetDrop := false
	for _, dropped := range tiny.Receipt.Dropped {
		if dropped.URI == "projects/fkf-rebuild.md" && dropped.Reason == "budget" {
			foundBudgetDrop = true
		}
	}
	if !foundBudgetDrop {
		t.Fatalf("dropped = %+v, want the capped pin reported as a budget drop", tiny.Receipt.Dropped)
	}
}

func TestContextPinChargesItsDeliveredExplanation(t *testing.T) {
	base := contextBase(t)
	plain, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "fkf rebuild", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "fkf rebuild", Budget: 4096, Explain: true, Pins: []string{"projects/fkf-rebuild.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	plainItem := itemByURI(t, plain, "projects/fkf-rebuild.md")
	pinnedItem := itemByURI(t, pinned, "projects/fkf-rebuild.md")
	if pinnedItem.Tokens <= plainItem.Tokens {
		t.Fatalf("pinned tokens = %d, plain tokens = %d; want the delivered pinned flag and reason charged",
			pinnedItem.Tokens, plainItem.Tokens)
	}
}

func TestContextPinMatchesOnlyPinnablePageKinds(t *testing.T) {
	base := contextBase(t)
	write(t, base, "wiki/summary.md", "---\ntype: insight\ntitle: Pinned summary\ntags: [test]\n---\n\n# Pinned summary\n")
	write(t, base, "projects/summary.md", "---\ntype: project\ntitle: Project summary\nstatus: active\ntags: [test]\n---\n\n# Project summary\n")
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "something unrelated", Budget: 4096, Pins: []string{"projects/summary.md"}, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var pinned []services.ContextItem
	for _, item := range pack.Items {
		if item.Pinned {
			pinned = append(pinned, item)
		}
	}
	if len(pinned) != 1 || pinned[0].URI != "projects/summary.md" {
		t.Fatalf("pinned = %+v, want only the exact project URI despite colliding page slugs", pinned)
	}
}

// TestContextRefusesAnUnknownPin is the regression test for silent-empty on --pin. It used to
// build the pack having quietly dropped the pin, exit 0 — the one guarantee `--pin` makes,
// "whatever this scores, it is in", failing exactly at the moment it was needed to override a
// low score, and looking indistinguishable from a pin that matched and simply scored zero.
func TestContextRefusesAnUnknownPin(t *testing.T) {
	base := contextBase(t)
	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "unrelated", Budget: 4096, Pins: []string{"no-such-page"},
	})
	if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), `unknown pin "no-such-page"`) ||
		!strings.Contains(err.Error(), "projects/fkf-rebuild.md") {
		t.Fatalf("error = %v, want an unknown-pin refusal naming a real URI", err)
	}
	for _, ambiguous := range []string{"fkf-rebuild", "fkf-rebuild.md", "wiki/../projects/fkf-rebuild.md"} {
		_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
			Query: "unrelated", Budget: 4096, Pins: []string{ambiguous},
		})
		if !errors.Is(err, core.ErrConfig) || !strings.Contains(err.Error(), `unknown pin "`+ambiguous+`"`) {
			t.Fatalf("BuildContext(pin=%q) error = %v, want exact-URI-only refusal", ambiguous, err)
		}
	}
}

func TestContextPageExcerptCentersTheFirstBodyMatch(t *testing.T) {
	base := contextBase(t)
	prefix := strings.Repeat("éclair filler ", 40)
	write(t, base, "wiki/distant-evidence.md", `---
type: insight
title: Distant evidence
tags: [test]
---

# Distant evidence

`+prefix+`RaReNeedle keeps its original case. `+strings.Repeat("tail filler ", 40)+"\n")

	request := services.ContextRequest{Query: "rareneedle", Budget: 4096}
	first, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByURI(t, first, "wiki/distant-evidence.md")
	if !strings.Contains(item.Excerpt, "RaReNeedle") {
		t.Fatalf("excerpt = %q, want the original-cased body match", item.Excerpt)
	}
	if strings.Contains(item.Excerpt, strings.Repeat("éclair filler ", 20)) ||
		!strings.HasPrefix(item.Excerpt, "…") || !strings.HasSuffix(item.Excerpt, "…") {
		t.Fatalf("excerpt = %q, want a bounded window centered on the distant match", item.Excerpt)
	}

	second, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if again := itemByURI(t, second, "wiki/distant-evidence.md").Excerpt; again != item.Excerpt {
		t.Fatalf("second excerpt = %q, first = %q; excerpts must be deterministic", again, item.Excerpt)
	}
}

func TestContextPageExcerptIsEmptyWithoutADirectBodyMatch(t *testing.T) {
	base := contextBase(t)
	write(t, base, "wiki/metadata-only.md", `---
type: insight
title: MetadataOnlyNeedle
tags: [test]
---

# Notes

This body contains unrelated evidence.
`)

	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "MetadataOnlyNeedle", Budget: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if excerpt := itemByURI(t, pack, "wiki/metadata-only.md").Excerpt; excerpt != "" {
		t.Fatalf("excerpt = %q, want no fabricated body context for a metadata-only match", excerpt)
	}
}

func TestContextFiltersAuthoredPagesByInclusiveValidity(t *testing.T) {
	base := contextBase(t)
	for name, bounds := range map[string]string{
		"current": "valid_from: 2026-05-10\nvalid_until: 2026-05-10\n",
		"future":  "valid_from: 2026-05-11\n",
		"expired": "valid_until: 2026-05-09\n",
	} {
		write(t, base, "wiki/"+name+"-validity.md", "---\ntype: insight\ntitle: "+name+" validity\n"+
			bounds+"tags: [test]\n---\n\n# "+name+" validity\n\nValidityneedle evidence.\n")
	}
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "validityneedle", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	itemByURI(t, pack, "wiki/current-validity.md")
	for _, excluded := range []string{"wiki/future-validity.md", "wiki/expired-validity.md"} {
		if slices.ContainsFunc(pack.Items, func(item services.ContextItem) bool { return item.URI == excluded }) {
			t.Fatalf("invalid authored page %s entered the as-of pack", excluded)
		}
	}
}

// TestContextExpandRescoresThroughTheGraph pins down what one hop actually does. Almost
// everything it reaches is already a candidate — the window gathered it — so the hop mostly
// RESCORES: an item sharing any declared entity with a top hit gains a fixed
// discount and a named reason, which is often what lifts it over the floor.
func TestContextExpandRescoresThroughTheGraph(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	// Two records share a repository. Only one names the ticket, so only the graph connects
	// the other to the query.
	collect(t, base, "2026-05-04", `[
	  {"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Fix FK-412","link":"https://x.test/a1","repo_uri":"repo:github.com/fmind/fkf","author_uris":["person:email/marc@example.test"],"ticket_uri":"ticket:FK-412"},
	  {"id":"a2","t":"2026-05-04T10:00:00Z","subject":"Bump a dependency","link":"https://x.test/a2","repo_uri":"repo:github.com/fmind/fkf","author_uris":["person:email/ines@example.test"]},
	  {"id":"a3","t":"2026-05-04T11:00:00Z","subject":"Unrelated work","link":"https://x.test/a3","repo_uri":"repo:github.com/acme/ledger","author_uris":["person:email/tomas@example.test"]}
	]`)
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	request := services.ContextRequest{Query: "FK-412", Budget: 4096, Explain: true}
	plain, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Expand = true
	expanded, err := services.BuildContext(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded.Items) <= len(plain.Items) {
		t.Fatalf("--expand selected %d item(s) against %d without; the hop must reach further",
			len(expanded.Items), len(plain.Items))
	}
	sawExpansion := false
	for _, item := range expanded.Items {
		for _, reason := range item.Reasons {
			if reason.Reason != "join-expansion" {
				continue
			}
			sawExpansion = true
			if reason.Points != 20 {
				t.Fatalf("expansion is a fixed discount, got %d points", reason.Points)
			}
			if !strings.Contains(reason.Detail, "one hop through ") {
				t.Fatalf("reason = %+v, want it to name the entity it came through", reason)
			}
		}
	}
	if !sawExpansion {
		t.Fatal("no item reached the pack through the graph; --expand did nothing visible")
	}
	// The record sharing only the repository is what the hop is for; the unrelated one stays out.
	reached := map[string]bool{}
	for _, item := range expanded.Items {
		reached[item.URI] = true
	}
	if !reached["events/2026-05-04/synthetic.json#a2"] {
		t.Fatalf("items = %+v, want the record sharing the repository reached by the hop", expanded.Items)
	}
	if reached["events/2026-05-04/synthetic.json#a3"] {
		t.Fatal("a record sharing nothing with the seeds must stay out")
	}
	// A seed is not expanded into itself: the reason has to mean something.
	for _, item := range expanded.Items {
		count := 0
		for _, reason := range item.Reasons {
			if reason.Reason == "join-expansion" {
				count++
			}
		}
		if count > 1 {
			t.Fatalf("%s carries %d expansion reasons; one hop is one reason", item.URI, count)
		}
	}
}

func TestContextExpandRefusesAPartialGraphJoin(t *testing.T) {
	base := contextBase(t)
	indexed := base.Now().UTC().Format(time.RFC3339)
	edges := make([]services.Edge, 0, 201)
	for index := 0; index < 201; index++ {
		edges = append(edges, services.Edge{
			Src:  "events/2026-05-04/synthetic.json#a1",
			Dst:  fmt.Sprintf("repo:example/repository-%03d", index),
			Kind: "repository", Via: "field:repository", Indexed: indexed,
		})
	}
	graph, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	currentMetaData, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var currentMeta services.EdgeListMeta
	if err := json.Unmarshal(currentMetaData, &currentMeta); err != nil {
		t.Fatal(err)
	}
	metadata, err := services.NewEdgeListMeta(
		edges, base.Now(), currentMeta.SHA256.Inputs, currentMeta.Inputs...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.WriteEdgeList(graph, meta, edges, metadata); err != nil {
		t.Fatal(err)
	}
	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, Expand: true,
	})
	if err == nil || !strings.Contains(err.Error(), "200-edge safety limit") {
		t.Fatalf("BuildContext(--expand) error = %v, want a partial-join refusal", err)
	}
}

func TestContextExpansionPropagatesAStaleGraphTarget(t *testing.T) {
	base := contextBase(t)
	graph, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		t.Fatal(err)
	}
	currentMetaData, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var currentMeta services.EdgeListMeta
	if err := json.Unmarshal(currentMetaData, &currentMeta); err != nil {
		t.Fatal(err)
	}
	indexed := base.Now().UTC().Format(time.RFC3339)
	edges := []services.Edge{
		{
			Src: "events/2026-05-03/missing.json#ghost", Dst: "ticket:FK-412",
			Kind: "work-item", Via: "field:work-item", Indexed: indexed,
		},
		{
			Src: "events/2026-05-04/synthetic.json#a1", Dst: "ticket:FK-412",
			Kind: "work-item", Via: "field:work-item", Indexed: indexed,
		},
	}
	metadata, err := services.NewEdgeListMeta(
		edges, base.Now(), currentMeta.SHA256.Inputs, currentMeta.Inputs...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.WriteEdgeList(graph, meta, edges, metadata); err != nil {
		t.Fatal(err)
	}

	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, Expand: true,
	})
	if err == nil || !strings.Contains(err.Error(), "events/2026-05-03/missing.json") {
		t.Fatalf("BuildContext() error = %v, want the stale graph target reported", err)
	}
}

func TestContextExpansionRejectsMalformedGraphRows(t *testing.T) {
	base := contextBase(t)
	graph, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(graph, os.O_APPEND|os.O_WRONLY, core.BaseFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("events/2026-05-04/synthetic.json#a1\tticket:FK-412\tbroken\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, Expand: true,
	})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("BuildContext() error = %v, want the malformed graph row reported", err)
	}
}

func TestContextExpansionRejectsARowCompleteTruncatedGraph(t *testing.T) {
	base := contextBase(t)
	truncateGraphCacheByOneRow(t, base)

	_, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412", Budget: 4096, Expand: true,
	})
	if err == nil || !strings.Contains(err.Error(), "metadata edges") ||
		!strings.Contains(err.Error(), "fkf build graph") {
		t.Fatalf("BuildContext() error = %v, want the row-count mismatch and rebuild remedy", err)
	}
}

func TestContextNeedsAQuery(t *testing.T) {
	base := contextBase(t)
	if _, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "   "}); err == nil {
		t.Fatal("BuildContext() must refuse an empty query")
	}
}

func TestContextExplainOmitsReasonsWhenNotAsked(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if len(item.Reasons) != 0 {
			t.Fatalf("item = %+v, want the breakdown only under --explain", item)
		}
		if item.Score == 0 {
			t.Fatal("the score itself always travels; only the breakdown is optional")
		}
	}
}

// TestRetrievalEvaluationSuite exercises the strict base-owned recall@k contract rather than
// maintaining a second ad hoc retrieval loop beside `fkf eval`.
func TestRetrievalEvaluationSuite(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	// Twelve topics, one record each, plus two pages — enough for rarity to matter.
	for index, topic := range []string{
		"retrieval boundary", "token budget receipt", "declarative source runner",
		"graph edge extraction", "trust gate", "lazy body fetching",
		"daily collection window", "people identity reduction", "uri grammar",
		"markdown validation", "index staleness", "quiet source watchdog",
	} {
		date := fmt.Sprintf("2026-04-%02d", index+1)
		collect(t, base, date, fmt.Sprintf(
			`[{"id":"r%d","t":"%sT09:00:00Z","subject":"Discuss %s in detail","link":"https://x.test/%d","repo":"fmind/fkf","who":"marc@example.test"}]`,
			index, date, topic, index,
		))
	}
	write(t, base, "wiki/retrieval-boundary.md",
		"---\ntype: decision\ntitle: Retrieval boundary\ntags: [retrieval]\n---\n\n# Retrieval boundary\n\nLexical and reproducible.\n")
	write(t, base, "projects/fkf-rebuild.md",
		"---\ntype: project\ntitle: fkf rebuild\nstatus: active\ntags: [fkf]\n---\n\n# fkf rebuild\n\nDeclarative source runner work.\n")
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	queries := []struct{ query, wantURI string }{
		{"retrieval boundary", "wiki/retrieval-boundary.md"},
		{"token budget receipt", "events/2026-04-02/synthetic.json#r1"},
		{"declarative source runner", "projects/fkf-rebuild.md"},
		{"graph edge extraction", "events/2026-04-04/synthetic.json#r3"},
		{"trust gate", "events/2026-04-05/synthetic.json#r4"},
		{"lazy body fetching", "events/2026-04-06/synthetic.json#r5"},
		{"daily collection window", "events/2026-04-07/synthetic.json#r6"},
		{"people identity reduction", "events/2026-04-08/synthetic.json#r7"},
		{"uri grammar", "events/2026-04-09/synthetic.json#r8"},
		{"markdown validation", "events/2026-04-10/synthetic.json#r9"},
		{"index staleness", "events/2026-04-11/synthetic.json#r10"},
		{"quiet source watchdog", "events/2026-04-12/synthetic.json#r11"},
	}
	var suite strings.Builder
	suite.WriteString("fkf: 1\nk: 10\nrecall_threshold: 1\nqueries:\n")
	for index, test := range queries {
		fmt.Fprintf(&suite, "  - name: query-%02d\n", index+1)
		fmt.Fprintf(&suite, "    question: %q\n", test.query)
		suite.WriteString("    window: {since: 2026-04-01, until: 2026-05-10}\n")
		fmt.Fprintf(&suite, "    expected_uris: [%q]\n", test.wantURI)
		suite.WriteString("    forbidden_uris: []\n")
	}
	writeEvalSuite(t, base, suite.String())
	report, err := services.Evaluate(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.PassedQueries != len(queries) || report.Failed != 0 {
		t.Fatalf("evaluation report = %+v, want %d/%d queries at recall@10", report, len(queries), len(queries))
	}
}

func TestContextMatchesCanonicalRecordAndPageIdentities(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[{"id":"opaque-77","t":"2026-05-04T09:00:00Z","subject":"Unrelated title"}]`)
	write(t, base, "wiki/quiet-page.md", "---\ntype: insight\ntitle: Unrelated page\ntags: [test]\n---\n\n# Unrelated page\n")

	for _, test := range []struct {
		query, wantURI string
	}{
		{query: "opaque-77", wantURI: "events/2026-05-04/synthetic.json#opaque-77"},
		{query: "events/2026-05-04/synthetic.json#opaque-77", wantURI: "events/2026-05-04/synthetic.json#opaque-77"},
		{query: "wiki/quiet-page.md", wantURI: "wiki/quiet-page.md"},
	} {
		t.Run(test.query, func(t *testing.T) {
			pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
				Query: test.query, Budget: 4096, Explain: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			item := itemByURI(t, pack, test.wantURI)
			if _, ok := hasReason(item, "exact-identifier"); !ok {
				t.Fatalf("reasons = %+v, want the canonical identity to score as an exact identifier", item.Reasons)
			}
		})
	}
}

func TestContextTokenizesUnicodeLettersAndNumbers(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[{"id":"u1","t":"2026-05-04T09:00:00Z","subject":"Discuss éàç and 東京駅"}]`)
	for _, query := range []string{"éàç", "東京駅"} {
		t.Run(query, func(t *testing.T) {
			pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
				Query: query, Budget: 4096,
			})
			if err != nil {
				t.Fatal(err)
			}
			itemByURI(t, pack, "synthetic.json#u1")
		})
	}
}

// TestContextRanksTaskTraces: the agent's own past work on a topic is the evidence it
// most needs when resuming it, and today is never collected, so a trace dated after the last
// collected day must still be a candidate. Its explicit relation, not prose inference,
// identifies the work item.
func TestContextRanksTaskTraces(t *testing.T) {
	base := contextBase(t)
	write(t, base, "tasks/2026-05-10/fix-fk-412/TASKS.md",
		"---\nrelations:\n  ticket: [ticket:FK-412]\n---\n\n# Fix FK-412 retrieval boundary\n\n## Request\n\nRaise the retrieval boundary for FK-412.\n\n## Verification\n\ngo test ./... ok\n")
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary FK-412", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]services.ContextItem{}
	for _, item := range pack.Items {
		byKind[item.Kind] = item
	}
	trace, ok := byKind["tasks"]
	if !ok || trace.URI != "tasks/2026-05-10/fix-fk-412/TASKS.md" || trace.Date != "2026-05-10" {
		t.Fatalf("items = %+v, want the trace dated after the last collected day", pack.Items)
	}
	identified := false
	for _, reason := range trace.Reasons {
		identified = identified || reason.Reason == "exact-identifier"
	}
	if !identified {
		t.Fatalf("trace reasons = %+v, want exact-identifier from its declared ticket relation", trace.Reasons)
	}
	if pack.Receipt.Window.Until != testClock.Format(time.DateOnly) {
		t.Fatalf("implicit window = %+v, want it to include today's task traces", pack.Receipt.Window)
	}
}

func TestContextExplicitUntilBoundsTaskTraces(t *testing.T) {
	base := contextBase(t)
	write(t, base, "tasks/2026-05-10/future/TASKS.md",
		"# Future trace\n\nAubergine-only evidence after the requested historical window.\n")
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "aubergine-only", Budget: 4096,
		Window: services.Window{Since: "2026-05-04", Until: "2026-05-05"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if item.Kind == "tasks" {
			t.Fatalf("items = %+v, want --until to exclude later task traces", pack.Items)
		}
	}
}

// TestContextRanksTheIndex pins the other half of "leverage the index". The candidate
// set used to be records-in-the-window plus the page layers and nothing else, so the most
// durable half of a base — every repository, every file, every contact, the whole toolchain —
// could never enter a pack, even though the graph indexed those same records and `read`
// resolved them. An index document carries no date, so no window bounds it.
func TestContextRanksTheIndex(t *testing.T) {
	base := contextBase(t)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "repos", Layer: core.LayerIndex,
		CollectedAt: "2026-05-05T09:00:00Z",
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}, core.FieldTitle: {mustFieldPath(t, ".title")}},
		Count:       1,
		Records:     []sources.Record{{"id": "fmind/fkf", "title": "chartreuse telemetry harness"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildGraph(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "chartreuse", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pack.Items {
		if item.URI == "index/repos.json#fmind/fkf" {
			return
		}
	}
	t.Fatalf("items = %+v, want the index record the query names", pack.Items)
}

// TestContextReceiptReportsCollectionFreshness pins the field that separates "nothing matched"
// from "nothing has been collected since May". A window alone cannot tell those apart: a query
// over the last thirty days looks identical either way.
func TestContextReceiptReportsCollectionFreshness(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	// testClock is 2026-05-10, so a day collected on the 4th is six days old.
	collect(t, base, "2026-05-04", `[{"id":"a","t":"2026-05-04T09:00:00Z","subject":"retrieval boundary"}]`)

	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval", Budget: 2048})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if pack.Receipt.NewestEventDay != "2026-05-04" {
		t.Errorf("newest_event_day = %q, want the newest collected event day", pack.Receipt.NewestEventDay)
	}
	encoded, err := json.Marshal(pack.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"newest_event_day":"2026-05-04"`)) || bytes.Contains(encoded, []byte(`"latest_day"`)) {
		t.Fatalf("receipt JSON = %s, want only the newest_event_day spelling", encoded)
	}
	if pack.Receipt.StaleDays != 6 {
		t.Errorf("stale_days = %d, want 6", pack.Receipt.StaleDays)
	}
}

// --- Recency: a bounded modifier on relevance, never a source of it -----------------------

// itemByURI finds the one item in a pack whose URI contains needle, failing the test if none
// does — every recency test isolates one record or page by a fragment unique to its fixture.
func itemByURI(t *testing.T, pack *services.ContextPack, needle string) *services.ContextItem {
	t.Helper()
	for index := range pack.Items {
		if strings.Contains(pack.Items[index].URI, needle) {
			return &pack.Items[index]
		}
	}
	t.Fatalf("no item in the pack contains %q; items = %+v", needle, pack.Items)
	return nil
}

func hasReason(item *services.ContextItem, name string) (services.Reason, bool) {
	for _, reason := range item.Reasons {
		if reason.Reason == name {
			return reason, true
		}
	}
	return services.Reason{}, false
}

// TestContextRecencyPrefersAFreshMatchOverAnOldOneAtEqualRelevance is the property the signal
// exists for: two records naming the identical term, at the identical rarity, differ only in
// how old they are — and the fresher one must win, with the receipt saying why.
func TestContextRecencyPrefersAFreshMatchOverAnOldOneAtEqualRelevance(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	base.Config.Sources["synthetic"].Recency.HalfLifeDays = 7
	// Distinct titles keep v6's identical-run collapse from hiding the source-local recency
	// comparison. testClock is 2026-05-10: old1 is 35 days old and fresh1 is 1 day old.
	collect(t, base, "2026-04-05", `[{"id":"old1","t":"2026-04-05T09:00:00Z","subject":"old evidence xyzzyplugh combo"}]`)
	collect(t, base, "2026-05-09", `[{"id":"fresh1","t":"2026-05-09T09:00:00Z","subject":"new evidence xyzzyplugh combo"}]`)
	// Two terms, matched verbatim as a phrase, so both records clear the floor well before
	// recency is even considered — the test isolates recency as the ONLY remaining difference.
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "xyzzyplugh combo", Budget: 4096, Explain: true,
		Window: services.Window{Since: "2026-04-01", Until: "2026-05-10"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	old := itemByURI(t, pack, "old1")
	fresh := itemByURI(t, pack, "fresh1")
	if fresh.Score <= old.Score {
		t.Fatalf("fresh.Score = %d, old.Score = %d; an equally-relevant, fresher record must outrank an old one",
			fresh.Score, old.Score)
	}
	if pack.Items[0].URI != fresh.URI {
		t.Fatalf("items[0] = %s, want the fresher record ranked first", pack.Items[0].URI)
	}
	reason, ok := hasReason(fresh, "recency")
	if !ok {
		t.Fatalf("fresh.Reasons = %+v, want a recency reason on a 1-day-old record", fresh.Reasons)
	}
	// round(15 * 0.5^(1/7)): the source declares the exponential half-life explicitly.
	if reason.Points != 14 {
		t.Fatalf("recency points = %d, want 14 for a 1-day-old item", reason.Points)
	}
	if _, ok := hasReason(old, "recency"); ok {
		t.Fatalf("old.Reasons = %+v, want its rounded five-half-life bonus to be zero", old.Reasons)
	}
	if pack.Receipt.RecencyModel["synthetic"] != 7 {
		t.Fatalf("recency_model = %+v, want the source-local seven-day half-life", pack.Receipt.RecencyModel)
	}
}

// TestContextRecencyNeverLetsAnUnrelatedFreshRecordClearTheFloor is the regression test for the
// bug the gate on candidate.Score > 0 exists to prevent: pointsRecencyMax (15) is bigger than
// relevanceFloor (10), so an ungated bonus would let today's completely unrelated record pass
// the floor on freshness alone.
func TestContextRecencyNeverLetsAnUnrelatedFreshRecordClearTheFloor(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	base.Config.Sources["synthetic"].Recency.HalfLifeDays = 7
	collect(t, base, "2026-05-09", `[{"id":"noise1","t":"2026-05-09T09:00:00Z","subject":"Completely unrelated chore"}]`)
	collect(t, base, "2026-05-04", `[{"id":"match1","t":"2026-05-04T09:00:00Z","subject":"Fix retrieval boundary FK-412"}]`)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval boundary", Budget: 4096})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	for _, item := range pack.Items {
		if strings.Contains(item.URI, "noise1") {
			t.Fatalf("noise1 = %+v, an unrelated record must never clear the floor on recency alone", item)
		}
	}
	var droppedForFloor bool
	for _, dropped := range pack.Receipt.Dropped {
		if strings.Contains(dropped.URI, "noise1") && dropped.Reason == "below-floor" {
			droppedForFloor = true
		}
	}
	if !droppedForFloor {
		t.Fatalf("dropped = %+v, want noise1 reported as dropped below the floor", pack.Receipt.Dropped)
	}
}

// TestContextRecencyDoesNotApplyToADatelessPage is what keeps durable knowledge from being
// treated as stale: OKF is explicit that wiki/ holds what is durably true, and
// wiki/retrieval-boundary.md carries no `date:` frontmatter, so it must score on relevance
// alone rather than earn — or lose — anything for having no shelf life.
func TestContextRecencyDoesNotApplyToADatelessPage(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	page := itemByURI(t, pack, "wiki/retrieval-boundary.md")
	if _, ok := hasReason(page, "recency"); ok {
		t.Fatalf("page.Reasons = %+v, want no recency reason on a page with no date", page.Reasons)
	}
}

// TestContextRecencyNeverOutweighsAStrongIdentifierMatch pins the constant relationship the
// design depends on: pointsRecencyMax (15) is small next to pointsIdentifier (100), so a
// six-week-old record that names the ticket exactly must still outrank a one-day-old record
// that only shares a common word.
func TestContextRecencyNeverOutweighsAStrongIdentifierMatch(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	base.Config.Sources["synthetic"].Recency.HalfLifeDays = 7
	collect(t, base, "2026-04-01", `[
	  {"id":"old1","t":"2026-04-01T09:00:00Z","subject":"Fix retrieval boundary (FK-412)","ticket_uri":"ticket:FK-412"},
	  {"id":"noise-old","t":"2026-04-01T10:00:00Z","subject":"Unrelated archival note"}
	]`)
	collect(t, base, "2026-05-09", `[
	  {"id":"fresh1","t":"2026-05-09T09:00:00Z","subject":"boundary chatter, nothing about the ticket"},
	  {"id":"noise-new","t":"2026-05-09T10:00:00Z","subject":"Unrelated fresh note"}
	]`)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "FK-412 boundary", Budget: 4096, Explain: true})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	old := itemByURI(t, pack, "old1")
	fresh := itemByURI(t, pack, "fresh1")
	if _, ok := hasReason(old, "exact-identifier"); !ok {
		t.Fatalf("old.Reasons = %+v, want the exact-identifier match this test depends on", old.Reasons)
	}
	if old.Score <= fresh.Score {
		t.Fatalf("old.Score = %d, fresh.Score = %d; a real identifier match must never lose to mere freshness",
			old.Score, fresh.Score)
	}
	if pack.Items[0].URI != old.URI {
		t.Fatalf("items[0] = %s, want the strong older match ranked first despite the fresher record", pack.Items[0].URI)
	}
}

// --- Hermetic gates for a future ranking change --------------------------------------------

// contextReasonVocabulary is every Reason.Reason string BuildContext can produce, documented in
// the table at docs/content/context.md. Nothing enforces that table stays in sync with the
// code on its own, so this list is the other half of the promise: a new scoring signal has to
// touch this test — and, by the same PR, the docs table — before it can ship.
var contextReasonVocabulary = []string{
	"exact-identifier", "exact-phrase", "term", "join-expansion", "superseded", "recency",
	"created-evidence", "navigation-page", "pinned",
}

// TestContextNeverProducesAnUndocumentedReasonKind is a hermetic fixture gate for a ranking
// change: any new scoring signal shows up here as an unrecognised Reason.Reason string, which
// is a build-breaking failure rather than a silent addition nobody had to review.
func TestContextNeverProducesAnUndocumentedReasonKind(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary FK-412", Budget: 4096, Explain: true, Expand: true,
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	for _, item := range pack.Items {
		for _, reason := range item.Reasons {
			found := false
			for _, known := range contextReasonVocabulary {
				if reason.Reason == known {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("reason %q on %s is not in the documented vocabulary %v; "+
					"update both this list and the table in docs/content/context.md",
					reason.Reason, item.URI, contextReasonVocabulary)
			}
		}
	}
}

// TestContextScoringArithmeticIsPinnedByAGoldenFixture is the other hermetic gate: every test
// above this one checks a PROPERTY of the ranking (fresher wins a tie, an identifier beats mere
// freshness); this one checks the actual NUMBER against a fixture chosen so the arithmetic can
// be verified by hand. A change to any constant it touches — pointsIdentifier, pointsTerm,
// maxRarityFactor, or pointsRecencyMax — fails here, which is what forces RankingVersion to be
// bumped deliberately rather than left to drift: its own doc comment promises it changes
// "whenever the arithmetic below changes", and nothing before this test enforced that promise.
func TestContextScoringArithmeticIsPinnedByAGoldenFixture(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	base.Config.Sources["synthetic"].Recency.HalfLifeDays = 7
	// Distinct titles avoid v6's identical-run collapse while keeping equal field lengths and
	// relevance. old1's five half-lives round to zero; fresh1 earns 14 points.
	collect(t, base, "2026-04-05", `[
	  {"id":"old1","t":"2026-04-05T09:00:00Z","subject":"old boundary FK-412","ticket_uri":"ticket:FK-412"},
	  {"id":"noise-old","t":"2026-04-05T10:00:00Z","subject":"Unrelated archival note"}
	]`)
	collect(t, base, "2026-05-09", `[
	  {"id":"fresh1","t":"2026-05-09T09:00:00Z","subject":"new boundary FK-412","ticket_uri":"ticket:FK-412"},
	  {"id":"noise-new","t":"2026-05-09T10:00:00Z","subject":"Unrelated fresh note"}
	]`)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "FK-412 boundary", Budget: 4096, Explain: true,
		Window: services.Window{Since: "2026-04-01", Until: "2026-05-10"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	old := itemByURI(t, pack, "old1")
	fresh := itemByURI(t, pack, "fresh1")
	// +100 exact identifier +50 weighted, length-normalized title term at rarity 2x.
	if old.Score != 150 {
		t.Fatalf("old.Score = %d, want 150 (100 exact identifier + 50 weighted term)", old.Score)
	}
	// old1's total plus the 1-day-old recency bonus computed in the docstring above and
	// verified exactly in TestContextRecencyPrefersAFreshMatchOverAnOldOneAtEqualRelevance.
	if fresh.Score != 164 {
		t.Fatalf("fresh.Score = %d, want 164 (150 baseline + 14 recency)", fresh.Score)
	}
}

// TestContextPhraseScoringIsPinnedByAGoldenFixture is the golden fixture's other half: it pins
// pointsPhrase against wiki/retrieval-boundary.md, the one candidate in contextBase with neither
// a `status:` to supersede it nor a `date:` to earn a recency bonus, so its score is exactly the
// term and phrase contributions with nothing else mixed in.
func TestContextPhraseScoringIsPinnedByAGoldenFixture(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "retrieval boundary", Budget: 4096, Explain: true,
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	page := itemByURI(t, pack, "wiki/retrieval-boundary.md")
	// v6: +100 exact title, +50 each for two rare weighted title terms, +50 exact phrase.
	if page.Score != 250 {
		t.Fatalf("page.Score = %d, want 250 (100 exact title + 100 weighted terms + 50 phrase)", page.Score)
	}
}

// --- Self-framing, budget honesty, and the empty-pack contract ------------------------------

// TestContextAlwaysCarriesTheNotice is the self-framing guarantee: `fkf-hook.sh` — the session-
// start hook every preset installs — calls `fkf context --format text` directly and never goes
// through MCP's Instructions, so the pack is the only place a session driven by it ever sees a
// trust framing at all. It has to be there whether the pack is full or empty.
func TestContextAlwaysCarriesTheNotice(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "retrieval boundary", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Receipt.Notice != services.ContextNotice || pack.Receipt.Notice == "" {
		t.Fatalf("receipt.Notice = %q, want the standard framing on every pack", pack.Receipt.Notice)
	}
	empty, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "zzz-nothing-matches-zzz", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Receipt.Notice != services.ContextNotice {
		t.Fatalf("receipt.Notice = %q on an empty pack, want it unchanged from a full one", empty.Receipt.Notice)
	}
}

// TestContextWarnsWhenTheBudgetIsTooSmallForAnyMatch is the budget-honesty regression test: a
// real match existed — it cleared the floor — but nothing fit inside a small valid budget. The old
// message ("nothing matched; try fewer terms") would have sent a reader in exactly the wrong
// direction; the fix is to raise the budget, not narrow the query.
func TestContextWarnsWhenTheBudgetIsTooSmallForAnyMatch(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	collect(t, base, "2026-05-04", `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"boundary FK-412"}]`)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "FK-412", Budget: 256})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(pack.Items) != 0 {
		t.Fatalf("items = %+v, want a small-budget pack to admit nothing", pack.Items)
	}
	if !strings.Contains(pack.Receipt.Warning, "budget") || !strings.Contains(pack.Receipt.Warning, "raise --budget") {
		t.Fatalf("receipt.Warning = %q, want it to name the budget as the cause and the fix", pack.Receipt.Warning)
	}
}

// TestContextWarnsWhenGenuinelyNothingMatches is the other half: a real, populated base where
// the query itself matches nothing at all — the one case the old blanket message was actually
// right for, and the new Warning must still say so rather than mention a budget that was never
// the problem.
func TestContextWarnsWhenGenuinelyNothingMatches(t *testing.T) {
	base := contextBase(t)
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "zzz-nothing-matches-zzz", Budget: 4096})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if len(pack.Items) != 0 {
		t.Fatalf("items = %+v, want nothing to match this query", pack.Items)
	}
	if pack.Receipt.Warning == "" || strings.Contains(pack.Receipt.Warning, "budget") {
		t.Fatalf("receipt.Warning = %q, want a no-match message that never blames the budget", pack.Receipt.Warning)
	}
}

// TestContextWarnsWhenNoCandidatesExistAtAll is the third empty-pack case: a base with nothing
// collected and no pages written has no candidates to even score, which is a different fact
// from "the query matched nothing among many candidates" and gets its own message.
func TestContextWarnsWhenNoCandidatesExistAtAll(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "anything", Budget: 4096})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if pack.Receipt.Candidates != 0 {
		t.Fatalf("receipt.Candidates = %d, want an empty base to gather nothing", pack.Receipt.Candidates)
	}
	if !strings.Contains(pack.Receipt.Warning, "no candidates") {
		t.Fatalf("receipt.Warning = %q, want it to say the window held nothing to score", pack.Receipt.Warning)
	}
}

// TestContextReportsTheUnharvestedBacklog is the trailing line the context receipt owes the
// `learn` skill's own backlog: `fkf list tasks learned --unharvested` and `fkf status` already surface
// it, but a session driven by `fkf-hook.sh` reads neither of those — it reads this pack, every
// turn, and the backlog stays invisible to it unless the pack says so itself.
func TestContextReportsTheUnharvestedBacklog(t *testing.T) {
	base := newBase(t, baseConfig, &fakeRunner{})
	write(t, base, "tasks/2026-05-04/session/TASKS.md", "# Session\n\n"+
		"## 1. First instruction\n\n## Learned\n\n- A bullet nothing has promoted yet.\n")
	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{Query: "session", Budget: 4096})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if pack.Receipt.UnharvestedBullets != 1 {
		t.Fatalf("receipt.UnharvestedBullets = %d, want the one bullet nothing has cited yet", pack.Receipt.UnharvestedBullets)
	}

	clean := newBase(t, baseConfig, &fakeRunner{})
	cleanPack, err := services.BuildContext(t.Context(), clean, services.ContextRequest{Query: "anything", Budget: 4096})
	if err != nil {
		t.Fatalf("BuildContext() error = %v", err)
	}
	if cleanPack.Receipt.UnharvestedBullets != 0 {
		t.Fatalf("receipt.UnharvestedBullets = %d, want 0 with no traces at all", cleanPack.Receipt.UnharvestedBullets)
	}
}

func TestContextIndexesCustomSourceFields(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"Generic update","topic":"quantum-roadmap"}]`)

	pack, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "quantum-roadmap", Budget: 2048, Window: services.Window{Since: "2026-05-04", Until: "2026-05-04"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) != 1 {
		t.Fatalf("items = %+v, want the record selected through its custom topic field", pack.Items)
	}
	if got := pack.Items[0].Fields["topic"]; !slices.Equal(got, []string{"quantum-roadmap"}) {
		t.Fatalf("custom fields = %+v, want topic projected into the bounded pack", pack.Items[0].Fields)
	}
	if pack.Receipt.InputDigest == "" {
		t.Fatal("custom semantic inputs must remain covered by the receipt digest")
	}
	document, err := base.ReadDocumentContext(t.Context(), "events/2026-05-04/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	document.Records[0]["topic"] = "stellar-roadmap" // same length as quantum-roadmap
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	changed, err := services.BuildContext(t.Context(), base, services.ContextRequest{
		Query: "quantum roadmap", Budget: 2048,
		Window: services.Window{Since: "2026-05-04", Until: "2026-05-04"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Receipt.InputDigest == pack.Receipt.InputDigest {
		t.Fatal("a same-length custom field edit left the semantic-input digest unchanged")
	}
}
