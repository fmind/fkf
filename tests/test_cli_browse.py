from __future__ import annotations

import io
from pathlib import Path
from threading import Event

import pytest

import fkf.cli_browse as cli_browse
from fkf.cli import app
from fkf.cli_browse import (
    _event_listing_text,
    _index_listing_text,
    _page_listing_text,
    _record_title_text,
    _tag_vocabulary_text,
    _task_listing_text,
    _validation_bundle_text,
    _validation_text,
)
from fkf.cli_support import run_app
from fkf.errors import CanceledError, OperationalError
from fkf.learned import LearnedListing
from fkf.listings import DayCount, EventDay, EventListing, IndexListing, TaskListing
from fkf.markdown import Page, ValidationReport
from fkf.output import jsonl_bytes
from fkf.pages import PageListing, TagCount, TagVocabulary
from fkf.query import Window
from fkf.store import Layer
from fkf.validation import RecordTitleReport, ValidationBundle
from tests.services.test_context import seeded_base


def invoke(*arguments: str) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    code = run_app(app, arguments, stdout=stdout, stderr=stderr)
    return code, stdout.getvalue(), stderr.getvalue()


def test_listing_text_uses_uris_context_and_explicit_empty_totals() -> None:
    events = EventListing(
        Window("2026-09-05", "2026-09-05"),
        (
            EventDay(
                "2026-09-05",
                "events/2026-09-05/",
                7,
                (
                    DayCount("a", "events/2026-09-05/a.json", 3, False),
                    DayCount("b", "events/2026-09-05/b.json", 2, False),
                    DayCount("c", "events/2026-09-05/c.json", 1, False),
                    DayCount("d", "events/2026-09-05/d.json", 1, False),
                ),
            ),
        ),
        7,
    )
    assert _event_listing_text(events) == (
        "events/2026-09-05/     7  a 3 · b 2 · c 1 · +1 more\n\n1 day(s), 7 record(s)"
    )
    assert _index_listing_text(IndexListing((), 0)) == "\n0 document(s)"
    assert _task_listing_text(TaskListing(Window(), ())) == "\n0 trace(s)"


def test_page_and_tag_text_keep_classifiers_tags_and_totals() -> None:
    pages = PageListing(
        Layer.PROJECTS,
        (Page("projects/a.md", "a", title="Alpha", status="active", tags=("one", "two")),),
        1,
    )
    assert _page_listing_text(pages) == (
        "projects/a.md  active     Alpha\n                          one two\n\n1 page(s) in projects/"
    )
    vocabulary = TagVocabulary(Layer.PROJECTS, (TagCount("one", 1, ("a",)),), ("plain",), 2)
    assert _tag_vocabulary_text(vocabulary) == "   1  one                      a\n\nuntagged: plain"


def test_validation_text_uses_go_summary_punctuation_and_bundle_sections() -> None:
    wiki = ValidationReport("wiki", 2, False, 0, 0, (), True)
    records = RecordTitleReport(1, 2, 3, False, 0, 0, (), True)
    assert _validation_text(wiki) == "\n2 page(s): 0 error(s), 0 warning(s)"
    assert _record_title_text(records) == "\n1 source(s), 2 document(s), 3 record(s): 0 error(s), 0 warning(s)"
    assert _validation_bundle_text(ValidationBundle(wiki=wiki, records=records, ok=True)) == (
        "\nwiki\n\n2 page(s): 0 error(s), 0 warning(s)\n"
        "\nrecords\n\n1 source(s), 2 document(s), 3 record(s): 0 error(s), 0 warning(s)"
    )


def test_empty_learned_jsonl_keeps_the_envelope() -> None:
    listing = LearnedListing(Window(), (), 0, 0)
    assert jsonl_bytes(listing) == b'{"window":{},"bullets":[],"harvested":0,"unharvested":0}\n'


def test_learned_listing_forwards_the_invocation_cancellation(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    base = seeded_base(tmp_path)
    observed: list[object] = []

    def stop(*_args: object, cancel: object = None, **_kwargs: object) -> None:
        observed.append(cancel)
        raise OperationalError("stopped after observing cancellation")

    monkeypatch.setattr("fkf.cli_browse.list_learned", stop)
    code, stdout, stderr = invoke("list", "tasks", "learned", "--base", str(base.root))

    assert code == 1
    assert stdout == ""
    assert "stopped after observing cancellation" in stderr
    assert len(observed) == 1
    assert isinstance(observed[0], Event)


@pytest.mark.parametrize(
    ("symbol", "arguments"),
    [
        ("list_events", ("list", "events")),
        ("list_index", ("list", "index")),
        ("list_tasks", ("list", "tasks")),
        ("list_pages", ("list", "projects")),
        ("validate_all", ("validate",)),
        ("validate_markdown_layer", ("validate", "wiki")),
        ("validate_record_titles", ("validate", "records")),
        ("build_tag_vocabulary", ("tags",)),
    ],
)
def test_browse_commands_forward_the_invocation_cancellation(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    symbol: str,
    arguments: tuple[str, ...],
) -> None:
    base = seeded_base(tmp_path)
    observed: list[object] = []

    def stop(*_args: object, cancel: object = None, **_kwargs: object) -> None:
        observed.append(cancel)
        raise CanceledError("stopped after observing browse cancellation")

    monkeypatch.setattr(cli_browse, symbol, stop)
    code, stdout, stderr = invoke(*arguments, "--base", str(base.root))

    assert code == 130
    assert stdout == ""
    assert "stopped after observing browse cancellation" in stderr
    assert len(observed) == 1
    assert isinstance(observed[0], Event)
