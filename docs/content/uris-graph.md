---
title: URIs and the graph
weight: 6
description: "Address every record, page, heading, entity, and explicit relationship with one open URI grammar."
---

FKF uses one address grammar everywhere:

```text
<path>[?jq=<expr>][#<record-id-or-heading>]
<scheme>:<identity>
https://...
```

The first form addresses files inside the base. The second is an open entity URI: the base chooses any non-reserved lowercase scheme and stable identity. HTTPS URLs remain external and verbatim.

## File and record URIs

| URI                                                                                | Meaning                                |
| ---------------------------------------------------------------------------------- | -------------------------------------- |
| `events/`                                                                          | event dates                            |
| `events/2026-05-04/`                                                               | one day's documents                    |
| `events/2026-05-04/github-pull-requests.json`                                      | one complete collected document        |
| `events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42` | one record selected by its declared id |
| `index/github-repositories.json#fmind/fkf`                                         | one record in a current snapshot       |
| `tasks/2026-08-22/review/TASKS.md#verification`                                    | one task-trace heading                 |
| `projects/fkf.md#decisions`                                                        | one project heading                    |
| `wiki/retrieval-boundary.md#decision`                                              | one wiki heading                       |
| `graph.tsv`                                                                        | one validated graph edge snapshot      |
| `graph.meta.json`                                                                  | that snapshot's integrity metadata     |
| `fkf.yaml`                                                                         | the base configuration                 |
| `AGENTS.md`                                                                        | base-specific agent instructions       |

Directories end in `/`. A fragment is accepted only when it names an existing document record or Markdown heading; files with no addressable children reject fragments. Record fragments use the declared id rather than an array position, so they survive reordering and re-collection.

Only enabled layers plus `fkf.yaml`, `AGENTS.md`, `graph.tsv`, and `graph.meta.json` are reachable. Every path goes through the store, which refuses traversal, absolute and home-relative paths, unknown root files, and symlinks below the base. A URI cannot read `.git/config`, `.env`, `.netrc`, or another neighbouring file.

## In-process jq

Append `?jq=<expr>` to select JSON, optionally after selecting a record:

```bash
fkf read 'events/2026-05-04/github-pull-requests.json?jq=.records|length'
fkf read 'events/2026-05-04/github-pull-requests.json?jq=.title#https://github.com/fmind/fkf/pull/42'
```

Evaluation is path, then fragment, then jq. The expression runs in-process through gojq with no environment, input, include, or import access. Final output is bounded. `halt` may return successfully; `halt_error` remains an error.

## Open entity URIs

An entity has no file of its own. Its URI is a lowercase scheme followed by an identity chosen by the base:

```text
person:email/marc@example.test
actor:github.com/marc
team:google.com/platform
repo:github.com/fmind/fkf
ticket:jira/FKF-412
topic:retrieval
tag:architecture
```

The scheme is a namespace, not a built-in FKF type. Identity spelling is preserved: FKF does not lowercase it, expand an email into a person record, or decide that an email and GitHub account are the same actor. Pick stable namespaces that make collisions and ambiguity unlikely.

`http`, `https`, `ftp`, and `mailto` remain external namespaces and cannot be repurposed as entity schemes. External addresses must be full HTTPS URLs. Percent-encode whitespace, control bytes, and delimiter data in canonical URI text. `fkf read <https-url>` returns only that URL node's local graph neighbourhood; it never fetches the remote page.

`fkf read <entity>` returns the identity and its local graph neighbourhood. Entity reads are always offline; `--body` applies only to collected record URIs, never an entity. MCP exposes the same offline entity view.

## A transcription-only graph

Root `graph.tsv` stores one edge per line:

```text
src<TAB>dst<TAB>kind<TAB>at<TAB>via<TAB>indexed
```

Edges have exactly four sources:

1. a collected field whose stored schema says `relation: true`;
1. an authored Markdown link outside code;
1. an authored page tag, which points to `tag:<name>`;
1. an explicit Markdown frontmatter relation.

For a collected relation, the schema field name is the edge kind and the stored canonical URI is the destination:

```yaml
schema:
  reviewer:
    description: Account that reviewed the change.
    cardinality: many
    relation: true

sources:
  reviews:
    fields:
      id: .id
      time: .submitted_at
      reviewer: [".reviewer_uris[]"]
```

For authored Markdown, relation keys must name root-schema fields declared with `relation: true`, and the list must satisfy their cardinality:

```yaml
---
type: decision
title: Retrieval boundary
tags: [architecture]
relations:
  related:
    - ../projects/fkf.md
  participant:
    - person:email/marc@example.test
---
```

File relations in frontmatter resolve relative to the page, just like Markdown links. Entity and HTTPS URIs are already absolute in FKF's grammar.

FKF does not scan a title for ticket-shaped strings, treat arbitrary frontmatter as relationships, mine prose, or infer an edge from shared terms. That restraint is deliberate: the graph is checkable because every edge transcribes a declaration or authored link.

## Markdown links

Use relative paths inside Markdown so local editors and GitHub resolve them:

```markdown
[trace](../tasks/2026-08-22/review/TASKS.md#verification) [Marc](person:email/marc@example.test) [PR](https://github.com/fmind/fkf/pull/42)
```

A provider URL and a local record are two visible links when both are useful:

```markdown
[FK-412 in Jira](https://acme.example/browse/FKF-412) ([stored record](../events/2026-08-20/jira-issues.json#FKF-412))
```

Markdown link titles are tooltip metadata and never hidden graph carriers. This keeps every graph edge visible in the authored file: use a visible link, a declared `relations:` entry, or a stored relation field.

## Cache integrity and queries

Graph metadata binds the exact URI and bytes of every collected document and authored Markdown input, plus the canonical TSV bytes. Every graph read recomputes the inputs before accepting the cache. A neighbourhood walk keeps one validated graph descriptor across its hops and rechecks the bytes before return.

```bash
fkf build graph
fkf graph
fkf graph ticket:jira/FKF-412 --in
fkf graph person:email/marc@example.test --depth 2
fkf graph wiki/retrieval-boundary.md --in
fkf graph nodes --kind person
```

`--in` follows backlinks, `--out` follows declared destinations, and `--both` is the default; choose at most one. Depth is bounded from one to three. A neighbourhood's `--kind` accepts the observed edge vocabulary, including base-defined relation names. `graph nodes --kind` instead accepts a node kind such as the `person` entity scheme.

The graph is a cache: delete and rebuild it without losing knowledge. The documents and authored files are the source of truth.
