"""Static and packaging invariants for FKF's shipped resource tree."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import zipfile
from datetime import UTC
from pathlib import Path, PurePosixPath

import pytest
import yaml

from fkf import assets as assets_module
from fkf.assets import BUNDLED_SKILLS, HOOK_SCRIPT, PRESETS, asset_files, read_asset, shipped_helpers, skill_digest
from fkf.documents import day_window, parse_day_in_location
from fkf.init import managed_ignore_block
from fkf.source_runtime import Environment, build_run_command
from fkf.timeutil import parse_duration

from .conftest import REPOSITORY, SOURCE_FIXTURES, load_preset


def test_resource_inventory_is_complete_confined_and_declared(tmp_path: Path) -> None:
    assert PRESETS == ("minimal", "personal", "team")
    assert BUNDLED_SKILLS == ("fkf-use", "fkf-learn", "daily-brief")
    assert {name for name, _ in asset_files("presets") if PurePosixPath(name).parent == PurePosixPath(".")} == {
        "minimal.yaml",
        "personal.yaml",
        "team.yaml",
    }

    declared_sources: set[str] = set()
    declared_requirements: set[str] = set()
    for preset in PRESETS:
        config = load_preset(tmp_path, preset)
        declared_sources.update(config.sources)
        for source in config.sources.values():
            declared_requirements.update(source.requires)

    fixtures = {path.stem for path in SOURCE_FIXTURES.glob("*.json")}
    assert fixtures == declared_sources
    helpers = shipped_helpers()
    assert HOOK_SCRIPT in helpers
    assert set(helpers) - {HOOK_SCRIPT} <= declared_requirements
    assert declared_requirements & set(helpers) == set(helpers) - {HOOK_SCRIPT}
    assert all("/" not in name and name == PurePosixPath(name).name for name in helpers)
    assert all(name.endswith((".sh", ".py", ".jq")) for name in helpers)


def test_preset_install_hints_match_declared_local_tools(tmp_path: Path) -> None:
    install_requirements = {
        "python@latest": "python3",
        "jq@latest": "jq",
        "sqlite@latest": "sqlite3",
    }
    for preset in PRESETS:
        config = load_preset(tmp_path, preset)
        for source in config.sources.values():
            if not source.install:
                continue
            for hint, executable in install_requirements.items():
                assert (hint in source.install) == (executable in source.requires), (
                    f"{preset}/{source.name} install hint disagrees with requires for {executable}"
                )


def test_every_preset_run_is_direct_bounded_argv(tmp_path: Path) -> None:
    window = day_window(parse_day_in_location("2026-05-04", UTC))
    for preset in PRESETS:
        config = load_preset(tmp_path, preset)
        environment = Environment(root=config.path.parent, environment={"PATH": "/usr/bin:/bin"})
        for source in config.sources.values():
            command = build_run_command(source, environment, window, parse_duration("1m"))
            assert command.argv[0] == source.run[0], f"{preset}/{source.name} changed executable during substitution"
            assert all("{{" not in argument and "}}" not in argument for argument in command.argv)
            for index, argument in enumerate(source.run):
                if argument == "--page-all":
                    assert source.run[index : index + 3] == ("--page-all", "--page-limit", "100"), (
                        f"{preset}/{source.name} delegates pagination without the finite page/token contract"
                    )
                    assert source.run[0] == "gws-page-json.py"


def test_shipped_collectors_have_no_nominally_unbounded_pagination() -> None:
    page_all = re.compile(r"""--page-all["']?,?\s+["']?--page-limit["']?,?\s+["']?100["']?""")
    for relative, content in asset_files("presets"):
        text = content.decode("utf-8")
        assert b"\r\n" not in content, f"{relative} is not LF-normalized"
        for number, line in enumerate(text.splitlines(), start=1):
            code = line.partition("#")[0]
            assert "--paginate" not in code, f"{relative}:{number} delegates an unbounded provider loop"
            assert "4294967295" not in code, f"{relative}:{number} uses a nominal page ceiling"
            if relative.startswith("sources/") and "--page-all" in code:
                assert page_all.search(code), f"{relative}:{number} lacks the adjacent --page-limit 100 contract"


def test_bundled_skills_have_valid_frontmatter_and_authoring_limits() -> None:
    description_length = 0
    for name in BUNDLED_SKILLS:
        data = read_asset(f"skills/{name}/SKILL.md")
        text = data.decode("utf-8-sig")
        assert text.startswith("---\n"), f"{name} has no opening frontmatter delimiter"
        frontmatter, separator, _ = text.removeprefix("---\n").partition("\n---\n")
        assert separator, f"{name} has no closing frontmatter delimiter"
        manifest = yaml.safe_load(frontmatter)
        assert isinstance(manifest, dict)
        assert manifest.get("name") == name
        description = manifest.get("description")
        assert isinstance(description, str)
        assert description.strip()
        if "license" in manifest:
            license_name = manifest["license"]
            assert isinstance(license_name, str)
            assert license_name.strip()
        assert len(text.removesuffix("\n").splitlines()) < 100
        description_length += len(description)
        assert len(description) <= 240
    assert description_length <= len(BUNDLED_SKILLS) * 175


def test_preset_regressions_stay_closed(tmp_path: Path) -> None:
    personal = load_preset(tmp_path, "personal")
    team = load_preset(tmp_path, "team")
    assert personal.sources["agent-sessions"].window is False
    assert personal.sources["agent-sessions"].run == ("agent-sessions.py", "{{start}}", "{{end}}")
    assert {"agent-sessions.py", "python3", "git"} <= set(personal.sources["agent-sessions"].requires)
    assert {"agent-memory-files.py", "agent-memory-body.py", "python3"} <= set(
        personal.sources["agent-memory-files"].requires
    )
    assert personal.sources["google-gmail-emails"].run == ("gmail-json.py", "{{start}}", "{{end}}")
    assert "python@latest" in personal.sources["google-gmail-emails"].install
    assert personal.sources["google-chat-spaces"].layer.value == "index"
    assert str(personal.sources["google-chat-spaces"].fields.paths("title").values[0]) == ".name"
    assert personal.sources["github-repositories"].run == ("github-list-json.py", "user-repositories")
    assert team.sources["github-repositories"].run == (
        "github-list-json.py",
        "org-repositories",
        "REPLACE_WITH_ORG",
    )
    assert {"agent-session-trace.py", "python3", "git"} <= set(personal.sources["agent-session-traces"].requires)
    assert {"agent-session-trace.py", "python3", "git"} <= set(team.sources["agent-session-traces"].requires)
    assert personal.sources["meeting-notes"].body == ("gws-doc-text.sh", "{{id}}")
    assert str(personal.sources["meeting-notes"].fields.paths("meeting").values[0]) == ".meeting_uris[]"
    assert str(personal.sources["meeting-notes"].fields.paths("attachment").values[0]) == ".attachment_uris[]"
    for name in ("github-pull-requests", "github-issues", "github-commits"):
        retry = personal.sources[name].retry
        assert retry.attempts == 3
        assert {"API rate limit exceeded", "secondary rate limit"} <= set(retry.on)
    rss_run = personal.sources["rss-items"].run
    private_feed = rss_run[rss_run.index("--optional-opml") + 1].rsplit("/", maxsplit=1)[-1]
    assert private_feed in managed_ignore_block(False).splitlines()


def test_built_wheel_exposes_the_exact_resource_tree(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    source_assets = tmp_path / "source-assets"
    shutil.copytree(
        REPOSITORY / "src/fkf/assets_data",
        source_assets,
        ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.pyo"),
    )
    cache = source_assets / "presets/sources/__pycache__"
    cache.mkdir()
    (cache / "gmail-body.cpython-314.pyc").write_bytes(b"ignored bytecode")
    (source_assets / "presets/sources/helper.pyc").write_bytes(b"ignored bytecode")
    (source_assets / "skills/fkf-use/references/helper.pyo").write_bytes(b"ignored bytecode")
    monkeypatch.setattr(assets_module, "asset_root", lambda: source_assets)

    output = tmp_path / "wheel"
    uv = shutil.which("uv")
    assert uv is not None
    completed = subprocess.run(  # noqa: S603 - the repository-standard build tool is intentional.
        [uv, "build", "--wheel", "--offline", "--out-dir", os.fspath(output)],
        check=False,
        capture_output=True,
        cwd=REPOSITORY,
        env={
            "HOME": os.fspath(tmp_path / "home"),
            "PATH": os.environ["PATH"],
            "UV_CACHE_DIR": os.fspath(tmp_path / "uv"),
        },
        text=True,
        timeout=60,
    )
    assert completed.returncode == 0, completed.stderr
    wheels = tuple(output.glob("*.whl"))
    assert len(wheels) == 1
    wheel = wheels[0]
    expected_presets = dict(asset_files("presets"))
    expected_skills = dict(asset_files("skills"))
    expected_paths = expected_presets.keys() | expected_skills.keys()
    assert all("__pycache__" not in PurePosixPath(path).parts for path in expected_paths)
    assert all(PurePosixPath(path).suffix not in {".pyc", ".pyo"} for path in expected_paths)
    with zipfile.ZipFile(wheel) as archive:
        names = set(archive.namelist())
        assert {"fkf/assets_data/presets/" + relative for relative in expected_presets} <= names
        assert {"fkf/assets_data/skills/" + relative for relative in expected_skills} <= names
        for relative, content in expected_presets.items():
            assert archive.read("fkf/assets_data/presets/" + relative) == content
        for relative, content in expected_skills.items():
            assert archive.read("fkf/assets_data/skills/" + relative) == content

    program = """
import hashlib
import json
import sys
sys.path.insert(0, sys.argv[1])
from fkf.assets import asset_files, read_asset, shipped_helpers, skill_digest
payload = {
    "preset": hashlib.sha256(read_asset("presets/personal.yaml")).hexdigest(),
    "helpers": sorted(shipped_helpers()),
    "skills": sorted(name for name, _ in asset_files("skills")),
    "skill_digest": skill_digest("fkf-use"),
}
print(json.dumps(payload, sort_keys=True))
"""
    isolated = subprocess.run(  # noqa: S603 - the interpreter and wheel are test-owned.
        [sys.executable, "-I", "-c", program, os.fspath(wheel)],
        check=False,
        capture_output=True,
        cwd=tmp_path,
        env={"HOME": os.fspath(tmp_path / "home"), "PATH": "/usr/bin:/bin"},
        text=True,
        timeout=30,
    )
    assert isolated.returncode == 0, isolated.stderr
    payload = json.loads(isolated.stdout)
    assert payload["preset"] == hashlib.sha256(read_asset("presets/personal.yaml")).hexdigest()
    assert payload["helpers"] == sorted(shipped_helpers())
    assert payload["skills"] == sorted(name for name, _ in asset_files("skills"))
    assert payload["skill_digest"] == skill_digest("fkf-use")
