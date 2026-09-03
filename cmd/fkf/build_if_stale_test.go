package main

import (
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestCLIBuildIfStaleSkipsTheWriterLockWhenCachesAreCurrent(t *testing.T) {
	root := demoBase(t)
	if got := invoke(t, "--base", root, "build"); got.code != ExitSuccess {
		t.Fatalf("initial build exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	lock, err := core.AcquireWriterLock(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	got := invoke(t, "--format", "json", "--base", root, "build", "--if-stale")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, `"nothing_stale": true`) {
		t.Fatalf("build --if-stale under held lock = exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

func TestCLIBuildIfStaleRejectsCheck(t *testing.T) {
	got := invoke(t, "build", "wiki", "--check", "--if-stale")
	if got.code != ExitInvalidUsage || !strings.Contains(got.stderr, "--check cannot be combined") {
		t.Fatalf("build --check --if-stale = exit %d, stderr %q", got.code, got.stderr)
	}
}

func TestCLIBuildBodiesRequiresExplicitPrune(t *testing.T) {
	root := demoBase(t)
	without := invoke(t, "--base", root, "build", "bodies")
	if without.code != ExitInvalidUsage || !strings.Contains(without.stderr, "requires --prune") {
		t.Fatalf("build bodies = exit %d, stderr %q", without.code, without.stderr)
	}
	with := invoke(t, "--format", "json", "--base", root, "build", "bodies", "--prune")
	if with.code != ExitSuccess || !strings.Contains(with.stdout, `"bodies"`) {
		t.Fatalf("build bodies --prune = exit %d, stdout %q, stderr %q", with.code, with.stdout, with.stderr)
	}
}
