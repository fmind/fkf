---
title: Overview
weight: 1
url: /docs/
description: "Understand fkf's local-first knowledge base, command runner, URI graph, deterministic retrieval, and agent integrations."
---

Your coding agent can read the repository. It cannot see the meeting that set a constraint, the review that rejected an approach, or the ticket that explains the final design.

`fkf` (Fmind Knowledge Framework) keeps that work history as plain JSON and Markdown in a git repository you own, then gives the agent only the strongest cited evidence under a token budget. [Try the synthetic demo](getting-started/#explore-without-connecting-a-provider) without credentials, network access, or configuration.

It is four things and nothing else:

1. **A folder convention.** A base is one git repository holding five typed layers. Anyone can read it with `ls`, `jq`, and `rg`; in twenty years a one-off script still can.
1. **A command runner.** A source is direct argv declared in the base's own `fkf.yaml`. The CLI that runs it owns the credential; `fkf` never reads a token.
1. **An open graph over URIs.** Every record and page has a relative URI; a base may declare any non-reserved lowercase entity scheme and shared relation field. The root-level `graph.tsv` transcribes those declarations and authored links.
1. **A bounded reader.** `context`, `find`, `day`, `timeline`, `who`, `read`, and `graph` answer across the base, `brief` narrows today to what needs attention, and `list`, `validate`, and `tags` inspect the layers. A context pack fits a token budget and carries a receipt saying what was selected and why.

## Where to start

- [Getting started](getting-started/) — install FKF, generate a demo base, then collect your first real day.
- [The base and fkf.yaml](base/) — understand the five layers, discovery, overrides, defaults, and trust inputs.
- [Configuration schema](schema/) — define shared field meanings and bind an editor to the generated JSON Schema.
- [Sources are commands](sources/) — compose direct argv and helpers, validate previews, and control body fetching.
- [URIs and the graph](uris-graph/) — use one address grammar and follow only declared relationships.
- [Context and receipts](context/) — see the closed ranking arithmetic, token budget, and reproducibility receipt.
- [Command reference](commands/) — find every command and its write or execution boundary.
- [MCP server](mcp/) and [agent harnesses](harnesses/) — connect a base to an agent through bounded offline reads.
- [Privacy and security](privacy/) — understand exactly what the trust and data boundaries protect.
- [The wiki format](okf/) — write flat, tagged OKF v0.2 pages for durable knowledge.

## The shape of a session

```bash
fkf status
fkf sync git-commits --preview
fkf sync --days 7
fkf context "<terms>" --explain
fkf read events/2026-05-04/github-pull-requests.json#https://github.com/fmind/fkf/pull/42
fkf graph ticket:jira/FKF-412 --in
```

This checks source readiness, validates one real collection without writing, fills missing completed days, selects a bounded context pack, opens exactly one record, and follows declared inbound relations. `fkf --help` is authoritative for the command surface; this site explains the reasoning behind it.
