package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, text string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestParseFieldPathAcceptsTheSubset(t *testing.T) {
	for _, raw := range []string{
		".id", ".a.b", ".a.b.c", ".items[0]", ".items[]", ".a[0].b[]",
		`."odd key"`, `.a."odd key".b`, ".items[-1]", ".",
	} {
		path, err := ParseFieldPath(raw)
		if err != nil {
			t.Fatalf("ParseFieldPath(%q) error = %v", raw, err)
		}
		// A path stays exactly what the user wrote, so pasting it into jq shows the same value.
		if path.String() != raw {
			t.Fatalf("ParseFieldPath(%q).String() = %q, want it kept verbatim", raw, path.String())
		}
	}
}

func TestValidateRelationValueRequiresCanonicalEntityText(t *testing.T) {
	for _, value := range []string{
		"person:email/marc@example.test",
		"actor:github.com/Marc",
		"tag:line%0Abreak",
		"events/2026-08-22/rss.json#https://example.test/post",
		"index/github-repositories.json#fmind/fkf",
		"tasks/2026-08-22/review/TASKS.md#verification",
		"projects/fkf.md#decisions",
		"wiki/retrieval-boundary.md#decision",
		"graph.tsv",
		"graph.meta.json?jq=.edges",
		"AGENTS.md#invariants",
		ConfigFileName,
	} {
		if err := ValidateRelationValue(value); err != nil {
			t.Errorf("ValidateRelationValue(%q) error = %v", value, err)
		}
	}

	for _, value := range []string{
		"http:example.test",
		"mailto:marc@example.test",
		"file:wiki/page.md",
		"external:example.test",
		"person:bad%escape",
		"person:line%0abreak",
		"person:raw?query",
		"person:raw#fragment",
		"wiki/../projects/p.md",
		"./wiki/x.md",
		"wiki//x.md",
		"wiki/x.md?evil=1",
		"wiki/x.md#",
		"wiki/x.md#bad%escape",
		"wiki/x.md?jq=.title#fragment with space",
		"wiki/nested/page.md",
		"events/not-a-date",
		"events/2026-08-22/not@source.json",
		"index/not@source.json",
		"tasks/2026-08-22/review/notes.md",
		"projects/nested/page.md",
		"wiki/diagram.png",
		"wiki",
		"wiki/",
		"events/2026-08-22",
		"tasks/2026-08-22/review",
		"wiki/page.md?jq=.title",
		"fkf.yaml#config",
		"derived/graph.tsv#row",
		"derived/graph.tsv?jq=.",
		"derived/graph.meta.json#row",
		"derived/private.json",
	} {
		if err := ValidateRelationValue(value); err == nil {
			t.Errorf("ValidateRelationValue(%q) succeeded, want noncanonical entity text rejected", value)
		}
	}
}

// TestParseFieldPathRefusesAnythingRicher is the boundary that keeps fkf from growing an
// expression language: anything jq can do beyond addressing belongs in a declared helper.
func TestParseFieldPathRefusesAnythingRicher(t *testing.T) {
	for _, raw := range []string{
		"id", ".a | .b", `.a[] | select(.x=="y")`, ".a[1:2]", ".a..b", "..", ".a[", `."unclosed`,
		".a[x]", "", "   ", ".a+.b",
	} {
		if _, err := ParseFieldPath(raw); err == nil {
			t.Fatalf("ParseFieldPath(%q) succeeded, want it refused at load", raw)
		}
	}
}

func TestFieldPathEval(t *testing.T) {
	document := decodeJSON(t, `{
		"number": 412,
		"repository": {"nameWithOwner": "fmind/fkf"},
		"to": [{"value": "a@x.test"}, {"value": "b@x.test"}],
		"headers": [{"name": "Subject", "value": "hello"}],
		"empty": null,
		"odd key": "yes",
		"big": 9007199254740993
	}`)

	cases := []struct {
		path string
		want []string
	}{
		{".number", []string{"412"}},
		{".repository.nameWithOwner", []string{"fmind/fkf"}},
		{".to[].value", []string{"a@x.test", "b@x.test"}},
		{".to[0].value", []string{"a@x.test"}},
		{".to[-1].value", []string{"b@x.test"}},
		{".headers[0].name", []string{"Subject"}},
		{`."odd key"`, []string{"yes"}},
		{".empty", nil},
		{".missing", nil},
		{".missing.deeper", nil},
		// A nineteen-digit id must survive the round trip; float64 would round it.
		{".big", []string{"9007199254740993"}},
	}
	for _, test := range cases {
		path, err := ParseFieldPath(test.path)
		if err != nil {
			t.Fatalf("ParseFieldPath(%q) error = %v", test.path, err)
		}
		got := path.EvalStrings(document)
		if !slices.Equal(got, test.want) {
			t.Fatalf("%s.EvalStrings() = %v, want %v", test.path, got, test.want)
		}
	}
}

// TestFieldPathEvalStringRequiresOneScalar keeps every consumer on the same cardinality rule:
// a declaration never silently changes from a union into "first value wins".
func TestFieldPathEvalStringRequiresOneScalar(t *testing.T) {
	path, _ := ParseFieldPath(".to[]")
	if got, ok := path.EvalString(decodeJSON(t, `{"to": ["first", "second"]}`)); ok {
		t.Fatalf("EvalString() = %q, true; want multiple values refused", got)
	}
	if _, ok := path.EvalString(decodeJSON(t, `{"to": [{"a": 1}]}`)); ok {
		t.Fatal("an object is not a scalar: a repo path landing on one is a configuration mistake")
	}
	if got, ok := path.EvalString(decodeJSON(t, `{"to": ["only"]}`)); !ok || got != "only" {
		t.Fatalf("EvalString() = %q, %v; want the one scalar", got, ok)
	}
}

// TestFieldPathIteratesObjectValuesDeterministically keeps a stored document byte-identical
// across runs when a source declares `.headers[]` over an object.
func TestFieldPathIteratesObjectValuesDeterministically(t *testing.T) {
	path, _ := ParseFieldPath(".labels[]")
	document := decodeJSON(t, `{"labels": {"z": "zulu", "a": "alpha", "m": "mike"}}`)
	for range 20 {
		if got := path.EvalStrings(document); !slices.Equal(got, []string{"alpha", "mike", "zulu"}) {
			t.Fatalf("EvalStrings() = %v, want the values in sorted key order every time", got)
		}
	}
}

func TestFieldPathJSONRoundTrip(t *testing.T) {
	original, _ := ParseFieldPath(`.a."odd key"[0]`)
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded FieldPath
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", encoded, err)
	}
	if decoded.String() != original.String() {
		t.Fatalf("round trip = %q, want %q", decoded.String(), original.String())
	}
	// A stored document must never hand a read an unvalidated path.
	if err := json.Unmarshal([]byte(`"a | b"`), &decoded); err == nil {
		t.Fatal("decoding a stored path outside the subset must fail")
	}
}

func TestFieldMapRoundTripKeepsOpenScalarOrListShape(t *testing.T) {
	id, _ := ParseFieldPath(".id")
	project, _ := ParseFieldPath(".project.key")
	fallback, _ := ParseFieldPath(".fallback_project")
	original := FieldMap{
		FieldID:   {id},
		"project": {project, fallback},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"id":".id","project":[".project.key",".fallback_project"]}` {
		t.Fatalf("Marshal() = %s, want the compact scalar-or-list public shape", encoded)
	}
	var decoded FieldMap
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	value, found := decoded.EvalString("project", decodeJSON(t, `{"fallback_project":"knowledge"}`))
	if !found || value != "knowledge" {
		t.Fatalf("EvalString(project) = %q, %v; want the second declared path as a fallback", value, found)
	}
	var invalid FieldPaths
	if err := json.Unmarshal([]byte(`".a | .b"`), &invalid); err == nil || !strings.Contains(err.Error(), ".a | .b") {
		t.Fatalf("invalid stored field-path error = %v, want the path parser's diagnostic", err)
	}
}

func TestFieldMapEvalStringRefusesAUnionWithSeveralValues(t *testing.T) {
	first, _ := ParseFieldPath(".first")
	second, _ := ParseFieldPath(".second")
	fields := FieldMap{"project": {first, second}}
	if got, ok := fields.EvalString("project", decodeJSON(t, `{"first":"one","second":"two"}`)); ok {
		t.Fatalf("EvalString(project) = %q, true; want the two-path union refused", got)
	}
}

func TestValidateFieldMapKeepsOnlyStructuralFieldsRequired(t *testing.T) {
	id, _ := ParseFieldPath(".id")
	fallback, _ := ParseFieldPath(".legacy_id")
	timestamp, _ := ParseFieldPath(".updated")
	topic, _ := ParseFieldPath(".topic")
	valid := FieldMap{FieldID: {fallback, id}, FieldTime: {fallback, timestamp}, "topic": {topic}}
	if err := ValidateFieldMap(valid, true); err != nil {
		t.Fatalf("ValidateFieldMap() rejected ordered fallback paths: %v", err)
	}

	cases := []struct {
		name   string
		fields FieldMap
		want   string
	}{
		{"missing identity", FieldMap{FieldTime: {timestamp}}, "fields.id is required"},
		{"missing event time", FieldMap{FieldID: {id}}, "fields.time is required"},
		{"invalid custom name", FieldMap{FieldID: {id}, "Project.Key": {topic}}, "field name"},
		{"empty custom projection", FieldMap{FieldID: {id}, "topic": {}}, "at least one path"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateFieldMap(test.fields, true)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateFieldMap() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestScalarString(t *testing.T) {
	cases := []struct {
		value any
		want  string
		ok    bool
	}{
		{"  text  ", "text", true},
		{"", "", false},
		{float64(412), "412", true},
		{float64(1.5), "1.5", true},
		{true, "true", true},
		{json.Number("9007199254740993"), "9007199254740993", true},
		{map[string]any{}, "", false},
		{[]any{}, "", false},
		{nil, "", false},
	}
	for _, test := range cases {
		got, ok := ScalarString(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("ScalarString(%#v) = %q, %v; want %q, %v", test.value, got, ok, test.want, test.ok)
		}
	}
}
