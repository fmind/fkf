package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/fmind/fkf/core"
)

const learnProposalRelative = ".agents/tmp/learn"

var learnProposalIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// LearnCandidate is one unharvested task lesson a deterministic log proposal can cite.
type LearnCandidate struct {
	Trace  string `json:"trace"`
	Text   string `json:"text"`
	Target string `json:"target"`
}

// LearnProposal describes one reviewable unified diff. Diff is populated only by review --diff.
type LearnProposal struct {
	ID    string   `json:"id"`
	Path  string   `json:"path"`
	Bytes int      `json:"bytes"`
	Files []string `json:"files"`
	Diff  string   `json:"diff,omitempty"`
}

// LearnProposalReport is returned by propose, including its non-writing dry-run candidate list.
type LearnProposalReport struct {
	DryRun     bool             `json:"dry_run"`
	Candidates []LearnCandidate `json:"candidates"`
	Proposal   *LearnProposal   `json:"proposal,omitempty"`
	Existing   bool             `json:"existing,omitempty"`
	Nothing    bool             `json:"nothing_to_propose,omitempty"`
}

// LearnReview is either the active queue or one proposal with its requested diff.
type LearnReview struct {
	Proposals []LearnProposal `json:"proposals"`
}

// LearnActionReport records an apply or reject transition into the ignored archive.
type LearnActionReport struct {
	ID          string              `json:"id"`
	Status      string              `json:"status"`
	Path        string              `json:"path"`
	Files       []string            `json:"files,omitempty"`
	Validations []*ValidationReport `json:"validations,omitempty"`
	Build       *BuildReport        `json:"build,omitempty"`
}

// ProposeLearn creates one deterministic wiki/log.md diff from the current unharvested backlog.
// Dry-run performs the same scan and reports the candidate bullets without touching .agents/tmp.
func ProposeLearn(ctx context.Context, base *Base, dryRun bool) (*LearnProposalReport, error) {
	if err := base.RequireLayer(core.LayerTasks); err != nil {
		return nil, err
	}
	if err := base.RequireLayer(core.LayerWiki); err != nil {
		return nil, err
	}
	listing, err := ListLearned(ctx, base, Window{}, true)
	if err != nil {
		return nil, err
	}
	candidates := make([]LearnCandidate, 0, len(listing.Bullets))
	for _, bullet := range listing.Bullets {
		candidates = append(candidates, LearnCandidate{Trace: bullet.Trace, Text: bullet.Text, Target: "wiki/log.md"})
	}
	slices.SortFunc(candidates, func(left, right LearnCandidate) int {
		if compared := strings.Compare(left.Trace, right.Trace); compared != 0 {
			return compared
		}
		return strings.Compare(left.Text, right.Text)
	})
	report := &LearnProposalReport{DryRun: dryRun, Candidates: candidates}
	if len(candidates) == 0 {
		report.Nothing = true
		return report, nil
	}
	if dryRun {
		return report, nil
	}

	const uri = "wiki/log.md"
	oldData, err := base.ReadFileContext(ctx, uri, core.MaxNarrativeBytes)
	if errors.Is(err, fs.ErrNotExist) {
		oldData = []byte{}
	} else if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	newData, err := proposeLearnLog(oldData, candidates, base.Now().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	diff, err := renderLearnDiff(uri, oldData, newData)
	if err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		report.Nothing = true
		return report, nil
	}
	patches, err := parseLearnDiff(diff)
	if err != nil {
		return nil, fmt.Errorf("validate generated proposal: %w", err)
	}
	id := learnProposalDigest(diff)
	proposal := learnProposalFromPatches(id, diff, patches, false)
	directory, err := ensureLearnDirectory(base, "")
	if err != nil {
		return nil, err
	}
	absolute := filepath.Join(directory, id+".diff")
	if existing, readErr := core.ReadFileLimit(absolute, maxLearnProposalBytes); readErr == nil {
		if string(existing) != string(diff) {
			return nil, fmt.Errorf("learn proposal id collision at %s", proposal.Path)
		}
		report.Proposal, report.Existing = &proposal, true
		return report, nil
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect %s: %w", proposal.Path, readErr)
	}
	if err := core.WriteFileAtomicMode(absolute, diff, core.BaseFileMode); err != nil {
		return nil, fmt.Errorf("write %s: %w", proposal.Path, err)
	}
	report.Proposal = &proposal
	return report, nil
}

// ReviewLearn lists active proposals or reads one exact proposal. It never creates directories.
func ReviewLearn(ctx context.Context, base *Base, id string, includeDiff bool) (*LearnReview, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if id != "" {
		id, err := normalizeLearnProposalID(id)
		if err != nil {
			return nil, err
		}
		proposal, _, err := readLearnProposal(base, "", id, includeDiff)
		if err != nil {
			return nil, err
		}
		return &LearnReview{Proposals: []LearnProposal{proposal}}, nil
	}
	if includeDiff {
		return nil, learnProposalError("review --diff requires one proposal id")
	}
	directory, err := inspectLearnDirectory(base, "")
	if errors.Is(err, fs.ErrNotExist) {
		return &LearnReview{Proposals: []LearnProposal{}}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("list learn proposals: %w", err)
	}
	proposals := make([]LearnProposal, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".diff") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".diff")
		if !learnProposalIDPattern.MatchString(id) {
			return nil, learnProposalError("active queue contains invalid filename %q", entry.Name())
		}
		proposal, _, err := readLearnProposal(base, "", id, false)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	slices.SortFunc(proposals, func(left, right LearnProposal) int { return strings.Compare(left.ID, right.ID) })
	return &LearnReview{Proposals: proposals}, nil
}

// RejectLearn moves one active proposal into the ignored rejected archive. Repeating the same
// transition reports already-rejected rather than losing the audit record or failing a routine.
func RejectLearn(ctx context.Context, base *Base, id string) (*LearnActionReport, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	id, err := normalizeLearnProposalID(id)
	if err != nil {
		return nil, err
	}
	proposal, source, err := readLearnProposal(base, "", id, false)
	if errors.Is(err, fs.ErrNotExist) {
		if rejected, _, rejectedErr := readLearnProposal(base, "rejected", id, false); rejectedErr == nil {
			return &LearnActionReport{ID: id, Status: "already-rejected", Path: rejected.Path}, nil
		}
		if _, _, appliedErr := readLearnProposal(base, "applied", id, false); appliedErr == nil {
			return nil, learnProposalError("%s was already applied and cannot be rejected", id)
		}
		return nil, fmt.Errorf("%w: learn proposal %s does not exist", fs.ErrNotExist, id)
	}
	if err != nil {
		return nil, err
	}
	destinationDirectory, err := ensureLearnDirectory(base, "rejected")
	if err != nil {
		return nil, err
	}
	destination := filepath.Join(destinationDirectory, id+".diff")
	if _, err := os.Lstat(destination); err == nil {
		return nil, learnProposalError("rejected archive already contains %s", id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	data, err := core.ReadFileLimit(source, maxLearnProposalBytes)
	if err != nil {
		return nil, fmt.Errorf("re-read rejected proposal %s: %w", id, err)
	}
	if err := validateLearnProposalDigest(id, data); err != nil {
		return nil, err
	}
	if err := moveValidatedLearnProposal(source, destination, id, data); err != nil {
		return nil, fmt.Errorf("archive rejected proposal %s: %w", id, err)
	}
	if err := syncLearnMove(filepath.Dir(source), destinationDirectory); err != nil {
		moveBackErr := restoreLearnMove(destination, source)
		return nil, errors.Join(fmt.Errorf("sync rejected proposal archive: %w", err), moveBackErr)
	}
	return &LearnActionReport{ID: id, Status: "rejected", Path: learnProposalPath("rejected", id), Files: proposal.Files}, nil
}

// ApplyLearn validates and applies one queued diff, validates every affected authored layer,
// rebuilds derived caches, and only then archives the proposal. Any failure restores both the
// authored pages and the derived files to their exact prior bytes.
func ApplyLearn(ctx context.Context, base *Base, id string) (*LearnActionReport, error) {
	id, err := normalizeLearnProposalID(id)
	if err != nil {
		return nil, err
	}
	proposal, source, archived, err := activeLearnProposalForApply(base, id)
	if err != nil {
		return nil, err
	}
	if archived != nil {
		return archived, nil
	}
	data, err := core.ReadFileLimit(source, maxLearnProposalBytes)
	if err != nil {
		return nil, err
	}
	if err := validateLearnProposalDigest(id, data); err != nil {
		return nil, err
	}
	patches, err := parseLearnDiff(data)
	if err != nil {
		return nil, err
	}
	updates, snapshots, layers, err := prepareLearnUpdates(ctx, base, patches)
	if err != nil {
		return nil, err
	}
	if err := addLearnDerivedSnapshots(base, snapshots); err != nil {
		return nil, err
	}
	appliedDirectory, destination, err := prepareLearnArchive(base, "applied", id)
	if err != nil {
		return nil, err
	}
	validations, build, err := executeLearnApplication(
		ctx, base, updates, snapshots, layers, source, destination, appliedDirectory, id, data,
	)
	if err != nil {
		return nil, err
	}
	return &LearnActionReport{
		ID: id, Status: "applied", Path: learnProposalPath("applied", id),
		Files: proposal.Files, Validations: validations, Build: build,
	}, nil
}

func activeLearnProposalForApply(
	base *Base, id string,
) (LearnProposal, string, *LearnActionReport, error) {
	proposal, source, err := readLearnProposal(base, "", id, false)
	if !errors.Is(err, fs.ErrNotExist) {
		return proposal, source, nil, err
	}
	if applied, _, appliedErr := readLearnProposal(base, "applied", id, false); appliedErr == nil {
		return LearnProposal{}, "", &LearnActionReport{
			ID: id, Status: "already-applied", Path: applied.Path, Files: applied.Files,
		}, nil
	}
	if _, _, rejectedErr := readLearnProposal(base, "rejected", id, false); rejectedErr == nil {
		return LearnProposal{}, "", nil, learnProposalError("%s was rejected and cannot be applied", id)
	}
	return LearnProposal{}, "", nil, fmt.Errorf("%w: learn proposal %s does not exist", fs.ErrNotExist, id)
}

func addLearnDerivedSnapshots(base *Base, snapshots map[string]learnSnapshot) error {
	derivedFiles := []struct {
		uri     string
		limit   int64
		private bool
	}{
		{uri: core.GraphFile, limit: core.MaxLocalInputBytes},
		{uri: core.GraphDstFile, limit: core.MaxLocalInputBytes},
		{uri: core.GraphOffsetsFile, limit: core.MaxLocalInputBytes},
		{uri: core.GraphMetaFile, limit: core.MaxLocalInputBytes},
		{uri: core.GraphGenerationFile, limit: core.MaxSourceDocumentBytes},
		{uri: "wiki/index.md", limit: core.MaxLocalInputBytes},
		{uri: LexicalIndexPath, limit: maxLexicalIndexBytes, private: true},
		{uri: lexicalIndexMetaPath, limit: core.MaxSourceDocumentBytes, private: true},
	}
	for _, derived := range derivedFiles {
		if derived.uri == "wiki/index.md" && !base.Store.Enabled(core.LayerWiki) {
			continue
		}
		if _, exists := snapshots[derived.uri]; exists {
			continue
		}
		snapshot, err := snapshotLearnDerivedFile(base, derived.uri, derived.limit, derived.private)
		if err != nil {
			return err
		}
		snapshots[derived.uri] = snapshot
	}
	return nil
}

func snapshotLearnDerivedFile(base *Base, uri string, limit int64, private bool) (learnSnapshot, error) {
	if !private {
		return snapshotLearnFile(base, uri, limit)
	}
	rows, meta, err := lexicalIndexPaths(base)
	if err != nil {
		return learnSnapshot{}, err
	}
	absolute := rows
	if uri == lexicalIndexMetaPath {
		absolute = meta
	}
	return snapshotLearnAbsolute(uri, absolute, limit)
}

func prepareLearnArchive(base *Base, archive, id string) (string, string, error) {
	directory, err := ensureLearnDirectory(base, archive)
	if err != nil {
		return "", "", err
	}
	destination := filepath.Join(directory, id+".diff")
	if _, err := os.Lstat(destination); err == nil {
		return "", "", learnProposalError("%s archive already contains %s", archive, id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}
	return directory, destination, nil
}

func executeLearnApplication(
	ctx context.Context,
	base *Base,
	updates []learnUpdate,
	snapshots map[string]learnSnapshot,
	layers []core.Layer,
	source, destination, appliedDirectory, id string,
	proposalData []byte,
) ([]*ValidationReport, *BuildReport, error) {
	published := maps.Clone(snapshots)
	rollback := func(cause error) error {
		if restoreErr := restoreLearnSnapshots(snapshots, published); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback learn proposal: %w", restoreErr))
		}
		return cause
	}
	if err := writeLearnUpdates(updates, snapshots, published); err != nil {
		return nil, nil, rollback(err)
	}
	validations, err := validateLearnUpdates(ctx, base, layers)
	if err != nil {
		return nil, nil, rollback(err)
	}
	build, err := buildWithObserver(ctx, base, "", false, func() error {
		return captureLearnDerivedPublications(snapshots, published, updates)
	})
	if err != nil {
		return nil, nil, rollback(fmt.Errorf("rebuild after applying proposal: %w", err))
	}
	if err := moveValidatedLearnProposal(source, destination, id, proposalData); err != nil {
		return nil, nil, rollback(fmt.Errorf("archive applied proposal %s: %w", id, err))
	}
	if err := syncLearnMove(filepath.Dir(source), appliedDirectory); err != nil {
		moveBackErr := restoreLearnMove(destination, source)
		return nil, nil, rollback(errors.Join(fmt.Errorf("sync applied proposal archive: %w", err), moveBackErr))
	}
	return validations, build, nil
}

func moveValidatedLearnProposal(source, destination, id string, expected []byte) error {
	current, err := core.ReadFileLimit(source, maxLearnProposalBytes)
	if err != nil {
		return err
	}
	if err := validateLearnProposalDigest(id, current); err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return learnProposalError("proposal %s changed before it could be archived", id)
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	archived, readErr := core.ReadFileLimit(destination, maxLearnProposalBytes)
	if readErr == nil {
		readErr = validateLearnProposalDigest(id, archived)
	}
	if readErr == nil && !bytes.Equal(archived, expected) {
		readErr = learnProposalError("proposal %s changed while it was being archived", id)
	}
	if readErr == nil {
		return nil
	}
	return errors.Join(readErr, restoreLearnMove(destination, source))
}

func restoreLearnMove(destination, source string) error {
	if _, err := os.Lstat(source); err == nil {
		return fmt.Errorf("cannot restore proposal because %s now exists", source)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(destination, source)
}

func writeLearnUpdates(
	updates []learnUpdate, snapshots, published map[string]learnSnapshot,
) error {
	return writeLearnUpdatesWithObserver(updates, snapshots, published, nil)
}

func writeLearnUpdatesWithObserver(
	updates []learnUpdate,
	snapshots, published map[string]learnSnapshot,
	observe func(int),
) error {
	// Applying a proposal may spend time snapshotting derived caches. Recheck the authored
	// inputs at the last possible point so an editor save during that work is never overwritten.
	if err := verifyLearnUpdateSnapshots(updates, snapshots); err != nil {
		return err
	}
	for index, update := range updates {
		if err := verifyLearnUpdateSnapshot(update, snapshots); err != nil {
			return err
		}
		if err := core.WriteFileAtomicMode(update.absolute, update.data, update.mode); err != nil {
			return fmt.Errorf("apply %s: %w", update.uri, err)
		}
		snapshot := snapshots[update.uri]
		published[update.uri] = learnSnapshot{
			absolute: update.absolute, exists: true, data: slices.Clone(update.data),
			mode: update.mode, limit: snapshot.limit,
		}
		if observe != nil {
			observe(index)
		}
	}
	return nil
}

func captureLearnDerivedPublications(
	snapshots, published map[string]learnSnapshot, updates []learnUpdate,
) error {
	authored := make(map[string]struct{}, len(updates))
	for _, update := range updates {
		authored[update.uri] = struct{}{}
	}
	for uri, original := range snapshots {
		if _, found := authored[uri]; found && uri != "wiki/index.md" {
			continue
		}
		current, err := snapshotLearnAbsolute(uri, original.absolute, original.limit)
		if err != nil {
			return err
		}
		published[uri] = current
	}
	return nil
}

func verifyLearnUpdateSnapshots(updates []learnUpdate, snapshots map[string]learnSnapshot) error {
	for _, update := range updates {
		if err := verifyLearnUpdateSnapshot(update, snapshots); err != nil {
			return err
		}
	}
	return nil
}

func verifyLearnUpdateSnapshot(update learnUpdate, snapshots map[string]learnSnapshot) error {
	expected, found := snapshots[update.uri]
	if !found {
		return learnProposalError("target %s has no approved snapshot", update.uri)
	}
	current, err := snapshotLearnAbsolute(update.uri, update.absolute, core.MaxNarrativeBytes)
	if err != nil {
		return err
	}
	if !learnSnapshotsEqual(current, expected) {
		return learnProposalError("target %s changed after the proposal was prepared", update.uri)
	}
	return nil
}

func validateLearnUpdates(
	ctx context.Context, base *Base, layers []core.Layer,
) ([]*ValidationReport, error) {
	validations := make([]*ValidationReport, 0, len(layers))
	for _, layer := range layers {
		report, err := ValidateMarkdownLayer(ctx, base, layer, layer == core.LayerProjects, true)
		if err != nil {
			return nil, fmt.Errorf("validate %s after applying proposal: %w", layer, err)
		}
		validations = append(validations, report)
		if !report.OK {
			return nil, learnValidationError(report)
		}
	}
	return validations, nil
}

func syncLearnMove(sourceDirectory, destinationDirectory string) error {
	if err := core.SyncDirectory(destinationDirectory); err != nil {
		return err
	}
	if sourceDirectory != destinationDirectory {
		return core.SyncDirectory(sourceDirectory)
	}
	return nil
}

type learnUpdate struct {
	uri      string
	absolute string
	data     []byte
	mode     os.FileMode
}

type learnSnapshot struct {
	absolute string
	exists   bool
	data     []byte
	mode     os.FileMode
	limit    int64
}

func prepareLearnUpdates(
	ctx context.Context, base *Base, patches []learnFilePatch,
) ([]learnUpdate, map[string]learnSnapshot, []core.Layer, error) {
	updates := make([]learnUpdate, 0, len(patches))
	snapshots := make(map[string]learnSnapshot, len(patches)+3)
	layerSet := map[core.Layer]bool{}
	for _, patch := range patches {
		if err := checkContext(ctx); err != nil {
			return nil, nil, nil, err
		}
		layer, _ := base.Store.LayerOf(patch.URI)
		if err := base.RequireLayer(layer); err != nil {
			return nil, nil, nil, err
		}
		snapshot, err := snapshotLearnFile(base, patch.URI, core.MaxNarrativeBytes)
		if err != nil {
			return nil, nil, nil, err
		}
		if patch.New == snapshot.exists {
			if patch.New {
				return nil, nil, nil, learnProposalError("new page %s already exists", patch.URI)
			}
			return nil, nil, nil, learnProposalError("page %s does not exist", patch.URI)
		}
		updated, err := applyLearnFilePatch(snapshot.data, patch)
		if err != nil {
			return nil, nil, nil, err
		}
		if int64(len(updated)) > core.MaxNarrativeBytes {
			return nil, nil, nil, learnProposalError("resulting page %s is %d bytes; limit is %d", patch.URI, len(updated), core.MaxNarrativeBytes)
		}
		if _, err := ParsePage(patch.URI, updated, base.Now()); err != nil {
			return nil, nil, nil, learnProposalError("resulting page %s cannot be parsed: %v", patch.URI, err)
		}
		mode := snapshot.mode
		if !snapshot.exists {
			mode = core.BaseFileMode
		}
		updates = append(updates, learnUpdate{uri: patch.URI, absolute: snapshot.absolute, data: updated, mode: mode})
		snapshots[patch.URI] = snapshot
		layerSet[layer] = true
	}
	layers := make([]core.Layer, 0, len(layerSet))
	for _, layer := range []core.Layer{core.LayerWiki, core.LayerProjects} {
		if layerSet[layer] {
			layers = append(layers, layer)
		}
	}
	return updates, snapshots, layers, nil
}

func snapshotLearnFile(base *Base, uri string, limit int64) (learnSnapshot, error) {
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		return learnSnapshot{}, err
	}
	return snapshotLearnAbsolute(uri, absolute, limit)
}

func snapshotLearnAbsolute(uri, absolute string, limit int64) (learnSnapshot, error) {
	info, err := os.Lstat(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		return learnSnapshot{absolute: absolute, mode: core.BaseFileMode, limit: limit}, nil
	}
	if err != nil {
		return learnSnapshot{}, fmt.Errorf("inspect %s: %w", uri, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return learnSnapshot{}, learnProposalError("target %s is not a regular non-symlink file", uri)
	}
	data, err := core.ReadFileLimit(absolute, limit)
	if err != nil {
		return learnSnapshot{}, fmt.Errorf("read %s: %w", uri, err)
	}
	return learnSnapshot{
		absolute: absolute, exists: true, data: data, mode: info.Mode().Perm(), limit: limit,
	}, nil
}

func restoreLearnSnapshots(snapshots, published map[string]learnSnapshot) error {
	keys := make([]string, 0, len(snapshots))
	for uri := range snapshots {
		keys = append(keys, uri)
	}
	slices.Sort(keys)
	var failures []error
	for _, uri := range keys {
		snapshot := snapshots[uri]
		expected, found := published[uri]
		if !found {
			failures = append(failures, fmt.Errorf("refuse to restore %s without a published snapshot", uri))
			continue
		}
		current, err := snapshotLearnAbsolute(uri, snapshot.absolute, snapshot.limit)
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect %s before restore: %w", uri, err))
			continue
		}
		if learnSnapshotsEqual(current, snapshot) {
			continue
		}
		if !learnSnapshotsEqual(current, expected) {
			failures = append(failures, fmt.Errorf("refuse to restore changed file %s", uri))
			continue
		}
		if snapshot.exists {
			if err := core.WriteFileAtomicMode(snapshot.absolute, snapshot.data, snapshot.mode); err != nil {
				failures = append(failures, fmt.Errorf("restore %s: %w", uri, err))
			}
			continue
		}
		if err := os.Remove(snapshot.absolute); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, fmt.Errorf("remove newly created %s: %w", uri, err))
		}
	}
	return errors.Join(failures...)
}

func learnSnapshotsEqual(left, right learnSnapshot) bool {
	return left.exists == right.exists && left.mode == right.mode && bytes.Equal(left.data, right.data)
}

func learnValidationError(report *ValidationReport) error {
	if len(report.Issues) == 0 {
		return learnProposalError("strict %s validation failed", report.Layer)
	}
	issue := report.Issues[0]
	return learnProposalError("strict %s validation failed at %s: %s", report.Layer, issue.URI, issue.Message)
}

func proposeLearnLog(existing []byte, candidates []LearnCandidate, date string) ([]byte, error) {
	frontmatter, body, _, err := splitFrontmatter(existing)
	if err != nil {
		return nil, learnProposalError("wiki/log.md: %v", err)
	}
	if len(existing) == 0 {
		body = []byte("# Log\n")
	}
	traces := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if !seen[candidate.Trace] {
			seen[candidate.Trace] = true
			traces = append(traces, candidate.Trace)
		}
	}
	slices.Sort(traces)
	frontmatter, err = addLearnSources(frontmatter, traces)
	if err != nil {
		return nil, err
	}
	body = insertLearnLogBullets(body, candidates, date)
	return []byte("---\n" + string(frontmatter) + "---\n\n" + string(body)), nil
}

func addLearnSources(frontmatter []byte, traces []string) ([]byte, error) {
	var document yaml.Node
	if len(frontmatter) == 0 {
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	} else if err := yaml.Unmarshal(frontmatter, &document); err != nil {
		return nil, learnProposalError("wiki/log.md frontmatter: %v", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, learnProposalError("wiki/log.md frontmatter must be a mapping")
	}
	mapping := document.Content[0]
	var sourcesNode *yaml.Node
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == "sources" {
			sourcesNode = mapping.Content[index+1]
			break
		}
	}
	if sourcesNode == nil {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sources"},
			&yaml.Node{Kind: yaml.SequenceNode},
		)
		sourcesNode = mapping.Content[len(mapping.Content)-1]
	}
	if sourcesNode.Kind != yaml.SequenceNode {
		return nil, learnProposalError("wiki/log.md frontmatter sources must be a list")
	}
	existing := map[string]bool{}
	for _, item := range sourcesNode.Content {
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			return nil, learnProposalError("wiki/log.md frontmatter sources must contain only non-empty strings")
		}
		existing[item.Value] = true
	}
	for _, trace := range traces {
		citation := "../" + trace + "#learned"
		if !existing[citation] {
			sourcesNode.Content = append(sourcesNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: citation})
		}
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return nil, learnProposalError("encode wiki/log.md frontmatter: %v", err)
	}
	return encoded, nil
}

func insertLearnLogBullets(body []byte, candidates []LearnCandidate, date string) []byte {
	lines, _ := learnTextLines(ensureTrailingNewline(body))
	bullets := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		bullets = append(bullets, "- "+candidate.Text)
	}
	heading := "## " + date
	for index, line := range lines {
		if line != heading {
			continue
		}
		at := index + 1
		if at < len(lines) && lines[at] == "" {
			at++
		}
		insert := append(append([]string{}, bullets...), "")
		lines = slices.Insert(lines, at, insert...)
		return []byte(strings.Join(lines, "\n") + "\n")
	}
	at := len(lines)
	for index, line := range lines {
		if strings.HasPrefix(line, "## ") {
			at = index
			break
		}
	}
	block := append([]string{heading, ""}, bullets...)
	block = append(block, "")
	if at > 0 && lines[at-1] != "" {
		block = append([]string{""}, block...)
	}
	lines = slices.Insert(lines, at, block...)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func ensureTrailingNewline(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return append(slices.Clone(data), '\n')
	}
	return data
}

func readLearnProposal(base *Base, archive, id string, includeDiff bool) (LearnProposal, string, error) {
	directory, err := inspectLearnDirectory(base, archive)
	if err != nil {
		return LearnProposal{}, "", err
	}
	absolute := filepath.Join(directory, id+".diff")
	info, err := os.Lstat(absolute)
	if err != nil {
		return LearnProposal{}, absolute, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return LearnProposal{}, absolute, learnProposalError("%s is not a regular non-symlink file", learnProposalPath(archive, id))
	}
	data, err := core.ReadFileLimit(absolute, maxLearnProposalBytes)
	if err != nil {
		return LearnProposal{}, absolute, err
	}
	if err := validateLearnProposalDigest(id, data); err != nil {
		return LearnProposal{}, absolute, err
	}
	patches, err := parseLearnDiff(data)
	if err != nil {
		return LearnProposal{}, absolute, fmt.Errorf("%s: %w", learnProposalPath(archive, id), err)
	}
	proposal := learnProposalFromPatches(id, data, patches, includeDiff)
	proposal.Path = learnProposalPath(archive, id)
	return proposal, absolute, nil
}

func learnProposalDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validateLearnProposalDigest(id string, data []byte) error {
	want := learnProposalDigest(data)
	if id != want {
		return learnProposalError("proposal id %s does not match its SHA-256 digest %s", id, want)
	}
	return nil
}

func learnProposalFromPatches(id string, data []byte, patches []learnFilePatch, includeDiff bool) LearnProposal {
	proposal := LearnProposal{ID: id, Path: learnProposalPath("", id), Bytes: len(data), Files: make([]string, 0, len(patches))}
	for _, patch := range patches {
		proposal.Files = append(proposal.Files, patch.URI)
	}
	if includeDiff {
		proposal.Diff = string(data)
	}
	return proposal
}

func normalizeLearnProposalID(value string) (string, error) {
	value = strings.TrimSuffix(value, ".diff")
	if !learnProposalIDPattern.MatchString(value) {
		return "", learnProposalError("proposal id %q must be lowercase letters, digits, and hyphens", value)
	}
	return value, nil
}

func learnProposalPath(archive, id string) string {
	parts := []string{learnProposalRelative}
	if archive != "" {
		parts = append(parts, archive)
	}
	parts = append(parts, id+".diff")
	return path.Join(parts...)
}

func inspectLearnDirectory(base *Base, archive string) (string, error) {
	root := filepath.Join(base.Root(), filepath.FromSlash(learnProposalRelative))
	if err := validateLearnPath(base.Root(), root); err != nil {
		return "", err
	}
	if archive != "" {
		root = filepath.Join(root, archive)
		if err := validateLearnPath(base.Root(), root); err != nil {
			return "", err
		}
	}
	return root, nil
}

func ensureLearnDirectory(base *Base, archive string) (string, error) {
	root := base.Root()
	for _, component := range []string{".agents", "tmp", "learn"} {
		root = filepath.Join(root, component)
		if err := ensureLearnPathComponent(base.Root(), root); err != nil {
			return "", err
		}
	}
	if archive != "" {
		if archive != "applied" && archive != "rejected" {
			return "", learnProposalError("unknown learn archive %q", archive)
		}
		root = filepath.Join(root, archive)
		if err := ensureLearnPathComponent(base.Root(), root); err != nil {
			return "", err
		}
	}
	return root, nil
}

func ensureLearnPathComponent(baseRoot, target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(target, core.BaseDirMode); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Base(target), err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return learnProposalError("%s must be a real directory below the base", filepath.ToSlash(strings.TrimPrefix(target, baseRoot+string(os.PathSeparator))))
	}
	return nil
}

func validateLearnPath(baseRoot, target string) error {
	if err := core.ValidateWithinRoot(baseRoot, target); err != nil {
		return err
	}
	relative, err := filepath.Rel(baseRoot, target)
	if err != nil {
		return err
	}
	cursor := baseRoot
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		cursor = filepath.Join(cursor, component)
		info, err := os.Lstat(cursor)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return learnProposalError("%s must be a real directory below the base", filepath.ToSlash(relative))
		}
	}
	return nil
}
