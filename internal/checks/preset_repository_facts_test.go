package checks_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type repositoryFact struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	RootRef       string           `json:"root_ref"`
	Remotes       []string         `json:"remotes"`
	Languages     []string         `json:"languages"`
	DeclaredTasks []map[string]any `json:"declared_tasks"`
	Instructions  []string         `json:"instructions"`
}

func runRepositoryFacts(t *testing.T, roots ...string) ([]byte, string, error) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{filepath.Join(repositoryRoot(t), "presets", "bin", "repository-facts.py")}, roots...)
	command := exec.CommandContext(t.Context(), python, args...)
	command.Env = []string{"HOME=" + t.TempDir(), "PATH=/nonexistent"}
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	return []byte(stdout.String()), stderr.String(), err
}

func writeRepositoryFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryFactsProjectsDeclarationsWithoutExecutingRepository(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "project space")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepositoryFile(t, repository, ".git/config", `[remote "origin"]
	url = https://token:secret@github.com/acme/project.git?access_token=hidden#fragment
`)
	writeRepositoryFile(t, repository, "go.mod", "module example.test/project\n\ngo 1.27\n")
	writeRepositoryFile(t, repository, "mise.toml", `[tasks.build]
run = "go build ./..."
[tasks.test]
run = "${DANGEROUS_TEST}"
`)
	writeRepositoryFile(t, repository, "package.json", `{"scripts":{"test":"touch sentinel"}}`)
	writeRepositoryFile(t, repository, "AGENTS.md", "# Synthetic instructions\n")
	writeRepositoryFile(t, repository, "hostile$(touch sentinel)", "data")

	output, stderr, err := runRepositoryFacts(t, root)
	if err != nil {
		t.Fatalf("repository-facts.py error = %v\n%s", err, stderr)
	}
	var records []repositoryFact
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatalf("decode repository facts: %v\n%s", err, output)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	record := records[0]
	if record.ID != "repo:github.com/acme/project" || record.Title != "github.com/acme/project" {
		t.Fatalf("safe identity = %#v", record)
	}
	if record.RootRef != "root-1/project%20space" {
		t.Fatalf("root reference = %q", record.RootRef)
	}
	if len(record.Remotes) != 1 || record.Remotes[0] != "https://github.com/acme/project" {
		t.Fatalf("sanitized remotes = %q", record.Remotes)
	}
	if strings.Contains(string(output), "token") || strings.Contains(string(output), "secret") ||
		strings.Contains(string(output), "access_token") {
		t.Fatalf("credential-bearing remote survived projection: %s", output)
	}
	if len(record.Instructions) != 1 || record.Instructions[0] != "AGENTS.md" {
		t.Fatalf("instruction paths = %q", record.Instructions)
	}
	if len(record.Languages) != 2 || len(record.DeclaredTasks) != 3 {
		t.Fatalf("metadata projection = %#v", record)
	}
	if _, err := os.Stat(filepath.Join(repository, "sentinel")); !os.IsNotExist(err) {
		t.Fatalf("repository task or hostile filename executed: %v", err)
	}
}

func TestRepositoryFactsUsesOriginForForkIdentityAndConfinedInstructionLinks(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "fork")
	writeRepositoryFile(t, repository, ".git/config", `[remote "upstream"]
	url = https://github.com/aaa/upstream.git
[remote "origin"]
	url = https://github.com/owner/fork.git
`)
	writeRepositoryFile(t, repository, "AGENTS.md", "# Instructions\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(repository, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	output, stderr, err := runRepositoryFacts(t, root)
	if err != nil {
		t.Fatalf("ordinary fork metadata failed: %v\n%s", err, stderr)
	}
	var records []repositoryFact
	if err := json.Unmarshal(output, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "repo:github.com/owner/fork" ||
		len(records[0].Instructions) != 2 || len(records[0].Remotes) != 2 {
		t.Fatalf("fork facts = %+v", records)
	}
}

func TestRepositoryFactsHandlesHostileNamesAndRejectsUnsafeMetadata(t *testing.T) {
	t.Run("hostile name", func(t *testing.T) {
		root := t.TempDir()
		repository := filepath.Join(root, "odd\nname")
		if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		output, stderr, err := runRepositoryFacts(t, root)
		if err != nil {
			t.Fatalf("hostile filename changed execution: %v\n%s", err, stderr)
		}
		if strings.Contains(string(output), "odd\nname") || !strings.Contains(string(output), "root-1/odd%0Aname") {
			t.Fatalf("unsafe root reference: %s", output)
		}
	})

	for name, prepare := range map[string]func(*testing.T, string){
		"malformed TOML": func(t *testing.T, repository string) {
			writeRepositoryFile(t, repository, "pyproject.toml", "[")
		},
		"large config": func(t *testing.T, repository string) {
			writeRepositoryFile(t, repository, "package.json", strings.Repeat("x", (256<<10)+1))
		},
		"symlink instruction": func(t *testing.T, repository string) {
			target := filepath.Join(t.TempDir(), "outside")
			writeRepositoryFile(t, filepath.Dir(target), filepath.Base(target), "outside")
			if err := os.Symlink(target, filepath.Join(repository, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			repository := filepath.Join(root, "repo")
			if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			prepare(t, repository)
			output, _, err := runRepositoryFacts(t, root)
			if err == nil {
				t.Fatal("unsafe repository metadata was accepted")
			}
			if len(output) != 0 {
				t.Fatalf("helper emitted a partial snapshot: %s", output)
			}
		})
	}
}
