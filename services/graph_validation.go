package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

func graphMetaStaticProblems(meta EdgeListMeta) []string {
	problems := make([]string, 0, 12)
	if meta.SchemaVersion != EdgeSchemaVersion {
		problems = append(problems, fmt.Sprintf("metadata schema_version is %d, want %d",
			meta.SchemaVersion, EdgeSchemaVersion))
	}
	if meta.ExtractorVersion != GraphExtractorVersion {
		problems = append(problems, fmt.Sprintf("metadata extractor_version is %d, want %d",
			meta.ExtractorVersion, GraphExtractorVersion))
	}
	if !slices.Equal(meta.Columns, EdgeColumns) {
		problems = append(problems, fmt.Sprintf("metadata columns are %v, want %v", meta.Columns, EdgeColumns))
	}
	if meta.Separator != "\\t" {
		problems = append(problems, fmt.Sprintf("metadata separator is %q, want %q", meta.Separator, "\\t"))
	}
	if meta.Edges < 0 || meta.Bytes < 0 {
		problems = append(problems, "metadata edge and byte counts must be non-negative")
	}
	generatedAt, generatedErr := time.Parse(time.RFC3339, meta.GeneratedAt)
	if generatedErr != nil || meta.GeneratedAt != generatedAt.UTC().Format(time.RFC3339) {
		problems = append(problems, fmt.Sprintf("metadata generated_at %q is not canonical UTC RFC3339", meta.GeneratedAt))
	}
	problems = append(problems, graphInputDigestProblems(meta.SHA256.Inputs)...)
	problems = append(problems, graphInputManifestProblems(meta)...)
	problems = append(problems, graphOutputManifestProblems(meta, generatedAt, generatedErr)...)
	if !strictlySortedUnique(meta.Extractors) {
		problems = append(problems, "metadata extractors are not strictly sorted and unique")
	}
	if !strictlySortedUnique(meta.Kinds) {
		problems = append(problems, "metadata kinds are not strictly sorted and unique")
	}
	return problems
}

func graphInputManifestProblems(meta EdgeListMeta) []string {
	problems := graphFileManifestProblems(meta.Inputs, "input")
	for _, file := range meta.Inputs {
		if _, found := graphInputLayer(file.URI); !found {
			problems = append(problems, fmt.Sprintf("metadata input URI %q is not a graph input", file.URI))
		}
	}
	derived, err := graphInputSHA256FromFiles(meta.Inputs, meta.SHA256.Inputs.Schema)
	if err != nil {
		problems = append(problems, fmt.Sprintf("metadata input manifest cannot be digested: %v", err))
	} else if derived != meta.SHA256.Inputs {
		problems = append(problems, "metadata input manifest does not match sha256.inputs")
	}
	return problems
}

func graphOutputManifestProblems(meta EdgeListMeta, generatedAt time.Time, generatedErr error) []string {
	problems := graphFileManifestProblems(meta.Outputs, "output")
	wantURIs := []string{core.GraphDstFile, core.GraphOffsetsFile, core.GraphFile}
	gotURIs := make([]string, 0, len(meta.Outputs))
	for _, file := range meta.Outputs {
		gotURIs = append(gotURIs, file.URI)
		if generatedErr == nil && file.ModifiedUnixNano != generatedAt.UnixNano() {
			problems = append(problems, fmt.Sprintf("metadata output %s mtime does not match generated_at", file.URI))
		}
	}
	if !slices.Equal(gotURIs, wantURIs) {
		problems = append(problems, fmt.Sprintf("metadata output URIs are %v, want %v", gotURIs, wantURIs))
	}
	if primary, found := graphManifestByURI(meta.Outputs, core.GraphFile); found && int64(meta.Bytes) != primary.Bytes {
		problems = append(problems, "metadata bytes does not match the graph.tsv output manifest")
	}
	for _, item := range []struct{ uri, digest string }{
		{core.GraphDstFile, meta.SHA256.Outputs.GraphDstTSV},
		{core.GraphOffsetsFile, meta.SHA256.Outputs.GraphOffsetsTSV},
		{core.GraphFile, meta.SHA256.Outputs.GraphTSV},
	} {
		manifest, found := graphManifestByURI(meta.Outputs, item.uri)
		if !found || manifest.SHA256 != item.digest {
			problems = append(problems, fmt.Sprintf("metadata sha256.outputs[%q] does not match its output manifest", item.uri))
		}
	}
	return problems
}

func graphFileManifestProblems(files []GraphFileManifest, label string) []string {
	problems := make([]string, 0, len(files))
	previous := ""
	for _, file := range files {
		if file.URI == "" || file.URI <= previous {
			problems = append(problems, fmt.Sprintf("metadata %s manifest is not strictly URI-sorted", label))
			break
		}
		previous = file.URI
		if file.Bytes < 0 {
			problems = append(problems, fmt.Sprintf("metadata %s %s has a negative byte count", label, file.URI))
		}
		if !isCanonicalSHA256(file.SHA256) {
			problems = append(problems, fmt.Sprintf("metadata %s %s has an invalid SHA-256", label, file.URI))
		}
	}
	return problems
}

func strictlySortedUnique(values []string) bool {
	for index, value := range values {
		if value == "" || index > 0 && value <= values[index-1] {
			return false
		}
	}
	return true
}

func graphManifestByURI(files []GraphFileManifest, uri string) (GraphFileManifest, bool) {
	for _, file := range files {
		if file.URI == uri {
			return file, true
		}
	}
	return GraphFileManifest{}, false
}

func (cache *validatedGraphCache) validateGraphOutputs(ctx context.Context, full bool) ([]string, error) {
	open := map[string]*os.File{
		core.GraphFile: cache.file, core.GraphDstFile: cache.dst, core.GraphOffsetsFile: cache.offsets,
	}
	problems := make([]string, 0, len(cache.meta.Outputs))
	cache.files = make([]graphFileSnapshot, 0, len(cache.meta.Outputs))
	for _, expected := range cache.meta.Outputs {
		file := open[expected.URI]
		if file == nil {
			continue
		}
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat %s snapshot: %w", expected.URI, err)
		}
		changed := info.Size() != expected.Bytes || info.ModTime().UnixNano() != expected.ModifiedUnixNano
		if full || changed {
			digest, stable, err := hashOpenGraphArtifact(ctx, file, expected.URI, info)
			if err != nil {
				return nil, err
			}
			info = stable
			if digest != expected.SHA256 {
				problems = append(problems, fmt.Sprintf(
					"%s bytes do not match metadata sha256.outputs", expected.URI))
			}
		}
		cache.files = append(cache.files, graphFileSnapshot{
			uri: expected.URI, file: file, bytes: info.Size(), modifiedUnixNano: info.ModTime().UnixNano(),
		})
	}
	return problems, nil
}

func graphTSVChanged(problems []string) bool {
	for _, problem := range problems {
		if strings.HasPrefix(problem, core.GraphFile+" bytes ") {
			return true
		}
	}
	return false
}

func hashOpenGraphArtifact(
	ctx context.Context, file *os.File, uri string, before os.FileInfo,
) (string, os.FileInfo, error) {
	if before.Size() > graphArtifactMaxBytes(uri) {
		return "", before, fmt.Errorf("%w: %s exceeds %d bytes",
			core.ErrFileTooLarge, uri, graphArtifactMaxBytes(uri))
	}
	digest := sha256.New()
	read, err := io.Copy(digest, contextReader{ctx: ctx, reader: io.NewSectionReader(file, 0, before.Size())})
	if err != nil {
		return "", before, fmt.Errorf("hash %s snapshot: %w", uri, err)
	}
	after, err := file.Stat()
	if err != nil {
		return "", before, fmt.Errorf("restat %s snapshot: %w", uri, err)
	}
	if read != before.Size() || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return "", after, fmt.Errorf("%s changed while it was being hashed; retry", uri)
	}
	return hex.EncodeToString(digest.Sum(nil)), after, nil
}

func graphArtifactMaxBytes(uri string) int64 {
	if uri == core.GraphOffsetsFile {
		return int64(maxGraphOffsetLineBytes) * int64(MaxEdgeListRows) * 2
	}
	return int64(MaxEdgeLineBytes) * int64(MaxEdgeListRows)
}

func scanGraphRows(
	ctx context.Context, base *Base, cache *validatedGraphCache, visit func(Edge) error,
) (EdgeScanStats, error) {
	if _, err := cache.file.Seek(0, io.SeekStart); err != nil {
		return EdgeScanStats{}, fmt.Errorf("rewind the edge-list snapshot: %w", err)
	}
	indexed := map[string]struct{}{}
	vias := map[string]struct{}{}
	kinds := map[string]struct{}{}
	edges := 0
	var previous [5]string
	havePrevious := false
	semanticProblem := ""
	counter := byteCounter(0)
	stats, err := ScanEdges(ctx, io.TeeReader(cache.file, &counter), EdgeQuery{}, func(edge Edge) error {
		edges++
		indexed[edge.Indexed], vias[edge.Via], kinds[edge.Kind] = struct{}{}, struct{}{}, struct{}{}
		if semanticProblem == "" {
			if err := validateCachedEdge(base, edge); err != nil {
				semanticProblem = err.Error()
			}
		}
		key := edge.sortKey()
		if semanticProblem == "" && havePrevious {
			switch compareEdgeSortKeys(previous, key) {
			case 0:
				semanticProblem = "graph rows contain a duplicate canonical edge"
			case 1:
				semanticProblem = "graph rows are not in canonical sort order"
			}
		}
		previous, havePrevious = key, true
		if visit != nil {
			return visit(edge)
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	problems := graphRowProblems(cache.meta, edges, int(counter), indexed, vias, kinds)
	if stats.Malformed > 0 {
		problems = append(problems, fmt.Sprintf("%s has %d malformed row(s)", core.GraphFile, stats.Malformed))
	}
	if semanticProblem != "" {
		problems = append(problems, semanticProblem)
	}
	if len(problems) > 0 {
		return stats, fmt.Errorf("invalid derived graph cache: %s; run `fkf build graph`",
			strings.Join(problems, "; "))
	}
	return stats, nil
}

func graphRowProblems(
	meta EdgeListMeta, edges, bytes int,
	indexed, vias, kinds map[string]struct{},
) []string {
	problems := make([]string, 0, 5)
	if meta.Edges != edges {
		problems = append(problems, fmt.Sprintf("metadata edges is %d, but %s holds %d valid row(s)",
			meta.Edges, core.GraphFile, edges))
	}
	if meta.Bytes != bytes {
		problems = append(problems, fmt.Sprintf("metadata bytes is %d, but %s holds %d byte(s)",
			meta.Bytes, core.GraphFile, bytes))
	}
	if observed := slices.Sorted(maps.Keys(vias)); !slices.Equal(meta.Extractors, observed) {
		problems = append(problems, fmt.Sprintf("metadata extractors are %v, but rows use %v", meta.Extractors, observed))
	}
	if observed := slices.Sorted(maps.Keys(kinds)); !slices.Equal(meta.Kinds, observed) {
		problems = append(problems, fmt.Sprintf("metadata kinds are %v, but rows use %v", meta.Kinds, observed))
	}
	for value := range indexed {
		if value != meta.GeneratedAt {
			problems = append(problems, fmt.Sprintf(
				"metadata generated_at %q does not match every indexed column", meta.GeneratedAt))
			break
		}
	}
	return problems
}
