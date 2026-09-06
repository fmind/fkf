from __future__ import annotations

import io
import json
from pathlib import Path

from fkf.cli import app
from fkf.cli_support import run_app
from fkf.config import load_config
from fkf.config_view import public_config
from fkf.jsoncodec import dumps
from fkf.schema import encode_config_schema


def make_base(tmp_path: Path) -> Path:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(
        """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  title: {description: Subject., cardinality: optional, weight: 7}
layers: {events: true, index: true, tasks: true, projects: true, wiki: true}
identities:
  owner:
    canonical: person:email/owner@example.com
    aliases: [owner]
    owner: true
sources:
  example-records:
    layer: index
    run: [example, list]
    fields: {id: .id, title: [.title, .subject]}
    retry: {attempts: 2, backoff: 1s, on: ["exit:7"]}
sync: {days: 4, index_max_age_hours: 12, timeout: 3s, concurrency: 2}
""",
        encoding="utf-8",
    )
    return root


def test_public_config_keeps_compact_paths_and_omits_runtime_internals(tmp_path: Path) -> None:
    root = make_base(tmp_path)
    encoded = dumps(public_config(load_config(root)), indent=True, newline=True)
    result = json.loads(encoded)

    assert list(result) == [
        "fkf",
        "name",
        "schema",
        "layers",
        "identities",
        "sources",
        "sync",
        "path",
    ]
    assert result["schema"]["id"] == {"description": "Stable identity.", "cardinality": "one"}
    assert result["sources"]["example-records"]["fields"] == {
        "id": ".id",
        "title": [".title", ".subject"],
    }
    assert result["sources"]["example-records"]["retry"] == {
        "attempts": 2,
        "backoff": 1_000_000_000,
        "on": ["exit:7"],
    }
    assert "schema" not in result["sources"]["example-records"]
    assert "_steps" not in encoded.decode()
    assert result["path"] == str(root / "fkf.yaml")


def test_config_parent_and_schema_cli_preserve_public_bytes(tmp_path: Path) -> None:
    root = make_base(tmp_path)
    stdout, stderr = io.StringIO(), io.StringIO()
    assert (
        run_app(
            app,
            ["--base", str(root), "--format", "json", "config"],
            stdout=stdout,
            stderr=stderr,
        )
        == 0
    )
    assert json.loads(stdout.getvalue())["name"] == "brain"
    assert stderr.getvalue() == ""

    stdout, stderr = io.StringIO(), io.StringIO()
    assert (
        run_app(
            app,
            ["--base", str(root), "--format", "text", "config"],
            stdout=stdout,
            stderr=stderr,
        )
        == 0
    )
    assert stdout.getvalue().splitlines()[:4] == [
        f"brain  {root / 'fkf.yaml'}",
        "layers:  events index projects tasks wiki",
        "sync:    4 day(s), timeout 3s, concurrency 2, index stale after 12h",
        "",
    ]
    assert "example-records          off  index    " in stdout.getvalue()
    assert stderr.getvalue() == ""

    stdout, stderr = io.StringIO(), io.StringIO()
    assert run_app(app, ["config", "schema", "--format", "text"], stdout=stdout, stderr=stderr) == 0
    assert stdout.getvalue().encode() == encode_config_schema()
    assert stderr.getvalue() == ""
