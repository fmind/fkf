"""Public resolved-configuration projection with Go-compatible JSON shape."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from fkf.config import Client, Config, Identity, RecencyPolicy, RetryPolicy, Source, SyncConfig
from fkf.fields import FieldDefinition, FieldMap, FieldSchema
from fkf.timeutil import DurationNS


@dataclass(frozen=True, slots=True)
class PublicFieldDefinition:
    description: str
    cardinality: str
    relation: bool = field(default=False, metadata={"json": "relation,omitempty"})
    examples: tuple[str, ...] = field(default=(), metadata={"json": "examples,omitempty"})
    weight: int = field(default=0, metadata={"json": "weight,omitempty"})


@dataclass(frozen=True, slots=True)
class PublicIdentity:
    canonical: str
    aliases: tuple[str, ...]
    kind: str | None = field(default=None, metadata={"json": "kind,omitempty"})
    owner: bool = field(default=False, metadata={"json": "owner,omitempty"})


@dataclass(frozen=True, slots=True)
class PublicRecency:
    half_life_days: int


@dataclass(frozen=True, slots=True)
class PublicRetry:
    attempts: int = field(default=0, metadata={"json": "attempts,omitempty"})
    backoff: DurationNS = field(default=DurationNS(0), metadata={"json": "backoff,omitempty"})
    on: tuple[str, ...] = field(default=(), metadata={"json": "on,omitempty"})


@dataclass(frozen=True, slots=True)
class PublicSync:
    days: int
    index_max_age_hours: int
    timeout: DurationNS
    concurrency: int


@dataclass(frozen=True, slots=True)
class PublicSource:
    name: str
    enabled: bool
    layer: str
    max_age_hours: int | None = field(default=None, metadata={"json": "max_age_hours,omitempty"})
    auth: tuple[str, ...] = field(default=(), metadata={"json": "auth,omitempty"})
    run: tuple[str, ...] = ()
    test: tuple[str, ...] = field(default=(), metadata={"json": "test,omitempty"})
    format: str = "json"
    records: str | None = field(default=None, metadata={"json": "records,omitempty"})
    fields: dict[str, str | list[str]] | None = field(default=None, metadata={"json": "fields,omitempty"})
    body: tuple[str, ...] = field(default=(), metadata={"json": "body,omitempty"})
    bodies: str = "none"
    recency: PublicRecency | None = field(default=None, metadata={"json": "recency,omitempty"})
    requires: tuple[str, ...] = field(default=(), metadata={"json": "requires,omitempty"})
    install: str = field(default="", metadata={"json": "install,omitempty"})
    timeout: DurationNS | None = field(default=None, metadata={"json": "timeout,omitempty"})
    retry: PublicRetry | None = field(default=None, metadata={"json": "retry,omitempty"})
    min_interval: DurationNS | None = field(default=None, metadata={"json": "min_interval,omitempty"})
    window: bool = field(default=False, metadata={"json": "window,omitempty"})


@dataclass(frozen=True, slots=True)
class PublicConfig:
    fkf: int
    name: str
    schema: dict[str, PublicFieldDefinition]
    layers: dict[str, bool]
    identities: dict[str, PublicIdentity] | None = field(default=None, metadata={"json": "identities,omitempty"})
    sources: dict[str, PublicSource] = field(default_factory=dict)
    sync: PublicSync = field(default_factory=lambda: PublicSync(30, 168, DurationNS(120_000_000_000), 4))
    bin: tuple[str, ...] = field(default=(), metadata={"json": "bin,omitempty"})
    path: Path = Path()
    local_path: Path | None = field(default=None, metadata={"json": "local_path,omitempty"})
    origins: dict[str, Path] | None = field(default=None, metadata={"json": "origins,omitempty"})
    clients: dict[str, Client] = field(default_factory=dict, metadata={"json": "clients,omitempty"})


def _field_definition(value: FieldDefinition) -> PublicFieldDefinition:
    return PublicFieldDefinition(
        value.description,
        value.cardinality.value,
        value.relation,
        value.examples,
        value.weight,
    )


def _field_schema(value: FieldSchema) -> dict[str, PublicFieldDefinition]:
    return {name: _field_definition(value[name]) for name in value.names()}


def _identity(value: Identity) -> PublicIdentity:
    return PublicIdentity(
        value.canonical,
        value.aliases,
        value.kind.value if value.kind is not None else None,
        value.owner,
    )


def _retry(value: RetryPolicy) -> PublicRetry | None:
    if value.attempts == 0 and value.backoff == 0 and not value.on:
        return None
    return PublicRetry(value.attempts, value.backoff, value.on)


def _recency(value: RecencyPolicy) -> PublicRecency | None:
    return PublicRecency(value.half_life_days) if value.half_life_days else None


def _field_map(value: FieldMap) -> dict[str, str | list[str]] | None:
    return value.to_json_value() if value else None


def _source(value: Source) -> PublicSource:
    return PublicSource(
        value.name,
        value.enabled,
        value.layer.value,
        value.max_age_hours,
        value.auth,
        value.run,
        value.test,
        value.format.value,
        value.records.raw if value.records is not None and not value.records.is_zero else None,
        _field_map(value.fields),
        value.body,
        value.bodies.value,
        _recency(value.recency),
        value.requires,
        value.install,
        value.timeout or None,
        _retry(value.retry),
        value.min_interval or None,
        value.window,
    )


def _sync(value: SyncConfig) -> PublicSync:
    return PublicSync(value.days, value.index_max_age_hours, value.timeout, value.concurrency)


def public_config(value: Config) -> PublicConfig:
    """Project runtime-only objects onto the documented resolved JSON contract."""

    return PublicConfig(
        value.fkf,
        value.name,
        _field_schema(value.schema),
        {name.value: enabled for name, enabled in value.layers.items()},
        {name: _identity(identity) for name, identity in value.identities.items()} or None,
        {name: _source(value.sources[name]) for name in value.source_names()},
        _sync(value.sync),
        value.bin,
        value.path,
        value.local_path,
        value.origins or None,
        value.clients,
    )


__all__ = ["PublicConfig", "public_config"]
