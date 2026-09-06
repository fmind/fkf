from __future__ import annotations

import subprocess
import sys


def test_ordinary_cli_import_does_not_load_the_mcp_sdk() -> None:
    completed = subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import sys; import fkf.cli; "
                "assert not any(name == 'mcp' or name.startswith(('mcp.', 'mcp_types')) "
                "for name in sys.modules), sorted(sys.modules)"
            ),
        ],
        check=False,
        capture_output=True,
        text=True,
    )

    assert completed.returncode == 0, completed.stderr
