# fkf improvement plan

Written on 2026-09-02 from a full review of `main` at `5a8bc4e` (v2.1.0), the live base `~/fmind/brain`, and a survey of comparable projects. The review report with evidence, the ranked defect table, and the landscape is at <https://claude.ai/code/artifact/93f2848a-a8db-4337-8e66-20a78f80b9ae>. This file is the implementation brief that follows it: read `AGENTS.md` and `.agents/skills/fkf-contribute/SKILL.md` first, take the slices in order, and finish every slice with focused tests plus a green `mise run all`. Do not commit, push, or publish unless the owner asks.

## 0. Owner decisions

These were settled with the owner on 2026-09-02 and are not open for re-litigation inside a slice.

1. **One base per boundary, never merged.** The customer laptop keeps a completely separate base. No federation, cross-base read, or merge feature. Every improvement must work on a fresh macOS base as well as on `~/fmind/brain`.
1. **Lexical retrieval, with a derived index allowed.** A rebuildable, git-ignored, digest-bound lexical index is acceptable as a cache. No embedding model, no LLM call, and no hosted service in any collection or read pipeline.
1. **Harness-agnostic by construction.** The owner switches between Antigravity, Claude Code, Codex, Copilot CLI, Gemini CLI, OpenCode, and Grok daily. Every integration ships for all supported harnesses through one command, and session capture reads the harness-independent session store.
1. **Login-aware, opportunistic sync.** Provider logins expire on some machines. Collection must probe readiness per source, skip a logged-out provider with a visible gap instead of failing the run, and catch up on its own when the login returns. Runs must be cheap when nothing is due; the default cadence is hourly.
1. **Declared identities.** The owner wants a mapping between identities (several email addresses, GitHub login, organisations and their people). Declared in files, never inferred.
1. **Links, not copies.** A record keeps a link to where its content lives and a meaningful subject line; bodies are fetched on demand through `body:`. A per-source `bodies:` policy may cache fetched bodies in an ignored, rebuildable cache, and may prefetch them for explicitly opted-in sources. Shipped presets keep mail bodies at `none`; a private base may deliberately opt mail or other conversational text into its owner-only cache.
1. **No coupling with the owner's dotfiles.** fkf ships its own triggers and installers; nothing in this plan depends on the `dot` CLI.
1. **Target experiences.** The plan is done when an agent connected to the base handles these without being given a link:
   - "Take my last meeting notes."
   - "What was my meeting with IMA about?"
   - "What did I do yesterday?" and "What did I do this week?"
   - "Who is Maxime Cordy?"
   - "Prepare my day."
   - "What changed in kagglathon this week?"

## 1. Constraints that stay

- One module, one binary, POSIX only, plain JSON and Markdown, explicit URIs, `fkf: 1` markers, additive evidence envelope.
- Stored reads execute nothing and open no network. `read --body`, `sync`, `test`, trusted `brief`, and explicit `status --live` are the only commands that execute declared provider argv; every execution, including `auth:` probes, is trust-digested.
- Collected content is untrusted data and never reaches a shell or executable position.
- Same query, base, binary, and `as_of` yield the same pack and receipt. A derived index must not change ranking results, only latency.
- Per-day, per-source documents and one writer lock per base stay as they are. Every new artefact under the base is either authored Markdown, collected JSON, or a rebuildable cache.
- `AGENTS.md` wording to update in slice A3: "no database" becomes "no source-of-truth database; derived caches must be rebuildable by `fkf build`, digest-bound, and git-ignored".

## 2. What each target experience needs

| Experience                       | Needs                                                                                                                        | Slices     |
| -------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ---------- |
| "Take my last meeting notes"     | a `meeting-notes` source with `bodies: sync`, a `body:` helper that prints document text, "last" in the temporal grammar     | C1, B4, A1 |
| "My meeting with IMA"            | organisation aliases (people, domains, names), calendar records linked to their notes document, a timeline around the event  | B5, C1, B1 |
| "What did I do yesterday / week" | a deterministic day and range digest, compact rendering, the digest exposed over MCP and the session hook                    | B1, B2, A2 |
| "Who is Maxime Cordy"            | identity aliases, a person page or a generated profile, the last interactions across sources                                 | B5, B1     |
| "Prepare my day"                 | `fkf brief`: yesterday's digest, today's calendar, open items, failing CI, stale sources, unharvested learnings; delta packs | C4, B1     |
| "What changed in kagglathon"     | repository identity matching, per-source diversity in ranking, the range digest filtered by repository                       | B3, B1     |

## 3. Slices

Effort: S is about a day, M a few days, L a week or more. Each slice names its acceptance test; add hermetic Go tests for every behaviour change and a fixture under `services/testdata/sources/` for every new preset source.

### Workstream A: make the base alive

#### A1. Login-aware, opportunistic sync (M)

1. Add an optional per-source `auth: [executable, argument...]` argv. It runs with stdout and stderr discarded and bounded, never logged, before any `run:` of that source in a sync. Exit 0 means ready; an ordinary non-zero provider exit marks the source `auth-required` for that run and skips it. A missing executable, timeout, signal, unsafe path, trust drift, or runner failure remains a hard error so infrastructure and policy defects cannot masquerade as login gaps. Local sources declare no `auth:`. The probe is execution and enters the trust digest and the `fkf trust` disclosure.
1. Preset probes: `[gh, auth, status]`, `[gws, auth, status]`, `gcloud-auth-ready.sh` around `[gcloud, auth, list, --filter=status:ACTIVE, --format=value(account)]` so empty successful output still means logged out, `[kaggle, config, view]`, `[hf, auth, whoami]`. Never use a probe that prints a token.
1. `fkf sync --if-due`: compute the plan without taking the lock; exit 0 with one line when nothing is due; take the lock only to write. Rebuild the graph only when a document was written; rebuild the lexical cache when a document or verified cached body changed.
1. `status` and the receipt gain `auth_required: [source...]`; `fkf brief` (C4) turns it into one action line, for example "log in to gws to collect calendar and mail".
1. `fkf schedule install|status|remove`: writes a systemd user timer on Linux and a launchd agent on macOS that run `fkf sync --if-due --base <base>` hourly and `fkf build --if-stale` afterwards. The unit exports `HOME` and a closed `PATH`, because `{{home}}` and the state directory misbehave without `HOME` (A4). `bin/fkf-hook.sh` may spawn a detached `fkf sync --if-due` at session start that never blocks the agent, and any login wrapper may run the same command after a successful login.
1. Acceptance: with `gws` logged out, `fkf sync` collects every local and GitHub source, writes no Google day, reports `auth_required: [google-calendar-events, ...]`, and exits 0; the next run after login fills the missing span in one windowed call. A timer run with no due work finishes in under one second.

#### A2. Harness integration in one command (M)

1. Add `fkf harness list`, `fkf harness print <name>`, and `fkf harness install <name>... | --all [--dry-run] [--check]`. Supported names: `claude`, `codex`, `gemini`, `copilot`, `antigravity`, `opencode`, `grok`, `cursor`, `kiro`, `cline`.
1. For each harness, `install` registers the read-only MCP server with the absolute base path, the session-start hook `bin/fkf-hook.sh <name>` with the harness's event and matcher (Claude Code: `startup|compact`, smaller pack on `compact`), and the skills bridge when the harness does not read `.agents/skills/` natively. Writes are idempotent managed entries, back up the file first, and print exactly what changed. `print` emits the same fragments for dotfile templates.
1. `fkf init` prints the `harness install --all` line among its next steps, and `status` reports which harnesses are registered for this base.
1. Acceptance: hermetic tests over fixture config files for every harness; `install --check` exits 1 when an entry drifts; the brain gets Claude Code, Codex, Gemini, Copilot, Antigravity, OpenCode, and Grok wired in one run.

#### A3. Writers rebuild, versions, demo, contract wording (S)

1. `fkf new` and `fkf init` rebuild the graph (and later the index) the way `sync` does, so `graph`, `read <entity>`, `context --expand`, and the MCP `graph` tool keep working after a trace is scaffolded (`services/graph.go:1204,1219`).
1. Stamp the release version in `mise.toml` builds with `-ldflags "-X github.com/fmind/fkf/core.version=$(git describe --tags --always)"`; `core/version.go:35-40` prefers the stamped value.
1. Make `init --demo` declare its six sources as disabled entries with the synthetic helper, so `status`, `sync --dry-run`, and `trust` behave on the tour the root help advertises (`services/demo.go`, `cmd/fkf/main.go:70-73`).
1. Update `AGENTS.md` for the derived-cache wording (section 1) and document that a stale `go install` binary shadows a release install; `install.sh` and `fkf upgrade` warn when another `fkf` precedes the target on `PATH`.

#### A4. Defect batch one: correctness and security (M)

Fix, with a regression test each:

1. Empty string in a mapped field fails the day with a misleading error (`core/fieldpath.go:506-509,797-799`): empty is absent for `optional` and `many`, an explicit "empty identity" error for `one`.
1. Forbidden root derived from cwd deletes `HOME` and `XDG_*` for children when cwd is `/`; `fkf upgrade` never sees `~/.curlrc` (`core/shell.go:245-248,349-370`; `services/upgrade.go:111,128,144`). Only honour an explicit forbidden root.
1. `retry.on` substrings match fkf's own error text (`sources/policy.go:133-140`): consult only the command-failure oracle.
1. `{{home}}` expands to an empty string when `HOME` is unset (`sources/run.go:324-330`; `sources/collect.go:552`): fail the plan instead.
1. Trust is digested once per process (`core/trust.go:598-612`; `services/sync.go:106,184`; `sources/run.go:191-202`): re-digest `bin/` and `tests/` before each exec and fail the unit on drift.
1. State directory falls back to a shared temp dir without `HOME` (`core/trust.go:203-212`; `core/writer_lock.go:43`): fail closed.
1. Truncation reported before cancellation and a buffer that keeps acknowledging bytes after truncation (`core/shell.go:108-117,263-271`).
1. Loader deny-list adds `LD_AUDIT`, `PERLLIB`, `RUBYGEMS_GEMDEPS`, `GEM_PATH` (`core/shell.go:325-336`).
1. Body argv: document that every max-one field named by the template is substituted, flag `@`-prefixed values in preset review, and align `AGENTS.md` with `sources/collect.go:553-589`.
1. Unquoted YAML dates and numbers in frontmatter decode to empty strings (`services/markdown.go:201-210`; `core/fieldpath.go:795-812`).
1. `status` marks commit, PR, and calendar sources quiet every weekend (`services/status.go:606-636`): compare against the same weekday, or against days with any collected activity.
1. Helper drift triggered by mode alone (`services/helpers.go:240`).
1. `find` summary after `--limit` and the `days` array in CLI JSON (`services/find.go:137-148`); `input_digest` includes budget, expand, explain, and pins (`services/context.go:1095-1131`).
1. `mcp serve` refuses the root `--base` flag position and lists `--base` twice (`cmd/fkf/commands_run.go:436-441`).
1. Windowed sources run the whole missing span under one per-command timeout (`services/sync.go:445-539`): scale the timeout by span days, or split spans at a documented maximum.

### Workstream B: answer the questions people ask

#### B1. `fkf day` and `fkf timeline` (M)

1. `fkf day [date|yesterday|2026-08-28] [--budget N]` renders one day chronologically: per-source groups, one line per item, identical titles collapsed with a count, noisy sources (`shell-commands`, `agent-prompts`, `github-runs`, `google-drive-changes`) summarised as counts unless `--all`, people met, repositories touched, in text and JSON with a receipt.
1. `fkf timeline --since <window> [--until] [--repo <uri>] [--person <uri>] [--source ...]` renders a range the same way; `fkf timeline <uri> --around 2h` returns the records near one record across sources.
1. Expose both as MCP tools (`day`, `timeline`) and use `day yesterday` plus the repository pack in `bin/fkf-hook.sh` for the `startup` event; keep the `compact` event small.
1. Acceptance: the 2026-08-28 digest of the brain fits in 600 tokens and lists the AAIF lunch with its attendees, the ten fgraph commits, and the Decathlon busy blocks; golden tests over the demo base pin the bytes.

#### B2. Compact pack format and honest budgets (S)

1. Make the line format the default for `--format text` and for the hook: `score kind date uri title · field=value ...`, receipt in three lines. Keep indented JSON for `--format json` but drop the raw `record` and the `days` array unless `--raw` is passed.
1. Account the budget on the bytes of the format actually delivered; document the receipt floor; `--budget` below the floor fails with the minimum budget as today.
1. Acceptance: the kagglathon pack on the brain carries the same 14 items in under 900 tokens in text; JSON stays byte-reproducible.

#### B3. Ranking v6 with an evaluation set (M)

1. Whole-token matching on the `isTermRune` boundary; substring only for identifier-shaped terms containing `-`, `/`, `:`, `@`, or `.`; plain terms need three runes (`services/context.go:527,546,558`).
1. Document-frequency stop rule: a term present in more than half the candidates scores zero; log-scaled rarity capped at 16 (`services/context.go:620-626`).
1. Remove field names and source names from the haystack (`services/context.go:481-500`); the identifier bonus applies only to relation fields, entity identities, ids, slugs, and exact titles (`services/context.go:33,601-618`).
1. Match the entity identity suffix after the last `/` so `marc@x.test` hits `person:email/marc@x.test`.
1. Optional integer `weight:` per root schema field (defaults `id` 10, `title` 5, others 1) with per-field length normalisation; the digest covers it.
1. Selection diversity: a per-source share cap (40 percent of items) with a reserved share for wiki and project pages; identical title and source runs collapse into one item with `count`; `wiki/index.md` and `wiki/log.md` rank below concept pages.
1. Expansion truncates hub entities to the newest N inbound edges inside the window and names `truncated_entities` in the receipt instead of failing (`services/context.go:670-730,766-775`).
1. Per-source `recency: { half_life_days: N }` replacing the single linear 30-day bonus, off for undated layers; receipt gains `recency_model`.
1. Ship `evals/queries.yaml` in the base (question, window, expected URIs, forbidden URIs) and `fkf eval` printing recall at k and a pass or fail against thresholds; seed it with section 2 and replace the tautological smoke test (`services/context_test.go:644-705`).
1. Acceptance: on the brain, `context "Declarative source runner"` style queries return the page first; the hook query for `fmind/fkf main` returns at most two CI notifications; every eval query passes.

#### B4. Temporal grammar (S)

1. Parse a closed set at the start or end of a query: `yesterday`, `today`, `last week`, `this week`, `last <weekday>`, `<weekday>`, `2026-08`, `2026-08-28`, `since <date>`, `last` as "newest matching". Derive `--since/--until`, echo `receipt.window` with `derived_from`, reject ambiguous forms.
1. `--until` alone must not scan all history (`services/context.go:343-362`).

#### B5. Declared identities (M)

1. Root `identities:` in `fkf.yaml` (and `fkf.local.yaml` may not change it): canonical entity URI, `aliases` list of entity URIs and bare emails or logins, optional `kind` (`person`, `organization`, `repository`). Authored pages may carry the same under frontmatter `aliases:` with `type: person` or `type: organization`.
1. The graph build rewrites alias nodes to the canonical node and records a `same-as` edge for provenance; `context` and `find` treat every alias as the exact identifier; `graph nodes --kind person` lists canonical nodes; the owner's own identities are excluded from "people" listings and from the expansion seed set.
1. `fkf who <name|uri>`: the page if any, the canonical node, aliases, the neighbourhood by kind, and the last ten interactions across sources as a timeline.
1. Preset helper derives `actor:github.com/<login>` from `ID+login@users.noreply.github.com` commit emails; the brain's schema stops declaring `url` as a relation, which removes 18,903 self-links.
1. Acceptance: `fkf who "Maxime Cordy"` on the brain returns mail, calendar, and notes records under one node; `context "IMA"` resolves the organisation page and its people.

#### B6. Derived lexical index (M)

1. Add `index/.fkf-index.sqlite` (git-ignored, digest-bound like `graph.meta.json`) built by `fkf build index` and by `sync` when documents changed. Engine: SQLite FTS5 through a pure-Go driver so `sqlite3` can inspect it; if the driver cost is unacceptable, a plain-file postings TSV is the fallback. Spike both for one day before choosing.
1. The index is a candidate generator and term-statistics source only; scoring stays in Go so results are identical with and without it. A stale or absent index makes `context` and `find` fall back to the scan and say so in the receipt.
1. Acceptance: `context` under 300 ms on the brain with the index; the byte-identical pack property test passes with and without it; the 100k-record benchmark shows the index path under one second.

### Workstream C: make the brain know things

#### C1. Meeting notes as a first-class source (M)

1. Preset `meeting-notes` (layer `events`, `window: true`): Drive query for documents named `* - Notes by Gemini*` and for documents carrying an owner-chosen prefix, projecting id, time, title, url, owner, and a `meeting` relation to the calendar record when the calendar attachment names the document. `body: [gws-doc-text.sh, "{{id}}"]` prints the document as plain text through a reviewed helper.
1. Extend `gws-calendars-json.sh` to keep `attachments(fileId,fileUrl,title)` and `conferenceData.conferenceId`, projecting a `attachment` relation field, so the event links to its notes; when no attachment exists, the notes helper joins by title prefix and start time and emits the relation itself.
1. `bodies: sync` (see C2) on `meeting-notes` caches each new document's text so lexical retrieval can match the content without copying it into the evidence record. The customer base also runs on Google Workspace, so the same helper serves both.
1. Acceptance: on the brain, `fkf context "last meeting notes"` returns the newest notes record first and `read --body` prints its text; `fkf timeline <notes-uri> --around 2h` shows the calendar event and the attendees.

#### C2. Subject lines and the bodies cache (M)

1. Every record must project a meaningful `title`. For content-first records without a natural subject (agent prompts, harness memory files, chat messages), the helper derives it deterministically from the first line or the first 160 characters, bounded and control-character free. `validate` warns when more than half of a source's records share one title.
1. Add a per-source `bodies:` policy: `none` (default, nothing stored), `cache` (store the text after an explicit `read --body`), or `sync` (fetch and store the text for new records during collection). Stored bodies live under an ignored, rebuildable `bodies/<source>/<record>.txt` with a manifest carrying the record URI, the provider's modified time, and a size bound. The cache is never versioned or mirrored, `fkf build bodies --prune` empties it, and a record always keeps its link.
1. `find --bodies` and `context` index cached bodies with weight 1; the receipt lists which bodies were consulted. Stored reads stay offline: a body that is not cached is not fetched during a read.
1. Presets: `sync` for `meeting-notes` and `agent-memory-files` (the agents' own reflections, small and local), `none` for mail, chat, and every provider source; `cache` may be turned on per source by the owner.
1. Add `category` (`created`, `received`, `saved`) and `visibility` (`private`, `shared`, `public`) as optional preset roles so `context` can weight self-authored evidence and exclude received or private records by default.
1. Acceptance: after one sync, the newest "Notes by Gemini" document is findable by a phrase from its body; wiping `bodies/` and rerunning `sync` restores it; a mail body never appears under `bodies/` unless the owner set `cache` and ran `read --body`.

#### C3. Session traces from every repository and a staged learn queue (M)

1. Preset helper `agent-session-trace.sh` reads the harness-independent session store (`~/.agents/sessions/v1`) and writes a bounded `tasks/<date>/<repo>-<sid>/TASKS.md` skeleton for each completed session: requests, files changed from git, verification commands seen, last assistant message, harness and model. No model call. Run it from `fkf sync` as an ordinary source that writes Markdown through a dedicated `layer: tasks` mode, or from a session-end hook; either way it is trust-digested and idempotent.
1. `fkf learn propose|review --diff|apply|reject`: proposals live under `.agents/tmp/learn/` as unified diffs against `wiki/` and `projects/`; `apply` writes through the existing validators and rebuilds. The `fkf-learn` skill writes proposals instead of pages, so approval is a diff review rather than a conversation, and any harness can run it.
1. A documented nightly routine (owner-scheduled agent, not fkf) reads the day's traces and cached memory bodies and files proposals.
1. Acceptance: after one Codex session in `~/fmind/kagglathon`, the brain holds a trace naming the changed files; `fkf learn propose --dry-run` lists candidate log bullets citing it.

#### C4. `fkf brief` and delta packs (M)

1. `fkf brief [--budget N]`: yesterday's digest, today's calendar, open PRs and issues assigned to the owner, failing CI runs, tasks due, `auth_required` and stale sources, unharvested learnings, active projects touched this week; text and JSON with a receipt. The `daily-brief` skill only narrates it.
1. `context --since-receipt <input_digest>` returns only records and pages new or changed since that receipt's snapshot, so a repeated brief costs only the delta.

#### C5. Knowledge lint and validity windows (M)

1. `validate --lint`: orphan wiki pages, dangling URIs, relative dates in frontmatter, project pages untouched for N days, `valid_until` in the past on an active page, missing `supersedes` targets.
1. Frontmatter `valid_from`, `valid_until`, and a `supersedes` relation; `context` filters by `as_of` and extends the `superseded` penalty; conflicts resolve by the newest date in code.

### Workstream D: scale and code health

#### D1. Graph reads in constant time (M)

1. Store a per-input manifest (uri, size, mtime, sha) in `graph.meta.json`; validate reads by stat and hash only changed inputs; keep the full digest for `build` and `--verify` (`services/graph.go:115-189,1132-1209`).
1. Add a `src` offset table and a `dst`-sorted twin so a walk step is a seek (`services/graph.go:878-906`); `status` reads each document once.

#### D2. Upstream the brain into the presets (M)

1. Move the reviewed brain helpers into `presets/bin/` with fixtures: RSS, bookmarks, GitHub runs, commits, events, stars, gists, all calendars, Chat, Docs, Meet, Tasks, Kaggle, Hugging Face, mise tools, agent prompts, writing documents.
1. Fix the known preset defects: Drive pagination needing `nextPageToken`, Chat spaces as `index`, Tasks upper bound, GitHub search rate handling, `ResolveLink` dropping URL-valued fragments, missing `timeout:` on `gmail-json.sh`, the Chromium live-database note.

#### D3. Structure and tests (M)

1. Split `services/graph.go`, `services/context.go`, `services/status.go`, `mcpserver/server.go`, `cmd/fkf/text.go`, and `core/config.go` along their natural seams; merge `find.go` and `find_page.go` scanners, the two marked-block parsers, and the four symlink walkers; keep one trust API family; delete the unused readers, `RunCLIStdin`, and the Windows branch.
1. Move repository-wide tests (notices, docs assets, preset scripts, `go list -deps`) out of `services/` into an `internal/checks` package; stop mutating `time.Local` in tests; add the missing coverage named in the review.
1. Trim rationale comments that describe a previous design (`core/shell.go:190-192,318-323`; `core/process_unix.go:10-12`; `core/executable.go:60-62`).
1. MCP polish: explicit `read` input schema, bounds on every string input, defaults and one example in each description, namespaced `_meta` graph-generation digests on graph-derived tool results, and `_meta` result-size hints. Protocol `ttlMs` applies to lists and resource reads, not `tools/call`; those volatile results stay immediately stale instead of inventing a tool-call TTL.

## 4. Order of work

1. A1, A2, A3 first: the base becomes fresh and connected, which makes every later change visible.
1. A4 next: the security and correctness items are small and each has a test.
1. B1, B2, B4: the digest, the compact format, and the grammar deliver the first two target experiences.
1. B5, C1, C2: identities, meeting notes, subject lines, and the bodies cache deliver the meeting experiences.
1. B3, B6: ranking and the index, measured by `fkf eval`.
1. C3, C4, C5, then D1, D2, D3.

## 5. Not doing

- Embedding models, rerankers, or LLM calls anywhere in fkf.
- Base federation, cross-base reads, or a merged view of two laptops.
- A daemon, a socket, hosted storage, telemetry, or native Windows.
- A source-of-truth database; the lexical index is a cache.
- Always-on injection of large packs: session start gets one small digest, everything richer is on demand.

## 6. Open questions

None. Every decision is recorded in section 0; the mail-body answer (never stored, on demand only) is the reviewer's recommendation, accepted on 2026-09-02.
