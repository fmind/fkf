package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaURL is where the generated schema is published, and what an editor's `# yaml-language-server`
// comment points at. `fkf config schema` prints the same bytes, so an offline editor never
// has to fetch it.
const SchemaURL = "https://fmind.github.io/fkf/fkf.schema.json"

// ConfigSchema returns the JSON Schema for fkf.yaml. It is written by hand rather than
// reflected from the structs on purpose: the interesting constraints — the placeholder set,
// the jq subset, `body` needing `{{id}}` — are not expressible as Go tags, and a schema that
// silently omits them would tell an editor a broken file is fine.
func ConfigSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  SchemaURL,
		"title":                "fkf base configuration",
		"description":          "The committed definition of one fkf base. It holds no secret.",
		"type":                 "object",
		"required":             []string{"fkf", "name", "schema", "layers"},
		"additionalProperties": false,
		"properties": map[string]any{
			"fkf": map[string]any{
				"type": "integer", "const": ConfigVersion,
				"description": "Configuration contract marker. v1 accepts exactly fkf: 1.",
			},
			"name": map[string]any{
				"type": "string", "pattern": `^[a-z0-9][a-z0-9-]*$`, "maxLength": MaxBaseNameLength,
				"description": "MCP server name and resource URI authority; informational elsewhere.",
			},
			"layers": map[string]any{
				"type": "object", "additionalProperties": false,
				"description": "Explicit activation. A disabled layer is not created, listed, served, or scanned.",
				"properties":  layerSchemaProperties(),
			},
			"schema": fieldDefinitionSchema(),
			"identities": map[string]any{
				"type":                 "object",
				"description":          "Declared exact aliases for canonical people, organizations, and repositories.",
				"propertyNames":        map[string]any{"pattern": `^[a-z0-9][a-z0-9-]*$`, "maxLength": MaxBaseNameLength},
				"additionalProperties": identitySchema(),
			},
			"bin": stringArray("Absolute or ~-relative machine-local directories outside the base, prepended to PATH for every declared command. Put base-controlled executables in <base>/bin so trust hashes them."),
			"sources": map[string]any{
				"type":                 "object",
				"description":          "Declared collection commands, keyed by <provider>-<resource>.",
				"propertyNames":        map[string]any{"pattern": `^[a-z0-9][a-z0-9-]*$`, "maxLength": MaxSourceNameLength},
				"additionalProperties": sourceSchema(),
			},
			"sync": syncSchema(),
		},
	}
}

func identitySchema() map[string]any {
	entity := map[string]any{
		"type": "string", "pattern": `^[a-z][a-z0-9+.-]*:[^\s]+$`,
		"description": "Canonical entity URI using an open non-reserved scheme.",
	}
	alias := map[string]any{
		"type": "string", "minLength": 1, "maxLength": 320,
		"pattern": `^(?:[a-z][a-z0-9+.-]*:[^\s]+|[A-Za-z0-9][A-Za-z0-9._+@-]*)$`,
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"canonical", "aliases"},
		"properties": map[string]any{
			"canonical": entity,
			"aliases": map[string]any{
				"type": "array", "minItems": 1, "uniqueItems": true, "items": alias,
				"description": "Exact entity URIs, emails, or provider logins that resolve to canonical.",
			},
			"kind": map[string]any{
				"type": "string", "enum": []string{string(IdentityPerson), string(IdentityOrganization), string(IdentityRepository)},
				"description": "Optional classification used by graph and people views.",
			},
			"owner": map[string]any{
				"type": "boolean", "default": false,
				"description": "Marks the one owning person, omitted from ambient people discovery and expansion.",
			},
		},
	}
}

func fieldDefinitionSchema() map[string]any {
	return map[string]any{
		"type": "object", "minProperties": 1, "maxProperties": MaxFields,
		"propertyNames": map[string]any{
			"pattern": `^[a-z][a-z0-9_-]*$`, "maxLength": MaxFieldNameLength,
		},
		"additionalProperties": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"description", "cardinality"},
			"properties": map[string]any{
				"description": map[string]any{
					"type": "string", "minLength": 1, "maxLength": MaxFieldDescriptionLength,
					"description": "Shared human and machine meaning of this field across sources and authored relations.",
				},
				"cardinality": map[string]any{
					"type": "string", "enum": []string{string(CardinalityOne), string(CardinalityOptional), string(CardinalityMany)},
					"description": "Allowed scalar count per record or explicit Markdown relation list.",
				},
				"relation": map[string]any{
					"type": "boolean", "default": false,
					"description": "Values are canonical fkf URIs transcribed as graph edges of this field name.",
				},
				"weight": map[string]any{
					"type": "integer", "minimum": 1, "maximum": MaxFieldWeight,
					"description": "Optional lexical-ranking multiplier. Defaults to 10 for id, 5 for title, and 1 for every other field.",
				},
				"examples": map[string]any{
					"type": "array", "maxItems": MaxFieldExamples,
					"items":       map[string]any{"type": "string", "maxLength": MaxFieldExampleLength},
					"description": "Bounded examples that clarify the semantic value or URI shape.",
				},
			},
		},
		"required":    []string{FieldID},
		"description": "Open semantic dictionary shared by every source and authored relation. id must have cardinality one; fkf enforces cross-field rules while loading.",
	}
}

func layerSchemaProperties() map[string]any {
	descriptions := map[Layer]string{
		LayerEvents:   "Dated collected documents (JSON).",
		LayerIndex:    "Point-in-time collected documents (JSON).",
		LayerTasks:    "Execution evidence (Markdown).",
		LayerProjects: "Intent and decisions over weeks (Markdown, status-bearing).",
		LayerWiki:     "Durable approved knowledge (Markdown, OKF v0.2).",
	}
	properties := make(map[string]any, len(Layers))
	for _, layer := range Layers {
		properties[string(layer)] = map[string]any{"type": "boolean", "description": descriptions[layer]}
	}
	return properties
}

func sourceSchema() map[string]any {
	fieldPath := func(description string) map[string]any {
		return map[string]any{
			"type": "string", "pattern": `^\.`,
			"description": description + " A jq subset: .key, .a.b, [n], [], .\"odd key\".",
		}
	}
	fieldPaths := func(description string) map[string]any {
		path := fieldPath(description)
		return map[string]any{
			"oneOf": []any{
				path,
				map[string]any{
					"type": "array", "minItems": 1, "maxItems": MaxPathsPerField,
					"items": path,
				},
			},
		}
	}
	fields := map[string]any{
		"type": "object", "required": []string{FieldID, FieldTitle}, "minProperties": 2, "maxProperties": MaxFields,
		"propertyNames": map[string]any{
			"pattern": `^[a-z][a-z0-9_-]*$`, "maxLength": MaxFieldNameLength,
		},
		"additionalProperties": fieldPaths("A declared semantic projection indexed for lexical context; every path contributes scalar values."),
		"properties": map[string]any{
			FieldID:         fieldPaths("Required. Exactly one scalar is the record identity that its URI fragment names."),
			FieldTime:       fieldPaths("Required for an events source. Exactly one scalar is the record timestamp."),
			FieldTitle:      fieldPaths("Required meaningful human-readable subject line; at most one scalar."),
			FieldURL:        fieldPaths("Suggested provider URL; at most one scalar."),
			FieldCategory:   fieldPaths("Optional authorship role: created, received, or saved; at most one scalar."),
			FieldVisibility: fieldPaths("Optional audience role: private, shared, or public; at most one scalar."),
		},
		"description": "Associates root schema names with provider paths. id and title are required, plus time for events; every declared value is indexed lexically and relation fields are transcribed into the graph.",
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"run"},
		"allOf": []any{
			map[string]any{
				"if": map[string]any{
					"properties": map[string]any{"layer": map[string]any{"const": string(LayerTasks)}},
					"required":   []string{"layer"},
				},
				"then": map[string]any{
					"required":   []string{"window"},
					"properties": map[string]any{"window": map[string]any{"const": true}},
					"not": map[string]any{"anyOf": []any{
						map[string]any{"required": []string{"records"}},
						map[string]any{"required": []string{"fields"}},
						map[string]any{"required": []string{"body"}},
						map[string]any{"required": []string{"bodies"}},
						map[string]any{"required": []string{"recency"}},
					}},
				},
				"else": map[string]any{"required": []string{"fields"}},
			},
			map[string]any{
				"if": map[string]any{"anyOf": []any{
					map[string]any{"not": map[string]any{"required": []string{"layer"}}},
					map[string]any{"properties": map[string]any{"layer": map[string]any{"const": string(LayerEvents)}}},
				}},
				"then": map[string]any{
					"properties": map[string]any{"fields": map[string]any{"required": []string{FieldID, FieldTime, FieldTitle}}},
				},
			},
		},
		"properties": map[string]any{
			"enabled": map[string]any{"type": "boolean", "description": "Whether sync runs this source. Disabled entries are still validated."},
			"layer": map[string]any{
				"type": "string", "enum": []string{string(LayerEvents), string(LayerIndex), string(LayerTasks)}, "default": string(LayerEvents),
				"description": "events files one JSON document per day; index files one point-in-time JSON document; tasks writes bounded Markdown session traces.",
			},
			"auth": map[string]any{
				"type": "array", "minItems": 1, "items": map[string]any{"type": "string"},
				"prefixItems": []any{map[string]any{
					"type": "string", "minLength": 1,
					"not":         map[string]any{"pattern": `\{\{`},
					"description": "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the base.",
				}},
				"description": "Optional direct argv that checks provider login readiness before collection. It accepts no placeholders; stdout and stderr are discarded and never logged.",
			},
			"run": map[string]any{
				"type": "array", "minItems": 1, "items": map[string]any{"type": "string"},
				"prefixItems": []any{map[string]any{
					"type": "string", "minLength": 1,
					"not":         map[string]any{"pattern": `\{\{`},
					"description": "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the base.",
				}},
				"description": "Direct argv producing JSON; no shell parses it. A helper shebang selects its interpreter. Argument placeholders: " + placeholderList(RunPlaceholders) + ". No collected data is ever substituted.",
			},
			"test": map[string]any{
				"type": "array", "minItems": 1, "items": map[string]any{"type": "string"},
				"prefixItems": []any{map[string]any{
					"type": "string", "minLength": 1,
					"not":         map[string]any{"pattern": `\{\{`},
					"description": "Literal executable: a bare name resolved on the test-only PATH (tests/ first), or an absolute machine-local path outside the base.",
				}},
				"description": "Optional direct argv run by `fkf test` to verify this source. The trusted base tests/ tree is prepended only for this command; it receives no record or collection window. Argument placeholders: " + placeholderList(TestPlaceholders) + ".",
			},
			"format": map[string]any{
				"type": "string", "enum": []string{string(FormatJSON), string(FormatNDJSON)}, "default": string(FormatJSON),
				"description": "json expects one document; ndjson expects one JSON value per line.",
			},
			"records": fieldPath("Path to the records inside each decoded document or page."),
			"fields":  fields,
			"body": map[string]any{
				"type": "array", "minItems": 2, "items": map[string]any{"type": "string"},
				"prefixItems": []any{map[string]any{
					"type": "string", "minLength": 1,
					"not":         map[string]any{"pattern": `\{\{`},
					"description": "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the base. Placeholders and base-relative paths are refused.",
				}},
				"description": "Argv (never a shell string) fetching one record's body on demand. Must name {{id}} and may name any declared field plus {{base}} or {{home}}.",
			},
			"bodies": map[string]any{
				"type": "string", "enum": []string{string(BodiesNone), string(BodiesCache), string(BodiesSync)}, "default": string(BodiesNone),
				"description": "Rebuildable body-cache policy: none never stores, cache stores after explicit read --body, and sync also prefetches missing bodies during collection.",
			},
			"recency": map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"half_life_days"},
				"properties": map[string]any{
					"half_life_days": map[string]any{
						"type": "integer", "minimum": 1, "maximum": MaxRecencyHalfLifeDays,
						"description": "Source-local exponential recency half-life; undated records receive no bonus.",
					},
				},
			},
			"requires": map[string]any{
				"type": "array", "uniqueItems": true,
				"items":       map[string]any{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._+-]*$`},
				"description": "Executable names fkf status checks on the ordinary collection/body PATH, including helpers and non-standard interpreters. FKF reports test[0] readiness separately on the test-only tests/ PATH and never infers dependencies from argv or helper contents.",
			},
			"install": map[string]any{"type": "string", "description": "Printed by `fkf status` when the binary is missing. Never executed."},
			"timeout": duration("Per-command timeout; overrides sync.timeout."),
			"min_interval": duration("Least time between two invocations of THIS source across the " +
				"whole sync. Retry spaces the attempts of one failing call; this spaces every call, " +
				"which is what a provider's rate limit actually counts."),
			"retry": map[string]any{
				"type": "object", "additionalProperties": false,
				"description": "How fkf re-invokes this command, never what it is — the same relationship " +
					"`timeout:` has to `run:`. `fkf trust` prints it beside the line it modifies.",
				"properties": map[string]any{
					"attempts": map[string]any{
						"type": "integer", "minimum": 1, "maximum": MaxRetryAttempts,
						"description": "Total runs allowed, including the first.",
					},
					"backoff": duration("Wait before the next attempt, growing linearly with the attempt number."),
					"on": map[string]any{
						"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1},
						"description": "Which failures may be retried: `exit:<n>`, or a substring matched " +
							"against the command's stderr. Required whenever attempts exceeds one — retrying " +
							"every failure is how a source failing for a real reason hammers a provider quietly. " +
							"The matched text is never logged or stored.",
					},
				},
				"dependentRequired": map[string]any{"backoff": []string{"attempts"}, "on": []string{"attempts"}},
			},
			"window": map[string]any{
				"type": "boolean", "default": false,
				"description": "Render run: ONCE for the whole requested range — {{start}}/{{end}} span " +
					"every day being collected, not one. Events bucket records by fields.time; tasks sources " +
					"must enable it and import completed session traces; index sources reject it.",
			},
		},
	}
}

func syncSchema() map[string]any {
	defaults := DefaultSync()
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"days": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 366, "default": defaults.Days,
				"description": "Completed local days to collect when no --date is given.",
			},
			"index_max_age_hours": map[string]any{
				"type": "integer", "minimum": 1, "maximum": MaxFreshnessAgeHours, "default": defaults.IndexMaxAgeHours,
				"description": fmt.Sprintf("Refresh an index document only when it is older than this; 1..%d.", MaxFreshnessAgeHours),
			},
			"timeout":     duration("Per-command timeout, 1s..1h."),
			"concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxSyncConcurrency, "default": defaults.Concurrency},
		},
	}
}

func duration(description string) map[string]any {
	return map[string]any{
		"type": "string", "pattern": `^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$`, "description": description,
	}
}

func stringArray(description string) map[string]any {
	return map[string]any{
		"type": "array", "items": map[string]any{"type": "string"}, "description": description,
	}
}

func placeholderList(names []string) string {
	rendered := make([]string, 0, len(names))
	for _, name := range names {
		rendered = append(rendered, "{{"+name+"}}")
	}
	return strings.Join(rendered, ", ")
}

// EncodeConfigSchema renders the schema exactly as it is published: two-space indented JSON
// with a trailing newline, which is what dprint and the docs site expect.
func EncodeConfigSchema() ([]byte, error) {
	encoded, err := json.MarshalIndent(ConfigSchema(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode configuration schema: %w", err)
	}
	return append(encoded, '\n'), nil
}
