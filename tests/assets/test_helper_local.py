"""Local-store and declared-metadata helpers remain bounded and non-executing."""

from __future__ import annotations

import json
import os
import runpy
import sqlite3
import time
from pathlib import Path
from typing import cast

import pytest

from .conftest import HelperInstallation


def _install_delayed_oversized_provider(helpers: HelperInstallation, name: str) -> None:
    target = helpers.bin / name
    target.unlink(missing_ok=True)
    target.write_text(
        "#!/usr/bin/env python3\n"
        "import os,pathlib,sys,time\n"
        "sys.stdout.write('123456789'); sys.stdout.flush()\n"
        "sys.stderr.write('sensitive-provider-diagnostic'); sys.stderr.flush()\n"
        "time.sleep(0.6)\n"
        "pathlib.Path(os.environ['PROVIDER_ESCAPE_MARKER']).write_text('escaped')\n",
        encoding="utf-8",
    )
    target.chmod(0o700)


@pytest.mark.parametrize(
    ("harness", "stdout"),
    [("claude", ""), ("codex", "{}\n"), ("gemini", "{}\n"), ("kiro", "")],
)
def test_session_hook_does_not_read_terminal_stdin_or_execute_a_child(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    harness: str,
    stdout: str,
) -> None:
    class TerminalBuffer:
        @staticmethod
        def isatty() -> bool:
            return True

        @staticmethod
        def read(_size: int) -> bytes:
            raise AssertionError("terminal stdin must not be read")

    class TerminalStdin:
        buffer = TerminalBuffer()

    workspace = helpers.root / "workspace"
    workspace.mkdir()
    executable = helpers.root / "fkf"
    executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    executable.chmod(0o700)
    namespace = runpy.run_path(os.fspath(helpers.bin / "fkf-hook.py"))
    main = namespace["main"]
    monkeypatch.setattr(main.__globals__["sys"], "stdin", TerminalStdin())

    def unexpected_invoke(_arguments: list[str], _environment: dict[str, str]) -> str:
        raise AssertionError("terminal invocation must not execute a child")

    monkeypatch.setitem(main.__globals__, "invoke", unexpected_invoke)

    assert main([harness, os.fspath(executable), os.fspath(workspace)]) == 0
    assert capsys.readouterr() == (stdout, "")


def test_session_hook_terminates_an_oversized_child_process_group(helpers: HelperInstallation) -> None:
    workspace = helpers.root / "workspace"
    workspace.mkdir()
    executable = helpers.root / "fkf"
    executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    executable.chmod(0o700)

    marker = helpers.root / "hook-provider-escaped"
    git = helpers.home / ".local" / "bin" / "git"
    git.parent.mkdir(parents=True)
    git.write_text(
        "#!/usr/bin/env python3\n"
        "import os,pathlib,subprocess,sys,time\n"
        "child = \"import os,pathlib,time; time.sleep(0.6); pathlib.Path(os.environ['HOOK_ESCAPE_MARKER']).write_text('escaped')\"\n"
        "subprocess.Popen([sys.executable, '-c', child])\n"
        "sys.stdout.buffer.write(b'x' * ((1 << 20) + 1)); sys.stdout.buffer.flush()\n"
        "sys.stderr.write('sensitive-provider-diagnostic'); sys.stderr.flush()\n"
        "time.sleep(1.2)\n",
        encoding="utf-8",
    )
    git.chmod(0o700)

    result = helpers.run(
        "fkf-hook.py",
        "codex",
        os.fspath(executable),
        os.fspath(workspace),
        stdin=json.dumps({"cwd": os.fspath(workspace)}),
        environment={"HOOK_ESCAPE_MARKER": os.fspath(marker)},
        timeout=5,
    )
    time.sleep(0.8)

    assert result.returncode == 0
    assert result.stdout == b"{}\n"
    assert b"sensitive-provider-diagnostic" not in result.stderr
    assert not marker.exists()


def test_session_hook_accepts_the_exact_child_output_limit(helpers: HelperInstallation) -> None:
    module = runpy.run_path(os.fspath(helpers.bin / "fkf-hook.py"))
    invoke = module["invoke"]
    invoke.__globals__["MAX_INVOKE_BYTES"] = 8
    helpers.fake("git", "printf 12345678\n")

    assert invoke(["git", "config", "--get", "remote.origin.url"], helpers.environment()) == "12345678"


def test_session_hook_times_out_and_terminates_a_silent_child_group(helpers: HelperInstallation) -> None:
    marker = helpers.root / "timed-out-hook-provider-escaped"
    provider = helpers.fake(
        "git",
        "python3 -c 'import os,pathlib,time; time.sleep(0.4); "
        'pathlib.Path(os.environ["HOOK_ESCAPE_MARKER"]).write_text("escaped")\' &\n'
        "sleep 2\n",
    )
    module = runpy.run_path(os.fspath(helpers.bin / "fkf-hook.py"))
    invoke = module["invoke"]
    invoke.__globals__["INVOKE_TIMEOUT_SECONDS"] = 0.1
    environment = helpers.environment({"HOOK_ESCAPE_MARKER": os.fspath(marker)})

    with pytest.raises(TimeoutError, match="hook child timed out"):
        invoke(["git", "config", "--get", "remote.origin.url"], environment)
    time.sleep(0.5)

    assert provider.is_file()
    assert not marker.exists()


def _atuin_database(path: Path, rows: list[tuple[object, ...]]) -> None:
    connection = sqlite3.connect(path)
    try:
        connection.execute(
            "create table history ("
            "id text, cwd text, exit integer, duration integer, command text, "
            "timestamp integer, deleted_at integer)"
        )
        connection.executemany("insert into history values (?, ?, ?, ?, ?, ?, ?)", rows)
        connection.commit()
    finally:
        connection.close()


def test_atuin_history_streams_rows_and_rejects_record_overflow_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    database = helpers.home / "history.db"
    timestamp = 1_777_852_800_000_000_000
    _atuin_database(
        database,
        [
            ("one", "/work/one", 0, 10, "git status", timestamp, None),
            ("two", "/work/two", 0, 20, "uv run", timestamp + 1, None),
        ],
    )
    namespace = runpy.run_path(os.fspath(helpers.bin / "atuin-history-json.py"))
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "MAX_RECORDS", 1)

    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", os.fspath(database)]) == 1
    captured = capsys.readouterr()

    assert captured.out == ""
    assert "record count exceeds 1" in captured.err
    assert ".fetchall()" not in (helpers.bin / "atuin-history-json.py").read_text(encoding="utf-8")


def test_atuin_history_output_limit_includes_the_final_newline(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    database = helpers.home / "history.db"
    timestamp = 1_777_852_800_000_000_000
    _atuin_database(database, [("one", "/work/one", 0, 10, "git status", timestamp, None)])
    namespace = runpy.run_path(os.fspath(helpers.bin / "atuin-history-json.py"))
    main = namespace["main"]
    expected = (
        '[{"id":"one","cwd":"/work/one","exit":0,"duration":10,'
        '"time":"2026-05-04T00:00:00Z","subject":"git status","tool":"git","action":"status"}]\n'
    )
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()))

    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", os.fspath(database)]) == 0
    assert capsys.readouterr() == (expected, "")

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", len(expected.encode()) - 1)
    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", os.fspath(database)]) == 1
    captured = capsys.readouterr()
    assert captured.out == ""
    assert "output exceeds" in captured.err


@pytest.mark.parametrize(("command", "expected_code"), [("x" * 8, 0), ("x" * 9, 1)])
def test_atuin_history_bounds_each_projected_text_value(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    command: str,
    expected_code: int,
) -> None:
    database = helpers.home / "history.db"
    timestamp = 1_777_852_800_000_000_000
    _atuin_database(database, [("one", "/work", 0, 10, command, timestamp, None)])
    namespace = runpy.run_path(os.fspath(helpers.bin / "atuin-history-json.py"))
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "MAX_ROW_TEXT_BYTES", 8)

    assert main(["2026-05-04T00:00:00Z", "2026-05-05T00:00:00Z", os.fspath(database)]) == expected_code
    captured = capsys.readouterr()
    if expected_code == 0:
        assert captured.out.endswith("]\n")
        assert captured.err == ""
    else:
        assert captured.out == ""
        assert "command exceeds 8 bytes" in captured.err


def test_writing_index_projects_front_matter_and_plain_drafts(tmp_path, helpers: HelperInstallation) -> None:
    article = tmp_path / "article.md"
    article.write_text(
        '+++\ntitle = "Reviewed title"\ntags = ["python", "agents"]\n+++\nBody.\n',
        encoding="utf-8",
    )
    draft = tmp_path / "plain-note.txt"
    draft.write_text("Unpublished body.\n", encoding="utf-8")

    result = helpers.run("writing-index.py", os.fspath(article), os.fspath(draft))

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert {record["id"] for record in records} == {tmp_path.name, "plain-note"}
    reviewed = next(record for record in records if record["title"] == "Reviewed title")
    assert reviewed["tags"] == ["python", "agents"]
    plain = next(record for record in records if record["id"] == "plain-note")
    assert plain["title"] == "plain-note"
    assert plain["chars"] == len("Unpublished body.\n")


@pytest.mark.parametrize(
    "remote",
    [
        "https://github.com/acme/project.git",
        "ssh://git@github.com/acme/project.git",
        "git@github.com:acme/project.git",
    ],
)
def test_writing_index_accepts_closed_github_remote_forms(
    tmp_path,
    helpers: HelperInstallation,
    remote: str,
) -> None:
    article = tmp_path / "note.md"
    article.write_text("Reviewable note.\n", encoding="utf-8")
    helpers.fake("git", "printf '%s\\n' \"$WRITING_REMOTE\"\n")

    result = helpers.run("writing-index.py", os.fspath(article), environment={"WRITING_REMOTE": remote})

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert records[0]["repo"] == "acme/project"


@pytest.mark.parametrize(
    "remote",
    [
        "https://user:sensitive-marker@github.com/acme/project.git",
        "https://github.com/acme/project.git?token=sensitive-marker",
        "https://github.com/acme/project.git#sensitive-marker",
        "ssh://git:sensitive-marker@github.com/acme/project.git",
        "ssh://sensitive-marker@github.com/acme/project.git",
        "ssh://github.com/acme/project.git",
        "sensitive-marker@github.com:acme/project.git",
        "https://github.com/acme/project.git\nsensitive-marker",
        "https://github.com/acme/project/sensitive-marker",
        "https://sensitive-marker.example/acme/project.git",
    ],
)
def test_writing_index_rejects_unsafe_remotes_without_leaking_them(
    tmp_path,
    helpers: HelperInstallation,
    remote: str,
) -> None:
    article = tmp_path / "note.md"
    article.write_text("Reviewable note.\n", encoding="utf-8")
    helpers.fake("git", "printf '%s\\n' \"$WRITING_REMOTE\"\n")

    result = helpers.run("writing-index.py", os.fspath(article), environment={"WRITING_REMOTE": remote})

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert "repo" not in records[0]
    assert b"sensitive-marker" not in result.stdout
    assert b"sensitive-marker" not in result.stderr


def test_writing_index_reads_only_a_bounded_prefix_of_a_sparse_plain_body(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    article = helpers.home / "large-note.txt"
    expected = 32 << 20
    with article.open("wb") as stream:
        stream.write(b"Plain note.\n")
        stream.seek(expected - 1)
        stream.write(b"x")
    namespace = runpy.run_path(os.fspath(helpers.bin / "writing-index.py"))
    project = namespace["project"]
    monkeypatch.setitem(project.__globals__, "remote_repository", lambda _path: None)

    def reject_whole_file_read(_path: Path) -> bytes:
        raise AssertionError("writing-index must not read the complete declared body")

    monkeypatch.setattr(Path, "read_bytes", reject_whole_file_read)

    record = project(article, helpers.home)

    assert record["chars"] == expected


@pytest.mark.parametrize(
    ("body", "expected_chars"),
    [
        (b"Body.\r\n", len(b"Body.\r\n")),
        (b"Body.\r\nLast", len(b"Body.\r\nLast") + 1),
        (b"Body.\nLast", len(b"Body.\nLast") + 1),
    ],
)
def test_writing_index_preserves_legacy_body_char_count_for_line_endings(
    helpers: HelperInstallation,
    body: bytes,
    expected_chars: int,
) -> None:
    article = helpers.home / "note.md"
    article.write_bytes(b'+++\r\ntitle = "Note"\r\n+++\r\n' + body)
    helpers.fake("git", "exit 1\n")

    result = helpers.run("writing-index.py", os.fspath(article))

    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert records[0]["chars"] == expected_chars


@pytest.mark.parametrize(
    "content",
    [
        pytest.param(b'+++\ntitle = "Unclosed"\n', id="unclosed-at-eof"),
        pytest.param(b"+++\n" + b"#" * (1 << 20) + b"\n+++\nBody.\n", id="closing-after-prefix"),
    ],
)
def test_writing_index_rejects_front_matter_without_a_closing_delimiter_in_the_prefix(
    helpers: HelperInstallation,
    content: bytes,
) -> None:
    article = helpers.home / "note.md"
    article.write_bytes(content)

    result = helpers.run("writing-index.py", os.fspath(article))

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"no closing +++ front-matter delimiter within 1 MiB" in result.stderr


def test_writing_index_rejects_symlinked_declared_files(helpers: HelperInstallation) -> None:
    target = helpers.home / "target.md"
    target.write_text("Private body.\n", encoding="utf-8")
    article = helpers.home / "linked.md"
    article.symlink_to(target)

    result = helpers.run("writing-index.py", os.fspath(article))

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"not a regular file" in result.stderr


def test_writing_index_rejects_declared_path_replacement_after_open(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    article = helpers.home / "note.md"
    article.write_text("Original.\n", encoding="utf-8")
    replacement = helpers.home / "replacement.md"
    replacement.write_text("Replacement.\n", encoding="utf-8")
    opened_path = helpers.home / "opened.md"
    namespace = runpy.run_path(os.fspath(helpers.bin / "writing-index.py"))
    read_document = namespace["read_document"]
    document_error = cast(type[Exception], namespace["DocumentIndexError"])
    original_prefix = namespace["front_matter_prefix"]

    def replace_after_open(descriptor: int, size: int):
        article.rename(opened_path)
        replacement.rename(article)
        return original_prefix(descriptor, size)

    monkeypatch.setitem(read_document.__globals__, "front_matter_prefix", replace_after_open)

    with pytest.raises(document_error, match="changed while it was being read"):
        read_document(article)


@pytest.mark.parametrize(
    ("provider", "content", "limit_name"),
    [
        pytest.param(
            "yq",
            b"+++\n" + b"#" * (128 << 10) + b"\n+++\nBody.\n",
            "MAX_YQ_BYTES",
            id="yq",
        ),
        pytest.param("git", b"Plain body.\n", "MAX_GIT_BYTES", id="git"),
    ],
)
def test_writing_index_terminates_oversized_subprocesses_without_partial_output(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
    provider: str,
    content: bytes,
    limit_name: str,
) -> None:
    article = helpers.home / "note.md"
    article.write_bytes(content)
    escaped = helpers.root / f"{provider}-escaped"
    _install_delayed_oversized_provider(helpers, provider)
    namespace = runpy.run_path(os.fspath(helpers.bin / "writing-index.py"))
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, limit_name, 8)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])
    monkeypatch.setenv("PROVIDER_ESCAPE_MARKER", os.fspath(escaped))

    assert main([os.fspath(article)]) == 1
    captured = capfd.readouterr()

    assert captured.out == ""
    assert "output exceeds" in captured.err
    assert "sensitive-provider-diagnostic" not in captured.err
    time.sleep(0.7)
    assert not escaped.exists()


def test_writing_index_rejects_aggregate_output_before_stdout(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    article = helpers.home / "note.md"
    article.write_text("Plain body.\n", encoding="utf-8")
    namespace = runpy.run_path(os.fspath(helpers.bin / "writing-index.py"))
    main = namespace["main"]
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", 8)
    monkeypatch.setitem(main.__globals__, "remote_repository", lambda _path: None)
    monkeypatch.setenv("HOME", os.fspath(helpers.home))

    assert main([os.fspath(article)]) == 1
    captured = capsys.readouterr()

    assert captured.out == ""
    assert "output exceeds 64 MiB" in captured.err


def test_chrome_bookmarks_namespaces_identical_profile_local_ids(helpers: HelperInstallation) -> None:
    fixtures = {
        ".config/chromium/Default/Bookmarks": {
            "roots": {
                "bookmark_bar": {
                    "type": "folder",
                    "name": "Bookmarks",
                    "children": [
                        {
                            "type": "url",
                            "guid": "shared",
                            "name": "One",
                            "url": "https://one.example.test/path",
                            "date_added": "13300000000000000",
                        }
                    ],
                }
            }
        },
        ".config/google-chrome/Profile 1/Bookmarks": {
            "roots": {
                "bookmark_bar": {
                    "type": "folder",
                    "name": "Bookmarks",
                    "children": [
                        {
                            "type": "url",
                            "guid": "shared",
                            "name": "Two",
                            "url": "https://two.example.test/path",
                            "date_added": "13300000000000000",
                        }
                    ],
                }
            }
        },
    }
    for relative, content in fixtures.items():
        target = helpers.home / relative
        target.parent.mkdir(parents=True)
        target.write_text(json.dumps(content), encoding="utf-8")
    result = helpers.run("chrome-bookmarks.py")
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert len(records) == 2
    assert records[0]["uid"] != records[1]["uid"]


def test_agent_prompts_reads_only_the_exact_window_from_durable_store(helpers: HelperInstallation) -> None:
    transcript = helpers.home / ".agents" / "sessions" / "v1" / "agy" / "lineage" / "session" / "transcript.jsonl"
    transcript.parent.mkdir(parents=True)
    transcript.write_text(
        "\n".join(
            (
                '{"ts":"2026-08-26T23:59:59Z","role":"user","sid":"before","content":"Before.","cwd":"/tmp/work","model":"test"}',
                '{"ts":"2026-08-27T00:00:00Z","role":"user","sid":"start","content":"At start.","cwd":"/tmp/work","model":"test"}',
                '{"ts":"2026-08-27T12:00:00Z","role":"user","sid":"inside","content":"Inside.","cwd":"/tmp/work","model":"test"}',
                '{"ts":"2026-08-28T00:00:00Z","role":"user","sid":"end","content":"At end.","cwd":"/tmp/work","model":"test"}',
            )
        )
        + "\n",
        encoding="utf-8",
    )
    result = helpers.run(
        "agent-prompts.py",
        "2026-08-27T00:00:00Z",
        "2026-08-28T00:00:00Z",
        "0",
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert [(record["agent"], record["sid"]) for record in records] == [
        ("antigravity", "start"),
        ("antigravity", "inside"),
    ]


def test_repository_facts_projects_declarations_without_executing_repository(helpers: HelperInstallation) -> None:
    root = helpers.root / "repositories"
    repository = root / "project space"
    (repository / ".git").mkdir(parents=True)
    (repository / ".git" / "config").write_text(
        '[remote "origin"]\n\turl = https://token:secret@github.com/acme/project.git?access_token=hidden#fragment\n',
        encoding="utf-8",
    )
    (repository / "go.mod").write_text("module example.test/project\n\ngo 1.27\n", encoding="utf-8")
    (repository / "mise.toml").write_text(
        '[tasks.build]\nrun = "go build ./..."\n[tasks.test]\nrun = "${DANGEROUS_TEST}"\n',
        encoding="utf-8",
    )
    (repository / "package.json").write_text('{"scripts":{"test":"touch sentinel"}}', encoding="utf-8")
    (repository / "AGENTS.md").write_text("# Synthetic instructions\n", encoding="utf-8")
    (repository / "hostile$(touch sentinel)").write_text("data", encoding="utf-8")

    result = helpers.run("repository-facts.py", os.fspath(root))
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert len(records) == 1
    record = records[0]
    assert record["id"] == "repo:github.com/acme/project"
    assert record["title"] == "github.com/acme/project"
    assert record["root_ref"] == "root-1/project%20space"
    assert record["remotes"] == ["https://github.com/acme/project"]
    assert record["instructions"] == ["AGENTS.md"]
    assert record["languages"] == ["Go", "TypeScript/JavaScript"]
    assert len(cast(list[object], record["declared_tasks"])) == 3
    assert not (repository / "sentinel").exists()
    for secret in (b"token", b"secret", b"access_token"):
        assert secret not in result.stdout


@pytest.mark.parametrize("defect", ["malformed", "oversized", "symlink"])
def test_repository_facts_rejects_unsafe_metadata_without_partial_output(
    helpers: HelperInstallation,
    defect: str,
) -> None:
    root = helpers.root / "repositories"
    repository = root / "project"
    (repository / ".git").mkdir(parents=True)
    if defect == "malformed":
        (repository / "pyproject.toml").write_text("[", encoding="utf-8")
    elif defect == "oversized":
        (repository / "package.json").write_bytes(b"x" * ((256 << 10) + 1))
    else:
        outside = helpers.root / "outside.md"
        outside.write_text("outside", encoding="utf-8")
        (repository / "AGENTS.md").symlink_to(outside)
    result = helpers.run("repository-facts.py", os.fspath(root))
    assert result.returncode != 0
    assert result.stdout == b""


@pytest.mark.parametrize(
    ("provider_exit", "account", "succeeds"),
    [(0, "owner@example.test", True), (0, "", False), (7, "owner@example.test", False)],
)
def test_gcloud_auth_requires_a_nonempty_active_account(
    helpers: HelperInstallation,
    provider_exit: int,
    account: str,
    succeeds: bool,
) -> None:
    helpers.fake("gcloud", 'printf "%s\\n" "$GCLOUD_ACCOUNT"\nexit "$GCLOUD_EXIT"\n')
    result = helpers.run(
        "gcloud-auth-ready.sh",
        environment={"GCLOUD_ACCOUNT": account, "GCLOUD_EXIT": str(provider_exit)},
    )
    assert (result.returncode == 0) is succeeds
    assert result.stdout == b""


def test_meeting_notes_join_only_to_durable_calendar_records(helpers: HelperInstallation) -> None:
    helpers.fake(
        "gws",
        """case "$*" in
  *"files list"*)
    printf '%s\n' '{"files":[{"id":"doc-1","name":"Attached Review - Notes by Gemini","createdTime":"2026-05-04T09:00:00Z","modifiedTime":"2026-05-04T09:30:00Z","webViewLink":"https://docs.google.com/document/d/doc-1/edit","owners":[{"emailAddress":"owner@example.test","me":true}]},{"id":"doc-2","name":"Owner Sync - Fmind Notes","createdTime":"2026-05-04T11:00:00Z","modifiedTime":"2026-05-04T11:30:00Z","webViewLink":"https://docs.google.com/document/d/doc-2/edit","owners":[{"emailAddress":"owner@example.test","me":true}]}]}' ;;
  *"calendarList list"*)
    printf '%s\n' '{"items":[{"id":"owner@example.test","summary":"Primary","primary":true}]}' ;;
  *"events list"*)
    printf '%s\n' '{"items":[{"id":"event-1","summary":"Attached Review","start":{"dateTime":"2026-05-04T09:00:00Z"},"end":{"dateTime":"2026-05-04T10:00:00Z"},"attachments":[{"fileId":"doc-1","fileUrl":"https://docs.google.com/document/d/doc-1/edit","title":"Attached Review - Notes by Gemini"}]},{"id":"event-2","summary":"Owner Sync","start":{"dateTime":"2026-05-04T11:00:00Z"},"end":{"dateTime":"2026-05-04T12:00:00Z"}}]}' ;;
  *) exit 2 ;;
esac
""",
    )
    base = helpers.root / "base"
    base.mkdir()
    arguments = (
        "2026-05-01T00:00:00Z",
        "2026-05-06T00:00:00Z",
        "2026-05-01",
        "2026-05-06",
        "Fmind",
        os.fspath(base),
    )
    missing = helpers.run("gws-meeting-notes-json.py", *arguments)
    assert missing.returncode != 0
    assert missing.stdout == b""
    assert b"enable and sync google-calendar-events" in missing.stderr

    calendar = base / "events" / "2026-05-04" / "google-calendar-events.json"
    calendar.parent.mkdir(parents=True)
    calendar.write_text(
        '{"fkf":1,"source":"google-calendar-events","fields":{"id":".uid"},'
        '"records":[{"uid":"owner@example.test~event-1"},{"uid":"owner@example.test~event-2"}]}\n',
        encoding="utf-8",
    )
    result = helpers.run("gws-meeting-notes-json.py", *arguments)
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    records = cast(list[dict[str, object]], json.loads(result.stdout))
    assert [record["id"] for record in records] == ["doc-1", "doc-2"]
    assert [record["meeting_uris"] for record in records] == [
        ["events/2026-05-04/google-calendar-events.json#owner@example.test%7Eevent-1"],
        ["events/2026-05-04/google-calendar-events.json#owner@example.test%7Eevent-2"],
    ]
    assert all(record["owner_uris"] == ["person:email/owner@example.test"] for record in records)
    assert all(len(cast(list[object], record["attachment_uris"])) == 1 for record in records)
