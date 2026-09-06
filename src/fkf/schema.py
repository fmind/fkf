"""The hand-authored, published JSON Schema for ``fkf.yaml``."""

from __future__ import annotations

import json
from typing import Final

from fkf.config import (
    CONFIG_VERSION,
    MAX_BASE_NAME_LENGTH,
    MAX_FRESHNESS_AGE_HOURS,
    MAX_RECENCY_HALF_LIFE_DAYS,
    MAX_RETRY_ATTEMPTS,
    MAX_SOURCE_NAME_LENGTH,
    MAX_SYNC_CONCURRENCY,
    RUN_PLACEHOLDERS,
    TEST_PLACEHOLDERS,
    BodyPolicy,
    IdentityKind,
    OutputFormat,
    SyncConfig,
)
from fkf.fields import (
    FIELD_CATEGORY,
    FIELD_ID,
    FIELD_TIME,
    FIELD_TITLE,
    FIELD_URL,
    FIELD_VISIBILITY,
    MAX_FIELD_DESCRIPTION_LENGTH,
    MAX_FIELD_EXAMPLE_LENGTH,
    MAX_FIELD_EXAMPLES,
    MAX_FIELD_NAME_LENGTH,
    MAX_FIELD_WEIGHT,
    MAX_FIELDS,
    MAX_PATHS_PER_FIELD,
    Cardinality,
)
from fkf.store import LAYERS, Layer

SCHEMA_URL: Final = "https://fmind.github.io/fkf/fkf.schema.json"


def config_schema() -> dict[str, object]:
    """Return the closed JSON Schema shared by the loader, CLI, and docs."""
    return {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "$id": SCHEMA_URL,
        "title": "fkf base configuration",
        "description": "The committed definition of one fkf base. It holds no secret.",
        "type": "object",
        "required": ["fkf", "name", "schema", "layers"],
        "additionalProperties": False,
        "properties": {
            "fkf": {
                "type": "integer",
                "const": CONFIG_VERSION,
                "description": "Configuration contract marker. v1 accepts exactly fkf: 1.",
            },
            "name": {
                "type": "string",
                "pattern": r"^[a-z0-9][a-z0-9-]*$",
                "maxLength": MAX_BASE_NAME_LENGTH,
                "description": "MCP server name and resource URI authority; informational elsewhere.",
            },
            "layers": {
                "type": "object",
                "additionalProperties": False,
                "description": "Explicit activation. A disabled layer is not created, listed, served, or scanned.",
                "properties": _layer_schema_properties(),
            },
            "schema": _field_definition_schema(),
            "identities": {
                "type": "object",
                "description": "Declared exact aliases for canonical people, organizations, and repositories.",
                "propertyNames": {
                    "pattern": r"^[a-z0-9][a-z0-9-]*$",
                    "maxLength": MAX_BASE_NAME_LENGTH,
                },
                "additionalProperties": _identity_schema(),
            },
            "bin": _string_array(
                "Absolute or ~-relative machine-local directories outside the base, prepended to PATH for every "
                "declared command. Put base-controlled executables in <base>/bin so trust hashes them."
            ),
            "sources": {
                "type": "object",
                "description": "Declared collection commands, keyed by <provider>-<resource>.",
                "propertyNames": {
                    "pattern": r"^[a-z0-9][a-z0-9-]*$",
                    "maxLength": MAX_SOURCE_NAME_LENGTH,
                },
                "additionalProperties": _source_schema(),
            },
            "sync": _sync_schema(),
        },
    }


def _identity_schema() -> dict[str, object]:
    entity = {
        "type": "string",
        "pattern": r"^[a-z][a-z0-9+.-]*:[^\s]+$",
        "description": "Canonical entity URI using an open non-reserved scheme.",
    }
    alias = {
        "type": "string",
        "minLength": 1,
        "maxLength": 320,
        "pattern": r"^(?:[a-z][a-z0-9+.-]*:[^\s]+|[A-Za-z0-9][A-Za-z0-9._+@-]*)$",
    }
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["canonical", "aliases"],
        "properties": {
            "canonical": entity,
            "aliases": {
                "type": "array",
                "minItems": 1,
                "uniqueItems": True,
                "items": alias,
                "description": "Exact entity URIs, emails, or provider logins that resolve to canonical.",
            },
            "kind": {
                "type": "string",
                "enum": [kind.value for kind in IdentityKind],
                "description": "Optional classification used by graph and people views.",
            },
            "owner": {
                "type": "boolean",
                "default": False,
                "description": "Marks the one owning person, omitted from ambient people discovery and expansion.",
            },
        },
    }


def _field_definition_schema() -> dict[str, object]:
    return {
        "type": "object",
        "minProperties": 1,
        "maxProperties": MAX_FIELDS,
        "propertyNames": {
            "pattern": r"^[a-z][a-z0-9_-]*$",
            "maxLength": MAX_FIELD_NAME_LENGTH,
        },
        "additionalProperties": {
            "type": "object",
            "additionalProperties": False,
            "required": ["description", "cardinality"],
            "properties": {
                "description": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_FIELD_DESCRIPTION_LENGTH,
                    "description": "Shared human and machine meaning of this field across sources and authored relations.",
                },
                "cardinality": {
                    "type": "string",
                    "enum": [cardinality.value for cardinality in Cardinality],
                    "description": "Allowed scalar count per record or explicit Markdown relation list.",
                },
                "relation": {
                    "type": "boolean",
                    "default": False,
                    "description": "Values are canonical fkf URIs transcribed as graph edges of this field name.",
                },
                "weight": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": MAX_FIELD_WEIGHT,
                    "description": "Optional lexical-ranking multiplier. Defaults to 10 for id, 5 for title, and 1 for every other field.",
                },
                "examples": {
                    "type": "array",
                    "maxItems": MAX_FIELD_EXAMPLES,
                    "items": {"type": "string", "maxLength": MAX_FIELD_EXAMPLE_LENGTH},
                    "description": "Bounded examples that clarify the semantic value or URI shape.",
                },
            },
        },
        "required": [FIELD_ID],
        "description": "Open semantic dictionary shared by every source and authored relation. id must have "
        "cardinality one; fkf enforces cross-field rules while loading.",
    }


def _layer_schema_properties() -> dict[str, object]:
    descriptions = {
        Layer.EVENTS: "Dated collected documents (JSON).",
        Layer.INDEX: "Point-in-time collected documents (JSON).",
        Layer.TASKS: "Execution evidence (Markdown).",
        Layer.PROJECTS: "Intent and decisions over weeks (Markdown, status-bearing).",
        Layer.WIKI: "Durable approved knowledge (Markdown, OKF v0.2).",
    }
    return {layer.value: {"type": "boolean", "description": descriptions[layer]} for layer in LAYERS}


def _field_path(description: str) -> dict[str, object]:
    return {
        "type": "string",
        "pattern": r"^\.",
        "description": f'{description} A jq subset: .key, .a.b, [n], [], ."odd key".',
    }


def _field_paths(description: str) -> dict[str, object]:
    path = _field_path(description)
    return {
        "oneOf": [
            path,
            {
                "type": "array",
                "minItems": 1,
                "maxItems": MAX_PATHS_PER_FIELD,
                "items": path,
            },
        ]
    }


def _source_fields_schema() -> dict[str, object]:
    return {
        "type": "object",
        "required": [FIELD_ID, FIELD_TITLE],
        "minProperties": 2,
        "maxProperties": MAX_FIELDS,
        "propertyNames": {
            "pattern": r"^[a-z][a-z0-9_-]*$",
            "maxLength": MAX_FIELD_NAME_LENGTH,
        },
        "additionalProperties": _field_paths(
            "A declared semantic projection indexed for lexical context; every path contributes scalar values."
        ),
        "properties": {
            FIELD_ID: _field_paths("Required. Exactly one scalar is the record identity that its URI fragment names."),
            FIELD_TIME: _field_paths("Required for an events source. Exactly one scalar is the record timestamp."),
            FIELD_TITLE: _field_paths("Required meaningful human-readable subject line; at most one scalar."),
            FIELD_URL: _field_paths("Suggested provider URL; at most one scalar."),
            FIELD_CATEGORY: _field_paths("Optional authorship role: created, received, or saved; at most one scalar."),
            FIELD_VISIBILITY: _field_paths("Optional audience role: private, shared, or public; at most one scalar."),
        },
        "description": "Associates root schema names with provider paths. id and title are required, plus time for "
        "events; every declared value is indexed lexically and relation fields are transcribed into the graph.",
    }


def _executable_prefix(description: str) -> list[dict[str, object]]:
    return [
        {
            "type": "string",
            "minLength": 1,
            "not": {"pattern": r"\{\{"},
            "description": description,
        }
    ]


def _source_schema() -> dict[str, object]:
    fields = _source_fields_schema()
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["run"],
        "allOf": [
            {"required": ["fields"]},
            {
                "if": {
                    "anyOf": [
                        {"not": {"required": ["layer"]}},
                        {"properties": {"layer": {"const": Layer.EVENTS.value}}},
                    ]
                },
                "then": {
                    "properties": {
                        "fields": {"required": [FIELD_ID, FIELD_TIME, FIELD_TITLE]},
                    }
                },
            },
            {
                "if": {"required": ["max_age_hours"]},
                "then": {
                    "required": ["layer"],
                    "properties": {"layer": {"const": Layer.INDEX.value}},
                },
            },
        ],
        "properties": {
            "enabled": {
                "type": "boolean",
                "description": "Whether sync runs this source. Disabled entries are still validated.",
            },
            "layer": {
                "type": "string",
                "enum": [Layer.EVENTS.value, Layer.INDEX.value],
                "default": Layer.EVENTS.value,
                "description": "events files one JSON document per day; index files one point-in-time JSON document.",
            },
            "max_age_hours": {
                "type": "integer",
                "minimum": 1,
                "maximum": MAX_FRESHNESS_AGE_HOURS,
                "description": "Refresh this index source after this many hours; overrides sync.index_max_age_hours.",
            },
            "auth": {
                "type": "array",
                "minItems": 1,
                "items": {"type": "string"},
                "prefixItems": _executable_prefix(
                    "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the base."
                ),
                "description": "Optional direct argv that checks provider login readiness before collection. It "
                "accepts no placeholders; stdout and stderr are discarded and never logged.",
            },
            "run": {
                "type": "array",
                "minItems": 1,
                "items": {"type": "string"},
                "prefixItems": _executable_prefix(
                    "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the base."
                ),
                "description": "Direct argv producing JSON; no shell parses it. A helper shebang selects its "
                f"interpreter. Argument placeholders: {_placeholder_list(RUN_PLACEHOLDERS)}. No collected data is "
                "ever substituted.",
            },
            "test": {
                "type": "array",
                "minItems": 1,
                "items": {"type": "string"},
                "prefixItems": _executable_prefix(
                    "Literal executable: a bare name resolved on the test-only PATH (tests/ first), or an absolute "
                    "machine-local path outside the base."
                ),
                "description": "Optional direct argv run by `fkf test` to verify this source. The trusted base "
                "tests/ tree is prepended only for this command; it receives no record or collection window. "
                f"Argument placeholders: {_placeholder_list(TEST_PLACEHOLDERS)}.",
            },
            "format": {
                "type": "string",
                "enum": [format_.value for format_ in OutputFormat],
                "default": OutputFormat.JSON.value,
                "description": "json expects one document; ndjson expects one JSON value per line.",
            },
            "records": _field_path("Path to the records inside each decoded document or page."),
            "fields": fields,
            "body": {
                "type": "array",
                "minItems": 2,
                "items": {"type": "string"},
                "prefixItems": _executable_prefix(
                    "Literal executable: a bare name resolved on PATH or an absolute machine-local path outside the "
                    "base. Placeholders and base-relative paths are refused."
                ),
                "description": "Argv (never a shell string) fetching one record's body on demand. Must name {{id}} "
                "and may name any declared field plus {{base}} or {{home}}.",
            },
            "bodies": {
                "type": "string",
                "enum": [policy.value for policy in BodyPolicy],
                "default": BodyPolicy.NONE.value,
                "description": "Rebuildable body-cache policy: none never stores, cache stores after explicit read "
                "--body, and sync also prefetches missing bodies during collection.",
            },
            "recency": {
                "type": "object",
                "additionalProperties": False,
                "required": ["half_life_days"],
                "properties": {
                    "half_life_days": {
                        "type": "integer",
                        "minimum": 1,
                        "maximum": MAX_RECENCY_HALF_LIFE_DAYS,
                        "description": "Source-local exponential recency half-life; undated records receive no bonus.",
                    }
                },
            },
            "requires": {
                "type": "array",
                "uniqueItems": True,
                "items": {"type": "string", "pattern": r"^[A-Za-z0-9][A-Za-z0-9._+-]*$"},
                "description": "Executable names fkf status checks on the ordinary collection/body PATH, including "
                "helpers and non-standard interpreters. FKF reports test[0] readiness separately on the test-only "
                "tests/ PATH and never infers dependencies from argv or helper contents.",
            },
            "install": {
                "type": "string",
                "description": "Printed by `fkf status` when the binary is missing. Never executed.",
            },
            "timeout": _duration("Per-command timeout; overrides sync.timeout."),
            "min_interval": _duration(
                "Least time between two invocations of THIS source across the whole sync. Retry spaces the attempts "
                "of one failing call; this spaces every call, which is what a provider's rate limit actually counts."
            ),
            "retry": {
                "type": "object",
                "additionalProperties": False,
                "description": "How fkf re-invokes this command, never what it is — the same relationship `timeout:` "
                "has to `run:`. `fkf trust` prints it beside the line it modifies.",
                "properties": {
                    "attempts": {
                        "type": "integer",
                        "minimum": 1,
                        "maximum": MAX_RETRY_ATTEMPTS,
                        "description": "Total runs allowed, including the first.",
                    },
                    "backoff": _duration("Wait before the next attempt, growing linearly with the attempt number."),
                    "on": {
                        "type": "array",
                        "minItems": 1,
                        "items": {"type": "string", "minLength": 1},
                        "description": "Which failures may be retried: `exit:<n>`, or a substring matched against the "
                        "command's stderr. Required whenever attempts exceeds one — retrying every failure is how a "
                        "source failing for a real reason hammers a provider quietly. The matched text is never "
                        "logged or stored.",
                    },
                },
                "dependentRequired": {"backoff": ["attempts"], "on": ["attempts"]},
            },
            "window": {
                "type": "boolean",
                "default": False,
                "description": "Render run: ONCE for the whole requested range — {{start}}/{{end}} span every day "
                "being collected, not one. Events bucket records by fields.time; index sources reject it.",
            },
        },
    }


def _sync_schema() -> dict[str, object]:
    defaults = SyncConfig()
    return {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "days": {
                "type": "integer",
                "minimum": 1,
                "maximum": 366,
                "default": defaults.days,
                "description": "Completed local days to collect when no --date is given.",
            },
            "index_max_age_hours": {
                "type": "integer",
                "minimum": 1,
                "maximum": MAX_FRESHNESS_AGE_HOURS,
                "default": defaults.index_max_age_hours,
                "description": "Refresh an index document only when it is older than this; "
                f"1..{MAX_FRESHNESS_AGE_HOURS}.",
            },
            "timeout": _duration("Per-command timeout, 1s..1h."),
            "concurrency": {
                "type": "integer",
                "minimum": 1,
                "maximum": MAX_SYNC_CONCURRENCY,
                "default": defaults.concurrency,
            },
        },
    }


def _duration(description: str) -> dict[str, object]:
    return {
        "type": "string",
        "pattern": r"^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$",
        "description": description,
    }


def _string_array(description: str) -> dict[str, object]:
    return {"type": "array", "items": {"type": "string"}, "description": description}


def _placeholder_list(names: tuple[str, ...]) -> str:
    return ", ".join(f"{{{{{name}}}}}" for name in names)


def encode_config_schema() -> bytes:
    """Render the exact stable bytes published by the docs site."""
    encoded = json.dumps(config_schema(), ensure_ascii=False, indent=2, sort_keys=True)
    # Go's encoding/json keeps UTF-8 but HTML-escapes these runes and always escapes the
    # JavaScript line separators; matching it preserves generated-artifact byte parity.
    for value, escaped in (
        ("&", r"\u0026"),
        ("<", r"\u003c"),
        (">", r"\u003e"),
        ("\u2028", r"\u2028"),
        ("\u2029", r"\u2029"),
    ):
        encoded = encoded.replace(value, escaped)
    return f"{encoded}\n".encode()


__all__ = ["SCHEMA_URL", "config_schema", "encode_config_schema"]
