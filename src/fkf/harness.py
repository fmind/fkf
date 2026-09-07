"""Generated, conflict-safe integrations for the closed coding-harness registry."""

from __future__ import annotations

import os
import re
import shutil
import stat
import tempfile
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Final, Protocol, cast
from urllib.parse import quote, unquote

from fkf.config import load_config
from fkf.errors import InvalidUsageError, OperationalError
from fkf.io import atomic_write, read_file_limited
from fkf.jsoncodec import dumps as dump_json
from fkf.jsoncodec import loads as load_json
from fkf.process import Cancellation, check_cancel
from fkf.store import BASE_FILE_MODE, MAX_CONTROL_FILE_BYTES, resolve_physical_path


class LauncherResolver(Protocol):
    """Resolve a persistent FKF console launcher without executing it."""

    def __call__(self, requested: str, search_path: str, /) -> Path: ...


class FragmentKind(StrEnum):
    JSON = "json"
    TOML = "toml"


class HarnessConflictError(InvalidUsageError):
    """A harness-owned selector contains bytes FKF cannot safely replace."""


HARNESSES: Final = (
    "claude",
    "codex",
    "gemini",
    "copilot",
    "antigravity",
    "opencode",
    "grok",
    "cursor",
    "kiro",
    "cline",
)
MANAGED_START: Final = "# >>> fkf harness "
MANAGED_END: Final = "# <<< fkf harness "
BACKUP_SUFFIX: Final = ".fkf.bak"
MAX_SCHEDULED_PATH_BYTES: Final = 16 << 10
_UNSTABLE_COMPONENTS: Final = frozenset({".cache", "cache", "caches", "uvx", ".venv", "venv"})


@dataclass(frozen=True, slots=True)
class HarnessFragment:
    path: str
    kind: FragmentKind
    content: str
    selector: str = field(default="", metadata={"json": "selector,omitempty"})
    value: object = field(default=None, repr=False, compare=False, metadata={"json": "-"})
    array: bool = field(default=False, repr=False, compare=False, metadata={"json": "-"})
    managed_kind: str = field(default="", repr=False, compare=False, metadata={"json": "-"})
    managed_base: str = field(default="", repr=False, compare=False, metadata={"json": "-"})
    managed_key: str = field(default="", repr=False, compare=False, metadata={"json": "-"})
    workspace: str = field(default="", repr=False, compare=False, metadata={"json": "-"})
    mode: int = field(default=BASE_FILE_MODE, repr=False, compare=False, metadata={"json": "-"})


@dataclass(frozen=True, slots=True)
class HarnessPlan:
    name: str
    base: Path
    base_name: str
    fragments: tuple[HarnessFragment, ...]
    workspace: Path | None = field(default=None, metadata={"json": "workspace,omitempty"})
    notes: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class HarnessInstallRequest:
    names: tuple[str, ...] = ()
    all: bool = False
    dry_run: bool = False
    check: bool = False
    home: Path | str = ""
    executable: Path | str = ""
    workspace: Path | str = ""
    path: str = ""


@dataclass(frozen=True, slots=True)
class HarnessChange:
    harness: str
    action: str
    path: Path
    backup: Path | None = None


@dataclass(frozen=True, slots=True)
class HarnessInstallReport:
    base: Path
    base_name: str
    mode: str
    harnesses: tuple[str, ...]
    complete: bool
    changes: tuple[HarnessChange, ...]
    workspace: Path | None = None


@dataclass(frozen=True, slots=True)
class HarnessRegistration:
    name: str
    registered: bool
    changes: int = 0
    error: str = ""
    manual_cleanup: tuple[str, ...] = ()


@dataclass(slots=True)
class _Mutation:
    harness: str
    path: Path
    before: bytes
    after: bytes
    before_mode: int
    mode: int
    exists: bool
    changed: bool
    action: str


@dataclass(slots=True)
class _Group:
    harness: str
    path: Path
    fragments: list[HarnessFragment]


def harness_names() -> tuple[str, ...]:
    """Return the closed registry in stable CLI display order."""

    return HARNESSES


def _is_within(parent: Path, child: Path) -> bool:
    try:
        child.relative_to(parent)
    except ValueError:
        return False
    return True


def _unstable_launcher(path: Path, *, implicit: bool) -> str:
    if not implicit:
        return ""
    candidates = (path, path.resolve(strict=False))
    temporary_roots = (Path(tempfile.gettempdir()).resolve(strict=False), Path(os.sep, "var", "tmp"))
    for candidate in candidates:
        folded = tuple(part.casefold() for part in candidate.parts)
        if any(_is_within(root, candidate) for root in temporary_roots):
            return "temporary"
        if any(part in {".cache", "cache", "caches", "uvx"} or part.startswith("uvx-") for part in folded):
            return "cache or uvx"
        if implicit and any(part in {".venv", "venv"} for part in folded):
            return "project virtual environment"
    return ""


def resolve_persistent_launcher(requested: str, search_path: str) -> Path:
    """Resolve an executable suitable for configuration that outlives this process."""

    raw = requested.strip()
    implicit = not raw
    name = raw or "fkf"
    if any(character in name for character in "\x00\r\n"):
        raise InvalidUsageError("persistent FKF executable contains a control character")
    if os.sep in name:
        candidate = Path(name).expanduser()
        if not candidate.is_absolute():
            raise InvalidUsageError("--executable must be absolute or a bare name resolved on PATH")
        candidate = Path(os.path.normpath(candidate))
    else:
        resolved = shutil.which(name, path=search_path or None)
        if not resolved:
            raise InvalidUsageError(
                "no stable fkf console launcher was found; install one with `uv tool install fkf` "
                "or pass --executable /absolute/path/to/fkf"
            )
        candidate = Path(os.path.normpath(resolved)).absolute()
    unstable = _unstable_launcher(candidate, implicit=implicit)
    if unstable:
        raise InvalidUsageError(
            f"persistent FKF executable {candidate} is in a {unstable} location; "
            "install a stable launcher with `uv tool install fkf`"
        )
    try:
        info = candidate.stat()
    except OSError as error:
        raise InvalidUsageError(f"persistent FKF executable {candidate} cannot be inspected: {error}") from error
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o111 == 0:
        raise InvalidUsageError(f"persistent FKF executable {candidate} is not an executable regular file")
    return candidate


def _safe_path(value: Path | str, label: str) -> Path:
    rendered = os.fspath(value)
    if not rendered or not Path(rendered).is_absolute():
        raise InvalidUsageError(f"{label} must be an absolute path")
    if any(character in rendered for character in "\x00\r\n"):
        raise InvalidUsageError(f"{label} may not contain NUL or newlines")
    if len(rendered.encode()) > MAX_SCHEDULED_PATH_BYTES:
        raise InvalidUsageError(f"{label} exceeds {MAX_SCHEDULED_PATH_BYTES} bytes")
    return Path(os.path.normpath(rendered))


def _harness_base(root: Path | str) -> tuple[Path, str]:
    selected = _safe_path(root, "harness base path")
    try:
        physical = resolve_physical_path(selected)
        config = load_config(physical)
    except (OSError, ValueError) as error:
        raise InvalidUsageError(f"load harness base identity: {error}") from error
    return physical, config.name


def _workspace(value: Path | str) -> Path | None:
    if not os.fspath(value):
        return None
    selected = _safe_path(value, "harness workspace")
    try:
        physical = resolve_physical_path(selected)
        info = physical.stat()
    except OSError as error:
        raise OperationalError(f"inspect harness workspace: {error}") from error
    if not stat.S_ISDIR(info.st_mode):
        raise InvalidUsageError("harness workspace must be a directory")
    return physical


def _home(value: Path | str) -> Path:
    selected = os.fspath(value) or os.fspath(Path.home())
    return _safe_path(selected, "harness home path")


def _launcher(
    executable: Path | str,
    path: str,
    resolver: LauncherResolver | None,
) -> Path:
    selected = resolver or resolve_persistent_launcher
    return _safe_path(selected(os.fspath(executable), path), "persistent FKF executable")


def harness_plan_for(
    base_root: Path | str,
    name: str,
    *,
    executable: Path | str = "",
    workspace: Path | str = "",
    path: str = "",
    launcher_resolver: LauncherResolver | None = resolve_persistent_launcher,
) -> HarnessPlan:
    """Render one harness's fragments from one canonical base and launcher."""

    root, base_name = _harness_base(base_root)
    if name not in HARNESSES:
        raise InvalidUsageError(f"unknown harness {name!r}; expected {', '.join(HARNESSES)}")
    launcher = _launcher(executable, path, launcher_resolver)
    selected_workspace = _workspace(workspace)
    return _build_plan(root, base_name, name, launcher, selected_workspace)


def _registration_key(base_name: str) -> str:
    return f"fkf-{base_name}"


def _json_fragment(
    path: str,
    selector: str,
    value: object,
    *,
    array: bool = False,
    managed_kind: str,
    base: Path,
    key: str,
    workspace: Path | None = None,
) -> HarnessFragment:
    return HarnessFragment(
        path=path,
        kind=FragmentKind.JSON,
        selector=selector,
        content=dump_json(value, indent=True).decode(),
        value=value,
        array=array,
        managed_kind=managed_kind,
        managed_base=os.fspath(base),
        managed_key=key,
        workspace=os.fspath(workspace) if workspace else "",
    )


def _toml_string(value: str) -> str:
    return dump_json(value).decode()


def _managed_toml(name: str, key: str, content: str) -> str:
    marker = f"{name} {key}"
    return f"{MANAGED_START}{marker}\n{content.strip()}\n{MANAGED_END}{marker}\n"


def _toml_fragment(
    path: str,
    content: str,
    *,
    base: Path,
    key: str,
    workspace: Path | None = None,
) -> HarnessFragment:
    return HarnessFragment(
        path=path,
        kind=FragmentKind.TOML,
        content=content,
        managed_base=os.fspath(base),
        managed_key=key,
        workspace=os.fspath(workspace) if workspace else "",
    )


def _hook_group(command: str, timeout: int) -> dict[str, object]:
    return {
        "matcher": "startup|compact",
        "hooks": [
            {
                "type": "command",
                "command": command,
                "timeout": timeout,
                "statusMessage": "Loading FKF context",
            }
        ],
    }


def _shell_quote(value: str) -> str:
    return "'" + value.replace("'", "'\\''") + "'"


def _empty_hook_command(harness: str) -> str:
    if harness in {"claude", "opencode", "grok", "kiro"}:
        return ":"
    if harness == "cline":
        return "printf '%s\\n' '{\"cancel\":false}'"
    return "printf '%s\\n' '{}'"


def _guarded_hook(base: Path, key: str, workspace: Path | None, hook: Path, harness: str, executable: Path) -> str:
    if workspace is None:
        return ""
    marker = (
        f": fkf-key={quote(key, safe='')} fkf-base={quote(os.fspath(base), safe='')} "
        f"fkf-workspace={quote(os.fspath(workspace), safe='')}"
    )
    check = (
        f"{_shell_quote(os.fspath(executable))} trust --check --base {_shell_quote(os.fspath(base))} >/dev/null 2>&1"
    )
    # Sanitize PATH before the env-python3 shebang resolves its interpreter, not only inside it.
    dispatch = (
        f"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin "
        f"{_shell_quote(os.fspath(hook))} {_shell_quote(harness)} {_shell_quote(os.fspath(executable))} "
        f"{_shell_quote(os.fspath(workspace))}"
    )
    return f"{marker}; {check} && {dispatch} || {_empty_hook_command(harness)}"


def _mcp(executable: Path, base: Path) -> dict[str, object]:
    return {"command": os.fspath(executable), "args": ["mcp", "serve", "--base", os.fspath(base)]}


def _build_plan(
    base: Path,
    base_name: str,
    name: str,
    executable: Path,
    workspace: Path | None,
) -> HarnessPlan:
    key = _registration_key(base_name)
    hook_command = _guarded_hook(base, key, workspace, base / "bin" / "fkf-hook.py", name, executable)
    stdio = _mcp(executable, base)
    fragments: list[HarnessFragment] = []
    notes: list[str] = []
    if name == "claude":
        value = {**stdio, "type": "stdio", "env": {}}
        fragments.append(
            _json_fragment("~/.claude.json", f"mcpServers.{key}", value, managed_kind="mcp", base=base, key=key)
        )
        if workspace:
            fragments.append(
                _json_fragment(
                    "~/.claude/settings.json",
                    "hooks.SessionStart",
                    _hook_group(hook_command, 20),
                    array=True,
                    managed_kind="hook",
                    base=base,
                    key=key,
                    workspace=workspace,
                )
            )
    elif name == "codex":
        lines = [
            f"# base: {_toml_string(os.fspath(base))}",
            f"[mcp_servers.{key}]",
            f"command = {_toml_string(os.fspath(executable))}",
            f'args = ["mcp", "serve", "--base", {_toml_string(os.fspath(base))}]',
        ]
        if workspace:
            lines.extend(
                (
                    "",
                    "[[hooks.SessionStart]]",
                    'matcher = "startup|compact"',
                    "",
                    "[[hooks.SessionStart.hooks]]",
                    'type = "command"',
                    f"command = {_toml_string(hook_command)}",
                    "timeout = 20",
                    'statusMessage = "Loading FKF context"',
                )
            )
        fragments.append(
            _toml_fragment(
                "~/.codex/config.toml",
                _managed_toml(name, key, "\n".join(lines)),
                base=base,
                key=key,
                workspace=workspace,
            )
        )
    elif name == "gemini":
        fragments.append(
            _json_fragment(
                "~/.gemini/settings.json", f"mcpServers.{key}", stdio, managed_kind="mcp", base=base, key=key
            )
        )
        if workspace:
            fragments.append(
                _json_fragment(
                    "~/.gemini/settings.json",
                    "hooks.SessionStart",
                    _hook_group(hook_command, 20_000),
                    array=True,
                    managed_kind="hook",
                    base=base,
                    key=key,
                    workspace=workspace,
                )
            )
    elif name == "copilot":
        value = {**stdio, "type": "local", "tools": ["*"]}
        fragments.append(
            _json_fragment(
                "~/.copilot/mcp-config.json", f"mcpServers.{key}", value, managed_kind="mcp", base=base, key=key
            )
        )
        notes.append("Copilot CLI ignores command output from sessionStart; this adapter is MCP-only.")
    elif name == "antigravity":
        fragments.append(
            _json_fragment(
                "~/.gemini/config/mcp_config.json", f"mcpServers.{key}", stdio, managed_kind="mcp", base=base, key=key
            )
        )
        notes.append("Antigravity ignores PreInvocation output; this adapter is MCP-only.")
    elif name == "opencode":
        value = {
            "type": "local",
            "command": [os.fspath(executable), "mcp", "serve", "--base", os.fspath(base)],
            "enabled": True,
        }
        fragments.append(
            _json_fragment(
                "~/.config/opencode/opencode.json", f"mcp.{key}", value, managed_kind="mcp", base=base, key=key
            )
        )
        notes.append("OpenCode has no stable passive session-context transform; this adapter is MCP-only.")
    elif name == "grok":
        content = "\n".join(
            (
                f"# base: {_toml_string(os.fspath(base))}",
                f"[mcp_servers.{key}]",
                f"command = {_toml_string(os.fspath(executable))}",
                f'args = ["mcp", "serve", "--base", {_toml_string(os.fspath(base))}]',
                "enabled = true",
            )
        )
        fragments.append(_toml_fragment("~/.grok/config.toml", _managed_toml(name, key, content), base=base, key=key))
        notes.append("Grok ignores passive SessionStart output; this adapter is MCP-only.")
    elif name == "cursor":
        fragments.append(
            _json_fragment("~/.cursor/mcp.json", f"mcpServers.{key}", stdio, managed_kind="mcp", base=base, key=key)
        )
        notes.append("Cursor has no verified per-base user hook contract; this adapter is MCP-only.")
    elif name == "kiro":
        value = {**stdio, "disabled": False, "autoApprove": []}
        fragments.append(
            _json_fragment(
                "~/.kiro/settings/mcp.json", f"mcpServers.{key}", value, managed_kind="mcp", base=base, key=key
            )
        )
        if workspace:
            path = f"~/.kiro/hooks/{key}.json"
            fragments.extend(
                (
                    _json_fragment(
                        path, "version", "v1", managed_kind="scalar", base=base, key=key, workspace=workspace
                    ),
                    _json_fragment(
                        path,
                        "hooks",
                        {
                            "name": "FKF context",
                            "trigger": "SessionStart",
                            "action": {"type": "command", "command": hook_command},
                            "timeout": 20,
                            "enabled": True,
                        },
                        array=True,
                        managed_kind="hook",
                        base=base,
                        key=key,
                        workspace=workspace,
                    ),
                )
            )
    elif name == "cline":
        fragments.append(
            _json_fragment(
                "~/.cline/data/settings/cline_mcp_settings.json",
                f"mcpServers.{key}",
                stdio,
                managed_kind="mcp",
                base=base,
                key=key,
            )
        )
        notes.append("Cline has one global TaskStart filename; this adapter is MCP-only.")
    notes.append(
        "Skills remain base-local. Install neutral shared FKF skills separately if the harness needs user-scope discovery."
    )
    return HarnessPlan(name, base, base_name, tuple(fragments), workspace, tuple(notes))


def _select(request: HarnessInstallRequest) -> tuple[str, ...]:
    if request.check and request.dry_run:
        raise InvalidUsageError("--check and --dry-run cannot be combined")
    if request.all and request.names:
        raise InvalidUsageError("--all cannot be combined with harness names")
    if not request.all and not request.names:
        raise InvalidUsageError("select one or more harness names, or use --all")
    names = HARNESSES if request.all else request.names
    seen: set[str] = set()
    for name in names:
        if name not in HARNESSES:
            raise InvalidUsageError(f"unknown harness {name!r}; expected {', '.join(HARNESSES)}")
        if name in seen:
            raise InvalidUsageError(f"harness {name!r} is selected more than once")
        seen.add(name)
    return tuple(names)


def _validate_assets(base: Path, needs_hook: bool) -> None:
    if not needs_hook:
        return
    hook = base / "bin" / "fkf-hook.py"
    try:
        info = hook.lstat()
    except OSError as error:
        raise InvalidUsageError(f"inspect harness hook {hook}: {error}") from error
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_mode & 0o111 == 0:
        raise InvalidUsageError(f"harness hook {hook} is not an executable non-symlink regular file")


def _target(home: Path, declared: str) -> Path:
    if not declared.startswith("~/"):
        raise InvalidUsageError(f"harness target {declared!r} is not home-relative")
    relative = Path(*declared.removeprefix("~/").split("/"))
    if relative.is_absolute() or ".." in relative.parts or relative == Path():
        raise InvalidUsageError(f"harness target {declared!r} escapes home")
    target = home / relative
    if not _is_within(home, target):
        raise InvalidUsageError(f"harness target {declared!r} escapes home")
    return target


def _groups(home: Path, plans: Sequence[HarnessPlan]) -> list[_Group]:
    groups: list[_Group] = []
    indexes: dict[Path, int] = {}
    for plan in plans:
        for fragment in plan.fragments:
            target = _target(home, fragment.path)
            if target in indexes:
                groups[indexes[target]].fragments.append(fragment)
            else:
                indexes[target] = len(groups)
                groups.append(_Group(plan.name, target, [fragment]))
    return groups


def _visit_strings(value: object, visit: Callable[[str], None]) -> None:
    if isinstance(value, str):
        visit(value)
    elif isinstance(value, list):
        for child in value:
            _visit_strings(child, visit)
    elif isinstance(value, Mapping):
        for child in value.values():
            _visit_strings(child, visit)


def _marker_value(value: object, name: str) -> str:
    prefix = f"{name}="
    found = ""

    def inspect(text: str) -> None:
        nonlocal found
        if found:
            return
        index = text.find(prefix)
        if index < 0:
            return
        encoded = text[index + len(prefix) :]
        stop = min((position for marker in " ;\t\r\n" if (position := encoded.find(marker)) >= 0), default=len(encoded))
        found = unquote(encoded[:stop])

    _visit_strings(value, inspect)
    return found


def _workspace_overlap(left: str, right: str) -> bool:
    if not left or not right:
        return False
    first, second = Path(left), Path(right)
    return _is_within(first, second) or _is_within(second, first)


def _check_workspace_conflict(path: Path, value: object, fragment: HarnessFragment) -> None:
    conflict = ""

    def inspect(command: str) -> None:
        nonlocal conflict
        if conflict or "fkf-hook.py" not in command or _marker_value(command, "fkf-base") == fragment.managed_base:
            return
        workspace = _marker_value(command, "fkf-workspace")
        if _workspace_overlap(workspace, fragment.workspace):
            conflict = workspace

    _visit_strings(value, inspect)
    if conflict:
        raise HarnessConflictError(f"{path} already has an overlapping FKF hook workspace {conflict}")


def _argv_has_mcp(value: object) -> bool:
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        return False
    argv = value[1:] if len(value) >= 5 and _is_fkf_executable(value[0]) else value
    return len(argv) == 4 and argv[:3] == ["mcp", "serve", "--base"]


def _is_fkf_executable(value: object) -> bool:
    return isinstance(value, str) and (value == "fkf" or Path(value).is_absolute())


def _find_hook(value: object, harness: str = "") -> bool:
    found = False

    def inspect(text: str) -> None:
        nonlocal found
        if "fkf-hook.py" in text and (not harness or f" {harness}" in text):
            found = True

    _visit_strings(value, inspect)
    return found


def _json_managed(value: object, kind: str, harness: str = "") -> bool:
    if kind == "hook":
        return _find_hook(value, harness)
    if kind != "mcp" or not isinstance(value, Mapping):
        return False
    command = value.get("command")
    if isinstance(command, str) and _is_fkf_executable(command):
        return _argv_has_mcp(value.get("args"))
    return _argv_has_mcp(command)


def _mcp_base(value: object) -> str:
    if not isinstance(value, Mapping):
        return ""
    argv = value.get("args")
    command = value.get("command")
    if isinstance(command, list):
        argv = command[1:] if command and _is_fkf_executable(command[0]) else command
    if not isinstance(argv, list):
        return ""
    for index, item in enumerate(argv[:-1]):
        if item == "--base" and isinstance(argv[index + 1], str):
            return argv[index + 1]
    return ""


def _owned(value: object, fragment: HarnessFragment) -> bool:
    if not _json_managed(value, fragment.managed_kind):
        return False
    if fragment.managed_kind == "mcp":
        return _mcp_base(value) == fragment.managed_base
    if fragment.managed_kind == "hook":
        return _marker_value(value, "fkf-base") == fragment.managed_base
    return False


def _apply_json_fragment(path: Path, root: dict[str, object], fragment: HarnessFragment) -> None:
    parts = fragment.selector.split(".")
    parent = root
    for part in parts[:-1]:
        existing = parent.get(part)
        if existing is None:
            child: dict[str, object] = {}
            parent[part] = child
            parent = child
        elif isinstance(existing, dict):
            parent = existing
        else:
            raise HarnessConflictError(f"{path} defines {part} as a non-object")
    key = parts[-1]
    exists = key in parent
    existing = parent.get(key)
    if fragment.array:
        if exists and not isinstance(existing, list):
            raise HarnessConflictError(f"{path} defines {fragment.selector} as a non-array")
        entries = list(existing) if isinstance(existing, list) else []
        if fragment.managed_kind == "hook":
            _check_workspace_conflict(path, entries, fragment)
        for index, entry in enumerate(entries):
            if entry == fragment.value:
                return
            if _owned(entry, fragment):
                entries[index] = fragment.value
                parent[key] = entries
                return
        entries.append(fragment.value)
        parent[key] = entries
        return
    if not exists:
        parent[key] = fragment.value
    elif existing == fragment.value:
        return
    elif _owned(existing, fragment):
        parent[key] = fragment.value
    else:
        raise HarnessConflictError(f"{path} already defines {fragment.selector} and FKF does not own it")


def _merge_json(path: Path, before: bytes, fragments: Sequence[HarnessFragment]) -> bytes:
    if before.strip():
        try:
            decoded = load_json(before)
        except ValueError as error:
            raise HarnessConflictError(f"decode harness config {path}: {error}") from error
        if not isinstance(decoded, dict):
            raise HarnessConflictError(f"decode harness config {path}: root must be an object")
        root = cast(dict[str, object], decoded)
    else:
        root = {}
    semantic_before = dump_json(root)
    for fragment in fragments:
        _apply_json_fragment(path, root, fragment)
    semantic_after = dump_json(root)
    if semantic_before == semantic_after:
        return before
    return dump_json(root, indent=True, newline=True)


def _marker_line(text: str, marker: str) -> int:
    match = re.search(rf"(?m)^{re.escape(marker)}\r?$", text)
    return -1 if match is None else match.start()


def _merge_toml(path: Path, harness: str, before: bytes, fragment: HarnessFragment) -> bytes:
    try:
        text = before.decode()
    except UnicodeDecodeError as error:
        raise HarnessConflictError(f"decode harness config {path}: TOML is not UTF-8") from error
    marker = f"{harness} {fragment.managed_key}"
    start_marker = f"{MANAGED_START}{marker}"
    end_marker = f"{MANAGED_END}{marker}"
    start, end = _marker_line(text, start_marker), _marker_line(text, end_marker)
    if (start >= 0) != (end >= 0) or (start >= 0 and end < start):
        raise HarnessConflictError(f"{path} has an incomplete FKF managed block")
    desired = fragment.content
    if fragment.workspace:
        for line in text.splitlines():
            _check_workspace_conflict(path, line, fragment)
    if start >= 0:
        end += len(end_marker)
        block = text[start:end]
        if f"# base: {_toml_string(fragment.managed_base)}" not in block:
            raise HarnessConflictError(f"{path} already owns {fragment.managed_key} for a different base")
        if end < len(text) and text[end] == "\r":
            end += 1
        if end < len(text) and text[end] == "\n":
            end += 1
        if not fragment.workspace:
            hook = block.find("\n[[hooks.SessionStart]]")
            if hook >= 0:
                desired = desired.removesuffix(end_marker + "\n") + block[hook:] + "\n"
        return (text[:start] + desired + text[end:]).encode()
    section = re.compile(rf"(?m)^\s*\[mcp_servers\.{re.escape(fragment.managed_key)}\]\s*(?:#.*)?$")
    if section.search(text) or f"fkf-key={quote(fragment.managed_key, safe='')}" in text:
        raise HarnessConflictError(f"{path} already defines an FKF MCP server or hook outside a managed block")
    if text and not text.endswith("\n"):
        text += "\n"
    if text.strip():
        text += "\n"
    return (text + desired).encode()


def _preflight_file(group: _Group) -> _Mutation:
    exists = False
    before = b""
    before_mode = 0
    mode = group.fragments[0].mode
    try:
        info = group.path.lstat()
    except FileNotFoundError:
        pass
    except OSError as error:
        raise HarnessConflictError(f"inspect harness config {group.path}: {error}") from error
    else:
        if stat.S_ISLNK(info.st_mode):
            raise HarnessConflictError(f"harness config {group.path} is a symlink")
        if not stat.S_ISREG(info.st_mode):
            raise HarnessConflictError(f"harness config {group.path} is not a regular file")
        exists = True
        before_mode = stat.S_IMODE(info.st_mode)
        mode = before_mode
        before = read_file_limited(group.path, MAX_CONTROL_FILE_BYTES)
    kinds = {fragment.kind for fragment in group.fragments}
    if len(kinds) != 1:
        raise RuntimeError(f"internal harness plan mixes formats for {group.path}")
    kind = group.fragments[0].kind
    if kind is FragmentKind.JSON:
        after = _merge_json(group.path, before, group.fragments)
    elif kind is FragmentKind.TOML:
        after = _merge_toml(group.path, group.harness, before, group.fragments[0])
    else:
        raise RuntimeError(f"internal harness plan has unknown format {kind!r}")
    return _Mutation(
        group.harness,
        group.path,
        before,
        after,
        before_mode,
        mode,
        exists,
        before != after,
        "update" if exists else "create",
    )


def _check_kiro_peers(home: Path, plan: HarnessPlan) -> None:
    if plan.name != "kiro" or plan.workspace is None:
        return
    directory = home / ".kiro" / "hooks"
    try:
        entries = tuple(directory.iterdir())
    except FileNotFoundError:
        return
    except OSError as error:
        raise HarnessConflictError(f"inspect Kiro hook workspaces: {error}") from error
    fragment = HarnessFragment(
        "",
        FragmentKind.JSON,
        "",
        managed_base=os.fspath(plan.base),
        workspace=os.fspath(plan.workspace),
    )
    for path in entries:
        if path.suffix != ".json" or path.is_dir():
            continue
        try:
            value = load_json(read_file_limited(path, MAX_CONTROL_FILE_BYTES))
        except ValueError as error:
            raise HarnessConflictError(f"decode Kiro hook {path}: {error}") from error
        _check_workspace_conflict(path, value, fragment)


def _preflight(home: Path, plans: Sequence[HarnessPlan], cancel: Cancellation | None) -> list[_Mutation]:
    for plan in plans:
        check_cancel(cancel)
        _check_kiro_peers(home, plan)
    files: list[_Mutation] = []
    for group in _groups(home, plans):
        check_cancel(cancel)
        files.append(_preflight_file(group))
    return files


def _revalidate(file: _Mutation) -> None:
    try:
        info = file.path.lstat()
    except FileNotFoundError:
        if not file.exists:
            return
        raise HarnessConflictError(f"harness config {file.path} disappeared after preflight") from None
    except OSError as error:
        raise HarnessConflictError(f"inspect harness config {file.path} before writing: {error}") from error
    if not file.exists:
        raise HarnessConflictError(f"harness config {file.path} appeared after preflight")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise HarnessConflictError(f"harness config {file.path} changed type after preflight")
    if stat.S_IMODE(info.st_mode) != file.before_mode:
        raise HarnessConflictError(f"harness config {file.path} changed mode after preflight")
    current = read_file_limited(file.path, MAX_CONTROL_FILE_BYTES)
    if current != file.before:
        raise HarnessConflictError(f"harness config {file.path} changed after preflight")


def _apply(files: Sequence[_Mutation], cancel: Cancellation | None) -> None:
    for file in files:
        if file.changed:
            _revalidate(file)
    for file in files:
        check_cancel(cancel)
        if not file.changed:
            continue
        _revalidate(file)
        if file.exists:
            atomic_write(Path(os.fspath(file.path) + BACKUP_SUFFIX), file.before, mode=file.mode)
        atomic_write(file.path, file.after, mode=file.mode)


def install_harnesses(
    base_root: Path | str,
    request: HarnessInstallRequest,
    *,
    launcher_resolver: LauncherResolver | None = resolve_persistent_launcher,
    cancel: Cancellation | None = None,
) -> HarnessInstallReport:
    """Preflight and optionally install selected user-scope harness integrations."""

    check_cancel(cancel)
    base, base_name = _harness_base(base_root)
    names = _select(request)
    home = _home(request.home)
    executable = _launcher(request.executable, request.path, launcher_resolver)
    workspace = _workspace(request.workspace)
    _validate_assets(base, workspace is not None)
    plans = tuple(_build_plan(base, base_name, name, executable, workspace) for name in names)
    files = _preflight(home, plans, cancel)
    changes = tuple(
        HarnessChange(
            file.harness,
            file.action,
            file.path,
            Path(os.fspath(file.path) + BACKUP_SUFFIX) if file.exists else None,
        )
        for file in files
        if file.changed
    )
    mode = "check" if request.check else "dry-run" if request.dry_run else "install"
    complete = not changes
    if changes and not request.check and not request.dry_run:
        _apply(files, cancel)
        complete = True
    return HarnessInstallReport(base, base_name, mode, names, complete, changes, workspace)


_LEGACY_TARGETS: Final[dict[str, tuple[tuple[str, str, bool], ...]]] = {
    "claude": ((".claude.json", "mcpServers.fkf", False), (".claude/settings.json", "hooks.SessionStart", False)),
    "codex": ((".codex/config.toml", "", True),),
    "gemini": (
        (".gemini/settings.json", "mcpServers.fkf", False),
        (".gemini/settings.json", "hooks.SessionStart", False),
    ),
    "copilot": (
        (".copilot/mcp-config.json", "mcpServers.fkf", False),
        (".copilot/hooks/fkf.json", "hooks.sessionStart", False),
    ),
    "antigravity": (
        (".gemini/config/mcp_config.json", "mcpServers.fkf", False),
        (".gemini/config/hooks.json", "fkf", False),
    ),
    "opencode": ((".config/opencode/opencode.json", "mcp.fkf", False), (".config/opencode/plugins/fkf.js", "", False)),
    "grok": ((".grok/config.toml", "", True),),
    "cursor": ((".cursor/mcp.json", "mcpServers.fkf", False),),
    "kiro": ((".kiro/settings/mcp.json", "mcpServers.fkf", False), (".kiro/hooks/fkf.json", "hooks", False)),
    "cline": (
        (".cline/data/settings/cline_mcp_settings.json", "mcpServers.fkf", False),
        (".cline/hooks/TaskStart", "", False),
    ),
}


def _json_at(root: object, selector: str) -> tuple[object, bool]:
    value = root
    for part in selector.split("."):
        if not isinstance(value, Mapping) or part not in value:
            return None, False
        value = value[part]
    return value, True


def _legacy(home: Path, harness: str) -> tuple[str, ...]:
    found: list[str] = []
    for relative, selector, toml in _LEGACY_TARGETS[harness]:
        path = home.joinpath(*relative.split("/"))
        try:
            body = read_file_limited(path, MAX_CONTROL_FILE_BYTES)
        except OSError, ValueError, OperationalError:
            continue
        unscoped = False
        if toml:
            text = body.decode(errors="replace")
            unscoped = "[mcp_servers.fkf]" in text and '"mcp", "serve"' in text
        elif not selector:
            unscoped = b"fkf-hook.py" in body
        else:
            try:
                value, exists = _json_at(load_json(body), selector)
            except ValueError:
                continue
            unscoped = exists and (_json_managed(value, "mcp", harness) or _find_hook(value, harness))
        if unscoped:
            location = f"~/{relative}"
            found.append(f"{location}#{selector}" if selector else location)
    return tuple(found)


def inspect_harnesses(
    base_root: Path | str,
    *,
    home: Path | str = "",
    executable: Path | str = "",
    path: str = "",
    launcher_resolver: LauncherResolver | None = resolve_persistent_launcher,
    cancel: Cancellation | None = None,
) -> tuple[HarnessRegistration, ...]:
    """Read every registry target without mutation or base-owned hook requirements."""

    check_cancel(cancel)
    base, base_name = _harness_base(base_root)
    selected_home = _home(home)
    launcher = _launcher(executable, path, launcher_resolver)
    registrations: list[HarnessRegistration] = []
    for name in HARNESSES:
        check_cancel(cancel)
        error = ""
        changes = 0
        try:
            files = _preflight(selected_home, (_build_plan(base, base_name, name, launcher, None),), cancel)
            changes = sum(file.changed for file in files)
        except (OSError, ValueError, OperationalError, HarnessConflictError) as caught:
            error = str(caught)
        registrations.append(
            HarnessRegistration(name, not error and changes == 0, changes, error, _legacy(selected_home, name))
        )
    return tuple(registrations)


__all__ = [
    "BACKUP_SUFFIX",
    "HARNESSES",
    "FragmentKind",
    "HarnessChange",
    "HarnessConflictError",
    "HarnessFragment",
    "HarnessInstallReport",
    "HarnessInstallRequest",
    "HarnessPlan",
    "HarnessRegistration",
    "LauncherResolver",
    "harness_names",
    "harness_plan_for",
    "inspect_harnesses",
    "install_harnesses",
    "resolve_persistent_launcher",
]
