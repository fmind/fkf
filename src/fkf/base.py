"""Open one FKF base and centralize its published storage boundary."""

from __future__ import annotations

import os
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path, PurePosixPath
from typing import TYPE_CHECKING, Any

from fkf.config import Config, ConfigError, Source, load_config
from fkf.errors import FKFError
from fkf.io import read_file_limited
from fkf.process import Runner, SubprocessRunner
from fkf.store import MAX_NARRATIVE_BYTES, MAX_SOURCE_DOCUMENT_BYTES, Layer, LayerDisabledError, Store, discover_base

if TYPE_CHECKING:
    from fkf.documents import Document
    from fkf.scan import ScanGuard
    from fkf.source_runtime import Environment


Clock = Callable[[], datetime]


def _now_local() -> datetime:
    # Go's time.Now retains the process timezone; civil-day windows must do the same.
    return datetime.now().astimezone()


@dataclass(slots=True)
class Base:
    """One loaded base with injectable execution and clock seams."""

    config: Config
    store: Store
    environment: Environment | Any = None
    runner: Runner = field(default_factory=SubprocessRunner)
    now: Clock = _now_local
    origin: str = ""

    @property
    def root(self) -> Path:
        return self.store.root

    def source(self, name: str) -> Source:
        """Resolve a declared source while making a typo distinguishable from no data."""
        if source := self.config.sources.get(name):
            return source
        declared = self.config.source_names()
        if not declared:
            raise ConfigError(f"{self.config.name} declares no sources; add one under `sources:` in {self.config.path}")
        raise ConfigError(
            f"source {name!r} is not declared in {self.config.path}; declared sources are {', '.join(declared)}"
        )

    def require_layer(self, layer: Layer) -> None:
        if not self.store.enabled(layer):
            raise LayerDisabledError(layer)

    def read_file(self, relative: str, limit: int) -> bytes:
        return read_file_limited(self.store.resolve(relative), limit)

    def exists(self, relative: str) -> bool:
        try:
            absolute = self.store.resolve(relative)
            absolute.stat()
        except OSError, ValueError, FKFError:
            return False
        return True

    def read_document(self, relative: str, *, scan: ScanGuard | None = None) -> Document:
        """Read one complete evidence document and bind it to its published URI."""
        from fkf.documents import decode_document, verify_document

        absolute = self.store.resolve(relative)
        data = read_file_limited(absolute, MAX_SOURCE_DOCUMENT_BYTES)
        if scan is not None:
            # Charge the inode bytes actually opened, not racy path metadata.
            scan.consume(len(data))
        document = decode_document(data, str(absolute))
        requested = PurePosixPath(relative).as_posix()
        minted = document.uri()
        if requested != minted:
            raise ConfigError(
                f"stored document {requested} mints {minted} from its source, layer, and date metadata; re-collect it"
            )
        verify_document(document)
        if document.count != len(document.records):
            raise ConfigError(
                f"stored document {requested} is incomplete: declares count {document.count} "
                f"but holds {len(document.records)} record(s)"
            )
        return document

    def write_document(self, document: Document) -> None:
        """Atomically file one already-complete evidence document."""
        from fkf.documents import verify_document, write_document

        verify_document(document)
        if document.count != len(document.records):
            raise ConfigError(
                f"document {document.uri()} is incomplete: declares count {document.count} "
                f"but holds {len(document.records)} record(s)"
            )
        write_document(self.store.resolve(document.uri()), document)

    def event_dates(self, *, scan: ScanGuard | None = None) -> tuple[str, ...]:
        self.require_layer(Layer.EVENTS)
        return _date_directories(self.store.directory(Layer.EVENTS), scan)

    def day_documents(self, value: str, *, scan: ScanGuard | None = None) -> tuple[str, ...]:
        directory = self.store.resolve(f"events/{value}")
        try:
            iterator = os.scandir(directory)
        except FileNotFoundError:
            return ()
        except OSError as error:
            raise OSError(f"list {directory}: {error}") from error
        with iterator:
            entries = []
            for entry in iterator:
                if scan is not None:
                    scan.visit()
                entries.append(entry)
            entries.sort(key=lambda entry: entry.name)
        try:
            return tuple(
                entry.name.removesuffix(".json")
                for entry in entries
                if not entry.is_dir(follow_symlinks=False) and entry.name.endswith(".json")
            )
        finally:
            for entry in entries:
                del entry

    def index_documents(self, *, scan: ScanGuard | None = None) -> tuple[str, ...]:
        self.require_layer(Layer.INDEX)
        directory = self.store.directory(Layer.INDEX)
        try:
            iterator = os.scandir(directory)
        except FileNotFoundError:
            return ()
        except OSError as error:
            raise OSError(f"list {directory}: {error}") from error
        with iterator:
            entries = []
            for entry in iterator:
                if scan is not None:
                    scan.visit()
                entries.append(entry)
            entries.sort(key=lambda entry: entry.name)
        return tuple(
            entry.name.removesuffix(".json")
            for entry in entries
            if not entry.is_dir(follow_symlinks=False)
            and not entry.name.startswith(".")
            and entry.name.endswith(".json")
        )

    def read_narrative(self, relative: str) -> bytes:
        return self.read_file(relative, MAX_NARRATIVE_BYTES)


def _date_directories(directory: Path, scan: ScanGuard | None = None) -> tuple[str, ...]:
    try:
        entries = os.scandir(directory)
    except FileNotFoundError:
        return ()
    except OSError as error:
        raise OSError(f"list {directory}: {error}") from error
    values: list[str] = []
    with entries:
        for entry in entries:
            if scan is not None:
                scan.visit()
            if not entry.is_dir(follow_symlinks=False):
                continue
            try:
                parsed = datetime.strptime(entry.name, "%Y-%m-%d").date()
            except ValueError:
                continue
            if parsed.isoformat() == entry.name:
                values.append(entry.name)
    return tuple(sorted(values))


def open_base(explicit: str = "") -> Base:
    """Discover and load a base without creating it."""
    from fkf.source_runtime import Environment

    root, origin = discover_base(explicit)
    config = load_config(root)
    return Base(
        config=config,
        store=config.store(),
        environment=Environment.from_config(config),
        runner=SubprocessRunner(),
        origin=origin,
    )


__all__ = ["Base", "Clock", "open_base"]
