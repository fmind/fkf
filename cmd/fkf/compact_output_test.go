package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestCLIContextTextUsesCompactLinesAndItsDeliveredBudget(t *testing.T) {
	root := demoBase(t)
	const budget = 900
	got := invoke(t, "--format", "text", "--base", root, "context", "retrieval boundary", "--budget", "900")
	if got.code != ExitSuccess {
		t.Fatalf("exit = %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if tokens := (len(got.stdout) + 3) / 4; tokens > budget {
		t.Fatalf("text delivery = %d tokens, budget %d\n%s", tokens, budget, got.stdout)
	}
	for _, want := range []string{
		"notice ", " record ", "receipt pack for", "window ", "digest ", " text tokens", "· index index/.fkf-index.tsv used",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("compact context = %q, want %q", got.stdout, want)
		}
	}
	if count := strings.Count(got.stdout, "\nreceipt ") + strings.Count(got.stdout, "\nwindow ") +
		strings.Count(got.stdout, "\ndigest "); count != 3 {
		t.Fatalf("compact context has %d receipt lines, want 3:\n%s", count, got.stdout)
	}
	receipt := got.stdout[strings.Index(got.stdout, "receipt pack for"):]
	var query string
	var selected, candidates int
	if _, err := fmt.Sscanf(receipt, "receipt pack for %q · %d/%d selected", &query, &selected, &candidates); err != nil {
		t.Fatalf("parse compact receipt: %v\n%s", err, got.stdout)
	}
	if selected < 13 {
		t.Fatalf("compact context selected %d/%d items, want at least 13 demo items under 900 tokens:\n%s",
			selected, candidates, got.stdout)
	}
}

func TestCLIContextTextRejectsABudgetBelowItsOwnReceiptFloor(t *testing.T) {
	root := demoBase(t)
	got := invoke(t, "--format", "text", "--base", root, "context", "retrieval", "--budget", "1")
	if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "smallest honest pack") {
		t.Fatalf("exit = %d stdout=%q stderr=%q, want the text receipt floor", got.code, got.stdout, got.stderr)
	}
}

func TestCLIContextJSONLDeliversOneBoundedPackWithItsReceipt(t *testing.T) {
	root := demoBase(t)
	const budget = 900
	got := invoke(t, "--format", "jsonl", "--base", root,
		"context", "retrieval boundary", "--budget", "900")
	if got.code != ExitSuccess {
		t.Fatalf("exit = %d: %s%s", got.code, got.stdout, got.stderr)
	}
	if strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("JSONL context = %q, want one compact pack line", got.stdout)
	}
	var pack services.ContextPack
	if err := json.Unmarshal([]byte(got.stdout), &pack); err != nil {
		t.Fatalf("decode JSONL context pack: %v\n%s", err, got.stdout)
	}
	if pack.Receipt.Format != services.ContextDeliveryJSONL ||
		pack.Receipt.Selected != len(pack.Items) {
		t.Fatalf("JSONL receipt = %+v for %d items", pack.Receipt, len(pack.Items))
	}
	actual := (len(got.stdout) + 3) / 4
	if actual != pack.Receipt.EncodedTokens || actual > budget {
		t.Fatalf("JSONL delivery = %d tokens, receipt = %d, budget = %d",
			actual, pack.Receipt.EncodedTokens, budget)
	}
}

func TestCLIFindStructuredOutputIsCompactUnlessRaw(t *testing.T) {
	root := demoBase(t)
	compact := invoke(t, "--format", "json", "--base", root, "find", "--limit", "1")
	if compact.code != ExitSuccess {
		t.Fatalf("compact find: %s%s", compact.stdout, compact.stderr)
	}
	var got struct {
		Days    []string `json:"days"`
		Records []struct {
			Record map[string]any `json:"record"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(compact.stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Days) != 0 || len(got.Records) == 0 || got.Records[0].Record != nil {
		t.Fatalf("compact find = %+v, want URIs/projections without days or raw records", got)
	}

	raw := invoke(t, "--format", "json", "--base", root, "find", "--limit", "1", "--raw")
	if raw.code != ExitSuccess {
		t.Fatalf("raw find: %s%s", raw.stdout, raw.stderr)
	}
	got = struct {
		Days    []string `json:"days"`
		Records []struct {
			Record map[string]any `json:"record"`
		} `json:"records"`
	}{}
	if err := json.Unmarshal([]byte(raw.stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Days) == 0 || len(got.Records) == 0 || len(got.Records[0].Record) == 0 {
		t.Fatalf("raw find = %+v, want days and provider record", got)
	}
}
