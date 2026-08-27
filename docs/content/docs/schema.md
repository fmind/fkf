---
title: Configuration schema
weight: 3
description: "Define shared semantic fields, map provider records, and use FKF's generated JSON Schema in an editor or validator."
---

`fkf.yaml` has two related schemas:

- the generated JSON Schema defines FKF's configuration syntax and rejects unknown or malformed configuration;
- the base-owned `schema:` dictionary defines what record fields mean across every source in that base.

The configuration and every stored evidence document use `fkf: 1`. They are separate contracts identified by the containing file. Compatible evidence additions remain readable under marker `1`; an incompatible change requires a new marker and an explicit release boundary.

## The semantic dictionary

Every field declaration requires a description and cardinality:

| Cardinality | Accepted values per record |
| ----------- | -------------------------- |
| `one`       | exactly one scalar         |
| `optional`  | zero or one scalar         |
| `many`      | zero or more scalars       |

Set `relation: true` when every value is an FKF URI that should become a graph edge. `examples` document intent; they do not validate or normalize provider values.

```yaml
fkf: 1
name: brain

schema:
  id:
    description: Stable record identity.
    cardinality: one
  time:
    description: Record timestamp when the provider exposes one.
    cardinality: optional
  title:
    description: Human-readable record label.
    cardinality: optional
  repository:
    description: Repository associated with the record.
    cardinality: optional
    relation: true
    examples: [repo:github.com/fmind/fkf]
  participant:
    description: Person or account involved in the record.
    cardinality: many
    relation: true
    examples: [person:email/user@example.test, actor:github.com/login]
```

`id` is required and must be `one`. Event sources also map `time`; `time`, `title`, and `url` are scalar presentation fields and therefore use `one` or `optional`. All other names are base-defined. A field name states a role such as `participant`, `author`, or `reviewer`; a URI value states an identity namespace such as `person:email/...` or `actor:github.com/...`. FKF never merges those identities.

## Source mappings

A source maps provider paths into the shared dictionary. Only `id` and event `time` are structural; every other mapping refers to a field already declared under root `schema:`.

```yaml
sources:
  git-commits:
    enabled: true
    layer: events
    requires: [git-log-json, git]
    window: true
    run: [git-log-json, "{{start}}", "{{end}}", "{{home}}"]
    fields:
      id: .uid
      time: .time
      title: .message
      repository: .repository_uri
      participant: [".participant_uris[]"]
```

FKF stores the exact schema subset and field map used by each collected document. That evidence lets current readers validate older records even after the base adds new optional fields.

## Generated JSON Schema

Print the schema without opening a base:

```bash
fkf config schema
```

The same generated artifact is published as [`fkf.schema.json`](https://fmind.github.io/fkf/fkf.schema.json). Editors that support a YAML language-server directive can bind a base directly to it:

```yaml
# yaml-language-server: $schema=https://fmind.github.io/fkf/fkf.schema.json
fkf: 1
```

The loader rejects unknown keys, unknown field references, missing descriptions, invalid cardinality, malformed placeholders, multiple YAML documents, and enabled sources targeting disabled layers. The published artifact is generated from that loader, not maintained independently. Contributors run `mise run generate:schema` after loader changes and never edit `docs/static/fkf.schema.json` by hand.

The JSON Schema proves configuration shape. Provider output still needs a trusted, write-free `fkf sync <source> --preview` to prove decoding, field projection, cardinality, relations, and completeness against a real sample.
