package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	fkf "github.com/fmind/fkf"
	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf init` is the first five minutes. One command creates a complete, trusted,
// git-initialised base from one explicit preset, then prints the next useful actions. On an
// existing base it is a refresh: it rewrites the two owned skills and two managed files, and
// creates missing harness bridges without replacing owner-authored files.

// Presets are the shipped fkf.yaml source sets.
const (
	PresetPersonal = "personal"
	PresetTeam     = "team"
	PresetMinimal  = "minimal"
)

// Presets lists the shipped presets in the order `--help` shows them.
var Presets = []string{PresetMinimal, PresetPersonal, PresetTeam}

// hookScript is the session-start hook every base gets. It is named once because `status`
// checks it for drift: it calls `fkf` itself, so a stale copy is a broken one.
const hookScript = "fkf-hook.sh"

var baseNamePattern = regexp.MustCompile(`[^a-z0-9-]+`)

// InitRequest is one scaffold or refresh.
type InitRequest struct {
	Path           string
	Preset         string
	Name           string
	TrackCollected bool
	Demo           int
	SkipGit        bool
	SkipValidate   bool
}

// InitStep is one thing `init` created or refreshed, in the order it is printed.
type InitStep struct {
	Item    string `json:"item"`
	Detail  string `json:"detail"`
	Changed bool   `json:"changed"`
}

// InitReport is what `fkf init` returns.
type InitReport struct {
	Base           string      `json:"base"`
	Name           string      `json:"name"`
	Preset         string      `json:"preset,omitempty"`
	Created        bool        `json:"created"`
	Refreshed      bool        `json:"refreshed"`
	Declared       int         `json:"declared_sources"`
	Enabled        int         `json:"enabled_sources"`
	TrackCollected bool        `json:"track_collected"`
	Trusted        bool        `json:"trusted"`
	Steps          []InitStep  `json:"steps"`
	Next           []string    `json:"next"`
	Demo           *DemoReport `json:"demo,omitempty"`
}

// initCreatedFile records the filesystem identity of a file this init invocation created.
// Recovery removes only that identity: a pre-existing file, or a path replaced concurrently,
// remains owner-controlled.
type initCreatedFile struct {
	path string
	info fs.FileInfo
}

func (r *InitReport) step(item, detail string, changed bool) {
	r.Steps = append(r.Steps, InitStep{Item: item, Detail: detail, Changed: changed})
}

// Init scaffolds a new base, or refreshes an existing one.
func Init(ctx context.Context, request InitRequest, now func() time.Time) (*InitReport, error) {
	if request.Demo != 0 {
		if err := validateDemoDays(request.Demo); err != nil {
			return nil, err
		}
		if strings.TrimSpace(request.Preset) != "" {
			return nil, fmt.Errorf("%w: --demo uses the minimal configuration; omit --preset", core.ErrConfig)
		}
	}
	declared := core.ExpandHome(strings.TrimSpace(request.Path))
	if declared == "" || filepath.Clean(declared) == "." {
		return nil, fmt.Errorf("%w: `fkf init` needs a path, for example `fkf init ~/brain`", core.ErrConfig)
	}
	root, err := core.ResolveAbsolutePath(declared)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve init path %q: %w", core.ErrConfig, request.Path, err)
	}
	if err := core.ValidateDirectoryConfinement(root); err != nil {
		return nil, err
	}
	// Preflight every path init owns before the first write. A cloned base may place a symlink
	// at any of these names; discovering the last one after refreshing the first would both
	// escape the base and leave a partial scaffold.
	if err := validateScaffoldTargets(ctx, root); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, core.ConfigFileName)); err == nil {
		if request.Demo != 0 {
			return nil, fmt.Errorf("%w: --demo only creates a new demo base; %s already contains %s",
				core.ErrConfig, root, core.ConfigFileName)
		}
		return refresh(ctx, root, request, now)
	}
	return create(ctx, root, request, now)
}

func validateScaffoldTargets(ctx context.Context, root string) error {
	directories := make([]string, 0, 5+len(core.Layers)+len(BundledSkills))
	directories = append(directories,
		filepath.Join(root, ".agents"),
		filepath.Join(root, filepath.FromSlash(core.BaseSkillsDir)),
		filepath.Join(root, core.BaseBinDir),
		filepath.Join(root, core.BaseTestsDir),
		filepath.Join(root, evalDirectory),
		filepath.Join(root, ".claude"),
	)
	for _, layer := range core.Layers {
		directories = append(directories, filepath.Join(root, string(layer)))
	}
	for _, name := range BundledSkills {
		directories = append(directories,
			filepath.Join(root, filepath.FromSlash(core.BaseSkillsDir), name))
	}
	for _, directory := range directories {
		if err := core.ValidateDirectoryConfinement(directory); err != nil {
			return err
		}
	}
	for _, name := range BundledSkills {
		target := filepath.Join(root, filepath.FromSlash(core.BaseSkillsDir), name)
		if err := validateSkillTree(target); err != nil {
			return err
		}
	}
	for _, name := range []string{
		core.ConfigFileName, core.BaseAgentsFile, core.GraphFile, core.GraphDstFile,
		core.GraphOffsetsFile, core.GraphMetaFile, core.GraphGenerationFile,
		".git", ".gitignore", ".gitattributes", "CLAUDE.md",
		filepath.ToSlash(filepath.Join(evalDirectory, evalQueriesFile)),
	} {
		if err := core.ValidatePathConfinement(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	if _, err := core.BinScripts(ctx, root); err != nil {
		return err
	}
	if _, err := core.TestScripts(ctx, root); err != nil {
		return err
	}
	return nil
}

func create(ctx context.Context, root string, request InitRequest, now func() time.Time) (report *InitReport, returnErr error) {
	preset, name, err := initialBaseIdentity(root, request)
	if err != nil {
		return nil, err
	}
	preexistingExecution, err := hasPreexistingExecutionInputs(ctx, root)
	if err != nil {
		return nil, err
	}
	report = &InitReport{Base: root, Name: name, Preset: preset, Created: true, TrackCollected: request.TrackCollected}
	createdFiles := []initCreatedFile{}
	if err := os.MkdirAll(root, core.BaseDirMode); err != nil {
		return nil, fmt.Errorf("create %s: %w", root, err)
	}
	config, err := renderConfig(name, preset, request.Demo != 0)
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(root, core.ConfigFileName)
	if err := core.WriteFileAtomicMode(configPath, []byte(config), core.BaseFileMode); err != nil {
		return nil, err
	}
	// fkf.yaml is the create-vs-refresh marker. If anything after this point fails, keeping
	// that marker would route an ordinary retry into refresh even though AGENTS.md, preset
	// helpers, git metadata or trust may still be absent. Remove the configuration and only
	// those helper/demo files this invocation created; pre-existing base content stays intact.
	defer func() {
		if returnErr == nil {
			return
		}
		if err := rollbackInitCreatedFiles(createdFiles); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
		if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove incomplete init marker %s: %w", configPath, err))
		}
	}()
	loaded, err := core.LoadConfig(root)
	if err != nil {
		return nil, fmt.Errorf("the %s preset produced a configuration this build rejects: %w", preset, err)
	}
	report.Declared, report.Enabled = len(loaded.Sources), len(loaded.EnabledSources())
	report.step(core.ConfigFileName, fmt.Sprintf("%d sources declared, %d enabled", report.Declared, report.Enabled), true)

	if err := scaffoldCreatedBase(ctx, root, name, request, loaded, report, now, &createdFiles); err != nil {
		return nil, err
	}
	// Trust is the final mutation. A failed demo or scaffold must not leave an external record
	// approving a configuration marker that create's recovery defer is about to remove.
	if err := recordInitialTrust(ctx, root, preexistingExecution, report, now); err != nil {
		return nil, err
	}
	report.Next = nextSteps(root, report)
	return report, nil
}

func initialBaseIdentity(root string, request InitRequest) (string, string, error) {
	preset := request.Preset
	if preset == "" {
		preset = PresetMinimal
	}
	if !contains(Presets, preset) {
		return "", "", fmt.Errorf("%w: unknown preset %q; expected %s", core.ErrConfig, preset, strings.Join(Presets, ", "))
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = baseNamePattern.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-")
		name = strings.Trim(name, "-")
	}
	if name == "" {
		name = "brain"
	}
	return preset, name, nil
}

func scaffoldCreatedBase(
	ctx context.Context,
	root, name string,
	request InitRequest,
	loaded *core.Config,
	report *InitReport,
	now func() time.Time,
	createdFiles *[]initCreatedFile,
) error {
	store := loaded.Store()
	for _, layer := range store.EnabledLayers() {
		directory, _ := store.Dir(layer)
		if err := os.MkdirAll(directory, core.BaseDirMode); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	report.step("layers", strings.Join(layerNamesOf(store), " "), true)
	if !request.SkipGit {
		if err := initGit(ctx, root); err != nil {
			return err
		}
	}
	if err := writeManagedBlocks(root, request.TrackCollected, report); err != nil {
		return err
	}
	if err := writeBaseAgents(root, name, report); err != nil {
		return err
	}
	if err := writeStarterEval(root, report, createdFiles); err != nil {
		return err
	}
	if err := writeSkillsAndHelpers(root, loaded, report, createdFiles); err != nil {
		return err
	}
	if err := writeAgentBridges(root, report); err != nil {
		return err
	}
	base := &Base{
		Config: loaded, Store: store, Env: sources.NewEnvironment(loaded),
		Runner: sources.ExecRunner(), Now: now,
	}
	if request.Demo == 0 {
		if _, err := Build(ctx, base, "", false); err != nil {
			return fmt.Errorf("build initial derived files: %w", err)
		}
		return nil
	}
	changed, err := installDemoHelper(root)
	if err != nil {
		return err
	}
	if changed {
		created, err := rememberInitCreatedFile(filepath.Join(root, core.BaseBinDir, demoHelperName))
		if err != nil {
			return err
		}
		*createdFiles = append(*createdFiles, created)
	}
	report.step(core.BaseBinDir+"/"+demoHelperName, "deterministic local demo collector", changed)
	demo, err := WriteDemo(ctx, base, request.Demo)
	if err != nil {
		return err
	}
	report.Demo = demo
	*createdFiles = append(*createdFiles, demo.created...)
	report.step("demo", fmt.Sprintf("%d synthetic days across %d sources", demo.Days, len(demo.Sources)), true)
	return nil
}

func rememberInitCreatedFile(path string) (initCreatedFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return initCreatedFile{}, fmt.Errorf("inspect newly created %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return initCreatedFile{}, fmt.Errorf("%w: newly created %s is not a regular file", core.ErrUnsafePath, path)
	}
	return initCreatedFile{path: path, info: info}, nil
}

func rollbackInitCreatedFiles(files []initCreatedFile) error {
	var rollbackErr error
	for index := len(files) - 1; index >= 0; index-- {
		created := files[index]
		current, err := os.Lstat(created.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("inspect incomplete init output %s: %w", created.path, err))
			continue
		case !os.SameFile(created.info, current):
			continue
		}
		if err := os.Remove(created.path); err != nil {
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("remove incomplete init output %s: %w", created.path, err))
		}
	}
	return rollbackErr
}

func writeBaseAgents(root, name string, report *InitReport) error {
	target := filepath.Join(root, core.BaseAgentsFile)
	_, err := os.Stat(target)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	if err := core.WriteFileAtomicMode(target, []byte(BaseAgentsTemplate(name)), core.BaseFileMode); err != nil {
		return err
	}
	report.step(core.BaseAgentsFile, "routes agents to the copied fkf skills", true)
	return nil
}

func writeStarterEval(root string, report *InitReport, createdFiles *[]initCreatedFile) error {
	relative := filepath.ToSlash(filepath.Join(evalDirectory, evalQueriesFile))
	target := filepath.Join(root, filepath.FromSlash(relative))
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s must be a regular file", core.ErrUnsafePath, relative)
		}
		report.step(relative, "left as the base-owned retrieval acceptance set", false)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", relative, err)
	}
	content, err := fs.ReadFile(fkf.Presets, "presets/evals/queries.yaml")
	if err != nil {
		return fmt.Errorf("read embedded starter evaluation: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), core.BaseDirMode); err != nil {
		return fmt.Errorf("create %s: %w", evalDirectory, err)
	}
	if err := core.WriteFileAtomicMode(target, content, core.BaseFileMode); err != nil {
		return err
	}
	if createdFiles != nil {
		created, err := rememberInitCreatedFile(target)
		if err != nil {
			return err
		}
		*createdFiles = append(*createdFiles, created)
	}
	report.step(relative, "runnable retrieval baseline plus target-journey prompts", true)
	return nil
}

func recordInitialTrust(ctx context.Context, root string, preexisting bool, report *InitReport, now func() time.Time) error {
	if preexisting {
		report.step("trust", "review required: fkf.local.yaml, bin/, or tests/ existed before init; run `fkf trust --all`", false)
		return nil
	}
	config, err := core.LoadConfig(root)
	if err != nil {
		return err
	}
	if _, err := core.WriteTrust(ctx, config, now()); err != nil {
		return err
	}
	report.Trusted = true
	report.step("trusted", "execution plan plus bin/ and tests/ digests recorded for this machine", true)
	return nil
}

func refresh(ctx context.Context, root string, request InitRequest, now func() time.Time) (*InitReport, error) {
	config, err := core.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	report := &InitReport{
		Base: root, Name: config.Name, Refreshed: true,
		Declared: len(config.Sources), Enabled: len(config.EnabledSources()),
	}
	if state, stateErr := core.ReadTrust(ctx, config); stateErr == nil {
		report.Trusted = state.Trusted
	}
	track, err := TracksCollected(root)
	if err != nil {
		return nil, err
	}
	if request.TrackCollected && !track {
		track = true
	}
	report.TrackCollected = track
	if err := writeManagedBlocks(root, track, report); err != nil {
		return nil, err
	}
	if err := writeStarterEval(root, report, nil); err != nil {
		return nil, err
	}
	if err := writeSkillsAndHelpers(root, nil, report, nil); err != nil {
		return nil, err
	}
	if err := writeAgentBridges(root, report); err != nil {
		return nil, err
	}
	base := &Base{
		Config: config, Store: config.Store(), Env: sources.NewEnvironment(config),
		Runner: sources.ExecRunner(), Now: now,
	}
	if _, err := Build(ctx, base, "", false); err != nil {
		return nil, fmt.Errorf("refresh derived files: %w", err)
	}
	report.step(core.ConfigFileName, "left as it is; `init` never rewrites a base's own configuration", false)
	report.step(core.BaseAgentsFile, "left as it is; it belongs to this base", false)
	if state, err := core.ReadTrust(ctx, config); err == nil && !state.Trusted {
		report.step("trust", "the configuration changed since it was trusted; run `fkf trust`", false)
	}
	report.Next = nextSteps(root, report)
	return report, nil
}

func writeManagedBlocks(root string, trackCollected bool, report *InitReport) error {
	ignore, err := planManagedBlock(filepath.Join(root, ".gitignore"), ManagedIgnoreBlock(trackCollected))
	if err != nil {
		return err
	}
	attributes, err := planManagedBlock(filepath.Join(root, ".gitattributes"), ManagedAttributesBlock())
	if err != nil {
		return err
	}
	// Parse and bound both owner-controlled files before writing either. A malformed second
	// marker topology must not leave the first file half-refreshed on an otherwise failed init.
	if err := applyManagedBlockPlan(ignore); err != nil {
		return err
	}
	if err := applyManagedBlockPlan(attributes); err != nil {
		return err
	}
	detail := "events/ and index/ stay out of history (re-create with --track-collected to version them)"
	if trackCollected {
		detail = "events/ and index/ are versioned; history is append-only"
	}
	report.step(".gitignore", detail, ignore.changed)
	report.step(".gitattributes", "JSON layers never line-merge", attributes.changed)
	return nil
}

// hasPreexistingExecutionInputs distinguishes a new base from a directory that already
// controls execution. Auto-trust is safe only for the preset and scripts embedded in this
// binary; a machine-local overlay, bin entry, or test hook that predates init must be shown by
// `fkf trust`.
func hasPreexistingExecutionInputs(ctx context.Context, root string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(root, core.LocalConfigName)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect %s: %w", core.LocalConfigName, err)
	}
	scripts, err := core.BinScripts(ctx, root)
	if err != nil {
		return false, err
	}
	tests, err := core.TestScripts(ctx, root)
	if err != nil {
		return false, err
	}
	return len(scripts) > 0 || len(tests) > 0, nil
}

func writeSkillsAndHelpers(
	root string, config *core.Config, report *InitReport, createdFiles *[]initCreatedFile,
) error {
	states, err := InstallSkills(root)
	if err != nil {
		return err
	}
	changed := false
	for _, state := range states {
		if state.Written {
			changed = true
		}
	}
	report.step(core.BaseSkillsDir+"/", strings.Join(BundledSkills, ", "), changed)

	if err := sources.EnsureBinDir(root); err != nil {
		return err
	}
	written, err := installMissingRequiredHelpers(root, config)
	if err != nil {
		return err
	}
	if len(written) > 0 {
		report.step("bin/", strings.Join(written, ", "), true)
	}
	if createdFiles != nil {
		for _, name := range written {
			created, err := rememberInitCreatedFile(filepath.Join(root, core.BaseBinDir, name))
			if err != nil {
				return err
			}
			*createdFiles = append(*createdFiles, created)
		}
	}
	return nil
}

// writeAgentBridges exposes the canonical AGENTS.md and .agents/skills package to Claude
// without copying either. Claude is the one supported harness that uses a different native
// skill directory; a relative link keeps every harness on the same bytes after an init refresh.
func writeAgentBridges(root string, report *InitReport) error {
	claudeInstructions := filepath.Join(root, "CLAUDE.md")
	if _, err := os.Lstat(claudeInstructions); errors.Is(err, os.ErrNotExist) {
		if err := core.WriteFileAtomicMode(claudeInstructions, []byte("@AGENTS.md\n"), core.BaseFileMode); err != nil {
			return err
		}
		report.step("CLAUDE.md", "Claude reads the canonical AGENTS.md", true)
	} else if err != nil {
		return fmt.Errorf("inspect CLAUDE.md: %w", err)
	}

	claudeDir := filepath.Join(root, ".claude")
	if info, err := os.Lstat(claudeDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: .claude must be a directory inside the base", core.ErrUnsafePath)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(claudeDir, core.BaseDirMode); err != nil {
			return fmt.Errorf("create .claude: %w", err)
		}
	} else {
		return fmt.Errorf("inspect .claude: %w", err)
	}
	bridge := filepath.Join(claudeDir, "skills")
	if _, err := os.Lstat(bridge); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(filepath.FromSlash("../"+core.BaseSkillsDir), bridge); err != nil {
			return fmt.Errorf("link .claude/skills: %w", err)
		}
		report.step(".claude/skills", "Claude discovers the canonical .agents/skills packages", true)
	} else if err != nil {
		return fmt.Errorf("inspect .claude/skills: %w", err)
	}
	return nil
}

// renderConfig composes a base's fkf.yaml from the shared header and the preset's own source
// block, so every default is visible in the file and the comments live with the sources they
// explain rather than in a Go string.
func renderConfig(name, preset string, demo bool) (string, error) {
	block, err := fs.ReadFile(fkf.Presets, "presets/"+preset+".yaml")
	if err != nil {
		return "", fmt.Errorf("read the embedded preset %s: %w", preset, err)
	}
	if demo {
		block = []byte(demoConfigBlock())
	}
	defaults := core.DefaultSync()
	var builder strings.Builder
	fmt.Fprintf(&builder, `# yaml-language-server: $schema=%s
# %s — this base's definition. Committed. No secrets, ever.
# Sources stay open: put collection helpers in bin/ and source verification hooks in tests/.
fkf: 1 # configuration contract; v1 accepts exactly this marker
name: %s # MCP server name and resource URI authority; informational elsewhere

schema: # shared semantic names; sources only map provider paths to these definitions
  id: {description: Stable record identity., cardinality: one}
  time: {description: Record timestamp when the provider exposes one., cardinality: optional}
  title: {description: Human-readable record label., cardinality: optional}
  modified: {description: Provider modification timestamp used to refresh rebuildable body caches., cardinality: optional}
  category: {description: Authorship role., cardinality: optional}
  visibility: {description: Audience role., cardinality: optional}
  url: {description: Provider page for the record., cardinality: optional, relation: true, examples: [https://example.test/item]}
  repo: {description: Provider owner/name value used by body commands., cardinality: optional}
  repository: {description: Repository associated with the record., cardinality: optional, relation: true, examples: [repo:github.com/owner/name]}
  participant: {description: Person or account involved in the record., cardinality: many, relation: true, examples: [person:email/user@example.test, actor:github.com/login]}
  owner: {description: Person or account that owns the record., cardinality: many, relation: true, examples: [person:email/user@example.test]}
  attachment: {description: Document attached to the record., cardinality: many, relation: true, examples: [document:drive.google.com/file-id]}
  meeting: {description: Calendar event associated with meeting evidence., cardinality: many, relation: true, examples: [events/2026-05-04/google-calendar-events.json#event-id]}
  ticket: {description: Work item associated with the record., cardinality: many, relation: true, examples: [ticket:jira/FKF-1]}
  related: {description: Related base resource or entity., cardinality: many, relation: true, examples: [projects/example.md]}
  supersedes: {description: Older authored knowledge replaced by this page., cardinality: many, relation: true, examples: [wiki/old-decision.md]}

layers: # a disabled layer is not created, listed, served, or scanned
  events: true # what happened, one document per source per day (JSON)
  index: true # what you have, one point-in-time document per source (JSON)
  tasks: true # execution evidence (Markdown)
  projects: true # intent and decisions over weeks (Markdown, status-bearing)
  wiki: true # durable approved knowledge (Markdown, OKF v0.2)

`, core.SchemaURL, core.ConfigFileName, name)
	builder.Write(block)
	fmt.Fprintf(&builder, `
sync:
  days: %d # completed local days to collect when no --date is given; 1..366
  index_max_age_hours: %d # refresh an index document only when it is older
  timeout: %s # per command; a source may override with its own timeout:
  concurrency: %d
`, defaults.Days, defaults.IndexMaxAgeHours, defaults.Timeout, defaults.Concurrency)
	return builder.String(), nil
}

func initGit(ctx context.Context, root string) error {
	if core.NewStore(root, nil).Versioned() {
		return nil
	}
	if _, err := runGit(ctx, root, 30*time.Second, "init", "--quiet"); err != nil {
		return fmt.Errorf("git init in %s: %w", root, err)
	}
	return nil
}

func layerNamesOf(store core.Store) []string {
	names := make([]string, 0, len(core.Layers))
	for _, layer := range store.EnabledLayers() {
		names = append(names, string(layer)+"/")
	}
	return names
}

func nextSteps(root string, report *InitReport) []string {
	rootArgument := shellArg(root)
	configure := fmt.Sprintf("$EDITOR %s  # flip enabled: true on the sources you want", shellArg(filepath.Join(root, core.ConfigFileName)))
	if report.Declared == 0 {
		configure = fmt.Sprintf("$EDITOR %s  # add a source under sources:", shellArg(filepath.Join(root, core.ConfigFileName)))
	}
	trust := []string{}
	if !report.Trusted {
		trust = append(trust, "fkf trust --all --base "+rootArgument+"  # review pre-existing execution inputs")
	}
	if report.Demo != nil {
		return append(trust, []string{
			"fkf status --base " + rootArgument,
			fmt.Sprintf("fkf find --base %s retrieval", rootArgument),
			fmt.Sprintf("fkf context --base %s \"<terms>\" --explain", rootArgument),
			"fkf harness install --all --base " + rootArgument + "  # connect MCP, hooks, and skills",
		}...)
	}
	return append(trust, []string{
		"fkf status --base " + rootArgument + "  # base overview, collector status, and repository health",
		configure,
		"fkf config helpers --refresh --base " + rootArgument + "  # install helpers required by newly enabled sources",
		"fkf trust --all --base " + rootArgument + "  # review the execution plan after editing enabled sources",
		"fkf sync --base " + rootArgument + " --days 7  # collect the last seven completed days",
		"fkf harness install --all --base " + rootArgument + "  # connect MCP, hooks, and skills",
		"fkf schedule install --base " + rootArgument + "  # collect due evidence hourly",
	}...)
}

// Trust prints what a base's enabled sources would run, then records the digest. Reading the
// commands IS the act of trusting, which is why the listing is part of the command rather
// than something the user is told to do first.
type TrustReport struct {
	Base string `json:"base"`
	// Policy is the effective base-level collection policy. These values decide which layers
	// may execute, how many commands a default sync schedules, and their concurrency/timeout.
	Policy TrustedBasePolicy `json:"policy"`
	// Bin is the extra PATH directories the base declares, anywhere on disk. They decide
	// which binary each `run:` word resolves to, so they belong to the same review as the
	// commands themselves.
	Bin      []string        `json:"bin,omitempty"`
	Commands []TrustedSource `json:"commands"`
	// Scripts is the base's own bin/, which is prepended to PATH for every declared command.
	// It is listed because approving `run: git log …` means nothing if a bin/git the reviewer
	// never saw is what actually runs.
	Scripts []core.BinScript `json:"scripts,omitempty"`
	// Tests is the base's own tests/, which is prepended only while source verification hooks run.
	Tests []core.BinScript `json:"tests,omitempty"`
	State core.TrustState  `json:"state"`
	// All asks for the full listing even when a diff is available. It is not carried in JSON
	// because JSON always holds both — the listing AND State.Changes — and only the text
	// rendering has to choose one.
	All      bool `json:"-"`
	Recorded bool `json:"recorded"`
}

// TrustedBasePolicy is the execution-relevant part of global configuration shown by trust.
type TrustedBasePolicy struct {
	Layers           map[core.Layer]bool `json:"layers"`
	Days             int                 `json:"days"`
	IndexMaxAgeHours int                 `json:"index_max_age_hours"`
	Timeout          string              `json:"timeout"`
	Concurrency      int                 `json:"concurrency"`
	WorkingDirectory string              `json:"working_directory"`
	Environment      string              `json:"environment"`
}

// TrustedSource is one declared source's enabled state and executable contract.
type TrustedSource struct {
	Name       string        `json:"name"`
	Enabled    bool          `json:"enabled"`
	Layer      core.Layer    `json:"layer"`
	Auth       []string      `json:"auth,omitempty"`
	Run        []string      `json:"run,omitempty"`
	Test       []string      `json:"test,omitempty"`
	Body       []string      `json:"body,omitempty"`
	BodyFields core.FieldMap `json:"body_fields,omitempty"`
	// Policy is how fkf will invoke the commands above — retries, pacing, timeout. It is part
	// of the disclosure because it changes what approving this line actually authorises, and
	// it appears nowhere in the line itself.
	Policy string `json:"policy,omitempty"`
}

// Trust records the base's configuration digest for this machine.
func Trust(ctx context.Context, base *Base, record, all bool) (*TrustReport, error) {
	report := &TrustReport{
		Base: base.Root(), Bin: base.Config.Bin,
		Policy: TrustedBasePolicy{
			Layers: base.Config.Layers, Days: base.Config.Sync.Days,
			IndexMaxAgeHours: base.Config.Sync.IndexMaxAgeHours,
			Timeout:          base.Config.Sync.Timeout.String(), Concurrency: base.Config.Sync.Concurrency,
			WorkingDirectory: core.DeclaredCommandDirectory,
			Environment:      core.DeclaredCommandEnvironmentPolicy,
		},
		Commands: []TrustedSource{}, All: all,
	}
	for _, name := range base.Config.SourceNames() {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		source := base.Config.Sources[name]
		bodyFields := make(core.FieldMap, len(source.BodyFieldNames()))
		for _, fieldName := range source.BodyFieldNames() {
			bodyFields[fieldName] = append(core.FieldPaths(nil), source.Fields.Paths(fieldName)...)
		}
		report.Commands = append(report.Commands, TrustedSource{
			Name: source.Name, Enabled: source.Enabled, Layer: source.Layer,
			Auth: source.Auth, Run: source.Run, Test: source.Test, Body: source.Body,
			BodyFields: bodyFields,
			// How fkf will invoke the line above. A review that says what runs but not how
			// many times, how often, or for how long is not the whole disclosure.
			Policy: sources.DescribePolicy(source),
		})
	}
	scripts, err := core.BinScripts(ctx, base.Root())
	if err != nil {
		return nil, err
	}
	report.Scripts = scripts
	tests, err := core.TestScripts(ctx, base.Root())
	if err != nil {
		return nil, err
	}
	report.Tests = tests
	if !record {
		state, err := core.ReadTrust(ctx, base.Config)
		if err != nil {
			return nil, err
		}
		report.State = state
		return report, nil
	}
	state, err := core.WriteTrust(ctx, base.Config, base.Now())
	if err != nil {
		return nil, err
	}
	report.State, report.Recorded = state, true
	return report, nil
}
