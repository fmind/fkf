package services_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
)

func TestKnowledgeLintNamesOrphansDatesStaleProjectsAndSupersedesGaps(t *testing.T) {
	config := strings.Replace(baseConfig,
		"  related: {description: Related base resources., cardinality: many, relation: true, examples: [\"projects/fkf-rebuild.md\"]}\n",
		"  related: {description: Related base resources., cardinality: many, relation: true, examples: [\"projects/fkf-rebuild.md\"]}\n"+
			"  supersedes: {description: Older knowledge replaced by this page., cardinality: many, relation: true, examples: [\"wiki/old.md\"]}\n", 1)
	base := newBase(t, config, nil)
	write(t, base, "wiki/index.md", "# Index\n\n[Linked](linked.md)\n")
	write(t, base, "wiki/linked.md", "---\ntype: insight\ntitle: Linked\ntags: [x]\n---\n\n# Linked\n")
	write(t, base, "wiki/orphan.md", `---
type: decision
title: Orphan
tags: [x]
review_date: yesterday
relations:
  supersedes: [wiki/missing.md]
---

# Orphan
`)
	write(t, base, "projects/stale.md", `---
type: project
title: Stale
status: active
tags: [x]
valid_until: 2026-05-09
---

# Stale
`)
	old := testClock.Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(mustResolve(t, base, "projects/stale.md"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := services.BuildWikiIndex(t.Context(), base, true); err != nil {
		t.Fatal(err)
	}

	report, err := services.ValidateKnowledgeLint(t.Context(), base, false, 90)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, issue := range report.Issues {
		joined += issue.Message + "\n"
	}
	for _, want := range []string{"orphan", "relative date", "supersedes target", "untouched for", "valid_until"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lint issues omit %q:\n%s", want, joined)
		}
	}
	if !report.OK || report.Warnings < 5 {
		t.Fatalf("lint report = %+v, want advisory findings", report)
	}
	for _, issue := range report.Issues {
		if issue.URI == "wiki/linked.md" && strings.Contains(issue.Message, "orphan") {
			t.Fatalf("curated index link did not count as inbound: %+v", issue)
		}
	}
	if !slices.ContainsFunc(report.Issues, func(issue services.Issue) bool {
		return issue.URI == "wiki/orphan.md" && strings.Contains(issue.Message, "orphan")
	}) {
		t.Fatalf("generated index listing hid the orphan: %+v", report.Issues)
	}
}
