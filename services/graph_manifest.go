package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
)

type graphInputState struct {
	SHA256 GraphInputSHA256
	Files  []GraphFileManifest
}

type graphValidationOptions struct {
	fullInputs  bool
	fullOutputs bool
	knownInputs map[string]GraphFileManifest
	hashInput   func(context.Context, *Base, string) (GraphFileManifest, error)
}

func readGraphInputState(ctx context.Context, base *Base) (graphInputState, error) {
	uris, err := graphInputURIs(ctx, base)
	if err != nil {
		return graphInputState{}, err
	}
	files := make([]GraphFileManifest, 0, len(uris))
	for _, uri := range uris {
		manifest, err := hashGraphInput(ctx, base, uri)
		if err != nil {
			return graphInputState{}, err
		}
		files = append(files, manifest)
	}
	inputs, err := graphInputSHA256FromFiles(files, graphSchemaInputSHA256(base))
	if err != nil {
		return graphInputState{}, err
	}
	return graphInputState{SHA256: inputs, Files: files}, nil
}

func graphInputSHA256FromFiles(files []GraphFileManifest, schema string) (GraphInputSHA256, error) {
	digests := make(map[core.Layer]string, 5)
	for _, layer := range []core.Layer{
		core.LayerEvents, core.LayerIndex, core.LayerProjects, core.LayerTasks, core.LayerWiki,
	} {
		digests[layer] = graphLayerInputSHA256(layer, files)
	}
	return NewGraphInputSHA256(
		digests[core.LayerEvents], digests[core.LayerIndex], digests[core.LayerProjects],
		digests[core.LayerTasks], digests[core.LayerWiki], schema,
	)
}

func graphLayerInputSHA256(layer core.Layer, files []GraphFileManifest) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-input-" + string(layer) + "-v2\x00"))
	for _, file := range files {
		fileLayer, found := graphInputLayer(file.URI)
		if !found || fileLayer != layer {
			continue
		}
		writeDigestValue(digest, []byte(file.URI))
		writeDigestValue(digest, []byte(file.SHA256))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func graphInputURIs(ctx context.Context, base *Base) ([]string, error) {
	uris, err := documentURIs(ctx, base)
	if err != nil {
		return nil, err
	}
	for _, layer := range []core.Layer{core.LayerProjects, core.LayerTasks, core.LayerWiki} {
		layerURIs, err := authoredGraphInputURIs(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		uris = append(uris, layerURIs...)
	}
	sort.Strings(uris)
	return uris, nil
}

func authoredGraphInputURIs(ctx context.Context, base *Base, layer core.Layer) ([]string, error) {
	if !base.Store.Enabled(layer) {
		return nil, nil
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	directory, err := base.Store.Dir(layer)
	if err != nil {
		return nil, err
	}
	if layer == core.LayerTasks {
		return taskGraphInputURIs(ctx, base, directory)
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list graph inputs under %s: %w", layer, err)
	}
	uris := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), core.MarkdownExtension) {
			uris = append(uris, path.Join(string(layer), entry.Name()))
		}
	}
	return uris, nil
}

// graphInputPageURIs remains the shared inventory seam used by the lexical cache. It lists
// authored graph inputs without parsing their contents.
func graphInputPageURIs(ctx context.Context, base *Base, layer core.Layer) ([]string, error) {
	return authoredGraphInputURIs(ctx, base, layer)
}

func taskGraphInputURIs(ctx context.Context, base *Base, directory string) ([]string, error) {
	dates, err := readDateDirectories(directory)
	if err != nil {
		return nil, err
	}
	var uris []string
	for _, date := range dates {
		slugs, err := readSubdirectories(path.Join(directory, date))
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			uri := path.Join(string(core.LayerTasks), date, slug, core.TaskTraceFile)
			if base.Exists(uri) {
				uris = append(uris, uri)
			}
		}
	}
	return uris, nil
}

func hashGraphInput(ctx context.Context, base *Base, uri string) (GraphFileManifest, error) {
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		return GraphFileManifest{}, fmt.Errorf("resolve graph input %s: %w", uri, err)
	}
	file, err := core.OpenRegularFile(absolute)
	if err != nil {
		return GraphFileManifest{}, fmt.Errorf("open graph input %s: %w", uri, err)
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return GraphFileManifest{}, fmt.Errorf("stat graph input %s: %w", uri, err)
	}
	limit := core.MaxNarrativeBytes
	if layer, _ := graphInputLayer(uri); layer == core.LayerEvents || layer == core.LayerIndex {
		limit = core.MaxSourceDocumentBytes
	}
	digest := sha256.New()
	read, err := io.Copy(digest, io.LimitReader(contextReader{ctx: ctx, reader: file}, limit+1))
	if err != nil {
		return GraphFileManifest{}, fmt.Errorf("hash graph input %s: %w", uri, err)
	}
	if read > limit {
		return GraphFileManifest{}, fmt.Errorf("%w: graph input %s exceeds %d bytes", core.ErrFileTooLarge, uri, limit)
	}
	after, err := file.Stat()
	if err != nil {
		return GraphFileManifest{}, fmt.Errorf("restat graph input %s: %w", uri, err)
	}
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return GraphFileManifest{}, fmt.Errorf("graph input %s changed while it was being hashed; retry", uri)
	}
	return GraphFileManifest{
		URI: uri, Bytes: after.Size(), ModifiedUnixNano: after.ModTime().UnixNano(),
		SHA256: hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

func graphInputLayer(uri string) (core.Layer, bool) {
	first, _, _ := strings.Cut(uri, "/")
	for _, layer := range []core.Layer{
		core.LayerEvents, core.LayerIndex, core.LayerProjects, core.LayerTasks, core.LayerWiki,
	} {
		if first == string(layer) {
			return layer, true
		}
	}
	return "", false
}

func currentGraphInputProblemsWithOptions(
	ctx context.Context, base *Base, meta EdgeListMeta, options graphValidationOptions,
) []string {
	currentURIs, err := graphInputURIs(ctx, base)
	if err != nil {
		return []string{fmt.Sprintf("cannot list current graph inputs: %v", err)}
	}
	manifest := make(map[string]GraphFileManifest, len(meta.Inputs))
	for _, input := range meta.Inputs {
		manifest[input.URI] = input
	}
	changed := make(map[string]bool, len(graphInputNames))
	if len(currentURIs) != len(meta.Inputs) {
		markGraphInputSetChanges(currentURIs, manifest, changed)
	}
	currentSet := make(map[string]bool, len(currentURIs))
	for _, uri := range currentURIs {
		currentSet[uri] = true
		expected, found := manifest[uri]
		if !found {
			markGraphInputChanged(uri, changed)
			continue
		}
		if err := validateCurrentGraphInput(ctx, base, expected, options); err != nil {
			if errors.Is(err, errGraphInputChanged) {
				markGraphInputChanged(uri, changed)
				continue
			}
			return []string{fmt.Sprintf("cannot validate current graph inputs: %v", err)}
		}
	}
	for _, expected := range meta.Inputs {
		if !currentSet[expected.URI] {
			markGraphInputChanged(expected.URI, changed)
		}
	}
	if meta.SHA256.Inputs.Schema != graphSchemaInputSHA256(base) {
		changed["schema"] = true
	}
	problems := make([]string, 0, len(changed))
	for _, name := range graphInputNames {
		if changed[name] {
			problems = append(problems, name+" input changed")
		}
	}
	return problems
}

var errGraphInputChanged = errors.New("graph input changed")

func validateCurrentGraphInput(
	ctx context.Context, base *Base, expected GraphFileManifest, options graphValidationOptions,
) error {
	absolute, err := base.Store.Resolve(expected.URI)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errGraphInputChanged
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: graph input %s is not a regular file", core.ErrUnsafePath, expected.URI)
	}
	known, hasKnown := options.knownInputs[expected.URI]
	if !options.fullInputs && hasKnown {
		if info.Size() != known.Bytes || info.ModTime().UnixNano() != known.ModifiedUnixNano {
			return errGraphInputChanged
		}
		if known.SHA256 != expected.SHA256 {
			return errGraphInputChanged
		}
		return nil
	}
	if !options.fullInputs && info.Size() == expected.Bytes &&
		info.ModTime().UnixNano() == expected.ModifiedUnixNano {
		return nil
	}
	hasher := options.hashInput
	if hasher == nil {
		hasher = hashGraphInput
	}
	current, err := hasher(ctx, base, expected.URI)
	if err != nil {
		return err
	}
	if current.SHA256 != expected.SHA256 {
		return errGraphInputChanged
	}
	return nil
}

func markGraphInputSetChanges(
	current []string, manifest map[string]GraphFileManifest, changed map[string]bool,
) {
	for _, uri := range current {
		if _, found := manifest[uri]; !found {
			markGraphInputChanged(uri, changed)
		}
	}
}

func markGraphInputChanged(uri string, changed map[string]bool) {
	if layer, found := graphInputLayer(uri); found {
		changed[string(layer)] = true
	}
}
