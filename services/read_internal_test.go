package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestApplyJQCountsTheCompleteMultiValueEncoding(t *testing.T) {
	// JSON encoding adds two quotes to the string. The second scalar and the array brackets
	// make the complete result exactly one byte larger than the advertised ceiling.
	payload := strings.Repeat("a", jqMaxOutputBytes-5)
	if _, err := applyJQ(context.Background(), ".,0", payload); !errors.Is(err, core.ErrFileTooLarge) {
		t.Fatalf("applyJQ() error = %v, want the complete array encoding refused", err)
	}

	payload = strings.Repeat("a", jqMaxOutputBytes-6)
	selection, err := applyJQ(context.Background(), ".,0", payload)
	if err != nil {
		t.Fatalf("applyJQ() rejected an exact-bound result: %v", err)
	}
	if len(selection) != jqMaxOutputBytes {
		t.Fatalf("selection size = %d, want exact %d-byte boundary", len(selection), jqMaxOutputBytes)
	}
}

func TestApplyJQDistinguishesHaltFromHaltError(t *testing.T) {
	selection, err := applyJQ(context.Background(), "halt", nil)
	if err != nil || string(selection) != "null" {
		t.Fatalf("halt selection = %s, error = %v; want successful empty stream", selection, err)
	}
	for _, expression := range []string{"null | halt_error", ". | halt_error(0)"} {
		if _, err := applyJQ(context.Background(), expression, nil); !errors.Is(err, ErrJQExpression) {
			t.Errorf("applyJQ(%q) error = %v; want halt_error refused at the expression boundary", expression, err)
		}
	}
	selection, err = applyJQ(context.Background(), `"halt_error"`, nil)
	if err != nil || string(selection) != `"halt_error"` {
		t.Fatalf("string selection = %s, error = %v; source text must not trigger the AST guard", selection, err)
	}
}
