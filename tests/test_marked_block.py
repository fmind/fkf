from __future__ import annotations

import pytest

from fkf.marked_block import BLOCK_END_MARKER, MarkedBlockError, block_begin_marker, replace_marked_block

BEGIN = block_begin_marker("fkf build wiki")
BLOCK = f"{BEGIN}\n\n## Pages\n\n- generated\n\n{BLOCK_END_MARKER}\n"


def test_replace_marked_block_preserves_every_authored_byte() -> None:
    existing = f"# Wiki\n\nCurated.  \n\n{BEGIN}\nold\n{BLOCK_END_MARKER}\n\nTail  \n"

    replaced = replace_marked_block(existing, BLOCK, "# Wiki\n")

    assert replaced == f"# Wiki\n\nCurated.  \n\n{BLOCK}\nTail  \n"


def test_replace_marked_block_appends_to_authored_or_default_content() -> None:
    assert replace_marked_block("Authored", BLOCK, "# Wiki\n") == f"Authored\n\n{BLOCK}"
    assert replace_marked_block(" \n", BLOCK, "# Wiki\n") == f"# Wiki\n\n{BLOCK}"


@pytest.mark.parametrize(
    ("existing", "message"),
    [
        (f"owner\n{BEGIN}\ntruncated\n", "no matching end marker"),
        (f"{BEGIN}\n{BEGIN}\n{BLOCK_END_MARKER}\n", "more than one canonical begin marker"),
        (f"{BEGIN}\n{BLOCK_END_MARKER}\n{BLOCK_END_MARKER}\n", "more than one canonical end marker"),
        (
            f"<!-- >>> fkf managed block — regenerate with `fkf wiki index`; edits between the markers are lost -->\n{BLOCK_END_MARKER}\n",
            "non-canonical begin marker",
        ),
        (f"{BEGIN}\n{BLOCK_END_MARKER} obsolete\n", "non-canonical end marker"),
        (f"owner\n{BLOCK_END_MARKER}\n", "no matching begin marker"),
        (f"{BLOCK_END_MARKER}\n{BEGIN}\n", "before it"),
    ],
)
def test_replace_marked_block_rejects_ambiguous_existing_topology(existing: str, message: str) -> None:
    with pytest.raises(MarkedBlockError, match=message):
        replace_marked_block(existing, BLOCK, "# Wiki\n")


@pytest.mark.parametrize(
    "generated",
    [
        "no markers\n",
        f"prefix\n{BLOCK}",
        f"{BEGIN}\nbody\n",
        f"{BEGIN}\n{BLOCK_END_MARKER}\nextra\n",
        f"{BEGIN}\n{BLOCK_END_MARKER}\n{BLOCK_END_MARKER}\n",
    ],
)
def test_replace_marked_block_rejects_invalid_generated_region(generated: str) -> None:
    with pytest.raises(MarkedBlockError, match="generated block"):
        replace_marked_block("# Wiki\n", generated, "# Wiki\n")


def test_crlf_markers_fail_closed_as_noncanonical() -> None:
    existing = f"# Wiki\r\n{BEGIN}\r\n{BLOCK_END_MARKER}\r\n"

    with pytest.raises(MarkedBlockError, match="non-canonical begin marker"):
        replace_marked_block(existing, BLOCK, "# Wiki\n")


def test_only_lf_delimits_marker_lines() -> None:
    existing = f"owner\r{BEGIN}\n{BLOCK_END_MARKER}\n"

    with pytest.raises(MarkedBlockError, match="non-canonical begin marker"):
        replace_marked_block(existing, BLOCK, "# Wiki\n")
