package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
)

func TestEvalCommandRendersRecallAndReturnsSuccess(t *testing.T) {
	root := demoBase(t)
	directory := filepath.Join(root, "evals")
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		t.Fatal(err)
	}
	content := `fkf: 1
k: 100
recall_threshold: 1
queries:
  - name: declarative-source
    question: Declarative source runner
    window: {}
    expected_uris: [wiki/declarative-sources.md]
    forbidden_uris: []
`
	if err := os.WriteFile(filepath.Join(directory, "queries.yaml"), []byte(content), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	got := invoke(t, "--format", "text", "--base", root, "eval")
	if got.code != ExitSuccess || !strings.Contains(got.stdout, "PASS declarative-source") ||
		!strings.Contains(got.stdout, "1 passed, 0 failed") {
		t.Fatalf("eval = code %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
}
