from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.base import Base
from fkf.config import ConfigError, load_config
from fkf.config_view import public_config
from fkf.documents import day_window
from fkf.errors import UntrustedError
from fkf.init import InitRequest, init_base
from fkf.jsoncodec import dumps
from fkf.output import text_bytes
from fkf.process import command_path
from fkf.source_runtime import Environment, build_body_command, build_run_command
from fkf.store import NotAddressableError, UnsafePathError
from fkf.sync import SyncRequest, sync
from fkf.trust import TrustItemKind, capture_trust, config_digest, read_trust, write_trust
from fkf.trust_service import trust
from tests.core.test_config import write_base
from tests.services.test_sync import FakeRunner

APP = "clients:\n  example-app: {url: https://example-app.example, script: example-app.py}\n"
SCRIPT = '# /// script\n# dependencies = []\n# ///\nprint("[]")\n'
SOURCE = """sources:
  example-app-records:
    enabled: true
    layer: index
    requires: [uv]
    run: [uv, run, --script, "{{base}}/clients/example-app.py", records]
    body: [uv, run, --script, "{{base}}/clients/example-app.py", body, "{{id}}"]
    fields: {id: .id, title: .title}
"""
NOW = datetime(2026, 9, 8, tzinfo=UTC)


def app_base(tmp_path: Path) -> Base:
    root = write_base(tmp_path, APP + SOURCE)
    (root / "clients").mkdir()
    (root / "clients" / "example-app.py").write_text(SCRIPT)
    config = load_config(root)
    return Base(config, config.store(), now=lambda: NOW)


def test_client_config_and_trust_disclose_app_script_and_explicit_commands(tmp_path: Path) -> None:
    base = app_base(tmp_path)
    assert b'"clients":{"example-app":{"url":"https://example-app.example","script":"example-app.py"}}' in dumps(
        public_config(base.config)
    )
    report = trust(base, record=False, all_items=True)
    rendered, native = text_bytes(report)
    assert native
    assert b"client example-app: https://example-app.example" in rendered
    assert b"script: clients/example-app.py (uv run --script)" in rendered
    assert any(item.kind is TrustItemKind.CLIENT and item.name == "example-app.py" for item in report.state.items)
    assert not report.state.trusted
    assert base.store.clients_dir == base.root / "clients"
    with pytest.raises(NotAddressableError):
        base.store.resolve("clients/example-app.py")
    assert str(base.root / "clients") not in command_path(base=base.root, inherited="/usr/bin").split(":")


@pytest.mark.parametrize(
    "declaration",
    [
        "example-app: {url: http://app.example, script: app.py}",
        "example-app: {url: https://user:pass@app.example, script: app.py}",
        "example-app: {url: 'https://app.example?token=secret', script: app.py}",
        "example-app: {url: 'https://app.example#fragment', script: app.py}",
        "example-app: {url: 'https://app.example:bad', script: app.py}",
        "example-app: {url: 'https://[', script: app.py}",
        "example-app: {url: 'https://app. example', script: app.py}",
        "ExampleApp: {url: https://app.example, script: app.py}",
        "example-app: {url: https://app.example, script: ../app.py}",
        "example-app: {url: https://app.example, script: /tmp/app.py}",
        "example-app: {url: https://app.example, script: app.sh}",
        "example-app: {url: https://app.example, script: [app.py, extra.py]}",
        "example-app: {url: https://app.example}",
        "example-app: {url: https://app.example, script: app.py, token: secret}",
        "example-app: {url: https://app.example, script: app.py}\n  other: {url: https://other.example, script: app.py}",
    ],
)
def test_client_declarations_reject_ambiguous_or_unsafe_inputs(tmp_path: Path, declaration: str) -> None:
    root = write_base(tmp_path, "clients:\n  " + declaration + "\n")
    with pytest.raises(ConfigError, match=r"clients|field token"):
        load_config(root)


def test_clients_cannot_be_replaced_by_machine_local_overlay(tmp_path: Path) -> None:
    root = write_base(tmp_path, APP, APP)
    with pytest.raises(ConfigError, match=r"clients|field token"):
        load_config(root)


def test_app_url_script_and_sidecar_edits_rearm_trust(tmp_path: Path) -> None:
    base = app_base(tmp_path)
    write_trust(base.config, NOW)
    original = config_digest(base.config)
    script = base.root / "clients" / "example-app.py"
    script.write_text(SCRIPT + "# reviewed change\n")
    assert not read_trust(base.config).trusted
    script.write_text(SCRIPT)
    assert config_digest(base.config) == original
    script.chmod(0o700)
    assert not read_trust(base.config).trusted
    write_trust(base.config, NOW)
    (script.parent / "example-app.py.lock").write_text("version = 1\n")
    assert not read_trust(base.config).trusted
    write_trust(base.config, NOW)
    path = base.root / "fkf.yaml"
    path.write_text(path.read_text().replace("example-app.example", "changed.example"))
    assert not read_trust(load_config(base.root)).trusted
    path.write_text(path.read_text().replace("script: example-app.py", "script: replacement.py"))
    (script.parent / "replacement.py").write_text(SCRIPT)
    assert not read_trust(load_config(base.root)).trusted


@pytest.mark.parametrize("location", ["missing", "root", "file", "nested", "directory"])
def test_declared_clients_require_real_files_and_reject_all_symlinks(tmp_path: Path, location: str) -> None:
    base = app_base(tmp_path)
    script = base.root / "clients" / "example-app.py"
    outside = tmp_path / "outside.py"
    outside.write_text(SCRIPT)
    if location == "missing":
        script.unlink()
    elif location == "file":
        script.unlink()
        script.symlink_to(outside)
    elif location == "root":
        script.unlink()
        script.parent.rmdir()
        script.parent.symlink_to(tmp_path, target_is_directory=True)
    elif location == "nested":
        (script.parent / "support").symlink_to(tmp_path, target_is_directory=True)
    else:
        script.unlink()
        script.mkdir()
    with pytest.raises(UnsafePathError):
        capture_trust(base.config)


def test_client_command_uses_one_literal_script_path_and_opaque_body_value(tmp_path: Path) -> None:
    base = app_base(tmp_path)
    environment = Environment.from_config(base.config, inherited_path="/usr/bin")
    source = base.config.sources["example-app-records"]
    command = build_run_command(source, environment, day_window(NOW), base.config.sync.timeout)
    assert command.argv == ("uv", "run", "--script", str(base.root / "clients" / "example-app.py"), "records")
    body = build_body_command(source, source.fields, environment, {"id": "record;literal"}, base.config.sync.timeout)
    assert body.argv == (*command.argv[:-1], "body", "record;literal")
    assert command.before_exec is not None
    with pytest.raises(UntrustedError):
        command.before_exec()
    write_trust(base.config, NOW)
    command.before_exec()
    (base.root / "clients" / "example-app.py").write_text(SCRIPT + "# changed before execution\n")
    with pytest.raises(UntrustedError):
        command.before_exec()


def test_init_never_automatically_trusts_preexisting_client_code(tmp_path: Path) -> None:
    root = tmp_path / "new-base"
    (root / "clients").mkdir(parents=True)
    (root / "clients" / "app.py").write_text(SCRIPT)
    init_base(InitRequest(path=root, skip_git=True), now=lambda: NOW)
    assert not read_trust(load_config(root)).trusted


def test_client_collection_uses_runner_and_keeps_stored_reads_offline(tmp_path: Path) -> None:
    base = app_base(tmp_path)
    runner = FakeRunner(lambda _command: b'[{"id":"rec-1","title":"ExampleApp item"}]')
    base.runner = runner
    # Requirement discovery uses a fake executable; all commands run through the Runner seam.
    tool_dir = tmp_path / "tools"
    tool_dir.mkdir()
    fake_uv = tool_dir / "uv"
    fake_uv.write_text("#!/bin/sh\nexit 97\n")
    fake_uv.chmod(0o700)
    base.environment = Environment(base.root, bin=(str(tool_dir),), config=base.config)
    write_trust(base.config, NOW)
    result = sync(base, SyncRequest(targets=("example-app-records",), no_graph=True))
    assert result.written == 1
    assert result.complete
    assert len(runner.commands) == 1
    assert runner.commands[0].argv == (
        "uv",
        "run",
        "--script",
        str(base.root / "clients" / "example-app.py"),
        "records",
    )
    assert base.read_document("index/example-app-records.json").records == [{"id": "rec-1", "title": "ExampleApp item"}]
    assert len(runner.commands) == 1
    before = (base.root / "index" / "example-app-records.json").read_bytes()
    base.runner = FakeRunner(lambda _command: b'[{"id":"rec-2"}]')
    failed = sync(base, SyncRequest(targets=("example-app-records",), force=True, no_graph=True))
    assert not failed.complete
    assert (base.root / "index" / "example-app-records.json").read_bytes() == before
