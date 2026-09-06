"""Local, offline evidence retrieval for coding agents."""

from __future__ import annotations

from importlib.metadata import version
from typing import NoReturn

__version__ = version("fkf")
DISPLAY_VERSION = f"v{__version__}"


def main() -> NoReturn:
    """Run the ``fkf`` console command."""
    from fkf.cli import app
    from fkf.cli_support import exit_main
    from fkf.logging_config import configure_logging

    configure_logging()
    exit_main(app)


__all__ = ["DISPLAY_VERSION", "__version__", "main"]
