package services_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/fmind/fkf/services"
)

// These are the invariants stated in AGENTS.md, checked mechanically rather than by review.
// Each one is a property that would be expensive to notice going wrong any other way.

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReleaseNotesSelectExactlyOneTaggedChangelogSection(t *testing.T) {
	changelog := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(changelog, []byte(`# Changelog

## [v2.0.0](https://example.test/v2) - 2026-08-26

Second release only.

## [v1.0.0](https://example.test/v1) - 2026-08-25

First release only.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), filepath.Join(repositoryRoot(t), "scripts", "release-notes"),
		"v2.0.0", changelog)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("release-notes failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "Second release only.") || strings.Contains(text, "First release only.") ||
		strings.Contains(text, "# Changelog") {
		t.Fatalf("v2 release notes = %q, want only the v2 section body", text)
	}
	missing := exec.CommandContext(t.Context(), filepath.Join(repositoryRoot(t), "scripts", "release-notes"),
		"v3.0.0", changelog)
	if output, err := missing.CombinedOutput(); err == nil || !strings.Contains(string(output), "no section") {
		t.Fatalf("missing release section = %v, %q; want a clear refusal", err, output)
	}
}

// TestNoPackageImportsNetHTTP is the strongest form of "no network at read time": there is no
// HTTP client anywhere in the module, so no code path can grow one by accident.
func TestNoPackageImportsNetHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "list", "-deps", "-json", "./...")
	command.Dir = repositoryRoot(t)
	output, err := command.Output()
	if err != nil {
		// Not a skip. This is the whole enforcement of "no network at read time"; letting it
		// vanish when `go list` fails means the invariant silently stops being checked, and the
		// suite already runs under a Go toolchain by construction.
		t.Fatalf("go list failed, so the net/http invariant went unchecked: %v", err)
	}
	type pkg struct {
		ImportPath string
		Module     *struct{ Path string }
		Imports    []string
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for decoder.More() {
		var entry pkg
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		// Only this module's own packages: a dependency's HTTP client is not reachable unless
		// fkf calls into it, and fkf's own import graph is what this test owns.
		if entry.Module == nil || entry.Module.Path != "github.com/fmind/fkf" {
			continue
		}
		for _, imported := range entry.Imports {
			if imported == "net/http" || strings.HasPrefix(imported, "net/http/") {
				t.Fatalf("%s imports %s; fkf makes no HTTP request, ever", entry.ImportPath, imported)
			}
		}
	}
}

// TestNoSourceReadsACredential keeps the "fkf reads no secret" invariant honest. The only
// mentions allowed are the scaffold's credential-pattern list, which exists to keep such files
// OUT of a base, and the documentation that says so.
func TestNoSourceReadsACredential(t *testing.T) {
	pattern := regexp.MustCompile(`\b(TOKEN|SECRET|PASSWORD|API_KEY|PAT)\b`)
	allowed := map[string]bool{
		"services/scaffold.go": true, // the credential-pattern ignore list
		"core/config.go":       true, // the removed-key map, which says fkf reads no secret
	}
	root := repositoryRoot(t)
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return walkErr
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if allowed[filepath.ToSlash(relative)] {
			return nil
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		for number, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if match := pattern.FindString(line); match != "" {
				t.Errorf("%s:%d mentions %s outside a comment; fkf reads no secret", relative, number+1, match)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOneModuleOneBinary checks the layout invariant: one Go module at the root and exactly one
// shipped command. docs/ pins the Hugo theme and holds no Go.
func TestOneModuleOneBinary(t *testing.T) {
	root := repositoryRoot(t)
	var modules []string
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if entry.Name() == "go.mod" {
			relative, _ := filepath.Rel(root, current)
			modules = append(modules, filepath.ToSlash(relative))
		}
		if entry.Name() == "go.work" {
			t.Fatalf("%s exists; the layout is one module at the root", current)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range modules {
		if module != "go.mod" && module != "docs/go.mod" {
			t.Fatalf("unexpected module %s; only the root and the docs theme pin may declare one", module)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "fkf" {
		t.Fatalf("cmd/ holds %v, want exactly cmd/fkf", entries)
	}
}

func TestDocumentationDistributionContracts(t *testing.T) {
	root := repositoryRoot(t)
	for relative, want := range map[string]string{
		"docs/assets/vendor/flexsearch-0.8.143/flexsearch.bundle.min.js": "433e941a8a573ebb9931fc16fc75266ab6b93f569ac2fb4f3dc66882e0416f4c",
		"docs/static/third-party/flexsearch-0.8.143-LICENSE.txt":         "1eb85fc97224598dad1852b5d6483bbcf0aa8608790dcc657a5a2a761ae9c8c6",
		"docs/static/third-party/hextra-0.12.3-LICENSE.txt":              "749fbcaf565aeec8feab572b554ad7f4798397521b737a35ee1dbdbfe6b9c1db",
		"docs/static/third-party/tailwindcss-4.1.18-LICENSE.txt":         "60e0b68c0f35c078eef3a5d29419d0b03ff84ec1df9c3f9d6e39a519a5ae7985",
		"docs/static/fmind-logo.webp":                                    "f77cd0c6fddec340e0902999b93178a768767d4847e742f99ea40860dec96abe",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Errorf("read pinned documentation asset %s: %v", relative, err)
			continue
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("%s sha256 = %x, want %s", relative, got, want)
		}
	}
	for _, relative := range []string{
		"docs/assets/vendor/flexsearch-0.8.143/flexsearch.bundle.min.js",
		"docs/static/third-party/flexsearch-0.8.143-LICENSE.txt",
	} {
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

	site := strings.TrimSpace(os.Getenv("FKF_DOCS_BUILD_DIR"))
	if site == "" {
		return
	}
	for _, relative := range []string{
		"fmind-logo.webp", "site.webmanifest", "third-party/flexsearch-0.8.143-LICENSE.txt",
		"third-party/hextra-0.12.3-LICENSE.txt", "third-party/tailwindcss-4.1.18-LICENSE.txt",
	} {
		if _, err := os.Stat(filepath.Join(site, filepath.FromSlash(relative))); err != nil {
			t.Errorf("built site omits %s: %v", relative, err)
		}
	}
	assertBuiltSiteIdentity(t, site)
}

func assertBuiltSiteIdentity(t *testing.T, site string) {
	t.Helper()
	var html strings.Builder
	if err := filepath.WalkDir(site, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".html" {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			html.Write(data)
		}
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"cdn.jsdelivr.net/npm/flexsearch", "© 2026 Hextra Project"} {
		if strings.Contains(html.String(), forbidden) {
			t.Errorf("built site contains forbidden inherited content %q", forbidden)
		}
	}
	for _, required := range []string{"© 2026 Médéric Hurier (Fmind).", "Powered by Hextra"} {
		if !strings.Contains(html.String(), required) {
			t.Errorf("built site omits %q", required)
		}
	}
	for _, inherited := range []string{
		"favicon.ico", "favicon.svg", "favicon-16x16.png", "favicon-32x32.png",
		"android-chrome-192x192.png", "android-chrome-512x512.png", "apple-touch-icon.png",
		"images/logo.svg", "images/logo-dark.svg",
	} {
		if _, err := os.Stat(filepath.Join(site, filepath.FromSlash(inherited))); err == nil {
			t.Errorf("built site publishes inherited Hextra identity file %s", inherited)
		} else if !os.IsNotExist(err) {
			t.Errorf("inspect inherited site file %s: %v", inherited, err)
		}
	}
}

// TestNoLegacyOrMigrationPaths is the "no migration, ever" invariant, checked over IDENTIFIERS
// rather than over prose. A comment explaining that there is no migration is exactly what
// should be written; a function named migrateSomething is the thing that must not exist.
func TestNoLegacyOrMigrationPaths(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)(legacy|migrat|deprecat|backwardcompat|compatib)`)
	root := repositoryRoot(t)
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return walkErr
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if filepath.Base(current) == "invariants_test.go" {
			return nil // this file names the thing it forbids, which is the point of it
		}
		parsed, err := parser.ParseFile(fileSet, current, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok || !pattern.MatchString(identifier.Name) {
				return true
			}
			t.Errorf("%s:%d declares or uses %q; there is one generation and no migration",
				relative, fileSet.Position(identifier.Pos()).Line, identifier.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSkillsAndPresetsAreEmbedded confirms `fkf init` can scaffold a base on a machine that has
// only the binary — which is what makes the binary the provenance, with no lock file to drift.
func TestSkillsAndPresetsAreEmbedded(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	// Run from a directory that holds no skills/ or presets/ tree, so only the embed can serve.
	t.Chdir(t.TempDir())
	if _, err := services.Init(t.Context(), services.InitRequest{
		Path: root, Preset: services.PresetPersonal, SkipGit: true,
	}, clock); err != nil {
		t.Fatalf("init away from the repository: %v", err)
	}
	for _, name := range services.BundledSkills {
		if _, err := os.Stat(filepath.Join(root, ".agents", "skills", name, "SKILL.md")); err != nil {
			t.Fatalf("skill %s was not written from the embed: %v", name, err)
		}
	}
}

func TestBundledSkillsHaveValidFrontmatter(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range services.BundledSkills {
		data, err := os.ReadFile(filepath.Join(root, "skills", name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.TrimPrefix(string(data), "\ufeff")
		if !strings.HasPrefix(text, "---\n") {
			t.Errorf("skill %s has no opening YAML frontmatter delimiter", name)
			continue
		}
		frontmatter, _, found := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
		if !found {
			t.Errorf("skill %s has no closing YAML frontmatter delimiter", name)
			continue
		}
		var manifest map[string]any
		if err := yaml.Unmarshal([]byte(frontmatter), &manifest); err != nil {
			t.Errorf("skill %s frontmatter is invalid YAML: %v", name, err)
			continue
		}
		for _, key := range []string{"name", "description"} {
			value, ok := manifest[key].(string)
			if !ok || strings.TrimSpace(value) == "" {
				t.Errorf("skill %s frontmatter %s must be a non-empty string", name, key)
			}
		}
		if manifest["name"] != name {
			t.Errorf("skill directory %s disagrees with frontmatter name %q", name, manifest["name"])
		}
		if license, present := manifest["license"]; present {
			if value, ok := license.(string); !ok || strings.TrimSpace(value) == "" {
				t.Errorf("skill %s frontmatter license must be a non-empty string when present", name)
			}
		}
	}
}
