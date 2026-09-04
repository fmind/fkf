package fkf_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMiseBuildStampsTheGitDescription(t *testing.T) {
	configuration, err := os.ReadFile("mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	// --dirty is what stops a build from an edited tree claiming to be the published tag.
	const stamp = `-ldflags "-X github.com/fmind/fkf/core.version=$(git describe --tags --always --dirty)"`
	if !strings.Contains(string(configuration), stamp) {
		t.Fatalf("mise build must stamp core.version with git describe: missing %s", stamp)
	}
	_, body, found := strings.Cut(string(configuration), "\n[tasks.build]\n")
	if !found {
		t.Fatal("mise.toml declares no [tasks.build]")
	}
	task, _, _ := strings.Cut(body, "\n[")
	// The stamp's real input is the git tag, which no file glob expresses, so an incremental
	// skip would hand `install:binary` a binary carrying the previous release's version.
	for _, key := range []string{"sources", "outputs"} {
		if strings.Contains(task, "\n"+key+" =") {
			t.Errorf("[tasks.build] declares %s, so a new tag would not invalidate the build", key)
		}
	}
}

func TestREADMESourceExampleDeclaresEveryBodyPlaceholder(t *testing.T) {
	content, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, found := strings.Cut(string(content), "```yaml\nsources:\n")
	if !found {
		t.Fatal("README.md has no source YAML example")
	}
	snippet, _, found := strings.Cut("sources:\n"+after, "\n```")
	if !found {
		t.Fatal("README.md source YAML example has no closing fence")
	}
	var example struct {
		Sources map[string]struct {
			Fields map[string]any `yaml:"fields"`
			Body   []string       `yaml:"body"`
		} `yaml:"sources"`
	}
	if err := yaml.Unmarshal([]byte(snippet), &example); err != nil {
		t.Fatalf("parse README.md source YAML example: %v", err)
	}
	placeholder := regexp.MustCompile(`^\{\{([a-z][a-z0-9_]*)\}\}$`)
	for name, source := range example.Sources {
		for _, argument := range source.Body {
			match := placeholder.FindStringSubmatch(argument)
			if len(match) == 0 {
				continue
			}
			if _, declared := source.Fields[match[1]]; !declared {
				t.Errorf("README.md source %s body uses {{%s}} without declaring fields.%s",
					name, match[1], match[1])
			}
		}
	}
}

func TestOutdatedTaskChecksBeyondExactPins(t *testing.T) {
	configuration, err := os.ReadFile("mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configuration), "mise outdated --local --bump") {
		t.Fatal("check:outdated compares exact pins to themselves; pass --bump to resolve available stable upgrades")
	}
}

func TestInstallBinaryTaskUsesPortableInstallFlags(t *testing.T) {
	configuration, err := os.ReadFile("mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(configuration), "\n[tasks.\"install:binary\"]\n")
	if !found {
		t.Fatal("mise.toml declares no install:binary task")
	}
	task, _, _ := strings.Cut(body, "\n[")
	if strings.Contains(task, "install -D") {
		t.Fatal("install:binary uses GNU install -D, which is not the same option on macOS")
	}
	for _, portable := range []string{`mkdir -p "$HOME/.local/bin"`, `install -m 0755 bin/fkf "$HOME/.local/bin/fkf"`} {
		if !strings.Contains(task, portable) {
			t.Errorf("install:binary does not contain portable, quoted command %q", portable)
		}
	}
}
