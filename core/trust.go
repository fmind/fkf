package core

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A base's configuration holds executable argv and helper scripts, so a base you did not create on this machine
// is untrusted until you say otherwise — the model `mise trust` and `direnv allow` use. The
// digest covers the resolved execution plan and the base's bin/ together. Comments, YAML key
// order, semantic descriptions, and retrieval-only paths do not re-arm execution trust.
//
// Trust state lives outside the base on purpose. Recording it inside would make it clonable,
// which is precisely the thing the gate exists to prevent, and machine-local state has no
// business in a repository the owner may push.

// ErrUntrusted reports a base whose configuration has not been trusted on this machine, or
// whose configuration changed since it was. The CLI maps it to exit code 3.
var ErrUntrusted = errors.New("base is not trusted on this machine")

// TrustRecord is what is stored per base. The aggregate digest is the gate; Items is what
// makes a re-trust reviewable.
//
// One hash answers "did anything change" and nothing else, so the second time trust is asked
// for — after a `git pull` on a shared base, which is the moment the gate exists for — the
// only honest thing fkf could say was "the digest changed", and re-approval meant re-reading
// every source and every script to find the one line that moved. A review nobody re-reads is
// a review nobody performs.
//
// Items is machine-local like the rest of the record, so a record written by an older build
// simply has none and the listing falls back to printing everything: the safe default, and no
// migration to write.
type TrustRecord struct {
	Base      string      `json:"base"`
	Digest    string      `json:"digest"`
	TrustedAt string      `json:"trusted_at"`
	Items     []TrustItem `json:"items,omitempty"`
}

// TrustItemKind names what a trusted item is, so a diff can say "source" or "script" rather
// than showing a path and leaving the reader to infer which review it belonged to.
type TrustItemKind string

const (
	// TrustItemConfig is the resolved base-wide execution policy.
	TrustItemConfig TrustItemKind = "config"
	// TrustItemSource is one declared source's enabled state, commands, body fields, and policy.
	TrustItemSource TrustItemKind = "source"
	// TrustItemScript is one entry under <base>/bin.
	TrustItemScript TrustItemKind = "script"
)

// TrustItem is one reviewable unit of what a base can execute, reduced to a digest. Detail
// carries the one property that is worth naming in a diff on its own — a script's executable
// bit, because a mode-only pull is what arms a shadow binary and its content digest does not
// move when the mode does.
type TrustItem struct {
	Kind       TrustItemKind `json:"kind"`
	Name       string        `json:"name"`
	Digest     string        `json:"digest"`
	Executable bool          `json:"executable,omitempty"`
}

// TrustState is the answer to "may fkf run this base's commands", with enough detail for a
// diagnostic to say what changed.
type TrustState struct {
	Base    string `json:"base"`
	Trusted bool   `json:"trusted"`
	Digest  string `json:"digest"`
	Stored  string `json:"stored_digest,omitempty"`
	Since   string `json:"trusted_at,omitempty"`
	Path    string `json:"record,omitempty"`
	// Items is what this base holds right now, and Changes is how it differs from what was
	// trusted. Changes is empty when the base is trusted, when it has never been trusted, and
	// when the stored record predates per-item digests — three states a reader tells apart
	// from Trusted and Stored, and all three of which mean "print the whole listing".
	Items   []TrustItem   `json:"items,omitempty"`
	Changes []TrustChange `json:"changes,omitempty"`
}

// TrustChangeKind is what happened to one item since it was trusted.
type TrustChangeKind string

const (
	TrustAdded    TrustChangeKind = "added"
	TrustRemoved  TrustChangeKind = "removed"
	TrustModified TrustChangeKind = "modified"
	// TrustArmed is a script whose contents did not change but which gained the executable
	// bit. It is its own kind because it is the change a reviewer is most likely to wave
	// through as cosmetic and the one that actually decides whether PATH lookup runs it.
	TrustArmed TrustChangeKind = "armed"
	// TrustDisarmed is the same edit in reverse.
	TrustDisarmed TrustChangeKind = "disarmed"
)

// TrustChange is one line of the re-trust review.
type TrustChange struct {
	Kind TrustChangeKind `json:"kind"`
	Item TrustItemKind   `json:"item"`
	Name string          `json:"name"`
}

// DiffTrustItems reports how the base's current items differ from what was trusted. Both sides
// are sorted by (kind, name) already, so the result is deterministic.
func DiffTrustItems(stored, current []TrustItem) []TrustChange {
	type itemKey struct {
		kind TrustItemKind
		name string
	}
	was := make(map[itemKey]TrustItem, len(stored))
	for _, item := range stored {
		was[itemKey{kind: item.Kind, name: item.Name}] = item
	}
	seen := make(map[itemKey]bool, len(current))
	changes := make([]TrustChange, 0)
	for _, item := range current {
		key := itemKey{kind: item.Kind, name: item.Name}
		seen[key] = true
		previous, existed := was[key]
		switch {
		case !existed:
			changes = append(changes, TrustChange{Kind: TrustAdded, Item: item.Kind, Name: item.Name})
		case previous.Digest != item.Digest:
			changes = append(changes, TrustChange{Kind: TrustModified, Item: item.Kind, Name: item.Name})
		case previous.Executable != item.Executable:
			kind := TrustDisarmed
			if item.Executable {
				kind = TrustArmed
			}
			changes = append(changes, TrustChange{Kind: kind, Item: item.Kind, Name: item.Name})
		}
	}
	for _, item := range stored {
		if !seen[itemKey{kind: item.Kind, name: item.Name}] {
			changes = append(changes, TrustChange{Kind: TrustRemoved, Item: item.Kind, Name: item.Name})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Item != changes[j].Item {
			return changes[i].Item < changes[j].Item
		}
		return changes[i].Name < changes[j].Name
	})
	return changes
}

// framedDigest makes variable-length execution definitions unambiguous. Delimiters are not
// sufficient here: YAML can represent every byte, including NUL, so only explicit lengths
// keep two different variable-length execution sequences from sharing a trust digest.
type framedDigest struct {
	hash hash.Hash
}

func newFramedDigest(domain string) *framedDigest {
	digest := &framedDigest{hash: sha256.New()}
	digest.value("fkf-framed-trust-v1")
	digest.value(domain)
	return digest
}

func (d *framedDigest) value(value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = d.hash.Write(size[:])
	_, _ = d.hash.Write([]byte(value))
}

func (d *framedDigest) field(name, value string) {
	d.value(name)
	d.value(value)
}

func (d *framedDigest) boolean(name string, value bool) {
	d.field(name, strconv.FormatBool(value))
}

func (d *framedDigest) integer(name string, value int64) {
	d.field(name, strconv.FormatInt(value, 10))
}

func (d *framedDigest) sum() string {
	return hex.EncodeToString(d.hash.Sum(nil))
}

// StateDir is where machine-local fkf state lives. It follows XDG so a test can redirect it
// with one variable, which is what keeps the suite hermetic.
func StateDir() string {
	if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); state != "" {
		return filepath.Join(ExpandHome(state), "fkf")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "fkf")
	}
	return filepath.Join(home, ".local", "state", "fkf")
}

// trustRecordPath names one base's record by the hash of its absolute path. Hashing rather
// than escaping keeps the filename bounded and free of separators, and the record repeats
// the path so the directory stays readable.
func trustRecordPath(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	sum := sha256.Sum256([]byte(absolute))
	return filepath.Join(StateDir(), "trust", hex.EncodeToString(sum[:])+".json")
}

// ConfigDigest reduces the canonical execution plan to one hash. The plan is loaded and merged
// first, so comments, key ordering, and retrieval-only semantics cannot re-arm trust while a
// local overlay that changes what runs still does.
//
// bin/ belongs in the plan because it is committed with the base and sits first on the PATH
// every declared command gets. Without it, a base whose `run: git log …` had been read and
// approved could ship or later receive a bin/git, and `fkf sync` would run it with no prompt:
// the digest would still match, because only the YAML had been hashed. For a shared team base
// that is a `git pull` away from code execution on every teammate's machine.
func ConfigDigest(ctx context.Context, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, err := LoadConfig(root)
	if err != nil {
		return "", err
	}
	return configDigest(ctx, config)
}

func configDigest(ctx context.Context, config *Config) (string, error) {
	items, err := trustItems(ctx, config)
	if err != nil {
		return "", err
	}
	return digestTrustItems(ctx, items)
}

func digestTrustItems(ctx context.Context, items []TrustItem) (string, error) {
	digest := newFramedDigest("execution-plan-v2")
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		digest.field("item-kind", string(item.Kind))
		digest.field("item-name", item.Name)
		digest.field("item-digest", item.Digest)
		digest.boolean("item-executable", item.Executable)
	}
	return digest.sum(), nil
}

// BinScript is one accepted entry of a base's bin/, reduced to what a reviewer has to agree
// to: its name, kind, executable state, and content digest when it is a regular file.
type BinScript struct {
	// Name is the entry's path relative to bin/, so a helper under bin/lib/ is named
	// "lib/impl.sh" and stays distinct from a sibling of the same base name.
	Name string `json:"name"`
	// Kind is "script" for a regular file or the filesystem mode type for another accepted
	// entry. Symlinks are refused before a BinScript can be returned.
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	// Target is empty for every accepted entry; links are rejected as unsafe.
	Target string `json:"target,omitempty"`
	// Executable reports the bit that decides whether PATH lookup will pick this entry up.
	Executable bool `json:"executable"`
}

// BinScripts lists a base's bin/ in name order, walking it to the bottom. An absent or empty
// bin/ is not an error: most bases have none, and "nothing to review" is a valid answer for
// the trust listing.
//
// The walk is recursive because a one-level listing left `bin/lib/impl.sh` outside the digest
// entirely: a reviewer approves `bin/helper` once, and every later edit to the file it sources
// is trusted silently. Every entry kind is recorded, so a directory, FIFO, or device under
// bin/ contributes to the hash instead of vanishing from it.
func BinScripts(ctx context.Context, root string) ([]BinScript, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Join(filepath.Clean(ExpandHome(root)), BaseBinDir)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", directory, err)
	}
	// A symlinked bin/ is refused rather than walked. WalkDir stops at a symlinked root and the
	// loop below skips the root entry itself, so every script behind the link stayed out of the
	// digest AND out of the `fkf trust` listing, while the directory it points at was still
	// first on the PATH of every declared command: a shadow `git` there ran, its output was
	// stored as a record, and swapping its contents afterwards left the digest byte-identical.
	//
	// Recording the link and hashing its target is not enough — the target lives outside the
	// base, so nothing bounds what it becomes next. Refusing is the same rule the store already
	// applies to every addressable path: a base addresses only its own files.
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s/ is a symlink; a base runs only its own scripts, and a link "+
			"leaves what fkf trusted outside the base", ErrUnsafePath, BaseBinDir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s/ is not a directory", ErrUnsafePath, BaseBinDir)
	}
	var scripts []BinScript
	// WalkDir uses lstat semantics, so links are visible without ever following them.
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("inspect %s: %w", path, walkErr)
		}
		if path == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		name := filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symlink; a base runs only files whose contents are "+
				"inside the trusted bin tree", ErrUnsafePath, filepath.ToSlash(filepath.Join(BaseBinDir, relative)))
		case info.Mode().IsRegular():
			data, err := ReadFileLimit(path, MaxControlFileBytes)
			if err != nil {
				// Deliberately not skipped. A script too large to hash is a script that
				// cannot be reviewed, and trusting it unread is the failure this gate exists
				// to prevent.
				return fmt.Errorf("read the base script %s: %w", path, err)
			}
			sum := sha256.Sum256(data)
			scripts = append(scripts, BinScript{
				Name: name, Kind: "script",
				Digest: hex.EncodeToString(sum[:]), Executable: info.Mode().Perm()&0o111 != 0,
			})
		default:
			// A directory, FIFO, socket, or device. It runs nothing itself, but its presence
			// and name belong in the digest so the tree cannot change unnoticed.
			scripts = append(scripts, BinScript{Name: name, Kind: info.Mode().Type().String()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	return scripts, nil
}

// TrustItems reduces everything a base can execute to reviewable canonical units: one
// base-wide policy, every declared source, and every entry under bin/. Invalid configuration
// cannot be trusted because there is no execution plan to review.
func TrustItems(ctx context.Context, root string) ([]TrustItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := LoadConfig(root)
	if err != nil {
		return nil, err
	}
	return trustItems(ctx, config)
}

func trustItems(ctx context.Context, config *Config) ([]TrustItem, error) {
	items := make([]TrustItem, 0, 8)
	items = append(items, TrustItem{Kind: TrustItemConfig, Name: "base", Digest: baseExecutionDigest(config)})
	for _, name := range config.SourceNames() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := config.Sources[name]
		items = append(items, TrustItem{
			Kind: TrustItemSource, Name: source.Name, Digest: sourceDigest(source),
		})
	}
	scripts, err := BinScripts(ctx, config.Store().Root())
	if err != nil {
		return nil, err
	}
	for _, script := range scripts {
		items = append(items, TrustItem{
			Kind: TrustItemScript, Name: script.Name,
			Digest: scriptTrustDigest(script), Executable: script.Executable,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

func scriptTrustDigest(script BinScript) string {
	digest := newFramedDigest("script-v2")
	digest.field("kind", script.Kind)
	digest.field("content-digest", script.Digest)
	return digest.sum()
}

func baseExecutionDigest(config *Config) string {
	digest := newFramedDigest("base-execution-v2")
	digest.field("command-directory", DeclaredCommandDirectory)
	digest.field("command-environment", DeclaredCommandEnvironmentPolicy)
	for _, layer := range Layers {
		digest.field("layer-name", string(layer))
		digest.boolean("layer-enabled", config.Layers[layer])
	}
	digest.integer("sync-days", int64(config.Sync.Days))
	digest.integer("index-max-age-hours", int64(config.Sync.IndexMaxAgeHours))
	digest.integer("timeout", int64(config.Sync.Timeout))
	digest.integer("concurrency", int64(config.Sync.Concurrency))
	for _, directory := range config.Bin {
		digest.field("bin", directory)
	}
	return digest.sum()
}

// sourceDigest covers exactly what `fkf trust` prints for a source: its commands, body-bound
// field paths, and invocation policy. Retrieval-only projections and semantic
// descriptions remain outside execution trust.
func sourceDigest(source *Source) string {
	digest := newFramedDigest("source-execution-v2")
	digest.boolean("enabled", source.Enabled)
	digest.field("layer", string(source.Layer))
	for _, argument := range source.Run {
		digest.field("run", argument)
	}
	for _, argument := range source.Body {
		digest.field("body", argument)
	}
	for _, name := range source.BodyFieldNames() {
		for _, fieldPath := range source.Fields.Paths(name) {
			digest.field("body-field-name", name)
			digest.field("body-field-path", fieldPath.String())
		}
	}
	digest.integer("timeout", int64(source.Timeout))
	digest.integer("retry-attempts", int64(source.Retry.Attempts))
	digest.integer("retry-backoff", int64(source.Retry.Backoff))
	digest.integer("min-interval", int64(source.MinInterval))
	digest.boolean("window", source.Window)
	for _, condition := range source.Retry.On {
		digest.field("retry-on", condition)
	}
	return digest.sum()
}

// ReadTrust reports whether this machine has trusted the base's current configuration.
func ReadTrust(ctx context.Context, root string) (TrustState, error) {
	if err := ctx.Err(); err != nil {
		return TrustState{}, err
	}
	config, err := LoadConfig(root)
	if err != nil {
		return TrustState{}, err
	}
	return ReadTrustConfig(ctx, config)
}

// ReadTrustConfig reports trust for the exact decoded execution plan a caller will use.
// Binding the check to this snapshot prevents a later disk reload from approving one plan
// while a long-lived Base executes another plan it opened earlier.
func ReadTrustConfig(ctx context.Context, config *Config) (TrustState, error) {
	if err := ctx.Err(); err != nil {
		return TrustState{}, err
	}
	root := config.Store().Root()
	items, err := trustItems(ctx, config)
	if err != nil {
		return TrustState{}, err
	}
	digest, err := digestTrustItems(ctx, items)
	if err != nil {
		return TrustState{}, err
	}
	state := TrustState{Base: root, Digest: digest, Items: items, Path: trustRecordPath(root)}
	data, err := ReadFileLimit(state.Path, MaxControlFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	var record TrustRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return state, fmt.Errorf("decode trust record %s: %w", state.Path, err)
	}
	state.Stored, state.Since = record.Digest, record.TrustedAt
	state.Trusted = record.Digest == digest
	// The diff is only computed when it can be honest: the base changed, and the stored record
	// is new enough to say what it held. Otherwise Changes stays empty and the caller prints
	// the whole listing, which is the safe default and the reason no migration is needed.
	if !state.Trusted && len(record.Items) > 0 {
		state.Changes = DiffTrustItems(record.Items, state.Items)
	}
	return state, nil
}

// WriteTrust records the base's current configuration digest for this machine.
func WriteTrust(ctx context.Context, root string, now time.Time) (TrustState, error) {
	config, err := LoadConfig(root)
	if err != nil {
		return TrustState{}, err
	}
	return WriteTrustConfig(ctx, config, now)
}

// WriteTrustConfig records the exact decoded execution plan shown to and approved by a caller.
func WriteTrustConfig(ctx context.Context, config *Config, now time.Time) (TrustState, error) {
	state, err := ReadTrustConfig(ctx, config)
	if err != nil {
		return TrustState{}, err
	}
	record := TrustRecord{
		Base: state.Base, Digest: state.Digest,
		TrustedAt: now.UTC().Format(time.RFC3339), Items: state.Items,
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return TrustState{}, err
	}
	if err := ctx.Err(); err != nil {
		return TrustState{}, err
	}
	if err := os.MkdirAll(filepath.Dir(state.Path), BaseDirMode); err != nil {
		return TrustState{}, fmt.Errorf("create trust directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return TrustState{}, err
	}
	if err := WriteFileAtomicMode(state.Path, append(encoded, '\n'), BaseFileMode); err != nil {
		return TrustState{}, err
	}
	state.Trusted, state.Stored, state.Since = true, record.Digest, record.TrustedAt
	return state, nil
}

// RequireTrust is the gate every command that executes a declared command calls first. It
// names the remedy, because a refusal the user cannot act on is just an outage.
func RequireTrust(ctx context.Context, root string) error {
	config, err := LoadConfig(root)
	if err != nil {
		return err
	}
	return RequireTrustConfig(ctx, config)
}

// RequireTrustConfig gates the exact decoded execution plan a caller is about to execute.
func RequireTrustConfig(ctx context.Context, config *Config) error {
	state, err := ReadTrustConfig(ctx, config)
	if err != nil {
		return err
	}
	if state.Trusted {
		return nil
	}
	if state.Stored == "" {
		return fmt.Errorf("%w: %s has never been trusted here; run `fkf trust --base %s` to read its commands and record them",
			ErrUntrusted, state.Base, state.Base)
	}
	return fmt.Errorf("%w: the configuration of %s changed since it was trusted on %s; review the change and run `fkf trust --base %s`",
		ErrUntrusted, state.Base, state.Since, state.Base)
}
