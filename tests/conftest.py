"""Hermetic defaults shared by the Python test suite."""

from __future__ import annotations

import os
from pathlib import Path

import pytest


@pytest.fixture(autouse=True)
def isolated_user_state(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """Keep tests away from real bases, trust records, and user configuration."""
    home = tmp_path / "home"
    state = home / "state"
    home.mkdir()
    state.mkdir()
    # Git hooks export repository selectors that otherwise redirect fixture Git calls.
    for name in tuple(os.environ):
        if name.startswith("GIT_"):
            monkeypatch.delenv(name)
    monkeypatch.delenv("FKF_BASE", raising=False)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_STATE_HOME", str(state))
