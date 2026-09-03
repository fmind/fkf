package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

// SchemaVersion is the permanent v1 evidence-envelope marker. Configuration and stored
// documents deliberately use the same fkf: 1 marker value; the containing file identifies
// which contract applies.
// Evolution inside this marker is additive: older readers ignore fields they do not know, and
// newer readers accept documents that omit later optional fields. Generated caches remain
// disposable, but collected evidence must not depend on a provider retaining enough history to
// re-create it.
const SchemaVersion = 1

// ErrUnknownSchema reports a document from a different generation.
var ErrUnknownSchema = errors.New("unknown document schema")

// Record is one collected item. Every decoded field and value is retained without redaction,
// dropping, or inference; storage then uses the document's canonical indented JSON encoding,
// so provider whitespace, object-key order, and escape spellings are not a byte-level contract.
// Numbers use json.Number so even their lexical form survives a round trip that float64 would
// round.
type Record map[string]any

// Fields is the open field map a document carries. It travels with the records so a read never
// depends on the live fkf.yaml, and editing a path in configuration never rewrites history.
type Fields = core.FieldMap

// Schema is the semantic subset used by one document.
type Schema = core.FieldSchema

// FieldsOf projects a source's declared paths into the map stored with its documents.
func FieldsOf(source *core.Source) Fields {
	fields := make(Fields, len(source.Fields))
	for name, paths := range source.Fields {
		fields[name] = append(core.FieldPaths(nil), paths...)
	}
	return fields
}

// SchemaOf copies the definitions used by one source into the self-describing document.
func SchemaOf(source *core.Source) Schema {
	schema := make(Schema, len(source.Schema))
	for name, definition := range source.Schema {
		definition.Examples = append([]string(nil), definition.Examples...)
		schema[name] = definition
	}
	return schema
}

// Document is one complete collection: every record a source produced for one day, or the
// point-in-time snapshot of an index source. It is complete or absent, never partial.
type Document struct {
	FKF         int        `json:"fkf"`
	Source      string     `json:"source"`
	Layer       core.Layer `json:"layer"`
	Date        string     `json:"date,omitempty"`
	WindowStart string     `json:"window_start,omitempty"`
	WindowEnd   string     `json:"window_end,omitempty"`
	CollectedAt string     `json:"collected_at"`
	Schema      Schema     `json:"schema"`
	Fields      Fields     `json:"fields"`
	Body        bool       `json:"body"`
	Count       int        `json:"count"`
	Records     []Record   `json:"records"`
}

// URI is this document's base-relative address.
func (d *Document) URI() string {
	if d.Layer == core.LayerIndex {
		return IndexDocumentURI(d.Source)
	}
	return EventDocumentURI(d.Date, d.Source)
}

// RecordURI addresses one record by the identity the document itself declares — never by a
// position, so a URI stays valid across re-collection even when the order changes.
func (d *Document) RecordURI(record Record) (string, bool) {
	id, ok := d.Fields.EvalString(core.FieldID, map[string]any(record))
	if !ok {
		return "", false
	}
	return d.URI() + "#" + EncodeFragment(id), true
}

// FindRecord returns the record whose declared identity matches.
func (d *Document) FindRecord(id string) (Record, bool) {
	for _, record := range d.Records {
		if value, ok := d.Fields.EvalString(core.FieldID, map[string]any(record)); ok && value == id {
			return record, true
		}
	}
	return nil, false
}

// EventDocumentURI is where an events source's day is filed.
func EventDocumentURI(date, source string) string {
	return path.Join(string(core.LayerEvents), date, source+".json")
}

// IndexDocumentURI is where an index source's snapshot is filed.
func IndexDocumentURI(source string) string {
	return path.Join(string(core.LayerIndex), source+".json")
}

// EncodeFragment percent-encodes a record identity for a URI fragment, leaving the characters
// that read naturally in an owner/name or an email address. It is the inverse of DecodeFragment.
func EncodeFragment(id string) string {
	var builder strings.Builder
	for _, char := range []byte(id) {
		if isFragmentSafe(char) {
			builder.WriteByte(char)
			continue
		}
		// %02X, not %X: a byte below 0x10 rendered as a single digit ("%A" for a newline),
		// which is not a percent-escape at all — DecodeFragment could not read back a URI fkf
		// itself had printed, and so a record whose id held such a byte was unaddressable.
		_, _ = fmt.Fprintf(&builder, "%%%02X", char)
	}
	return builder.String()
}

func isFragmentSafe(char byte) bool {
	switch {
	case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		return true
	case strings.IndexByte("._:/@+-", char) >= 0:
		return true
	default:
		return false
	}
}

// DecodeFragment reverses EncodeFragment.
func DecodeFragment(fragment string) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(fragment); index++ {
		if fragment[index] != '%' {
			builder.WriteByte(fragment[index])
			continue
		}
		if index+2 >= len(fragment) {
			return "", fmt.Errorf("fragment %q ends in a truncated percent escape", fragment)
		}
		value, err := strconv.ParseUint(fragment[index+1:index+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("fragment %q holds an invalid percent escape: %w", fragment, err)
		}
		builder.WriteByte(byte(value))
		index += 2
	}
	return builder.String(), nil
}

// EncodeDocument renders a document for storage: two-space indented JSON with a trailing
// newline, byte-identical for identical input.
func EncodeDocument(document *Document) ([]byte, error) {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode document %s: %w", document.URI(), err)
	}
	return append(encoded, '\n'), nil
}

// DecodeDocument parses a stored document and refuses an unrecognised schema marker.
func DecodeDocument(data []byte, path string) (*Document, error) {
	return DecodeDocumentContext(context.Background(), data, path)
}

// DecodeDocumentContext is DecodeDocument with cooperative cancellation during JSON parsing.
// Stored documents are bounded but can be large enough that a cancelled status or retrieval
// request must stop before decoding all records.
func DecodeDocumentContext(ctx context.Context, data []byte, path string) (*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(&documentContextReader{ctx: ctx, reader: bytes.NewReader(data)})
	decoder.UseNumber()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("decode %s: trailing JSON holds more than one document", path)
	} else if !errors.Is(err, io.EOF) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("decode %s: invalid trailing JSON: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if document.FKF != SchemaVersion {
		return nil, fmt.Errorf("%w: %s declares unsupported evidence envelope fkf %d; use a build that reads marker %d",
			ErrUnknownSchema, path, document.FKF, document.FKF)
	}
	// A layer this build does not know must fail rather than fall through. URI() answers
	// "events" for anything that is not the index layer, so an unrecognised value does not
	// error — it silently addresses the document at a path it was never written to, which is
	// the one failure a schema marker exists to make impossible.
	if document.Layer != core.LayerEvents && document.Layer != core.LayerIndex {
		return nil, fmt.Errorf("%w: %s declares layer %q; a stored document is filed under %s or %s",
			ErrUnknownSchema, path, document.Layer, core.LayerEvents, core.LayerIndex)
	}
	return &document, nil
}

type documentContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *documentContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// ReadDocumentContext loads and decodes one bounded stored document with cancellation.
func ReadDocumentContext(ctx context.Context, path string) (*Document, error) {
	data, err := core.ReadFileLimitContext(ctx, path, core.MaxSourceDocumentBytes)
	if err != nil {
		return nil, err
	}
	return DecodeDocumentContext(ctx, data, path)
}

// WriteDocument replaces a document atomically, so a reader sees the previous complete
// document or the new one and a crash never leaves a partial day that a skip-if-exists run
// would later treat as collected.
func WriteDocument(path string, document *Document) error {
	encoded, err := EncodeDocument(document)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > core.MaxSourceDocumentBytes {
		return fmt.Errorf("%w: encoded document %s is %d bytes (limit %d)",
			core.ErrFileTooLarge, path, len(encoded), core.MaxSourceDocumentBytes)
	}
	return core.WriteFileAtomicMode(path, encoded, core.BaseFileMode)
}

// recordTimeLayouts are the shapes a provider CLI actually emits. Anything outside them fails
// the day rather than being filed under an invented timestamp.
var recordTimeLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	// Jira's REST API — and therefore `acli jira workitem search --json` — emits the offset
	// without the colon RFC 3339 requires ("2026-08-22T10:15:30.000+0200"), so every collected
	// day of the shipped Jira source failed on a timestamp that was never malformed.
	"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700",
	"2006-01-02 15:04:05Z07:00", time.DateOnly,
}

// ParseRecordTime reads the timestamp at a source's declared `time` path. Epoch values are
// accepted because provider CLIs and local databases emit seconds, milliseconds, microseconds,
// and nanoseconds. Consecutive powers of 1000 are distinguishable by magnitude for every
// instant between 1973 and the year 5138.
func ParseRecordTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	for _, layout := range recordTimeLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		switch {
		case epoch >= 1e17 || epoch <= -1e17:
			return time.Unix(0, epoch).UTC(), nil
		case epoch >= 1e14 || epoch <= -1e14:
			return time.UnixMicro(epoch).UTC(), nil
		case epoch >= 1e11 || epoch <= -1e11:
			return time.UnixMilli(epoch).UTC(), nil
		default:
			return time.Unix(epoch, 0).UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"timestamp %q matches no known layout (RFC 3339 with an explicit offset, YYYY-MM-DD, or a Unix epoch)",
		value,
	)
}
