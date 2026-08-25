---
title: fkf
layout: hextra-home
description: "Local, inspectable work context for coding agents: plain JSON and Markdown, URI-linked, token-budgeted, and owned by you."
---

{{< hextra/hero-badge link="docs/" >}} One binary, no account, no daemon, no credentials {{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}} Your coding agent knows your codebase perfectly and nothing about your job {{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}} The meeting behind a decision, the ticket that explains a constraint, the review that rejected an approach, and the page you read last week are rarely in the repository. `fkf` keeps that work context as plain JSON and Markdown, connects only declared relationships, and hands an agent a token-budgeted slice with a reproducible receipt. {{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Read the docs" link="docs/" >}}

{{< cards >}} {{< card title="A base is a folder" subtitle="Five typed layers of JSON and Markdown that ls, jq, rg, and future scripts can read without FKF." >}} {{< card title="A source is a command" subtitle="Keep direct provider argv inline; put pipelines or expansion in a trusted helper, or use any clearer executable. The named CLI owns its login." >}} {{< card title="The schema is yours" subtitle="Declare role-based fields, cardinality, descriptions, examples, and relations once; map each provider to them." >}} {{< card title="The graph is explicit" subtitle="Any lowercase entity scheme is valid. Edges come from declared relation fields, authored links, tags, and explicit frontmatter — never inference." >}} {{< card title="The base is the boundary" subtitle="No profiles, bundles, or visibility labels. Two disclosure boundaries are two repositories." >}} {{< card title="Context shows its work" subtitle="Every bounded pack carries scores, omissions, freshness, and semantic digests that make the result reproducible." >}} {{< /cards >}}

## Try it without connecting anything

`--demo` generates a local synthetic base. It runs no source command and reads no machine state:

```bash
fkf init ~/demo --demo 30
fkf status --base ~/demo
fkf find --base ~/demo retrieval
fkf context --base ~/demo "retrieval boundary" --budget 1024 --explain
fkf graph --base ~/demo topic:retrieval --in
```

The retrieval path is offline and model-free. `find` is exhaustive; `context` is selective and token-bounded; `read` opens one URI; `graph` follows transcribed relationships. Collected records remain untrusted data and every result carries a URI to cite.

Delete the demo when finished. Its only external state is the machine-local trust record under `$XDG_STATE_HOME/fkf/trust/`, deliberately stored outside the clone so a base cannot vouch for itself.
