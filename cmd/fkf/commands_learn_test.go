package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestLearnCLIStagesReviewsAndAppliesAnExactDiff(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	config := cliTestContract + `name: learn-cli
layers: {tasks: true, projects: true, wiki: true}
sources: {}
`
	writeLearnCLIFile(t, root, core.ConfigFileName, config)
	writeLearnCLIFile(t, root, "wiki/index.md", "# Wiki\n")
	writeLearnCLIFile(t, root, "wiki/log.md", "# Log\n\n## 2026-05-09\n\n- Existing entry.\n")
	writeLearnCLIFile(t, root, "tasks/2026-05-09/fkf-session/TASKS.md",
		"# Session\n\n## Learned\n\n- Keep learn proposals reviewable.\n")

	dryRun := invoke(t, "--base", root, "learn", "propose", "--dry-run")
	if dryRun.code != ExitSuccess || !strings.Contains(dryRun.stdout, "Keep learn proposals reviewable.") {
		t.Fatalf("learn propose --dry-run = exit %d stdout %q stderr %q", dryRun.code, dryRun.stdout, dryRun.stderr)
	}
	if _, err := os.Lstat(filepath.Join(root, ".agents", "tmp", "learn")); !os.IsNotExist(err) {
		t.Fatalf("learn propose --dry-run created the queue: %v", err)
	}

	staged := invoke(t, "--format", "json", "--base", root, "learn", "propose")
	if staged.code != ExitSuccess {
		t.Fatalf("learn propose = exit %d: %s%s", staged.code, staged.stdout, staged.stderr)
	}
	var proposal services.LearnProposalReport
	if err := json.Unmarshal([]byte(staged.stdout), &proposal); err != nil {
		t.Fatalf("decode learn proposal: %v\n%s", err, staged.stdout)
	}
	if proposal.Proposal == nil || proposal.Proposal.ID == "" {
		t.Fatalf("learn proposal = %+v, want one staged diff", proposal)
	}

	reviewed := invoke(t, "--base", root, "learn", "review", proposal.Proposal.ID, "--diff")
	if reviewed.code != ExitSuccess || !strings.Contains(reviewed.stdout, "+++ b/wiki/log.md") ||
		!strings.Contains(reviewed.stdout, "../tasks/2026-05-09/fkf-session/TASKS.md#learned") {
		t.Fatalf("learn review --diff = exit %d stdout %q stderr %q", reviewed.code, reviewed.stdout, reviewed.stderr)
	}

	applied := invoke(t, "--format", "json", "--base", root, "learn", "apply", proposal.Proposal.ID)
	if applied.code != ExitSuccess {
		t.Fatalf("learn apply = exit %d: %s%s", applied.code, applied.stdout, applied.stderr)
	}
	var action services.LearnActionReport
	if err := json.Unmarshal([]byte(applied.stdout), &action); err != nil {
		t.Fatalf("decode learn action: %v\n%s", err, applied.stdout)
	}
	if action.Status != "applied" || action.Build == nil || action.Build.Index == nil {
		t.Fatalf("learn action = %+v, want validation and all derived caches rebuilt", action)
	}
	updated, err := os.ReadFile(filepath.Join(root, "wiki", "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "Keep learn proposals reviewable.") {
		t.Fatalf("applied wiki/log.md = %q, want the reviewed lesson", updated)
	}

	invalid := invoke(t, "--base", root, "learn", "review", "--diff")
	if invalid.code != ExitInvalidUsage || !strings.Contains(invalid.stderr, "usage: fkf learn review <proposal> --diff") {
		t.Fatalf("learn review --diff = exit %d stdout %q stderr %q, want usage error", invalid.code, invalid.stdout, invalid.stderr)
	}
}

func writeLearnCLIFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(contents), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}
