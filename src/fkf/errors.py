"""Typed failures at FKF's stable command-line exit boundary."""

from __future__ import annotations

import asyncio
from enum import StrEnum
from typing import ClassVar


class ExitCategory(StrEnum):
    """The public failure categories consumed by schedulers and shell scripts."""

    SUCCESS = "success"
    OPERATIONAL = "operational"
    INVALID_USAGE = "invalid_usage"
    UNTRUSTED = "untrusted"
    CANCELED = "canceled"

    @property
    def exit_code(self) -> int:
        """Return the stable process exit code for this category."""
        return _EXIT_CODES[self]


_EXIT_CODES: dict[ExitCategory, int] = {
    ExitCategory.SUCCESS: 0,
    ExitCategory.OPERATIONAL: 1,
    ExitCategory.INVALID_USAGE: 2,
    ExitCategory.UNTRUSTED: 3,
    ExitCategory.CANCELED: 130,
}


class FKFError(RuntimeError):
    """Base class for an expected FKF failure with a stable exit category."""

    category: ClassVar[ExitCategory] = ExitCategory.OPERATIONAL

    def __init__(self, message: str, *, cause: BaseException | None = None) -> None:
        super().__init__(message)
        if cause is not None:
            self.__cause__ = cause

    @property
    def exit_code(self) -> int:
        """Return the process exit code without coupling callers to CLI code."""
        return self.category.exit_code


class OperationalError(FKFError):
    """A runtime or provider operation failed."""


class InvalidUsageError(FKFError):
    """Configuration or caller input is invalid."""

    category = ExitCategory.INVALID_USAGE


class UntrustedError(FKFError):
    """Execution was refused because the trusted plan changed."""

    category = ExitCategory.UNTRUSTED


class CanceledError(FKFError):
    """The caller canceled an in-flight operation."""

    category = ExitCategory.CANCELED


def exit_code_for(error: BaseException | None) -> int:
    """Map one result to FKF's documented command-line exit code."""
    if error is None:
        return ExitCategory.SUCCESS.exit_code
    if isinstance(error, (asyncio.CancelledError, KeyboardInterrupt)):
        return ExitCategory.CANCELED.exit_code
    if isinstance(error, FKFError):
        return error.exit_code
    return ExitCategory.OPERATIONAL.exit_code


__all__ = [
    "CanceledError",
    "ExitCategory",
    "FKFError",
    "InvalidUsageError",
    "OperationalError",
    "UntrustedError",
    "exit_code_for",
]
