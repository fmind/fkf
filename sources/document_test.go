package sources_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func TestEncodeFragmentRoundTrip(t *testing.T) {
	// The characters that read naturally in an owner/name or an address stay literal; a
	// fragment is meant to be pasteable, not opaque.
	for _, id := range []string{
		"18c2a9f", "412", "fmind/fkf", "marc@example.test", "a.b_c:d+e-f",
		"has space", "has#hash", "has?question", "has%percent", "héllo",
	} {
		encoded := sources.EncodeFragment(id)
		if strings.ContainsAny(encoded, " #?") {
			t.Fatalf("EncodeFragment(%q) = %q, want the delimiters escaped", id, encoded)
		}
		decoded, err := sources.DecodeFragment(encoded)
		if err != nil {
			t.Fatalf("DecodeFragment(%q) error = %v", encoded, err)
		}
		if decoded != id {
			t.Fatalf("round trip of %q = %q", id, decoded)
		}
	}
	// A slash survives unescaped, which is what keeps `#fmind/fkf` readable in a URI.
	if got := sources.EncodeFragment("fmind/fkf"); got != "fmind/fkf" {
		t.Fatalf("EncodeFragment(\"fmind/fkf\") = %q, want it kept literal", got)
	}
	if _, err := sources.DecodeFragment("bad%zz"); err == nil {
		t.Fatal("DecodeFragment() must refuse an invalid escape")
	}
	if _, err := sources.DecodeFragment("truncated%"); err == nil {
		t.Fatal("DecodeFragment() must refuse a truncated escape")
	}
}

func TestDecodeDocumentContextStopsBeforeParsing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sources.DecodeDocumentContext(ctx, []byte(`{"fkf":1}`), "x.json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decode error = %v, want context.Canceled", err)
	}
}

func TestDocumentURIs(t *testing.T) {
	if got := sources.EventDocumentURI("2026-05-04", "google-gmail-emails"); got != "events/2026-05-04/google-gmail-emails.json" {
		t.Fatalf("EventDocumentURI() = %q", got)
	}
	if got := sources.IndexDocumentURI("github-repositories"); got != "index/github-repositories.json" {
		t.Fatalf("IndexDocumentURI() = %q", got)
	}
}

// TestDecodeDocumentRefusesAnotherGeneration freezes the v1 evidence envelope. An unknown
// marker needs a matching reader; recollection is not a compatibility strategy because the
// provider may no longer retain the evidence.
func TestDecodeDocumentRefusesAnotherGeneration(t *testing.T) {
	_, err := sources.DecodeDocument([]byte(`{"fkf": 99, "source": "s", "records": []}`), "x.json")
	if !errors.Is(err, sources.ErrUnknownSchema) {
		t.Fatalf("DecodeDocument() error = %v, want ErrUnknownSchema", err)
	}
	if !strings.Contains(err.Error(), "unsupported evidence envelope") || strings.Contains(err.Error(), "sync --force") {
		t.Fatalf("error = %v, want a matching-reader remedy and no recollection advice", err)
	}
}

func TestV1EvidenceEnvelopeAcceptsAdditiveFieldsAndNeedsNoExecutionMetadata(t *testing.T) {
	stored := `{
	"fkf": 1,
  "source": "repos",
  "layer": "index",
  "collected_at": "2026-08-25T12:00:00Z",
  "schema": {"id": {"description": "Stable identity.", "cardinality": "one"}},
  "fields": {"id": ".id"},
  "body": false,
  "count": 1,
  "records": [{"id": "fmind/fkf"}],
  "added_by_a_future_v1_writer": {"kept_out_of_the_reader_contract": true}
}`
	document, err := sources.DecodeDocument([]byte(stored), "index/repos.json")
	if err != nil {
		t.Fatalf("DecodeDocument() error = %v, want additive fields tolerated", err)
	}
	if err := sources.VerifyRecords(document); err != nil {
		t.Fatalf("VerifyRecords() error = %v, want evidence independent of a stored command", err)
	}
}

func TestDecodeDocumentRefusesTrailingJSON(t *testing.T) {
	valid := `{"fkf":1,"source":"s","layer":"events","records":[]}`
	for name, suffix := range map[string]string{
		"second document": `{"fkf":1}`,
		"malformed data":  `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sources.DecodeDocument([]byte(valid+suffix), "events/2026-05-04/s.json")
			if err == nil {
				t.Fatal("DecodeDocument() accepted bytes after the stored JSON document")
			}
			if !strings.Contains(err.Error(), "trailing JSON") {
				t.Fatalf("DecodeDocument() error = %v, want it to name trailing JSON", err)
			}
		})
	}
}

func TestDocumentRoundTripsThroughDisk(t *testing.T) {
	source := mustSource(t, logSource)
	runner := &fakeRunner{stdout: `[{"id":"a1","t":"2026-05-04T09:00:00Z","subject":"s","repo":"o/r","who":"m@x.test","big":9007199254740993}]`}
	document, err := sources.Collect(t.Context(), runner, source, testEnvironment(t),
		sources.DayWindow(testDay), time.Minute, testDay)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.json")
	if err := sources.WriteDocument(path, document); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != core.BaseFileMode {
		t.Fatalf("a collected document is owner-only, got %o", mode)
	}
	loaded, err := sources.ReadDocument(path)
	if err != nil {
		t.Fatalf("ReadDocument() error = %v", err)
	}
	// A nineteen-digit id has to survive: float64 would round it and the URI would change.
	if got, _ := loaded.Fields.EvalString(core.FieldID, map[string]any(loaded.Records[0])); got != "a1" {
		t.Fatalf("id after a round trip = %q", got)
	}
	if big, _ := core.ScalarString(loaded.Records[0]["big"]); big != "9007199254740993" {
		t.Fatalf("big number after a round trip = %q, want exact", big)
	}
	first, _ := sources.EncodeDocument(document)
	second, _ := sources.EncodeDocument(loaded)
	if string(first) != string(second) {
		t.Fatal("a document must encode byte-identically after a disk round trip")
	}
}

func TestWriteDocumentRefusesAnUnreadableEncodedSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	document := &sources.Document{
		Records: []sources.Record{{"payload": strings.Repeat("x", int(core.MaxSourceDocumentBytes))}},
	}

	err := sources.WriteDocument(path, document)
	if !errors.Is(err, core.ErrFileTooLarge) {
		t.Fatalf("WriteDocument() error = %v, want ErrFileTooLarge", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized document was persisted despite the refusal: %v", statErr)
	}
}

func TestParseRecordTime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-05-04T09:00:00Z", "2026-05-04T09:00:00Z"},
		{"2026-05-04T11:00:00+02:00", "2026-05-04T09:00:00Z"},
		{"2026-05-04", "2026-05-04T00:00:00Z"},
		// Provider and local-store clocks commonly expose all four Unix precisions.
		{"1777928400000", "2026-05-04T21:00:00Z"},
		{"1777928400", "2026-05-04T21:00:00Z"},
		{"1777928400000000", "2026-05-04T21:00:00Z"},
		{"1777928400000000000", "2026-05-04T21:00:00Z"},
	}
	for _, test := range cases {
		got, err := sources.ParseRecordTime(test.in)
		if err != nil {
			t.Fatalf("ParseRecordTime(%q) error = %v", test.in, err)
		}
		if got.Format(time.RFC3339) != test.want {
			t.Fatalf("ParseRecordTime(%q) = %s, want %s", test.in, got.Format(time.RFC3339), test.want)
		}
	}
	for _, invalid := range []string{
		"", "   ", "last tuesday", "May 4th",
		"2026-05-04T09:00:00", "2026-05-04 09:00:00",
	} {
		if _, err := sources.ParseRecordTime(invalid); err == nil {
			t.Fatalf("ParseRecordTime(%q) succeeded; an invented timestamp is worse than a failed day", invalid)
		}
	}
}

func TestDayWindowIsHalfOpenInUTC(t *testing.T) {
	window := sources.DayWindow(time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC))
	if window.Date != "2026-05-04" || window.Next != "2026-05-05" {
		t.Fatalf("window dates = %q, %q", window.Date, window.Next)
	}
	if window.Start != "2026-05-04T00:00:00Z" || window.End != "2026-05-05T00:00:00Z" {
		t.Fatalf("window bounds = %q .. %q, want a half-open UTC day", window.Start, window.End)
	}
}

// TestEncodeFragmentEscapesLowBytesAsTwoDigits pins the escape width. %X on a byte below 0x10
// emitted a single digit — "%A" for a newline — which is not a percent-escape at all, so
// DecodeFragment could not read back a URI fkf itself had printed and the record was
// unaddressable through the grammar the whole design rests on.
func TestEncodeFragmentEscapesLowBytesAsTwoDigits(t *testing.T) {
	for _, id := range []string{"a\nb", "a\tb", "\x01\x02", "sp ace"} {
		encoded := sources.EncodeFragment(id)
		decoded, err := sources.DecodeFragment(encoded)
		if err != nil {
			t.Fatalf("DecodeFragment(%q) error = %v", encoded, err)
		}
		if decoded != id {
			t.Errorf("round trip of %q through %q = %q", id, encoded, decoded)
		}
	}
}

// TestDecodeDocumentRefusesALayerThisBuildDoesNotKnow pins the gap the schema marker alone left
// open: URI() answers "events/..." for any layer that is not the index one, so a document
// carrying a retired spelling decoded cleanly and then addressed itself at a path it had never
// been written to. An unrecognised layer has to fail the way an unrecognised marker does.
func TestDecodeDocumentRefusesALayerThisBuildDoesNotKnow(t *testing.T) {
	stored := `{"fkf":1,"source":"github-repos","layer":"inventory","records":[]}`

	_, err := sources.DecodeDocument([]byte(stored), "index/github-repos.json")
	if err == nil {
		t.Fatal("DecodeDocument() accepted a layer this build does not write")
	}
	if !errors.Is(err, sources.ErrUnknownSchema) {
		t.Fatalf("DecodeDocument() error = %v, want it to be an unknown-schema error", err)
	}
	if !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("DecodeDocument() error = %v, want it to name the layer it refused", err)
	}
}
