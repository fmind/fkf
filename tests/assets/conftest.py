"""Hermetic execution support for bundled preset helpers."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path

import pytest

from fkf.assets import shipped_helpers
from fkf.config import Config, load_config
from fkf.documents import Document, build_document, day_window, decode_records, parse_day_in_location
from fkf.init import render_config

REPOSITORY = Path(__file__).parents[2]
FIXTURES = Path(__file__).parent / "fixtures"
SOURCE_FIXTURES = FIXTURES / "sources"
_TOOL_ROOTS = {
    name: os.environ.get(name)
    for name in ("HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME")
}


@dataclass(frozen=True)
class HelperResult:
    """Captured helper outcome without conflating stdout and diagnostics."""

    returncode: int
    stdout: bytes
    stderr: bytes


@dataclass
class HelperInstallation:
    """One exact bundled helper tree with isolated user and temporary state."""

    root: Path
    home: Path
    bin: Path
    temporary: Path

    def fake(self, name: str, body: str, *, shebang: str = "#!/bin/sh") -> Path:
        """Install one fake provider ahead of host tools on the helper PATH."""
        target = self.bin / name
        # A local tool may be linked to the workstation; replace the entry, never its target.
        target.unlink(missing_ok=True)
        target.write_text(f"{shebang}\nset -eu\n{body}", encoding="utf-8")
        target.chmod(0o700)
        return target

    def environment(self, extra: dict[str, str] | None = None, *, host_path: bool = True) -> dict[str, str]:
        """Return a deliberately small child environment."""
        path = os.fspath(self.bin)
        if host_path:
            path = os.pathsep.join((path, "/usr/bin", "/bin"))
        environment = {
            "HOME": os.fspath(self.home),
            "LC_ALL": "C.UTF-8",
            "PATH": path,
            "TMPDIR": os.fspath(self.temporary),
            "TZ": "UTC",
            "XDG_CACHE_HOME": os.fspath(self.home / ".cache"),
            "XDG_CONFIG_HOME": os.fspath(self.home / ".config"),
            "XDG_STATE_HOME": os.fspath(self.home / ".local" / "state"),
        }
        if extra is not None:
            environment.update(extra)
        return environment

    def run(
        self,
        name: str,
        *arguments: str,
        stdin: str | bytes | None = None,
        environment: dict[str, str] | None = None,
        timeout: float = 15,
    ) -> HelperResult:
        """Run one installed helper through its declared interpreter."""
        target = self.bin / name
        command = (
            [sys.executable, "-I", os.fspath(target)] if target.suffix == ".py" else ["/bin/sh", os.fspath(target)]
        )
        encoded = stdin.encode() if isinstance(stdin, str) else stdin
        completed = subprocess.run(  # noqa: S603 - argv and the isolated executable tree are test-owned.
            [*command, *arguments],
            check=False,
            cwd="/",
            input=encoded,
            capture_output=True,
            env=self.environment(environment),
            timeout=timeout,
        )
        return HelperResult(completed.returncode, completed.stdout, completed.stderr)


@pytest.fixture
def helpers(tmp_path: Path) -> HelperInstallation:
    """Materialize exact package bytes without relying on a source checkout path."""
    root = tmp_path / "installation"
    home = root / "home"
    bin_directory = root / "sources"
    temporary = root / "tmp"
    home.mkdir(parents=True)
    bin_directory.mkdir()
    temporary.mkdir()
    for name, content in shipped_helpers().items():
        target = bin_directory / name
        target.write_bytes(content)
        target.chmod(0o700)
    # These reviewed local tools are part of the helper execution boundary; provider CLIs are
    # always installed as test fakes below and can never fall through to the workstation.
    for name in ("jq", "sqlite3", "yq"):
        executable = shutil.which(name)
        if executable is not None and Path(executable).parent.name == "shims":
            mise = shutil.which("mise")
            if mise is not None:
                resolution_environment = dict(os.environ)
                for variable, value in _TOOL_ROOTS.items():
                    if value is None:
                        resolution_environment.pop(variable, None)
                    else:
                        resolution_environment[variable] = value
                resolved = subprocess.run(  # noqa: S603 - resolve the repository-standard tool before HOME isolation.
                    [mise, "which", name],
                    check=False,
                    capture_output=True,
                    cwd=REPOSITORY,
                    env=resolution_environment,
                )
                candidate = Path(resolved.stdout.decode().strip())
                if resolved.returncode == 0 and candidate.is_file():
                    executable = os.fspath(candidate)
        if executable is not None:
            (bin_directory / name).symlink_to(executable)
    # Nested helper calls use their env-python3 shebang, so pin them to the package's tested
    # interpreter instead of whichever older Python happens to live in /usr/bin.
    (bin_directory / "python3").symlink_to(sys.executable)
    return HelperInstallation(root, home, bin_directory, temporary)


def load_preset(root: Path, preset: str) -> Config:
    """Load one bundled preset as a complete base configuration."""
    base = root / f"base-{preset}"
    base.mkdir()
    (base / "fkf.yaml").write_text(render_config("test-base", preset), encoding="utf-8")
    return load_config(base)


def validate_helper_output(config: Config, source_name: str, output: bytes) -> Document:
    """Run helper output through the same pure decode and document validator as collection."""
    source = config.sources[source_name]
    records = decode_records(source, output)
    window = day_window(parse_day_in_location("2026-05-04", UTC)) if source.layer.value == "events" else None
    return build_document(
        source,
        records,
        window=window,
        collected_at=datetime(2026, 5, 10, 12, tzinfo=UTC),
    )


def prompt_body_arguments(helpers: HelperInstallation, *, turn: int = 1) -> list[str]:
    """Address the exact synthetic generation through its durable source record."""
    base = helpers.root / "base"
    path = base / "events" / "2026-05-04" / "agent-prompts.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    identifier = "codex-session-1-20260504T000000Z"
    path.write_text(
        json.dumps(
            {
                "fkf": 1,
                "source": "agent-prompts",
                "layer": "events",
                "date": "2026-05-04",
                "records": [
                    {
                        "id": identifier,
                        "lineage": hashlib.sha256(b"codex\0session-1\0").hexdigest(),
                        "session": "a" * 64,
                        "turn": turn,
                    }
                ],
            }
        )
    )
    return [os.fspath(base), "agent-prompts", identifier]
