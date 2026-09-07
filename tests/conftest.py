"""Hermetic defaults shared by the Python test suite."""

from __future__ import annotations

from pathlib import Path

import pytest


@pytest.fixture(autouse=True)
def isolated_user_state(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """Keep tests away from real bases, trust records, and user configuration."""
    home = tmp_path / "home"
    state = home / "state"
    home.mkdir()
    state.mkdir()
    monkeypatch.delenv("FKF_BASE", raising=False)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
