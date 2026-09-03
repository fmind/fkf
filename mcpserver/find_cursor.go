package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// findCursor uses a semantic keyset instead of an offset. Replaying page N therefore retains
// only the next bounded candidate set, not every item returned by pages 1 through N-1.
type findCursor struct {
	Version        int                   `json:"v"`
	Tool           string                `json:"tool"`
	QuerySHA256    string                `json:"query_sha256"`
	SnapshotSHA256 string                `json:"snapshot_sha256"`
	Position       services.FindPosition `json:"position"`

	continued bool
}

// A find cursor carries the last result URI, unlike the fixed-size offset cursors. Keep its
// encoder and decoder at the published tool-schema bound so the server never emits a token its
// next request rejects. An unusually long record identity must be narrowed through the CLI.
const maxFindCursorBytes = maxInputTextLength

func openFindCursor(raw string, query any) (findCursor, error) {
	querySHA256, err := jsonSHA256(query)
	if err != nil {
		return findCursor{}, fmt.Errorf("encode the find cursor query: %w", err)
	}
	first := findCursor{Version: pageCursorVersion, Tool: "find", QuerySHA256: querySHA256}
	if raw == "" {
		return first, nil
	}
	if len(raw) > maxFindCursorBytes {
		return findCursor{}, invalidCursor("is %d bytes; expected at most %d", len(raw), maxFindCursorBytes)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return findCursor{}, invalidCursor("is not unpadded base64url")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var cursor findCursor
	if err := decoder.Decode(&cursor); err != nil {
		return findCursor{}, invalidCursor("does not hold the published find cursor shape")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return findCursor{}, invalidCursor("holds more than one JSON value")
	}
	if cursor.Version != pageCursorVersion {
		return findCursor{}, invalidCursor("has version %d; expected %d", cursor.Version, pageCursorVersion)
	}
	if cursor.Tool != "find" {
		return findCursor{}, invalidCursor("belongs to %s, not find", cursor.Tool)
	}
	if !isSHA256(cursor.QuerySHA256) || !isSHA256(cursor.SnapshotSHA256) {
		return findCursor{}, invalidCursor("holds an invalid digest")
	}
	if cursor.QuerySHA256 != querySHA256 {
		return findCursor{}, invalidCursor("does not match this effective query; repeat it or restart without cursor")
	}
	if cursor.Position == (services.FindPosition{}) {
		return findCursor{}, invalidCursor("holds an empty position")
	}
	cursor.continued = true
	return cursor, nil
}

func (cursor findCursor) bindSnapshot(snapshotSHA256 string) error {
	if cursor.continued && cursor.SnapshotSHA256 != snapshotSHA256 {
		return fmt.Errorf("%w: cursor is stale because the result changed; restart without cursor", core.ErrConfig)
	}
	return nil
}

func (cursor findCursor) next(snapshotSHA256 string, position services.FindPosition) (string, error) {
	if !isSHA256(snapshotSHA256) || position == (services.FindPosition{}) {
		return "", fmt.Errorf("encode the find cursor: invalid snapshot or position")
	}
	next := findCursor{
		Version: pageCursorVersion, Tool: "find", QuerySHA256: cursor.QuerySHA256,
		SnapshotSHA256: snapshotSHA256, Position: position,
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return "", fmt.Errorf("encode the find cursor: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(encoded)
	if len(token) > maxFindCursorBytes {
		return "", fmt.Errorf("%w: find continuation cursor is %d bytes, over the %d-byte MCP response bound",
			core.ErrFileTooLarge, len(token), maxFindCursorBytes)
	}
	return token, nil
}
