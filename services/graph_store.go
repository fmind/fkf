package services

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fmind/fkf/core"
)

// The edge list is stored as tab-separated rows rather than JSONL, which is a deliberate,
// documented exception to fkf's "JSON for stored data" rule. Three reasons, in order:
//
//  1. Every field is structurally token-safe. A URI cannot contain a raw tab, newline, or
//     carriage return (RFC 3986), and the remaining fields are timestamps and identifiers. So
//     the one failure mode of delimiter-separated text does not exist here, and rejecting those
//     bytes on write turns it from an assumption into an enforced invariant.
//  2. It makes the prefilter exact instead of merely necessary. `src` is a line prefix and
//     `dst` is a tab-delimited field, so a byte match cannot collide with a substring of some
//     other value — which is exactly the failure a JSON prefilter has to guard against.
//  3. It composes with the whole text toolkit. sort, join, cut, comm, uniq -c and awk operate
//     on this file directly; a sorted edge list is a relational join away from any question the
//     owner wants to ask, with no library involved.
//
// The cost is that `jq` cannot read the file. That is paid back at the command layer: fkf's
// output envelopes already emit JSON, so `fkf graph <uri>` feeds jq pipelines while storage
// stays optimized for scanning. The file is a derived cache — deletable and rebuildable — so
// there is no format lock-in to regret.
const (
	// EdgeSchemaVersion is the edge-list contract version. It lives in the sidecar rather
	// than on every row, because an index is regenerated as one unit.
	EdgeSchemaVersion = 3
	// GraphExtractorVersion changes only when edge-derivation semantics change. It is separate
	// from the metadata shape so identical inputs interpreted by an older extractor become stale.
	GraphExtractorVersion = 2
	// EdgeFieldSeparator separates columns. It may not appear inside any field.
	EdgeFieldSeparator = '\t'
	// EdgeFieldCount is the exact column count of a well-formed row.
	EdgeFieldCount = 6
)

const (
	// MaxEdgeLineBytes bounds one record. A row is six short tokens, so anything near this
	// bound is a corrupt or hostile file rather than a large edge.
	MaxEdgeLineBytes = 64 << 10
	// MaxEdgeListRows bounds one scan. Scanning is streaming, so this is a runaway guard.
	MaxEdgeListRows = 5_000_000
)

// EdgeColumns names the columns in on-disk order. It is recorded in the sidecar so the file
// stays self-describing without carrying a header row that `sort` would shuffle into the body.
var EdgeColumns = []string{"src", "dst", "kind", "at", "via", "indexed"}

var (
	ErrEdgeIncomplete  = errors.New("edge is missing a required field")
	ErrEdgeSeparator   = errors.New("edge field contains a separator byte")
	ErrEdgeControl     = errors.New("edge field contains a control or invisible character")
	ErrEdgeTime        = errors.New("edge timestamp is not canonical")
	ErrEdgeLineTooLong = errors.New("edge line exceeds the record size limit")
	ErrEdgeListTooBig  = errors.New("edge list exceeds the row limit")
)

// Edge is one derived relationship between two addressable records.
//
// Field order in this struct IS the on-disk column order. Reordering these fields is a storage
// format change, not a refactor.
//
// The two timestamps are deliberately separate and must not be merged. At is when the underlying
// fact happened, read from the source record; Indexed is when fkf derived this row. Conflating
// them makes "what changed in my base last night" and "what happened at work last night" the
// same query, which they are not.
type Edge struct {
	Src     string `json:"src"`               // URI of the record the relationship points from
	Dst     string `json:"dst"`               // URI of the record the relationship points to
	Kind    string `json:"kind"`              // observed relationship: declared fields, link/tag, or a frontmatter key
	At      string `json:"at,omitempty"`      // when the fact happened, from the source record
	Via     string `json:"via"`               // extractor that derived the edge, for provenance
	Indexed string `json:"indexed,omitempty"` // when fkf derived this row
}

func (e Edge) fields() [EdgeFieldCount]string {
	return [EdgeFieldCount]string{e.Src, e.Dst, e.Kind, e.At, e.Via, e.Indexed}
}

func (e Edge) encodedRowBytes() int {
	size := EdgeFieldCount // five separators plus the trailing newline
	for _, field := range e.fields() {
		size += len(field)
	}
	return size
}

// Valid reports whether an edge is well-formed. Via is required because an edge with no
// provenance cannot be audited, explained, or selectively rebuilt when one extractor changes.
// Separator bytes are rejected rather than escaped: an extractor that produces one has a bug,
// and silently encoding it would hide the bug instead of surfacing it.
func (e Edge) Valid() error {
	required := map[string]string{"src": e.Src, "dst": e.Dst, "kind": e.Kind, "via": e.Via}
	for _, name := range []string{"src", "dst", "kind", "via"} {
		if strings.TrimSpace(required[name]) == "" {
			return fmt.Errorf("%w: %s", ErrEdgeIncomplete, name)
		}
	}
	for position, value := range e.fields() {
		if strings.ContainsAny(value, "\t\n\r") {
			return fmt.Errorf("%w: %s", ErrEdgeSeparator, EdgeColumns[position])
		}
		for _, char := range value {
			if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
				return fmt.Errorf("%w: %s contains U+%04X", ErrEdgeControl, EdgeColumns[position], char)
			}
		}
	}
	if err := validateEdgeFactTime(e.At); err != nil {
		return fmt.Errorf("%w: at %q", err, e.At)
	}
	if e.Indexed != "" {
		if err := validateCanonicalEdgeTime(e.Indexed); err != nil {
			return fmt.Errorf("%w: indexed %q", err, e.Indexed)
		}
	}
	return nil
}

func validateEdgeFactTime(value string) error {
	if value == "" {
		return nil
	}
	if parsed, err := time.Parse(time.DateOnly, value); err == nil && parsed.Format(time.DateOnly) == value {
		return nil
	}
	return validateCanonicalEdgeTime(value)
}

func validateCanonicalEdgeTime(value string) error {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || value != parsed.UTC().Format(time.RFC3339) {
		return ErrEdgeTime
	}
	return nil
}

// sortKey orders edges so an index is byte-identical for identical input regardless of the
// order extractors happened to emit rows in. Sorting by Src first also groups every edge of a
// node contiguously, which is what makes `join` and a future binary search possible for free.
func (e Edge) sortKey() [5]string { return [5]string{e.Src, e.Dst, e.Kind, e.At, e.Via} }

// SortEdges sorts in place into the canonical on-disk order.
func SortEdges(edges []Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		return compareEdgeSortKeys(edges[i].sortKey(), edges[j].sortKey()) < 0
	})
}

func compareEdgeSortKeys(left, right [5]string) int {
	for position := range left {
		switch {
		case left[position] < right[position]:
			return -1
		case left[position] > right[position]:
			return 1
		}
	}
	return 0
}

// DedupeEdges removes rows identical on every field except Indexed, keeping the first. Two
// extractors legitimately find the same relationship; the index should record it once. It
// returns a new slice rather than compacting in place, because an exported helper that silently
// rewrites its caller's backing array is a trap.
func DedupeEdges(edges []Edge) []Edge {
	seen := make(map[[5]string]struct{}, len(edges))
	unique := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		key := edge.sortKey()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, edge)
	}
	return unique
}

// EncodeEdges writes rows as canonical TSV. It sorts and dedupes first, so the same input always
// produces the same bytes and `fkf build graph` stays a verifiable pure function.
func EncodeEdges(writer io.Writer, edges []Edge) error {
	rows := make([]Edge, len(edges))
	copy(rows, edges)
	SortEdges(rows)
	rows = DedupeEdges(rows)
	if err := validateEncodedEdges(rows); err != nil {
		return err
	}
	return encodeOrderedEdges(writer, rows)
}

func validateEncodedEdges(rows []Edge) error {
	if err := validateEdgeCount(len(rows)); err != nil {
		return err
	}
	// Validate every row before emitting the first byte. WriteEdgeList already buffers before
	// its atomic replace, and this preflight gives every other caller the same no-partial-output
	// behavior when an extractor produces a row no reader could admit.
	for index, edge := range rows {
		if err := edge.Valid(); err != nil {
			return fmt.Errorf("edge row %d: %w", index, err)
		}
		if size := edge.encodedRowBytes(); size > MaxEdgeLineBytes {
			return fmt.Errorf("%w: edge row %d encodes to %d bytes including its newline; maximum is %d",
				ErrEdgeLineTooLong, index, size, MaxEdgeLineBytes)
		}
	}
	return nil
}

func encodeOrderedEdges(writer io.Writer, rows []Edge) error {
	buffered := bufio.NewWriter(writer)
	for _, edge := range rows {
		fields := edge.fields()
		for position, value := range fields {
			if position > 0 {
				_ = buffered.WriteByte(EdgeFieldSeparator)
			}
			if _, err := buffered.WriteString(value); err != nil {
				return fmt.Errorf("encode edge %s -> %s: %w", edge.Src, edge.Dst, err)
			}
		}
		if err := buffered.WriteByte('\n'); err != nil {
			return fmt.Errorf("encode edge %s -> %s: %w", edge.Src, edge.Dst, err)
		}
	}
	return buffered.Flush()
}

func validateEdgeCount(count int) error {
	if count > MaxEdgeListRows {
		return fmt.Errorf("%w: %d rows exceeds the maximum of %d", ErrEdgeListTooBig, count, MaxEdgeListRows)
	}
	return nil
}

// DecodeEdge parses one row. A row with the wrong column count is malformed, which a scan
// reports rather than treating as fatal.
func DecodeEdge(line []byte) (Edge, bool) {
	parts := bytes.Split(line, []byte{EdgeFieldSeparator})
	if len(parts) != EdgeFieldCount {
		return Edge{}, false
	}
	edge := Edge{
		Src: string(parts[0]), Dst: string(parts[1]), Kind: string(parts[2]),
		At: string(parts[3]), Via: string(parts[4]), Indexed: string(parts[5]),
	}
	if err := edge.Valid(); err != nil {
		return Edge{}, false
	}
	return edge, true
}

// EdgeQuery selects rows. An empty field matches everything, so the zero query is a full scan.
type EdgeQuery struct {
	Src  string
	Dst  string
	Kind string
}

// prefix is the literal a matching line must start with. Because Src is the first column and no
// field may contain a tab, this is an exact test rather than a heuristic.
func (q EdgeQuery) prefix() []byte {
	if q.Src == "" {
		return nil
	}
	return []byte(q.Src + string(EdgeFieldSeparator))
}

// contains renders the interior columns as tab-delimited literals. Delimiting both sides is what
// prevents a match on a substring of a neighboring value.
func (q EdgeQuery) contains() [][]byte {
	var filters [][]byte
	for _, value := range []string{q.Dst, q.Kind} {
		if value == "" {
			continue
		}
		separator := string(EdgeFieldSeparator)
		filters = append(filters, []byte(separator+value+separator))
	}
	return filters
}

// Match confirms a decoded edge against the query. The prefilters are exact for this format, but
// confirming after decode keeps correctness independent of the filter implementation.
func (q EdgeQuery) Match(edge Edge) bool {
	return (q.Src == "" || edge.Src == q.Src) &&
		(q.Dst == "" || edge.Dst == q.Dst) &&
		(q.Kind == "" || edge.Kind == q.Kind)
}

// EdgeScanStats reports what one scan saw. Malformed is surfaced rather than fatal: a single
// corrupt row must not make an otherwise good index unreadable, but a caller has to be able to
// notice it, and `fkf status` reports a non-zero count.
//
// Malformed counts only rows that passed the prefilter and then failed to decode. A corrupt row
// that cannot match the query is skipped without being parsed, which is the point of the
// prefilter — so an integrity audit must scan with the zero EdgeQuery, not a narrow one.
type EdgeScanStats struct {
	Lines     int `json:"lines"`
	Matched   int `json:"matched"`
	Malformed int `json:"malformed"`
}

// ScanEdges streams an index, calling visit for each matching row under bounded memory. A visit
// error stops the scan and is returned, which lets a caller take the first N matches cheaply.
func ScanEdges(ctx context.Context, reader io.Reader, query EdgeQuery, visit func(Edge) error) (EdgeScanStats, error) {
	var stats EdgeScanStats
	prefix, contains := query.prefix(), query.contains()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), MaxEdgeLineBytes)

	for scanner.Scan() {
		if err := checkContext(ctx); err != nil {
			return stats, err
		}
		if stats.Lines++; stats.Lines > MaxEdgeListRows {
			return stats, ErrEdgeListTooBig
		}
		line := bytes.TrimRight(scanner.Bytes(), "\r")
		if len(line) == 0 {
			continue
		}
		if prefix != nil && !bytes.HasPrefix(line, prefix) {
			continue
		}
		if !containsAllFilters(line, contains) {
			continue
		}
		edge, ok := DecodeEdge(line)
		if !ok {
			stats.Malformed++
			continue
		}
		if !query.Match(edge) {
			continue
		}
		stats.Matched++
		if visit != nil {
			if err := visit(edge); err != nil {
				return stats, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return stats, fmt.Errorf("%w: line %d", ErrEdgeLineTooLong, stats.Lines+1)
		}
		return stats, fmt.Errorf("scan edge list: %w", err)
	}
	return stats, nil
}

func containsAllFilters(line []byte, filters [][]byte) bool {
	for _, filter := range filters {
		if !bytes.Contains(line, filter) {
			return false
		}
	}
	return true
}

// EdgeListMeta is the sidecar describing one generated index. It lives beside the rows rather
// than as a header line so that every line of the index is an edge and nothing else, which keeps
// sort, join, and cut usable with no skip rule.
type EdgeListMeta struct {
	SchemaVersion    int                 `json:"schema_version"`
	ExtractorVersion int                 `json:"extractor_version"`
	Columns          []string            `json:"columns"`
	Separator        string              `json:"separator"`
	GeneratedAt      string              `json:"generated_at"`
	Edges            int                 `json:"edges"`
	Extractors       []string            `json:"extractors"`
	Bytes            int                 `json:"bytes"`
	Kinds            []string            `json:"kinds"`
	Inputs           []GraphFileManifest `json:"inputs"`
	Outputs          []GraphFileManifest `json:"outputs"`
	SHA256           GraphSHA256Manifest `json:"sha256"`
}

// GraphFileManifest binds one exact input or generated artifact to its filesystem identity.
// Ordinary reads compare size and mtime first; --verify hashes every listed file.
type GraphFileManifest struct {
	URI              string `json:"uri"`
	Bytes            int64  `json:"bytes"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
	SHA256           string `json:"sha256"`
}

// GraphSHA256Manifest separates the exact graph output from every meaningful logical input.
// These are deterministic freshness and integrity fingerprints, not authenticity signatures.
type GraphSHA256Manifest struct {
	Inputs  GraphInputSHA256  `json:"inputs"`
	Outputs GraphOutputSHA256 `json:"outputs"`
}

// GraphInputSHA256 is the closed graph-input vocabulary. AGGREGATE is synthetic; every other
// key names one source layer or the edge-relevant root schema semantics.
type GraphInputSHA256 struct {
	Aggregate string `json:"AGGREGATE"`
	Events    string `json:"events"`
	Index     string `json:"index"`
	Projects  string `json:"projects"`
	Tasks     string `json:"tasks"`
	Wiki      string `json:"wiki"`
	Schema    string `json:"schema"`
}

// GraphOutputSHA256 binds metadata to the exact canonical TSV bytes published beside it.
type GraphOutputSHA256 struct {
	GraphTSV        string `json:"graph.tsv"`
	GraphDstTSV     string `json:"graph.dst.tsv"`
	GraphOffsetsTSV string `json:"graph.offsets.tsv"`
}

var graphInputNames = []string{"events", "index", "projects", "tasks", "wiki", "schema"}

// NewGraphInputSHA256 validates each named component and computes the canonical aggregate.
func NewGraphInputSHA256(events, index, projects, tasks, wiki, schema string) (GraphInputSHA256, error) {
	inputs := GraphInputSHA256{
		Events: events, Index: index, Projects: projects, Tasks: tasks, Wiki: wiki, Schema: schema,
	}
	for _, name := range graphInputNames {
		if value := inputs.named(name); !isCanonicalSHA256(value) {
			return GraphInputSHA256{}, fmt.Errorf("%s input digest %q is not a lowercase SHA-256 digest", name, value)
		}
	}
	inputs.Aggregate = aggregateGraphInputsSHA256(inputs)
	return inputs, nil
}

func (inputs GraphInputSHA256) named(name string) string {
	switch name {
	case "events":
		return inputs.Events
	case "index":
		return inputs.Index
	case "projects":
		return inputs.Projects
	case "tasks":
		return inputs.Tasks
	case "wiki":
		return inputs.Wiki
	case "schema":
		return inputs.Schema
	default:
		return ""
	}
}

func aggregateGraphInputsSHA256(inputs GraphInputSHA256) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-inputs-v3\x00"))
	writeDigestValue(digest, []byte("extractor_version"))
	writeDigestValue(digest, []byte(strconv.Itoa(GraphExtractorVersion)))
	for _, name := range graphInputNames {
		writeDigestValue(digest, []byte(name))
		writeDigestValue(digest, []byte(inputs.named(name)))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// NewEdgeListMeta derives the sidecar from the rows being written. The clock is a parameter
// rather than an ambient time.Now call so a test can assert byte-identical output; determinism
// is a property of the function, not something a caller has to arrange.
func NewEdgeListMeta(
	edges []Edge, generatedAt time.Time, inputs GraphInputSHA256, inputFiles ...GraphFileManifest,
) (EdgeListMeta, error) {
	artifacts, err := encodeGraphArtifacts(edges)
	if err != nil {
		return EdgeListMeta{}, err
	}
	if problems := graphInputDigestProblems(inputs); len(problems) > 0 {
		return EdgeListMeta{}, errors.New(strings.Join(problems, "; "))
	}
	return edgeListMetaFromArtifacts(edges, generatedAt, inputs, inputFiles, artifacts), nil
}

func edgeListMetaFromArtifacts(
	edges []Edge,
	generatedAt time.Time,
	inputs GraphInputSHA256,
	inputFiles []GraphFileManifest,
	artifacts graphArtifacts,
) EdgeListMeta {
	// Edge rows use RFC3339 seconds, so every artifact records that same canonical instant.
	// Keeping sub-second clock noise would make the manifest disagree with its own rows.
	generatedAt = generatedAt.UTC().Truncate(time.Second)
	seen := make(map[string]struct{}, 8)
	extractors := make([]string, 0, 8)
	kindSet := make(map[string]struct{}, 8)
	for _, edge := range edges {
		kindSet[edge.Kind] = struct{}{}
		if _, known := seen[edge.Via]; known || edge.Via == "" {
			continue
		}
		seen[edge.Via] = struct{}{}
		extractors = append(extractors, edge.Via)
	}
	sort.Strings(extractors)
	kinds := slices.Sorted(maps.Keys(kindSet))
	outputFiles := graphOutputManifests(artifacts, generatedAt)
	return EdgeListMeta{
		SchemaVersion: EdgeSchemaVersion, ExtractorVersion: GraphExtractorVersion,
		Columns: slices.Clone(EdgeColumns), Separator: "\\t",
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Edges:       len(DedupeEdges(edges)), Extractors: extractors, Bytes: len(artifacts.src),
		Kinds: kinds, Inputs: slices.Clone(inputFiles), Outputs: outputFiles,
		SHA256: GraphSHA256Manifest{
			Inputs: inputs,
			Outputs: GraphOutputSHA256{
				GraphTSV:        outputFilesSHA256(outputFiles, core.GraphFile),
				GraphDstTSV:     outputFilesSHA256(outputFiles, core.GraphDstFile),
				GraphOffsetsTSV: outputFilesSHA256(outputFiles, core.GraphOffsetsFile),
			},
		},
	}
}

func graphInputDigestProblems(inputs GraphInputSHA256) []string {
	problems := make([]string, 0, len(graphInputNames)+1)
	for _, name := range graphInputNames {
		if value := inputs.named(name); !isCanonicalSHA256(value) {
			problems = append(problems, fmt.Sprintf("%s input digest %q is not a lowercase SHA-256 digest", name, value))
		}
	}
	if !isCanonicalSHA256(inputs.Aggregate) {
		problems = append(problems, fmt.Sprintf("AGGREGATE input digest %q is not a lowercase SHA-256 digest", inputs.Aggregate))
	} else if want := aggregateGraphInputsSHA256(inputs); inputs.Aggregate != want {
		problems = append(problems, "AGGREGATE input digest does not match extractor_version and named inputs")
	}
	return problems
}

func isCanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// WriteEdgeList atomically replaces each file, rows first and metadata second. Several file
// renames cannot be atomic, so the sidecar binds the exact TSV bytes by length and SHA-256: a
// reader during the short publication window, or after a metadata-write failure, fails closed
// instead of accepting new rows under old metadata.
func WriteEdgeList(path, metaPath string, edges []Edge, meta EdgeListMeta) error {
	artifacts, err := encodeGraphArtifacts(edges)
	if err != nil {
		return err
	}
	generatedAt, err := time.Parse(time.RFC3339, meta.GeneratedAt)
	if err != nil || meta.GeneratedAt != generatedAt.UTC().Format(time.RFC3339) {
		return fmt.Errorf("edge-list metadata generated_at %q is not canonical UTC RFC3339", meta.GeneratedAt)
	}
	if problems := graphInputDigestProblems(meta.SHA256.Inputs); len(problems) > 0 {
		return fmt.Errorf("edge-list metadata %s", strings.Join(problems, "; "))
	}
	for index, edge := range edges {
		if edge.Indexed != meta.GeneratedAt {
			return fmt.Errorf("edge row %d indexed %q does not match metadata generated_at %q",
				index, edge.Indexed, meta.GeneratedAt)
		}
	}
	expected := edgeListMetaFromArtifacts(edges, generatedAt, meta.SHA256.Inputs, meta.Inputs, artifacts)
	if meta.SchemaVersion != expected.SchemaVersion || !slices.Equal(meta.Columns, expected.Columns) ||
		meta.ExtractorVersion != expected.ExtractorVersion ||
		meta.Separator != expected.Separator || meta.GeneratedAt != expected.GeneratedAt ||
		meta.Edges != expected.Edges || !slices.Equal(meta.Extractors, expected.Extractors) ||
		!slices.Equal(meta.Kinds, expected.Kinds) || !slices.Equal(meta.Inputs, expected.Inputs) ||
		!slices.Equal(meta.Outputs, expected.Outputs) || meta.Bytes != expected.Bytes ||
		meta.SHA256 != expected.SHA256 {
		return errors.New("edge-list metadata does not describe the exact canonical encoded rows")
	}
	generationPath := filepath.Join(filepath.Dir(metaPath), core.GraphGenerationFile)
	generation := graphGenerationSHA256(meta)
	if err := writeGraphGenerationState(generationPath, graphGenerationBuilding, generation); err != nil {
		return fmt.Errorf("mark graph generation as building: %w", err)
	}
	if err := writeGraphArtifact(path, artifacts.src, generatedAt); err != nil {
		return fmt.Errorf("write edge list: %w", err)
	}
	directory := filepath.Dir(path)
	if err := writeGraphArtifact(filepath.Join(directory, core.GraphDstFile), artifacts.dst, generatedAt); err != nil {
		return fmt.Errorf("write destination edge list: %w", err)
	}
	if err := writeGraphArtifact(filepath.Join(directory, core.GraphOffsetsFile), artifacts.offsets, generatedAt); err != nil {
		return fmt.Errorf("write graph offsets: %w", err)
	}
	if err := core.WriteDataToJSON(meta, metaPath); err != nil {
		return fmt.Errorf("write edge list metadata: %w", err)
	}
	if err := writeGraphGenerationState(generationPath, graphGenerationCurrent, generation); err != nil {
		return fmt.Errorf("publish graph generation: %w", err)
	}
	return nil
}

func writeGraphArtifact(path string, data []byte, generatedAt time.Time) error {
	if err := core.WriteFileAtomicMode(path, data, core.BaseFileMode); err != nil {
		return err
	}
	canonical := generatedAt.UTC()
	if err := os.Chtimes(path, canonical, canonical); err != nil {
		return fmt.Errorf("set deterministic modification time: %w", err)
	}
	return nil
}
