package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/fmind/fkf/core"
)

const (
	HarnessFragmentJSON = "json"
	HarnessFragmentTOML = "toml"
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
	managedBase string
	managedKey  string
	workspace   string
	mode        os.FileMode
}

// HarnessPlan is the complete integration contract for one named harness and one absolute base.
type HarnessPlan struct {
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	BaseName  string            `json:"base_name"`
	Workspace string            `json:"workspace,omitempty"`
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
	Workspace  string
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
	BaseName  string          `json:"base_name"`
	Workspace string          `json:"workspace,omitempty"`
	Mode      string          `json:"mode"`
	Harnesses []string        `json:"harnesses"`
	Complete  bool            `json:"complete"`
	Changes   []HarnessChange `json:"changes"`
}

// HarnessNames returns the closed supported vocabulary in CLI display order.
func HarnessNames() []string { return append([]string(nil), harnessOrder...) }

// HarnessPlanFor renders one harness's managed fragments for an absolute base and executable.
func HarnessPlanFor(baseRoot, name, executable, workspace string) (*HarnessPlan, error) {
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
	baseName, err := harnessBaseName(baseRoot)
	if err != nil {
		return nil, err
	}
	selectedWorkspace, err := validateHarnessWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return buildHarnessPlan(baseRoot, baseName, name, executable, selectedWorkspace), nil
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
	workspace, err := validateHarnessWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	baseName, err := harnessBaseName(baseRoot)
	if err != nil {
		return nil, err
	}
	if err := validateHarnessAssets(baseRoot, workspace != ""); err != nil {
		return nil, err
	}

	report := newHarnessInstallReport(baseRoot, baseName, workspace, names, request)

	plans := make([]*HarnessPlan, 0, len(names))
	for _, name := range names {
		plans = append(plans, buildHarnessPlan(baseRoot, baseName, name, executable, workspace))
	}
	files, err := preflightHarnessPlans(ctx, home, plans)
	if err != nil {
		return nil, err
	}
	report.Changes = harnessChanges(files)
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
	if err := revalidateHarnessFiles(files); err != nil {
		return nil, err
	}
	if err := applyHarnessFiles(ctx, files); err != nil {
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

func newHarnessInstallReport(
	baseRoot, baseName, workspace string, names []string, request HarnessInstallRequest,
) *HarnessInstallReport {
	mode := "install"
	if request.DryRun {
		mode = "dry-run"
	}
	if request.Check {
		mode = "check"
	}
	return &HarnessInstallReport{
		Base: baseRoot, BaseName: baseName, Workspace: workspace,
		Mode: mode, Harnesses: names, Complete: true,
	}
}

func harnessChanges(files []harnessFileMutation) []HarnessChange {
	changes := make([]HarnessChange, 0, len(files))
	for _, file := range files {
		if file.changed {
			changes = append(changes, harnessChange(file.harness, file.action, file.path, file.exists))
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

func validateHarnessBase(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("harness base path must be absolute")
	}
	physical, err := core.ResolvePhysicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve harness base: %w", err)
	}
	return physical, nil
}

func harnessBaseName(root string) (string, error) {
	config, err := core.LoadConfig(root)
	if err != nil {
		return "", fmt.Errorf("load harness base identity: %w", err)
	}
	return config.Name, nil
}

func validateHarnessWorkspace(workspace string) (string, error) {
	if workspace == "" {
		return "", nil
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("harness workspace must be an absolute directory")
	}
	physical, err := core.ResolvePhysicalPath(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve harness workspace: %w", err)
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", fmt.Errorf("inspect harness workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("harness workspace must be a directory")
	}
	return physical, nil
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

func validateHarnessAssets(baseRoot string, needsHook bool) error {
	if !needsHook {
		return nil
	}
	hook := filepath.Join(baseRoot, core.BaseBinDir, "fkf-hook.sh")
	info, err := os.Lstat(hook)
	if err != nil {
		return fmt.Errorf("inspect harness hook %s: %w", hook, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("harness hook %s is not an executable non-symlink regular file", hook)
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

type harnessFileGroup struct {
	harness   string
	path      string
	fragments []HarnessFragment
}

func preflightHarnessPlans(
	ctx context.Context, home string, plans []*HarnessPlan,
) ([]harnessFileMutation, error) {
	for _, plan := range plans {
		if plan.Name == "kiro" && plan.Workspace != "" {
			if err := checkKiroHookWorkspaces(ctx, home, plan); err != nil {
				return nil, err
			}
		}
	}
	groups, err := groupHarnessPlans(home, plans)
	if err != nil {
		return nil, err
	}
	files := make([]harnessFileMutation, 0, len(groups))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mutation, err := preflightHarnessFile(group.harness, group.path, group.fragments)
		if err != nil {
			return nil, err
		}
		files = append(files, mutation)
	}
	return files, nil
}

// Kiro stores each base's hook in a separate file; a single-file merge cannot detect a
// conflicting workspace in a peer file. Inspect only its hook directory, without execution.
func checkKiroHookWorkspaces(ctx context.Context, home string, plan *HarnessPlan) error {
	directory := filepath.Join(home, ".kiro", "hooks")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Kiro hook workspaces: %w", err)
	}
	fragment := HarnessFragment{managedBase: plan.Base, workspace: plan.Workspace}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, err := core.ReadFileLimitContext(ctx, path, core.MaxControlFileBytes)
		if err != nil {
			return fmt.Errorf("inspect Kiro hook %s: %w", path, err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("decode Kiro hook %s: %w", path, err)
		}
		if err := checkHarnessWorkspaceConflict(path, value, fragment); err != nil {
			return err
		}
	}
	return nil
}

func groupHarnessPlans(home string, plans []*HarnessPlan) ([]harnessFileGroup, error) {
	groups := make([]harnessFileGroup, 0)
	groupIndex := map[string]int{}
	for _, plan := range plans {
		for _, fragment := range plan.Fragments {
			path, err := expandHarnessPath(home, fragment.Path)
			if err != nil {
				return nil, err
			}
			if index, exists := groupIndex[path]; exists {
				groups[index].fragments = append(groups[index].fragments, fragment)
				continue
			}
			groupIndex[path] = len(groups)
			groups = append(groups, harnessFileGroup{harness: plan.Name, path: path, fragments: []HarnessFragment{fragment}})
		}
	}
	return groups, nil
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
		mutation.after, err = mergeHarnessTOML(path, harness, mutation.before, fragments[0])
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
		return applyHarnessJSONArrayFragment(path, parent, key, existing, exists, fragment)
	}
	return applyHarnessJSONObjectFragment(path, parent, key, existing, exists, fragment)
}

func applyHarnessJSONArrayFragment(
	path string, parent map[string]any, key string, existing any, exists bool, fragment HarnessFragment,
) error {
	var entries []any
	if exists {
		var ok bool
		entries, ok = existing.([]any)
		if !ok {
			return fmt.Errorf("%w: %s defines %s as a non-array", ErrHarnessConflict, path, fragment.Selector)
		}
	}
	// Check every peer before replacing our entry: an earlier owned entry must not hide a
	// later conflicting base when its workspace moves.
	if fragment.managedKind == "hook" {
		if err := checkHarnessWorkspaceConflict(path, entries, fragment); err != nil {
			return err
		}
	}
	for index, entry := range entries {
		switch {
		case reflect.DeepEqual(entry, fragment.value):
			return nil
		case jsonValueOwnedByFragment(entry, fragment):
			entries[index] = fragment.value
			parent[key] = entries
			return nil
		}
	}
	parent[key] = append(entries, fragment.value)
	return nil
}

func checkHarnessWorkspaceConflict(path string, value any, fragment HarnessFragment) error {
	var conflict error
	visitHarnessStrings(value, func(command string) {
		if conflict != nil || !strings.Contains(command, "fkf-hook.sh") ||
			harnessMarkerValue(command, "fkf-base") == fragment.managedBase {
			return
		}
		workspace := harnessMarkerValue(command, "fkf-workspace")
		if workspaceScopesOverlap(workspace, fragment.workspace) {
			conflict = fmt.Errorf("%w: %s already has an overlapping FKF hook workspace %s", ErrHarnessConflict, path, workspace)
		}
	})
	return conflict
}

func applyHarnessJSONObjectFragment(
	path string, parent map[string]any, key string, existing any, exists bool, fragment HarnessFragment,
) error {
	if !exists {
		parent[key] = fragment.value
		return nil
	}
	if reflect.DeepEqual(existing, fragment.value) {
		return nil
	}
	if !jsonValueOwnedByFragment(existing, fragment) {
		return fmt.Errorf("%w: %s already defines %s and FKF does not own it", ErrHarnessConflict, path, fragment.Selector)
	}
	// The selector names the complete FKF-managed value. Preserve its surrounding object,
	// but replace the managed value exactly so extra behavior cannot hide as an allowed subset.
	parent[key] = fragment.value
	return nil
}

func jsonValueOwnedByFragment(value any, fragment HarnessFragment) bool {
	if !jsonValueManaged(value, fragment.managedKind, "") {
		return false
	}
	switch fragment.managedKind {
	case "mcp":
		return mcpEntryBase(value) == fragment.managedBase
	case "hook":
		return harnessMarkerValue(value, "fkf-base") == fragment.managedBase
	default:
		return false
	}
}

func mcpEntryBase(value any) string {
	entry, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	argv, _ := entry["args"].([]any)
	if command, ok := entry["command"].([]any); ok {
		argv = command
		if len(argv) > 0 && isFKFExecutable(fmt.Sprint(argv[0])) {
			argv = argv[1:]
		}
	}
	for index := 0; index+1 < len(argv); index++ {
		if argv[index] == "--base" {
			base, _ := argv[index+1].(string)
			return base
		}
	}
	return ""
}

func harnessMarkerValue(value any, name string) string {
	prefix := name + "="
	var found string
	visitHarnessStrings(value, func(text string) {
		if found != "" {
			return
		}
		index := strings.Index(text, prefix)
		if index < 0 {
			return
		}
		encoded := text[index+len(prefix):]
		if end := strings.IndexAny(encoded, " ;\t\r\n"); end >= 0 {
			encoded = encoded[:end]
		}
		if decoded, err := url.PathUnescape(encoded); err == nil {
			found = decoded
		}
	})
	return found
}

func visitHarnessStrings(value any, visit func(string)) {
	switch value := value.(type) {
	case string:
		visit(value)
	case []any:
		for _, child := range value {
			visitHarnessStrings(child, visit)
		}
	case map[string]any:
		for _, child := range value {
			visitHarnessStrings(child, visit)
		}
	}
}

func workspaceScopesOverlap(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	within := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
	}
	return within(left, right) || within(right, left)
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

func mergeHarnessTOML(path, harness string, before []byte, fragment HarnessFragment) ([]byte, error) {
	text := string(before)
	desired := fragment.Content
	marker := harness + " " + fragment.managedKey
	startMarker := harnessManagedStart + marker
	endMarker := harnessManagedEnd + marker
	// Match complete marker lines: the base alpha must not claim alpha-team's block.
	start := harnessMarkerLine(text, startMarker)
	end := harnessMarkerLine(text, endMarker)
	if (start >= 0) != (end >= 0) || (start >= 0 && end < start) {
		return nil, fmt.Errorf("%w: %s has an incomplete FKF managed block", ErrHarnessConflict, path)
	}
	if fragment.workspace != "" {
		for line := range strings.SplitSeq(text, "\n") {
			if err := checkHarnessWorkspaceConflict(path, line, fragment); err != nil {
				return nil, err
			}
		}
	}
	if start >= 0 {
		end += len(endMarker)
		block := text[start:end]
		if !strings.Contains(block, "# base: "+strconv.Quote(fragment.managedBase)) {
			return nil, fmt.Errorf("%w: %s already owns %s for a different base", ErrHarnessConflict, path, fragment.managedKey)
		}
		if end < len(text) && text[end] == '\r' {
			end++
		}
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if fragment.workspace == "" {
			// MCP-only refreshes and status must leave explicitly installed hooks intact,
			// just as the JSON adapters do when no hook fragment was requested.
			if hook := strings.Index(block, "\n[[hooks.SessionStart]]"); hook >= 0 {
				desired = strings.TrimSuffix(desired, endMarker+"\n") + block[hook:] + "\n"
			}
		}
		return []byte(text[:start] + desired + text[end:]), nil
	}
	section := regexp.MustCompile(`(?m)^\s*\[mcp_servers\.` + regexp.QuoteMeta(fragment.managedKey) + `\]\s*(?:#.*)?$`)
	if section.MatchString(text) || strings.Contains(text, "fkf-key="+url.PathEscape(fragment.managedKey)) {
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

func harnessMarkerLine(text, marker string) int {
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(marker) + `\r?$`).FindStringIndex(text)
	if match == nil {
		return -1
	}
	return match[0]
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
