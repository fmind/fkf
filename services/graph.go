package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/fmind/fkf/core"
)

// graph.tsv at the base root is the edge list over the base's URIs. It is derived, gitignored,
// rebuildable, and never a source of truth — deleting it costs speed, never data.
//
// Every edge is transcription, never inference. A record contributes edges only through fields
// its stored schema declares as relations; a page contributes explicit relations, tags, and
// links its author wrote. Bodies are never scanned. The moment a graph edge can be wrong in an
// interesting way, the receipt built on top of it stops being credible.

// Edge kinds.
const (
	EdgeTag    = "tag"
	EdgeLink   = "link"
	EdgeSameAs = "same-as"
)

// GraphBuild reports one derive step.
type GraphBuild struct {
	URI       string       `json:"uri"`
	Edges     int          `json:"edges"`
	Documents int          `json:"documents"`
	Pages     int          `json:"pages"`
	Mode      string       `json:"mode"`
	Elapsed   string       `json:"elapsed"`
	Meta      EdgeListMeta `json:"meta"`
	Stale     bool         `json:"stale,omitempty"`
}

// BuildGraph rescans the whole base and replaces the derived edge cache. It is a pure function of
// the files on disk and the clock it is given, so the same base and clock yield byte-identical output.
func BuildGraph(ctx context.Context, base *Base) (*GraphBuild, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	started := base.Now()
	inputs, err := readGraphInputState(ctx, base)
	if err != nil {
		return nil, err
	}
	edges, counts, err := ExtractEdges(ctx, base)
	if err != nil {
		return nil, err
	}
	confirmedInputs, err := readGraphInputState(ctx, base)
	if err != nil {
		return nil, err
	}
	if confirmedInputs.SHA256 != inputs.SHA256 || !slices.Equal(confirmedInputs.Files, inputs.Files) {
		return nil, errors.New("graph inputs changed while the derived caches were being built; retry")
	}
	meta, err := writeGraph(base, edges, started, inputs)
	if err != nil {
		return nil, err
	}
	return &GraphBuild{
		URI: core.GraphFile, Edges: len(DedupeEdges(edges)),
		Documents: counts.documents, Pages: counts.pages, Mode: "full",
		Elapsed: base.Now().Sub(started).Round(time.Millisecond).String(),
		Meta:    meta,
	}, nil
}

func writeGraph(
	base *Base, edges []Edge, at time.Time, inputs graphInputState,
) (EdgeListMeta, error) {
	indexed := at.UTC().Format(time.RFC3339)
	for index := range edges {
		edges[index].Indexed = indexed
	}
	rows, err := base.Store.Resolve(core.GraphFile)
	if err != nil {
		return EdgeListMeta{}, err
	}
	meta, err := base.Store.Resolve(core.GraphMetaFile)
	if err != nil {
		return EdgeListMeta{}, err
	}
	if err := os.MkdirAll(path.Dir(rows), core.BaseDirMode); err != nil {
		return EdgeListMeta{}, fmt.Errorf("create %s: %w", path.Dir(rows), err)
	}
	metadata, err := NewEdgeListMeta(edges, at, inputs.SHA256, inputs.Files...)
	if err != nil {
		return EdgeListMeta{}, err
	}
	if err := WriteEdgeList(rows, meta, edges, metadata); err != nil {
		return EdgeListMeta{}, err
	}
	return metadata, nil
}

func graphSchemaInputSHA256(base *Base) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-graph-input-schema-v2\x00"))
	for _, name := range base.Config.Schema.Names() {
		definition := base.Config.Schema[name]
		writeDigestValue(digest, []byte(name))
		writeDigestValue(digest, []byte(definition.Cardinality))
		if definition.Relation {
			writeDigestValue(digest, []byte("relation"))
		} else {
			writeDigestValue(digest, []byte("value"))
		}
	}
	identityNames := make([]string, 0, len(base.Config.Identities))
	for name := range base.Config.Identities {
		identityNames = append(identityNames, name)
	}
	sort.Strings(identityNames)
	for _, name := range identityNames {
		identity := base.Config.Identities[name]
		writeDigestValue(digest, []byte("identity:"+name))
		writeDigestValue(digest, []byte(identity.Canonical))
		writeDigestValue(digest, []byte(identity.EffectiveKind()))
		if identity.Owner {
			writeDigestValue(digest, []byte("owner"))
		}
		for _, alias := range identity.Aliases {
			writeDigestValue(digest, []byte(alias))
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}
