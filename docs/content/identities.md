---
title: Declared identities
weight: 34
description: "Join exact emails, logins, entity URIs, and authored pages under one canonical person, organization, or repository."
---

An FKF base can declare the exact spellings that identify the same person, organization, or repository. This is an explicit same-as map, not fuzzy entity extraction:

```yaml
identities:
  fmind:
    canonical: person:email/fmind@example.com
    aliases:
      - actor:github.com/fmind
      - fmind
    kind: person
    owner: true
  fkf:
    canonical: repo:github.com/fmind/fkf
    aliases:
      - repository:github.com/fmind/fkf
    kind: repository
```

`canonical` is required and must be an entity URI. `aliases` is a non-empty list of entity URIs, bare provider logins, or email addresses. Alias matching is exact and case-insensitive; FKF never derives an identity from prose. `kind` is optional and accepts `person`, `organization`, or `repository`.

At most one root person may carry `owner: true`. FKF omits that identity from ambient people lists and graph-expansion seeds. An explicit `--person` filter can still select the owner.

`fkf.local.yaml` cannot declare or change identities. Keep the complete explicit identity map in the committed `fkf.yaml` so the same base resolves a spelling identically on every machine.

## Authored pages

A wiki or project page with `type: person` or `type: organization` may carry the same alias list:

```yaml
---
type: person
title: Maxime Cordy
aliases:
  - actor:github.com/maxime
  - maxime@work.example
---
```

Overlapping declared aliases merge transitively. A component with a root declaration uses its root canonical URI; a page-only component uses a deterministic page URI. Conflicting root canonicals or kinds fail the read instead of guessing.

## Retrieval and provenance

`find`, `context`, `timeline --person`, and graph neighbourhood reads resolve an exact alias to the canonical URI. Returned projected fields use the canonical URI; stored provider JSON remains unchanged. Graph builds rewrite relationship endpoints and add `same-as` edges from URI-shaped aliases and authored pages so the merge remains auditable.

Use `who` to inspect the complete joined view:

```bash
fkf who "Maxime Cordy"
fkf who actor:github.com/maxime
```

The report includes matching pages, canonical identity, aliases, neighbours grouped by kind, per-source counts, and the ten most recent stored interactions. One directly linked stored record is also an interaction, so a calendar event can bring in its attached meeting-notes record; traversal stops there instead of expanding through everything linked from the notes. Busy neighbourhoods are bounded at 200 edges and report `neighbourhood_truncated` without hiding the newest direct or linked interactions. It is an offline read and executes no source command.

GitHub commit addresses in the documented `ID+login@users.noreply.github.com` form normalize to `actor:github.com/login` at the retrieval boundary. FKF preserves the original collected email in durable evidence.
