from __future__ import annotations

from tests.assets.conftest import HelperInstallation


def test_installed_helpers_run_from_the_declared_command_directory(helpers: HelperInstallation) -> None:
    helpers.fake("working-directory", "pwd\n")

    result = helpers.run("working-directory")

    assert result.returncode == 0
    assert result.stdout == b"/\n"


def test_fake_replaces_tool_link_without_writing_through_it(helpers: HelperInstallation) -> None:
    original = helpers.root / "original-tool"
    original.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    original.chmod(0o755)
    target = helpers.bin / "linked-tool"
    target.symlink_to(original)

    helpers.fake("linked-tool", "exit 37\n")

    assert not target.is_symlink()
    assert original.read_text(encoding="utf-8") == "#!/bin/sh\nexit 0\n"
    assert original.stat().st_mode & 0o777 == 0o755
    assert helpers.run("linked-tool").returncode == 37
