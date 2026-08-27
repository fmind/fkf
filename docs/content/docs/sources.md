---
title: Sources are commands
weight: 4
description: "Compose credential-free collectors from commands, shared semantic fields, and complete self-describing documents."
---

FKF ships no provider SDK and owns no login. A source is a command declared by the base. The named CLI — `git`, `gh`, `gws`, `gcloud`, `acli`, `sqlite3`, or another executable — owns authentication and prints JSON. FKF verifies one complete result, stores it, and indexes only the semantic fields the base declares.

## Shared meaning, provider-specific paths

Declare meaning once at the root, then associate each source with it:

```yaml
fkf: 1
schema:
  id:
    description: Stable record identity.
    cardinality: one
  time:
    description: Record timestamp when the provider exposes one.
    cardinality: optional
  title:
    description: Human-readable label.
    cardinality: optional
  repo:
    description: Raw provider repository used by body argv.
    cardinality: optional
  repository:
    description: Repository associated with the record.
    cardinality: optional
    relation: true
    examples: [repo:github.com/fmind/fkf]
  participant:
    description: Account involved in the record.
    cardinality: many
    relation: true
    examples: [person:email/user@example.test, actor:github.com/login]

sources:
  github-pull-requests:
    enabled: true
    layer: events
    requires: [github-search-json, gh, jq]
    window: true
    run: [github-search-json, prs, author, "{{start}}", "{{end}}"]
    fields:
      id: .url
      time: .updatedAt
      title: .title
      repo: .repository.nameWithOwner
      repository: .repository_uri
      participant: [".participant_uris[]"]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --json, "body,comments"]
    retry:
      attempts: 3
      backoff: 30s
      on: ["API rate limit exceeded", "secondary rate limit"]
```

This avoids ambiguous built-ins such as `people`. A field name says **why the value is attached**: `participant`, `author`, `reviewer`, or `owner`. Its URI says **which identity namespace it belongs to**: `person:email/...`, `actor:github.com/...`, or any other stable lowercase scheme the base chooses.

FKF neither assigns those names nor infers equivalence. It validates cardinality and URI shape, stores the document's schema subset beside its field paths, and later rebuilds retrieval and graph state from that historical declaration rather than today's configuration.

## Source keys

| Key            | Meaning                                                       |
| -------------- | ------------------------------------------------------------- |
| `enabled`      | Whether sync may run the source; false by default             |
| `layer`        | `events` for completed days, `index` for a current snapshot   |
| `requires`     | Explicit bare executable names checked by `status`            |
| `run`          | Direct argv producing JSON; required                          |
| `format`       | `json` or `ndjson`; default `json`                            |
| `records`      | Field path selecting records inside a wrapper                 |
| `fields`       | Root-schema name to one path or ordered list of paths         |
| `window`       | Run once per contiguous missing date range and bucket records |
| `body`         | Argv that fetches one record body on explicit `read --body`   |
| `install`      | Human guidance printed by status; never executed              |
| `timeout`      | Source timeout overriding the base default                    |
| `retry`        | Bounded attempts, backoff, and named retryable failures       |
| `min_interval` | Minimum interval between calls to this source                 |

`requires:` is the readiness contract. Each item is a unique bare executable name such as `gh`, `jq`, `github-search-json`, or `fish`; paths and inferred names are rejected. `status` checks all requirements for enabled sources without running them. FKF deliberately does not infer dependencies from argv or helper contents.

Mapped event times accept a date, a Unix epoch, or a timestamp with an explicit `Z` or numeric UTC offset. A timezone-less date-time is rejected because the same provider value would otherwise move between civil days when a base runs on another machine.

## Commands and helpers

Use the smallest clear integration form:

1. Use direct provider argv in `run:` when no composition is needed.
1. Put pipelines, glob expansion, or provider-specific projection in a helper under `<base>/bin/`, where trust covers its bytes and executable bit.
1. Use another executable when it improves clarity. Python is a good choice for structured or stateful transformations; Go belongs in FKF only when the framework must own the behavior.

Presets provide curated shell helpers for provider boundaries where pagination, privacy projection, or completeness checks are easy to get wrong. `fkf init` copies only the helpers required by enabled sources. Later, `fkf config helpers` shows the current or drifted state of installed official helpers plus any missing official helper required by the configuration; `fkf config helpers --refresh` is the only explicit refresh and never touches an unknown custom executable.

This is the middle ground: FKF remains one static core and a set of helpers, while users can compose any executable without writing a new Go adapter.

Declared commands run with `/` as their working directory, never the base root. Use `{{base}}` when a command needs an explicit data path. Executable or interpreted support belongs under trust-digested `bin/` and is invoked by its bare PATH name; a relative argument such as `wiki/helper.py` therefore cannot turn mutable authored content into code.

`fkf new helper` creates an owner-only, fail-closed `/bin/sh` template and prints its `run:` and `requires:` entries:

```bash
fkf new helper collect-prs
```

The generated scaffold is deliberately portable and does not select a runtime with environment-dependent startup loaders. You can still author any reviewed executable under `bin/`; its shebang selects the interpreter, and every non-standard interpreter belongs in `requires:`.

## Placeholder boundary

`run:` is a YAML argument list. FKF substitutes only values it generated in arguments after the literal executable:

- `{{date}}` and `{{next_date}}` for one completed day;
- `{{start}}` and `{{end}}` for the half-open requested window;
- `{{base}}` and `{{home}}` as opaque path values.

The exact lowercase spelling is mandatory. Whitespace inside braces, unknown names, uppercase names, malformed braces, placeholders in the executable position, and date placeholders on an index source fail configuration loading. Each YAML item remains exactly one argument after substitution; FKF never invokes a shell or performs expansion.

Collected data never enters `run:`. `body:` is an argv array because its field placeholders come from a record. Every record-derived substituted value is valid Unicode, contains no invisible control or format character, cannot begin with an option marker, and stays one opaque argument even when it contains spaces or punctuation; FKF supplies trusted base and home paths separately. A body fetch runs the current trusted `body:` argv but evaluates its field placeholders through the map stored with that historical document. A newly added placeholder absent from the document is refused until the record is re-collected.

## Field paths and cardinality

Field paths are a deliberately small jq subset:

| Form         | Meaning                                                     |
| ------------ | ----------------------------------------------------------- |
| `.key`       | object key                                                  |
| `.a.b`       | nested key                                                  |
| `[n]`        | array index; a negative index counts from the end           |
| `[]`         | array items, or object values in sorted key order           |
| `."odd key"` | key containing characters outside the simple identifier set |

No pipes, functions, or `select` are accepted. Rich reshaping belongs in the command, where a normal `jq` or another program is explicit and testable.

Paths are evaluated in declaration order. Missing and null values contribute nothing; objects and arrays do not silently stringify. The total scalar count must satisfy the root declaration: exactly one for `one`, zero or one for `optional`, any count for `many`. A field used in `body:` must be `one` or `optional` because it becomes one argv value.

Every source maps `id`; an events source also maps `time`. Those two structural fields are never relations. `title` and `url` are optional display fields. Every other field is generic retrieval input. A relation field additionally requires each projected scalar to be a canonical FKF URI; its field name becomes the graph edge kind.

## Complete or absent

Any of these fails the whole collection unit and writes nothing:

- non-zero exit, failed pipeline stage, cancellation, or timeout;
- output above the bound;
- empty stdout for `format: json`, invalid JSON, or multiple JSON documents;
- an unwrapped object where an array is expected;
- a non-object record;
- missing `id` or event `time`;
- a cardinality or relation-value violation;
- an incomplete preset pagination boundary.

Writes are atomic. A reader sees the previous complete document or the new complete document, never a partial day. Today is never collected. Existing event documents are skipped unless `--force` is supplied. Normal sync is therefore safe to repeat: completed event units and fresh indexes are skipped, due indexes refresh, missing units resume, and a failed derived rebuild is retried from the complete stored inputs.

`format: json` expects one array, or one wrapper selected by `records:`; empty output is failure because a real empty JSON result is `[]`. `format: ndjson` expects one value per line and accepts empty output as an empty result.

`window: true` runs once for each contiguous missing span in the requested half-open range and buckets records by their mapped time. An already collected day splits the plan, so a source is never asked to re-fetch across that gap unless `--force` makes the whole span eligible. It is for event sources whose fixed cost is scanning a large file tree or paginated range once.

Before a real write, preview exactly one source:

```bash
fkf sync github-pull-requests --preview --date 2026-08-24
```

Preview performs the real trusted execution, pacing, decode, field projection, cardinality checks, relation validation, and completeness checks once. It reports the full count and at most three projected samples, then writes no document, summary, helper, or graph. It accepts one target and an optional `--date`; write-planning flags such as `--days`, `--force`, `--dry-run`, and `--no-graph` are rejected.

## Stored documents

Stored marker `1` is the permanent v1 evidence envelope, matching the configuration marker. Evolution within that marker is additive: a newer reader accepts omitted optional fields, and an older reader ignores fields it does not know. A collected record may no longer be reproducible after a provider ages it out, so adding an optional envelope field is never a reason to demand re-collection.

Each canonical JSON document contains:

- the `fkf: 1` evidence-envelope marker;
- source, layer, date, collection time, and the exact UTC bounds used for an event day;
- the selected semantic `schema` definitions and `fields` paths;
- whether body fetching is declared;
- record count and every decoded provider field and value.

The event bounds preserve the collection-time civil-day interpretation if a base later moves between timezones. Documents collected before these additive fields existed necessarily fall back to the reader's current local zone because their original zone cannot be recovered. The envelope deliberately omits the rendered command, `windowed` planning state, retry history, and other executable metadata. Those belong to the current trusted configuration, not historical evidence. FKF does not redact provider output; the source command must project a reviewed metadata allowlist and keep sensitive bodies behind `body:`. A build that does not recognize the envelope marker refuses it rather than pretending it understands the evidence.

## Bodies

```bash
fkf read events/2026-08-20/github-pull-requests.json#https://github.com/o/r/pull/42 --body
```

This executes the record source's current trusted `body:` argv, prints the result, and stores nothing. Entity URIs remain graph nodes assembled from record relations or authored Markdown relations; they do not execute an on-demand resolver. Body execution is never available over MCP.

## Retry, pacing, and trust

`retry.attempts` includes the first call and is bounded. More than one attempt requires `retry.on`, whose entries are `exit:<n>` or stderr substrings. Cancellation and timeout are never retried. Backoff grows linearly. `min_interval` spaces every invocation independently of retry.

When a declared `run:` exits unsuccessfully, the diagnostic names the source, date or window, safely rendered argv, neutral working directory, timeout, and process status. The substituted command is also repeated in the failed unit summary. Provider stderr remains private: it may contain response bodies, account identifiers, or credentials, so FKF uses it only as an in-memory retry oracle. A `body:` argv is never logged because it may contain a value copied from collected evidence.

`fkf trust` prints the commands and their execution policy. Its canonical digest changes only when execution changes: command, body-bound path, enabled state, timeout, retry, pacing, extra path, or `bin/` content and mode. Editing `requires:`, a description, example, YAML comment, retrieval-only mapping, or the inherited process environment does not demand approval for an unchanged executable plan.

## Presets and custom sources

`personal` keeps a small supported set: git activity and local agent metadata are enabled; shell-history metadata, privacy-projected browser history, GitHub, Google Workspace, and Google Cloud collectors are present but disabled. `team` declares one disabled GitHub repository snapshot whose organization sentinel must be replaced before it can be enabled. Network collection is always opt-in. `minimal` starts with no sources. `--demo N` writes synthetic data without running a source. FKF has no plugin manager: presets are examples plus maintained helpers, while a base may run any reviewed command or script.

Before enabling a source, run `fkf sync --dry-run`, read the rendered command and field mappings, then run `fkf sync <source> --preview`. A contributed preset source also needs a synthetic fixture under `services/testdata/sources/`; CI sends that fixture through the real decode, projection, cardinality, relation, and addressability path without a credential or network.
