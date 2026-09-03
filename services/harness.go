package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/fmind/fkf/core"
)

const (
	HarnessFragmentJSON = "json"
	HarnessFragmentTOML = "toml"
	HarnessFragmentFile = "file"
	HarnessFragmentLink = "link"
)

const (
	harnessManagedStart = "# >>> fkf harness "
	harnessManagedEnd   = "# <<< fkf harness "
	harnessBackupSuffix = ".fkf.bak"
)

// ErrHarnessName reports a name outside the closed harness vocabulary.
var ErrHarnessName = errors.New("unknown harness")

// ErrHarnessConflict reports an existing entry at an FKF-owned key that was not written by FKF.
// The installer refuses it instead of silently taking ownership of somebody else's command.
var ErrHarnessConflict = errors.New("conflicting unmanaged harness entry")

var harnessOrder = []string{
	"claude", "codex", "gemini", "copilot", "antigravity",
	"opencode", "grok", "cursor", "kiro", "cline",
}

// HarnessFragment is one pasteable config fragment or skills link. Install uses these same
// fragments, so `harness print` cannot drift into being a second hand-maintained recipe.
type HarnessFragment struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Selector string `json:"selector,omitempty"`
	Content  string `json:"content"`

	value       any
	array       bool
	managedKind string
	mode        os.FileMode
}

// HarnessPlan is the complete integration contract for one named harness and one absolute base.
type HarnessPlan struct {
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	Fragments []HarnessFragment `json:"fragments"`
	Notes     []string          `json:"notes,omitempty"`
}

// HarnessInstallRequest selects harnesses and whether the installer writes, previews, or checks.
// Home is injectable for hermetic callers; an empty value uses the current process HOME.
type HarnessInstallRequest struct {
	Names      []string
	All        bool
	DryRun     bool
	Check      bool
	Home       string
	Executable string
}

// HarnessChange names one exact filesystem mutation needed for the selected base.
type HarnessChange struct {
	Harness string `json:"harness"`
	Action  string `json:"action"`
	Path    string `json:"path"`
	Backup  string `json:"backup,omitempty"`
}

// HarnessInstallReport is both the dry-run plan and the post-install receipt. Complete means the
// selected integrations match the requested base; it is false for a dry-run or drift check.
type HarnessInstallReport struct {
	Base      string          `json:"base"`
	Mode      string          `json:"mode"`
	Harnesses []string        `json:"harnesses"`
	Complete  bool            `json:"complete"`
	Changes   []HarnessChange `json:"changes"`
}

// HarnessNames returns the closed supported vocabulary in CLI display order.
func HarnessNames() []string { return append([]string(nil), harnessOrder...) }

// HarnessPlanFor renders one harness's managed fragments for an absolute base and executable.
func HarnessPlanFor(baseRoot, name, executable string) (*HarnessPlan, error) {
	baseRoot, err := validateHarnessBase(baseRoot)
	if err != nil {
		return nil, err
	}
	executable, err = validateHarnessExecutable(executable)
	if err != nil {
		return nil, err
	}
	if !knownHarness(name) {
		return nil, fmt.Errorf("%w %q; expected %s", ErrHarnessName, name, strings.Join(harnessOrder, ", "))
	}
	return buildHarnessPlan(baseRoot, name, executable), nil
}

// InstallHarnesses preflights every selected file before the first write. Dry-run and check never
// write; check differs only in the report mode so the CLI can give drift the documented exit 1.
func InstallHarnesses(
	ctx context.Context, baseRoot string, request HarnessInstallRequest,
) (*HarnessInstallReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	baseRoot, err := validateHarnessBase(baseRoot)
	if err != nil {
		return nil, err
	}
	names, err := selectHarnesses(request)
	if err != nil {
		return nil, err
	}
	home, err := harnessHome(request.Home)
	if err != nil {
		return nil, err
	}
	executable, err := validateHarnessExecutable(request.Executable)
	if err != nil {
		return nil, err
	}
	if err := validateHarnessAssets(baseRoot); err != nil {
		return nil, err
	}

	report := newHarnessInstallReport(baseRoot, names, request)

	plans := make([]*HarnessPlan, 0, len(names))
	for _, name := range names {
		plans = append(plans, buildHarnessPlan(baseRoot, name, executable))
	}
	files, links, err := preflightHarnessPlans(ctx, home, plans)
	if err != nil {
		return nil, err
	}
	report.Changes = harnessChanges(files, links)
	if len(report.Changes) == 0 {
		return report, nil
	}
	report.Complete = false
	if request.DryRun || request.Check {
		return report, nil
	}

	// The home-owned targets are outside FKF's base lock. Revalidate the complete plan
	// immediately before the first mutation so a concurrent editor is never overwritten
	// from stale preflight bytes or link targets.
	if err := revalidateHarnessMutations(files, links); err != nil {
		return nil, err
	}
	if err := applyHarnessFiles(ctx, files); err != nil {
		return nil, err
	}
	if err := applyHarnessLinks(ctx, links); err != nil {
		return nil, err
	}
	report.Complete = true
	return report, nil
}

func validateHarnessExecutable(executable string) (string, error) {
	if executable == "" || !filepath.IsAbs(executable) {
		return "", fmt.Errorf("%w: FKF executable must be an absolute path", ErrHarnessConflict)
	}
	return filepath.Clean(executable), nil
}

func newHarnessInstallReport(baseRoot string, names []string, request HarnessInstallRequest) *HarnessInstallReport {
	mode := "install"
	if request.DryRun {
		mode = "dry-run"
	}
	if request.Check {
		mode = "check"
	}
	return &HarnessInstallReport{Base: baseRoot, Mode: mode, Harnesses: names, Complete: true}
}

func harnessChanges(files []harnessFileMutation, links []harnessLinkMutation) []HarnessChange {
	changes := make([]HarnessChange, 0, len(files)+len(links))
	for _, file := range files {
		if file.changed {
			changes = append(changes, harnessChange(file.harness, file.action, file.path, file.exists))
		}
	}
	for _, link := range links {
		if link.changed {
			changes = append(changes, harnessChange(link.harness, link.action, link.path, link.exists))
		}
	}
	return changes
}

func harnessChange(harness, action, path string, backup bool) HarnessChange {
	change := HarnessChange{Harness: harness, Action: action, Path: path}
	if backup {
		change.Backup = path + harnessBackupSuffix
	}
	return change
}

func applyHarnessFiles(ctx context.Context, files []harnessFileMutation) error {
	if err := revalidateHarnessFiles(files); err != nil {
		return err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !file.changed {
			continue
		}
		if err := revalidateHarnessFile(file); err != nil {
			return err
		}
		if file.exists {
			if err := core.WriteFileAtomicMode(file.path+harnessBackupSuffix, file.before, file.mode); err != nil {
				return fmt.Errorf("back up harness config %s: %w", file.path, err)
			}
		}
		if err := core.WriteFileAtomicMode(file.path, file.after, file.mode); err != nil {
			return fmt.Errorf("write harness config %s: %w", file.path, err)
		}
	}
	return nil
}

func applyHarnessLinks(ctx context.Context, links []harnessLinkMutation) error {
	if err := revalidateHarnessLinks(links); err != nil {
		return err
	}
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !link.changed {
			continue
		}
		if err := revalidateHarnessLink(link); err != nil {
			return err
		}
		if link.exists {
			if err := replaceSymlink(link.path+harnessBackupSuffix, link.before); err != nil {
				return fmt.Errorf("back up skills link %s: %w", link.path, err)
			}
		}
		if err := replaceSymlink(link.path, link.after); err != nil {
			return fmt.Errorf("write skills link %s: %w", link.path, err)
		}
	}
	return nil
}

func validateHarnessBase(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("harness base path must be absolute")
	}
	return filepath.Clean(root), nil
}

func knownHarness(name string) bool {
	for _, candidate := range harnessOrder {
		if name == candidate {
			return true
		}
	}
	return false
}

func selectHarnesses(request HarnessInstallRequest) ([]string, error) {
	if request.Check && request.DryRun {
		return nil, fmt.Errorf("--check and --dry-run cannot be combined")
	}
	if request.All && len(request.Names) > 0 {
		return nil, fmt.Errorf("--all cannot be combined with harness names")
	}
	if !request.All && len(request.Names) == 0 {
		return nil, fmt.Errorf("select one or more harness names, or use --all")
	}
	if request.All {
		return HarnessNames(), nil
	}
	seen := make(map[string]bool, len(request.Names))
	names := make([]string, 0, len(request.Names))
	for _, name := range request.Names {
		if !knownHarness(name) {
			return nil, fmt.Errorf("%w %q; expected %s", ErrHarnessName, name, strings.Join(harnessOrder, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("harness %q is selected more than once", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

func harnessHome(explicit string) (string, error) {
	home := explicit
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for harness config: %w", err)
		}
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("harness home path must be absolute")
	}
	return filepath.Clean(home), nil
}

func validateHarnessAssets(baseRoot string) error {
	hook := filepath.Join(baseRoot, core.BaseBinDir, "fkf-hook.sh")
	info, err := os.Lstat(hook)
	if err != nil {
		return fmt.Errorf("inspect harness hook %s: %w", hook, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("harness hook %s is not an executable non-symlink regular file", hook)
	}
	for _, skill := range BundledSkills {
		path := filepath.Join(baseRoot, core.BaseSkillsDir, skill)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err == nil {
				err = fmt.Errorf("%w: not a real directory", core.ErrUnsafePath)
			}
			return fmt.Errorf("inspect harness skill %s: %w", path, err)
		}
		if err := validateSkillTree(path); err != nil {
			return fmt.Errorf("inspect harness skill %s: %w", path, err)
		}
	}
	return nil
}

type harnessFileMutation struct {
	harness       string
	path          string
	before, after []byte
	beforeMode    os.FileMode
	mode          os.FileMode
	exists        bool
	changed       bool
	action        string
}

type harnessLinkMutation struct {
	harness string
	path    string
	before  string
	after   string
	exists  bool
	changed bool
	action  string
}

type harnessFileGroup struct {
	harness   string
	path      string
	fragments []HarnessFragment
}

func preflightHarnessPlans(
	ctx context.Context, home string, plans []*HarnessPlan,
) ([]harnessFileMutation, []harnessLinkMutation, error) {
	groups, links, err := groupHarnessPlans(home, plans)
	if err != nil {
		return nil, nil, err
	}
	files := make([]harnessFileMutation, 0, len(groups))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		mutation, err := preflightHarnessFile(group.harness, group.path, group.fragments)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, mutation)
	}
	for index := range links {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := preflightHarnessLink(&links[index]); err != nil {
			return nil, nil, err
		}
	}
	return files, links, nil
}

func groupHarnessPlans(home string, plans []*HarnessPlan) ([]harnessFileGroup, []harnessLinkMutation, error) {
	groups := make([]harnessFileGroup, 0)
	groupIndex := map[string]int{}
	links := make([]harnessLinkMutation, 0)
	linkIndex := map[string]int{}
	for _, plan := range plans {
		for _, fragment := range plan.Fragments {
			path, err := expandHarnessPath(home, fragment.Path)
			if err != nil {
				return nil, nil, err
			}
			if fragment.Kind == HarnessFragmentLink {
				if index, exists := linkIndex[path]; exists {
					if links[index].after != fragment.Content {
						return nil, nil, fmt.Errorf("%w: harnesses select different targets for %s", ErrHarnessConflict, path)
					}
					continue
				}
				linkIndex[path] = len(links)
				links = append(links, harnessLinkMutation{harness: plan.Name, path: path, after: fragment.Content})
				continue
			}
			if index, exists := groupIndex[path]; exists {
				groups[index].fragments = append(groups[index].fragments, fragment)
				continue
			}
			groupIndex[path] = len(groups)
			groups = append(groups, harnessFileGroup{harness: plan.Name, path: path, fragments: []HarnessFragment{fragment}})
		}
	}
	return groups, links, nil
}

func expandHarnessPath(home, path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return "", fmt.Errorf("harness target %q is not home-relative", path)
	}
	relative := filepath.FromSlash(strings.TrimPrefix(path, "~/"))
	if relative == "." || relative == "" || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("harness target %q escapes home", path)
	}
	target := filepath.Join(home, relative)
	if rel, err := filepath.Rel(home, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("harness target %q escapes home", path)
	}
	return target, nil
}

func preflightHarnessFile(
	harness, path string, fragments []HarnessFragment,
) (harnessFileMutation, error) {
	mutation := harnessFileMutation{harness: harness, path: path, mode: fragments[0].mode, action: "create"}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return mutation, fmt.Errorf("%w: harness config %s is a symlink", ErrHarnessConflict, path)
		}
		if !info.Mode().IsRegular() {
			return mutation, fmt.Errorf("%w: harness config %s is not a regular file", ErrHarnessConflict, path)
		}
		mutation.exists = true
		mutation.action = "update"
		mutation.beforeMode = info.Mode().Perm()
		mutation.mode = mutation.beforeMode
		if fragments[0].mode&0o111 != 0 {
			mutation.mode = fragments[0].mode
		}
		mutation.before, err = core.ReadFileLimit(path, core.MaxControlFileBytes)
		if err != nil {
			return mutation, fmt.Errorf("read harness config %s: %w", path, err)
		}
	case os.IsNotExist(err):
		mutation.before = nil
		err = nil
	default:
		return mutation, fmt.Errorf("inspect harness config %s: %w", path, err)
	}

	kind := fragments[0].Kind
	for _, fragment := range fragments[1:] {
		if fragment.Kind != kind {
			return mutation, fmt.Errorf("internal harness plan mixes formats for %s", path)
		}
	}
	switch kind {
	case HarnessFragmentJSON:
		mutation.after, err = mergeHarnessJSON(path, mutation.before, fragments)
	case HarnessFragmentTOML:
		mutation.after, err = mergeHarnessTOML(path, harness, mutation.before, fragments[0].Content)
	case HarnessFragmentFile:
		mutation.after = []byte(fragments[0].Content)
		if mutation.exists && !bytes.Equal(mutation.before, mutation.after) && !isManagedHarnessFile(mutation.before, harness) {
			err = fmt.Errorf("%w: %s already exists and FKF does not own it", ErrHarnessConflict, path)
		}
	default:
		err = fmt.Errorf("internal harness plan has unknown format %q", kind)
	}
	if err != nil {
		return mutation, err
	}
	mutation.changed = !bytes.Equal(mutation.before, mutation.after)
	return mutation, nil
}

func mergeHarnessJSON(path string, before []byte, fragments []HarnessFragment) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(before)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(before))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return nil, fmt.Errorf("decode harness config %s: %w", path, err)
		}
		if root == nil {
			return nil, fmt.Errorf("decode harness config %s: root must be an object", path)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err != nil {
				return nil, fmt.Errorf("decode harness config %s: trailing data: %w", path, err)
			}
			return nil, fmt.Errorf("decode harness config %s: multiple JSON values", path)
		}
	}
	semanticBefore, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode existing harness config %s: %w", path, err)
	}
	for _, fragment := range fragments {
		if err := applyHarnessJSONFragment(path, root, fragment); err != nil {
			return nil, err
		}
	}
	semanticAfter, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode merged harness config %s: %w", path, err)
	}
	// Harnesses may reformat their own files; unchanged FKF selectors must not make
	// the installer claim or rewrite unrelated host-owned bytes.
	if bytes.Equal(semanticBefore, semanticAfter) {
		return before, nil
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode harness config %s: %w", path, err)
	}
	return append(encoded, '\n'), nil
}

func applyHarnessJSONFragment(path string, root map[string]any, fragment HarnessFragment) error {
	parts := strings.Split(fragment.Selector, ".")
	parent := root
	for _, part := range parts[:len(parts)-1] {
		value, exists := parent[part]
		if !exists {
			next := map[string]any{}
			parent[part] = next
			parent = next
			continue
		}
		next, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s defines %s as a non-object", ErrHarnessConflict, path, part)
		}
		parent = next
	}
	key := parts[len(parts)-1]
	existing, exists := parent[key]
	if fragment.array {
		var entries []any
		if exists {
			var ok bool
			entries, ok = existing.([]any)
			if !ok {
				return fmt.Errorf("%w: %s defines %s as a non-array", ErrHarnessConflict, path, fragment.Selector)
			}
		}
		for index, entry := range entries {
			if reflect.DeepEqual(entry, fragment.value) {
				return nil
			}
			if jsonValueManaged(entry, fragment.managedKind, "") {
				entries[index] = fragment.value
				parent[key] = entries
				return nil
			}
		}
		parent[key] = append(entries, fragment.value)
		return nil
	}
	if !exists {
		parent[key] = fragment.value
		return nil
	}
	if reflect.DeepEqual(existing, fragment.value) {
		return nil
	}
	if !jsonValueManaged(existing, fragment.managedKind, "") {
		return fmt.Errorf("%w: %s already defines %s and FKF does not own it", ErrHarnessConflict, path, fragment.Selector)
	}
	// The selector names the complete FKF-managed value. Preserve its surrounding object,
	// but replace the managed value exactly so extra behavior cannot hide as an allowed subset.
	parent[key] = fragment.value
	return nil
}

func jsonValueManaged(value any, kind, harness string) bool {
	switch kind {
	case "mcp":
		entry, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if command, ok := entry["command"].(string); ok && isFKFExecutable(command) {
			args, _ := entry["args"].([]any)
			return argvHasFKFMCP(args)
		}
		if command, ok := entry["command"].([]any); ok {
			return argvHasFKFMCP(command)
		}
		return false
	case "hook":
		return findHarnessHookString(value, harness)
	case "scalar":
		return false
	default:
		return false
	}
}

func argvHasFKFMCP(argv []any) bool {
	values := make([]string, 0, len(argv))
	for _, value := range argv {
		text, ok := value.(string)
		if !ok {
			return false
		}
		values = append(values, text)
	}
	if len(values) >= 5 && isFKFExecutable(values[0]) {
		values = values[1:]
	}
	return len(values) == 4 && values[0] == "mcp" && values[1] == "serve" && values[2] == "--base"
}

func isFKFExecutable(command string) bool {
	// The selector is already the dedicated `fkf` entry and argvHasFKFMCP checks the exact
	// subcommand shape. Accept an absolute renamed build as owned too: Go test binaries and
	// locally staged release candidates do not necessarily have the basename `fkf`.
	return command == "fkf" || filepath.IsAbs(command)
}

func findHarnessHookString(value any, harness string) bool {
	switch value := value.(type) {
	case string:
		if !strings.Contains(value, "fkf-hook.sh") {
			return false
		}
		return harness == "" || strings.Contains(value, " "+harness)
	case []any:
		for _, child := range value {
			if findHarnessHookString(child, harness) {
				return true
			}
		}
	case map[string]any:
		for _, child := range value {
			if findHarnessHookString(child, harness) {
				return true
			}
		}
	}
	return false
}

func mergeHarnessTOML(path, harness string, before []byte, desired string) ([]byte, error) {
	text := string(before)
	startMarker := harnessManagedStart + harness
	endMarker := harnessManagedEnd + harness
	start := strings.Index(text, startMarker)
	end := strings.Index(text, endMarker)
	if (start >= 0) != (end >= 0) || (start >= 0 && end < start) {
		return nil, fmt.Errorf("%w: %s has an incomplete FKF managed block", ErrHarnessConflict, path)
	}
	if start >= 0 {
		end += len(endMarker)
		if end < len(text) && text[end] == '\r' {
			end++
		}
		if end < len(text) && text[end] == '\n' {
			end++
		}
		return []byte(text[:start] + desired + text[end:]), nil
	}
	section := regexp.MustCompile(`(?m)^\s*\[mcp_servers\.fkf\]\s*(?:#.*)?$`)
	if section.MatchString(text) || (strings.Contains(text, "fkf-hook.sh") && strings.Contains(text, harness)) {
		return nil, fmt.Errorf("%w: %s already defines an FKF MCP server or hook outside a managed block", ErrHarnessConflict, path)
	}
	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if strings.TrimSpace(text) != "" {
		text += "\n"
	}
	return []byte(text + desired), nil
}

func isManagedHarnessFile(content []byte, harness string) bool {
	marker := "Managed by fkf harness install: " + harness
	return bytes.Contains(content, []byte(marker))
}

func preflightHarnessLink(link *harnessLinkMutation) error {
	info, err := os.Lstat(link.path)
	switch {
	case os.IsNotExist(err):
		link.changed = true
		link.action = "link"
		return nil
	case err != nil:
		return fmt.Errorf("inspect skills bridge %s: %w", link.path, err)
	case info.Mode()&os.ModeSymlink == 0:
		return fmt.Errorf("%w: skills bridge %s already exists and is not a symlink", ErrHarnessConflict, link.path)
	}
	link.exists = true
	link.before, err = os.Readlink(link.path)
	if err != nil {
		return fmt.Errorf("read skills bridge %s: %w", link.path, err)
	}
	if link.before == link.after {
		return nil
	}
	if !isManagedSkillTarget(link.before) {
		return fmt.Errorf("%w: skills bridge %s points to an unmanaged target", ErrHarnessConflict, link.path)
	}
	link.changed = true
	link.action = "relink"
	return nil
}

func revalidateHarnessMutations(files []harnessFileMutation, links []harnessLinkMutation) error {
	if err := revalidateHarnessFiles(files); err != nil {
		return err
	}
	return revalidateHarnessLinks(links)
}

func revalidateHarnessFiles(files []harnessFileMutation) error {
	for _, file := range files {
		if file.changed {
			if err := revalidateHarnessFile(file); err != nil {
				return err
			}
		}
	}
	return nil
}

func revalidateHarnessFile(file harnessFileMutation) error {
	info, err := os.Lstat(file.path)
	if !file.exists {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect harness config %s before writing: %w", file.path, err)
		}
		return fmt.Errorf("%w: harness config %s appeared after preflight", ErrHarnessConflict, file.path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: harness config %s disappeared after preflight", ErrHarnessConflict, file.path)
	}
	if err != nil {
		return fmt.Errorf("inspect harness config %s before writing: %w", file.path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: harness config %s changed type after preflight", ErrHarnessConflict, file.path)
	}
	if info.Mode().Perm() != file.beforeMode {
		return fmt.Errorf("%w: harness config %s changed mode after preflight", ErrHarnessConflict, file.path)
	}
	current, err := core.ReadFileLimit(file.path, core.MaxControlFileBytes)
	if err != nil {
		return fmt.Errorf("read harness config %s before writing: %w", file.path, err)
	}
	if !bytes.Equal(current, file.before) {
		return fmt.Errorf("%w: harness config %s changed after preflight", ErrHarnessConflict, file.path)
	}
	return nil
}

func revalidateHarnessLinks(links []harnessLinkMutation) error {
	for _, link := range links {
		if link.changed {
			if err := revalidateHarnessLink(link); err != nil {
				return err
			}
		}
	}
	return nil
}

func revalidateHarnessLink(link harnessLinkMutation) error {
	info, err := os.Lstat(link.path)
	if !link.exists {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect skills bridge %s before writing: %w", link.path, err)
		}
		return fmt.Errorf("%w: skills bridge %s appeared after preflight", ErrHarnessConflict, link.path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: skills bridge %s disappeared after preflight", ErrHarnessConflict, link.path)
	}
	if err != nil {
		return fmt.Errorf("inspect skills bridge %s before writing: %w", link.path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%w: skills bridge %s changed type after preflight", ErrHarnessConflict, link.path)
	}
	current, err := os.Readlink(link.path)
	if err != nil {
		return fmt.Errorf("read skills bridge %s before writing: %w", link.path, err)
	}
	if current != link.before {
		return fmt.Errorf("%w: skills bridge %s changed after preflight", ErrHarnessConflict, link.path)
	}
	return nil
}

func isManagedSkillTarget(target string) bool {
	target = filepath.ToSlash(filepath.Clean(target))
	for _, skill := range BundledSkills {
		if strings.HasSuffix(target, "/.agents/skills/"+skill) {
			return true
		}
	}
	return false
}

func replaceSymlink(path, target string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.link")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := os.Symlink(target, temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return core.SyncDirectory(directory)
}
