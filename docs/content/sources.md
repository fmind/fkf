---
title: Sources are commands
weight: 5
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
  category:
    description: Authorship role for default retrieval policy.
    cardinality: optional
  visibility:
    description: Audience role for default retrieval policy.
    cardinality: optional
  owner:
    description: Person or account assigned to the record.
    cardinality: many
    relation: true

sources:
  github-pull-requests:
    enabled: true
    layer: events
    requires: [github-search-json.sh, gh, jq]
    auth: [gh, auth, status]
    window: true
    run: [github-search-json.sh, prs, assignee, "{{start}}", "{{end}}"]
    fields:
      id: .url
      time: .updatedAt
      title: .title
      repo: .repository.nameWithOwner
      repository: .repository_uri
      owner: [".assignee_uris[]"]
      participant: [".participant_uris[]"]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --json, "body,comments"]
    retry:
      attempts: 3
      backoff: 30s
      on: ["API rate limit exceeded", "secondary rate limit"]
    recency: { half_life_days: 14 }
```

This avoids ambiguous built-ins such as `people`. A field name says **why the value is attached**: `participant`, `author`, `reviewer`, or `owner`. Its URI says **which identity namespace it belongs to**: `person:email/...`, `actor:github.com/...`, or any other stable lowercase scheme the base chooses.

FKF neither assigns those names nor infers equivalence. It validates cardinality and URI shape, stores the document's schema subset beside its field paths, and later rebuilds retrieval and graph state from that historical declaration rather than today's configuration.

## Source keys

| Key            | Meaning                                                       |
| -------------- | ------------------------------------------------------------- |
| `enabled`      | Whether sync may run the source; false by default             |
| `layer`        | `events`, `index`, or the dedicated `tasks` trace importer    |
| `requires`     | Explicit bare executable names checked by `status`            |
| `auth`         | Optional literal argv probing provider login readiness        |
| `run`          | Direct argv producing JSON; required                          |
| `test`         | Optional direct argv verifying the source without collection  |
| `format`       | `json` or `ndjson`; default `json`                            |
| `records`      | Field path selecting records inside a wrapper                 |
| `fields`       | Root-schema name to one path or ordered list of paths         |
| `window`       | Run once per contiguous missing date range and bucket records |
| `body`         | Argv that fetches one record body on explicit `read --body`   |
| `bodies`       | Rebuildable body policy: `none`, `cache`, or `sync`           |
| `recency`      | Optional lexical recency half-life in days                    |
| `install`      | Human guidance printed by status; never executed              |
| `timeout`      | Source timeout overriding the base default                    |
| `retry`        | Bounded attempts, backoff, and named retryable failures       |
| `min_interval` | Minimum interval between calls to this source                 |

`requires:` is the ordinary collection/body readiness contract. Each item is a unique bare executable name such as `gh`, `jq`, `github-search-json.sh`, or `fish`; paths and inferred names are rejected. `status` checks all requirements for enabled sources without running them. It reports the `test[0]` entrypoint separately on the test-only PATH, so a base-owned hook need not duplicate its own name in `requires:`. External tools that the hook invokes still belong in `requires:`. FKF deliberately does not infer dependencies from argv or helper contents.

`auth:` is the readiness half of that contract: literal argv such as `[gh, auth, status]`, run once per sync and only for a source that already has due work. It accepts no placeholder, and its stdout and stderr are discarded rather than logged, so a failing probe skips that source as `auth-required` without exposing provider output. `fkf brief` and `fkf status --live` run the same probes without collecting.

Mapped event times accept a date, a Unix epoch, or a timestamp with an explicit `Z` or numeric UTC offset. A timezone-less date-time is rejected because the same provider value would otherwise move between civil days when a base runs on another machine.

## Commands and helpers

Use the smallest clear integration form:

1. Use direct provider argv in `run:` when no composition is needed.
1. Put pipelines, glob expansion, or provider-specific projection in a helper under `<base>/bin/`, where trust covers its bytes and executable bit.
1. Use another executable when it improves clarity. Python is a good choice for structured or stateful transformations; Go belongs in FKF only when the framework must own the behavior.

Presets provide curated helpers for provider boundaries where pagination, privacy projection, or completeness checks are easy to get wrong. `fkf init` copies only the helpers required by enabled sources. Later, `fkf config helpers` shows the current or drifted state of installed official helpers plus any missing official helper required by the configuration; `fkf config helpers --refresh` is the only explicit refresh and never touches an unknown custom executable.

This is the middle ground: FKF remains one static core and a set of helpers, while users can compose any executable without writing a new Go adapter.

Declared commands run with `/` as their working directory, never the base root. Use `{{base}}` when a command needs an explicit data path. Collection and body support belongs under trust-digested `bin/`; source verification hooks belong under trust-digested `tests/`. Both are invoked by bare PATH names. A relative argument such as `wiki/helper.py` therefore cannot turn mutable authored content into code.

Shell helpers use `.sh`; Python helpers use `.py`. The extension makes the interpreter contract visible without executing the file. `fkf new helper` requires one of those extensions, creates an owner-only fail-closed template, and prints its `run:` and `requires:` entries:

```bash
fkf new helper collect-prs.sh
fkf new helper collect-prs.py
```

The generated scaffold is deliberately portable and does not select a runtime with environment-dependent startup loaders. You can still author any reviewed executable under `bin/`; its shebang selects the interpreter, and every non-standard interpreter belongs in `requires:`.

## Placeholder boundary

`run:` is a YAML argument list. FKF substitutes only values it generated in arguments after the literal executable:

- `{{date}}` and `{{next_date}}` for the selected local day;
- `{{start}}` and `{{end}}` for its half-open window;
- `{{base}}` and `{{home}}` as opaque path values.

For event and task sources the date values describe the requested completed range. An index source receives the current local day, which supports replaceable agenda snapshots without collecting an incomplete event document. The exact lowercase spelling is mandatory. Whitespace inside braces, unknown names, uppercase names, malformed braces, and placeholders in the executable position fail configuration loading. Each YAML item remains exactly one argument after substitution; FKF never invokes a shell or performs expansion.

`test:` is also one direct argv array. It may use only `{{base}}` and `{{home}}`, because a verification hook is independent of collection windows and stored values. Put a base-owned hook and its fixtures or support files under `tests/`; FKF hashes the complete tree and prepends it only for `test:` execution, so a fixture cannot shadow collection or body commands. With no names, `fkf test` selects enabled sources that declare a hook; explicit names also select disabled sources, and `--all` selects every declared hook. An empty selection preserves the compatible successful 0/0 report. Name every mandatory source in a project completion task so the gate also detects one accidentally removed hook. Hooks use the source timeout, run sequentially, discard stdout, expose no provider stderr, and never write evidence.

Collected data never enters `run:`. `body:` is an argv array because its field placeholders come from a record. Every record-derived substituted value is valid Unicode, contains no invisible control or format character, cannot begin with an option marker (`-`) or a response-file selector (`@`), and stays one opaque argument even when it contains spaces or punctuation; FKF supplies trusted base and home paths separately. A body fetch runs the current trusted `body:` argv but evaluates its field placeholders through the map stored with that historical document. A newly added placeholder absent from the document is refused until the record is re-collected.

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

Every source maps `id` and `title`; an events source also maps `time`. Those structural roles are never relations. A newly collected record must project a non-empty, control-free title. Content-first helpers derive one deterministic line, capped at 160 characters, without copying the body. Historical v1 documents that predate a title remain readable and never require forced re-collection. `url` is an optional display field. Every other field is generic retrieval input. A relation field additionally requires each projected scalar to be a canonical FKF URI; its field name becomes the graph edge kind.

A root schema field may declare `weight: 1..100` for lexical ranking. Omitted weights resolve to 10 for `id`, 5 for `title`, and 1 otherwise. Optional max-one roles `category` (`created`, `received`, or `saved`) and `visibility` (`private`, `shared`, or `public`) let context apply explicit defaults without inferring sensitivity from a provider or note type. A source may declare `recency: {half_life_days: N}`; undated records and sources without that policy receive no freshness bonus.

## Complete or absent

Any of these fails the whole collection unit and writes nothing:

- non-zero exit, failed pipeline stage, cancellation, or timeout;
- output above the bound;
- empty stdout for `format: json`, invalid JSON, or multiple JSON documents;
- an unwrapped object where an array is expected;
- a non-object record;
- missing `id` or event `time`;
- missing, empty, or unsafe `title` on a source that maps it;
- a cardinality or relation-value violation;
- an incomplete preset pagination boundary.

Writes are atomic. A reader sees the previous complete document or the new complete document, never a partial day. Today is never collected. Existing event documents are skipped unless `--force` is supplied. Normal sync is therefore safe to repeat: completed event units and fresh indexes are skipped, due indexes refresh, missing units resume, and a failed derived rebuild is retried from the complete stored inputs.

`format: json` expects one array, or one wrapper selected by `records:`; empty output is failure because a real empty JSON result is `[]`. `format: ndjson` expects one value per line and accepts empty output as an empty result.

`window: true` runs once for each contiguous missing span in the requested half-open range and buckets records by their mapped time. An already collected day splits the plan, so a source is never asked to re-fetch across that gap unless `--force` makes the whole span eligible. It is for event sources whose fixed cost is scanning a large file tree or paginated range once.

A `layer: tasks` source is a deliberately smaller contract. It must be windowed JSON and may declare only its execution controls: `records`, `fields`, `body`, `bodies`, and `recency` are rejected. Its helper returns the closed bounded session-trace array consumed by FKF, which validates the whole batch before creating any `tasks/<date>/<repo>-<sid>/TASKS.md` skeleton. Existing traces are never overwritten because they may already contain owner annotations. Preview is unavailable; use `sync <source> --dry-run` to disclose its command without execution.

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

A source may set `bodies: none`, `cache`, or `sync`; the default is `none`:

- `none` fetches only for the explicit read and stores nothing. Mail, Chat, and ordinary provider sources use this policy.
- `cache` stores a body only after an explicit `read --body`.
- `sync` prefetches new or provider-modified bodies after the evidence document is safely written. A fresh current index snapshot repairs missing cache entries. A failed new event document gets one later retry; after a complete cache prune, the newest selected event document for each opted-in source gets the same bounded retry cycle. An attempt marker prevents a vanished historical resource from becoming perpetual hourly work. Use an explicit `read --body` for any other historical miss or `sync --force` to re-collect and prefetch its document. Meeting notes and local harness memory files opt in.

Cached text lives under ignored `bodies/<source>/` and is bound by `bodies/manifest.json` to its record URI, provider modification time, byte count, SHA-256, and the cache-local event restore markers. It is bounded to 4,096 entries, 512 MiB total, a 1 MiB manifest, and 4 MiB per body. FKF refuses growth before publishing a body that the manifest cannot name. The cache is UTF-8, machine-local, rebuildable data—not evidence and never mirrored by FKF. `read --body` uses a valid cached copy before executing. `find --bodies` and `context` consult valid cached text offline; neither fetches a miss. `fkf build bodies --prune` explicitly empties the cache (or selectively prunes by `--older-than` and `--source`) and re-arms the one-time newest-event restoration for the next sync.

Entity URIs remain graph nodes assembled from record relations or authored Markdown relations; they do not execute an on-demand resolver. Body execution is never available over MCP. `fkf validate records` warns when one title is shared by more than half of a source's records; `--strict` promotes that warning.

## Retry, pacing, and trust

`retry.attempts` includes the first call and is bounded. More than one attempt requires `retry.on`, whose entries are `exit:<n>` or stderr substrings. Cancellation and timeout are never retried. Backoff grows linearly. `min_interval` spaces every invocation independently of retry.

When a declared `run:` exits unsuccessfully, the diagnostic names the source, date or window, safely rendered argv, neutral working directory, timeout, and process status. The substituted command is also repeated in the failed unit summary. Provider stderr remains private: it may contain response bodies, account identifiers, or credentials, so FKF uses it only as an in-memory retry oracle. A `body:` argv is never logged because it may contain a value copied from collected evidence.

`fkf trust` prints the commands and their execution policy. Its canonical digest changes only when execution changes: command, body-bound path, enabled state, timeout, retry, pacing, extra path, or `bin/`/`tests/` content and mode. Editing `requires:`, a description, example, YAML comment, retrieval-only mapping, or the inherited process environment does not demand approval for an unchanged executable plan.

## Presets and custom sources

`personal` enables only git activity and local agent metadata. Its disabled, opt-in examples cover agent prompts, Chromium history and bookmarks, RSS, authored documents, mise tools, GitHub activity and Actions, Google Workspace, Google Cloud, Kaggle, and Hugging Face. `google-calendar-agenda` is a refreshable index snapshot for today's brief; `google-calendar-events` remains the permanent completed-day history. The `meeting-notes` source joins Google Docs to calendar records by attachment ID, then selects the nearest start time among attachment-less events with the exact title prefix. Enable and sync `google-calendar-events` with it: the notes helper refuses to emit a matched calendar record URI until that owning document is durable, keeping `read` and `timeline` addressable. Its reviewed body helper emits Docs text without storing it in the evidence record. Remote collectors remain disabled until the owner reviews their metadata projection and authentication probe. Explicit sentinels such as `REPLACE_WITH_OWNER`, `REPLACE_WITH_MEETING_PREFIX`, or `REPLACE_WITH_WRITING_DOCUMENT.md` must be edited before enabling that source; FKF does not guess an account or filesystem corpus. `team` declares one disabled GitHub repository snapshot whose organization sentinel must be replaced before it can be enabled. Network collection is always opt-in. `minimal` starts with no sources. `--demo N` writes synthetic data without running a source. FKF has no plugin manager: presets are examples plus maintained helpers, while a base may run any reviewed command or script.

The Chromium helper never copies a live database file directly. SQLite's online backup API creates one consistent snapshot and includes committed rows still present only in the browser's live WAL; URL credentials, queries, and fragments are removed before JSON reaches FKF.

Before enabling a source, run `fkf sync --dry-run`, read the rendered command and field mappings, then run `fkf sync <source> --preview`. A contributed preset source also needs a synthetic fixture under `services/testdata/sources/`; CI sends that fixture through the real decode, projection, cardinality, relation, and addressability path without a credential or network.
