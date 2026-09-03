package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/fmind/fkf/core"
)

const maxGraphOffsetLineBytes = MaxEdgeLineBytes + 128

func (cache *validatedGraphCache) openSeekArtifacts(base *Base) error {
	dst, err := openGraphArtifact(base, core.GraphDstFile)
	if err != nil {
		return err
	}
	cache.dst = dst
	offsets, err := openGraphArtifact(base, core.GraphOffsetsFile)
	if err != nil {
		return err
	}
	cache.offsets = offsets
	return nil
}

func openGraphArtifact(base *Base, uri string) (*os.File, error) {
	absolute, err := base.Store.Resolve(uri)
	if err != nil {
		return nil, err
	}
	file, err := core.OpenRegularFile(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("invalid derived graph cache: %s does not exist; run `fkf build graph`", uri)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", uri, err)
	}
	return file, nil
}

func (cache *validatedGraphCache) scanRange(
	ctx context.Context,
	file *os.File,
	direction, node string,
	query EdgeQuery,
	visit func(Edge) error,
) (EdgeScanStats, error) {
	offset, found, err := cache.findOffset(ctx, direction, node)
	if err != nil {
		return EdgeScanStats{}, err
	}
	if !found {
		return EdgeScanStats{}, nil
	}
	info, err := file.Stat()
	if err != nil {
		return EdgeScanStats{}, fmt.Errorf("stat %s graph snapshot: %w", direction, err)
	}
	if offset.start > info.Size() || offset.bytes > info.Size()-offset.start {
		return EdgeScanStats{}, fmt.Errorf("invalid derived graph cache: %s range for %s exceeds its edge list; run `fkf build graph`",
			direction, node)
	}
	return ScanEdges(ctx, io.NewSectionReader(file, offset.start, offset.bytes), query, visit)
}

func (cache *validatedGraphCache) findOffset(
	ctx context.Context, direction, node string,
) (graphOffset, bool, error) {
	info, err := cache.offsets.Stat()
	if err != nil {
		return graphOffset{}, false, fmt.Errorf("stat graph offset snapshot: %w", err)
	}
	target := direction + "\t" + node
	low, high := int64(0), info.Size()
	for low < high {
		if err := checkContext(ctx); err != nil {
			return graphOffset{}, false, err
		}
		middle := low + (high-low)/2
		offset, start, end, found, err := readGraphOffsetAtOrAfter(cache.offsets, info.Size(), middle)
		if err != nil {
			return graphOffset{}, false, err
		}
		if !found || start >= high {
			high = middle
			continue
		}
		if offset.key() < target {
			low = end
		} else {
			high = start
		}
	}
	offset, _, _, found, err := readGraphOffsetAtOrAfter(cache.offsets, info.Size(), low)
	if err != nil || !found || offset.key() != target {
		return graphOffset{}, false, err
	}
	return offset, true, nil
}

func readGraphOffsetAtOrAfter(
	file *os.File, size, position int64,
) (graphOffset, int64, int64, bool, error) {
	if position >= size {
		return graphOffset{}, position, position, false, nil
	}
	start := position
	if position > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], position-1); err != nil {
			return graphOffset{}, 0, 0, false, fmt.Errorf("read graph offset boundary: %w", err)
		}
		if previous[0] != '\n' {
			partial, err := readGraphOffsetLine(file, size, position)
			if err != nil {
				return graphOffset{}, 0, 0, false, err
			}
			start += int64(len(partial))
		}
	}
	if start >= size {
		return graphOffset{}, start, start, false, nil
	}
	line, err := readGraphOffsetLine(file, size, start)
	if err != nil {
		return graphOffset{}, 0, 0, false, err
	}
	decoded, err := decodeGraphOffset(bytes.TrimSuffix(line, []byte{'\n'}))
	if err != nil {
		return graphOffset{}, 0, 0, false, fmt.Errorf("decode %s at byte %d: %w", core.GraphOffsetsFile, start, err)
	}
	return decoded, start, start + int64(len(line)), true, nil
}

func readGraphOffsetLine(file *os.File, size, start int64) ([]byte, error) {
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, size-start), 4096)
	line, err := reader.ReadBytes('\n')
	if len(line) > maxGraphOffsetLineBytes {
		return nil, fmt.Errorf("%s line exceeds %d bytes", core.GraphOffsetsFile, maxGraphOffsetLineBytes)
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return nil, fmt.Errorf("%s does not end with a newline", core.GraphOffsetsFile)
		}
		return nil, fmt.Errorf("read %s: %w", core.GraphOffsetsFile, err)
	}
	if bytes.ContainsRune(line, '\r') {
		return nil, fmt.Errorf("%s contains a carriage return", core.GraphOffsetsFile)
	}
	return line, nil
}
