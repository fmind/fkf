package checks_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoticeLegalTextNormalization(t *testing.T) {
	got, err := normalizeNoticeLegalText([]byte(" \t\nFirst line \t\n\nSecond line\n\t\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "First line\n\nSecond line\n"; got != want {
		t.Fatalf("normalized legal text = %q, want %q", got, want)
	}
	if _, err := normalizeNoticeLegalText([]byte("legal text\n```\n")); err == nil {
		t.Fatal("legal text containing a Markdown fence was accepted")
	}
}

type fakeNoticeGoTool struct {
	environment map[string]string
	modules     map[noticeTarget][]noticeModule
	failures    map[noticeTarget]error
	visited     []noticeTarget
}

func (tool *fakeNoticeGoTool) Modules(_ context.Context, target noticeTarget) ([]noticeModule, error) {
	tool.visited = append(tool.visited, target)
	if err := tool.failures[target]; err != nil {
		return nil, err
	}
	return append([]noticeModule(nil), tool.modules[target]...), nil
}

func (tool *fakeNoticeGoTool) Env(_ context.Context, key string) (string, error) {
	if value := tool.environment[key]; value != "" {
		return value, nil
	}
	return "", errors.New("missing fake go env " + key)
}

func TestNoticeGeneratorStopsWhenAnyTargetCannotBeInspected(t *testing.T) {
	failure := noticeTarget{GOOS: "linux", GOARCH: "arm64"}
	tool := &fakeNoticeGoTool{failures: map[noticeTarget]error{failure: errors.New("inspection failed")}}
	_, err := (noticeGenerator{root: repositoryRoot(t), tool: tool}).generate(t.Context())
	if err == nil || !strings.Contains(err.Error(), failure.String()) {
		t.Fatalf("Generate() error = %v, want the failed target named", err)
	}
	want := noticeReleaseTargets[:2]
	if len(tool.visited) != len(want) {
		t.Fatalf("visited targets = %v, want immediate stop after %v", tool.visited, want)
	}
	for index := range want {
		if tool.visited[index] != want[index] {
			t.Fatalf("visited targets = %v, want %v", tool.visited, want)
		}
	}
}

func TestNoticeGeneratorRequiresTheGoRuntimeLegalFiles(t *testing.T) {
	missingGoRoot := filepath.Join(t.TempDir(), "missing-goroot")
	tool := &fakeNoticeGoTool{environment: map[string]string{
		"GOVERSION": "go1.test",
		"GOROOT":    missingGoRoot,
	}}
	_, err := (noticeGenerator{root: repositoryRoot(t), tool: tool}).generate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "required legal file is missing") ||
		!strings.Contains(err.Error(), filepath.Join(missingGoRoot, "LICENSE")) {
		t.Fatalf("Generate() error = %v, want the missing runtime LICENSE named", err)
	}
}

func TestNoticeGeneratorUnionsAndSortsModulesAcrossTargets(t *testing.T) {
	root := noticeGeneratorRoot(t)
	goRoot := filepath.Join(root, "goroot")
	writeNoticeFixture(t, filepath.Join(goRoot, "LICENSE"), "Runtime license\n")
	writeNoticeFixture(t, filepath.Join(goRoot, "PATENTS"), "Runtime patents\n")
	aDir := filepath.Join(root, "modules", "a")
	zDir := filepath.Join(root, "modules", "z")
	writeNoticeFixture(t, filepath.Join(aDir, "LICENSE"), "A license\n")
	writeNoticeFixture(t, filepath.Join(zDir, "LICENSE"), "Z license\n")
	aModule := noticeModule{Path: "example.test/a", Version: "v1.0.0", Dir: aDir}
	zModule := noticeModule{Path: "example.test/z", Version: "v2.0.0", Dir: zDir}
	tool := &fakeNoticeGoTool{
		environment: map[string]string{"GOVERSION": "go1.test", "GOROOT": goRoot},
		modules: map[noticeTarget][]noticeModule{
			noticeReleaseTargets[0]: {zModule},
			noticeReleaseTargets[1]: {aModule, zModule},
		},
	}
	generated, err := (noticeGenerator{root: root, tool: tool}).generate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	text := string(generated)
	aHeading := "## example.test/a v1.0.0"
	zHeading := "## example.test/z v2.0.0"
	if strings.Count(text, aHeading) != 1 || strings.Count(text, zHeading) != 1 ||
		strings.Index(text, aHeading) > strings.Index(text, zHeading) {
		t.Fatalf("generated module headings are not a sorted union:\n%s", text)
	}
}

func TestNoticeGeneratorRejectsAModuleWithoutLegalText(t *testing.T) {
	root := noticeGeneratorRoot(t)
	goRoot := filepath.Join(root, "goroot")
	writeNoticeFixture(t, filepath.Join(goRoot, "LICENSE"), "Runtime license\n")
	writeNoticeFixture(t, filepath.Join(goRoot, "PATENTS"), "Runtime patents\n")
	module := noticeModule{Path: "example.test/missing", Version: "v1.0.0", Dir: filepath.Join(root, "missing")}
	tool := &fakeNoticeGoTool{
		environment: map[string]string{"GOVERSION": "go1.test", "GOROOT": goRoot},
		modules:     map[noticeTarget][]noticeModule{noticeReleaseTargets[0]: {module}},
	}
	_, err := (noticeGenerator{root: root, tool: tool}).generate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no license file for example.test/missing v1.0.0") {
		t.Fatalf("Generate() error = %v, want the module without legal text refused", err)
	}
}

func noticeGeneratorRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, fixture := range []string{"mgo-LICENSE.txt", "upb-LICENSE.txt", "xrash-smetrics-LICENSE.txt"} {
		writeNoticeFixture(t, filepath.Join(root, "services", "testdata", "notices", fixture), fixture+"\n")
	}
	return root
}

func writeNoticeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCopiedSourceLicenseFixturesAreExact(t *testing.T) {
	root := repositoryRoot(t)
	fixtures := map[string]string{
		"services/testdata/notices/mgo-LICENSE.txt":            "86258a47c5340187ef120b9c20e5427a84d3040ecab3243315692862e07dedae",
		"services/testdata/notices/upb-LICENSE.txt":            "82c09e00aabc4239ce29e99ead6bde0d5bd47d31cf266537f33e58f366373679",
		"services/testdata/notices/xrash-smetrics-LICENSE.txt": "1b0edc159f8b0395fc51dc72b95619404e494563b7df7d683d97e35e0a4a8650",
	}
	for relative, want := range fixtures {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("%s sha256 = %x, want exact upstream bytes %s", relative, got, want)
		}
		command := exec.CommandContext(t.Context(), "git", "check-attr", "text", "diff", "--", relative)
		command.Dir = root
		output, err := command.Output()
		if err != nil {
			t.Fatalf("inspect attributes for %s: %v", relative, err)
		}
		if strings.Count(string(output), ": unset") != 2 {
			t.Errorf("%s must be marked -text -diff, got %q", relative, output)
		}
	}
}
