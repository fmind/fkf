"""Filesystem address and confinement contracts."""

from __future__ import annotations

from pathlib import Path

import pytest

from fkf.store import (
    BASE_AGENTS_FILE,
    CONFIG_FILE_NAME,
    GRAPH_DST_FILE,
    GRAPH_FILE,
    GRAPH_GENERATION_FILE,
    GRAPH_META_FILE,
    GRAPH_OFFSETS_FILE,
    LAYERS,
    LOCAL_CONFIG_NAME,
    Layer,
    LayerDisabledError,
    NotAddressableError,
    PathEscapesError,
    Store,
    UnsafePathError,
    clean_relative,
    discover_base,
    expand_home,
    parse_layer,
    resolve_physical_path,
    validate_date,
    validate_directory_confinement,
    validate_within_root,
)


def all_layers() -> dict[Layer, bool]:
    return dict.fromkeys(LAYERS, True)


def test_store_activation_and_layer_parsing(tmp_path: Path) -> None:
    store = Store(tmp_path, {Layer.EVENTS: True, Layer.WIKI: True})
    assert store.enabled_layers == (Layer.EVENTS, Layer.WIKI)
    assert not store.enabled(Layer.PROJECTS)
    with pytest.raises(LayerDisabledError, match=r"layers\.projects: true"):
        store.directory(Layer.PROJECTS)
    assert parse_layer("  WIKI ") is Layer.WIKI
    with pytest.raises(ValueError, match="events, index, tasks, projects, wiki"):
        parse_layer("logs")


@pytest.mark.parametrize(
    "unsafe",
    ["../etc/passwd", "events/../../etc/passwd", "/etc/passwd", "~/secrets", "events\\windows", "events/\0", ".."],
)
def test_store_rejects_lexical_escapes(tmp_path: Path, unsafe: str) -> None:
    store = Store(tmp_path, all_layers())
    with pytest.raises(PathEscapesError):
        store.resolve(unsafe)


def test_store_resolves_and_round_trips_addressable_paths(tmp_path: Path) -> None:
    store = Store(tmp_path, all_layers())
    resolved = store.resolve("events/2026-08-22/gmail.json")
    assert resolved == tmp_path / "events" / "2026-08-22" / "gmail.json"
    assert store.relative(resolved) == "events/2026-08-22/gmail.json"
    assert store.resolve(".") == tmp_path
    with pytest.raises(PathEscapesError):
        store.relative(tmp_path.parent / "outside")


def test_store_admits_only_published_grammar(tmp_path: Path) -> None:
    store = Store(tmp_path, all_layers())
    accepted = [
        "events",
        "events/2026-05-04",
        "events/2026-05-04/gmail.json",
        "index",
        "index/github-repositories.json",
        "tasks",
        "tasks/2026-05-04",
        "tasks/2026-05-04/x",
        "tasks/2026-05-04/x/TASKS.md",
        "projects",
        "projects/a.md",
        "wiki",
        "wiki/b.md",
        GRAPH_FILE,
        GRAPH_DST_FILE,
        GRAPH_OFFSETS_FILE,
        GRAPH_META_FILE,
        GRAPH_GENERATION_FILE,
        CONFIG_FILE_NAME,
        BASE_AGENTS_FILE,
    ]
    for relative in accepted:
        assert store.resolve(relative)

    refused = [
        ".env",
        ".git/config",
        ".netrc",
        LOCAL_CONFIG_NAME,
        "credentials.json",
        ".agents/skills/fkf-use/SKILL.md",
        "sources/git-log-json.py",
        "README.md",
        "events/.env",
        "events/2026-05-04/SUMMARY.md",
        "events/2026-05-04/private.txt",
        "events/not-a-day/gmail.json",
        "events/2026-05-04/NESTED.json",
        "index/.env",
        "index/backup.key",
        "tasks/.env",
        "tasks/2026-05-04/x/private.md",
        "tasks/not-a-day/x/TASKS.md",
        "projects/.env",
        "projects/backup.key",
        "projects/nested/page.md",
        "wiki/.env",
        "wiki/backup.key",
        "wiki/nested/page.md",
    ]
    for relative in refused:
        with pytest.raises(NotAddressableError):
            store.resolve(relative)


def test_store_refuses_symlinks_below_selected_base_root(tmp_path: Path) -> None:
    root = tmp_path / "base"
    outside = tmp_path / "outside"
    root.mkdir()
    outside.mkdir()
    (root / "wiki").symlink_to(outside, target_is_directory=True)
    store = Store(root, {Layer.WIKI: True})
    with pytest.raises(UnsafePathError, match="symlink inside the base"):
        store.directory(Layer.WIKI)

    alias = tmp_path / "alias"
    alias.symlink_to(root, target_is_directory=True)
    alias_store = Store(alias, {Layer.WIKI: True})
    assert alias_store.root == alias


def test_confinement_rejects_links_and_non_directories(tmp_path: Path) -> None:
    real = tmp_path / "real"
    real.mkdir()
    link = tmp_path / "link"
    link.symlink_to(real, target_is_directory=True)
    dangling = tmp_path / "dangling"
    dangling.symlink_to(tmp_path / "missing")
    regular = tmp_path / "regular"
    regular.write_text("x")

    for path in (link, link / "child", dangling, regular / "child"):
        with pytest.raises(UnsafePathError):
            validate_directory_confinement(path)
    validate_directory_confinement(real / "missing" / "child")


def test_validate_within_root_rejects_outside_and_internal_link(tmp_path: Path) -> None:
    root = tmp_path / "base"
    root.mkdir()
    link = root / "link"
    link.symlink_to(tmp_path, target_is_directory=True)
    with pytest.raises(PathEscapesError):
        validate_within_root(root, tmp_path / "outside")
    with pytest.raises(UnsafePathError):
        validate_within_root(root, link / "file")


def test_path_helpers(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("HOME", str(tmp_path))
    assert expand_home("~/brain") == str(tmp_path / "brain")
    assert expand_home("~notauser/x") == "~notauser/x"
    assert clean_relative("events/./a.json") == "events/a.json"
    assert clean_relative("events/") == "events/"
    assert clean_relative("a/b/../c") == "a/c"
    assert clean_relative(".") == "."
    validate_date("2026-08-22")
    validate_date("")
    with pytest.raises(ValueError, match="YYYY-MM-DD"):
        validate_date("22/08/2026")


def test_resolve_physical_path_preserves_missing_suffix_below_alias(tmp_path: Path) -> None:
    real = tmp_path / "real"
    real.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(real, target_is_directory=True)
    assert resolve_physical_path(alias / "future" / "brain") == real / "future" / "brain"


def test_discover_base_precedence(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    explicit = tmp_path / "explicit"
    environment = tmp_path / "environment"
    discovered = tmp_path / "discovered"
    child = discovered / "a" / "b"
    child.mkdir(parents=True)
    (discovered / CONFIG_FILE_NAME).write_text("fkf: 1\n")
    monkeypatch.setenv("FKF_BASE", str(environment))
    monkeypatch.chdir(child)

    assert discover_base(str(explicit)) == (explicit.absolute(), "flag")
    assert discover_base("") == (environment.absolute(), "environment")
    monkeypatch.delenv("FKF_BASE")
    assert discover_base("") == (discovered.absolute(), "discovery")


def test_store_detects_real_git_metadata_without_following_symlinks(tmp_path: Path) -> None:
    root = tmp_path / "base"
    root.mkdir()
    store = Store(root, all_layers())
    assert not store.versioned
    dot_git = root / ".git"
    dot_git.mkdir()
    assert not store.versioned
    (dot_git / "HEAD").write_text("ref: refs/heads/main\n")
    assert store.versioned
    assert not store.enforce_permissions

    other = tmp_path / "other"
    other.mkdir()
    (other / "HEAD").write_text("ref: refs/heads/main\n")
    dot_git.replace(tmp_path / "old-git")
    dot_git.symlink_to(other, target_is_directory=True)
    assert not store.versioned
