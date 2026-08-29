package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The schema is published for editor completion, so the interesting property is that it stays
// in agreement with what the loader actually enforces. These tests check the constraints a
// wrong schema would silently drop.
func TestConfigSchemaDescribesWhatTheLoaderEnforces(t *testing.T) {
	schema := ConfigSchema()
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		t.Fatal("the schema has no properties")
	}
	for _, key := range []string{"fkf", "name", "schema", "layers", "sources", "sync", "bin"} {
		if _, ok := properties[key]; !ok {
			t.Fatalf("the schema omits %q, so an editor would flag a valid file", key)
		}
	}
	if _, exists := properties["graph"]; exists {
		t.Fatal("the schema publishes graph, but every graph build is a canonical full rebuild")
	}
	if _, exists := properties["env"]; exists {
		t.Fatal("the schema still publishes the removed base environment configuration")
	}
	if schema["additionalProperties"] != false {
		t.Fatal("the schema must be closed: the loader rejects unknown keys")
	}
	sync := mustSchemaObject(t, properties["sync"], "sync")
	syncProperties := mustSchemaObject(t, sync["properties"], "sync properties")
	indexAge := mustSchemaObject(t, syncProperties["index_max_age_hours"], "index_max_age_hours")
	if indexAge["maximum"] != MaxFreshnessAgeHours {
		t.Fatalf("index_max_age_hours maximum = %v, want loader maximum %d",
			indexAge["maximum"], MaxFreshnessAgeHours)
	}
	concurrency := mustSchemaObject(t, syncProperties["concurrency"], "concurrency")
	if concurrency["maximum"] != MaxSyncConcurrency {
		t.Fatalf("concurrency maximum = %v, want loader maximum %d", concurrency["maximum"], MaxSyncConcurrency)
	}
	layersEntry, ok := properties["layers"].(map[string]any)
	if !ok {
		t.Fatal("the schema does not describe layers as an object")
	}
	layers, _ := layersEntry["properties"].(map[string]any)
	for _, layer := range Layers {
		if _, ok := layers[string(layer)]; !ok {
			t.Fatalf("the schema omits the %s layer", layer)
		}
	}
	sources := mustSchemaObject(t, properties["sources"], "sources")
	sourceNames := mustSchemaObject(t, sources["propertyNames"], "source property names")
	if sourceNames["maxLength"] != MaxSourceNameLength {
		t.Fatalf("source-name maximum = %v, want loader maximum %d", sourceNames["maxLength"], MaxSourceNameLength)
	}
	source := mustSchemaObject(t, sources["additionalProperties"], "source")
	sourceProperties := mustSchemaObject(t, source["properties"], "source properties")
	required, _ := source["required"].([]string)
	if strings.Join(required, ",") != "run,fields" {
		t.Fatalf("the source shape requires %v; the loader requires run and fields", required)
	}
	assertSchemaDescriptionHasPlaceholders(t, source, "run", RunPlaceholders)
	assertSchemaDescriptionHasPlaceholders(t, source, "test", TestPlaceholders)
	if !strings.Contains(description(t, source, "body"), "{{id}}") {
		t.Fatal("the body description must say it has to name {{id}}")
	}
	fields := mustSchemaObject(t, sourceProperties["fields"], "fields")
	fieldNames := mustSchemaObject(t, fields["propertyNames"], "field names")
	if fieldNames["maxLength"] != MaxFieldNameLength || fields["maxProperties"] != MaxFields {
		t.Fatalf("field-map bounds = name %v, fields %v; want %d and %d",
			fieldNames["maxLength"], fields["maxProperties"], MaxFieldNameLength, MaxFields)
	}
	fieldProperties := mustSchemaObject(t, fields["properties"], "well-known fields")
	for _, name := range wellKnownFields {
		entry, present := fieldProperties[name]
		if !present {
			t.Fatalf("the schema omits suggested field %q", name)
		}
		alternatives, _ := mustSchemaObject(t, entry, "well-known field "+name)["oneOf"].([]any)
		if len(alternatives) != 2 {
			t.Fatalf("the schema gives field %q %d shape(s); the loader accepts one path or an ordered list", name, len(alternatives))
		}
	}
	if _, open := fields["additionalProperties"].(map[string]any); !open {
		t.Fatal("the fields map is closed; user-defined semantic projections must be allowed")
	}
	if _, exists := sourceProperties["lookup"]; exists {
		t.Fatal("the schema still publishes the removed lookup-only execution surface")
	}
	if _, exists := sourceProperties["env"]; exists {
		t.Fatal("the schema still publishes the removed source environment configuration")
	}
	requires := mustSchemaObject(t, sourceProperties["requires"], "requires")
	if requires["uniqueItems"] != true {
		t.Fatal("the schema must reject duplicate executable requirements")
	}
	assertExecutionAndDurationSchema(t, properties, sourceProperties)
}

func assertSchemaDescriptionHasPlaceholders(t *testing.T, source map[string]any, key string, placeholders []string) {
	t.Helper()
	for _, placeholder := range placeholders {
		if !strings.Contains(description(t, source, key), "{{"+placeholder+"}}") {
			t.Fatalf("the %s description omits {{%s}}", key, placeholder)
		}
	}
}

func assertExecutionAndDurationSchema(t *testing.T, properties, sourceProperties map[string]any) {
	t.Helper()
	bin := mustSchemaObject(t, properties["bin"], "bin")
	if !strings.Contains(fmt.Sprint(bin["description"]), "outside the base") {
		t.Fatal("the bin description does not publish the loader's outside-base boundary")
	}
	timeout := mustSchemaObject(t, sourceProperties["timeout"], "source timeout")
	durationPattern := fmt.Sprint(timeout["pattern"])
	for _, value := range []string{"1h30m", "1.5s", "250ms", "0"} {
		matched, err := regexp.MatchString(durationPattern, value)
		if err != nil || !matched {
			t.Errorf("duration pattern %q rejects %q, which time.ParseDuration accepts", durationPattern, value)
		}
	}
	for _, value := range []string{"", "1", "1d", "1h-30m"} {
		matched, err := regexp.MatchString(durationPattern, value)
		if err != nil {
			t.Fatalf("invalid duration pattern %q: %v", durationPattern, err)
		}
		if matched {
			t.Errorf("duration pattern %q accepts invalid duration %q", durationPattern, value)
		}
	}
}

func mustSchemaObject(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is not a schema object: %T", name, value)
	}
	return object
}

// description reads one property's description, failing the test rather than panicking when the
// schema's shape is not what it claims.
func description(t *testing.T, object map[string]any, property string) string {
	t.Helper()
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema object has no properties: %v", object)
	}
	entry, ok := properties[property].(map[string]any)
	if !ok {
		t.Fatalf("the schema omits the %q property", property)
	}
	text, ok := entry["description"].(string)
	if !ok {
		t.Fatalf("the %q property has no description", property)
	}
	return text
}

func TestEncodeConfigSchemaIsStableJSON(t *testing.T) {
	encoded, err := EncodeConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(encoded), "}\n") {
		t.Fatal("the published schema must end in a newline, like every other file dprint owns")
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the encoded schema is not valid JSON: %v", err)
	}
	second, err := EncodeConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(encoded) {
		t.Fatal("EncodeConfigSchema() must be byte-identical across calls")
	}
}

// TestPublishedSchemaMatchesTheBinary catches the drift that matters to a reader: the file the
// docs site serves and the rules this build enforces have to be the same generation.
func TestPublishedSchemaMatchesTheBinary(t *testing.T) {
	path := filepath.Join("..", "docs", "static", "fkf.schema.json")
	published, err := os.ReadFile(path)
	if err != nil {
		// Not a skip. `mise run check:schema` runs exactly this test, so skipping on a missing
		// file made the gate report success for the one state it exists to catch: the published
		// schema gone or moved while editors keep validating against it.
		t.Fatalf("no published schema at %s: %v (regenerate it with `mise run generate:schema`)", path, err)
	}
	generated, err := EncodeConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	if string(published) != string(generated) {
		t.Fatalf("%s is stale; regenerate it with `mise run generate:schema`", path)
	}
}

// TestSchemaPropertyNamesMatchTheDecoder is the check that was missing when the schema
// advertised a source key named `kind` while the decoder had always read `layer`. The
// published schema sets additionalProperties:false, so a name only the generator believes in
// does not merely lose completion — it makes every real fkf.yaml an editor error. Comparing
// the file to the generator, which is what TestPublishedSchemaMatchesTheBinary does, cannot
// see that: both sides of that comparison come from the same wrong string.
func TestSchemaPropertyNamesMatchTheDecoder(t *testing.T) {
	encoded, err := EncodeConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []struct {
		name     string
		value    any
		schemaAt []string
	}{
		{"fileConfig", fileConfig{}, []string{"properties"}},
		{"fileSource", fileSource{}, []string{"properties", "sources", "additionalProperties", "properties"}},
		{"fileSync", fileSync{}, []string{"properties", "sync", "properties"}},
	} {
		declared := yamlTagNames(shape.value)
		published := schemaKeysAt(t, schema, shape.schemaAt)
		for name := range published {
			if !declared[name] {
				t.Errorf("%s: the schema publishes %q, which %s does not decode; a name only the "+
					"generator believes in is an editor error on every real fkf.yaml",
					shape.name, name, shape.name)
			}
		}
		for name := range declared {
			if !published[name] {
				t.Errorf("%s: the decoder reads %q, which the schema does not publish; with "+
					"additionalProperties:false an editor rejects the key the loader requires",
					shape.name, name)
			}
		}
	}
}

// yamlTagNames returns the yaml keys a struct actually decodes.
func yamlTagNames(value any) map[string]bool {
	names := make(map[string]bool)
	structType := reflect.TypeOf(value)
	for index := range structType.NumField() {
		tag, _, _ := strings.Cut(structType.Field(index).Tag.Get("yaml"), ",")
		if tag != "" && tag != "-" {
			names[tag] = true
		}
	}
	return names
}

// schemaKeysAt walks the generated schema to one "properties" map and returns its key set.
func schemaKeysAt(t *testing.T, schema map[string]any, path []string) map[string]bool {
	t.Helper()
	var current any = schema
	for _, step := range path {
		node, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: %q is not an object", path, step)
		}
		if current, ok = node[step]; !ok {
			t.Fatalf("schema path %v: no %q", path, step)
		}
	}
	node, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("schema path %v does not end at an object", path)
	}
	names := make(map[string]bool, len(node))
	for name := range node {
		names[name] = true
	}
	return names
}
