package checks_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
)

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
