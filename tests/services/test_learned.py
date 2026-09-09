from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path
from threading import Event
from typing import Any

import pytest

import fkf.learned as learned_module
from fkf.base import Base
from fkf.config import load_config
from fkf.learned import learned_bullets, list_learned
from fkf.markdown import parse_page
from fkf.process import CommandCanceledError
from fkf.query import Window

CONFIG = """\
fkf: 1
name: brain
schema:
  id: {description: Stable identity., cardinality: one}
  time: {description: Event time., cardinality: one}
layers: {events: false, index: false, tasks: true, projects: true, wiki: true}
sources: {}
"""


def make_base(tmp_path: Path) -> Base:
    root = tmp_path / "brain"
    root.mkdir()
    (root / "fkf.yaml").write_text(CONFIG)
    config = load_config(root)
    return Base(config=config, store=config.store(), now=lambda: datetime(2026, 5, 10, tzinfo=UTC))


def write(base: Base, uri: str, text: str) -> None:
    target = base.root / uri
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(text)


def test_learned_bullets_follow_exact_commonmark_sections() -> None:
    page = parse_page(
        "tasks/2026-05-04/session/TASKS.md",
        b"""# Session

## Lessons Learned

- Not selected.

## Learned

- A short bullet.
- A bullet that wraps across two
  physical lines with **markup** and [a link](https://example.com).
  - Nested lesson.

### Detail

- A child-section bullet.

## End

- Not selected either.
""",
    )
    assert learned_bullets(page) == (
        "A short bullet.",
        "A bullet that wraps across two physical lines with markup and a link.",
        "Nested lesson.",
        "A child-section bullet.",
    )


def test_list_learned_marks_cited_trace_and_preserves_whole_counts(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    base = make_base(tmp_path)
    write(base, "tasks/2026-05-04/promoted/TASKS.md", "# Promoted\n\n## Learned\n\n- Cited.\n")
    write(base, "tasks/2026-05-05/orphan/TASKS.md", "# Orphan\n\n## Learned\n\n- Not cited.\n")
    write(
        base,
        "wiki/promoted.md",
        "---\ntype: decision\ntitle: Promoted\ntags: [x]\n"
        "sources:\n  - ../tasks/2026-05-04/promoted/TASKS.md#learned\n---\n\n# Promoted\n",
    )
    cancel = Event()
    forwarded: set[str] = set()
    original_list_tasks = learned_module.list_tasks
    original_load_layer = learned_module.load_markdown_layer

    def list_tasks(selected: Base, window: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("tasks")
        return original_list_tasks(selected, window, cancel=cancel_event)

    def load_layer(selected: Base, layer: Any, *, cancel: object) -> Any:
        assert cancel is cancel_event
        forwarded.add("pages")
        return original_load_layer(selected, layer, cancel=cancel_event)

    cancel_event = cancel
    monkeypatch.setattr(learned_module, "list_tasks", list_tasks)
    monkeypatch.setattr(learned_module, "load_markdown_layer", load_layer)

    listing = list_learned(base, cancel=cancel)
    assert [(item.trace, item.text, item.harvested) for item in listing.bullets] == [
        ("tasks/2026-05-05/orphan/TASKS.md", "Not cited.", False),
        ("tasks/2026-05-04/promoted/TASKS.md", "Cited.", True),
    ]
    assert listing.harvested == 1
    assert listing.unharvested == 1

    backlog = list_learned(base, Window(), only_unharvested=True, cancel=cancel)
    assert [item.text for item in backlog.bullets] == ["Not cited."]
    assert backlog.harvested == 1
    assert backlog.unharvested == 1
    assert forwarded == {"tasks", "pages"}


def test_list_learned_honors_window_and_cancellation(tmp_path: Path) -> None:
    base = make_base(tmp_path)
    write(base, "tasks/2026-05-04/old/TASKS.md", "# Old\n\n## Learned\n\n- Old.\n")
    write(base, "tasks/2026-05-05/new/TASKS.md", "# New\n\n## Learned\n\n- New.\n")
    listing = list_learned(base, Window("2026-05-05", "2026-05-05"))
    assert [item.text for item in listing.bullets] == ["New."]

    canceled = Event()
    canceled.set()
    with pytest.raises(CommandCanceledError):
        list_learned(base, cancel=canceled)


@pytest.mark.parametrize(
    "heading", ["## Le**arn**ed", "## Le&#97;rned", "Learned\n=======", "## [Learned](https://example.test)"]
)
def test_rendered_heading_prefilter_preserves_commonmark(heading: str) -> None:
    page = parse_page("tasks/2026-05-04/session/TASKS.md", (heading + "\n\n- Preserve this lesson.\n").encode())
    assert learned_bullets(page) == ("Preserve this lesson.",)
