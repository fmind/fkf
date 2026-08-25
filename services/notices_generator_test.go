package services_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateThirdPartyNotices = flag.Bool(
	"update-third-party-notices",
	false,
	"rewrite THIRD_PARTY_NOTICES.md from the linked release targets",
)

type noticeTarget struct {
	GOOS   string
	GOARCH string
}

func (target noticeTarget) String() string { return target.GOOS + "/" + target.GOARCH }

var noticeReleaseTargets = []noticeTarget{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
}

type noticeModule struct {
	Path    string
	Version string
	Dir     string
}

func (module noticeModule) key() string {
	return module.Path + "|" + module.Version + "|" + module.Dir
}

type noticeGoTool interface {
	Modules(context.Context, noticeTarget) ([]noticeModule, error)
	Env(context.Context, string) (string, error)
}

type commandNoticeGoTool struct {
	root string
}

func (tool commandNoticeGoTool) Modules(ctx context.Context, target noticeTarget) ([]noticeModule, error) {
	command := exec.CommandContext(ctx, "go", "list", "-deps", "-json", "./cmd/fkf")
	command.Dir = tool.root
	command.Env = append(os.Environ(),
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
		"CGO_ENABLED=0",
	)
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("go list for %s: %w: %s", target, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("go list for %s: %w", target, err)
	}

	var modules []noticeModule
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var listed struct {
			Module *struct {
				Path    string
				Version string
				Dir     string
				Main    bool
			}
		}
		if err := decoder.Decode(&listed); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output for %s: %w", target, err)
		}
		if listed.Module == nil || listed.Module.Main {
			continue
		}
		modules = append(modules, noticeModule{
			Path: listed.Module.Path, Version: listed.Module.Version, Dir: listed.Module.Dir,
		})
	}
	return modules, nil
}

func (tool commandNoticeGoTool) Env(ctx context.Context, key string) (string, error) {
	command := exec.CommandContext(ctx, "go", "env", key)
	command.Dir = tool.root
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", key, err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("go env %s returned an empty value", key)
	}
	return value, nil
}

type noticeGenerator struct {
	root string
	tool noticeGoTool
}

func (generator noticeGenerator) generate(ctx context.Context) ([]byte, error) {
	modules, err := generator.linkedModules(ctx)
	if err != nil {
		return nil, err
	}
	goVersion, err := generator.tool.Env(ctx, "GOVERSION")
	if err != nil {
		return nil, err
	}
	goRoot, err := generator.tool.Env(ctx, "GOROOT")
	if err != nil {
		return nil, err
	}

	var output strings.Builder
	appendNoticeHeader(&output, goVersion)
	if err := appendNoticeLegalFile(&output, "LICENSE", filepath.Join(goRoot, "LICENSE")); err != nil {
		return nil, err
	}
	if err := appendNoticeLegalFile(&output, "PATENTS", filepath.Join(goRoot, "PATENTS")); err != nil {
		return nil, err
	}
	for _, module := range modules {
		if err := appendModuleNotice(&output, module); err != nil {
			return nil, err
		}
	}
	if err := generator.appendIncorporatedNotices(&output); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}

func (generator noticeGenerator) linkedModules(ctx context.Context) ([]noticeModule, error) {
	modulesByKey := map[string]noticeModule{}
	for _, target := range noticeReleaseTargets {
		modules, err := generator.tool.Modules(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("inspect linked modules for %s: %w", target, err)
		}
		for _, module := range modules {
			if module.Path == "" || module.Version == "" || module.Dir == "" {
				return nil, fmt.Errorf("inspect linked modules for %s: incomplete module metadata %+v", target, module)
			}
			modulesByKey[module.key()] = module
		}
	}
	moduleKeys := make([]string, 0, len(modulesByKey))
	for key := range modulesByKey {
		moduleKeys = append(moduleKeys, key)
	}
	sort.Strings(moduleKeys)
	modules := make([]noticeModule, 0, len(moduleKeys))
	for _, key := range moduleKeys {
		modules = append(modules, modulesByKey[key])
	}
	return modules, nil
}

func appendNoticeHeader(output *strings.Builder, goVersion string) {
	output.WriteString("# Third-party notices\n\n")
	output.WriteString("This file is generated from the Go runtime and external-module union linked into ")
	output.WriteString("the four supported `./cmd/fkf` release targets. Regenerate it with ")
	output.WriteString("`mise run generate:notices`; `mise run check` rejects drift. The project itself is ")
	output.WriteString("licensed separately in `LICENSE`.\n\n")
	fmt.Fprintf(output, "## Go runtime %s\n\n", goVersion)
	output.WriteString("Source: <https://go.dev/>\n")
}

func appendModuleNotice(output *strings.Builder, module noticeModule) error {
	fmt.Fprintf(output, "\n## %s %s\n\n", module.Path, module.Version)
	fmt.Fprintf(output, "Source: <https://pkg.go.dev/%s@%s>\n", module.Path, module.Version)
	found := false
	for _, name := range moduleLegalFileNames {
		legalPath := filepath.Join(module.Dir, name)
		info, err := os.Stat(legalPath)
		if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect legal file %s: %w", legalPath, err)
		}
		if err := appendNoticeLegalFile(output, name, legalPath); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("no license file for %s %s", module.Path, module.Version)
	}
	return nil
}

var moduleLegalFileNames = []string{
	"LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "COPYING.txt",
	"NOTICE", "NOTICE.txt", "NOTICE.md", "PATENTS",
}

func (generator noticeGenerator) appendIncorporatedNotices(output *strings.Builder) error {
	for _, incorporated := range incorporatedNoticeSources {
		fmt.Fprintf(output, "\n## %s\n\n", incorporated.heading)
		output.WriteString(incorporated.provenance)
		output.WriteString("\n\nSource: <" + incorporated.source + ">\n")
		fixture := filepath.Join(generator.root, "services", "testdata", "notices", incorporated.fixture)
		if err := appendNoticeLegalFile(output, "LICENSE", fixture); err != nil {
			return err
		}
	}
	return nil
}

var incorporatedNoticeSources = []struct {
	heading, provenance, source, fixture string
}{
	{
		heading: "Incorporated source: xrash/smetrics 5f08fbb34913",
		provenance: "Provenance: `github.com/urfave/cli/v3@v3.11.0/suggestions.go` states that its Jaro " +
			"and Jaro-Winkler implementations were adapted from this exact revision.",
		source:  "https://github.com/xrash/smetrics/tree/5f08fbb34913bc8ab95bb4f2a89a0637ca922666",
		fixture: "xrash-smetrics-LICENSE.txt",
	},
	{
		heading: "Incorporated source: go-mgo/mgo bson 9856a29383ce",
		provenance: "Provenance: `go.yaml.in/yaml/v3@v3.0.5/yaml.go` states that its structure-field " +
			"mapping section was copied from `mgo/bson`.",
		source:  "https://github.com/go-mgo/mgo/tree/9856a29383ce1c59f308dd1cf0363a79b5bef6b5/bson",
		fixture: "mgo-LICENSE.txt",
	},
	{
		heading: "Incorporated source: protocolbuffers/upb 22182e6e54e8",
		provenance: "Provenance: `github.com/segmentio/encoding@v0.5.4/iso8601/parse.go` lines 149–163 " +
			"derive the civil-date conversion from this exact revision.",
		source:  "https://github.com/protocolbuffers/upb/tree/22182e6e54e892f93f26d0522487997d30f604af",
		fixture: "upb-LICENSE.txt",
	},
}

func appendNoticeLegalFile(output *strings.Builder, heading, legalPath string) error {
	data, err := os.ReadFile(legalPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("required legal file is missing: %s", legalPath)
	}
	if err != nil {
		return fmt.Errorf("read legal file %s: %w", legalPath, err)
	}
	normalized, err := normalizeNoticeLegalText(data)
	if err != nil {
		return fmt.Errorf("normalize legal file %s: %w", legalPath, err)
	}
	fmt.Fprintf(output, "\n### %s\n\n```text\n", heading)
	output.WriteString(normalized)
	output.WriteString("```\n")
	return nil
}

func normalizeNoticeLegalText(data []byte) (string, error) {
	if bytes.Contains(data, []byte("```")) {
		return "", errors.New("cannot fence legal text containing triple backticks")
	}
	lines := strings.Split(string(data), "\n")
	normalized := make([]string, 0, len(lines))
	started, pendingBlankLines := false, 0
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			if started {
				pendingBlankLines++
			}
			continue
		}
		for pendingBlankLines > 0 {
			normalized = append(normalized, "")
			pendingBlankLines--
		}
		normalized = append(normalized, line)
		started = true
	}
	if len(normalized) == 0 {
		return "", nil
	}
	return strings.Join(normalized, "\n") + "\n", nil
}

func TestThirdPartyNoticesAreCurrent(t *testing.T) {
	root := repositoryRoot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	generated, err := (noticeGenerator{root: root, tool: commandNoticeGoTool{root: root}}).generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "THIRD_PARTY_NOTICES.md")
	if *updateThirdPartyNotices {
		if err := writeNoticeAtomically(path, generated); err != nil {
			t.Fatal(err)
		}
		return
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read THIRD_PARTY_NOTICES.md: %v; run mise run generate:notices", err)
	}
	if !bytes.Equal(current, generated) {
		t.Fatalf("THIRD_PARTY_NOTICES.md is stale; run mise run generate:notices; %s",
			firstNoticeDifference(current, generated))
	}
}

func writeNoticeAtomically(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".THIRD_PARTY_NOTICES.md-")
	if err != nil {
		return fmt.Errorf("create temporary notice file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary notice mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary notice file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary notice file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace THIRD_PARTY_NOTICES.md: %w", err)
	}
	return nil
}

func firstNoticeDifference(current, generated []byte) string {
	currentLines := strings.Split(string(current), "\n")
	generatedLines := strings.Split(string(generated), "\n")
	shared := min(len(currentLines), len(generatedLines))
	for index := range shared {
		if currentLines[index] != generatedLines[index] {
			return fmt.Sprintf("line %d is %q, want %q", index+1, currentLines[index], generatedLines[index])
		}
	}
	return fmt.Sprintf("file has %d lines, generated output has %d", len(currentLines), len(generatedLines))
}
