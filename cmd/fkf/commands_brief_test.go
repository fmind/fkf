package main

import (
	"encoding/json"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestBriefAndContextDeltaAreWiredThroughTheCLI(t *testing.T) {
	root := demoBase(t)
	brief := invoke(t, "--format", "json", "--base", root, "brief", "--budget", "1200")
	if brief.code != ExitSuccess {
		t.Fatalf("brief exited %d: %s%s", brief.code, brief.stdout, brief.stderr)
	}
	var report services.BriefReport
	if err := json.Unmarshal([]byte(brief.stdout), &report); err != nil {
		t.Fatalf("decode brief: %v\n%s", err, brief.stdout)
	}
	if len(report.Sections) != 7 || report.Receipt.InputDigest == "" {
		t.Fatalf("brief = %+v, want seven sections and a receipt", report)
	}

	first := invoke(t, "--format", "json", "--base", root, "context", "retrieval", "--budget", "2048")
	if first.code != ExitSuccess {
		t.Fatalf("first context exited %d: %s%s", first.code, first.stdout, first.stderr)
	}
	var full services.ContextPack
	if err := json.Unmarshal([]byte(first.stdout), &full); err != nil {
		t.Fatal(err)
	}
	delta := invoke(t, "--format", "json", "--base", root, "context", "retrieval", "--budget", "2048",
		"--since-receipt", full.Receipt.InputDigest)
	if delta.code != ExitSuccess {
		t.Fatalf("delta context exited %d: %s%s", delta.code, delta.stdout, delta.stderr)
	}
	var unchanged services.ContextPack
	if err := json.Unmarshal([]byte(delta.stdout), &unchanged); err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Items) != 0 || unchanged.Receipt.SinceReceipt != full.Receipt.InputDigest ||
		unchanged.Receipt.InputDigest != full.Receipt.InputDigest {
		t.Fatalf("delta = %+v, want an empty pack bound to the prior full snapshot", unchanged)
	}
}
