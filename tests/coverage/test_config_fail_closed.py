from __future__ import annotations

from pathlib import Path

import pytest

import fkf.config as config_module
from fkf.config import (
    ConfigError,
    Identity,
    IdentityKind,
    Source,
    load_config,
    validate_identity_alias,
    validate_source_name,
)
from fkf.store import Layer

BASE = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Subject., cardinality: optional}
  related: {description: Related identities., cardinality: many, relation: true}
layers: {events: true, index: true, tasks: false, projects: false, wiki: false}
sources:
  source:
    enabled: true
    run: [provider, "{{date}}"]
    fields: {id: .id, time: .time, title: .title}
"""


def load_text(tmp_path: Path, text: str, *, local: str | None = None):
    root = tmp_path / "brain"
    root.mkdir(parents=True)
    (root / "fkf.yaml").write_text(text, encoding="utf-8")
    if local is not None:
        (root / "fkf.local.yaml").write_text(local, encoding="utf-8")
    return load_config(root)


def source_option(line: str) -> str:
    return BASE.replace("    fields:", f"    {line}\n    fields:")


def source_replace(old: str, new: str) -> str:
    return BASE.replace(old, new)


@pytest.mark.parametrize(
    ("text", "message"),
    [
        (BASE.replace("source:", "Bad_Name:", 1), "name must be lowercase"),
        (source_replace('run: [provider, "{{date}}"]', "run: []"), "run is required"),
        (source_option("auth: []"), "auth must contain"),
        (source_option("test: []"), "test must contain"),
        (source_option("layer: tasks"), "authored evidence"),
        (source_option("layer: invented"), "expected events or index"),
        (source_option("layer: projects"), "expected events or index"),
        (source_option("format: csv"), "expected json or ndjson"),
        (source_option("timeout: soon"), "timeout:"),
        (source_option("timeout: 2h"), "expected 0"),
        (source_option("min_interval: later"), "min_interval:"),
        (source_option("min_interval: 0s"), "positive duration"),
        (source_option("min_interval: 11m"), "positive duration"),
        (source_option("recency: {half_life_days: 0}"), "half_life_days"),
        (source_replace("title: .title", "title: [title, .name]"), r"fields.title\[0\]"),
        (source_option("records: not-a-path"), "field path 'not-a-path'"),
        (source_option("bodies: forever"), "expected none, cache, or sync"),
        (source_option("retry: {attempts: 0, backoff: 1s}"), "nothing would ever be retried"),
        (source_option("retry: {attempts: -1}"), "expected 1..5"),
        (source_option("retry: {attempts: 2}"), "retry.on is empty"),
        (source_option('retry: {attempts: 2, on: [""]}'), "empty condition"),
        (source_option('retry: {attempts: 2, on: ["exit:nope"]}'), "invalid syntax"),
        (source_option('retry: {attempts: 2, on: ["bad\\ncondition"]}'), "control character"),
        (source_option("retry: {attempts: 2, backoff: 11m, on: [exit:1]}"), "expected a duration up to"),
        (source_option("max_age_hours: 2"), "valid only for an index source"),
        (source_option("window: true\n    layer: index"), "only events support"),
        (source_replace("events: true", "events: false"), "is enabled but layers.events is false"),
        (source_replace('run: [provider, "{{date}}"]', 'run: [provider, "{{unknown}}"]'), "unknown placeholder"),
        (source_option('auth: [provider, "{{base}}"]'), "must be literal"),
        (source_replace("run: [provider", 'run: ["{{base}}/provider"'), "literal executable"),
        (source_option('requires: [provider, "bad/name"]'), "bare executable name"),
        (source_option("requires: [provider, provider]"), "more than once"),
        (source_replace("title: .title", "related: .related"), "fields.title is required"),
        (
            source_option('body: [provider, "{{id}}", "{{related}}"]\n    bodies: cache').replace(
                "title: .title}", "title: .title, related: .related}"
            ),
            "body placeholder {{related}}",
        ),
        (source_option("body: [provider]"), "at least one argument"),
        (source_option('body: ["{{id}}", value]'), "must not use collected placeholder"),
        (source_option('body: [provider, "{{unknown}}"]'), "unknown placeholder"),
        (source_option('body: [provider, "{{home}}"]'), "must name {{id}}"),
        (source_option("bodies: cache"), "body is not declared"),
    ],
)
def test_source_execution_contract_rejects_unsafe_or_ambiguous_declarations(
    tmp_path: Path, text: str, message: str
) -> None:
    with pytest.raises(ConfigError, match=message):
        load_text(tmp_path, text)


@pytest.mark.parametrize(
    ("text", "message"),
    [
        (BASE.replace("fkf: 1", "fkf: 2"), "fkf must be 1"),
        (BASE.replace("name: brain", 'name: ""'), "name is required"),
        (BASE.replace("name: brain", "name: Brain"), "must be lowercase"),
        (BASE.replace("name: brain", "name: " + "a" * 64), "expected at most 63"),
        (BASE.replace("events: true", "mystery: true"), "unknown layer"),
        (BASE + "sync: {days: 0}\n", "sync.days"),
        (BASE + "sync: {index_max_age_hours: 0}\n", "index_max_age_hours"),
        (BASE + "sync: {concurrency: 5}\n", "sync.concurrency"),
        (BASE + "sync: {timeout: 500ms}\n", "sync.timeout"),
        (
            BASE + "identities:\n  owner:\n    canonical: person:owner\n    aliases: [owner]\n    kind: alien\n",
            "kind 'alien'",
        ),
        (BASE + "identities:\n  Bad_Name:\n    canonical: person:owner\n    aliases: [owner]\n", "name must be"),
        (BASE + "identities:\n  owner:\n    aliases: [owner]\n", "canonical is required"),
        (BASE + "identities:\n  owner:\n    canonical: person:owner\n", "aliases must contain"),
        (
            BASE + "identities:\n  team:\n    canonical: org:team\n    aliases: [team]\n    owner: true\n",
            "owner may only mark a person",
        ),
        (
            BASE
            + "identities:\n"
            + "  one: {canonical: person:one, aliases: [shared]}\n"
            + "  two: {canonical: person:two, aliases: [SHARED]}\n",
            "also belongs",
        ),
        (
            BASE
            + "identities:\n"
            + "  one: {canonical: person:one, aliases: [one], owner: true}\n"
            + "  two: {canonical: person:two, aliases: [two], owner: true}\n",
            "at most one identity",
        ),
        (BASE + "bin: [relative/tools]\n", "absolute or ~-relative"),
    ],
)
def test_root_schema_identity_and_sync_invariants_fail_closed(tmp_path: Path, text: str, message: str) -> None:
    with pytest.raises(ConfigError, match=message):
        load_text(tmp_path, text)


def test_config_files_are_regular_bounded_utf8_documents(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    def fail_resolve(_root: object) -> Path:
        raise ValueError("unresolvable")

    monkeypatch.setattr(config_module, "resolve_absolute_path", fail_resolve)
    with pytest.raises(ConfigError, match="resolve base path"):
        load_config(tmp_path)
    monkeypatch.undo()

    missing = tmp_path / "missing"
    missing.mkdir()
    with pytest.raises(ConfigError, match="does not exist"):
        load_config(missing)

    directory = tmp_path / "directory"
    directory.mkdir()
    (directory / "fkf.yaml").mkdir()
    with pytest.raises(ConfigError, match="regular non-symlink"):
        load_config(directory)

    linked = tmp_path / "linked"
    linked.mkdir()
    target = tmp_path / "target.yaml"
    target.write_text(BASE, encoding="utf-8")
    (linked / "fkf.yaml").symlink_to(target)
    with pytest.raises(ConfigError, match="symlink"):
        load_config(linked)

    invalid = tmp_path / "invalid"
    invalid.mkdir()
    (invalid / "fkf.yaml").write_bytes(b"\xff")
    with pytest.raises(ConfigError, match="not valid UTF-8"):
        load_config(invalid)

    oversized = tmp_path / "oversized"
    oversized.mkdir()
    monkeypatch.setattr(config_module, "MAX_CONFIG_BYTES", 8)
    (oversized / "fkf.yaml").write_text("x" * 9, encoding="utf-8")
    with pytest.raises(ConfigError, match="expected at most 8"):
        load_config(oversized)


def test_identity_aliases_and_inferred_kinds_cover_supported_namespaces() -> None:
    assert Identity("person:a", ("a",)).effective_kind() is IdentityKind.PERSON
    assert Identity("actor:a", ("a",)).effective_kind() is IdentityKind.PERSON
    assert Identity("org:a", ("a",)).effective_kind() is IdentityKind.ORGANIZATION
    assert Identity("repository:a", ("a",)).effective_kind() is IdentityKind.REPOSITORY
    assert Identity("ticket:a", ("a",)).effective_kind() is None
    assert Identity("ticket:a", ("a",), kind=IdentityKind.ORGANIZATION).effective_kind() is IdentityKind.ORGANIZATION

    for alias in ("login", "owner@example.com", "person:owner"):
        validate_identity_alias(alias)
    for alias in (" padded ", "bad name", "@example.com", "owner@", "a@b@c"):
        with pytest.raises(ValueError, match=r"must|email"):
            validate_identity_alias(alias)
    for source in ("source", "source-2", "3-source"):
        validate_source_name(source)
    with pytest.raises(ValueError, match="expected at most"):
        validate_source_name("a" * 251)

    event = Source("event", layer=Layer.EVENTS)
    index = Source("index", layer=Layer.INDEX, max_age_hours=12)
    assert event.effective_max_age_hours(168) == 168
    assert index.effective_max_age_hours(168) == 12
    assert event.retry_attempts() == 1
