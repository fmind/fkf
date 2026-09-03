package mcpserver

import (
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestFindCursorEncoderUsesThePublishedInputBound(t *testing.T) {
	cursor, err := openFindCursor("", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cursor.next(strings.Repeat("a", 64), services.FindPosition{
		Phase: services.FindPhaseRecord, URI: strings.Repeat("x", maxInputTextLength),
	}); err == nil || !strings.Contains(err.Error(), "over the 4096-byte") {
		t.Fatalf("oversized next cursor error = %v, want the schema's exact bound", err)
	}
}

func TestSafeClientTextDoesNotTreatMissingStateDirectoryAsDot(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	root := t.TempDir()
	base := &services.Base{Store: core.NewStore(root, map[core.Layer]bool{})}
	const message = "field .kind is invalid"
	if got := safeClientText(base, message); got != message {
		t.Fatalf("safeClientText() = %q, want %q; an unavailable state root must not redact dots", got, message)
	}
}
