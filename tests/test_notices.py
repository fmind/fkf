from __future__ import annotations

import re
import tomllib
from pathlib import Path
from typing import cast

ROOT = Path(__file__).resolve().parents[1]
_DISTRIBUTION_HEADER = re.compile(r"^## (?P<name>[A-Za-z0-9._-]+) (?P<version>\S+)$", re.MULTILINE)
_PLATFORM_MARKERS = {
    "linux": {
        "implementation_name != 'PyPy'": True,
        "platform_python_implementation != 'PyPy'": True,
        "python_full_version < '3.15'": True,
        "sys_platform != 'emscripten'": True,
        "sys_platform == 'emscripten'": False,
        "sys_platform == 'win32'": False,
    },
    "macos": {
        "implementation_name != 'PyPy'": True,
        "platform_python_implementation != 'PyPy'": True,
        "python_full_version < '3.15'": True,
        "sys_platform != 'emscripten'": True,
        "sys_platform == 'emscripten'": False,
        "sys_platform == 'win32'": False,
    },
}


def _canonical_name(value: str) -> str:
    return re.sub(r"[-_.]+", "-", value).lower()


def _mapping(value: object, label: str) -> dict[str, object]:
    assert isinstance(value, dict), f"{label} must be a table"
    assert all(isinstance(key, str) for key in value), f"{label} keys must be strings"
    return cast("dict[str, object]", value)


def _sequence(value: object, label: str) -> list[object]:
    assert isinstance(value, list), f"{label} must be an array"
    return cast("list[object]", value)


def _marker_matches(marker: object, platform: str) -> bool:
    if marker is None:
        return True
    assert isinstance(marker, str), "dependency marker must be a string"
    outcomes = _PLATFORM_MARKERS[platform]
    assert marker in outcomes, f"unsupported {platform} runtime marker in uv.lock: {marker}"
    return outcomes[marker]


def _dependency_entries(
    package: dict[str, object], *, include_base: bool, extras: set[str], package_name: str
) -> list[object]:
    entries = _sequence(package.get("dependencies", []), f"{package_name}.dependencies") if include_base else []
    optional = _mapping(package.get("optional-dependencies", {}), f"{package_name}.optional-dependencies")
    for extra in sorted(extras):
        assert extra in optional, f"{package_name} has no locked optional dependency group {extra}"
        entries.extend(_sequence(optional[extra], f"{package_name}.optional-dependencies.{extra}"))
    return entries


def _runtime_distributions(platform: str) -> dict[str, str]:
    assert platform in _PLATFORM_MARKERS, f"unsupported declared platform: {platform}"
    lock: dict[str, object] = tomllib.loads((ROOT / "uv.lock").read_text(encoding="utf-8"))
    raw_packages = _sequence(lock.get("package"), "package")
    packages: dict[str, dict[str, object]] = {}
    for index, value in enumerate(raw_packages):
        package = _mapping(value, f"package[{index}]")
        raw_name = package.get("name")
        assert isinstance(raw_name, str), f"package[{index}].name must be a string"
        name = _canonical_name(raw_name)
        assert name not in packages, f"uv.lock contains multiple entries for {name}"
        packages[name] = package

    requested_extras: dict[str, set[str]] = {"fkf": set()}
    processed_extras: dict[str, set[str]] = {}
    selected: set[str] = set()
    pending = ["fkf"]
    while pending:
        name = pending.pop()
        assert name in packages, f"uv.lock has no package entry for runtime dependency {name}"
        package = packages[name]
        first_visit = name not in selected
        new_extras = requested_extras[name] - processed_extras.get(name, set())
        if not first_visit and not new_extras:
            continue
        selected.add(name)
        processed_extras.setdefault(name, set()).update(new_extras)

        for value in _dependency_entries(package, include_base=first_visit, extras=new_extras, package_name=name):
            dependency = _mapping(value, f"{name} dependency")
            if not _marker_matches(dependency.get("marker"), platform):
                continue
            raw_dependency_name = dependency.get("name")
            assert isinstance(raw_dependency_name, str), f"{name} dependency name must be a string"
            dependency_name = _canonical_name(raw_dependency_name)
            raw_extras = _sequence(dependency.get("extra", []), f"{name}->{dependency_name}.extra")
            extras: set[str] = set()
            for extra in raw_extras:
                assert isinstance(extra, str), f"{name}->{dependency_name} extra must be a string"
                extras.add(extra)
            known_extras = requested_extras.setdefault(dependency_name, set())
            if dependency_name not in selected or not extras.issubset(known_extras):
                known_extras.update(extras)
                pending.append(dependency_name)

    distributions: dict[str, str] = {}
    for name in selected:
        package = packages[name]
        source = _mapping(package.get("source"), f"{name}.source")
        if "registry" not in source:
            assert name == "fkf", f"unexpected non-registry runtime package: {name}"
            assert source == {"editable": "."}, "fkf must remain the editable lock root"
            continue
        version = package.get("version")
        assert isinstance(version, str), f"{name}.version must be a string"
        distributions[name] = version
    return distributions


def _declared_notices() -> tuple[dict[str, str], tuple[str, ...]]:
    text = (ROOT / "THIRD_PARTY_NOTICES.md").read_text(encoding="utf-8")
    matches = tuple(_DISTRIBUTION_HEADER.finditer(text))
    declared: dict[str, str] = {}
    for index, match in enumerate(matches):
        name = _canonical_name(match.group("name"))
        assert name not in declared, f"duplicate third-party notice for {name}"
        declared[name] = match.group("version")
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        section = text[match.end() : end]
        assert "\nSource: <https://" in section, f"{name} notice has no authoritative source"
        assert "\nLicense: `" in section, f"{name} notice has no license expression"
        assert "\nEvidence: `" in section, f"{name} notice has no installed legal-file evidence"
        assert "\n```text\n" in section, f"{name} notice has no legal text"
    return declared, tuple(match.group(0) for match in matches)


def test_notices_match_the_complete_locked_runtime_distribution_closure() -> None:
    linux = _runtime_distributions("linux")
    macos = _runtime_distributions("macos")
    declared, headings = _declared_notices()

    assert linux == macos, "declared Linux and macOS runtime closures diverged"
    assert declared == linux
    assert headings == tuple(sorted(headings, key=str.casefold))


def test_distribution_packages_both_project_and_third_party_licenses() -> None:
    project = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))["project"]

    assert project["license"] == "MIT"
    assert project["license-files"] == ["LICENSE", "THIRD_PARTY_NOTICES.md"]
