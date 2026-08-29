package services_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestHelpersReportAndExplicitRefreshTouchOnlyShippedHelpers(t *testing.T) {
	config := strings.Replace(baseConfig, "    run: [cli", "    requires: [git-log-json.sh]\n    run: [cli", 1)
	base := newBase(t, config, nil)
	bin := filepath.Join(base.Root(), core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(bin, "fkf-hook.sh")
	custom := filepath.Join(bin, "custom-source")
	if err := os.WriteFile(drifted, []byte("#!/bin/sh\necho locally edited\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte("#!/bin/sh\necho custom\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	before, err := services.InspectHelpers(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	if before.Drifted != 1 || before.Missing != 1 || before.Refreshed != 0 {
		t.Fatalf("before = %+v, want one drifted hook and one missing required helper", before)
	}
	encoded, err := json.Marshal(before.Helpers[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"uri"`) || !strings.Contains(string(encoded), `"path":"bin/`) {
		t.Fatalf("helper status = %s, want a non-addressable path rather than a URI", encoded)
	}
	after, err := services.InspectHelpers(t.Context(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if after.Drifted != 0 || after.Missing != 0 || after.Refreshed != 2 {
		t.Fatalf("after = %+v, want both shipped helpers explicitly refreshed", after)
	}
	if got, err := os.ReadFile(custom); err != nil || string(got) != "#!/bin/sh\necho custom\n" {
		t.Fatalf("custom helper changed: %q, %v", got, err)
	}
}

func TestDisabledSourcesDoNotRequireOrMaterializeOfficialHelpers(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}

	// This helper belongs only to disabled examples in the personal preset.
	helper := filepath.Join(root, core.BaseBinDir, "github-search-json.sh")
	if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled provider helper was materialized: %v", err)
	}
	base := openBase(t, root, nil)
	report, err := services.InspectHelpers(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range report.Helpers {
		if status.Name == "github-search-json.sh" {
			t.Fatalf("disabled provider enlarged the helper audit: %+v", status)
		}
	}
}

func TestHelperRefreshMaterializesANewlyEnabledOfficialHelper(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, core.ConfigFileName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(config), "  github-pull-requests:\n    enabled: false", "  github-pull-requests:\n    enabled: true", 1)
	if updated == string(config) {
		t.Fatal("personal preset no longer contains the expected disabled source")
	}
	if err := os.WriteFile(configPath, []byte(updated), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	base := openBase(t, root, nil)
	before, err := services.InspectHelpers(t.Context(), base, false)
	if err != nil {
		t.Fatal(err)
	}
	var missing bool
	for _, status := range before.Helpers {
		if status.Name == "github-search-json.sh" && status.Required && status.State == services.HelperMissing {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("before = %+v, want the newly required helper reported missing", before)
	}
	after, err := services.InspectHelpers(t.Context(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if after.Refreshed == 0 {
		t.Fatalf("after = %+v, want the missing required helper refreshed", after)
	}
	if info, err := os.Stat(filepath.Join(root, core.BaseBinDir, "github-search-json.sh")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("refreshed helper = %v, %v", info, err)
	}
}

func TestDisablingASourceRemovesItsOfficialHelperFromTheHelperAudit(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, core.ConfigFileName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(config), "    enabled: true", "    enabled: false", 1)
	if updated == string(config) {
		t.Fatal("personal preset no longer contains the expected enabled source")
	}
	if err := os.WriteFile(configPath, []byte(updated), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, core.BaseBinDir, "git-log-json.sh")); err != nil {
		t.Fatalf("disabling a source must not delete an existing helper: %v", err)
	}
	report, err := services.InspectHelpers(t.Context(), openBase(t, root, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range report.Helpers {
		if status.Name == "git-log-json.sh" {
			t.Fatalf("disabled source retained an unnecessary helper audit entry: %+v", status)
		}
	}
}

func TestHelpersRefuseASymlinkBeforeRefresh(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	bin := filepath.Join(base.Root(), core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(bin, "fkf-hook.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := services.InspectHelpers(t.Context(), base, true); err == nil {
		t.Fatal("InspectHelpers(refresh) followed or replaced a symlink")
	}
}

func TestHelpersRefuseASymlinkedBinDirectoryBeforeInspection(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "fkf-hook.sh"), []byte("outside\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base.Root(), core.BaseBinDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := services.InspectHelpers(t.Context(), base, false); err == nil {
		t.Fatal("InspectHelpers followed a symlinked bin directory outside the base")
	}
}

func TestHelpersRefreshThroughASymlinkSpelledBaseRoot(t *testing.T) {
	realBase := newBase(t, baseConfig, nil)
	bin := filepath.Join(realBase.Root(), core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(bin, "fkf-hook.sh")
	if err := os.WriteFile(drifted, []byte("#!/bin/sh\necho locally edited\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "brain")
	if err := os.Symlink(realBase.Root(), alias); err != nil {
		t.Fatal(err)
	}
	base, err := services.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	if base.Root() != alias {
		t.Fatalf("opened root = %q, want the chosen symlink spelling %q", base.Root(), alias)
	}

	report, err := services.InspectHelpers(t.Context(), base, true)
	if err != nil {
		t.Fatalf("InspectHelpers(refresh) through a symlink-spelled base: %v", err)
	}
	if report.Refreshed != 1 || report.Drifted != 0 {
		t.Fatalf("report = %+v, want the drifted helper refreshed through the alias", report)
	}
}

func TestHelpersPreflightEveryOfficialTargetBeforeRefreshingAny(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	bin := filepath.Join(base.Root(), core.BaseBinDir)
	if err := os.MkdirAll(bin, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(bin, "agent-memory-files.sh")
	want := []byte("#!/bin/sh\necho locally edited\n")
	if err := os.WriteFile(drifted, want, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(bin, "fkf-hook.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := services.InspectHelpers(t.Context(), base, true); err == nil {
		t.Fatal("InspectHelpers(refresh) accepted a later official symlink")
	}
	got, err := os.ReadFile(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("refresh changed %s before preflight finished: %q", drifted, got)
	}
}
