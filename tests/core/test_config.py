from __future__ import annotations

from pathlib import Path

import pytest

import fkf.config as config_module
from fkf.config import BodyPolicy, IdentityKind, OutputFormat, decode_strict_yaml, load_config, valid_body_value
from fkf.errors import FKFError
from fkf.store import Layer
from fkf.timeutil import parse_duration

SCHEMA = """\
fkf: 1
name: brain
schema:
  id: {description: Stable provider identity., cardinality: one}
  time: {description: Event time., cardinality: one}
  title: {description: Meaningful subject line., cardinality: optional}
  repo: {description: Provider repository value., cardinality: optional}
  author:
    description: Canonical author identities.
    cardinality: many
    relation: true
    examples: ["actor:github.com/fmind"]
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
"""


def write_base(tmp_path: Path, body: str, local: str | None = None) -> Path:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(SCHEMA + body, encoding="utf-8")
    if local is not None:
        (root / "fkf.local.yaml").write_text(local, encoding="utf-8")
    return root


def test_load_config_resolves_defaults_sources_identities_and_local_origins(tmp_path: Path) -> None:
    root = write_base(
        tmp_path,
        """\
identities:
  fmind:
    canonical: person:email/fmind@example.com
    aliases: [fmind, actor:github.com/fmind]
    kind: person
    owner: true
sources:
  github-pull-requests:
    enabled: true
    run: [gh, search, prs, "--updated={{date}}"]
    fields:
      id: .number
      time: .updatedAt
      title: .title
      repo: .repository.nameWithOwner
      author: [".author_uri", ".reviewer_uris[]"]
    body: [gh, pr, view, "{{id}}", --repo, "{{repo}}"]
    bodies: cache
    retry: {attempts: 3, backoff: 30s, on: ["exit:1", rate limit]}
    min_interval: 5s
""",
        """\
bin: [~/tools]
sources:
  github-pull-requests:
    enabled: false
    timeout: 30s
""",
    )

    config = load_config(root)

    assert config.fkf == 1
    assert config.name == "brain"
    assert config.path == root / "fkf.yaml"
    assert config.local_path == root / "fkf.local.yaml"
    assert config.sync.days == 30
    assert config.sync.index_max_age_hours == 168
    assert config.sync.timeout == parse_duration("2m")
    assert config.sync.concurrency == 4
    assert config.layers[Layer.EVENTS] is True
    assert config.bin == ("~/tools",)
    assert config.origins == {
        "bin": root / "fkf.local.yaml",
        "sources.github-pull-requests.enabled": root / "fkf.local.yaml",
        "sources.github-pull-requests.timeout": root / "fkf.local.yaml",
    }

    identity = config.identities["fmind"]
    assert identity.kind is IdentityKind.PERSON
    assert identity.effective_kind() is IdentityKind.PERSON
    assert identity.aliases == ("fmind", "actor:github.com/fmind")
    assert identity.owner is True

    source = config.sources["github-pull-requests"]
    assert source.enabled is False
    assert source.layer is Layer.EVENTS
    assert source.format is OutputFormat.JSON
    assert source.bodies is BodyPolicy.CACHE
    assert source.timeout == parse_duration("30s")
    assert source.retry.attempts == 3
    assert source.retry.backoff == parse_duration("30s")
    assert source.retry.on == ("exit:1", "rate limit")
    assert source.retry_attempts() == 3
    assert source.min_interval == parse_duration("5s")
    assert source.has_body()
    assert source.caches_bodies()
    assert source.body_field_names() == ("id", "repo")
    assert config.source_names() == ("github-pull-requests",)
    assert config.enabled_sources() == ()
    assert config.store().root == root


@pytest.mark.parametrize(
    ("body", "message"),
    [
        ("name: other\n", "duplicate key"),
        ("---\nname: other\n", "more than one YAML document"),
        ("sources:\n  s:\n    enabled: yes\n    run: [cli]\n", "enabled"),
        ("unknown: true\n", "unknown"),
    ],
)
def test_load_config_rejects_ambiguous_or_unpublished_yaml(tmp_path: Path, body: str, message: str) -> None:
    root = write_base(tmp_path, body)

    with pytest.raises(FKFError, match=message):
        load_config(root)


def test_config_yaml_graph_is_bounded_before_alias_construction() -> None:
    recursive = b"value: &value [*value]\n"
    with pytest.raises(FKFError, match="recursive YAML alias"):
        decode_strict_yaml(recursive, Path("fkf.yaml"))

    aliases = ["value: &n0 [leaf]\n"]
    aliases.extend(f"level{depth}: &n{depth} [*n{depth - 1}, *n{depth - 1}]\n" for depth in range(1, 18))
    with pytest.raises(FKFError, match="alias expansion"):
        decode_strict_yaml("".join(aliases).encode(), Path("fkf.yaml"))

    nested = b"value: " + b"[" * 129 + b"leaf" + b"]" * 129 + b"\n"
    with pytest.raises(FKFError, match="depth limit"):
        decode_strict_yaml(nested, Path("fkf.yaml"))


def test_config_yaml_graph_limits_expanded_nodes_independently(monkeypatch: pytest.MonkeyPatch) -> None:
    assert config_module.MAX_CONFIG_YAML_EXPANDED_NODES == 100_000
    # Lower only the test threshold to avoid parsing 100,000 nodes on every run.
    monkeypatch.setattr(config_module, "MAX_CONFIG_YAML_EXPANDED_NODES", 8)
    many_nodes = b"value: [one, two, three, four, five, six]\n"

    with pytest.raises(FKFError, match="8-node limit"):
        decode_strict_yaml(many_nodes, Path("fkf.yaml"))


def test_config_yaml_graph_limits_expanded_scalar_bytes_independently() -> None:
    shared = b"x" * 5_000
    repeated = b"shared: &shared " + shared + b"\nvalues: [" + b"*shared," * 1_000 + b"]\n"

    with pytest.raises(FKFError, match="4194304-byte scalar limit"):
        decode_strict_yaml(repeated, Path("fkf.yaml"))


def test_config_yaml_graph_accepts_bounded_aliases() -> None:
    bounded = b"shared: &shared [one, two]\nfirst: *shared\nsecond: *shared\n"

    assert decode_strict_yaml(bounded, Path("fkf.yaml")) == {
        "shared": ["one", "two"],
        "first": ["one", "two"],
        "second": ["one", "two"],
    }


def test_local_overlay_is_closed_and_may_only_override_declared_sources(tmp_path: Path) -> None:
    root = write_base(tmp_path, "sources: {}\n", "sources:\n  invented:\n    enabled: true\n")

    with pytest.raises(FKFError, match="is not declared"):
        load_config(root)

    (root / "fkf.local.yaml").write_text("identities: {}\n", encoding="utf-8")
    with pytest.raises(FKFError, match="identities"):
        load_config(root)


def test_config_rejects_unsafe_execution_surfaces(tmp_path: Path) -> None:
    root = write_base(
        tmp_path,
        """\
sources:
  source:
    enabled: true
    run: ["{{home}}/cli", "{{unknown}}"]
    fields: {id: .id, time: .time, title: .title}
""",
    )

    with pytest.raises(FKFError, match="literal executable"):
        load_config(root)

    (root / "fkf.yaml").write_text(
        SCHEMA
        + """\
sources:
  source:
    enabled: true
    run: [cli]
    fields: {id: .id, time: .time, title: .title, repo: .repo}
    body: [cli, view, "{{repo}}"]
""",
        encoding="utf-8",
    )
    with pytest.raises(FKFError, match=r"body must name \{\{id\}\}"):
        load_config(root)


def test_command_paths_must_stay_outside_the_base_even_through_symlinks(tmp_path: Path) -> None:
    root = write_base(tmp_path, "")
    inside = root / "tools"
    inside.mkdir()
    alias = tmp_path / "machine-tools"
    alias.symlink_to(inside, target_is_directory=True)
    (root / "fkf.yaml").write_text(SCHEMA + f'bin: ["{alias}"]\n', encoding="utf-8")

    with pytest.raises(FKFError, match="inside the base"):
        load_config(root)


def test_body_values_are_opaque_argv_but_not_options_or_ambiguous_text() -> None:
    for value in ("42", "fmind/fkf", "révision n° 42", "a; $(not-a-shell) | 👍"):
        assert valid_body_value(value)
    for value in ("--help", "@response-file", "a\nb", "a\tb", "", "   ", "a\u200bb", "a" * 257):
        assert not valid_body_value(value)
