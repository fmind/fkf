package services_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func findFinding(status *services.Status, check string) *services.Finding {
	for index := range status.Findings {
		if status.Findings[index].Check == check {
			return &status.Findings[index]
		}
	}
	return nil
}

func TestStatusReportsReadinessAndTrust(t *testing.T) {
	config := strings.Replace(baseConfig, "    run: [cli", "    requires: [cli]\n    run: [cli", 1)
	base := newBase(t, config, nil)
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "brain" || len(status.Layers) != 5 {
		t.Fatalf("status = %+v", status)
	}
	if status.Trust.Trusted {
		t.Fatal("a base nobody trusted here must say so, because sync will refuse")
	}
	if len(status.Sources) != 1 {
		t.Fatalf("sources = %+v", status.Sources)
	}
	source := status.Sources[0]
	if len(source.Requires) != 1 || source.Requires[0].Name != "cli" || source.Requires[0].OnPath {
		t.Fatalf("source = %+v, want the explicitly declared requirement reported as missing", source)
	}
	if status.Missing != 1 {
		t.Fatalf("missing = %d, want the one missing enabled-source requirement", status.Missing)
	}
	// A fresh, untrusted, unversioned base with no derived indexes has findings for trust, git, and derived.
	for _, check := range []string{"trust", "git", "derived"} {
		finding := findFinding(status, check)
		if finding == nil {
			t.Fatalf("findings = %+v, want a %s finding", status.Findings, check)
		}
		if finding.Fix == "" {
			t.Fatalf("finding = %+v, want a remedy", finding)
		}
	}
	if !status.OK {
		t.Fatalf("status = %+v, want warnings only on a fresh base", status)
	}
	prefix := "fkf --base '" + base.Root() + "' "
	for _, next := range status.Next {
		if !strings.HasPrefix(next, prefix) {
			t.Errorf("next command %q is not bound to the status base with prefix %q", next, prefix)
		}
	}
	for _, finding := range status.Findings {
		if strings.HasPrefix(finding.Fix, "fkf ") &&
			!strings.HasPrefix(finding.Fix, prefix) &&
			!strings.HasPrefix(finding.Fix, "fkf init '") {
			t.Errorf("finding fix %q is not bound to the status base", finding.Fix)
		}
	}
}

func TestStatusShellQuotesGitRemediesForAnArbitraryBasePath(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain space'quote;not-a-command")
	if err := os.MkdirAll(root, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName),
		[]byte(withServiceTestContract(baseConfig)), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base := openBase(t, root, nil)
	quoted := "'" + strings.ReplaceAll(root, "'", `'"'"'`) + "'"

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if finding := findFinding(status, "git"); finding == nil || finding.Fix != "git init "+quoted {
		t.Fatalf("git finding = %+v, want a shell-quoted base path", finding)
	}

	git(t, root, "init")
	status, err = services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := "git -C " + quoted + " add -A && git -C " + quoted + " commit -m 'chore: first snapshot'"
	if finding := findFinding(status, "uncommitted"); finding == nil || finding.Fix != want {
		t.Fatalf("uncommitted finding = %+v, want %q", finding, want)
	}
}

func TestStatusQuietWatchdog(t *testing.T) {
	fill := func(t *testing.T, counts []int) *services.Status {
		t.Helper()
		base := newBase(t, baseConfig, nil)
		for index, count := range counts {
			date := fmt.Sprintf("2026-04-%02d", index+1)
			records := make([]string, 0, count)
			for record := range count {
				records = append(records, fmt.Sprintf(
					`{"id":"r%d","t":"%sT09:00:00Z","subject":"s","link":"https://x.test","repo":"o/r","who":"m@x.test"}`,
					record, date,
				))
			}
			collect(t, base, date, "["+strings.Join(records, ",")+"]")
		}
		status, err := services.Report(t.Context(), base, services.StatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return status
	}

	t.Run("arms only after a week of history", func(t *testing.T) {
		status := fill(t, []int{10, 10, 10, 0})
		if status.Sources[0].Quiet {
			t.Fatal("four days is not enough history to accuse a source of having stopped")
		}
	})
	t.Run("flags a day that returned nothing", func(t *testing.T) {
		status := fill(t, []int{10, 12, 9, 11, 10, 13, 10, 11, 0})
		if !status.Sources[0].Quiet {
			t.Fatalf("source = %+v, want it flagged quiet", status.Sources[0])
		}
		if !strings.Contains(status.Sources[0].QuietReason, "median") {
			t.Fatalf("reason = %q, want it to compare against the median", status.Sources[0].QuietReason)
		}
		if status.Quiet != 1 {
			t.Fatalf("quiet = %d, want one", status.Quiet)
		}
	})
	t.Run("flags a collapse below a fifth of the median", func(t *testing.T) {
		status := fill(t, []int{100, 120, 90, 110, 100, 130, 100, 110, 3})
		if !status.Sources[0].Quiet {
			t.Fatalf("source = %+v, want a collapse flagged", status.Sources[0])
		}
	})
	t.Run("leaves an ordinary day alone", func(t *testing.T) {
		status := fill(t, []int{10, 12, 9, 11, 10, 13, 10, 11, 8})
		if status.Sources[0].Quiet {
			t.Fatalf("source = %+v, want an ordinary day left alone", status.Sources[0])
		}
		if status.Sources[0].Median == 0 {
			t.Fatal("the median has to be reported once the watchdog is armed")
		}
	})
}

func TestStatusReportsUndeclaredHistory(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	reopened := newBase(t, strings.Replace(baseConfig, "    enabled: true", "    enabled: false", 1), nil)
	collect(t, reopened, "2026-05-04", dayOne)
	status, err := services.Report(t.Context(), reopened, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.LastSync != "2026-05-04" {
		t.Fatalf("last sync = %q, want the day that is on disk", status.LastSync)
	}
}

func TestStatusReportsAnUndeclaredIndexSnapshot(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "retired-index", Layer: core.LayerIndex,
		CollectedAt: testClock.UTC().Format(time.RFC3339),
		Fields: sources.Fields{
			core.FieldID:    {mustFieldPath(t, ".id")},
			core.FieldTitle: {mustFieldPath(t, ".title")},
		},
		Count: 1, Records: []sources.Record{{"id": "retired-1", "title": "Retained snapshot"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range status.Sources {
		if source.Name != document.Source {
			continue
		}
		if !source.Undeclared || source.Kind != core.LayerIndex || source.LastCount != 1 || source.Days != 1 {
			t.Fatalf("source = %+v, want the retained undeclared index snapshot named with its volume", source)
		}
		return
	}
	t.Fatalf("sources = %+v, want undeclared index source %q", status.Sources, document.Source)
}

func TestStatusStalenessIsAnExitCode(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", dayOne)
	fresh, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24 * 30})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Stale {
		t.Fatal("a day inside the window is not stale")
	}
	old, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if !old.Stale {
		t.Fatalf("status = %+v, want it stale so a timer can read the exit code", old)
	}
}

func TestStatusHourFreshnessUsesTheEvidenceBoundary(t *testing.T) {
	t.Run("completed event day", func(t *testing.T) {
		base := newBase(t, baseConfig, nil)
		collect(t, base, "2026-05-09", strings.ReplaceAll(dayOne, "2026-05-04", "2026-05-09"))

		status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
		if err != nil {
			t.Fatal(err)
		}
		if status.Stale {
			t.Fatalf("status = %+v, want yesterday's completed day fresh for 24 hours after its boundary", status)
		}
	})

	t.Run("same-day index snapshot", func(t *testing.T) {
		base := newBase(t, indexFreshnessConfig, nil)
		writeIndexFreshnessSnapshot(t, base, testClock.Add(-30*time.Minute), testClock.Add(-30*time.Minute))

		status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 1})
		if err != nil {
			t.Fatal(err)
		}
		if status.Stale {
			t.Fatalf("status = %+v, want an index collected 30 minutes ago fresh for one hour", status)
		}
	})
}

func TestStatusFreshnessIsPerEnabledSource(t *testing.T) {
	config := baseConfig + `
  missing:
    enabled: true
    layer: events
    run: [cli, --since, "{{date}}", --until, "{{next_date}}"]
    fields:
      id: .id
      time: .t
`
	base := newBase(t, config, nil)
	document := collect(t, base, "2026-05-09", strings.ReplaceAll(dayOne, "2026-05-04", "2026-05-09"))

	status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale {
		t.Fatal("one enabled source without evidence must make the base stale")
	}
	byName := map[string]services.SourceStatus{}
	for _, source := range status.Sources {
		byName[source.Name] = source
	}
	fresh := byName["synthetic"]
	boundary, err := time.Parse(time.RFC3339, document.WindowEnd)
	if err != nil {
		t.Fatal(err)
	}
	wantLagHours := int(testClock.Sub(boundary) / time.Hour)
	if fresh.LastCollectedAt != document.WindowEnd || fresh.LagHours != wantLagHours || fresh.Stale {
		t.Fatalf("fresh source = %+v, want its exact evidence boundary and %d-hour lag", fresh, wantLagHours)
	}
	missing := byName["missing"]
	if missing.LastCollectedAt != "" || missing.LagHours != 0 || !missing.Stale {
		t.Fatalf("missing source = %+v, want no invented boundary and stale = true", missing)
	}
}

func TestStatusChecksExplicitRequirementsAgainstTheRunnersPath(t *testing.T) {
	config := strings.Replace(baseConfig, "    run: [cli", "    requires: [helper-script]\n    run: [cli", 1)
	base := newBase(t, config, nil)
	if err := sources.EnsureBinDir(base.Root()); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(base.Root(), core.BaseBinDir, "helper-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '[]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Sources[0].Requires) != 1 || !status.Sources[0].Requires[0].OnPath || status.Missing != 0 {
		t.Fatalf("status = %+v, want the base's own bin/ searched", status.Sources[0])
	}
	absent := newBase(t, strings.Replace(config, "requires: [helper-script]",
		"requires: [definitely-not-installed-anywhere]", 1), nil)
	report, err := services.Report(t.Context(), absent, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sources[0].Requires[0].OnPath || report.Missing != 1 {
		t.Fatalf("status = %+v, want a genuinely missing requirement reported", report.Sources[0])
	}
}

func TestStatusReadsIndexSources(t *testing.T) {
	const config = `name: brain
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
sources:
  snapshot:
    enabled: true
    layer: index
    run: [cli, list]
    fields:
      id: .id
      title: .name
`
	base := newBase(t, config, nil)
	document := completeTestDocument(base, &sources.Document{
		FKF: sources.SchemaVersion, Source: "snapshot", Layer: core.LayerIndex,
		CollectedAt: "2026-05-04T09:00:00Z",
		Fields:      sources.Fields{core.FieldID: {mustFieldPath(t, ".id")}},
		Count:       2, Records: []sources.Record{{"id": "a"}, {"id": "b"}},
	})
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	entry := status.Sources[0]
	if entry.LastCount != 2 {
		t.Errorf("last_count = %d, want 2 records", entry.LastCount)
	}
	if entry.LastDate == "" {
		t.Error("last_date is empty")
	}
	if entry.Quiet {
		t.Error("an index source can never be quiet")
	}
}

func TestStatusAuditsGitAndTrackedFiles(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	git(t, root, "init", "--quiet")
	git(t, root, "config", "user.email", "test@example.test")
	git(t, root, "config", "user.name", "Test")

	if _, err := services.EnsureManagedBlock(filepath.Join(root, ".gitignore"), services.ManagedIgnoreBlock(false)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "-f", ".env")
	git(t, root, "commit", "-m", "secret", "--quiet")

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "tracked-credentials")
	if finding == nil || finding.Severity != services.SeverityError {
		t.Fatalf("findings = %+v, want an error for tracked .env", status.Findings)
	}
	if status.OK {
		t.Fatal("status with tracked credential must report OK = false")
	}
}

func TestStatusDoesNotRunGitFromBaseBin(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	git(t, root, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "-f", ".env")
	marker := installBaseGitSubstitute(t, root)

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("status executed the untrusted %s substitute: %v", filepath.Join(core.BaseBinDir, "git"), err)
	}
	if finding := findFinding(status, "tracked-credentials"); finding == nil {
		t.Fatalf("findings = %+v, want the host Git result to expose the tracked credential", status.Findings)
	}
}

func TestStatusDisablesRepositoryConfiguredGitHooks(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	git(t, root, "init", "--quiet")
	git(t, root, "config", "user.email", "test@example.test")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "core.fsmonitor", `sh -c 'touch FS_HOOK_RAN; printf "\n"'`)

	if _, err := services.Report(t.Context(), base, services.StatusRequest{}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "FS_HOOK_RAN")); !os.IsNotExist(err) {
		t.Fatalf("status executed the repository-configured fsmonitor hook: %v", err)
	}
}

func TestStatusAuditsPermissionsWithoutMutating(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	loose := filepath.Join(root, "wiki")
	if err := os.MkdirAll(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "permissions")
	if finding == nil {
		t.Fatalf("findings = %+v, want a permission finding", status.Findings)
	}
	if strings.Contains(finding.Fix, "status --repair") || !strings.Contains(finding.Fix, "chmod 700") {
		t.Fatalf("permission fix = %q, want an exact chmod recipe and no mutating status flag", finding.Fix)
	}
	info, err := os.Stat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("status changed mode to %o, want diagnostic-only behavior", got)
	}
}

func TestStatusPermissionRemedyPreservesNestedBinExecutableIntent(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	nested := filepath.Join(base.Root(), core.BaseBinDir, "lib")
	if err := os.MkdirAll(nested, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(nested, "helper")
	plain := filepath.Join(nested, "data")
	for path, mode := range map[string]os.FileMode{executable: 0o755, plain: 0o644} {
		if err := os.WriteFile(path, []byte("fixture\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "permissions")
	if finding == nil || !slices.Contains(finding.Paths, "bin/lib/helper") ||
		!slices.Contains(finding.Paths, "bin/lib/data") {
		t.Fatalf("finding = %+v, want both nested helper modes named", finding)
	}
	command := exec.CommandContext(t.Context(), "bash", "-c", finding.Fix)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("permission remedy failed: %v\n%s", err, output)
	}
	for path, want := range map[string]os.FileMode{executable: 0o700, plain: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode(%s) = %o, want %o", path, got, want)
		}
	}
}

func TestStatusPermissionRemedyWorksThroughASymlinkedBaseRoot(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	realRoot := base.Root()
	alias := filepath.Join(t.TempDir(), "brain")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	configPath := filepath.Join(realRoot, core.ConfigFileName)
	if err := os.Chmod(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}

	throughAlias, err := services.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	if throughAlias.Root() != alias {
		t.Fatalf("opened root = %q, want the chosen alias %q preserved", throughAlias.Root(), alias)
	}
	status, err := services.Report(t.Context(), throughAlias, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "permissions")
	if finding == nil ||
		!slices.Contains(finding.Paths, ".") || !slices.Contains(finding.Paths, core.ConfigFileName) {
		t.Fatalf("findings = %+v, want the real root and config audited through the alias", status.Findings)
	}
	command := exec.CommandContext(t.Context(), "bash", "-c", finding.Fix)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("permission remedy failed: %v\n%s", err, output)
	}
	for path, want := range map[string]os.FileMode{realRoot: core.BaseDirMode, configPath: core.BaseFileMode} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode(%s) = %o, want %o", path, got, want)
		}
	}
	after, err := services.Report(t.Context(), throughAlias, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if finding := findFinding(after, "permissions"); finding != nil {
		t.Fatalf("permission finding remains after applying the remedy: %+v", finding)
	}
}

func TestStatusVersionedPermissionRemedyPreservesExecutableHelpers(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	git(t, root, "init", "--quiet")
	bin := filepath.Join(root, core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(bin, "lib")
	if err := os.MkdirAll(nested, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(nested, "collector")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "permissions")
	if finding == nil {
		t.Fatalf("findings = %+v, want the non-executable helper reported", status.Findings)
	}
	if !strings.Contains(finding.Fix, filepath.Join(root, ".git")) ||
		!strings.Contains(finding.Fix, "-prune") {
		t.Fatalf("permission remedy walks repository internals: %q", finding.Fix)
	}
	if !strings.Contains(finding.Fix, bin) || strings.Contains(finding.Fix, "-maxdepth") ||
		!strings.Contains(finding.Fix, `if [ -x "$file" ]`) {
		t.Fatalf("permission remedy does not preserve executable intent recursively under bin/: %q", finding.Fix)
	}
}

func TestStatusPermissionAuditIgnoresSkillDiscoverySymlink(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	if err := os.Chmod(root, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, core.BaseSkillsDir)
	if err := os.MkdirAll(target, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(root, ".claude")
	if err := os.MkdirAll(claude, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash("../"+core.BaseSkillsDir), filepath.Join(claude, "skills")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if finding := findFinding(status, "permissions"); finding != nil {
		t.Fatalf("permission audit treated the discovery symlink's synthetic mode as repairable: %+v", finding)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != core.BaseDirMode {
		t.Fatalf("skill directory mode = %o, want %o; repair must not chmod through the link",
			info.Mode().Perm(), core.BaseDirMode)
	}
}

func mustFieldPath(t *testing.T, raw string) core.FieldPath {
	t.Helper()
	path, err := core.ParseFieldPath(raw)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStatusAuditsConflictMarkers(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	conflicted := filepath.Join(root, "events", "2026-08-24", "events.json")
	if err := os.MkdirAll(filepath.Dir(conflicted), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "line 1\n<<<<<<< HEAD\nline 2\n=======\nline 3\n>>>>>>> branch\n"
	if err := os.WriteFile(conflicted, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "conflict-markers")
	if finding == nil || finding.Severity != services.SeverityError {
		t.Fatalf("findings = %+v, want conflict-markers error", status.Findings)
	}
}

func TestStatusConflictAuditNeverReadsThroughSymlinks(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	outside := filepath.Join(t.TempDir(), "outside.json")
	content := "<<<<<<< external bytes must stay unread\n"
	if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base.Root(), "events", "2026-08-24", "linked.json")
	if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, linked); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if finding := findFinding(status, "conflict-markers"); finding != nil {
		t.Fatalf("conflict audit followed an unsafe path and observed external bytes: %+v", finding)
	}
	if finding := findFinding(status, "documents"); finding == nil {
		t.Fatalf("findings = %+v, want the document audit to report the unsafe symlink", status.Findings)
	}
}

func TestStatusAuditsSkillDrift(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	skillPath := filepath.Join(root, ".agents", "skills", "fkf-use", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("tampered content"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "skills")
	if finding == nil {
		t.Fatalf("findings = %+v, want skills drift finding", status.Findings)
	}
}

func TestStatusAuditsLearnedBullets(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	taskPath := filepath.Join(root, "tasks", "2026-08-24", "my-task", "TASKS.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o700); err != nil {
		t.Fatal(err)
	}
	taskContent := "# Title\n\n## 1. Req\n\n## Learned\n- [ ] Unharvested bullet that needs promotion\n"
	if err := os.WriteFile(taskPath, []byte(taskContent), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "learned")
	if finding == nil || !strings.Contains(finding.Message, "Unharvested") && !strings.Contains(finding.Message, "bullet") {
		t.Fatalf("findings = %+v, want learned bullet warning", status.Findings)
	}
	wantLearned := "fkf --base '" + root + "' list tasks learned --unharvested"
	if finding.Fix != wantLearned {
		t.Fatalf("learned fix = %q, want the base-qualified current list command %q", finding.Fix, wantLearned)
	}
	joinedNext := strings.Join(status.Next, "\n")
	if !strings.Contains(joinedNext, wantLearned) {
		t.Fatalf("next = %q, want the current learned command", joinedNext)
	}
	if strings.Contains(joinedNext, "fkf tasks learned --unharvested") {
		t.Fatalf("next = %q, still names the removed root tasks command", joinedNext)
	}
}

func TestStatusAuditsDocumentSchema(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	root := base.Root()
	docPath := filepath.Join(root, "events", "2026-08-24", "broken.json")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Invalid JSON document missing schema markers and records
	if err := os.WriteFile(docPath, []byte(`{"invalid": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "documents")
	if finding == nil || finding.Severity != services.SeverityError {
		t.Fatalf("findings = %+v, want document schema error", status.Findings)
	}
}

func TestStatusReportsAMalformedEventBoundaryAsADocumentFinding(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	document := collect(t, base, "2026-05-04", dayOne)
	absolute, err := base.Store.Resolve(document.URI())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(encoded), document.WindowEnd, "not-a-timestamp", 1)
	if corrupt == string(encoded) {
		t.Fatal("fixture did not contain its event window boundary")
	}
	if err := os.WriteFile(absolute, []byte(corrupt), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	status, err := services.Report(t.Context(), base, services.StatusRequest{MaxAgeHours: 24})
	if err != nil {
		t.Fatalf("status aborted on malformed stored evidence: %v", err)
	}
	finding := findFinding(status, "documents")
	if finding == nil || finding.Severity != services.SeverityError ||
		!strings.Contains(finding.Message, "window_end") {
		t.Fatalf("findings = %+v, want the malformed boundary reported by the document audit", status.Findings)
	}
	if !status.Stale {
		t.Fatalf("status = %+v, want invalid evidence excluded from freshness", status)
	}
}

func TestStatusRejectsARecordFiledOutsideItsEventDay(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/misfiled.json", `{
  "fkf": 1, "source": "misfiled", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-05T08:00:00Z", "command": "fixture",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 1, "records": [{"id": "outside", "t": "2026-05-05"}]
}`)

	status, err := services.Report(t.Context(), base, services.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finding := findFinding(status, "documents")
	if finding == nil || finding.Severity != services.SeverityError ||
		!strings.Contains(finding.Message, "outside the requested window") {
		t.Fatalf("findings = %+v, want the misfiled civil-date record reported", status.Findings)
	}
	if status.OK {
		t.Fatal("status with a record outside its event day reported OK")
	}
}

type graphCorruption string

const (
	graphMalformedRow       graphCorruption = "malformed-row"
	graphMissingMetadata    graphCorruption = "missing-metadata"
	graphInvalidMetadata    graphCorruption = "invalid-metadata"
	graphSchemaMismatch     graphCorruption = "schema-mismatch"
	graphColumnsMismatch    graphCorruption = "columns-mismatch"
	graphSeparatorMismatch  graphCorruption = "separator-mismatch"
	graphEdgeCountMismatch  graphCorruption = "edge-count-mismatch"
	graphInvalidGeneratedAt graphCorruption = "invalid-generated-at"
	graphGeneratedMismatch  graphCorruption = "generated-mismatch"
)

func corruptGraphCache(t *testing.T, corruption graphCorruption, graphPath, metaPath string) {
	t.Helper()
	switch corruption {
	case graphMalformedRow:
		data, err := os.ReadFile(graphPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(graphPath, append(data, []byte("malformed\n")...), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
		return
	case graphMissingMetadata:
		if err := os.Remove(metaPath); err != nil {
			t.Fatal(err)
		}
		return
	case graphInvalidMetadata:
		if err := os.WriteFile(metaPath, []byte("{"), core.BaseFileMode); err != nil {
			t.Fatal(err)
		}
		return
	}

	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta services.EdgeListMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	switch corruption {
	case graphSchemaMismatch:
		meta.SchemaVersion++
	case graphColumnsMismatch:
		meta.Columns = []string{"src", "dst"}
	case graphSeparatorMismatch:
		meta.Separator = ","
	case graphEdgeCountMismatch:
		meta.Edges++
	case graphInvalidGeneratedAt:
		meta.GeneratedAt = "not-a-time"
	case graphGeneratedMismatch:
		meta.GeneratedAt = "2026-05-10T13:00:00Z"
	default:
		t.Fatalf("unknown graph corruption %q", corruption)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, encoded, core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
}

func TestStatusReportsInvalidGraphCacheWithoutAborting(t *testing.T) {
	cases := map[string]struct {
		corruption graphCorruption
		want       string
	}{
		"malformed row": {
			corruption: graphMalformedRow,
			want:       "malformed",
		},
		"missing metadata": {
			corruption: graphMissingMetadata,
			want:       core.GraphMetaFile,
		},
		"invalid metadata": {
			corruption: graphInvalidMetadata,
			want:       "decode",
		},
		"schema mismatch": {
			corruption: graphSchemaMismatch,
			want:       "schema_version",
		},
		"columns mismatch": {
			corruption: graphColumnsMismatch,
			want:       "columns",
		},
		"separator mismatch": {
			corruption: graphSeparatorMismatch,
			want:       "separator",
		},
		"edge count mismatch": {
			corruption: graphEdgeCountMismatch,
			want:       "edges",
		},
		"invalid generated time": {
			corruption: graphInvalidGeneratedAt,
			want:       "generated_at",
		},
		"generated time mismatch": {
			corruption: graphGeneratedMismatch,
			want:       "generated_at",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			base := newBase(t, baseConfig, nil)
			collect(t, base, "2026-05-04", dayOne)
			if _, err := services.BuildGraph(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			graphPath, err := base.Store.Resolve(core.GraphFile)
			if err != nil {
				t.Fatal(err)
			}
			metaPath, err := base.Store.Resolve(core.GraphMetaFile)
			if err != nil {
				t.Fatal(err)
			}
			corruptGraphCache(t, test.corruption, graphPath, metaPath)

			status, err := services.Report(t.Context(), base, services.StatusRequest{})
			if err != nil {
				t.Fatalf("Report() aborted on a rebuildable cache problem: %v", err)
			}
			finding := findFinding(status, "derived")
			if finding == nil || finding.Severity != services.SeverityError {
				t.Fatalf("findings = %+v, want a derived-cache error", status.Findings)
			}
			wantFix := "fkf --base '" + base.Root() + "' build graph"
			if finding.Fix != wantFix || !strings.Contains(finding.Message, test.want) {
				t.Fatalf("finding = %+v, want remedy and message containing %q", finding, test.want)
			}
			if status.OK {
				t.Fatal("status with a corrupt graph cache reported OK")
			}
		})
	}
}
