package services_test

import (
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestKnowledgeLintSeparatesMissingLinksFromInvalidSupersedesTargets(t *testing.T) {
	config := strings.Replace(baseConfig,
		"  related: {description: Related base resources., cardinality: many, relation: true, examples: [\"projects/fkf-rebuild.md\"]}\n",
		"  related: {description: Related base resources., cardinality: many, relation: true, examples: [\"projects/fkf-rebuild.md\"]}\n"+
			"  supersedes: {description: Replaced knowledge., cardinality: many, relation: true, examples: [\"wiki/old.md\"]}\n", 1)
	base := newBase(t, config, nil)
	write(t, base, "wiki/index.md", "# Index\n")
	write(t, base, "wiki/known.md", "---\ntype: insight\ntitle: Known\ntags: [x]\n---\n\n# Known\n")
	write(t, base, "wiki/source.md", `---
type: decision
title: Source
tags: [x]
relations:
  supersedes:
    - person:email/maxime@example.com
    - known.md#detail
    - missing.md
    - known.md
---

# Source

[Known](known.md), [missing](absent.md), and [external](https://example.test).
`)

	report, err := services.ValidateKnowledgeLint(t.Context(), base, false, 90)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		messages = append(messages, issue.Message)
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{
		"must be a wiki or project page URI",
		"must name a whole page",
		"supersedes target \"missing.md\" does not exist",
		"link target \"absent.md\" does not exist",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("lint messages omit %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "https://example.test") || strings.Contains(joined, "known.md\" does not exist") {
		t.Fatalf("lint rejected an external ordinary link or existing page:\n%s", joined)
	}
}

func TestKnowledgeLintChecksRelationChildrenThroughTheReadBoundary(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "wiki/index.md", "# Index\n")
	write(t, base, "wiki/known.md", "---\ntype: insight\ntitle: Known\ntags: [x]\n---\n\n# Known\n\n## Existing detail\n")
	write(t, base, "wiki/source.md", `---
type: decision
title: Source
tags: [x]
relations:
  related:
    - known.md#existing-detail
    - known.md#missing-detail
---

# Source
`)
	report, err := services.ValidateKnowledgeLint(t.Context(), base, false, 90)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, issue := range report.Issues {
		joined += issue.Message + "\n"
	}
	if !strings.Contains(joined, `relation related target "known.md#missing-detail" is not addressable`) {
		t.Fatalf("lint messages omit the dangling relation child:\n%s", joined)
	}
	if strings.Contains(joined, `relation related target "known.md#existing-detail" is not addressable`) {
		t.Fatalf("lint rejected an existing relation child:\n%s", joined)
	}
}
