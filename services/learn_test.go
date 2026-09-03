package services_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const learnConfig = `fkf: 1
name: learn-test
schema:
  id: {description: Stable record identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Human-readable title., cardinality: optional}
  related: {description: Related resource., cardinality: many, relation: true}
layers: {tasks: true, projects: true, wiki: true}
sources: {}
`

func learnBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t, learnConfig, nil)
	write(t, base, "wiki/index.md", "# Wiki\n")
	write(t, base, "wiki/log.md", "# Log\n\n## 2026-05-09\n\n- Existing entry.\n")
	write(t, base, "tasks/2026-05-09/kagglathon-abc/TASKS.md",
		"# Session\n\n## Learned\n\n- Preserve the bounded trust digest.\n")
	return base
}

func TestLearnProposalDryRunListsCitedCandidatesWithoutWriting(t *testing.T) {
	base := learnBase(t)
	report, err := services.ProposeLearn(t.Context(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Nothing || report.Proposal != nil || len(report.Candidates) != 1 {
		t.Fatalf("ProposeLearn(dry-run) = %+v, want one non-writing candidate", report)
	}
	candidate := report.Candidates[0]
	if candidate.Trace != "tasks/2026-05-09/kagglathon-abc/TASKS.md" ||
		candidate.Text != "Preserve the bounded trust digest." || candidate.Target != "wiki/log.md" {
		t.Fatalf("candidate = %+v, want the exact trace-backed log bullet", candidate)
	}
	if _, err := os.Lstat(filepath.Join(base.Root(), ".agents", "tmp", "learn")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dry-run created the proposal queue: %v", err)
	}
}

func TestLearnProposalReviewApplyAndRepeatAreIdempotent(t *testing.T) {
	base := learnBase(t)
	proposed, err := services.ProposeLearn(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Proposal == nil || proposed.Existing || len(proposed.Proposal.Files) != 1 ||
		proposed.Proposal.Files[0] != "wiki/log.md" {
		t.Fatalf("ProposeLearn() = %+v, want one new log diff", proposed)
	}
	id := proposed.Proposal.ID
	second, err := services.ProposeLearn(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Proposal == nil || second.Proposal.ID != id || !second.Existing {
		t.Fatalf("second ProposeLearn() = %+v, want the same queued diff", second)
	}
	review, err := services.ReviewLearn(t.Context(), base, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Proposals) != 1 || !strings.Contains(review.Proposals[0].Diff, "+++ b/wiki/log.md") ||
		!strings.Contains(review.Proposals[0].Diff, "../tasks/2026-05-09/kagglathon-abc/TASKS.md#learned") ||
		!strings.Contains(review.Proposals[0].Diff, "+- Preserve the bounded trust digest.") {
		t.Fatalf("review = %+v, want a trace-citing unified diff", review)
	}

	applied, err := services.ApplyLearn(t.Context(), base, id)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.Build == nil || len(applied.Validations) != 1 || !applied.Validations[0].OK {
		t.Fatalf("ApplyLearn() = %+v, want strict validation and a cache rebuild", applied)
	}
	logBytes, err := os.ReadFile(mustResolve(t, base, "wiki/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "## 2026-05-10\n\n- Preserve the bounded trust digest.") ||
		!strings.Contains(log, "../tasks/2026-05-09/kagglathon-abc/TASKS.md#learned") {
		t.Fatalf("applied wiki/log.md omits the candidate or provenance:\n%s", log)
	}
	if _, err := os.Stat(mustResolve(t, base, core.GraphFile)); err != nil {
		t.Fatalf("apply did not rebuild the graph: %v", err)
	}
	backlog, err := services.ListLearned(t.Context(), base, services.Window{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Unharvested != 0 || len(backlog.Bullets) != 0 {
		t.Fatalf("backlog after apply = %+v, want the cited trace harvested", backlog)
	}
	repeated, err := services.ApplyLearn(t.Context(), base, id)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != "already-applied" {
		t.Fatalf("repeated apply = %+v, want an idempotent archive result", repeated)
	}
}

func TestLearnRejectArchivesWithoutChangingPages(t *testing.T) {
	base := learnBase(t)
	before, err := os.ReadFile(mustResolve(t, base, "wiki/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := services.ProposeLearn(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	id := proposal.Proposal.ID
	rejected, err := services.RejectLearn(t.Context(), base, id)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != "rejected" || !strings.Contains(rejected.Path, "/rejected/") {
		t.Fatalf("RejectLearn() = %+v", rejected)
	}
	after, err := os.ReadFile(mustResolve(t, base, "wiki/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("reject changed an authored page")
	}
	repeated, err := services.RejectLearn(t.Context(), base, id)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != "already-rejected" {
		t.Fatalf("repeated reject = %+v", repeated)
	}
}

func TestLearnApplyRejectsBytesReplacedAfterReview(t *testing.T) {
	base := learnBase(t)
	proposal, err := services.ProposeLearn(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	id := proposal.Proposal.ID
	if _, err := services.ReviewLearn(t.Context(), base, id, true); err != nil {
		t.Fatal(err)
	}
	tampered := "--- a/wiki/log.md\n+++ b/wiki/log.md\n@@ -1 +1 @@\n-# Log\n+# Replaced after review\n"
	path := filepath.Join(base.Root(), ".agents", "tmp", "learn", id+".diff")
	if err := os.WriteFile(path, []byte(tampered), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := services.ApplyLearn(t.Context(), base, id); err == nil ||
		!strings.Contains(err.Error(), "does not match its SHA-256 digest") {
		t.Fatalf("ApplyLearn() error = %v, want reviewed-byte digest mismatch", err)
	}
	data, err := os.ReadFile(mustResolve(t, base, "wiki/log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Replaced after review") {
		t.Fatal("replacement proposal bytes reached durable knowledge")
	}
}

func TestLearnApplyRejectsTargetsOutsideFlatKnowledgePages(t *testing.T) {
	base := learnBase(t)
	id := writeLearnProposal(t, base, ""+
		"--- /dev/null\n"+
		"+++ b/events/2026-05-10/escape.md\n"+
		"@@ -0,0 +1,1 @@\n"+
		"+outside\n")
	_, err := services.ApplyLearn(t.Context(), base, id)
	if err == nil || !strings.Contains(err.Error(), "flat wiki/*.md or projects/*.md") {
		t.Fatalf("ApplyLearn() error = %v, want the closed target grammar", err)
	}
	if _, statErr := os.Stat(filepath.Join(base.Root(), "events", "2026-05-10", "escape.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("invalid proposal escaped its target boundary: %v", statErr)
	}
}

func TestLearnApplyRollsBackAProposalThatFailsStrictValidation(t *testing.T) {
	base := learnBase(t)
	id := writeLearnProposal(t, base, ""+
		"--- /dev/null\n"+
		"+++ b/projects/invalid.md\n"+
		"@@ -0,0 +1,6 @@\n"+
		"+---\n"+
		"+type: project\n"+
		"+tags: [invalid]\n"+
		"+---\n"+
		"+\n"+
		"+# Missing status\n")
	_, err := services.ApplyLearn(t.Context(), base, id)
	if err == nil || !strings.Contains(err.Error(), "frontmatter `status` is required") {
		t.Fatalf("ApplyLearn() error = %v, want strict projects validation", err)
	}
	if _, statErr := os.Stat(mustResolve(t, base, "projects/invalid.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("failed proposal left its new page behind: %v", statErr)
	}
	review, reviewErr := services.ReviewLearn(t.Context(), base, id, true)
	if reviewErr != nil || len(review.Proposals) != 1 {
		t.Fatalf("failed proposal was not left reviewable: review=%+v error=%v", review, reviewErr)
	}
}

func TestLearnApplyRestoresWikiCacheWhenGraphBuildFails(t *testing.T) {
	base := learnBase(t)
	if _, err := services.Build(t.Context(), base, "", false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(mustResolve(t, base, "wiki/index.md"))
	if err != nil {
		t.Fatal(err)
	}
	id := writeLearnProposal(t, base, ""+
		"--- /dev/null\n"+
		"+++ b/wiki/broken-relation.md\n"+
		"@@ -0,0 +1,11 @@\n"+
		"+---\n"+
		"+type: insight\n"+
		"+title: Broken relation\n"+
		"+tags: [test]\n"+
		"+relations:\n"+
		"+  related: [log.md#missing-heading]\n"+
		"+---\n"+
		"+\n"+
		"+# Broken relation\n"+
		"+\n"+
		"+This page makes graph construction reject a missing child.\n")
	_, err = services.ApplyLearn(t.Context(), base, id)
	if err == nil || !strings.Contains(err.Error(), "fragment does not name an addressable child") {
		t.Fatalf("ApplyLearn() error = %v, want a late graph-build failure", err)
	}
	if _, statErr := os.Lstat(mustResolve(t, base, "wiki/broken-relation.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("failed proposal left its authored page: %v", statErr)
	}
	after, readErr := os.ReadFile(mustResolve(t, base, "wiki/index.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatal("late graph failure did not restore the prior wiki index bytes")
	}
}

func TestLearnApplyRestoresEveryDerivedCacheWhenArchivingFails(t *testing.T) {
	base := learnBase(t)
	if _, err := services.Build(t.Context(), base, "", false); err != nil {
		t.Fatal(err)
	}
	proposal, err := services.ProposeLearn(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"wiki/log.md", "wiki/index.md", core.GraphFile, core.GraphDstFile,
		core.GraphOffsetsFile, core.GraphMetaFile, core.GraphGenerationFile,
		services.LexicalIndexPath, "index/.fkf-index.meta.json",
	}
	before := make(map[string][]byte, len(paths))
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(base.Root(), filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read baseline %s: %v", relative, err)
		}
		before[relative] = data
	}

	queue := filepath.Join(base.Root(), ".agents", "tmp", "learn")
	if err := os.Mkdir(filepath.Join(queue, "applied"), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(queue, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queue, core.BaseDirMode) })
	_, err = services.ApplyLearn(t.Context(), base, proposal.Proposal.ID)
	if err == nil || !strings.Contains(err.Error(), "archive applied proposal") {
		t.Fatalf("ApplyLearn() error = %v, want the post-build archive failure", err)
	}
	if err := os.Chmod(queue, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	for _, relative := range paths {
		after, err := os.ReadFile(filepath.Join(base.Root(), filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read restored %s: %v", relative, err)
		}
		if string(after) != string(before[relative]) {
			t.Errorf("failed apply did not restore exact bytes for %s", relative)
		}
	}
	if review, err := services.ReviewLearn(t.Context(), base, proposal.Proposal.ID, true); err != nil || len(review.Proposals) != 1 {
		t.Fatalf("failed proposal was not left reviewable: review=%+v error=%v", review, err)
	}
}

func writeLearnProposal(t *testing.T, base *services.Base, diff string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(diff))
	id := hex.EncodeToString(digest[:])
	directory := filepath.Join(base.Root(), ".agents", "tmp", "learn")
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, id+".diff"), []byte(diff), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	return id
}
