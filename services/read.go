package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/itchyny/gojq"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf read <uri>` is the one resolver for every form of the grammar. Every other read
// command is sugar over it, which is what keeps "everything has a URI" true rather than
// aspirational: if a command can show you something, `read` can address it.

// ErrJQExpression reports a `?jq=` expression gojq refused to parse or compile. The CLI maps
// it to exit code 2: a bad expression is the caller's, like a mistyped flag.
var ErrJQExpression = errors.New("invalid jq expression")

// jqTimeout bounds one selection in TIME. A jq expression over a bounded document is
// milliseconds; thirty seconds is a runaway guard, not a budget.
const jqTimeout = 30 * time.Second

// jqMaxOutputBytes bounds the same selection in SPACE, which the timeout does not. gojq streams
// values, and the loop below accumulated every one before returning: `?jq=range(50000000)` over
// a two-kilobyte document reached 7.2 GB of resident memory in twelve seconds and was still
// growing when the timeout would have fired. A read is supposed to be bounded, and the same
// expression is reachable through the MCP `read` tool, where the string comes from a connected
// agent rather than from the person at the terminal.
//
// The bound is on the encoded output rather than on a value count, because one value can be the
// whole document and a million can be integers. It matches MaxNarrativeBytes, the ceiling on
// what `fkf read` will hand back from a file, so a selection cannot return more than the
// document it selects from would have.
const jqMaxOutputBytes = int(core.MaxNarrativeBytes)

// ReadResult is what `fkf read` returns. Exactly one payload field is populated, and `uri`
// always names what was resolved, so an agent can cite the answer it just received.
type ReadResult struct {
	URI       string            `json:"uri"`
	Kind      string            `json:"kind"`
	Source    string            `json:"source,omitempty"`
	Date      string            `json:"date,omitempty"`
	Document  *sources.Document `json:"document,omitempty"`
	Record    sources.Record    `json:"record,omitempty"`
	Page      *Page             `json:"page,omitempty"`
	Text      string            `json:"text,omitempty"`
	Entries   []string          `json:"entries,omitempty"`
	Selection json.RawMessage   `json:"selection,omitempty"`
	Entity    *EntityView       `json:"entity,omitempty"`
	Body      string            `json:"body,omitempty"`
	BodyState string            `json:"body_state,omitempty"`
	// SnapshotSHA256 binds MCP entity continuation to the validated graph generation without
	// exposing an implementation detail in CLI or stored JSON.
	SnapshotSHA256 string `json:"-"`
}

// EntityView is what the base knows about any declared entity from stored graph evidence.
type EntityView struct {
	URI          string `json:"uri"`
	Scheme       Scheme `json:"scheme"`
	Value        string `json:"value"`
	Neighbours   []Edge `json:"neighbours"`
	NeighbourCap bool   `json:"neighbours_truncated,omitempty"`
}

// ReadOptions tunes one resolution.
type ReadOptions struct {
	// Body runs a collected record's declared body argv. It is the only read that runs
	// anything, which is why it is a flag, why it needs a trusted base, and why MCP never
	// exposes it.
	Body bool
	// Limit bounds a directory listing and an entity's neighbourhood.
	Limit int
	// Offset replays an entity neighbourhood without retaining earlier edges. It is used by
	// bounded MCP continuation; CLI reads always leave it at zero.
	Offset int
}

// Read resolves any URI in the grammar.
func Read(ctx context.Context, base *Base, raw string, options ReadOptions) (*ReadResult, error) {
	result, err := resolveRead(ctx, base, raw, options)
	if err != nil {
		return result, withSuggestions(ctx, base, raw, err)
	}
	return result, nil
}

func resolveRead(ctx context.Context, base *Base, raw string, options ReadOptions) (*ReadResult, error) {
	uri, err := ParseURI(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case uri.Scheme == SchemeExternal:
		if options.Body {
			return nil, fmt.Errorf("--body fetches a collected record; %s is an external graph node", uri.String())
		}
		// External URLs are graph nodes, not fetch targets. Returning the same bounded local
		// neighbourhood as an entity keeps every emitted URI composable without widening the
		// offline read boundary.
		return readEntity(ctx, base, uri, options)
	case uri.IsEntity():
		if options.Body {
			return nil, fmt.Errorf("--body fetches a collected record; %s is an entity", uri.String())
		}
		return readEntity(ctx, base, uri, options)
	case uri.Dir:
		if options.Body {
			return nil, fmt.Errorf("--body fetches one record; %s names a directory", uri.String())
		}
		return readDirectory(ctx, base, uri, options)
	case isGraphArtifact(uri.Path):
		if options.Body {
			return nil, fmt.Errorf("--body fetches a collected record; %s is derived", uri.String())
		}
		return readGraphArtifact(ctx, base, uri)
	case strings.HasSuffix(uri.Path, ".json"):
		return readJSON(ctx, base, uri, options)
	default:
		if options.Body {
			return nil, fmt.Errorf("--body fetches a collected record; %s is not a stored document", uri.String())
		}
		return readText(ctx, base, uri)
	}
}

func isGraphArtifact(relative string) bool {
	return relative == core.GraphFile ||
		relative == core.GraphDstFile ||
		relative == core.GraphOffsetsFile ||
		relative == core.GraphMetaFile
}

// readGraphArtifact returns either half of one validated graph generation. Both published
// paths therefore share the same strict sidecar, row, digest, semantic, and current-input
// checks as graph queries. The open graph descriptor pins atomic replacements to one snapshot;
// revalidation catches in-place mutation before any bytes cross the read boundary.
func readGraphArtifact(ctx context.Context, base *Base, uri URI) (*ReadResult, error) {
	if uri.Fragment != "" {
		return nil, fmt.Errorf("%s does not support fragments", uri.Path)
	}
	if uri.Path != core.GraphMetaFile && uri.JQ != "" {
		return nil, fmt.Errorf("?jq= applies to a JSON document; %s is not one", uri.Path)
	}
	cache, _, meta, err := openValidatedGraphCache(ctx, base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cache.close() }()

	if uri.Path != core.GraphMetaFile {
		var file *os.File
		switch uri.Path {
		case core.GraphFile:
			file = cache.file
		case core.GraphDstFile:
			file = cache.dst
		case core.GraphOffsetsFile:
			file = cache.offsets
		}
		manifest, found := graphManifestByURI(meta.Outputs, uri.Path)
		if !found {
			return nil, fmt.Errorf("invalid derived graph cache: metadata omits %s; run `fkf build graph`", uri.Path)
		}
		if manifest.Bytes > core.MaxNarrativeBytes {
			return nil, fmt.Errorf("read %s: file exceeds %d-byte limit", uri.Path, core.MaxNarrativeBytes)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind %s snapshot: %w", uri.Path, err)
		}
		data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: file}, core.MaxNarrativeBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s snapshot: %w", uri.Path, err)
		}
		if int64(len(data)) != manifest.Bytes {
			return nil, fmt.Errorf("invalid derived graph cache: %s changed during the read; run `fkf build graph`",
				uri.Path)
		}
		if err := cache.revalidateBytes(ctx); err != nil {
			return nil, err
		}
		return &ReadResult{URI: uri.String(), Kind: "file", Text: string(data)}, nil
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := cache.revalidateBytes(ctx); err != nil {
		return nil, err
	}
	result := &ReadResult{URI: uri.String(), Kind: "index", Selection: data}
	if uri.JQ == "" {
		return result, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	selection, err := applyJQ(ctx, uri.JQ, value)
	if err != nil {
		return nil, err
	}
	result.Kind, result.Selection = "selection", selection
	return result, nil
}

func readDirectory(ctx context.Context, base *Base, uri URI, options ReadOptions) (*ReadResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	absolute, err := base.Store.Resolve(uri.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", uri.String(), err)
	}
	result := &ReadResult{URI: uri.String(), Kind: "directory", Entries: []string{}}
	for _, entry := range entries {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := path.Join(uri.Path, name)
		// A filesystem neighbour does not gain a URI merely by sitting under an enabled
		// layer. Filter through the same closed grammar as read itself so a listing neither
		// leaks private filenames nor advertises addresses that the next call will refuse.
		if _, err := base.Store.Resolve(child); err != nil {
			continue
		}
		if entry.IsDir() {
			child += "/"
		}
		result.Entries = append(result.Entries, child)
	}
	sort.Strings(result.Entries)
	if options.Limit > 0 && len(result.Entries) > options.Limit {
		result.Entries = result.Entries[:options.Limit]
	}
	return result, nil
}

func readJSON(ctx context.Context, base *Base, uri URI, options ReadOptions) (*ReadResult, error) {
	document, err := base.ReadDocumentContext(ctx, uri.Path)
	if err != nil {
		return nil, err
	}
	result := &ReadResult{URI: uri.String(), Kind: "document", Source: document.Source, Date: document.Date}
	var payload any = document
	if uri.Fragment != "" {
		record, found := document.FindRecord(uri.Fragment)
		if !found {
			return nil, fmt.Errorf("%s holds no record with id %q at its declared fields.id paths %v",
				uri.Path, uri.Fragment, document.Fields.Paths(core.FieldID))
		}
		result.Kind, result.Record, payload = "record", record, record
		if options.Body {
			if err := attachBody(ctx, base, result, document, record); err != nil {
				return nil, err
			}
		}
	} else if options.Body {
		return nil, fmt.Errorf("--body fetches one record; add #<id> to name it (for example %s#<id>)", uri.Path)
	}
	if uri.JQ == "" {
		if result.Kind == "document" {
			result.Document = document
		}
		return result, nil
	}
	selection, err := applyJQ(ctx, uri.JQ, payload)
	if err != nil {
		return nil, err
	}
	result.Kind, result.Selection, result.Record, result.Document = "selection", selection, nil, nil
	return result, nil
}

// attachBody distinguishes the three absences the design insists on: a body is fetchable
// (the source declares `body:`), never (it does not), or the fetch failed. Policy none leaves
// the fetched text ephemeral; cache and sync publish it only in the ignored body cache.
func attachBody(ctx context.Context, base *Base, result *ReadResult, document *sources.Document, record sources.Record) error {
	source, err := base.Source(document.Source)
	if err != nil {
		return err
	}
	if !source.HasBody() {
		result.BodyState = "never"
		if document.Body {
			result.BodyState = "no-longer-declared"
		}
		return fmt.Errorf("source %s declares no body: command, so its record bodies are not fetchable", document.Source)
	}
	if source.CachesBodies() {
		body, _, found, err := readCachedBody(ctx, base, result.URI)
		if err != nil {
			result.BodyState = "failed"
			return err
		}
		if found {
			result.Body, result.BodyState = body, "cached"
			return nil
		}
	}
	body, err := base.RunBody(ctx, source, document.Fields, record)
	if err != nil {
		result.BodyState = "failed"
		return err
	}
	result.Body, result.BodyState = body, "fetched"
	if source.CachesBodies() {
		if _, err := cacheBody(ctx, base, document, record, result.URI, body); err != nil {
			result.BodyState = "failed"
			return fmt.Errorf("cache body for %s: %w", result.URI, err)
		}
		result.BodyState = "fetched-and-cached"
	}
	return nil
}

func readText(ctx context.Context, base *Base, uri URI) (*ReadResult, error) {
	if uri.JQ != "" {
		return nil, fmt.Errorf("?jq= applies to a JSON document; %s is not one", uri.Path)
	}
	if strings.HasSuffix(uri.Path, core.MarkdownExtension) {
		page, err := ReadPageContext(ctx, base, uri.Path)
		if err != nil {
			return nil, err
		}
		result := &ReadResult{URI: uri.String(), Kind: "page", Page: &page, Text: page.Body}
		if uri.Fragment != "" {
			section, found := sectionOf(page, uri.Fragment)
			if !found {
				return nil, fmt.Errorf("%s has no heading anchored %q; its anchors are %s",
					uri.Path, uri.Fragment, strings.Join(anchorsOf(page), ", "))
			}
			result.Kind, result.Text = "section", section
		}
		return result, nil
	}
	if uri.Fragment != "" {
		return nil, fmt.Errorf("%s does not support fragments", uri.Path)
	}
	data, err := base.ReadFileContext(ctx, uri.Path, core.MaxNarrativeBytes)
	if err != nil {
		return nil, err
	}
	return &ReadResult{URI: uri.String(), Kind: "file", Text: string(data)}, nil
}

// sectionOf returns one heading and everything under it up to the next heading of the same or
// a higher level — the unit a citation means when it says `#decision`. Headings are recomputed
// against the body so the offsets are body-relative regardless of the frontmatter's length.
func sectionOf(page Page, anchor string) (string, bool) {
	lines := strings.Split(page.Body, "\n")
	headings := extractHeadings(page.Body, 0)
	for index, heading := range headings {
		if heading.Anchor != anchor {
			continue
		}
		start, end := heading.Line, len(lines)
		for _, later := range headings[index+1:] {
			if later.Level <= heading.Level {
				end = later.Line
				break
			}
		}
		start, end = min(start, len(lines)), min(end, len(lines))
		return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n"), true
	}
	return "", false
}

func anchorsOf(page Page) []string {
	anchors := make([]string, 0, len(page.Headings))
	for _, heading := range page.Headings {
		anchors = append(anchors, heading.Anchor)
	}
	return anchors
}

func readEntity(ctx context.Context, base *Base, uri URI, options ReadOptions) (*ReadResult, error) {
	view := &EntityView{URI: uri.String(), Scheme: uri.Scheme, Value: uri.Value, Neighbours: []Edge{}}
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}
	// Propagated, not swallowed — except for the one absence that is not a fault. A corrupt or
	// unreadable edge list used to be discarded here, answering an entity query with a
	// confident empty neighbourhood indistinguishable from "it appears in nothing"; an edge
	// list that has simply never been built is a different thing and stays an empty answer,
	// which is what a fresh clone legitimately has.
	neighbourhood, err := Neighbours(ctx, base, GraphQuery{
		URI: uri.String(), Direction: DirectionBoth, Depth: 1,
		Offset: options.Offset, Limit: limit,
	})
	snapshotSHA256 := ""
	switch {
	case errors.Is(err, ErrDerivedMissing):
	case err != nil:
		return nil, fmt.Errorf("read the neighbourhood of %s: %w", uri.String(), err)
	default:
		snapshotSHA256 = neighbourhood.SnapshotSHA256
		if err := requireCleanGraphStats(neighbourhood.Stats); err != nil {
			return nil, fmt.Errorf("read the neighbourhood of %s: %w", uri.String(), err)
		}
		for _, edge := range neighbourhood.Edges {
			view.Neighbours = append(view.Neighbours, edge.Edge)
		}
		view.NeighbourCap = neighbourhood.Truncated
	}
	result := &ReadResult{URI: uri.String(), Kind: "entity", Entity: view, SnapshotSHA256: snapshotSHA256}
	return result, nil
}

// applyJQ evaluates the expression in-process with gojq. It used to shell out to the jq on
// PATH, which made the read path a subprocess and — because that subprocess inherited fkf's
// environment — turned `?jq=$ENV` into a credential oracle reachable from the ungated MCP
// `read` tool. gojq closes that by construction rather than by a denylist: with no compiler
// options, `env`/`$ENV` evaluate to an empty object, and `input`, `inputs`, `include` and
// `import` refuse to compile, so an expression can reach neither the environment nor the
// filesystem. It also drops the external jq binary from the requirements, which is what makes
// "reads open files" literally true.
func applyJQ(ctx context.Context, expression string, payload any) (json.RawMessage, error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQExpression, err)
	}
	// gojq represents both `halt` and `halt_error(0)` as a zero-code HaltError at runtime,
	// so the iterator cannot distinguish the permitted empty-stream terminator from an
	// explicit failure. Reject the latter from the parsed AST; matching source text would
	// also reject harmless strings and object keys named "halt_error".
	if jqCallsFunction(query, "halt_error") {
		return nil, fmt.Errorf("%w: halt_error is not available; only halt may terminate a selection successfully", ErrJQExpression)
	}
	// No CompilerOption on purpose. Every option gojq offers widens what an expression can
	// reach; the default is the closed one, and this call site must stay closed.
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQExpression, err)
	}
	input, err := jqInput(payload)
	if err != nil {
		return nil, err
	}
	// An expression like `repeat(.)` never terminates, so evaluation is bounded the same way
	// the subprocess was.
	ctx, cancel := context.WithTimeout(ctx, jqTimeout)
	defer cancel()

	var values []json.RawMessage
	produced := 0
	iterator := code.RunWithContext(ctx, input)
	for {
		value, ok := iterator.Next()
		if !ok {
			break
		}
		if failure, isError := value.(error); isError {
			var halt *gojq.HaltError
			if errors.As(failure, &halt) && halt.ExitCode() == 0 {
				break // Only `halt` succeeds; `halt_error` remains an explicit jq failure even for null.
			}
			return nil, fmt.Errorf("jq %q: %w", expression, failure)
		}
		// gojq.Marshal, not encoding/json: it renders a *big.Int without scientific notation,
		// so a 19-digit record id survives a selection intact.
		encoded, err := gojq.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode the jq result: %w", err)
		}
		switch len(values) {
		case 0:
			produced = len(encoded)
		case 1:
			// The second value changes the representation from one scalar to [first,second].
			produced += len(encoded) + 3
		default:
			produced += len(encoded) + 1
		}
		if produced > jqMaxOutputBytes {
			return nil, fmt.Errorf("%w: jq %q produced more than %d bytes; narrow the expression",
				core.ErrFileTooLarge, expression, jqMaxOutputBytes)
		}
		values = append(values, encoded)
	}
	switch len(values) {
	case 0:
		return json.RawMessage("null"), nil
	case 1:
		return values[0], nil
	default:
		// jq streams one value per line; wrap a multi-value result in an array so the envelope
		// stays valid JSON without the caller having to know which shape it will get.
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = string(value)
		}
		return json.RawMessage("[" + strings.Join(parts, ",") + "]"), nil
	}
}

// jqCallsFunction walks gojq's public AST rather than matching source text. Reflection keeps
// this guard complete when gojq nests a query in a new public AST container, while the exact
// gojq.Func type check means data strings and field names cannot trigger it.
func jqCallsFunction(query *gojq.Query, name string) bool {
	walker := jqASTWalker{
		name: name, functionType: reflect.TypeFor[gojq.Func](), seen: make(map[astPointer]struct{}),
	}
	return walker.callsFunction(reflect.ValueOf(query))
}

type jqASTWalker struct {
	name         string
	functionType reflect.Type
	seen         map[astPointer]struct{}
}

func (walker *jqASTWalker) callsFunction(value reflect.Value) bool {
	value, ok := walker.dereference(value)
	if !ok {
		return false
	}
	if value.Type() == walker.functionType {
		return value.FieldByName("Name").String() == walker.name
	}
	return walker.containerCallsFunction(value)
}

func (walker *jqASTWalker) dereference(value reflect.Value) (reflect.Value, bool) {
	if !value.IsValid() {
		return reflect.Value{}, false
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return reflect.Value{}, false
		}
		if value.Kind() == reflect.Pointer {
			pointer := astPointer{Type: value.Type(), Address: value.Pointer()}
			if _, found := walker.seen[pointer]; found {
				return reflect.Value{}, false
			}
			walker.seen[pointer] = struct{}{}
		}
		value = value.Elem()
	}
	return value, true
}

func (walker *jqASTWalker) containerCallsFunction(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Struct:
		for index := range value.NumField() {
			if walker.callsFunction(value.Field(index)) {
				return true
			}
		}
	case reflect.Array, reflect.Slice:
		for index := range value.Len() {
			if walker.callsFunction(value.Index(index)) {
				return true
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if walker.callsFunction(iterator.Key()) || walker.callsFunction(iterator.Value()) {
				return true
			}
		}
	}
	return false
}

type astPointer struct {
	Type    reflect.Type
	Address uintptr
}

// jqInput renders any payload as the plain map/slice/scalar tree gojq evaluates over. The
// round trip through JSON is what keeps one rule for every payload shape — a struct, a decoded
// document, or an entity view — instead of a type switch that drifts from the encoder.
func jqInput(payload any) (any, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode the document for jq: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	// UseNumber, then widen by hand: decoding straight into `any` turns every number into a
	// float64, which silently rounds a record id past 2^53.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode the document for jq: %w", err)
	}
	return jqNumbers(value), nil
}

// jqNumbers converts the json.Number leaves into the int, *big.Int, and float64 that gojq
// evaluates over.
func jqNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, element := range typed {
			typed[key] = jqNumbers(element)
		}
		return typed
	case []any:
		for index, element := range typed {
			typed[index] = jqNumbers(element)
		}
		return typed
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return int(integer)
		}
		if big, ok := new(big.Int).SetString(typed.String(), 10); ok {
			return big
		}
		float, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return float
	default:
		return value
	}
}
