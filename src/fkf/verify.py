"""Full stored-evidence validation without short-circuiting on corrupt files."""

from __future__ import annotations

from dataclasses import dataclass

from fkf.base import Base
from fkf.documents import event_document_uri, index_document_uri
from fkf.errors import CanceledError
from fkf.process import Cancellation, check_cancel
from fkf.store import Layer


@dataclass(frozen=True, slots=True)
class VerifyFinding:
    uri: str
    problem: str


@dataclass(frozen=True, slots=True)
class VerifyReport:
    base: str
    documents: int
    records: int
    findings: tuple[VerifyFinding, ...]
    ok: bool


def document_uris(base: Base, *, cancel: Cancellation | None = None) -> tuple[str, ...]:
    """List stored documents in stable events-then-index order."""
    check_cancel(cancel)
    uris: list[str] = []
    if base.store.enabled(Layer.EVENTS):
        for day in base.event_dates():
            check_cancel(cancel)
            for name in base.day_documents(day):
                check_cancel(cancel)
                uris.append(event_document_uri(day, name))
    if base.store.enabled(Layer.INDEX):
        for name in base.index_documents():
            check_cancel(cancel)
            uris.append(index_document_uri(name))
    return tuple(uris)


def verify(base: Base, *, cancel: Cancellation | None = None) -> VerifyReport:
    """Reapply collection-time rules to every durable evidence document."""
    check_cancel(cancel)
    findings: list[VerifyFinding] = []
    records = 0
    uris = document_uris(base, cancel=cancel)
    for uri in uris:
        check_cancel(cancel)
        try:
            document = base.read_document(uri)
        except CanceledError:
            raise
        except Exception as error:
            findings.append(VerifyFinding(uri, str(error)))
            continue
        records += document.count
    return VerifyReport(str(base.root), len(uris), records, tuple(findings), not findings)


__all__ = ["VerifyFinding", "VerifyReport", "document_uris", "verify"]
