package core

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main:     debug.Module{Version: mainVersion},
			Settings: settings,
		}, true
	}
}

func TestResolveVersionPrefersTheInjectedTag(t *testing.T) {
	// A release archive is built with -ldflags; nothing the build info says may override the
	// tag the maintainer actually published.
	got := resolveVersion("  v1.2.3  ", buildInfo("v9.9.9"))
	if got != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", got)
	}
}

func TestResolveVersionUsesTheModuleVersionFromGoInstall(t *testing.T) {
	got := resolveVersion("", buildInfo("v0.4.0"))
	if got != "v0.4.0" {
		t.Fatalf("version = %q, want v0.4.0", got)
	}
}

func TestResolveVersionFallsThroughDevelToTheVCSStamp(t *testing.T) {
	// "(devel)" is what a working-tree build reports when no pseudo-version is available; it
	// identifies nothing, so the revision must win instead.
	got := resolveVersion("", buildInfo("(devel)",
		debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	))
	if got != devVersion+"+0123456789ab.dirty" {
		t.Fatalf("version = %q, want %s+0123456789ab.dirty", got, devVersion)
	}
}

func TestResolveVersionOmitsTheDirtyMarkerForACleanTree(t *testing.T) {
	got := resolveVersion("", buildInfo("(devel)",
		debug.BuildSetting{Key: "vcs.revision", Value: "abcdef123456"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"},
	))
	if got != devVersion+"+abcdef123456" {
		t.Fatalf("version = %q, want %s+abcdef123456", got, devVersion)
	}
}

func TestResolveVersionIsNeverEmpty(t *testing.T) {
	// Provenance validation requires a non-empty generator_version, so every fallback path
	// must still name something.
	for name, read := range map[string]func() (*debug.BuildInfo, bool){
		"no build info": func() (*debug.BuildInfo, bool) { return nil, false },
		"no vcs stamp":  buildInfo("(devel)"),
		"empty module":  buildInfo(""),
	} {
		if got := resolveVersion("", read); got == "" {
			t.Errorf("%s: version is empty", name)
		}
	}
}

func TestPackageVersionIsPopulated(t *testing.T) {
	if Version == "" {
		t.Fatal("core.Version must never be empty")
	}
}
