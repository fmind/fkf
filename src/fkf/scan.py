"""Optional scan accounting seam for bounded adapters."""

from __future__ import annotations

from typing import Protocol


class ScanGuard(Protocol):
    """Account work without imposing a policy on ordinary service callers."""

    def visit(self) -> None:
        """Account one examined filesystem entry."""

    def consume(self, size: int) -> None:
        """Account bytes from one source file before it is parsed."""

    def retain(self, value: object) -> None:
        """Account one retained result item and its encoded metadata."""


__all__ = ["ScanGuard"]
