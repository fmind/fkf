package core

import (
	"runtime/debug"
	"strings"
)

// version is overridable at link time (-ldflags "-X github.com/fmind/fkf/core.version=v1.2.3")
// so a release archive reports its tag. It is deliberately empty by default: an empty value
// means "ask the build info", which is what makes `go install ...@latest` self-describing.
var version = ""

// Version is the fkf build version, surfaced via `fkf --version`, the MCP implementation
// record, and every stored document's provenance, so a result can always be traced to the
// build that produced it.
//
// It is resolved once from three sources, most authoritative first: a linker-injected tag,
// the module version Go records for `go install`, and finally a VCS-stamped development
// string. Resolving it rather than hardcoding a constant is what stops `fkf --version` from
// claiming 0.1.0 forever.
var Version = resolveVersion(version, debug.ReadBuildInfo)

// devVersion is what a build with no tag and no VCS stamp reports. It must stay non-empty:
// provenance validation requires generator_version to be a non-empty string.
const devVersion = "0.0.0-dev"

func resolveVersion(injected string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if trimmed := strings.TrimSpace(injected); trimmed != "" {
		return trimmed
	}
	info, ok := readBuildInfo()
	if !ok {
		return devVersion
	}
	// `go install module@version` records the resolved version here. A build from a working
	// tree records "(devel)", which says nothing useful, so fall through to the VCS stamp.
	if module := strings.TrimSpace(info.Main.Version); module != "" && module != "(devel)" {
		return module
	}
	return developmentVersion(info)
}

// developmentVersion reconstructs a useful identifier from the VCS settings the toolchain
// stamps into a `go build` of a working tree: short revision plus a dirty marker, which is
// exactly what distinguishes one local build from the next.
func developmentVersion(info *debug.BuildInfo) string {
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return devVersion
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	build := devVersion + "+" + revision
	if modified {
		build += ".dirty"
	}
	return build
}
