"""Bounded local-input and output contracts for the bundled RSS helper."""

from __future__ import annotations

import json
import os
import runpy
import subprocess
import threading
from pathlib import Path
from typing import Any

import pytest

from .conftest import HelperInstallation

MAX_OPML_BYTES = 1 << 20
MAX_FEED_BYTES = 8 << 20


def _feed_bytes(size: int | None = None) -> bytes:
    source = (
        b'<rss version="2.0"><channel><title>Example</title><link>https://example.test</link>'
        b"<item><guid>post-1</guid><title>Post</title><link>https://example.test/post</link>"
        b"<pubDate>Mon, 04 May 2026 09:00:00 +0000</pubDate></item></channel></rss>"
    )
    if size is None:
        return source
    assert len(source) <= size
    return source + b" " * (size - len(source))


def _opml_bytes(feed_count: int, size: int | None = None) -> bytes:
    outlines = b"".join(
        f'<outline xmlUrl="https://example.test/feed-{number}.xml"/>'.encode() for number in range(feed_count)
    )
    source = b"<opml><body>" + outlines + b"</body></opml>"
    if size is None:
        return source
    assert len(source) <= size
    return source + b" " * (size - len(source))


def _install_copying_curl(helpers: HelperInstallation) -> None:
    helpers.fake(
        "curl",
        """output=
while [ "$#" -gt 0 ]; do
  case "$1" in --output) output=$2; shift 2 ;; *) shift ;; esac
done
printf 'called\n' >> "$CALL_LOG"
cp "$RSS_FIXTURE" "$output"
""",
    )


def test_rss_xml_preserves_cdata_entities_namespaces_attributes_and_nested_text() -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    feed_type = namespace["Feed"]
    normalize = namespace["normalize"]
    source = b"""<?xml version="1.0"?>
<atom:feed xmlns:atom="https://www.w3.org/2005/Atom">
  <atom:title><![CDATA[Research <!DOCTYPE text> &amp;]]> &amp; &#x1f680;</atom:title>
  <atom:link REL="alternate" HREF="http://example.test/site"/>
  <atom:entry>
    <atom:id>post&#45;1</atom:id>
    <atom:title>Nested <atom:em>XML</atom:em> &amp; entities</atom:title>
    <atom:link REL="alternate" HREF="http://example.test/post-1"/>
    <atom:updated>2026-05-04T09:00:00Z</atom:updated>
  </atom:entry>
</atom:feed>"""

    record, items = normalize(
        feed_type(1, "public", "https://example.test/feed.xml", "https://example.test/feed.xml", ""),
        source,
    )

    assert record["title"] == "Research <!DOCTYPE text> &amp; & 🚀"
    assert record["site_url"] == "https://example.test/site"
    assert len(items) == 1
    assert items[0]["title"] == "Nested XML & entities"
    assert items[0]["url"] == "https://example.test/post-1"


@pytest.mark.parametrize(
    "declaration",
    [
        '<!DOCTYPE rss [<!ENTITY author "Mallory">]>',
        '<!DOCTYPE rss SYSTEM "https://example.test/feed.dtd">',
    ],
    ids=["internal", "external"],
)
@pytest.mark.parametrize("encoding", ["utf-8", "utf-16"])
def test_rss_xml_rejects_internal_and_external_dtds(declaration: str, encoding: str) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    secure_xml = namespace["secure_xml"]
    parse_error = namespace["XMLParseError"]
    document = (
        f'<?xml version="1.0" encoding="{encoding.upper()}"?>'
        f"{declaration}<rss><channel><title>&author;</title></channel></rss>"
    )

    with pytest.raises(parse_error, match=r"DTD.*forbidden"):
        secure_xml(document.encode(encoding))


@pytest.mark.parametrize(
    ("encoding", "codec"),
    [("UTF-8", "utf-8"), ("UTF-16", "utf-16"), ("ISO-8859-1", "iso-8859-1")],
)
def test_rss_xml_honors_encodings_standalone_declarations_and_comments(
    encoding: str,
    codec: str,
) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    secure_xml = namespace["secure_xml"]
    child = namespace["child"]
    element_text = namespace["element_text"]
    document = (
        f'<?xml version="1.0" encoding="{encoding}" standalone="yes"?>'
        "<rss><!-- ignored --><channel><title>Café<!-- ignored --> &#x1f680;</title></channel></rss>"
    )

    root = secure_xml(document.encode(codec))

    assert element_text(child(child(root, "channel"), "title")) == "Café 🚀"


def test_rss_xml_rejects_processing_instructions() -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    secure_xml = namespace["secure_xml"]
    parse_error = namespace["XMLParseError"]

    with pytest.raises(parse_error, match="processing instructions are forbidden"):
        secure_xml(b"<?feed refresh?><rss></rss>")


@pytest.mark.parametrize(
    "source",
    [
        b"<rss><channel></rss>",
        b"<rss></rss><feed></feed>",
        b"<rss><channel><title>&custom;</title></channel></rss>",
        b'<?xml version="1.0" encoding="x-nope"?><rss></rss>',
        b'<?xml version="1.0" encoding="UTF-7"?><rss></rss>',
    ],
    ids=["malformed", "multiple-roots", "undeclared-entity", "unknown-encoding", "unsupported-encoding"],
)
def test_rss_xml_rejects_invalid_documents(source: bytes) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    secure_xml = namespace["secure_xml"]
    parse_error = namespace["XMLParseError"]

    with pytest.raises(parse_error, match="invalid XML"):
        secure_xml(source)


@pytest.mark.parametrize(
    "source",
    [
        b'<?xml version="1.0" encoding="x-nope"?><rss></rss>',
        b'<?xml version="1.0" encoding="UTF-7"?><rss></rss>',
    ],
    ids=["unknown", "unsupported"],
)
def test_rss_reports_invalid_xml_encodings_without_traceback(
    helpers: HelperInstallation,
    source: bytes,
) -> None:
    fixture = helpers.home / "feed.xml"
    fixture.write_bytes(source)
    call_log = helpers.root / "curl-calls"
    _install_copying_curl(helpers)

    result = helpers.run(
        "rss-json.py",
        "https://example.test/feed.xml",
        environment={"CALL_LOG": os.fspath(call_log), "RSS_FIXTURE": os.fspath(fixture)},
    )

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"not XML" in result.stderr
    assert b"Traceback" not in result.stderr


def test_rss_opml_accepts_exact_limit_and_rejects_limit_plus_one_before_curl(
    helpers: HelperInstallation,
) -> None:
    exact = helpers.home / "exact.opml"
    exact.write_bytes(_opml_bytes(1, MAX_OPML_BYTES))
    over = helpers.home / "over.opml"
    over.write_bytes(_opml_bytes(1, MAX_OPML_BYTES + 1))
    fixture = helpers.home / "feed.xml"
    fixture.write_bytes(_feed_bytes())
    call_log = helpers.root / "curl-calls"
    _install_copying_curl(helpers)
    environment = {"CALL_LOG": os.fspath(call_log), "RSS_FIXTURE": os.fspath(fixture)}

    accepted = helpers.run("rss-json.py", os.fspath(exact), environment=environment)

    assert accepted.returncode == 0, accepted.stderr.decode(errors="replace")
    assert json.loads(accepted.stdout)
    assert call_log.read_text(encoding="utf-8").splitlines() == ["called"]
    call_log.unlink()

    rejected = helpers.run("rss-json.py", os.fspath(over), environment=environment)

    assert rejected.returncode == 1
    assert rejected.stdout == b""
    assert b"public OPML exceeds 1 MiB" in rejected.stderr
    assert not call_log.exists()


def test_rss_rejects_symlinked_opml_before_curl(helpers: HelperInstallation) -> None:
    target = helpers.home / "feeds.opml"
    target.write_bytes(_opml_bytes(1))
    linked = helpers.home / "linked.opml"
    linked.symlink_to(target)
    marker = helpers.root / "curl-called"
    helpers.fake("curl", 'touch "$CALL_MARKER"\nexit 7\n')

    result = helpers.run(
        "rss-json.py",
        os.fspath(linked),
        environment={"CALL_MARKER": os.fspath(marker)},
    )

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"public OPML is not a regular non-symlink file" in result.stderr
    assert not marker.exists()


def test_rss_caps_admitted_feed_count() -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    inputs = namespace["inputs"]
    exact = [f"https://example.test/{number}.xml" for number in range(256)]

    assert namespace["MAX_FEEDS"] == 256
    assert len(inputs(exact)) == 256
    with pytest.raises(ValueError, match="at most 256 feeds are allowed"):
        inputs([*exact, "https://example.test/over.xml"])


@pytest.mark.parametrize("mutation", ["replace", "grow"])
def test_rss_rejects_download_replacement_or_growth(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    mutation: str,
) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    read_regular = namespace["read_regular"]
    fingerprint = namespace["file_fingerprint"]
    target = tmp_path / "feed.xml"
    target.write_bytes(_feed_bytes())
    downloaded = fingerprint(target.lstat())
    if mutation == "replace":
        replacement = tmp_path / "replacement.xml"
        replacement.write_bytes(_feed_bytes())
        open_file = namespace["os"].open

        def swapping_open(path: str | os.PathLike[str], flags: int) -> int:
            replacement.replace(target)
            return open_file(path, flags)

        monkeypatch.setattr(namespace["os"], "open", swapping_open)
        message = "feed changed while it was being opened"
    else:
        read_file = namespace["os"].read
        grew = False

        def growing_read(descriptor: int, size: int) -> bytes:
            nonlocal grew
            if not grew:
                grew = True
                with target.open("ab") as stream:
                    stream.write(b" ")
            return read_file(descriptor, size)

        monkeypatch.setattr(namespace["os"], "read", growing_read)
        message = "feed changed while it was being read"

    with pytest.raises(ValueError, match=message):
        read_regular(target, MAX_FEED_BYTES, "feed", expected=downloaded)


@pytest.mark.parametrize(("size", "accepted"), [(MAX_FEED_BYTES, True), (MAX_FEED_BYTES + 1, False)])
def test_rss_enforces_exact_download_limit(
    helpers: HelperInstallation,
    size: int,
    accepted: bool,
) -> None:
    fixture = helpers.home / "feed.xml"
    fixture.write_bytes(_feed_bytes(size))
    call_log = helpers.root / "curl-calls"
    _install_copying_curl(helpers)

    result = helpers.run(
        "rss-json.py",
        "https://example.test/feed.xml",
        environment={"CALL_LOG": os.fspath(call_log), "RSS_FIXTURE": os.fspath(fixture)},
    )

    assert (result.returncode == 0) is accepted
    assert bool(result.stdout) is accepted
    if not accepted:
        assert b"download exceeds 8 MiB" in result.stderr


def test_rss_reads_only_a_bounded_curl_error_tail(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    fetch = namespace["fetch"]
    feed_type = namespace["Feed"]

    def fake_run(*_arguments: object, stderr: Any, **_keywords: object) -> subprocess.CompletedProcess[bytes]:
        stderr.write(b"sensitive-prefix\n" + b"x" * 32 + b"\nlast\n")
        stderr.flush()
        return subprocess.CompletedProcess([], 7)

    def reject_whole_file_read(_path: Path, *_arguments: object, **_keywords: object) -> str:
        raise AssertionError("curl diagnostics must not be whole-read")

    monkeypatch.setattr(subprocess, "run", fake_run)
    monkeypatch.setattr(Path, "read_text", reject_whole_file_read)
    monkeypatch.setitem(fetch.__globals__, "MAX_ERROR_TAIL_BYTES", 8)

    result = fetch(feed_type(1, "public", "https://example.test/feed.xml", "identity", ""), tmp_path)

    assert result.failure is not None
    assert "last" in result.failure
    assert "sensitive-prefix" not in result.failure


def test_rss_aggregate_output_limit_is_all_or_nothing(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    fixture = helpers.home / "feed.xml"
    fixture.write_bytes(_feed_bytes())
    call_log = helpers.root / "curl-calls"
    _install_copying_curl(helpers)
    namespace = runpy.run_path(os.fspath(helpers.bin / "rss-json.py"))
    main = namespace["main"]
    monkeypatch.setenv("PATH", helpers.environment()["PATH"])
    monkeypatch.setenv("CALL_LOG", os.fspath(call_log))
    monkeypatch.setenv("RSS_FIXTURE", os.fspath(fixture))

    assert main(["https://example.test/feed.xml"]) == 0
    expected = capfd.readouterr().out
    expected_bytes = len(expected.encode())
    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", expected_bytes)
    assert main(["https://example.test/feed.xml"]) == 0
    exact = capfd.readouterr()
    assert exact.out == expected

    monkeypatch.setitem(main.__globals__, "MAX_OUTPUT_BYTES", expected_bytes - 1)
    assert main(["https://example.test/feed.xml"]) == 1
    over = capfd.readouterr()
    assert over.out == ""
    assert "output exceeds 64 MiB" in over.err


def test_rss_incremental_retention_deduplicates_then_stops_at_exact_budget(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    namespace = runpy.run_path("src/fkf/assets_data/presets/bin/rss-json.py")
    retain_unique = namespace.get("retain_unique")

    assert retain_unique is not None, "RSS records need bounded incremental retention"
    first = {"id": "item:first", "kind": "item", "time": "2026-05-04T09:00:00Z", "title": "First"}
    duplicate = {**first, "title": "x" * 1_000_000}
    over = {"id": "item:over", "kind": "item", "time": "2026-05-04T09:00:00Z", "title": "Over"}
    exact_limit = len(namespace["encode_output"]([first]))
    monkeypatch.setitem(retain_unique.__globals__, "MAX_OUTPUT_BYTES", exact_limit)
    pulled = 0

    def candidates():
        nonlocal pulled
        for record in (first, duplicate, over):
            pulled += 1
            yield record
        raise AssertionError("retention continued after the final output could not fit")

    unique: dict[str, dict[str, Any]] = {}
    retained_bytes, fits = retain_unique(unique, len(b"[]\n"), candidates())

    assert not fits
    assert pulled == 3
    assert unique == {first["id"]: first}
    assert retained_bytes == exact_limit


def test_rss_keeps_temporary_files_within_the_worker_frontier(
    helpers: HelperInstallation,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    namespace = runpy.run_path(os.fspath(helpers.bin / "rss-json.py"))
    main = namespace["main"]
    feed_type = namespace["Feed"]
    result_type = namespace["FetchResult"]
    fingerprint = namespace["file_fingerprint"]
    feeds = [
        feed_type(number, "public", f"https://example.test/{number}.xml", f"feed-{number}", "")
        for number in range(1, 13)
    ]
    peak_files = 0
    lock = threading.Lock()

    def fake_fetch(feed: Any, directory: Path) -> Any:
        nonlocal peak_files
        target = directory / f"{feed.number}.xml"
        target.write_bytes(_feed_bytes())
        (directory / f"{feed.number}.err").write_bytes(b"")
        downloaded = fingerprint(target.lstat())
        with lock:
            peak_files = max(peak_files, len(list(directory.iterdir())))
        return result_type(downloaded, None)

    monkeypatch.setitem(main.__globals__, "inputs", lambda _arguments: feeds)
    monkeypatch.setitem(main.__globals__, "fetch", fake_fetch)

    assert main(["synthetic"]) == 0

    assert len(json.loads(capfd.readouterr().out)) == 24
    assert peak_files <= 16


def test_rss_emits_no_partial_output_when_one_feed_is_invalid(helpers: HelperInstallation) -> None:
    fixture = helpers.home / "feed.xml"
    fixture.write_bytes(_feed_bytes())
    helpers.fake(
        "curl",
        """output=
bad=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    *invalid*) bad=true; shift ;;
    *) shift ;;
  esac
done
if [ "$bad" = true ]; then printf 'not XML' > "$output"; else cp "$RSS_FIXTURE" "$output"; fi
""",
    )

    result = helpers.run(
        "rss-json.py",
        "https://example.test/valid.xml",
        "https://example.test/invalid.xml",
        environment={"RSS_FIXTURE": os.fspath(fixture)},
    )

    assert result.returncode == 1
    assert result.stdout == b""
    assert b"not XML" in result.stderr
