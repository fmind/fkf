# fkf — Fmind Knowledge Framework

[![CI](https://github.com/fmind/fkf/actions/workflows/ci.yml/badge.svg)](https://github.com/fmind/fkf/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/fmind/fkf?sort=semver)](https://github.com/fmind/fkf/releases/latest) [![Go Reference](https://pkg.go.dev/badge/github.com/fmind/fkf.svg)](https://pkg.go.dev/github.com/fmind/fkf) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Your coding agent knows your repository. It does not know the meeting that set the constraint, the review that rejected the approach, or the ticket that explains why the code looks like that.**

`fkf` collects that work history into a git repository you own — plain JSON and Markdown, gathered by the provider CLIs you already trust — and hands your agent a small, budgeted, reproducible slice of it on demand.

One binary. No account, daemon, database, or telemetry. Stored reads are offline.

## See it in 30 seconds

No credentials, no network, no configuration:

```bash
fkf init ~/demo --demo 30
fkf --format text context "retrieval boundary" --budget 1024 --explain --base ~/demo
```

The demo base is synthetic, but it is a real base: 30 days of events across six sources, a wiki, project pages, and a graph. This abridged result shortens URIs, fields, and receipt values:

```text
350 wiki   wiki/retrieval-boundary.md  Retrieval boundary · tags=decision,retrieval
           why=exact-identifier:+100(retrieval boundary),term:+100(retrieval …),exact-phrase:+50
182 record events/…/git-commits.json#…            Design retrieval boundary (LG-77)
182 record events/…/google-calendar-events.json#… Document retrieval boundary (GW-1203)
182 record events/…/google-gmail-emails.json#…    Fix retrieval boundary (FK-412)
182 record events/…/jira-issues.json#FK-418-4     Revert retrieval boundary (FK-418)
... 3 more selected items ...
 80 wiki   wiki/index.md  Wiki · navigation-page:-50(curated navigation ranks below concept pages)
receipt pack for "retrieval boundary" · 9/736 selected · <1024 text tokens · floor 10
window <30 days> · as_of <today> · digest <hex> · ranking v6 · dropped 717
```

One decision, scattered across a commit, a calendar invite, an email, and a Jira issue, pulled back together under a token budget.

Three things make that pack usable by an agent rather than merely interesting:

1. **It fits.** You set the budget; `context` selects the strongest evidence and stops. No pack silently blows past the window.
1. **It explains itself.** Every line carries the score and the reason it was chosen — and the receipt records what was dropped, so a wrong answer is debuggable instead of mysterious.
1. **It is reproducible.** Ranking is deterministic, and indexed and fallback reads return the same semantic answer. The receipt records the effective inputs, semantic digest, selection boundaries, and execution path. There are no embeddings or model calls in the read path.

Then look around the base with `fkf find`, `fkf graph repo:github.com/fmind/fkf --in`, and `fkf list projects --status active`. Delete `~/demo` when you are done.

## Install

The installer picks the right Linux or macOS archive, verifies it against the published checksums, and writes to `~/.local/bin` without `sudo`:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | sh
```

Require GitHub's release provenance attestation as well with an authenticated `gh`:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/fmind/fkf/main/install.sh | FKF_VERIFY_ATTESTATION=1 sh
```

Set `FKF_INSTALL_DIR` to an absolute directory to change the destination, or `FKF_VERSION` to pin a release. Archives and `checksums.txt` are on the [latest release](https://github.com/fmind/fkf/releases/latest); each archive holds the binary, license, README, and linked-dependency notices.

Already installed? `fkf upgrade` replaces the running executable in place, verifying the checksum first.

FKF supports Linux and macOS. WSL2 works when the base stays on its Linux filesystem; native Windows is out of scope because cancellation uses POSIX process groups.

<details>
<summary>Install from source</summary>

```bash
mise trust -y
mise install --locked
mise run install
```

The module intentionally stays `github.com/fmind/fkf`, so Go's major-version import rules keep `go install github.com/fmind/fkf/cmd/fkf@latest` on the v1 line. Use a release archive or a tagged checkout for v2 and later.

</details>

## Connect your own work

Start with one real source. The personal preset ships a reviewed GitHub Search helper — it needs `gh` and `jq` on your `PATH`, and `gh` owns the login:

```bash
gh auth status
fkf init ~/brain --preset personal
$EDITOR ~/brain/fkf.yaml                            # sources.github-pull-requests.enabled: true
fkf config helpers --refresh --base ~/brain
fkf trust --all --base ~/brain
fkf status --base ~/brain                           # confirms every requirement is on PATH
fkf sync github-pull-requests --days 30 --base ~/brain
fkf context "repo:github.com/OWNER/REPOSITORY" --since 30d --explain --base ~/brain
```

Set `FKF_BASE=~/brain`, or run from inside the base, to drop `--base`.

`fkf init` creates the five layers, `fkf.yaml`, managed git rules, the helpers your enabled sources need under `bin/`, and three agent skills under `.agents/skills/`. It contacts no provider and asks for no token. Browser, mail, and shell-history sources stay disabled until you turn them on.

From there, `fkf sync` is safe to re-run: existing event documents are skipped, due index snapshots refresh, the graph follows document writes, and the lexical cache rebuilds only when searchable bytes change. `fkf brief` gives you the daily loop — yesterday's digest, today's calendar, assigned work, failing CI, stale or login-blocked sources.

## The model

Four ideas cover most of FKF.

**A base is a folder.** One git repository, five readable layers. `ls`, `rg`, and `jq` still work.

```text
events/YYYY-MM-DD/  one complete JSON document per event source
index/              current point-in-time source documents
tasks/              agent session traces and learned items
projects/           active, paused, or completed efforts
wiki/               reusable decisions, patterns, tools, and insights
graph.tsv           rebuildable relation cache at the base root
```

**A source is a command.** A source runs a reviewed command that prints one JSON document, and the named CLI owns its login. Adding GitHub, Google Workspace, Jira, or a local database needs no Go adapter — just YAML and, when the glue gets real, a small reviewed helper under the base's `bin/`:

```yaml
sources:
  github-pull-requests:
    enabled: true
    layer: events
    requires: [github-search-json.sh, gh, jq]
    window: true
    run: [github-search-json.sh, prs, assignee, "{{start}}", "{{end}}"]
    fields:
      id: .url
      time: .updatedAt
      title: .title
      repo: .repository.nameWithOwner
      repository: .repository_uri # relation: builds a graph edge
      owner: [".assignee_uris[]"]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}", --json, "body,comments"]
```

**Relations are explicit URIs.** Records and pages link to file URIs or to entities such as `repo:github.com/fmind/fkf`. `graph.tsv` is built from declared relation fields and authored links — FKF never guesses a relationship from prose.

**Retrieval is bounded and reproducible.** `find` returns every lexical match; `context` selects under a budget and explains itself. By default, records are stored as metadata plus a link and bodies stay at the provider. The opt-in `cache` and `sync` body policies keep ignored, manifest-verified local copies after an explicit read or evidence sync.

Full detail: [sources](https://fmind.github.io/fkf/docs/sources/), [URIs and the graph](https://fmind.github.io/fkf/docs/uris-graph/), [context packs](https://fmind.github.io/fkf/docs/context/), [configuration schema](https://fmind.github.io/fkf/docs/schema/).

## Use it from your coding agent

```bash
fkf harness install --all --dry-run --base ~/brain
fkf harness install --all --base ~/brain
```

The first command shows exactly what FKF would manage. The second wires the base into Claude Code, Codex, Gemini, Copilot, Antigravity, OpenCode, Grok, Cursor, Kiro, and Cline, pinning the executable and base by absolute path. If you manage your client configuration yourself, the primitive is `fkf mcp serve --base ~/brain`.

The MCP server is read-only and bounded: `context`, `find`, `day`, `timeline`, `list`, `read`, and `graph`. It cannot write, run a shell, or fetch bodies. Connecting does not preload your base or train anything—the agent asks for a pack when it needs one. The bundled `fkf-use`, `fkf-learn`, and `daily-brief` skills teach it how. See the [harness guide](https://fmind.github.io/fkf/docs/harnesses/).

## Trust and privacy

- **FKF reads no credential** and expands no secret environment variable. Provider credentials stay with the provider CLI.
- **Collected content is untrusted data** — evidence, never instructions. A stored value never becomes shell syntax or an executable name.
- **`fkf trust` hashes the executable plan**: the effective `auth:`, `run:`, `test:`, and `body:` argv plus every file under the base's `bin/` and `tests/`. A meaningful change requires review again. It detects change; it is not a sandbox.
- **Stored reads are offline.** `read --body` is the explicit read-time fetch; `sync` may prefetch bodies under the opt-in `bodies: sync` policy, while `brief` and `status --live` run only bounded trusted `auth:` probes.
- **FKF encrypts nothing and provides no backup.** Protect the disk and the remote. Whether event and index documents enter git history is your choice at `init`, recorded in `.gitignore`.

Details and the full threat boundary: [privacy and trust](https://fmind.github.io/fkf/docs/privacy/).

## Scope

FKF is intentionally small. Do not use it if you need semantic search, a dashboard, a hosted service, native Windows, or a local cache your organization's data policy forbids. Your editor, shell, and coding agent remain the interface.

Configuration and stored documents each carry `fkf: 1`. Evidence-envelope additions stay compatible within that marker, so old evidence stays readable without re-fetching provider history.

## Development

Start with [CONTRIBUTING.md](CONTRIBUTING.md), [AGENTS.md](AGENTS.md), and the [Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities through a [private security advisory](https://github.com/fmind/fkf/security/advisories/new), never a public issue — see [SECURITY.md](SECURITY.md).

```bash
mise run all       # format, check, test, coverage, and build
mise run benchmark # optional 100k-record and 500k-edge observation
```

The suite is hermetic and race-enabled. The benchmark measures the supported 100,000-record and 500,000-edge envelope as a local observation, never as a cross-machine threshold.

Full documentation: <https://fmind.github.io/fkf/>

## License

MIT. See [LICENSE](LICENSE).
