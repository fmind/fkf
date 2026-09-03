package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"
	"unicode"

	"github.com/fmind/fkf/core"
)

// A successful collection is complete. A non-zero exit, a timeout, output over the bound,
// undecodable output, a record with no identity, or an event record with no parseable timestamp
// fails the whole day and writes nothing. There is deliberately no partial state and no Skip:
// a day is collected or it is absent, and `status` says which.

// ErrIncomplete marks every completeness failure, so `sync` can report the day as failed
// without inspecting messages.
var ErrIncomplete = errors.New("collection is incomplete")

// Collect runs one source for one window and returns the document it would file. It writes
// nothing: the caller decides where a complete document goes, which is what keeps the runner
// pure enough to golden-test.
func Collect(
	ctx context.Context, runner Runner, source *core.Source,
	env Environment, window Window, timeout time.Duration, now time.Time,
) (*Document, error) {
	command, err := BuildRunCommand(source, env, window, timeout)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	stdout, err := runner.Run(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	records, err := DecodeRecords(source, stdout)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	document := &Document{
		FKF: SchemaVersion, Source: source.Name, Layer: source.Layer,
		CollectedAt: now.UTC().Format(time.RFC3339),
		Schema:      SchemaOf(source), Fields: FieldsOf(source), Body: source.HasBody(),
		Count: len(records), Records: records,
	}
	if source.Layer == core.LayerEvents {
		document.Date = window.Date
		document.WindowStart = window.Start
		document.WindowEnd = window.End
	}
	if err := VerifyRecords(document); err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	if err := verifyCollectedTitles(source, records); err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	return document, nil
}

func verifyRecordsWithinWindow(fields core.FieldMap, records []Record, window Window) error {
	start, err := time.Parse(time.RFC3339, window.Start)
	if err != nil {
		return fmt.Errorf("requested window start %q is not RFC 3339: %w", window.Start, err)
	}
	end, err := time.Parse(time.RFC3339, window.End)
	if err != nil {
		return fmt.Errorf("requested window end %q is not RFC 3339: %w", window.End, err)
	}
	for index, record := range records {
		raw, ok := fields.EvalString(core.FieldTime, map[string]any(record))
		if !ok {
			return fmt.Errorf("record %d has no value at the declared fields.time paths %v",
				index, fields.Paths(core.FieldTime))
		}
		if date, dateOnly := civilDateValue(raw); dateOnly {
			if date != window.Date {
				return fmt.Errorf("record %d has civil date %s outside the requested window [%s, %s)",
					index, date, window.Start, window.End)
			}
			continue
		}
		at, err := ParseRecordTime(raw)
		if err != nil {
			return fmt.Errorf("record %d: %w", index, err)
		}
		if at.Before(start) || !at.Before(end) {
			return fmt.Errorf("record %d has time %s outside the requested window [%s, %s)",
				index, at.Format(time.RFC3339), window.Start, window.End)
		}
	}
	return nil
}

func civilDateValue(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if len(value) != len("2006-01-02") {
		return "", false
	}
	parsed, err := time.Parse(time.DateOnly, value)
	return value, err == nil && parsed.Format(time.DateOnly) == value
}

// CollectWindow runs a `window: true` source ONCE for the whole requested range and partitions
// what it returns into one Document per requested day, bucketed by each record's own declared
// `fields.time`. It is the range analogue of Collect: the same completeness rule applies to the
// whole range rather than to one day, because the command that produced every record ran once
// — a record whose time falls outside every requested day fails the range entirely, the same
// way a record with no identity fails a day today.
//
// dates must be requested in ascending order and represent a contiguous, gap-free span: the
// caller (services/sync.go) derives {{start}}/{{end}} from the first and last day, so a day
// silently missing from the middle of dates would still be covered by the command's own
// output and its records would have nowhere to land.
func CollectWindow(
	ctx context.Context, runner Runner, source *core.Source,
	env Environment, rangeWindow Window, dates []string, timeout time.Duration, now time.Time,
) (map[string]*Document, error) {
	command, err := BuildRunCommand(source, env, rangeWindow, timeout)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	stdout, err := runner.Run(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	records, err := DecodeRecords(source, stdout)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	loc := now.Location()
	buckets, err := bucketRecordsByDay(source, records, dates, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
	}
	documents := make(map[string]*Document, len(dates))
	for _, date := range dates {
		recs := buckets[date]
		if recs == nil {
			recs = []Record{}
		}
		window, err := eventWindowInLocation(date, loc)
		if err != nil {
			return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
		}
		document := &Document{
			FKF: SchemaVersion, Source: source.Name, Layer: source.Layer, Date: date,
			WindowStart: window.Start, WindowEnd: window.End,
			CollectedAt: now.UTC().Format(time.RFC3339),
			Schema:      SchemaOf(source), Fields: FieldsOf(source), Body: source.HasBody(),
			Count: len(recs), Records: recs,
		}
		if err := VerifyRecords(document); err != nil {
			return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
		}
		if err := verifyCollectedTitles(source, recs); err != nil {
			return nil, fmt.Errorf("%w: source %s: %w", ErrIncomplete, source.Name, err)
		}
		documents[date] = document
	}
	return documents, nil
}

// verifyCollectedTitles applies the new subject-line contract only to sources that declare
// the title projection. Historical v1 documents without it remain readable: the permanent
// evidence envelope cannot be made contingent on provider retention or forced recollection.
func verifyCollectedTitles(source *core.Source, records []Record) error {
	if source.Fields.Path(core.FieldTitle).IsZero() {
		return nil
	}
	for index, record := range records {
		title, ok := source.Fields.EvalString(core.FieldTitle, map[string]any(record))
		if !ok || strings.TrimSpace(title) == "" {
			return fmt.Errorf("record %d has no meaningful title at the declared fields.title paths %v",
				index, source.Fields.Paths(core.FieldTitle))
		}
		for _, char := range title {
			if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
				return fmt.Errorf("record %d title contains control or invisible character U+%04X", index, char)
			}
		}
	}
	return nil
}

// bucketRecordsByDay sorts a windowed source's records into the requested days by each
// record's own value at the source's declared `fields.time` path. A day's boundary is computed the
// same DST-safe way DayWindow computes the command's own {{start}}/{{end}}, so a record is
// bucketed by exactly the rule that decided which command produced it.
type collectionDayBound struct {
	date       string
	start, end time.Time
}

func collectionDayBounds(dates []string, loc *time.Location) ([]collectionDayBound, error) {
	bounds := make([]collectionDayBound, 0, len(dates))
	for _, date := range dates {
		day, err := ParseDayInLocation(date, loc)
		if err != nil {
			return nil, fmt.Errorf("requested day %q: %w", date, err)
		}
		window := DayWindow(day)
		start, err := time.Parse(time.RFC3339, window.Start)
		if err != nil {
			return nil, err
		}
		end, err := time.Parse(time.RFC3339, window.End)
		if err != nil {
			return nil, err
		}
		bounds = append(bounds, collectionDayBound{date: date, start: start, end: end})
	}
	return bounds, nil
}

func bucketRecordsByDay(
	source *core.Source, records []Record, dates []string, loc *time.Location,
) (map[string][]Record, error) {
	bounds, err := collectionDayBounds(dates, loc)
	if err != nil {
		return nil, err
	}
	buckets := make(map[string][]Record, len(dates))
	for index, record := range records {
		date, err := recordCollectionDate(source, record, index, bounds, dates)
		if err != nil {
			return nil, err
		}
		buckets[date] = append(buckets[date], record)
	}
	return buckets, nil
}

func recordCollectionDate(
	source *core.Source,
	record Record,
	index int,
	bounds []collectionDayBound,
	dates []string,
) (string, error) {
	raw, ok := source.Fields.EvalString(core.FieldTime, map[string]any(record))
	if !ok {
		return "", fmt.Errorf("record %d has no value at the declared fields.time paths %v",
			index, source.Fields.Paths(core.FieldTime))
	}
	if date, dateOnly := civilDateValue(raw); dateOnly {
		for _, candidate := range bounds {
			if date == candidate.date {
				return candidate.date, nil
			}
		}
		return "", fmt.Errorf("record %d has civil date %s, which falls outside the requested window %s..%s",
			index, date, dates[0], dates[len(dates)-1])
	}
	at, err := ParseRecordTime(raw)
	if err != nil {
		return "", fmt.Errorf("record %d: %w", index, err)
	}
	for _, candidate := range bounds {
		if !at.Before(candidate.start) && at.Before(candidate.end) {
			return candidate.date, nil
		}
	}
	return "", fmt.Errorf("record %d has time %s, which falls outside the requested window %s..%s "+
		"(a record whose time is outside every requested day fails the whole range, "+
		"the way a missing identity fails a day today)",
		index, at.Format(time.RFC3339), dates[0], dates[len(dates)-1])
}

// VerifyRecords enforces the rules that make a record addressable: it has the identity its
// source declared, that identity is unique within the document, and — for a dated document —
// a timestamp fkf can read and that belongs to the civil day the document declares.
func VerifyRecords(document *Document) error {
	if err := verifyDocumentDefinition(document); err != nil {
		return err
	}
	seen := make(map[string]int, len(document.Records))
	for index, record := range document.Records {
		id, err := verifyRecord(document, record, index)
		if err != nil {
			return err
		}
		// A URI names a record by its declared identity. Duplicate identities would leave
		// one record unreachable, so the whole collected day is invalid.
		if first, duplicate := seen[id]; duplicate {
			return fmt.Errorf("records %d and %d share the id %q at fields.id paths %v; a record URI must name exactly one record",
				first, index, id, document.Fields.Paths(core.FieldID))
		}
		seen[id] = index
	}
	if document.Layer != core.LayerEvents {
		return nil
	}
	window, err := documentEventWindow(document)
	if err != nil {
		return err
	}
	return verifyRecordsWithinWindow(document.Fields, document.Records, window)
}

func eventWindow(date string) (Window, error) {
	return eventWindowInLocation(date, time.Local)
}

func eventWindowInLocation(date string, loc *time.Location) (Window, error) {
	day, err := ParseDayInLocation(date, loc)
	if err != nil {
		return Window{}, fmt.Errorf("event document date %q: %w", date, err)
	}
	return DayWindow(day), nil
}

func documentEventWindow(document *Document) (Window, error) {
	if document.WindowStart == "" && document.WindowEnd == "" {
		// Bounds were added to the permanent additive envelope after some evidence had already
		// been collected. Those documents cannot recover their original zone, so retain the
		// only honest fallback: interpret their civil date in the reader's current local zone.
		return eventWindow(document.Date)
	}
	if document.WindowStart == "" || document.WindowEnd == "" {
		return Window{}, errors.New("event document must declare both window_start and window_end or neither")
	}
	start, err := parseCanonicalUTCBound("window_start", document.WindowStart)
	if err != nil {
		return Window{}, err
	}
	end, err := parseCanonicalUTCBound("window_end", document.WindowEnd)
	if err != nil {
		return Window{}, err
	}
	if !start.Before(end) {
		return Window{}, fmt.Errorf("event document window [%s, %s) is empty or reversed", document.WindowStart, document.WindowEnd)
	}
	// A civil day can be 23, 24, 25, or a fractional-hour variant, and a historical date-line
	// move can repeat nearly a whole day. The collector's boundary search admits shifts up to
	// four hours; [20h, 48h] therefore keeps those real intervals while refusing a hand-edited
	// arbitrary range that happens to contain every record.
	if span := end.Sub(start); span < 20*time.Hour || span > 48*time.Hour {
		return Window{}, fmt.Errorf("event document window spans %s; a civil day must span 20h..48h", span)
	}
	date, err := time.Parse(time.DateOnly, document.Date)
	if err != nil {
		return Window{}, fmt.Errorf("event document date %q is not YYYY-MM-DD: %w", document.Date, err)
	}
	// UTC bounds can sit on either side of their local midnight. Every real civil offset and
	// the midnight-gap search fit inside 18 hours, so this check binds the interval to the
	// declared date without pretending the additive envelope retained an IANA zone name.
	if !plausibleCivilBoundary(start, date) || !plausibleCivilBoundary(end, date.AddDate(0, 0, 1)) {
		return Window{}, fmt.Errorf("event document window [%s, %s) is not aligned with civil date %s",
			document.WindowStart, document.WindowEnd, document.Date)
	}
	return Window{Date: document.Date, Start: document.WindowStart, End: document.WindowEnd}, nil
}

func parseCanonicalUTCBound(name, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("event document %s %q is not RFC 3339: %w", name, value, err)
	}
	if canonical := parsed.UTC().Format(time.RFC3339); value != canonical {
		return time.Time{}, fmt.Errorf("event document %s %q is not canonical UTC; want %q", name, value, canonical)
	}
	return parsed, nil
}

func plausibleCivilBoundary(boundary, utcMidnight time.Time) bool {
	const maximumOffsetAndGap = 18 * time.Hour
	delta := boundary.Sub(utcMidnight)
	return delta >= -maximumOffsetAndGap && delta <= maximumOffsetAndGap
}

func verifyDocumentDefinition(document *Document) error {
	if strings.TrimSpace(document.Source) == "" {
		return errors.New("document declares no source")
	}
	if err := core.ValidateSourceName(document.Source); err != nil {
		return fmt.Errorf("document source %q: %w", document.Source, err)
	}
	if _, err := time.Parse(time.RFC3339, document.CollectedAt); err != nil {
		return fmt.Errorf("document collected_at %q is not RFC 3339: %w", document.CollectedAt, err)
	}
	if err := core.ValidateFieldMap(document.Fields, document.Layer == core.LayerEvents); err != nil {
		return fmt.Errorf("document field map: %w", err)
	}
	if err := core.ValidateFieldSchema(document.Schema); err != nil {
		return fmt.Errorf("document schema: %w", err)
	}
	if len(document.Schema) != len(document.Fields) {
		return fmt.Errorf("document schema declares %d fields but the field map uses %d", len(document.Schema), len(document.Fields))
	}
	for _, name := range document.Fields.Names() {
		if _, declared := document.Schema[name]; !declared {
			return fmt.Errorf("document field %s is not declared in its schema", name)
		}
	}
	if document.Layer == core.LayerEvents {
		if parsed, err := time.Parse(time.DateOnly, document.Date); err != nil || parsed.Format(time.DateOnly) != document.Date {
			return fmt.Errorf("event document date %q is not YYYY-MM-DD", document.Date)
		}
	} else if document.Date != "" {
		return fmt.Errorf("index document declares date %q; an index is a point-in-time snapshot", document.Date)
	} else if document.WindowStart != "" || document.WindowEnd != "" {
		return errors.New("index document declares an event collection window")
	}
	return nil
}

func verifyRecord(document *Document, record Record, index int) (string, error) {
	values := map[string]any(record)
	for _, name := range document.Fields.Names() {
		definition := document.Schema[name]
		projected, err := document.Fields.EvalDeclaredField(name, values, definition)
		if err != nil {
			return "", fmt.Errorf("record %d field %s: %w", index, name, err)
		}
		if !definition.Cardinality.Allows(len(projected)) {
			return "", fmt.Errorf("record %d field %s projects %d values; schema cardinality %s", index, name, len(projected), definition.Cardinality)
		}
		if err := verifyRelationValues(name, definition, projected); err != nil {
			return "", fmt.Errorf("record %d: %w", index, err)
		}
	}
	id, ok := document.Fields.EvalString(core.FieldID, values)
	if !ok {
		return "", fmt.Errorf("record %d has no value at the declared fields.id paths %v",
			index, document.Fields.Paths(core.FieldID))
	}
	return id, nil
}

func verifyRelationValues(name string, definition core.FieldDefinition, values []string) error {
	if !definition.Relation {
		return nil
	}
	for _, value := range values {
		if err := core.ValidateRelationValue(value); err != nil {
			return fmt.Errorf("field %s value %q is not a canonical relation URI: %w", name, value, err)
		}
	}
	return nil
}

// DecodeRecords turns one command's stdout into records under the source's declared format.
//
// The empty-output rule differs by format on purpose. A CLI invoked for one JSON document
// prints `[]` for an empty result, so silence means the command was cut short and must fail.
// A paginating CLI streaming NDJSON legitimately prints nothing for a day that held nothing,
// so silence there is an empty day.
func DecodeRecords(source *core.Source, stdout string) ([]Record, error) {
	if strings.TrimSpace(stdout) == "" {
		if source.Format == core.FormatNDJSON {
			return []Record{}, nil
		}
		return nil, errors.New("command exited zero but printed nothing; a CLI emitting json prints [] for an empty result")
	}
	if source.Format == core.FormatNDJSON {
		return decodeNDJSON(source, stdout)
	}
	return decodeJSON(source, stdout)
}

func decodeJSON(source *core.Source, stdout string) ([]Record, error) {
	value, err := decodeValue(stdout)
	if err != nil {
		return nil, err
	}
	return recordsFrom(source, value, "output", true)
}

func decodeNDJSON(source *core.Source, stdout string) ([]Record, error) {
	records := []Record{}
	for number, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		value, err := decodeValue(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		page, err := recordsFrom(source, value, fmt.Sprintf("line %d", number+1), false)
		if err != nil {
			return nil, err
		}
		records = append(records, page...)
	}
	return records, nil
}

func decodeValue(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w", err)
	}
	// A `--paginate` CLI prints one JSON document per page, concatenated. Decoding only the
	// first would file page 1 as a complete day and lose the rest with no error, which is the
	// exact opposite of "a successful collection is complete". Trailing noise — a warning
	// printed after the payload — is refused for the same reason.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New(
			"output holds more than one JSON document: set `format: ndjson` if the command prints one per line, " +
				"or `records: <path>` if it prints a paginated envelope")
	}
	return value, nil
}

// recordsFrom extracts the record list from one decoded value.
//
// Without `records:` the rule differs by format, because the two formats mean different things
// by "one value". A `json` command prints ONE document, so it must be an array — an object at
// the top level is a paginated envelope, and accepting it as a single record would file one
// page-shaped "record" per day and look like it worked. An `ndjson` command prints one value per
// LINE, so each line is already a record, which is exactly what `gh api --jq '.[]'` emits.
//
// With `records:` the path selects the array, and a path that selects the records one by one is
// accepted too, because `.items[]` is the natural thing to write.
func recordsFrom(source *core.Source, value any, origin string, wholeDocument bool) ([]Record, error) {
	if source.Records.IsZero() {
		switch typed := value.(type) {
		case []any:
			return objectRecords(typed, origin)
		case map[string]any:
			if wholeDocument {
				return nil, fmt.Errorf("%s: expected a JSON array of records, got an object"+
					" (declare `records:` when the command wraps them in an envelope)", origin)
			}
			return []Record{typed}, nil
		default:
			return nil, fmt.Errorf("%s: expected a JSON object or array, got %T", origin, value)
		}
	}
	selected := source.Records.Eval(value)
	if selected == nil {
		if wholeDocument {
			return nil, fmt.Errorf("%s: declared records path %s selected nothing", origin, source.Records)
		}
		// An absent terminal page is normal for an NDJSON-paginating CLI: the last page of a
		// quiet day can carry pagination metadata without the selected records field.
		return []Record{}, nil
	}
	records := []Record{}
	for _, item := range selected {
		switch typed := item.(type) {
		case []any:
			page, err := objectRecords(typed, origin)
			if err != nil {
				return nil, err
			}
			records = append(records, page...)
		case map[string]any:
			records = append(records, typed)
		default:
			return nil, fmt.Errorf("%s: %s selected a %T; a record must be a JSON object",
				origin, source.Records, item)
		}
	}
	return records, nil
}

func objectRecords(array []any, origin string) ([]Record, error) {
	records := make([]Record, 0, len(array))
	for _, element := range array {
		record, ok := element.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: a record must be a JSON object, got %T", origin, element)
		}
		records = append(records, record)
	}
	return records, nil
}

// FetchBody runs a source's `body:` command for one record. Every placeholder value comes
// from collected data, so each is checked against the charset before exec and the argv is
// passed to the kernel without a shell. The output is printed and never stored: a body is
// evidence to read once, not a second copy of the provider's database.
func FetchBody(
	ctx context.Context, runner Runner, source *core.Source,
	fields Fields, env Environment, record Record, timeout time.Duration,
) (string, Command, error) {
	if !source.HasBody() {
		return "", Command{}, fmt.Errorf("source %s declares no body: command, so its record bodies are not fetchable", source.Name)
	}
	values := map[string]string{"base": env.Root}
	if err := addHomePlaceholder(source.Body, values); err != nil {
		return "", Command{}, fmt.Errorf("plan body command for source %s: %w", source.Name, err)
	}
	for _, name := range source.Fields.Names() {
		// The execution placeholders keep their static meaning even when the open semantic map
		// happens to use the same name. Otherwise record data could replace a trusted path and
		// skip the collected-value validation below.
		if name == "base" || name == "home" {
			continue
		}
		if !argvUsesPlaceholder(source.Body, name) {
			continue
		}
		if _, declared := fields[name]; !declared {
			return "", Command{}, fmt.Errorf(
				"collected document declares no fields.%s mapping required by the current body: command; recollect the record before fetching its body",
				name,
			)
		}
		value, found := fields.EvalString(name, map[string]any(record))
		if !found {
			return "", Command{}, fmt.Errorf("record has no scalar value at the declared fields.%s path", name)
		}
		values[name] = value
	}
	for name, value := range values {
		if name == "base" || name == "home" {
			continue
		}
		if !core.ValidBodyValue(value) {
			return "", Command{}, fmt.Errorf(
				"refusing to run body: for source %s: the %s value %q is not a safe opaque argv value (valid UTF-8, 1..256 bytes, no leading '-' or '@', controls, or invisible format characters)",
				source.Name, name, value,
			)
		}
	}
	argv := make([]string, 0, len(source.Body))
	for _, element := range source.Body {
		argv = append(argv, substitute(element, values))
	}
	command := Command{
		Argv: argv, Dir: core.DeclaredCommandDirectory, ForbiddenRoot: env.Root,
		TrustConfig: env.TrustConfig,
		Env:         maps.Clone(env.Env), Timeout: timeout,
	}
	if source.Timeout > 0 {
		command.Timeout = source.Timeout
	}
	stdout, err := runner.Run(ctx, command)
	if err != nil {
		return "", command, fmt.Errorf("fetch body for source %s: %w", source.Name, err)
	}
	return stdout, command, nil
}

func argvUsesPlaceholder(argv []string, name string) bool {
	placeholder := "{{" + name + "}}"
	for _, element := range argv {
		if strings.Contains(element, placeholder) {
			return true
		}
	}
	return false
}
