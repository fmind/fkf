package fkf_test

import (
	"os"
	"strings"
	"testing"
)

func TestMiseBuildStampsTheGitDescription(t *testing.T) {
	configuration, err := os.ReadFile("mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	const stamp = `-ldflags "-X github.com/fmind/fkf/core.version=$(git describe --tags --always)"`
	if !strings.Contains(string(configuration), stamp) {
		t.Fatalf("mise build must stamp core.version with git describe: missing %s", stamp)
	}
}
