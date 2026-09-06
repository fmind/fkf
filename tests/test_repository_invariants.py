from __future__ import annotations

import ast
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "src/fkf"
_CREDENTIAL_LABEL = re.compile(r"\b(TOKEN|SECRET|PASSWORD|API_KEY|PAT)\b")
_NETWORK_MODULES = frozenset(
    {
        "aiohttp",
        "http",
        "http.client",
        "httpx",
        "requests",
        "socket",
        "urllib.request",
        "urllib3",
    }
)
_LOCAL_TREES = frozenset({".git", ".opengrep", ".venv", "dist", "htmlcov", "node_modules"})


def _imports(path: Path) -> set[str]:
    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    imported: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            imported.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            imported.add(node.module)
    return imported


def _working_tree_files(pattern: str) -> list[Path]:
    """Include new source files while excluding ignored tool and build caches."""
    return [path for path in ROOT.rglob(pattern) if not _LOCAL_TREES.intersection(path.relative_to(ROOT).parts)]


def test_application_source_has_no_network_client_import() -> None:
    violations: list[str] = []
    for path in sorted(SOURCE.glob("*.py")):
        violations.extend(
            f"{path.relative_to(ROOT)} imports {module}"
            for module in sorted(_imports(path))
            if any(module == forbidden or module.startswith(f"{forbidden}.") for forbidden in _NETWORK_MODULES)
        )
    assert violations == []


def test_application_source_has_no_credential_shaped_identifier() -> None:
    violations: list[str] = []
    for path in sorted(SOURCE.glob("*.py")):
        for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            if line.lstrip().startswith("#"):
                continue
            if _CREDENTIAL_LABEL.search(line):
                violations.append(f"{path.relative_to(ROOT)}:{line_number}")
    assert violations == []


def test_no_product_go_source_or_root_module_remains() -> None:
    assert _working_tree_files("*.go") == []
    assert _working_tree_files("go.mod") == []
    assert _working_tree_files("go.sum") == []
