from __future__ import annotations

import asyncio

import pytest

from fkf.errors import (
    CanceledError,
    ExitCategory,
    FKFError,
    InvalidUsageError,
    OperationalError,
    UntrustedError,
    exit_code_for,
)


@pytest.mark.parametrize(
    ("category", "code"),
    [
        (ExitCategory.SUCCESS, 0),
        (ExitCategory.OPERATIONAL, 1),
        (ExitCategory.INVALID_USAGE, 2),
        (ExitCategory.UNTRUSTED, 3),
        (ExitCategory.CANCELED, 130),
    ],
)
def test_exit_categories_keep_the_cli_contract(category: ExitCategory, code: int) -> None:
    assert category.exit_code == code


@pytest.mark.parametrize(
    ("error", "category"),
    [
        (OperationalError("failed"), ExitCategory.OPERATIONAL),
        (InvalidUsageError("bad flag"), ExitCategory.INVALID_USAGE),
        (UntrustedError("changed plan"), ExitCategory.UNTRUSTED),
        (CanceledError("stopped"), ExitCategory.CANCELED),
    ],
)
def test_typed_errors_expose_their_category(error: FKFError, category: ExitCategory) -> None:
    assert error.category is category
    assert error.exit_code == category.exit_code
    assert str(error)


def test_exit_code_for_handles_success_unknown_failures_and_cancellation() -> None:
    assert exit_code_for(None) == 0
    assert exit_code_for(RuntimeError("boom")) == 1
    assert exit_code_for(InvalidUsageError("bad input")) == 2
    assert exit_code_for(asyncio.CancelledError()) == 130
    assert exit_code_for(KeyboardInterrupt()) == 130


def test_error_can_keep_a_cause_without_hiding_its_public_message() -> None:
    cause = ValueError("provider detail")
    error = OperationalError("collection failed", cause=cause)

    assert str(error) == "collection failed"
    assert error.__cause__ is cause
