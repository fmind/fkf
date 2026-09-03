package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

const graphOffsetFieldCount = 4

type graphArtifacts struct {
	src     []byte
	dst     []byte
	offsets []byte
}

type graphOffset struct {
	direction string
	node      string
	start     int64
	bytes     int64
}

func (offset graphOffset) key() string { return offset.direction + "\t" + offset.node }

func encodeGraphArtifacts(edges []Edge) (graphArtifacts, error) {
	rows := slicesClone(edges)
	SortEdges(rows)
	rows = DedupeEdges(rows)
	if err := validateEncodedEdges(rows); err != nil {
		return graphArtifacts{}, err
	}

	var src bytes.Buffer
	if err := encodeOrderedEdges(&src, rows); err != nil {
		return graphArtifacts{}, err
	}
	srcOffsets := graphOffsetsForRows(rows, "src", func(edge Edge) string { return edge.Src })

	dstRows := slicesClone(rows)
	sort.SliceStable(dstRows, func(left, right int) bool {
		return compareDestinationEdges(dstRows[left], dstRows[right]) < 0
	})
	var dst bytes.Buffer
	if err := encodeOrderedEdges(&dst, dstRows); err != nil {
		return graphArtifacts{}, err
	}
	dstOffsets := graphOffsetsForRows(dstRows, "dst", func(edge Edge) string { return edge.Dst })
	offsets, err := encodeGraphOffsets(append(srcOffsets, dstOffsets...))
	if err != nil {
		return graphArtifacts{}, err
	}
	return graphArtifacts{src: src.Bytes(), dst: dst.Bytes(), offsets: offsets}, nil
}

func slicesClone(values []Edge) []Edge {
	return append([]Edge(nil), values...)
}

func compareDestinationEdges(left, right Edge) int {
	leftKey := [5]string{left.Dst, left.Src, left.Kind, left.At, left.Via}
	rightKey := [5]string{right.Dst, right.Src, right.Kind, right.At, right.Via}
	return compareEdgeSortKeys(leftKey, rightKey)
}

func graphOffsetsForRows(rows []Edge, direction string, nodeOf func(Edge) string) []graphOffset {
	offsets := make([]graphOffset, 0, len(rows))
	position := int64(0)
	for index := 0; index < len(rows); {
		node := nodeOf(rows[index])
		start := position
		for index < len(rows) && nodeOf(rows[index]) == node {
			position += int64(rows[index].encodedRowBytes())
			index++
		}
		offsets = append(offsets, graphOffset{
			direction: direction, node: node, start: start, bytes: position - start,
		})
	}
	return offsets
}

func encodeGraphOffsets(offsets []graphOffset) ([]byte, error) {
	sort.Slice(offsets, func(left, right int) bool { return offsets[left].key() < offsets[right].key() })
	var buffer bytes.Buffer
	previous := ""
	for _, offset := range offsets {
		if offset.key() <= previous || offset.start < 0 || offset.bytes <= 0 ||
			strings.ContainsAny(offset.node, "\t\n\r") {
			return nil, fmt.Errorf("invalid graph offset for %q", offset.node)
		}
		previous = offset.key()
		_, _ = fmt.Fprintf(&buffer, "%s\t%s\t%d\t%d\n",
			offset.direction, offset.node, offset.start, offset.bytes)
	}
	return buffer.Bytes(), nil
}

func decodeGraphOffset(line []byte) (graphOffset, error) {
	fields := bytes.Split(line, []byte{'\t'})
	if len(fields) != graphOffsetFieldCount {
		return graphOffset{}, errors.New("offset row has the wrong field count")
	}
	direction, node := string(fields[0]), string(fields[1])
	if (direction != "src" && direction != "dst") || node == "" {
		return graphOffset{}, errors.New("offset row has an invalid key")
	}
	start, err := strconv.ParseInt(string(fields[2]), 10, 64)
	if err != nil || start < 0 {
		return graphOffset{}, errors.New("offset row has an invalid start")
	}
	length, err := strconv.ParseInt(string(fields[3]), 10, 64)
	if err != nil || length <= 0 {
		return graphOffset{}, errors.New("offset row has an invalid byte length")
	}
	return graphOffset{direction: direction, node: node, start: start, bytes: length}, nil
}

func graphOutputManifests(artifacts graphArtifacts, generatedAt time.Time) []GraphFileManifest {
	mtime := generatedAt.UTC().UnixNano()
	entries := []struct {
		uri  string
		data []byte
	}{
		{uri: core.GraphDstFile, data: artifacts.dst},
		{uri: core.GraphOffsetsFile, data: artifacts.offsets},
		{uri: core.GraphFile, data: artifacts.src},
	}
	manifests := make([]GraphFileManifest, 0, len(entries))
	for _, entry := range entries {
		digest := sha256.Sum256(entry.data)
		manifests = append(manifests, GraphFileManifest{
			URI: entry.uri, Bytes: int64(len(entry.data)), ModifiedUnixNano: mtime,
			SHA256: hex.EncodeToString(digest[:]),
		})
	}
	return manifests
}

func outputFilesSHA256(files []GraphFileManifest, uri string) string {
	for _, file := range files {
		if file.URI == uri {
			return file.SHA256
		}
	}
	return ""
}
