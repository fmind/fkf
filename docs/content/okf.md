---
title: The wiki format (OKF v0.2)
weight: 12
description: "Author and validate flat, tagged Markdown knowledge, project pages, and task traces in an fkf base."
---

Three of a base's five layers are Markdown: `wiki/` holds what is durably true, `projects/` holds what you are trying to achieve, and `tasks/` holds what an agent actually did. They share one parser and the same heading-anchor and body-link semantics, while their locations, lifecycle, and validation requirements remain distinct.

`wiki/` follows OKF v0.2: flat, tagged Markdown with YAML frontmatter. This page states the rules `fkf` enforces, and why each one exists.

## Why the wiki is flat

`wiki/` has no subdirectories. A page is `wiki/<slug>.md` and nothing else; a directory under `wiki/` is a validation error, not a namespace.

A directory tree forces one classification to be the primary one, and the choice is made the day the second page is written, when you know least. Then the same idea belongs under two branches, so it gets filed twice or filed wrong, and six months later you cannot find it because you are guessing at the taxonomy of a past self. Tags are a set, not a path: a page about a retrieval decision can be `decision` and `retrieval` and `architecture` at once, and the vocabulary can be read back and reused rather than remembered.

The consequence is that **an untagged page is harder to discover through tag-filtered navigation**. Direct reads, listings, lexical search, and links from `wiki/index.md` still reach it. That is why a missing tag is a validation finding rather than a matter of taste:

```bash
fkf tags wiki                       # the whole vocabulary, most-used first, plus the untagged pages
fkf list wiki --tag decision --tag retrieval   # repeatable, and every tag must match
fkf find "retrieval boundary" --layer wiki
```

`fkf find` with `--layer wiki` is an exhaustive lexical scan over the slug, title, description, tags, explicit relations, and body. A few hundred pages is a scan, and a scan remains inspectable — the same reasoning that governs [context packs](../context/).

`projects/` is flat for the same reason, with one extra requirement: a `status`.

## Frontmatter: strict on write, permissive on read

Reading never fails on incomplete frontmatter. A page with no frontmatter at all parses; its title falls back to the first heading; a `tags:` value written as a YAML list or as a comma-separated string both parse. That is what the OKF spec asks for, and it is what keeps a page you wrote by hand years ago from becoming unreadable because a tool changed.

Writing is where the requirements apply, because a page that fkf or a skill writes today has no excuse for being incomplete.

| Field         | Required on write    | Missing it is | What it does                                                       |
| ------------- | -------------------- | ------------- | ------------------------------------------------------------------ |
| `type`        | `wiki/`, `projects/` | warning       | classifies the page; `fkf list wiki --type` filters on it          |
| `title`       | both                 | warning       | falls back to the first heading when absent                        |
| `tags`        | both                 | warning       | enables tag-filtered navigation; becomes `tag:` edges in the graph |
| `status`      | `projects/` only     | **error**     | `active`, `paused`, or `done` — nothing else                       |
| `description` | no                   | —             | ranked with the title, and used as the excerpt in a context pack   |
| `date`        | no                   | —             | carried onto the page's graph edges as the `at` column             |

The type vocabulary is open, not a closed set. The `learn` skill uses `decision`, `pattern`, `tool`, `insight`, and `person` for concepts and `project` for project pages. `fkf` only requires that a page declare one.

**Unknown frontmatter is preserved verbatim.** The parser reads the fields it understands and keeps the whole map, so a field FKF has never heard of survives every read and validation. Graph meaning is explicit: only URI lists nested under `relations:` become frontmatter edges, and each key must name a root-schema field declared with `relation: true`. Other fields such as `sources:` remain application metadata and are never inferred as relationships.

`status` is a closed set of three because a project page nobody can act on is not a project. A `done` project is kept rather than deleted: it is the record of a decision that was made. It simply ranks lower — `fkf context` applies a named `superseded` penalty to any candidate whose status is `done` or `deprecated`, and the receipt says so.

## The two skeletons

These are the shapes the `learn` skill writes, so a page created by an agent and a page created by hand are the same page.

A concept, `wiki/<slug>.md`:

```markdown
---
type: decision
title: Explicit sync boundary
description: Why collection covers completed days only and never writes a partial day.
tags: [decision, collection]
sources:
  - ../tasks/2026-07-10/review-template/TASKS.md#verification
relations:
  related:
    - daily-collection.md
---

# Explicit sync boundary

One paragraph stating the claim. Then the rationale, the evidence, and what would change it.
```

A project, `projects/<slug>.md`:

```markdown
---
type: project
title: fkf rebuild
status: active
tags: [fkf]
relations:
  related:
    - ../wiki/explicit-sync-boundary.md
---

# fkf rebuild

## Intent

## Open questions

- Retrieval boundary for [FK-412 in Jira](https://acme.example/browse/FKF-412) ([stored record](../events/2026-08-20/jira-issues.json#FKF-412)), waiting on [Marc](person:email/marc@example.test)

## Decisions

- 2026-08-22 — sources are commands; the base is the configuration
```

A concept states one durable idea and what would falsify it. A project is not a task tracker: link to the tickets, do not mirror them.

## `wiki/index.md` and `wiki/log.md`

Two slugs are structural rather than conceptual, so the validator exempts them from `type` and `tags` and holds them to their own rule instead.

`wiki/index.md` is the entry point: the hand-curated way in, since a flat directory listing is not one. It must start with a level-one heading. Anything below it is yours — usually a short list of links to the concepts worth reading first.

One exception, and it is opt-in: `fkf build wiki` maintains a marked block inside the page holding the complete listing, grouped by `type`, with the tag vocabulary under it. It rewrites only between the markers, the way the managed `.gitignore` block works, so the curated part above stays exactly as you wrote it. Run it after adding a page, or wire `fkf build wiki --check` into a hook to be told when it drifts. The block links pages by bare slug and prints tags as code spans rather than `tag:` links — a link would be transcribed into an edge, and a listing linking every tag would answer `fkf graph tag:<name> --in` for every tag in the base.

`wiki/log.md` is a dated stream for a thought worth keeping that is not yet a concept. Its level-two headings are ISO dates, newest first, each appearing once:

```markdown
# Log

## 2026-08-22

- Sources are commands; the base is the configuration.

## 2026-08-20

- The receipt matters more than the ranking.
```

When a log bullet harvests a task trace, its inline link keeps the evidence readable and the trace URI must also appear in the log page's optional `sources:` frontmatter. `fkf list tasks learned` intentionally uses structured frontmatter citations—not arbitrary body links—to decide whether a trace has been harvested. `sources:` does not create a graph edge; add the same URI under a declared `relations:` field when the relationship should be traversable.

Each of those three properties is an error when broken — a non-date heading, a repeated date, or a date out of order. Newest first is enforced rather than suggested because the log is read from the top and appended to at the top, and one out-of-order entry silently sends the next append to the wrong place.

## Links

Links in a page body are transcribed into the graph, so they are held to real rules.

- **Relative to the linking file**, not to the base root. From `wiki/a.md` a sibling is `b.md`, a record is `../events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42`. This is the one form that resolves in your editor, on GitHub, and through `fkf read` alike. Explicit `relations:` URIs resolve the same way; base-relative form is what you see in root `graph.tsv`, in FKF output, and on the command line.
- **Confined to the base.** A link that escapes with `../` is rejected, never clamped, and validation reports it as an error.
- **Titles are metadata, not graph carriers.** When a provider URL and a local record both matter, write both links visibly: `[FK-412 in Jira](https://acme.example/browse/FKF-412) ([stored record](../events/2026-08-20/jira-issues.json#FKF-412))`. A tooltip such as `"Open the example"` never creates an edge. Use a visible body link or an explicit `relations:` value so the relationship is reviewable in the file.
- **Links inside code are skipped.** Fenced blocks and inline code spans are stripped before the extractor runs. A link in a code sample is an illustration, not an assertion, and indexing it would put edges in the graph that the author never claimed.

Heading anchors follow GitHub's rules — lowercased, spaces to hyphens, punctuation dropped — so `## Open questions` is reachable as `projects/fkf-rebuild.md#open-questions` from an editor, from GitHub, and from `fkf read`. A heading made only of punctuation is a validation error because those rules leave it with no addressable fragment. The full grammar is on [URIs and the graph](../uris-graph/).

## What validation checks

```bash
fkf validate wiki            # frontmatter, the flat rule, slugs, links, index and log conventions
fkf validate wiki --strict   # every warning becomes an error
fkf validate projects        # the same, plus the required status
fkf validate                 # validates enabled wiki and project pages
```

Both commands report one issue per finding with its URI, line, and severity, then a count. They exit 1 when there is at least one error, so a pre-commit hook or a skill's final step can depend on the code rather than on parsing the output. Asking for a layer the base has disabled is a configuration error and exits 2 instead.

**Errors** — the page is wrong:

| Rule                                                                         | Why it is not merely untidy                                      |
| ---------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| a directory under `wiki/` or `projects/`                                     | the layer is flat; a nested page is invisible to every listing   |
| a slug outside lowercase letters, digits, `.`, `_`, `-`                      | the slug is half a URI, and a URI has to be typeable             |
| two slugs differing only in case                                             | one of the two loses on a case-insensitive filesystem            |
| a project `status` that is absent or outside the closed set                  | nobody can act on it, and `fkf list projects --status` misses it |
| a link that escapes the base                                                 | the base is the boundary; it is rejected, never clamped          |
| a link that resolves into a disabled or out-of-bounds layer                  | it names something this base does not have                       |
| a `relations:` key absent from the root schema or not declared as a relation | graph semantics must be explicit and shared                      |
| a `relations:` list that violates its declared cardinality                   | authored and collected relationships obey one contract           |
| an invisible or non-layout control character in the body                     | it can hide text or alter terminal output                        |
| an invisible or control character in any frontmatter scalar                  | generated listings and terminal output must stay reviewable      |
| a heading whose visible text produces an empty anchor                        | it cannot be named by the published fragment grammar             |
| `wiki/index.md` without a level-one heading                                  | the entry point has to name itself                               |
| a `wiki/log.md` heading that is not an ISO date, repeats, or is out of order | the log is appended to at the top                                |

**Warnings** — the page is incomplete, and `--strict` promotes each one to an error:

| Rule                                        | Effect while it stands                                      |
| ------------------------------------------- | ----------------------------------------------------------- |
| no `type`                                   | `fkf list wiki --type` cannot reach it                      |
| no `title` and no heading                   | it appears untitled in every listing and every context pack |
| no `tags` (outside `index.md` and `log.md`) | absent from tag-filtered navigation                         |
| a tag that is not lowercase kebab-case      | the same idea grows a second spelling                       |
| a link to a path that does not exist yet    | a link written ahead of the page it points at               |

The last one is a warning on purpose: writing a link before its target is a normal way to work, and `--strict` is what says "not in what I am about to commit". A deliberate new tag is the other case where a warning is the right answer — it is a new word in the vocabulary, not a mistake.

Invisible and control characters are errors rather than warnings because they are never stylistic slips. A zero-width space, soft hyphen, or right-to-left override changes what a human believes a page says; an escape or other terminal control can change what the terminal displays. Frontmatter is single-line metadata and permits no control character. Markdown bodies retain their ordinary newline, carriage-return, and tab layout, while every other control is refused. `fkf build wiki` checks the type, title, description, and tags before it writes the generated index, so unsafe metadata cannot be copied into a page the validator would then reject. See [privacy and security](../privacy/) for the wider trust boundary.

## Task traces

`tasks/YYYY-MM-DD/<slug>/TASKS.md` is the evidence layer: one trace per session, with a `## <n>. <request>` section per instruction — the request, a concise step trace, the files changed, and the exact verification output, with records cited by URI — and a closing `## Learned` list the `learn` skill harvests. An instruction is addressable by its heading anchor. In a shared base, prefix the slug with your handle. It has no required frontmatter and no validator, because a trace is written under time pressure and a rejected trace is a trace that does not get written. Its title comes from the first heading, its links enter the graph like any other page, and `fkf context` ranks it alongside the records — with no upper date bound, since a trace written today is the only same-day evidence the base holds.

Both skills are written into every base at `.agents/skills/` by `fkf init`, and refreshed by running `init` again. Bases also receive `.claude/skills -> ../.agents/skills` when that path is absent, so Claude reads the same packages rather than a copy. The use skill reads. The learn skill may append an evidence-linked, dated bullet to `wiki/log.md` and add its trace to `sources:` without approval, but it stops for explicit approval before creating a durable concept or changing a project. `--dry-run` writes nothing at all, including log bullets.

```bash
fkf list tasks --since 7d
fkf list wiki && fkf list projects --status active
fkf validate --strict && fkf build
```
