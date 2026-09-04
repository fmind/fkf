package checks_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
)

// TestMain names the host tools the preset helpers in this package execute. Without it a
// missing jq or yq surfaces from inside a helper as a claim about the data — "start is not an
// RFC3339 timestamp", "not XML" — because the helper's jq or yq program is what parses those,
// and the tool's exit 127 is swallowed on the way out. jq and yq are pinned in mise.toml;
// sqlite3 is the one host tool the toolchain cannot pin with verifiable provenance.
func TestMain(m *testing.M) {
	for _, tool := range []string{"jq", "sqlite3", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintf(os.Stderr, "internal/checks runs the preset helpers and needs %s: %v\n", tool, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

var testClock = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return testClock }

func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(core.BaseEnvVar, "")
}
