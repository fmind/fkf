package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A base is one directory: a git working tree holding five typed layers. Every path an
// agent, a command, or a URI names resolves through a Store, so no caller ever joins a
// layer name onto a root of its own — that is what keeps a base relocatable as one unit
// and what makes confinement a single enforced rule rather than a convention.

// Layer is one typed storage layer of a base.
type Layer string

const (
	// LayerEvents holds one collected document per source per completed local day.
	LayerEvents Layer = "events"
	// LayerIndex holds one point-in-time document per source: the things you have, as
	// opposed to the things that happened. It was called `index` while it also held the
	// derived caches, which made one word mean a layer, a source kind, two rebuildable files,
	// a wiki command, and a wiki page at once.
	LayerIndex Layer = "index"
	// LayerTasks holds task execution traces.
	LayerTasks Layer = "tasks"
	// LayerProjects holds status-bearing intent pages.
	LayerProjects Layer = "projects"
	// LayerWiki holds the flat OKF v0.2 knowledge bundle.
	LayerWiki Layer = "wiki"
)

// Layers is the canonical ordered layer list, consumed by path resolution, scaffolding,
// configuration validation, and every `list` command.
var Layers = []Layer{LayerEvents, LayerIndex, LayerTasks, LayerProjects, LayerWiki}

// ParseLayer converts a user-supplied name into a known layer.
func ParseLayer(value string) (Layer, error) {
	candidate := Layer(strings.ToLower(strings.TrimSpace(value)))
	for _, layer := range Layers {
		if candidate == layer {
			return layer, nil
		}
	}
	return "", fmt.Errorf("unknown layer %q; valid layers: %s", value, LayerNames())
}

// LayerNames renders the canonical layer list for a diagnostic.
func LayerNames() string {
	names := make([]string, 0, len(Layers))
	for _, layer := range Layers {
		names = append(names, string(layer))
	}
	return strings.Join(names, ", ")
}

// Base permissions. A base is created owner-only whether or not it is versioned: git stores
// no directory mode and tracks only the executable bit, so versioning is never a reason to
// relax them. What versioning changes is that fkf stops repairing modes inside a tree that
// git, an editor, and a teammate's clone also own.
const (
	BaseDirMode  os.FileMode = 0o700
	BaseFileMode os.FileMode = 0o600
)

const (
	baseDirMode  = BaseDirMode
	baseFileMode = BaseFileMode
)

// Generated graph filenames live at the base root. They remain outside every typed layer so
// listings cannot confuse rebuildable cache data with collected or authored content.
const (
	GraphFile         = "graph.tsv"
	GraphMetaFile     = "graph.meta.json"
	TaskTraceFile     = "TASKS.md"
	BaseAgentsFile    = "AGENTS.md"
	BaseSkillsDir     = ".agents/skills"
	BaseBinDir        = "bin"
	ConfigFileName    = "fkf.yaml"
	LocalConfigName   = "fkf.local.yaml"
	MarkdownExtension = ".md"
)

// ErrNotAddressable reports a base-relative path outside the published URI grammar. The CLI
// maps it to exit code 2: naming a file a base does not address is a usage error, not a
// failure.
var ErrNotAddressable = errors.New("path is not addressable in a base")

// pageSlugPattern is the path-level half of the Markdown contract. Content validation owns
// frontmatter and links, but Store.Resolve must reject hidden files, backups, and nested pages
// before a generic read (including MCP) can open them.
var pageSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ErrLayerDisabled reports a request for a layer this base does not enable. It is a distinct
// error because "you turned it off" and "it is empty" are different answers, and a command
// that conflates them teaches the user to ignore both.
type ErrLayerDisabled struct{ Layer Layer }

func (e ErrLayerDisabled) Error() string {
	return fmt.Sprintf("layer %s is disabled in %s; set layers.%s: true to enable it",
		e.Layer, ConfigFileName, e.Layer)
}

// Store is the resolved, immutable layout of one base.
type Store struct {
	root    string
	enabled map[Layer]bool
}

// NewStore resolves a store from a root and the layer activation the base declares. An
// absent entry is disabled, so a hand-written configuration cannot silently enable a layer.
func NewStore(root string, enabled map[Layer]bool) Store {
	store := Store{
		root:    filepath.Clean(ExpandHome(root)),
		enabled: make(map[Layer]bool, len(Layers)),
	}
	for _, layer := range Layers {
		store.enabled[layer] = enabled[layer]
	}
	return store
}

// Root returns the base directory.
func (s Store) Root() string { return s.root }

// Enabled reports whether a layer is activated.
func (s Store) Enabled(layer Layer) bool { return s.enabled[layer] }

// EnabledLayers returns the activated layers in canonical order.
func (s Store) EnabledLayers() []Layer {
	active := make([]Layer, 0, len(Layers))
	for _, layer := range Layers {
		if s.enabled[layer] {
			active = append(active, layer)
		}
	}
	return active
}

// Dir returns the resolved directory of an enabled layer.
func (s Store) Dir(layer Layer) (string, error) {
	// Resolve applies both activation and the same below-root symlink confinement as a file
	// read. Directory enumeration is still a read: following a committed layer-root symlink
	// would otherwise expose filenames and shape outside the base before a later file open failed.
	return s.Resolve(string(layer))
}

// LayerOf returns the layer a base-relative path belongs to, if any. It is how a URI is
// checked against layer activation without every caller re-deriving the first segment.
func (s Store) LayerOf(relative string) (Layer, bool) {
	first, _, _ := strings.Cut(strings.TrimPrefix(relative, "/"), "/")
	for _, layer := range Layers {
		if first == string(layer) {
			return layer, true
		}
	}
	return "", false
}

// Resolve maps a base-relative slash path to an absolute path inside the base. Every read and
// write in fkf goes through it, so it is where the three ways out of a base are refused: a
// path that escapes lexically, a path addressing a disabled layer, and a path outside the
// addressable set.
//
// The addressable set is the published URI grammar and nothing else. Confining to the root
// was not enough on its own: a base IS a git repository, so `.git/config` always exists beside
// the layers, and a user wiring up a source drops a `.env` there — both were readable through
// `fkf read`, and therefore through the ungated MCP `read` tool. `fkf.local.yaml` stays out
// deliberately; it is the machine-local overlay and no URI names it.
func (s Store) Resolve(relative string) (string, error) {
	cleaned, err := CleanRelative(relative)
	if err != nil {
		return "", err
	}
	cleaned = strings.TrimSuffix(cleaned, "/")
	if cleaned == "." {
		return s.root, nil
	}
	switch layer, isLayer := s.LayerOf(cleaned); {
	case isLayer && !s.enabled[layer]:
		return "", ErrLayerDisabled{Layer: layer}
	case !addressableBasePath(cleaned):
		return "", notAddressable(cleaned)
	}
	absolute := filepath.Join(s.root, filepath.FromSlash(cleaned))
	if err := ValidateWithinRoot(s.root, absolute); err != nil {
		return "", err
	}
	return absolute, nil
}

// addressableBasePath is the shared lexical half of Store.Resolve. Collection validates
// relation paths before it has a Base, so keeping the closed grammar here prevents evidence
// that writes successfully but makes the graph unreadable on its next rebuild.
func addressableBasePath(relative string) bool {
	cleaned := strings.TrimSuffix(relative, "/")
	first, _, _ := strings.Cut(cleaned, "/")
	for _, layer := range Layers {
		if first == string(layer) {
			return addressableLayerPath(layer, cleaned)
		}
	}
	// The root graph files are addressable but are not a layer: there is nothing to enable or
	// browse. Both the rows and their integrity metadata resolve through the Store internally.
	return cleaned == GraphFile ||
		cleaned == GraphMetaFile ||
		cleaned == ConfigFileName ||
		cleaned == BaseAgentsFile
}

type relationFileCapabilities struct {
	Fragment bool
	JQ       bool
}

// relationFilePath is the file-node half of the public relation grammar. Store.Resolve also
// admits directories because listings need them; an edge cannot point at a listing. The two
// optional URI suffixes are restricted to the file kinds whose read path implements them.
func relationFilePath(relative string) (relationFileCapabilities, bool) {
	parts := strings.Split(relative, "/")
	switch {
	case relative == ConfigFileName, relative == GraphFile:
		return relationFileCapabilities{}, true
	case relative == BaseAgentsFile:
		return relationFileCapabilities{Fragment: true}, true
	case relative == GraphMetaFile:
		return relationFileCapabilities{JQ: true}, true
	case len(parts) == 3 && parts[0] == string(LayerEvents) && validAddressDate(parts[1]):
		if addressableSourceDocument(parts[2]) {
			return relationFileCapabilities{Fragment: true, JQ: true}, true
		}
	case len(parts) == 2 && parts[0] == string(LayerIndex) && addressableSourceDocument(parts[1]):
		return relationFileCapabilities{Fragment: true, JQ: true}, true
	case len(parts) == 4 && parts[0] == string(LayerTasks) && validAddressDate(parts[1]) &&
		pageSlugPattern.MatchString(parts[2]) && parts[3] == TaskTraceFile:
		return relationFileCapabilities{Fragment: true}, true
	case len(parts) == 2 && (parts[0] == string(LayerProjects) || parts[0] == string(LayerWiki)):
		slug, found := strings.CutSuffix(parts[1], MarkdownExtension)
		if found && pageSlugPattern.MatchString(slug) {
			return relationFileCapabilities{Fragment: true}, true
		}
	}
	return relationFileCapabilities{}, false
}

func notAddressable(relative string) error {
	return fmt.Errorf("%w: %s (a base addresses the published shapes under the %s layers, plus %s, %s, %s and %s)",
		ErrNotAddressable, relative, LayerNames(), GraphFile, GraphMetaFile, ConfigFileName, BaseAgentsFile)
}

// addressableLayerPath is deliberately a closed grammar. A layer directory may contain
// malformed or private neighbour files because a base is an ordinary git repository; being
// below an enabled layer never grants those files a URI.
func addressableLayerPath(layer Layer, relative string) bool {
	parts := strings.Split(relative, "/")
	if len(parts) == 1 {
		return true
	}
	switch layer {
	case LayerEvents:
		if len(parts) == 2 {
			return validAddressDate(parts[1])
		}
		if len(parts) != 3 || !validAddressDate(parts[1]) {
			return false
		}
		return addressableSourceDocument(parts[2])
	case LayerIndex:
		return len(parts) == 2 && addressableSourceDocument(parts[1])
	case LayerTasks:
		if len(parts) == 2 {
			return validAddressDate(parts[1])
		}
		if len(parts) == 3 {
			return validAddressDate(parts[1]) && pageSlugPattern.MatchString(parts[2])
		}
		return len(parts) == 4 && validAddressDate(parts[1]) &&
			pageSlugPattern.MatchString(parts[2]) && parts[3] == TaskTraceFile
	case LayerProjects, LayerWiki:
		if len(parts) != 2 {
			return false
		}
		slug, found := strings.CutSuffix(parts[1], MarkdownExtension)
		return found && pageSlugPattern.MatchString(slug)
	default:
		return false
	}
}

func validAddressDate(value string) bool {
	return value != "" && ValidateDate(value) == nil
}

func addressableSourceDocument(name string) bool {
	source, found := strings.CutSuffix(name, ".json")
	return found && sourceNamePattern.MatchString(source)
}

// Relative maps an absolute path inside the base back to its base-relative slash form. It is
// the inverse of Resolve and the only place a URI is minted from a filesystem walk.
func (s Store) Relative(absolute string) (string, error) {
	rel, err := filepath.Rel(s.root, absolute)
	if err != nil {
		return "", fmt.Errorf("%w: %s is outside %s", ErrPathEscapes, absolute, s.root)
	}
	slashed := filepath.ToSlash(rel)
	if slashed == ".." || strings.HasPrefix(slashed, "../") {
		return "", fmt.Errorf("%w: %s is outside %s", ErrPathEscapes, absolute, s.root)
	}
	return slashed, nil
}

// ConfigPath is the committed configuration file of this base.
func (s Store) ConfigPath() string { return filepath.Join(s.root, ConfigFileName) }

// LocalConfigPath is the gitignored machine-local overlay of this base.
func (s Store) LocalConfigPath() string { return filepath.Join(s.root, LocalConfigName) }

// configStore preserves the absolute root spelling the operator chose, including a root that
// is itself a symlink. That spelling is part of trust identity; only entries beneath it are
// confined and forbidden from being links.
func configStore(root string) (Store, error) {
	absolute, err := ResolveAbsolutePath(root)
	if err != nil {
		return Store{}, err
	}
	return Store{root: absolute}, nil
}

// readConfigLeaf is the one read boundary for both execution-defining YAML files. Existing
// leaves must be regular base-owned files: a symlink moves reviewed commands outside the base,
// while a FIFO or device can block or produce different bytes on each read. ValidateWithinRoot
// deliberately starts below store.root, so a symlink-spelled base root remains supported.
func readConfigLeaf(store Store, name string) ([]byte, bool, error) {
	if name != ConfigFileName && name != LocalConfigName {
		return nil, false, fmt.Errorf("%w: %s is not a configuration leaf", ErrUnsafePath, name)
	}
	path := filepath.Join(store.root, name)
	if err := ValidateWithinRoot(store.root, path); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect %s: %w", ErrUnsafePath, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %s must be a regular non-symlink file", ErrUnsafePath, path)
	}
	data, err := ReadFileLimit(path, MaxConfigBytes)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// BinDir is the base's own script directory, prepended to PATH for every declared command.
func (s Store) BinDir() string { return filepath.Join(s.root, BaseBinDir) }

// Versioned reports whether the base has recognizable, real git working-tree metadata.
// Detecting this beats declaring it: a configuration key could claim a base is versioned when
// it is not, and the permission contract would then be wrong in the one direction that matters.
func (s Store) Versioned() bool {
	marker := filepath.Join(s.root, ".git")
	info, err := os.Lstat(marker)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if info.IsDir() {
		return hasGitHead(marker)
	}
	if !info.Mode().IsRegular() {
		return false
	}
	data, err := ReadFileLimit(marker, MaxControlFileBytes)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	gitDir, found := strings.CutPrefix(line, "gitdir:")
	if !found || strings.ContainsAny(gitDir, "\r\n") {
		return false
	}
	gitDir = strings.TrimSpace(gitDir)
	if gitDir == "" {
		return false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(s.root, gitDir)
	}
	return hasGitHead(filepath.Clean(gitDir))
}

func hasGitHead(gitDir string) bool {
	directory, err := os.Lstat(gitDir)
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return false
	}
	head, err := os.Lstat(filepath.Join(gitDir, "HEAD"))
	return err == nil && head.Mode().IsRegular() && head.Mode()&os.ModeSymlink == 0
}

// EnforcePermissions reports whether a permission audit may repair modes. A versioned base
// is only inspected.
func (s Store) EnforcePermissions() bool { return !s.Versioned() }
