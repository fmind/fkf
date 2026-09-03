package services_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const validQueuedLearnDiff = "--- a/wiki/log.md\n+++ b/wiki/log.md\n@@ -1 +1 @@\n-# Log\n+# Updated log\n"

func TestLearnReviewListsOnlyValidProposalFilesInStableOrder(t *testing.T) {
	base := learnBase(t)
	empty, err := services.ReviewLearn(t.Context(), base, "", false)
	if err != nil || len(empty.Proposals) != 0 {
		t.Fatalf("empty ReviewLearn() = %+v, error %v", empty, err)
	}
	if _, err := services.ReviewLearn(t.Context(), base, "", true); err == nil ||
		!strings.Contains(err.Error(), "requires one proposal id") {
		t.Fatalf("ReviewLearn(includeDiff without id) error = %v", err)
	}

	firstID := writeLearnProposal(t, base, validQueuedLearnDiff)
	secondDiff := strings.Replace(validQueuedLearnDiff, "Updated log", "Another log", 1)
	secondID := writeLearnProposal(t, base, secondDiff)
	wantIDs := []string{firstID, secondID}
	sort.Strings(wantIDs)
	queue := filepath.Join(base.Root(), ".agents", "tmp", "learn")
	if err := os.WriteFile(filepath.Join(queue, "README.txt"), []byte("ignored"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(queue, "directory.diff"), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	review, err := services.ReviewLearn(t.Context(), base, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Proposals) != 2 || review.Proposals[0].ID != wantIDs[0] || review.Proposals[1].ID != wantIDs[1] {
		t.Fatalf("proposal order = %+v", review.Proposals)
	}
	if review.Proposals[0].Diff != "" || review.Proposals[0].Bytes != len(validQueuedLearnDiff) {
		t.Fatalf("queue summary unexpectedly included diff or wrong size: %+v", review.Proposals[0])
	}

	if err := os.WriteFile(filepath.Join(queue, "INVALID.diff"), []byte(validQueuedLearnDiff), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := services.ReviewLearn(t.Context(), base, "", false); err == nil ||
		!strings.Contains(err.Error(), "invalid filename") {
		t.Fatalf("invalid queue filename error = %v", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := services.ReviewLearn(canceled, base, "", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReviewLearn() error = %v", err)
	}
}

func TestLearnTransitionsRefuseInvalidMissingAndTerminalStates(t *testing.T) {
	base := learnBase(t)
	for _, id := range []string{"../escape", "UPPERCASE", strings.Repeat("a", 65)} {
		if _, err := services.ReviewLearn(t.Context(), base, id, false); !errors.Is(err, core.ErrConfig) {
			t.Errorf("ReviewLearn(%q) error = %v, want ErrConfig", id, err)
		}
		if _, err := services.RejectLearn(t.Context(), base, id); !errors.Is(err, core.ErrConfig) {
			t.Errorf("RejectLearn(%q) error = %v, want ErrConfig", id, err)
		}
		if _, err := services.ApplyLearn(t.Context(), base, id); !errors.Is(err, core.ErrConfig) {
			t.Errorf("ApplyLearn(%q) error = %v, want ErrConfig", id, err)
		}
	}
	for _, action := range []struct {
		name string
		run  func() error
	}{
		{"reject", func() error { _, err := services.RejectLearn(t.Context(), base, "missing"); return err }},
		{"apply", func() error { _, err := services.ApplyLearn(t.Context(), base, "missing"); return err }},
	} {
		if err := action.run(); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s missing proposal error = %v, want os.ErrNotExist", action.name, err)
		}
	}

	appliedBase := learnBase(t)
	appliedProposal, err := services.ProposeLearn(t.Context(), appliedBase, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.ApplyLearn(t.Context(), appliedBase, appliedProposal.Proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := services.RejectLearn(t.Context(), appliedBase, appliedProposal.Proposal.ID); err == nil ||
		!strings.Contains(err.Error(), "already applied") {
		t.Fatalf("reject applied proposal error = %v", err)
	}

	rejectedBase := learnBase(t)
	rejectedProposal, err := services.ProposeLearn(t.Context(), rejectedBase, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.RejectLearn(t.Context(), rejectedBase, rejectedProposal.Proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := services.ApplyLearn(t.Context(), rejectedBase, rejectedProposal.Proposal.ID); err == nil ||
		!strings.Contains(err.Error(), "was rejected") {
		t.Fatalf("apply rejected proposal error = %v", err)
	}
}

func TestLearnProposalReportsNothingAndRefusesDigestCollision(t *testing.T) {
	nothingBase := learnBase(t)
	write(t, nothingBase, "tasks/2026-05-09/kagglathon-abc/TASKS.md", "# Session\n\nNo learned section.\n")
	nothing, err := services.ProposeLearn(t.Context(), nothingBase, false)
	if err != nil {
		t.Fatal(err)
	}
	if !nothing.Nothing || nothing.Proposal != nil || len(nothing.Candidates) != 0 {
		t.Fatalf("nothing-to-propose report = %+v", nothing)
	}
	if _, err := os.Stat(filepath.Join(nothingBase.Root(), ".agents", "tmp", "learn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing-to-propose created a queue: %v", err)
	}

	collisionBase := learnBase(t)
	proposal, err := services.ProposeLearn(t.Context(), collisionBase, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(collisionBase.Root(), ".agents", "tmp", "learn", proposal.Proposal.ID+".diff")
	const collision = "different bytes\n"
	if err := os.WriteFile(path, []byte(collision), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := services.ProposeLearn(t.Context(), collisionBase, false); err == nil ||
		!strings.Contains(err.Error(), "proposal id collision") {
		t.Fatalf("digest collision error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != collision {
		t.Fatalf("collision changed queued bytes to %q, error %v", got, err)
	}
}

func TestLearnReviewRefusesASymlinkedQueue(t *testing.T) {
	base := learnBase(t)
	parent := filepath.Join(base.Root(), ".agents", "tmp")
	if err := os.MkdirAll(parent, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(parent, "learn")); err != nil {
		t.Fatal(err)
	}
	if _, err := services.ReviewLearn(t.Context(), base, "", false); err == nil ||
		!strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("symlinked learn queue error = %v", err)
	}
}
