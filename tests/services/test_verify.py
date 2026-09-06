from __future__ import annotations

from pathlib import Path
from threading import Event

import pytest

from fkf.base import Base
from fkf.config import Config, SyncConfig
from fkf.errors import CanceledError
from fkf.fields import FieldSchema
from fkf.store import Layer, Store
from fkf.verify import document_uris, verify


def _base(tmp_path: Path) -> Base:
    config_path = tmp_path / "fkf.yaml"
    config_path.write_text("name: test\n")
    layers = {layer: layer in {Layer.EVENTS, Layer.INDEX} for layer in Layer}
    config = Config(1, "test", FieldSchema(), layers, {}, {}, SyncConfig(), (), config_path)
    return Base(config, Store(tmp_path, layers))


def _write(tmp_path: Path, relative: str, *, fkf: int = 1, count: int = 1) -> None:
    source = Path(relative).stem
    layer = "events" if relative.startswith("events/") else "index"
    date = ',"date":"2026-05-04"' if layer == "events" else ""
    path = tmp_path / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        f'{{"fkf":{fkf},"source":"{source}","layer":"{layer}"{date},'
        '"collected_at":"2026-05-04T09:00:00Z",'
        '"schema":{"id":{"description":"Stable identity.","cardinality":"one"}'
        + (',"time":{"description":"Event time.","cardinality":"one"}' if layer == "events" else "")
        + '},"fields":{"id":".id"'
        + (',"time":".time"' if layer == "events" else "")
        + f'}},"body":false,"count":{count},"records":[{{"id":"one"'
        + (',"time":"2026-05-04T09:00:00Z"' if layer == "events" else "")
        + "}]}"
    )


def test_verify_counts_clean_documents_in_stable_order(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write(tmp_path, "events/2026-05-04/beta.json")
    _write(tmp_path, "events/2026-05-04/alpha.json")
    _write(tmp_path, "index/repos.json")
    assert document_uris(base) == (
        "events/2026-05-04/alpha.json",
        "events/2026-05-04/beta.json",
        "index/repos.json",
    )
    report = verify(base)
    assert (report.documents, report.records, report.ok, report.findings) == (3, 3, True, ())


def test_verify_continues_after_bad_documents(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write(tmp_path, "events/2026-05-04/bad.json", fkf=99)
    _write(tmp_path, "events/2026-05-04/fine.json")
    report = verify(base)
    assert (report.documents, report.records, report.ok) == (2, 1, False)
    assert report.findings[0].uri.endswith("bad.json")
    assert "fkf 99" in report.findings[0].problem


def test_verify_reports_count_mismatch(tmp_path: Path) -> None:
    base = _base(tmp_path)
    _write(tmp_path, "index/bad.json", count=7)
    report = verify(base)
    assert not report.ok
    assert "count 7 does not match 1 records" in report.findings[0].problem


def test_verify_and_document_listing_honor_preexisting_cancellation(tmp_path: Path) -> None:
    base = _base(tmp_path)
    cancel = Event()
    cancel.set()

    with pytest.raises(CanceledError, match="operation canceled"):
        document_uris(base, cancel=cancel)
    with pytest.raises(CanceledError, match="operation canceled"):
        verify(base, cancel=cancel)
