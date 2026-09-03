package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	FieldID    = "id"
	FieldTime  = "time"
	FieldTitle = "title"
	FieldURL   = "url"
	// FieldCategory and FieldVisibility are optional retrieval roles. They stay in the open
	// projected field map so context can apply policy without hiding the evidence value.
	FieldCategory   = "category"
	FieldVisibility = "visibility"
)

// ConfigVersion is the fkf.yaml contract frozen for the v1 line. It is deliberately separate
// from the binary version and from the stored-document marker: one says how to read the base,
// one says which program is running, and one says how a collected file was encoded.
const ConfigVersion = 1

// Cardinality is the number of scalar values one declared field may project from one record.
// It is intentionally smaller than JSON Schema: provider reshaping belongs in run:, while fkf
// only needs enough information to reject ambiguous identities and presentation values.
type Cardinality string

const (
	CardinalityOne      Cardinality = "one"
	CardinalityOptional Cardinality = "optional"
	CardinalityMany     Cardinality = "many"
)

// FieldDefinition gives one base-chosen semantic name a stable meaning across every source and
// authored page. Relation values are canonical fkf URIs produced by the source command; fkf
// validates and transcribes them but never guesses or coerces provider identities.
type FieldDefinition struct {
	Description string      `json:"description" yaml:"description"`
	Cardinality Cardinality `json:"cardinality" yaml:"cardinality"`
	Relation    bool        `json:"relation,omitempty" yaml:"relation,omitempty"`
	Examples    []string    `json:"examples,omitempty" yaml:"examples,omitempty"`
	// Weight is an optional lexical-ranking multiplier. Zero means use the stable default
	// for the field name, so existing v1 bases keep their ranking contract unchanged.
	Weight int `json:"weight,omitempty" yaml:"weight,omitempty"`
}

// FieldSchema is the base's open semantic dictionary. Keys are user-chosen field and relation
// names; sources only associate those names with provider paths.
type FieldSchema map[string]FieldDefinition

const (
	MaxFieldDescriptionLength = 512
	MaxFieldExamples          = 8
	MaxFieldExampleLength     = 512
	MaxFieldWeight            = 100
	DefaultIDFieldWeight      = 10
	DefaultTitleFieldWeight   = 5
	DefaultFieldWeight        = 1
)

var (
	entityReferencePattern = regexp.MustCompile(`^([a-z][a-z0-9+.-]*):(.+)$`)
	entitySchemePattern    = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
)

// ValidateEntityScheme enforces the open-but-non-reserved entity namespace shared by the
// stored relation boundary and the URI parser. `file` and `external` are internal URI kinds;
// the protocol names remain reserved for external addresses rather than entity aliases.
func ValidateEntityScheme(value string) error {
	if !entitySchemePattern.MatchString(value) {
		return errors.New("must start with a lowercase letter and contain only lowercase letters, digits, +, ., or -")
	}
	switch value {
	case "file", "external", "http", "https", "ftp", "mailto":
		return fmt.Errorf("scheme %q is reserved and cannot name an entity", value)
	default:
		return nil
	}
}

// ValidateEntityURI admits only canonical entity URIs. Unlike ValidateRelationValue it does
// not accept files or external URLs: identity declarations must name a graph entity node.
func ValidateEntityURI(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return errors.New("must be a non-empty canonical entity URI with no whitespace")
	}
	match := entityReferencePattern.FindStringSubmatch(value)
	if match == nil {
		return errors.New("must be an entity URI of the form scheme:identity")
	}
	if err := ValidateEntityScheme(match[1]); err != nil {
		return err
	}
	if err := validateCanonicalEntityIdentity(match[2]); err != nil {
		return fmt.Errorf("entity identity: %w", err)
	}
	return nil
}

// Allows reports whether a projected scalar count satisfies the declaration.
func (c Cardinality) Allows(count int) bool {
	switch c {
	case CardinalityOne:
		return count == 1
	case CardinalityOptional:
		return count <= 1
	case CardinalityMany:
		return true
	default:
		return false
	}
}

// MaxOne reports whether a consumer may safely request one scalar from the field.
func (c Cardinality) MaxOne() bool { return c == CardinalityOne || c == CardinalityOptional }

// ValidateRelationValue checks the canonical relation boundary available to core and sources.
// File references receive their existence and child-addressability checks when the graph is
// built, but their complete lexical grammar is enforced here so a successful collection can
// never store a relation the graph parser will later refuse.
func ValidateRelationValue(value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("must be a non-empty canonical URI with no surrounding whitespace")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return errors.New("must be a canonical URI; whitespace must be percent-encoded")
	}
	if strings.HasPrefix(strings.ToLower(value), "https://") {
		return validateHTTPSRelation(value)
	}
	if strings.Contains(fileRelationHead(value), "://") {
		return errors.New("external URIs must use https")
	}
	if match := entityReferencePattern.FindStringSubmatch(value); match != nil {
		if err := ValidateEntityScheme(match[1]); err != nil {
			return err
		}
		if err := validateCanonicalEntityIdentity(match[2]); err != nil {
			return fmt.Errorf("entity identity: %w", err)
		}
		return nil
	}
	return validateFileRelation(value)
}

func fileRelationHead(value string) string {
	head := value
	if hash := strings.LastIndex(head, "#"); hash >= 0 {
		head = head[:hash]
	}
	if query := strings.Index(head, "?"); query >= 0 {
		head = head[:query]
	}
	return head
}

func validateHTTPSRelation(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("must be an absolute HTTPS URI: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return errors.New("must be an absolute HTTPS URI with a host")
	}
	return nil
}

func validateFileRelation(value string) error {
	pathValue := value
	hasFragment := false
	if hash := strings.LastIndex(pathValue, "#"); hash >= 0 {
		hasFragment = true
		fragment := pathValue[hash+1:]
		if err := validateCanonicalEntityIdentity(fragment); err != nil {
			return fmt.Errorf("file URI fragment: %w", err)
		}
		pathValue = pathValue[:hash]
	}
	hasJQ := false
	if query := strings.Index(pathValue, "?"); query >= 0 {
		hasJQ = true
		rawQuery := pathValue[query+1:]
		pathValue = pathValue[:query]
		if err := validateFileRelationQuery(rawQuery); err != nil {
			return err
		}
	}
	cleaned, err := CleanRelative(pathValue)
	if err != nil {
		return fmt.Errorf("must be an entity URI, HTTPS URI, or base-relative path: %w", err)
	}
	if cleaned != pathValue {
		return fmt.Errorf("base-relative relation path is not canonical; want %q", cleaned)
	}
	capabilities, addressable := relationFilePath(cleaned)
	if !addressable {
		return errors.New("base-relative relation must name a published file, not a listing or private path")
	}
	if hasFragment && !capabilities.Fragment {
		return fmt.Errorf("base-relative relation %q does not address records or Markdown headings", cleaned)
	}
	if hasJQ && !capabilities.JQ {
		return fmt.Errorf("base-relative relation %q is not a selectable JSON document", cleaned)
	}
	return nil
}

func validateFileRelationQuery(rawQuery string) error {
	if !strings.HasPrefix(rawQuery, "jq=") {
		return errors.New("the only supported file URI query is ?jq=<expression>")
	}
	rawExpression := strings.TrimPrefix(rawQuery, "jq=")
	expression, err := url.QueryUnescape(rawExpression)
	if err != nil {
		return fmt.Errorf("file URI jq expression: %w", err)
	}
	if strings.TrimSpace(expression) == "" {
		return errors.New("file URI ?jq= must name an expression")
	}
	if canonical := url.QueryEscape(expression); canonical != rawExpression {
		return fmt.Errorf("file URI jq expression is not canonical; want %q", canonical)
	}
	return nil
}

func validateCanonicalEntityIdentity(identity string) error {
	decoded := make([]byte, 0, len(identity))
	for index := 0; index < len(identity); index++ {
		char := identity[index]
		if isEntityIdentitySafe(char) {
			decoded = append(decoded, char)
			continue
		}
		if char != '%' || index+2 >= len(identity) || !isUpperHex(identity[index+1]) || !isUpperHex(identity[index+2]) {
			return errors.New("must use uppercase percent escapes for bytes outside A-Z a-z 0-9 . _ : / @ + -")
		}
		value, err := strconv.ParseUint(identity[index+1:index+3], 16, 8)
		if err != nil {
			return fmt.Errorf("invalid percent escape: %w", err)
		}
		decodedByte := byte(value)
		if isEntityIdentitySafe(decodedByte) {
			return fmt.Errorf("percent escape %%%s is not canonical; write %q directly", identity[index+1:index+3], decodedByte)
		}
		decoded = append(decoded, decodedByte)
		index += 2
	}
	if !utf8.Valid(decoded) {
		return errors.New("percent-decoded value is not valid UTF-8")
	}
	value := string(decoded)
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("must decode to a non-empty value with no surrounding whitespace")
	}
	return nil
}

func isEntityIdentitySafe(char byte) bool {
	switch {
	case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		return true
	default:
		return strings.IndexByte("._:/@+-", char) >= 0
	}
}

func isUpperHex(char byte) bool {
	return char >= '0' && char <= '9' || char >= 'A' && char <= 'F'
}

// ValidateFieldSchema enforces the small semantic contract shared by config, documents, graph,
// retrieval, and authored relations.
func ValidateFieldSchema(schema FieldSchema) error {
	if len(schema) == 0 {
		return errors.New("schema is required and must declare at least id")
	}
	if len(schema) > MaxFields {
		return fmt.Errorf("schema declares %d fields; expected at most %d", len(schema), MaxFields)
	}
	for _, name := range schema.Names() {
		definition := schema[name]
		switch {
		case !fieldNamePattern.MatchString(name):
			return fmt.Errorf("schema field name %q must start with a lowercase letter and contain only lowercase letters, digits, hyphens, or underscores", name)
		case len(name) > MaxFieldNameLength:
			return fmt.Errorf("schema field name %q is %d bytes; expected at most %d", name, len(name), MaxFieldNameLength)
		case strings.TrimSpace(definition.Description) == "":
			return fmt.Errorf("schema.%s.description is required", name)
		case len(definition.Description) > MaxFieldDescriptionLength:
			return fmt.Errorf("schema.%s.description is %d bytes; expected at most %d", name, len(definition.Description), MaxFieldDescriptionLength)
		case len(definition.Examples) > MaxFieldExamples:
			return fmt.Errorf("schema.%s.examples has %d entries; expected at most %d", name, len(definition.Examples), MaxFieldExamples)
		case definition.Weight < 0 || definition.Weight > MaxFieldWeight:
			return fmt.Errorf("schema.%s.weight is %d; expected 1..%d when declared", name, definition.Weight, MaxFieldWeight)
		}
		if definition.Cardinality != CardinalityOne && definition.Cardinality != CardinalityOptional && definition.Cardinality != CardinalityMany {
			return fmt.Errorf("schema.%s.cardinality %q must be one, optional, or many", name, definition.Cardinality)
		}
		for index, example := range definition.Examples {
			if len(example) > MaxFieldExampleLength {
				return fmt.Errorf("schema.%s.examples[%d] is %d bytes; expected at most %d", name, index, len(example), MaxFieldExampleLength)
			}
			if definition.Relation {
				if err := ValidateRelationValue(example); err != nil {
					return fmt.Errorf("schema.%s.examples[%d] %q: %w", name, index, example, err)
				}
			}
		}
	}
	id, exists := schema[FieldID]
	if !exists {
		return errors.New("schema.id is required")
	}
	if id.Cardinality != CardinalityOne {
		return errors.New("schema.id.cardinality must be one")
	}
	if id.Relation {
		return errors.New("schema.id must not be a relation")
	}
	if eventTime, exists := schema[FieldTime]; exists && eventTime.Relation {
		return errors.New("schema.time must not be a relation")
	}
	for _, name := range []string{FieldTime, FieldTitle, FieldURL, FieldCategory, FieldVisibility} {
		if definition, exists := schema[name]; exists && !definition.Cardinality.MaxOne() {
			return fmt.Errorf("schema.%s.cardinality must be one or optional because fkf consumes one scalar", name)
		}
	}
	return nil
}

// Weight returns one field's configured lexical multiplier, or the stable well-known
// default. Unknown fields use the ordinary weight because authored relations may be absent
// from an older stored document's semantic subset.
func (s FieldSchema) Weight(name string) int {
	if definition, exists := s[name]; exists && definition.Weight > 0 {
		return definition.Weight
	}
	switch name {
	case FieldID:
		return DefaultIDFieldWeight
	case FieldTitle:
		return DefaultTitleFieldWeight
	default:
		return DefaultFieldWeight
	}
}

// Names returns the semantic names in deterministic order.
func (s FieldSchema) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Select copies the definitions used by one source so a collected document remains
// self-describing even after fkf.yaml changes.
func (s FieldSchema) Select(fields FieldMap) FieldSchema {
	selected := make(FieldSchema, len(fields))
	for _, name := range fields.Names() {
		selected[name] = s[name]
	}
	return selected
}

// wellKnownFields are the small runtime projection: identity, event time, and optional display
// values. Every other field is retrieved and graphed generically from its declaration.
var wellKnownFields = [...]string{
	FieldID, FieldTime, FieldTitle, FieldURL,
}

const (
	MaxFieldNameLength = 64
	MaxFields          = 64
	MaxPathsPerField   = 32
)

var fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// A field path is a deliberate subset of jq: `.key`, `.a.b`, `[n]`, `[]`, and `."odd key"`.
// Nothing else — no `select`, no pipes, no functions — because anything richer belongs in
// the source's helper where the real jq already is. Two properties follow, and both
// are the reason for the subset:
//
//   - A path that fkf accepts is valid jq, so a user debugging a source can paste it
//     straight into `jq` and see the same value.
//   - Evaluation is one small total function over decoded JSON, with no expression
//     language, no evaluator state, and nothing to sandbox.
type FieldPath struct {
	raw   string
	steps []fieldStep
}

// FieldPaths is one or more alternative projections for one semantic field. YAML and stored
// JSON keep the common one-path case as a string while accepting a list when several provider
// locations contribute values, so the open map stays readable without special-casing people.
type FieldPaths []FieldPath

// MarshalJSON preserves the compact public shape: one path is a string, several are an array.
func (p FieldPaths) MarshalJSON() ([]byte, error) {
	if len(p) == 1 {
		return json.Marshal(p[0])
	}
	return json.Marshal([]FieldPath(p))
}

// UnmarshalJSON accepts the same scalar-or-list shape from stored documents.
func (p *FieldPaths) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("field paths must be a path string or a list of path strings")
	}
	switch trimmed[0] {
	case '"':
		var single FieldPath
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return fmt.Errorf("field path: %w", err)
		}
		*p = FieldPaths{single}
		return nil
	case '[':
		var many []FieldPath
		if err := json.Unmarshal(trimmed, &many); err != nil {
			return fmt.Errorf("field paths list: %w", err)
		}
		*p = FieldPaths(many)
		return nil
	default:
		return errors.New("field paths must be a path string or a list of path strings")
	}
}

// UnmarshalYAML compiles every configured path at the trust boundary.
func (p *FieldPaths) UnmarshalYAML(node *yaml.Node) error {
	parse := func(value string) (FieldPath, error) {
		path, err := ParseFieldPath(value)
		if err != nil {
			return FieldPath{}, err
		}
		return path, nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		path, err := parse(node.Value)
		if err != nil {
			return err
		}
		*p = FieldPaths{path}
		return nil
	case yaml.SequenceNode:
		paths := make(FieldPaths, 0, len(node.Content))
		for index, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return fmt.Errorf("field path %d must be a string", index)
			}
			path, err := parse(child.Value)
			if err != nil {
				return fmt.Errorf("field path %d: %w", index, err)
			}
			paths = append(paths, path)
		}
		*p = paths
		return nil
	default:
		return errors.New("field paths must be a path string or a list of path strings")
	}
}

// FieldMap is a source's open semantic projection. Keys are user-chosen; built-in consumers
// read only the well-known names while retrieval may index every additional value.
type FieldMap map[string]FieldPaths

// Names returns a deterministic field order for retrieval and receipts.
func (m FieldMap) Names() []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Paths returns every path declared for one field.
func (m FieldMap) Paths(name string) FieldPaths { return m[name] }

// Path returns the first path for a single-valued well-known field, or the zero path.
func (m FieldMap) Path(name string) FieldPath {
	paths := m[name]
	if len(paths) == 0 {
		return FieldPath{}
	}
	return paths[0]
}

// EvalString projects the first scalar value selected by the field's paths, in declaration
// order. This gives a custom field a deterministic fallback when providers use more than one
// location for the same meaning.
func (m FieldMap) EvalString(name string, value any) (string, bool) {
	values := m.EvalStrings(name, value)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// EvalStrings flattens every declared path in order and removes duplicate scalar values.
func (m FieldMap) EvalStrings(name string, value any) []string {
	var values []string
	seen := map[string]struct{}{}
	for _, path := range m[name] {
		for _, projected := range path.EvalStrings(value) {
			if _, duplicate := seen[projected]; duplicate {
				continue
			}
			seen[projected] = struct{}{}
			values = append(values, projected)
		}
	}
	return values
}

// EvalField projects every declared path while refusing non-scalar values. Collection uses it
// before accepting a record; the simpler read helpers can then rely on stored documents having
// crossed this typed boundary already.
func (m FieldMap) EvalField(name string, value any) ([]string, error) {
	return m.evalField(name, value, CardinalityMany, ScalarString)
}

// EvalRelation projects relation values without trimming or otherwise normalizing provider
// strings. Presentation fields may discard surrounding whitespace, but a relation is an
// identity: changing even one byte would make the stored graph mean something the provider did
// not emit.
func (m FieldMap) EvalRelation(name string, value any) ([]string, error) {
	return m.evalField(name, value, CardinalityMany, exactScalarString)
}

// EvalDeclaredField projects one field under its schema declaration. Empty provider strings
// mean "not present" for optional and many fields, while a required identity must explain the
// actual defect instead of reporting the empty string as a non-scalar value.
func (m FieldMap) EvalDeclaredField(
	name string, value any, definition FieldDefinition,
) ([]string, error) {
	scalar := ScalarString
	if definition.Relation {
		scalar = exactScalarString
	}
	return m.evalField(name, value, definition.Cardinality, scalar)
}

func (m FieldMap) evalField(
	name string, value any, cardinality Cardinality, scalar func(any) (string, bool),
) ([]string, error) {
	var values []string
	seen := map[string]struct{}{}
	for _, fieldPath := range m[name] {
		for _, selected := range fieldPath.Eval(value) {
			if text, isString := selected.(string); isString && strings.TrimSpace(text) == "" {
				if cardinality == CardinalityOne {
					return nil, fmt.Errorf("path %s selected an empty identity", fieldPath)
				}
				continue
			}
			projected, ok := scalar(selected)
			if !ok {
				return nil, fmt.Errorf("path %s selected a %T; fields project only JSON scalars", fieldPath, selected)
			}
			if _, duplicate := seen[projected]; duplicate {
				continue
			}
			seen[projected] = struct{}{}
			values = append(values, projected)
		}
	}
	return values, nil
}

func exactScalarString(value any) (string, bool) {
	if text, ok := value.(string); ok {
		return text, true
	}
	return ScalarString(value)
}

// IsWellKnownField reports whether fkf gives this suggested name built-in semantics.
func IsWellKnownField(name string) bool {
	for _, known := range wellKnownFields {
		if name == known {
			return true
		}
	}
	return false
}

// ValidateFieldMap enforces the open map's small structural contract. Only identity and event
// time are mandatory; the other well-known names and every custom name are optional.
func ValidateFieldMap(fields FieldMap, event bool) error {
	if len(fields) > MaxFields {
		return fmt.Errorf("declares %d fields; expected at most %d", len(fields), MaxFields)
	}
	for _, name := range fields.Names() {
		paths := fields[name]
		switch {
		case !fieldNamePattern.MatchString(name):
			return fmt.Errorf("field name %q must start with a lowercase letter and contain only lowercase letters, digits, hyphens, or underscores", name)
		case len(name) > MaxFieldNameLength:
			return fmt.Errorf("field name %q is %d bytes; expected at most %d", name, len(name), MaxFieldNameLength)
		case len(paths) == 0:
			return fmt.Errorf("fields.%s must declare at least one path", name)
		case len(paths) > MaxPathsPerField:
			return fmt.Errorf("fields.%s declares %d paths; expected at most %d", name, len(paths), MaxPathsPerField)
		}
		for index, path := range paths {
			if path.IsZero() {
				return fmt.Errorf("fields.%s[%d] is empty", name, index)
			}
		}
	}
	if fields.Path(FieldID).IsZero() {
		return errors.New("fields.id is required: a record with no declared identity cannot be addressed by a URI")
	}
	if event && fields.Path(FieldTime).IsZero() {
		return errors.New("fields.time is required for an events source: a dated document needs a per-record timestamp")
	}
	return nil
}

type fieldStepKind int

const (
	stepKey fieldStepKind = iota
	stepIndex
	stepIterate
)

type fieldStep struct {
	kind  fieldStepKind
	key   string
	index int
}

// ParseFieldPath compiles one path, naming the offending character when it falls outside the
// subset. A path is validated at configuration load, so a typo fails before any command runs.
func ParseFieldPath(raw string) (FieldPath, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return FieldPath{}, errors.New("field path is empty")
	}
	if !strings.HasPrefix(trimmed, ".") {
		return FieldPath{}, fmt.Errorf("field path %q must start with `.` (for example `.id` or `.a.b[0]`)", raw)
	}
	path := FieldPath{raw: trimmed}
	rest := trimmed
	for rest != "" && rest != "." {
		var err error
		if rest, err = path.consume(rest, raw); err != nil {
			return FieldPath{}, err
		}
	}
	return path, nil
}

func (p *FieldPath) consume(rest, raw string) (string, error) {
	switch rest[0] {
	case '.':
		return p.consumeKey(rest[1:], raw)
	case '[':
		return p.consumeBracket(rest[1:], raw)
	default:
		return "", fmt.Errorf("field path %q: expected `.` or `[` at %q", raw, rest)
	}
}

func (p *FieldPath) consumeKey(rest, raw string) (string, error) {
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return "", fmt.Errorf("field path %q: quoted key is not closed", raw)
		}
		p.steps = append(p.steps, fieldStep{kind: stepKey, key: rest[1 : 1+end]})
		return rest[end+2:], nil
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r == '.' || r == '[' })
	if end < 0 {
		end = len(rest)
	}
	key := rest[:end]
	if key == "" {
		return "", fmt.Errorf("field path %q: empty key; quote it as .\"…\" if the key really is empty", raw)
	}
	for _, char := range key {
		isWord := char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if !isWord {
			return "", fmt.Errorf("field path %q: key %q contains %q; quote it as .%q", raw, key, char, key)
		}
	}
	p.steps = append(p.steps, fieldStep{kind: stepKey, key: key})
	return rest[end:], nil
}

func (p *FieldPath) consumeBracket(rest, raw string) (string, error) {
	end := strings.Index(rest, "]")
	if end < 0 {
		return "", fmt.Errorf("field path %q: `[` is not closed", raw)
	}
	inner := strings.TrimSpace(rest[:end])
	if inner == "" {
		p.steps = append(p.steps, fieldStep{kind: stepIterate})
		return rest[end+1:], nil
	}
	index, err := strconv.Atoi(inner)
	if err != nil {
		return "", fmt.Errorf("field path %q: `[%s]` must be empty or an integer index", raw, inner)
	}
	p.steps = append(p.steps, fieldStep{kind: stepIndex, index: index})
	return rest[end+1:], nil
}

// String returns the path exactly as it was written, which is what the stored document
// records and what a user pastes into jq.
func (p FieldPath) String() string { return p.raw }

// IsZero reports an undeclared path. An optional field is absent rather than empty, so a
// caller never has to distinguish "not declared" from "declared as nothing".
func (p FieldPath) IsZero() bool { return p.raw == "" }

// MarshalJSON stores the path verbatim, so a document's field map round-trips through
// re-collection and stays pasteable into jq.
func (p FieldPath) MarshalJSON() ([]byte, error) { return json.Marshal(p.raw) }

// UnmarshalJSON recompiles a stored path so a read never trusts an unvalidated string.
func (p *FieldPath) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		*p = FieldPath{}
		return nil
	}
	parsed, err := ParseFieldPath(raw)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// Eval returns every value the path selects. It is multi-valued because `[]` iterates, so one
// semantic field can combine provider arrays and scalar fallbacks without a special case.
// Null and missing are the same absence: neither is a graph destination or searchable value.
func (p FieldPath) Eval(value any) []any {
	if p.IsZero() {
		return nil
	}
	current := []any{value}
	for _, step := range p.steps {
		next := make([]any, 0, len(current))
		for _, item := range current {
			next = step.apply(next, item)
		}
		if len(next) == 0 {
			return nil
		}
		current = next
	}
	return current
}

func (s fieldStep) apply(into []any, item any) []any {
	switch s.kind {
	case stepKey:
		if object, ok := item.(map[string]any); ok {
			if selected, present := object[s.key]; present && selected != nil {
				return append(into, selected)
			}
		}
	case stepIndex:
		if array, ok := item.([]any); ok {
			index := s.index
			if index < 0 {
				index += len(array)
			}
			if index >= 0 && index < len(array) && array[index] != nil {
				return append(into, array[index])
			}
		}
	case stepIterate:
		return appendIterated(into, item)
	}
	return into
}

func appendIterated(into []any, item any) []any {
	switch typed := item.(type) {
	case []any:
		for _, element := range typed {
			if element != nil {
				into = append(into, element)
			}
		}
	case map[string]any:
		// jq iterates an object's values; sorting the keys is what keeps a stored
		// document byte-identical across runs when a source declares `.headers[]`.
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if typed[key] != nil {
				into = append(into, typed[key])
			}
		}
	}
	return into
}

// EvalString returns the selected value only when the path projects exactly one scalar.
// Numbers are rendered without an exponent so a JSON id of 412 addresses the same record as
// the string "412" — the alternative is a URI fragment reading `4.12e+02`.
func (p FieldPath) EvalString(value any) (string, bool) {
	values := p.EvalStrings(value)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// EvalStrings returns every selected value rendered as a scalar string, in path order and
// deduplicated. Every field uses this same union before its declared cardinality is checked.
func (p FieldPath) EvalStrings(value any) []string {
	selected := p.Eval(value)
	seen := make(map[string]struct{}, len(selected))
	rendered := make([]string, 0, len(selected))
	for _, item := range selected {
		text, ok := ScalarString(item)
		if !ok {
			continue
		}
		if _, duplicate := seen[text]; duplicate {
			continue
		}
		seen[text] = struct{}{}
		rendered = append(rendered, text)
	}
	return rendered
}

// ScalarString renders a decoded JSON scalar. Objects and arrays are refused rather than
// stringified: a field path that lands on an object is a configuration mistake, and `map[…]`
// in a URI would hide it.
func ScalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		return trimmed, trimmed != ""
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10), true
		}
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case json.Number:
		return typed.String(), true
	default:
		return "", false
	}
}
