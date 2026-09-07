from __future__ import annotations

from importlib.metadata import version

import fkf
from fkf import __main__ as module_entrypoint


def test_distribution_version_is_public() -> None:
    assert fkf.__version__ == version("fkf")


def test_module_entrypoint_uses_public_main() -> None:
    assert module_entrypoint.main is fkf.main
