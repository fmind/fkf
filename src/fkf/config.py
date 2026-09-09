"""Strict, local-only ``fkf.yaml`` loading and execution-plan validation."""

from __future__ import annotations

import copy
import os
import re
import stat
import unicodedata
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Final, Protocol, cast
from urllib.parse import urlsplit

import yaml
from pydantic import BaseModel, ConfigDict, ValidationError
from pydantic import Field as PydanticField
from yaml.constructor import ConstructorError
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode

from fkf.errors import FKFError, InvalidUsageError
from fkf.fields import (
    FIELD_ID,
    FIELD_TITLE,
    Cardinality,
    FieldDefinition,
    FieldMap,
    FieldPath,
    FieldPaths,
    FieldSchema,
    parse_field_path,
    validate_entity_uri,
    validate_field_map,
    validate_field_schema,
)
from fkf.store import (
    BASE_CLIENTS_DIR,
    BASE_SOURCES_DIR,
    BASE_TESTS_DIR,
    CONFIG_FILE_NAME,
    LOCAL_CONFIG_NAME,
    MAX_CONFIG_BYTES,
    Layer,
    Store,
    expand_home,
    resolve_absolute_path,
    resolve_physical_path,
    validate_within_root,
)
from fkf.timeutil import DurationNS, format_duration, parse_duration

CONFIG_VERSION: Final = 1
MAX_SOURCE_NAME_LENGTH: Final = 255 - len(".json")
MAX_BASE_NAME_LENGTH: Final = 63
MAX_FRESHNESS_AGE_HOURS: Final = 10 * 365 * 24
MAX_RECENCY_HALF_LIFE_DAYS: Final = 10 * 365
MAX_RETRY_ATTEMPTS: Final = 5
MAX_SYNC_CONCURRENCY: Final = 4
MAX_CONFIG_YAML_DEPTH: Final = 128
MAX_CONFIG_YAML_EXPANDED_NODES: Final = 100_000
MAX_CONFIG_YAML_ALIAS_VISITS: Final = 10_000
MAX_CONFIG_YAML_EXPANDED_SCALAR_BYTES: Final = 4 << 20

MAX_RETRY_BACKOFF: Final = parse_duration("10m")
MAX_MIN_INTERVAL: Final = parse_duration("10m")
MIN_SYNC_TIMEOUT: Final = parse_duration("1s")
MAX_COMMAND_TIMEOUT: Final = parse_duration("1h")
_ZERO_DURATION: Final = DurationNS(0)
_DEFAULT_SYNC_TIMEOUT: Final = parse_duration("2m")

RUN_PLACEHOLDERS: Final[tuple[str, ...]] = ("date", "next_date", "start", "end", "base", "home")
TEST_PLACEHOLDERS: Final[tuple[str, ...]] = ("base", "home")
_BODY_STATIC_PLACEHOLDERS: Final[tuple[str, ...]] = ("base", "home")

_SOURCE_NAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]*$")
_REQUIREMENT_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]*$")
_BARE_IDENTITY_ALIAS_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+@-]*$")
_PLACEHOLDER_PATTERN = re.compile(r"\{\{([a-z][a-z0-9_-]*)\}\}")


class ConfigError(InvalidUsageError):
    """A malformed or unsafe base configuration."""


class OutputFormat(StrEnum):
    """How one source's standard output is decoded."""

    JSON = "json"
    NDJSON = "ndjson"


class BodyPolicy(StrEnum):
    """Persistence policy for the rebuildable body cache."""

    NONE = "none"
    CACHE = "cache"
    SYNC = "sync"


class IdentityKind(StrEnum):
    """Optional human classification of a declared graph identity."""

    PERSON = "person"
    ORGANIZATION = "organization"
    REPOSITORY = "repository"


@dataclass(slots=True)
class Identity:
    """Exact aliases that resolve to one canonical graph entity."""

    canonical: str
    aliases: tuple[str, ...]
    kind: IdentityKind | None = None
    owner: bool = False

    def effective_kind(self) -> IdentityKind | None:
        """Return the explicit or conventionally inferred identity kind."""
        if self.kind is not None:
            return self.kind
        scheme = self.canonical.partition(":")[0]
        if scheme in {"person", "actor"}:
            return IdentityKind.PERSON
        if scheme in {"organization", "org"}:
            return IdentityKind.ORGANIZATION
        if scheme in {"repository", "repo"}:
            return IdentityKind.REPOSITORY
        return None


@dataclass(frozen=True, slots=True)
class RecencyPolicy:
    """Optional exponential freshness modifier for lexical retrieval."""

    half_life_days: int = 0


@dataclass(frozen=True, slots=True)
class RetryPolicy:
    """Declared bounded retry behavior for one source."""

    attempts: int = 0
    backoff: DurationNS = _ZERO_DURATION
    on: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class SyncConfig:
    """Resolved collection defaults."""

    days: int = 30
    index_max_age_hours: int = 168
    timeout: DurationNS = _DEFAULT_SYNC_TIMEOUT
    concurrency: int = 4


@dataclass(slots=True)
class Source:
    """One validated direct-argv collection source."""

    name: str
    enabled: bool = False
    layer: Layer = Layer.EVENTS
    max_age_hours: int | None = None
    auth: tuple[str, ...] = ()
    run: tuple[str, ...] = ()
    test: tuple[str, ...] = ()
    format: OutputFormat = OutputFormat.JSON
    records: FieldPath | None = None
    fields: FieldMap = field(default_factory=FieldMap)
    schema: FieldSchema = field(default_factory=FieldSchema)
    body: tuple[str, ...] = ()
    bodies: BodyPolicy = BodyPolicy.NONE
    recency: RecencyPolicy = field(default_factory=RecencyPolicy)
    requires: tuple[str, ...] = ()
    install: str = ""
    timeout: DurationNS = _ZERO_DURATION
    retry: RetryPolicy = field(default_factory=RetryPolicy)
    min_interval: DurationNS = _ZERO_DURATION
    window: bool = False

    def effective_max_age_hours(self, fallback: int) -> int:
        """Resolve the source-local index cadence against the base default."""
        return self.max_age_hours if self.max_age_hours is not None else fallback

    def retry_attempts(self) -> int:
        """Return the total number of permitted runs, never fewer than one."""
        return max(1, self.retry.attempts)

    def has_body(self) -> bool:
        """Return whether a body command is declared."""
        return bool(self.body)

    def caches_bodies(self) -> bool:
        """Return whether body reads may populate the rebuildable cache."""
        return self.bodies in {BodyPolicy.CACHE, BodyPolicy.SYNC}

    def body_field_names(self) -> tuple[str, ...]:
        """Return record fields used as body-command arguments in stable order."""
        names: list[str] = []
        for name in self.fields.names():
            if name in _BODY_STATIC_PLACEHOLDERS:
                continue
            placeholder = f"{{{{{name}}}}}"
            if any(placeholder in argument for argument in self.body):
                names.append(name)
        return tuple(names)


@dataclass(frozen=True, slots=True)
class Client:
    """One online app and its single base-owned uv Python script."""

    url: str
    script: str


@dataclass(slots=True)
class Config:
    """One base's complete resolved definition."""

    fkf: int
    name: str
    schema: FieldSchema
    layers: dict[Layer, bool]
    identities: dict[str, Identity]
    sources: dict[str, Source]
    sync: SyncConfig
    bin: tuple[str, ...]
    path: Path
    local_path: Path | None = None
    origins: dict[str, Path] = field(default_factory=dict)
    clients: dict[str, Client] = field(default_factory=dict)

    def source_names(self) -> tuple[str, ...]:
        """Return source names in stable order."""
        return tuple(sorted(self.sources))

    def enabled_sources(self) -> tuple[Source, ...]:
        """Return enabled sources in stable order."""
        return tuple(self.sources[name] for name in self.source_names() if self.sources[name].enabled)

    def store(self) -> Store:
        """Return the confined base layout described by this configuration."""
        return Store(self.path.parent, self.layers)


class _BoundaryModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class _FileFieldDefinition(_BoundaryModel):
    description: str = ""
    cardinality: str = ""
    relation: bool = False
    examples: list[str] = PydanticField(default_factory=list)
    weight: int = 0


class _FileIdentity(_BoundaryModel):
    canonical: str = ""
    aliases: list[str] | None = None
    kind: str = ""
    owner: bool = False


type _FilePathValue = str | list[str]


class _FileRecency(_BoundaryModel):
    half_life_days: int = 0


class _FileRetry(_BoundaryModel):
    attempts: int = 0
    backoff: str = ""
    on: list[str] = PydanticField(default_factory=list)


class _FileSource(_BoundaryModel):
    enabled: bool = False
    layer: str = ""
    max_age_hours: int | None = None
    auth: list[str] | None = None
    run: list[str] | None = None
    test: list[str] | None = None
    format: str = ""
    records: str = ""
    fields: dict[str, _FilePathValue] = PydanticField(default_factory=dict)
    body: list[str] = PydanticField(default_factory=list)
    bodies: str = ""
    recency: _FileRecency | None = None
    requires: list[str] = PydanticField(default_factory=list)
    install: str = ""
    timeout: str = ""
    retry: _FileRetry | None = None
    min_interval: str = ""
    window: bool = False


class _FileSync(_BoundaryModel):
    days: int | None = None
    index_max_age_hours: int | None = None
    timeout: str | None = None
    concurrency: int | None = None


class _FileClient(_BoundaryModel):
    url: str
    script: str


class _FileConfig(_BoundaryModel):
    fkf: int = 0
    name: str = ""
    field_schema: dict[str, _FileFieldDefinition] = PydanticField(default_factory=dict, alias="schema")
    layers: dict[str, bool] = PydanticField(default_factory=dict)
    identities: dict[str, _FileIdentity] = PydanticField(default_factory=dict)
    bin: list[str] = PydanticField(default_factory=list)
    sources: dict[str, _FileSource] = PydanticField(default_factory=dict)
    clients: dict[str, _FileClient] = PydanticField(default_factory=dict)
    sync: _FileSync | None = None


class _FileLocalSource(_BoundaryModel):
    enabled: bool | None = None
    run: list[str] | None = None
    timeout: str | None = None
    max_age_hours: int | None = None


class _FileLocal(_BoundaryModel):
    bin: list[str] = PydanticField(default_factory=list)
    sources: dict[str, _FileLocalSource] = PydanticField(default_factory=dict)


class _StrictLoader(yaml.SafeLoader):
    """Safe YAML 1.2-ish loader that also refuses duplicate mapping keys."""

    def construct_mapping(self, node: MappingNode, deep: bool = False) -> dict[object, object]:
        seen: set[str] = set()
        for key_node, _ in node.value:
            if isinstance(key_node, ScalarNode) and key_node.tag != "tag:yaml.org,2002:merge":
                if key_node.value in seen:
                    raise ConstructorError(
                        "while constructing a mapping",
                        node.start_mark,
                        f"duplicate key {key_node.value!r}",
                        key_node.start_mark,
                    )
                seen.add(key_node.value)
        return super().construct_mapping(node, deep=deep)


# PyYAML defaults to YAML 1.1 booleans (yes/no/on/off) and datetime objects. FKF's
# cross-language file uses YAML 1.2 boolean spellings and keeps timestamps as text.
_StrictLoader.yaml_implicit_resolvers = copy.deepcopy(yaml.SafeLoader.yaml_implicit_resolvers)
for _initial, _resolvers in tuple(_StrictLoader.yaml_implicit_resolvers.items()):
    _StrictLoader.yaml_implicit_resolvers[_initial] = [
        (tag, matcher)
        for tag, matcher in _resolvers
        if tag not in {"tag:yaml.org,2002:bool", "tag:yaml.org,2002:timestamp"}
    ]
_StrictLoader.add_implicit_resolver(
    "tag:yaml.org,2002:bool",
    re.compile(r"^(?:true|True|TRUE|false|False|FALSE)$"),
    list("tTfF"),
)


def _error(message: str, *, cause: BaseException | None = None) -> ConfigError:
    return ConfigError(f"invalid configuration: {message}", cause=cause)


def _read_config_leaf(root: Path, name: str) -> bytes | None:
    path = root / name
    try:
        validate_within_root(root, path)
        info = path.lstat()
    except FileNotFoundError:
        return None
    except (FKFError, OSError, ValueError) as error:
        raise _error(f"inspect {path}: {error}", cause=error) from error
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _error(f"{path} must be a regular non-symlink file")
    if info.st_size > MAX_CONFIG_BYTES:
        raise _error(f"{path} is {info.st_size} bytes; expected at most {MAX_CONFIG_BYTES}")
    try:
        with path.open("rb") as stream:
            data = stream.read(MAX_CONFIG_BYTES + 1)
    except OSError as error:
        raise _error(f"read {path}: {error}", cause=error) from error
    if len(data) > MAX_CONFIG_BYTES:
        raise _error(f"{path} exceeds the {MAX_CONFIG_BYTES}-byte read limit")
    return data


def _decode_strict(data: bytes, path: Path) -> object:
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise _error(f"{path}: configuration is not valid UTF-8", cause=error) from error
    loader = _StrictLoader(text)
    try:
        if not loader.check_node():
            return {}
        node = loader.get_node()
        if node is None:
            return {}
        _validate_config_yaml_graph(path, node)
        first = loader.construct_document(node)
        try:
            if loader.check_node():
                loader.get_node()
                raise _error(f"{path} holds more than one YAML document")
        except yaml.YAMLError as error:
            raise _error(f"{path} has invalid trailing YAML: {error}", cause=error) from error
        return {} if first is None else first
    except ConfigError:
        raise
    except RecursionError as error:
        raise _error(f"{path}: YAML nesting exceeds the parser limit", cause=error) from error
    except yaml.YAMLError as error:
        raise _error(f"{path}: {error}", cause=error) from error
    finally:
        loader.dispose()


def _validate_config_yaml_graph(path: Path, root: Node) -> None:
    """Reject recursive and explosively aliased YAML before object construction."""
    stack: list[tuple[Node, int, bool]] = [(root, 1, False)]
    active: set[int] = set()
    seen: set[int] = set()
    expanded = 0
    scalar_bytes = 0
    alias_visits = 0
    while stack:
        node, depth, leaving = stack.pop()
        identity = id(node)
        if leaving:
            active.remove(identity)
            continue
        if identity in active:
            raise _error(f"{path}: recursive YAML alias")
        if depth > MAX_CONFIG_YAML_DEPTH:
            raise _error(f"{path}: YAML exceeds the {MAX_CONFIG_YAML_DEPTH}-level depth limit")
        expanded += 1
        if expanded > MAX_CONFIG_YAML_EXPANDED_NODES:
            raise _error(f"{path}: YAML alias expansion exceeds the {MAX_CONFIG_YAML_EXPANDED_NODES}-node limit")
        if identity in seen:
            alias_visits += 1
            if alias_visits > MAX_CONFIG_YAML_ALIAS_VISITS:
                raise _error(f"{path}: YAML alias expansion exceeds the {MAX_CONFIG_YAML_ALIAS_VISITS}-visit limit")
        else:
            seen.add(identity)

        if isinstance(node, MappingNode):
            children = tuple(item for pair in node.value for item in pair)
        elif isinstance(node, SequenceNode):
            children = tuple(node.value)
        elif isinstance(node, ScalarNode):
            children = ()
            scalar_bytes += len(node.value.encode())
            if scalar_bytes > MAX_CONFIG_YAML_EXPANDED_SCALAR_BYTES:
                raise _error(
                    f"{path}: YAML alias expansion exceeds the "
                    f"{MAX_CONFIG_YAML_EXPANDED_SCALAR_BYTES}-byte scalar limit"
                )
        else:
            raise _error(f"{path}: unsupported YAML node")
        if not children:
            continue
        active.add(identity)
        stack.append((node, depth, True))
        stack.extend((child, depth + 1, False) for child in reversed(children))


def decode_strict_yaml(data: bytes, path: Path) -> object:
    """Decode one YAML 1.2-shaped document with duplicate-key rejection."""
    return _decode_strict(data, path)


def _validate_model[T: _BoundaryModel](model: type[T], value: object, path: Path) -> T:
    try:
        return model.model_validate(value)
    except ValidationError as error:
        problem = error.errors(include_url=False)[0]
        location = ".".join(str(part) for part in problem["loc"])
        if problem["type"] == "extra_forbidden":
            message = f"field {problem['loc'][-1]} not found"
        else:
            message = f"{location}: {problem['msg']}" if location else str(problem["msg"])
        raise _error(f"{path}: {message}", cause=error) from error


def _parse_duration(raw: str, label: str, path: Path) -> DurationNS:
    try:
        return parse_duration(raw.strip())
    except ValueError as error:
        raise _error(f"{path}: {label}: {error}", cause=error) from error


def _field_schema(file_schema: dict[str, _FileFieldDefinition]) -> FieldSchema:
    definitions: dict[str, FieldDefinition] = {}
    for name, raw in file_schema.items():
        try:
            cardinality = Cardinality(raw.cardinality)
        except ValueError:
            cardinality = cast(Cardinality, raw.cardinality)
        definitions[name] = FieldDefinition(
            description=raw.description,
            # Keep the raw enum spelling until the ordinary validation pass so fkf/name
            # retain the same fail-fast precedence as the Go loader.
            cardinality=cardinality,
            relation=raw.relation,
            examples=tuple(raw.examples),
            weight=raw.weight,
        )
    return FieldSchema(definitions)


class _Fail(Protocol):
    def __call__(self, message: str, *, cause: BaseException | None = None) -> ConfigError: ...


def _field_map(raw_fields: dict[str, _FilePathValue], label: str) -> FieldMap:
    mapped: dict[str, FieldPaths] = {}
    for name, raw_paths in raw_fields.items():
        values = [raw_paths] if isinstance(raw_paths, str) else raw_paths
        parsed = []
        for index, value in enumerate(values):
            try:
                parsed.append(parse_field_path(value))
            except ValueError as error:
                suffix = "" if len(values) == 1 else f"[{index}]"
                raise ValueError(f"{label}.{name}{suffix}: {error}") from error
        mapped[name] = FieldPaths(tuple(parsed))
    return FieldMap(mapped)


def _build_source(name: str, raw: _FileSource, path: Path) -> Source:
    def fail(message: str, *, cause: BaseException | None = None) -> ConfigError:
        return _error(f"{path}: sources.{name}: {message}", cause=cause)

    try:
        validate_source_name(name)
    except ValueError as error:
        raise fail(str(error), cause=error) from error
    if raw.run is None or not raw.run:
        raise fail("run is required and must contain an executable")
    if raw.auth is not None and not raw.auth:
        raise fail("auth must contain an executable when declared")
    if raw.test is not None and not raw.test:
        raise fail("test must contain an executable when declared")

    layer = Layer.EVENTS
    if raw.layer.strip():
        if raw.layer.strip() == Layer.TASKS:
            raise fail(
                "layer is 'tasks'; task pages are authored evidence, not a source target; "
                "declare transcript collectors as layer: events with fields"
            )
        try:
            layer = Layer(raw.layer.strip())
        except ValueError as error:
            raise fail(f"layer is {raw.layer!r}; expected events or index", cause=error) from error
        if layer not in {Layer.EVENTS, Layer.INDEX}:
            raise fail(f"layer is {raw.layer!r}; expected events or index")

    output_format = OutputFormat.JSON
    if raw.format.strip():
        try:
            output_format = OutputFormat(raw.format.strip())
        except ValueError as error:
            raise fail(f"format is {raw.format!r}; expected json or ndjson", cause=error) from error

    timeout = DurationNS(0)
    if raw.timeout:
        try:
            timeout = parse_duration(raw.timeout.strip())
        except ValueError as error:
            raise fail(f"timeout: {error}", cause=error) from error
    interval = DurationNS(0)
    if raw.min_interval:
        try:
            interval = parse_duration(raw.min_interval.strip())
        except ValueError as error:
            raise fail(f"min_interval: {error}", cause=error) from error
        if interval <= 0 or interval > MAX_MIN_INTERVAL:
            raise fail(
                f"min_interval is {format_duration(interval)}; expected a positive duration up to "
                f"{format_duration(MAX_MIN_INTERVAL)}"
            )

    retry = _build_retry(raw.retry, fail)
    recency = RecencyPolicy()
    if raw.recency is not None:
        days = raw.recency.half_life_days
        if days < 1 or days > MAX_RECENCY_HALF_LIFE_DAYS:
            raise fail(f"recency.half_life_days is {days}; expected 1..{MAX_RECENCY_HALF_LIFE_DAYS}")
        recency = RecencyPolicy(days)

    try:
        fields = _field_map(raw.fields, "fields")
        records = parse_field_path(raw.records) if raw.records.strip() else None
    except ValueError as error:
        raise fail(str(error), cause=error) from error

    bodies = BodyPolicy.NONE
    if raw.bodies.strip():
        try:
            bodies = BodyPolicy(raw.bodies.strip())
        except ValueError as error:
            raise fail(f"bodies is {raw.bodies!r}; expected none, cache, or sync", cause=error) from error

    return Source(
        name=name,
        enabled=raw.enabled,
        layer=layer,
        max_age_hours=raw.max_age_hours,
        auth=tuple(raw.auth or ()),
        run=tuple(raw.run),
        test=tuple(raw.test or ()),
        format=output_format,
        records=records,
        fields=fields,
        body=tuple(raw.body),
        bodies=bodies,
        recency=recency,
        requires=tuple(raw.requires),
        install=raw.install.strip(),
        timeout=timeout,
        retry=retry,
        min_interval=interval,
        window=raw.window,
    )


def _build_retry(raw: _FileRetry | None, fail: _Fail) -> RetryPolicy:
    if raw is None:
        return RetryPolicy()
    backoff = DurationNS(0)
    if raw.backoff:
        try:
            backoff = parse_duration(raw.backoff.strip())
        except ValueError as error:
            raise fail(f"retry.backoff: {error}", cause=error) from error
        if backoff < 0 or backoff > MAX_RETRY_BACKOFF:
            raise fail(
                f"retry.backoff is {format_duration(backoff)}; expected a duration up to "
                f"{format_duration(MAX_RETRY_BACKOFF)}"
            )
    if raw.attempts == 0 and (backoff != 0 or raw.on):
        raise fail("retry declares backoff or on but no attempts, so nothing would ever be retried")
    if raw.attempts < 0 or raw.attempts > MAX_RETRY_ATTEMPTS:
        raise fail(f"retry.attempts is {raw.attempts}; expected 1..{MAX_RETRY_ATTEMPTS}")
    if raw.attempts > 1 and not raw.on:
        raise fail(
            f"retry.attempts is {raw.attempts} but retry.on is empty; name the failures that may be "
            "retried (`exit:<n>` or a stderr substring) rather than retrying every failure"
        )
    for condition in raw.on:
        if not condition.strip():
            raise fail("retry.on holds an empty condition")
        problem = _execution_text_problem(condition)
        if problem:
            raise fail(f"retry.on {condition!r} {problem}")
        if condition.startswith("exit:"):
            code = condition.removeprefix("exit:").strip()
            if re.fullmatch(r"[+-]?[0-9]+", code) is None or not -(1 << 63) <= int(code) < 1 << 63:
                raise fail(f"retry.on {condition!r}: invalid syntax")
    return RetryPolicy(raw.attempts, backoff, tuple(raw.on))


def _build_config(raw: _FileConfig, path: Path) -> Config:
    layers: dict[Layer, bool] = {}
    for name, enabled in raw.layers.items():
        try:
            layer = Layer(name.strip().lower())
        except ValueError as error:
            raise _error(f"{path}: unknown layer {name!r}", cause=error) from error
        layers[layer] = enabled

    identities: dict[str, Identity] = {}
    for name, declaration in raw.identities.items():
        kind: IdentityKind | None = None
        if declaration.kind.strip():
            try:
                kind = IdentityKind(declaration.kind.strip())
            except ValueError as error:
                raise _error(
                    f"{path}: identities.{name}: kind {declaration.kind!r} must be person, organization, or repository",
                    cause=error,
                ) from error
        identities[name] = Identity(
            canonical=declaration.canonical.strip(),
            aliases=tuple(declaration.aliases or ()),
            kind=kind,
            owner=declaration.owner,
        )

    sources = {name: _build_source(name, source, path) for name, source in raw.sources.items()}
    sync = _build_sync(raw.sync, path)
    return Config(
        fkf=raw.fkf,
        name=raw.name.strip(),
        schema=_field_schema(raw.field_schema),
        layers=layers,
        identities=identities,
        sources=sources,
        sync=sync,
        bin=tuple(raw.bin),
        path=path,
        clients={name: Client(client.url, client.script) for name, client in raw.clients.items()},
    )


def _build_sync(raw: _FileSync | None, path: Path) -> SyncConfig:
    defaults = SyncConfig()
    if raw is None:
        return defaults
    timeout = defaults.timeout
    if raw.timeout is not None:
        timeout = _parse_duration(raw.timeout, "sync.timeout", path)
    return SyncConfig(
        days=defaults.days if raw.days is None else raw.days,
        index_max_age_hours=(
            defaults.index_max_age_hours if raw.index_max_age_hours is None else raw.index_max_age_hours
        ),
        timeout=timeout,
        concurrency=defaults.concurrency if raw.concurrency is None else raw.concurrency,
    )


def _apply_local_overlay(config: Config, raw: _FileLocal, path: Path) -> None:
    config.local_path = path
    if raw.bin:
        config.bin += tuple(raw.bin)
        config.origins["bin"] = path
    for name, override in raw.sources.items():
        source = config.sources.get(name)
        if source is None:
            raise _error(
                f"{path}: sources.{name} is not declared in {CONFIG_FILE_NAME}; "
                "the local overlay may only override a declared source"
            )
        if override.enabled is not None:
            source.enabled = override.enabled
            config.origins[f"sources.{name}.enabled"] = path
        if override.run is not None:
            source.run = tuple(override.run)
            config.origins[f"sources.{name}.run"] = path
        if override.timeout is not None:
            source.timeout = _parse_duration(override.timeout, f"sources.{name}.timeout", path)
            config.origins[f"sources.{name}.timeout"] = path
        if override.max_age_hours is not None:
            source.max_age_hours = override.max_age_hours
            config.origins[f"sources.{name}.max_age_hours"] = path


def load_config(root: str | os.PathLike[str]) -> Config:
    """Read and validate one base's committed config and constrained local overlay."""
    try:
        absolute_root = resolve_absolute_path(root)
    except (OSError, ValueError) as error:
        raise _error(f"resolve base path {os.fspath(root)!r}: {error}", cause=error) from error
    path = absolute_root / CONFIG_FILE_NAME
    data = _read_config_leaf(absolute_root, CONFIG_FILE_NAME)
    if data is None:
        raise _error(f"{path} does not exist; run `fkf init {absolute_root}` to create a base")
    raw = _validate_model(_FileConfig, _decode_strict(data, path), path)
    config = _build_config(raw, path)

    local_path = absolute_root / LOCAL_CONFIG_NAME
    local_data = _read_config_leaf(absolute_root, LOCAL_CONFIG_NAME)
    if local_data is not None:
        local = _validate_model(_FileLocal, _decode_strict(local_data, local_path), local_path)
        _apply_local_overlay(config, local, local_path)
    _validate_config(config)
    return config


def _validate_config(config: Config) -> None:
    path = config.path
    if config.fkf != CONFIG_VERSION:
        raise _error(f"{path}: fkf must be {CONFIG_VERSION}; got {config.fkf}")
    if not config.name:
        raise _error(f"{path}: name is required; it is the MCP server name and the resource authority")
    if _SOURCE_NAME_PATTERN.fullmatch(config.name) is None:
        raise _error(f"{path}: name {config.name!r} must be lowercase letters, digits, and hyphens")
    if len(config.name.encode()) > MAX_BASE_NAME_LENGTH:
        raise _error(f"{path}: name is {len(config.name.encode())} bytes; expected at most {MAX_BASE_NAME_LENGTH}")
    try:
        validate_field_schema(config.schema)
    except ValueError as error:
        raise _error(f"{path}: {error}", cause=error) from error
    _validate_clients(config)
    _validate_identities(config)
    _validate_command_bin(config)
    _validate_sync(config.sync, path)
    for name in config.source_names():
        _validate_source(config, config.sources[name])


def _validate_clients(config: Config) -> None:
    scripts: set[str] = set()
    for name, client in config.clients.items():
        label = f"{config.path}: clients.{name}"
        if _SOURCE_NAME_PATTERN.fullmatch(name) is None or len(name) > MAX_BASE_NAME_LENGTH:
            raise _error(f"{label}: name must be 1..{MAX_BASE_NAME_LENGTH} lowercase letters, digits, or hyphens")
        if re.fullmatch(r"[a-z0-9][a-z0-9_-]*\.py", client.script) is None or len(client.script) > 255:
            raise _error(f"{label}: script must be one Python filename under {BASE_CLIENTS_DIR}/")
        if client.script in scripts:
            raise _error(f"{label}: script {client.script!r} is already associated with another app")
        scripts.add(client.script)
        # URLs describe the app, never credentials, account selectors, or request arguments.
        try:
            url = urlsplit(client.url)
            valid = (
                url.scheme == "https"
                and bool(url.hostname)
                and url.username is None
                and url.password is None
                and not url.query
                and not url.fragment
                and not any(character.isspace() or ord(character) < 32 for character in client.url)
                and not _execution_text_problem(client.url)
                and url.port != 0
            )
        except ValueError:
            valid = False
        if not valid:
            raise _error(f"{label}: url must be an HTTPS app URL without credentials, query, or fragment")


def _validate_sync(sync: SyncConfig, path: Path) -> None:
    if sync.days < 1 or sync.days > 366:
        raise _error(f"{path}: sync.days is {sync.days}; expected 1..366")
    if sync.index_max_age_hours < 1 or sync.index_max_age_hours > MAX_FRESHNESS_AGE_HOURS:
        raise _error(
            f"{path}: sync.index_max_age_hours is {sync.index_max_age_hours}; expected 1..{MAX_FRESHNESS_AGE_HOURS}"
        )
    if sync.concurrency < 1 or sync.concurrency > MAX_SYNC_CONCURRENCY:
        raise _error(f"{path}: sync.concurrency is {sync.concurrency}; expected 1..{MAX_SYNC_CONCURRENCY}")
    if sync.timeout < MIN_SYNC_TIMEOUT or sync.timeout > MAX_COMMAND_TIMEOUT:
        raise _error(f"{path}: sync.timeout is {format_duration(sync.timeout)}; expected 1s..1h")


def _validate_identities(config: Config) -> None:
    claimed: dict[str, str] = {}
    owners = 0
    for name in sorted(config.identities):
        identity = config.identities[name]
        prefix = f"{config.path}: identities.{name}:"
        if _SOURCE_NAME_PATTERN.fullmatch(name) is None or len(name.encode()) > MAX_BASE_NAME_LENGTH:
            raise _error(f"{prefix} name must be lowercase letters, digits, and hyphens and at most 63 bytes")
        if not identity.canonical:
            raise _error(f"{prefix} canonical is required")
        try:
            _validate_entity_uri(identity.canonical)
        except ValueError as error:
            raise _error(f"{prefix} canonical {identity.canonical!r}: {error}", cause=error) from error
        if not identity.aliases:
            raise _error(f"{prefix} aliases must contain at least one exact entity URI, email, or login")
        if identity.owner and identity.effective_kind() is not IdentityKind.PERSON:
            raise _error(f"{prefix} owner may only mark a person identity")
        normalized: list[str] = []
        for index, original in enumerate((identity.canonical, *identity.aliases)):
            value = original.strip()
            if index:
                try:
                    validate_identity_alias(value)
                except ValueError as error:
                    raise _error(f"{prefix} alias {value!r}: {error}", cause=error) from error
                normalized.append(value)
            key = value.lower()
            if owner := claimed.get(key):
                raise _error(f"{prefix} value {value!r} also belongs to identities.{owner}")
            claimed[key] = name
        identity.aliases = tuple(normalized)
        owners += int(identity.owner)
    if owners > 1:
        raise _error(f"{config.path}: at most one identity may be the owner; found {owners}")


def _validate_entity_uri(value: str) -> None:
    validate_entity_uri(value)


def validate_identity_alias(value: str) -> None:
    """Validate the root/authored-page exact alias grammar."""
    if not value or value.strip() != value or len(value.encode()) > 320:
        raise ValueError("must be non-empty, unpadded, and at most 320 bytes")
    if ":" in value:
        _validate_entity_uri(value)
        return
    if _BARE_IDENTITY_ALIAS_PATTERN.fullmatch(value) is None or value.count("@") > 1:
        raise ValueError("must be an entity URI, email, or login without whitespace")
    if "@" in value:
        local, domain = value.split("@", 1)
        if not local or not domain:
            raise ValueError("email aliases need non-empty local and domain parts")


def _validate_source(config: Config, source: Source) -> None:
    def fail(message: str, *, cause: BaseException | None = None) -> ConfigError:
        return _error(f"{config.path}: sources.{source.name}: {message}", cause=cause)

    _validate_argv(config, "run", source.run, RUN_PLACEHOLDERS, fail)
    _validate_argv(config, "auth", source.auth, (), fail, literal=True)
    _validate_argv(config, "test", source.test, TEST_PLACEHOLDERS, fail)
    _validate_requirements(source, fail)
    try:
        validate_field_map(source.fields, event=source.layer is Layer.EVENTS)
    except ValueError as error:
        raise fail(str(error)) from error
    for name in source.fields.names():
        if name not in config.schema:
            raise fail(f"fields.{name} is not declared in schema")
    source.schema = config.schema.select(source.fields)
    for name in source.body_field_names():
        if source.schema[name].cardinality not in {Cardinality.ONE, Cardinality.OPTIONAL}:
            raise fail(f"body placeholder {{{{{name}}}}} needs schema.{name}.cardinality one or optional")
    _validate_source_policy(config, source, fail)
    _validate_body(config, source, fail)
    if FIELD_TITLE not in source.fields:
        raise fail("fields.title is required: every collected record needs a meaningful subject line")


def _validate_argv(
    config: Config,
    label: str,
    argv: tuple[str, ...],
    allowed: tuple[str, ...],
    fail: _Fail,
    *,
    literal: bool = False,
) -> None:
    if label == "run" and not argv:
        raise fail("run is required and must contain an executable")
    for index, argument in enumerate(argv):
        problem = _execution_text_problem(argument)
        if problem:
            raise fail(f"{label}[{index}] {problem}")
        try:
            names = _placeholder_names(argument)
        except ValueError as error:
            raise fail(f"{label}[{index}]: {error}", cause=error) from error
        if literal and names:
            raise fail(f"{label}[{index}] must be literal; auth probes accept no placeholders")
        if index == 0:
            if not argument.strip():
                raise fail(f"{label}[0] must be the executable to run")
            if names:
                raise fail(f"{label}[0] must be a literal executable; placeholders are allowed only in arguments")
            _validate_argv_executable(config, label, argument, fail)
        for name in names:
            if name not in allowed:
                raise fail(
                    f"{label}[{index}]: unknown placeholder {{{{{name}}}}}; known placeholders are {', '.join(allowed)}"
                )


def _validate_requirements(source: Source, fail: _Fail) -> None:
    seen: set[str] = set()
    for index, requirement in enumerate(source.requires):
        if _REQUIREMENT_PATTERN.fullmatch(requirement) is None:
            raise fail(f"requires[{index}] is {requirement!r}; expected a bare executable name resolved through PATH")
        if requirement in seen:
            raise fail(f"requires names {requirement!r} more than once")
        seen.add(requirement)


def _validate_source_policy(config: Config, source: Source, fail: _Fail) -> None:
    if source.max_age_hours is not None:
        if source.layer is not Layer.INDEX:
            raise fail("max_age_hours is valid only for an index source")
        if source.max_age_hours < 1 or source.max_age_hours > MAX_FRESHNESS_AGE_HOURS:
            raise fail(f"max_age_hours is {source.max_age_hours}; expected 1..{MAX_FRESHNESS_AGE_HOURS}")
    if source.bodies is not BodyPolicy.NONE and not source.has_body():
        raise fail(f"bodies is {source.bodies} but body is not declared; caching cannot fetch this source")
    if source.window and source.layer is not Layer.EVENTS:
        raise fail(
            f"window is true but layer is {source.layer}; only events support it because a whole-range collection buckets by day"
        )
    if source.timeout < 0 or source.timeout > MAX_COMMAND_TIMEOUT:
        raise fail(f"timeout is {format_duration(source.timeout)}; expected 0 (inherit sync.timeout) up to 1h")
    if source.enabled and not config.layers.get(source.layer, False):
        raise fail(f"is enabled but layers.{source.layer} is false; enable the layer or disable the source")


def _validate_body(config: Config, source: Source, fail: _Fail) -> None:
    if not source.body:
        return
    if len(source.body) < 2:
        raise fail("body must contain an executable and at least one argument")
    saw_id = False
    for index, element in enumerate(source.body):
        problem = _execution_text_problem(element)
        if problem:
            raise fail(f"body[{index}] {problem}")
        try:
            names = _placeholder_names(element)
        except ValueError as error:
            raise fail(f"body[{index}]: {error}", cause=error) from error
        for name in names:
            field_name = name in source.fields
            static = name in _BODY_STATIC_PLACEHOLDERS
            if not field_name and not static:
                raise fail(
                    f"body[{index}]: unknown placeholder {{{{{name}}}}}; declare fields.{name} or use base or home"
                )
            if index == 0:
                if field_name and not static:
                    raise fail(
                        f"body[0] must not use collected placeholder {{{{{name}}}}}; the executable is trusted configuration"
                    )
                raise fail("body[0] must be a literal executable; placeholders are allowed only in arguments")
            saw_id = saw_id or name == FIELD_ID
        if index == 0:
            _validate_argv_executable(config, "body", element, fail)
    if not saw_id:
        raise fail("body must name {{id}}: it fetches exactly one record")
    if not source.body[0].strip():
        raise fail("body[0] must be the executable to run")


def _validate_command_bin(config: Config) -> None:
    for index, declared in enumerate(config.bin):
        if problem := _execution_text_problem(declared):
            raise _error(f"{config.path}: bin[{index}] {problem}")
        problem = _machine_local_path_problem(config, declared)
        if problem:
            raise _error(f"{config.path}: bin[{index}] {problem}")


def _machine_local_path_problem(config: Config, declared: str) -> str:
    expanded = Path(os.path.normpath(expand_home(declared.strip())))
    if not declared.strip() or not expanded.is_absolute():
        return "must be an absolute or ~-relative directory outside the base"
    root = config.path.parent.absolute()
    absolute = expanded.absolute()
    if _path_is_within(root, absolute):
        return (
            f"value {declared!r} resolves inside the base; put base-controlled executables in sources/ "
            "and keep extra PATH directories outside the base"
        )
    try:
        resolved_root = resolve_physical_path(root)
        resolved = resolve_physical_path(absolute)
    except OSError as error:
        raise _error(f"{config.path}: cannot inspect {declared!r}: {error}", cause=error) from error
    if _path_is_within(resolved_root, resolved):
        return (
            f"value {declared!r} resolves inside the base through a symlink; put base-controlled executables in sources/ "
            "and keep extra PATH directories outside the base"
        )
    return ""


def _path_is_within(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
    except ValueError:
        return False
    return True


def _validate_argv_executable(config: Config, label: str, executable: str, fail: _Fail) -> None:
    if os.sep not in executable:
        if _REQUIREMENT_PATTERN.fullmatch(executable) is None:
            raise fail(f"{label}[0] must be a bare executable name or an absolute machine-local path outside the base")
        return
    if not Path(executable).is_absolute():
        raise fail(
            f"{label}[0] must be a bare executable resolved on PATH or an absolute machine-local path outside the base"
        )
    problem = _machine_local_path_problem(config, executable)
    if problem:
        tree = BASE_TESTS_DIR if label == "test" else BASE_SOURCES_DIR
        raise fail(
            f"{label}[0] must resolve outside the base; put base-controlled code in {tree}/ and name it without a path"
        )


def _placeholder_names(value: str) -> tuple[str, ...]:
    stripped = _PLACEHOLDER_PATTERN.sub("", value)
    if "{{" in stripped or "}}" in stripped:
        index = stripped.find("{{")
        closing = stripped.find("}}")
        if index < 0 or (closing >= 0 and closing < index):
            index = closing
        nearby = stripped[index : index + 24]
        raise ValueError(f"malformed placeholder near {nearby!r}; use {{{{name}}}} in lowercase")
    return tuple(match.group(1) for match in _PLACEHOLDER_PATTERN.finditer(value))


def _execution_text_problem(value: str) -> str:
    try:
        value.encode("utf-8")
    except UnicodeEncodeError:
        return "is not valid UTF-8"
    for character in value:
        category = unicodedata.category(character)
        if category == "Cc":
            return f"contains control character U+{ord(character):04X}"
        if category == "Cf":
            return f"contains invisible format character U+{ord(character):04X}"
    return ""


def validate_source_name(name: str) -> None:
    """Keep one source name usable as both a filename and URI segment."""
    if _SOURCE_NAME_PATTERN.fullmatch(name) is None:
        raise ValueError("name must be lowercase letters, digits, and hyphens (for example github-pull-requests)")
    if len(name.encode()) > MAX_SOURCE_NAME_LENGTH:
        raise ValueError(
            f"name is {len(name.encode())} bytes; expected at most {MAX_SOURCE_NAME_LENGTH} bytes "
            "so <source>.json fits in one filename"
        )


def valid_body_value(value: str) -> bool:
    """Return whether collected text is safe as one opaque, non-option argv value."""
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    if not value or len(encoded) > 256 or not value.strip() or value[0] in {"-", "@"}:
        return False
    return not _execution_text_problem(value)


__all__ = [
    "CONFIG_VERSION",
    "MAX_BASE_NAME_LENGTH",
    "MAX_COMMAND_TIMEOUT",
    "MAX_FRESHNESS_AGE_HOURS",
    "MAX_MIN_INTERVAL",
    "MAX_RECENCY_HALF_LIFE_DAYS",
    "MAX_RETRY_ATTEMPTS",
    "MAX_RETRY_BACKOFF",
    "MAX_SOURCE_NAME_LENGTH",
    "MAX_SYNC_CONCURRENCY",
    "RUN_PLACEHOLDERS",
    "TEST_PLACEHOLDERS",
    "BodyPolicy",
    "Client",
    "Config",
    "ConfigError",
    "Identity",
    "IdentityKind",
    "Layer",
    "OutputFormat",
    "RecencyPolicy",
    "RetryPolicy",
    "Source",
    "SyncConfig",
    "decode_strict_yaml",
    "load_config",
    "valid_body_value",
    "validate_identity_alias",
    "validate_source_name",
]
