package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/fmind/fkf/core"
)

const (
	pageCursorVersion = 1
	maxCursorBytes    = 512
)

// pageCursor is deliberately self-contained: an MCP stdio server keeps no session state, so a
// continuation survives a client reconnect while still refusing a different query or result
// generation. It carries no evidence and grants no authority; changing the offset can only skip
// items in the same already-authorized read, so a signing key would add lifecycle without safety.
type pageCursor struct {
	Version        int    `json:"v"`
	Tool           string `json:"tool"`
	QuerySHA256    string `json:"query_sha256"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Offset         int    `json:"offset"`

	continued bool
}

func openPageCursor(raw, tool string, query any) (pageCursor, error) {
	querySHA256, err := jsonSHA256(query)
	if err != nil {
		return pageCursor{}, fmt.Errorf("encode the %s cursor query: %w", tool, err)
	}
	first := pageCursor{Version: pageCursorVersion, Tool: tool, QuerySHA256: querySHA256}
	if raw == "" {
		return first, nil
	}
	if len(raw) > maxCursorBytes {
		return pageCursor{}, invalidCursor("is %d bytes; expected at most %d", len(raw), maxCursorBytes)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return pageCursor{}, invalidCursor("is not unpadded base64url")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var cursor pageCursor
	if err := decoder.Decode(&cursor); err != nil {
		return pageCursor{}, invalidCursor("does not hold the published cursor shape")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return pageCursor{}, invalidCursor("holds more than one JSON value")
	}
	if cursor.Version != pageCursorVersion {
		return pageCursor{}, invalidCursor("has version %d; expected %d", cursor.Version, pageCursorVersion)
	}
	if cursor.Tool != tool {
		return pageCursor{}, invalidCursor("belongs to %s, not %s", cursor.Tool, tool)
	}
	if !isSHA256(cursor.QuerySHA256) || !isSHA256(cursor.SnapshotSHA256) {
		return pageCursor{}, invalidCursor("holds an invalid digest")
	}
	if cursor.QuerySHA256 != querySHA256 {
		return pageCursor{}, invalidCursor("does not match this effective query; repeat it or restart without cursor")
	}
	if cursor.Offset <= 0 || cursor.Offset > math.MaxInt-PageSize {
		return pageCursor{}, invalidCursor("holds an invalid offset")
	}
	cursor.continued = true
	return cursor, nil
}

func (cursor pageCursor) bindSnapshot(snapshotSHA256 string) error {
	if !cursor.continued {
		return nil
	}
	if cursor.SnapshotSHA256 != snapshotSHA256 {
		return fmt.Errorf("%w: cursor is stale because the result changed; restart without cursor", core.ErrConfig)
	}
	return nil
}

func (cursor pageCursor) validateOffset(total int) error {
	if cursor.continued && cursor.Offset >= total {
		return invalidCursor("offset %d is outside the %d-item result", cursor.Offset, total)
	}
	return nil
}

func (cursor pageCursor) next(snapshotSHA256 string, offset int) (string, error) {
	if !isSHA256(snapshotSHA256) {
		return "", fmt.Errorf("encode the %s cursor: invalid snapshot digest", cursor.Tool)
	}
	next := pageCursor{
		Version: pageCursorVersion, Tool: cursor.Tool, QuerySHA256: cursor.QuerySHA256,
		SnapshotSHA256: snapshotSHA256, Offset: offset,
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return "", fmt.Errorf("encode the %s cursor: %w", cursor.Tool, err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func invalidCursor(format string, values ...any) error {
	return fmt.Errorf("%w: invalid cursor: %s", core.ErrConfig, fmt.Sprintf(format, values...))
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func jsonSHA256(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func pageBounds(offset, limit, total int) (int, int) {
	start := min(offset, total)
	return start, min(start+limit, total)
}

func pageValues[T any](cursor pageCursor, snapshotSHA256 string, values []T, limit int) ([]T, string, error) {
	if err := cursor.bindSnapshot(snapshotSHA256); err != nil {
		return nil, "", err
	}
	if err := cursor.validateOffset(len(values)); err != nil {
		return nil, "", err
	}
	start, end := pageBounds(cursor.Offset, limit, len(values))
	if end == len(values) {
		return values[start:end], "", nil
	}
	nextCursor, err := cursor.next(snapshotSHA256, end)
	if err != nil {
		return nil, "", err
	}
	return values[start:end], nextCursor, nil
}

// pageJSON adds continuation metadata without decoding evidence through map[string]any, which
// would round large JSON numbers through float64. RawMessage preserves every original value.
func pageJSON(result any, nextCursor string) (json.RawMessage, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, fmt.Errorf("encode a paged result object: %w", err)
	}
	if nextCursor != "" {
		cursor, err := json.Marshal(nextCursor)
		if err != nil {
			return nil, err
		}
		object["next_cursor"] = cursor
	}
	return json.Marshal(object)
}
