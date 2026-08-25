package mcpserver

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

func TestFindCursorBindsQuerySnapshotAndSemanticPosition(t *testing.T) {
	query := struct {
		Grep  []string `json:"grep"`
		Limit int      `json:"limit"`
	}{Grep: []string{"needle"}, Limit: 10}
	first, err := openFindCursor("", query)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("a", 64)
	position := services.FindPosition{
		Phase: services.FindPhaseRecord, Time: "2026-08-25T12:00:00Z", URI: "index/items.json#42",
	}
	raw, err := first.next(snapshot, position)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := openFindCursor(raw, query)
	if err != nil {
		t.Fatal(err)
	}
	if !continued.continued || continued.Position != position {
		t.Fatalf("continued find cursor = %+v, want position %+v", continued, position)
	}
	if err := continued.bindSnapshot(snapshot); err != nil {
		t.Fatalf("same snapshot was refused: %v", err)
	}
	if err := continued.bindSnapshot(strings.Repeat("b", 64)); !errors.Is(err, core.ErrConfig) ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed snapshot error = %v, want a stale configuration error", err)
	}
	changed := query
	changed.Limit = 5
	if _, err := openFindCursor(raw, changed); !errors.Is(err, core.ErrConfig) {
		t.Fatalf("changed-query error = %v, want a configuration error", err)
	}
}

func TestFindCursorAcceptsEverySuccessfulMaximumComponentContinuation(t *testing.T) {
	query := struct {
		Limit int `json:"limit"`
	}{Limit: 1}
	first, err := openFindCursor("", query)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("a", 64)
	positions := []services.FindPosition{
		{
			Phase: services.FindPhaseRecord,
			Time:  "2026-08-26T12:00:00Z",
			URI: "events/2026-08-26/" + strings.Repeat("s", core.MaxSourceNameLength) +
				".json#record",
		},
		{
			Phase: services.FindPhasePage,
			Score: 1,
			URI:   "wiki/" + strings.Repeat("p", 255-len(core.MarkdownExtension)) + core.MarkdownExtension,
		},
	}
	for _, position := range positions {
		raw, err := first.next(snapshot, position)
		if err != nil {
			t.Fatalf("next(%s): %v", position.Phase, err)
		}
		if len(raw) <= maxCursorBytes {
			t.Fatalf("%s cursor = %d bytes, want regression to exceed the fixed offset-cursor bound %d",
				position.Phase, len(raw), maxCursorBytes)
		}
		continued, err := openFindCursor(raw, query)
		if err != nil {
			t.Fatalf("openFindCursor(%s maximum component): %v", position.Phase, err)
		}
		if continued.Position != position {
			t.Fatalf("continued %s position = %+v, want %+v", position.Phase, continued.Position, position)
		}
	}
}

func TestFindCursorRejectsEmptyAndUnknownPositions(t *testing.T) {
	query := struct {
		Limit int `json:"limit"`
	}{Limit: 10}
	queryDigest, err := jsonSHA256(query)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("a", 64)
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	for _, raw := range []string{
		encode(`{"v":1,"tool":"find","query_sha256":"` + queryDigest + `","snapshot_sha256":"` + snapshot + `","position":{}}`),
		encode(`{"v":1,"tool":"find","query_sha256":"` + queryDigest + `","snapshot_sha256":"` + snapshot + `","position":{"phase":"record","uri":"index/x.json#1","extra":true}}`),
	} {
		if _, err := openFindCursor(raw, query); !errors.Is(err, core.ErrConfig) {
			t.Fatalf("openFindCursor(%q) error = %v, want a configuration error", raw, err)
		}
	}
}

func TestFindCursorRejectsInputBeyondTheMCPResponseBound(t *testing.T) {
	query := struct {
		Limit int `json:"limit"`
	}{Limit: 1}
	if _, err := openFindCursor(strings.Repeat("a", maxFindCursorBytes+1), query); !errors.Is(err, core.ErrConfig) {
		t.Fatalf("oversized find cursor error = %v, want a configuration error", err)
	}
}

func TestPageCursorBindsQueryToolAndSnapshot(t *testing.T) {
	query := struct {
		Layer string `json:"layer"`
		Limit int    `json:"limit"`
	}{Layer: "wiki", Limit: 10}
	first, err := openPageCursor("", "list", query)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("a", 64)
	raw, err := first.next(snapshot, 10)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := openPageCursor(raw, "list", query)
	if err != nil {
		t.Fatal(err)
	}
	if continued.Offset != 10 || !continued.continued {
		t.Fatalf("cursor = %+v, want a continuation at offset 10", continued)
	}
	if err := continued.bindSnapshot(snapshot); err != nil {
		t.Fatalf("same snapshot was refused: %v", err)
	}
	if err := continued.bindSnapshot(strings.Repeat("b", 64)); !errors.Is(err, core.ErrConfig) ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed snapshot error = %v, want a stale configuration error", err)
	}
	if _, err := openPageCursor(raw, "find", query); !errors.Is(err, core.ErrConfig) {
		t.Fatalf("wrong-tool error = %v, want a configuration error", err)
	}
	changed := query
	changed.Limit = 5
	if _, err := openPageCursor(raw, "list", changed); !errors.Is(err, core.ErrConfig) {
		t.Fatalf("changed-query error = %v, want a configuration error", err)
	}
}

func TestPageCursorRejectsMalformedTokens(t *testing.T) {
	query := struct {
		Limit int `json:"limit"`
	}{Limit: 10}
	validQuery, err := jsonSHA256(query)
	if err != nil {
		t.Fatal(err)
	}
	validSnapshot := strings.Repeat("a", 64)
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	for _, test := range []struct {
		name, raw string
	}{
		{name: "base64", raw: "%%%"},
		{name: "oversized", raw: strings.Repeat("a", maxCursorBytes+1)},
		{name: "unknown field", raw: encode(`{"v":1,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":10,"extra":true}`)},
		{name: "trailing value", raw: encode(`{"v":1,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":10} {}`)},
		{name: "version", raw: encode(`{"v":2,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":10}`)},
		{name: "digest", raw: encode(`{"v":1,"tool":"list","query_sha256":"no","snapshot_sha256":"` + validSnapshot + `","offset":10}`)},
		{name: "offset", raw: encode(`{"v":1,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":0}`)},
		{name: "negative offset", raw: encode(`{"v":1,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":-1}`)},
		{name: "overflowing offset", raw: encode(`{"v":1,"tool":"list","query_sha256":"` + validQuery + `","snapshot_sha256":"` + validSnapshot + `","offset":999999999999999999999999}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := openPageCursor(test.raw, "list", query); !errors.Is(err, core.ErrConfig) {
				t.Fatalf("openPageCursor() error = %v, want a configuration error", err)
			}
		})
	}
}
