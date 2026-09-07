from __future__ import annotations

import hashlib
import io
import json
import os
import re
import runpy
import shutil
import subprocess
import textwrap
import time
import tomllib
import urllib.error
import urllib.request
from email.message import Message
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def pypi_verifier() -> str:
    workflow = read(".github/workflows/cd.yml")
    marker = "          python3 - <<'PY'\n"
    start = workflow.index(marker) + len(marker)
    end = workflow.index("\n          PY", start)
    return textwrap.dedent(workflow[start:end])


def pypi_payload(files: dict[str, str]) -> io.BytesIO:
    payload = {"urls": [{"filename": name, "digests": {"sha256": digest}} for name, digest in files.items()]}
    return io.BytesIO(json.dumps(payload).encode())


def run_pypi_verifier(tmp_path: Path) -> None:
    verifier_path = tmp_path / "verify_pypi.py"
    verifier_path.write_text(pypi_verifier(), encoding="utf-8")
    runpy.run_path(str(verifier_path), run_name="__main__")


def test_python_package_metadata_and_quality_contract() -> None:
    project = tomllib.loads(read("pyproject.toml"))

    assert project["build-system"]["build-backend"] == "uv_build"
    assert project["project"]["scripts"] == {"fkf": "fkf:main"}
    assert project["tool"]["coverage"]["run"]["branch"] is True
    assert project["tool"]["ty"]["environment"]["python-version"] == "3.14"

    dev_dependencies = "\n".join(project["dependency-groups"]["dev"])
    for dependency in ("pytest", "pytest-cov", "ruff", "ty", "validate-pyproject", "zensical"):
        assert dependency in dev_dependencies


def test_package_version_has_extractable_release_notes() -> None:
    version = tomllib.loads(read("pyproject.toml"))["project"]["version"]
    extractor = ROOT / "scripts/release-notes"

    completed = subprocess.run(  # noqa: S603 - the repository-owned extractor reads the repository changelog.
        [str(extractor), f"v{version}", str(ROOT / "CHANGELOG.md")],
        check=False,
        capture_output=True,
        text=True,
    )

    assert completed.returncode == 0, completed.stderr
    release_notes = completed.stdout.strip()
    assert release_notes.startswith("### ")
    assert "\n- " in release_notes
    assert "\n## [" not in release_notes


def test_release_tag_verifier_accepts_a_direct_tag_on_current_main(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    gh = fake_bin / "gh"
    gh.write_text(
        """#!/bin/sh
case "$2" in
  */git/ref/tags/*|*/git/ref/heads/main) printf 'commit\\trelease-commit\\n' ;;
  *) exit 2 ;;
esac
""",
        encoding="utf-8",
    )
    gh.chmod(0o700)

    completed = subprocess.run(  # noqa: S603 - the repository-owned verifier executes only a test-double gh.
        [str(ROOT / "scripts/verify-release-tag")],
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "GITHUB_REPOSITORY": "fmind/fkf",
            "GITHUB_SHA": "release-commit",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
    )

    assert completed.returncode == 0, completed.stderr


def test_release_tag_verifier_peels_an_annotated_tag(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    gh = fake_bin / "gh"
    gh.write_text(
        """#!/bin/sh
case "$2" in
  */git/ref/tags/*) printf 'tag\\ttag-object\\n' ;;
  */git/tags/tag-object) printf 'commit\\trelease-commit\\n' ;;
  */git/ref/heads/main) printf 'commit\\trelease-commit\\n' ;;
  *) exit 2 ;;
esac
""",
        encoding="utf-8",
    )
    gh.chmod(0o700)

    completed = subprocess.run(  # noqa: S603 - the repository-owned verifier executes only a test-double gh.
        [str(ROOT / "scripts/verify-release-tag")],
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "GITHUB_REPOSITORY": "fmind/fkf",
            "GITHUB_SHA": "release-commit",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
    )

    assert completed.returncode == 0, completed.stderr


def test_release_tag_verifier_rejects_a_moved_tag(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    gh = fake_bin / "gh"
    gh.write_text("#!/bin/sh\nprintf 'commit\\tmoved-commit\\n'\n", encoding="utf-8")
    gh.chmod(0o700)

    completed = subprocess.run(  # noqa: S603 - the repository-owned verifier executes only a test-double gh.
        [str(ROOT / "scripts/verify-release-tag")],
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "GITHUB_REPOSITORY": "fmind/fkf",
            "GITHUB_SHA": "release-commit",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
    )

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert "does not resolve to workflow commit" in completed.stderr


def test_release_tag_verifier_rejects_a_commit_not_on_current_main(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    gh = fake_bin / "gh"
    gh.write_text(
        """#!/bin/sh
case "$2" in
  */git/ref/tags/*) printf 'commit\\trelease-commit\\n' ;;
  */git/ref/heads/main) printf 'commit\\tmain-commit\\n' ;;
  */compare/release-commit...main-commit) printf 'behind\\n' ;;
  *) exit 2 ;;
esac
""",
        encoding="utf-8",
    )
    gh.chmod(0o700)

    completed = subprocess.run(  # noqa: S603 - the repository-owned verifier executes only a test-double gh.
        [str(ROOT / "scripts/verify-release-tag")],
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "GITHUB_REPOSITORY": "fmind/fkf",
            "GITHUB_SHA": "release-commit",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
    )

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert "release commit release-commit is not contained in current main main-commit" in completed.stderr


def test_release_tag_verifier_accepts_an_ancestor_after_main_advances(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    gh = fake_bin / "gh"
    gh.write_text(
        """#!/bin/sh
case "$2" in
  */git/ref/tags/*) printf 'commit\\trelease-commit\\n' ;;
  */git/ref/heads/main) printf 'commit\\tnew-main-commit\\n' ;;
  */compare/release-commit...new-main-commit) printf 'ahead\\n' ;;
  *) exit 2 ;;
esac
""",
        encoding="utf-8",
    )
    gh.chmod(0o700)

    completed = subprocess.run(  # noqa: S603 - the repository-owned verifier executes only a test-double gh.
        [str(ROOT / "scripts/verify-release-tag")],
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "GITHUB_REPOSITORY": "fmind/fkf",
            "GITHUB_SHA": "release-commit",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
    )

    assert completed.returncode == 0, completed.stderr


def run_release_asset_verifier(
    tmp_path: Path,
    *,
    local_assets: dict[str, bytes],
    remote_assets: dict[str, bytes],
    local_symlinks: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    dist = tmp_path / "dist"
    remote = tmp_path / "remote"
    fake_bin = tmp_path / "bin"
    dist.mkdir()
    remote.mkdir()
    fake_bin.mkdir()
    for name, content in local_assets.items():
        (dist / name).write_bytes(content)
    for name, target in (local_symlinks or {}).items():
        (dist / name).symlink_to(target)
    for name, content in remote_assets.items():
        (remote / name).write_bytes(content)

    gh = fake_bin / "gh"
    gh.write_text(
        """#!/bin/sh
set -eu
case "$1:$2" in
  release:view)
    for asset in "$REMOTE_ASSETS"/*; do
      test -f "$asset" || continue
      basename "$asset"
    done
    ;;
  release:download)
    test "$4" = --dir
    for asset in "$REMOTE_ASSETS"/*; do
      test -f "$asset" || continue
      cp "$asset" "$5/"
    done
    ;;
  *) exit 2 ;;
esac
""",
        encoding="utf-8",
    )
    gh.chmod(0o700)

    return subprocess.run(  # noqa: S603 - the verifier executes only a hermetic test-double gh.
        [str(ROOT / "scripts/verify-release-assets")],
        cwd=tmp_path,
        check=False,
        capture_output=True,
        env={
            "GITHUB_REF_NAME": "v5.0.0",
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
            "REMOTE_ASSETS": os.fspath(remote),
        },
        text=True,
    )


def test_release_asset_verifier_accepts_exact_inventory_and_bytes(tmp_path: Path) -> None:
    assets = {
        "fkf-5.0.0-py3-none-any.whl": b"wheel",
        "fkf-5.0.0.tar.gz": b"sdist",
    }

    completed = run_release_asset_verifier(tmp_path, local_assets=assets, remote_assets=assets)

    assert completed.returncode == 0, completed.stderr
    assert completed.stdout == ""


def test_release_asset_verifier_ignores_exact_uv_dist_marker(tmp_path: Path) -> None:
    release_assets = {
        "fkf-5.0.0-py3-none-any.whl": b"wheel",
        "fkf-5.0.0.tar.gz": b"sdist",
    }

    completed = run_release_asset_verifier(
        tmp_path,
        local_assets={**release_assets, ".gitignore": b"*"},
        remote_assets=release_assets,
    )

    assert completed.returncode == 0, completed.stderr
    assert completed.stdout == ""


def test_release_asset_verifier_rejects_an_empty_local_inventory(tmp_path: Path) -> None:
    completed = run_release_asset_verifier(tmp_path, local_assets={}, remote_assets={})

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert "dist contains no release assets" in completed.stderr


@pytest.mark.parametrize(
    ("name", "content"),
    [
        (".gitignore", b"*\n"),
        (".gitignore", b"!"),
        (".unexpected", b"hidden"),
        ("unexpected.txt", b"ordinary"),
    ],
)
def test_release_asset_verifier_rejects_non_marker_local_entries(tmp_path: Path, name: str, content: bytes) -> None:
    release_assets = {
        "fkf-5.0.0-py3-none-any.whl": b"wheel",
        "fkf-5.0.0.tar.gz": b"sdist",
    }

    completed = run_release_asset_verifier(
        tmp_path,
        local_assets={**release_assets, name: content},
        remote_assets=release_assets,
    )

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert "release asset inventory does not match dist" in completed.stderr


def test_release_asset_verifier_rejects_a_symlinked_uv_marker(tmp_path: Path) -> None:
    release_assets = {
        "fkf-5.0.0-py3-none-any.whl": b"wheel",
        "fkf-5.0.0.tar.gz": b"sdist",
    }

    completed = run_release_asset_verifier(
        tmp_path,
        local_assets=release_assets,
        local_symlinks={".gitignore": "fkf-5.0.0-py3-none-any.whl"},
        remote_assets=release_assets,
    )

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert "dist contains a non-regular release asset .gitignore" in completed.stderr


@pytest.mark.parametrize(
    ("remote_assets", "message"),
    [
        ({"fkf-5.0.0-py3-none-any.whl": b"wheel"}, "release asset inventory does not match dist"),
        (
            {
                "fkf-5.0.0-py3-none-any.whl": b"wheel",
                "fkf-5.0.0.tar.gz": b"sdist",
                "unexpected.txt": b"unexpected",
            },
            "release asset inventory does not match dist",
        ),
        (
            {
                "fkf-5.0.0-py3-none-any.whl": b"changed",
                "fkf-5.0.0.tar.gz": b"sdist",
            },
            "release asset fkf-5.0.0-py3-none-any.whl does not match dist",
        ),
    ],
)
def test_release_asset_verifier_rejects_inventory_or_byte_drift(
    tmp_path: Path, remote_assets: dict[str, bytes], message: str
) -> None:
    local_assets = {
        "fkf-5.0.0-py3-none-any.whl": b"wheel",
        "fkf-5.0.0.tar.gz": b"sdist",
    }

    completed = run_release_asset_verifier(
        tmp_path,
        local_assets=local_assets,
        remote_assets=remote_assets,
    )

    assert completed.returncode == 1
    assert completed.stdout == ""
    assert message in completed.stderr


def test_mise_owns_the_python_delivery_gate() -> None:
    mise = tomllib.loads(read("mise.toml"))
    tools = mise["tools"]
    tasks = mise["tasks"]

    assert tools["aqua:astral-sh/uv"] == "0.12.10"
    assert "uv" not in tools
    for retired_tool in ("go", "golangci-lint", "goreleaser", "gotestsum", "hugo"):
        assert retired_tool not in tools

    for canonical_task in ("install", "format", "check", "test", "build", "all"):
        assert canonical_task in tasks

    task_source = read("mise.toml")
    for required_command in (
        "uv sync",
        "uv run validate-pyproject pyproject.toml",
        "uv run ruff format --check",
        "uv run ruff check",
        "uv run ty check",
        "uv audit --preview-features audit-command --locked",
        "uv run pytest",
        "--cov-branch",
        "--cov-fail-under=85",
        "uv build --wheel --sdist",
        "fkf config schema",
        "actionlint",
        "zizmor --offline --persona=pedantic",
        "shellcheck -S warning scripts/release-notes scripts/verify-release-assets scripts/verify-release-tag",
        "gitleaks",
        "trivy --config trivy.yaml fs .",
    ):
        assert required_command in task_source

    for retired_command in ("format:go", "install:binary", "goreleaser", "govulncheck"):
        assert retired_command not in task_source

    lock = tomllib.loads(read("mise.lock"))["tools"]
    [locked_uv] = lock["aqua:astral-sh/uv"]
    assert locked_uv["backend"] == "aqua:astral-sh/uv"
    platforms = {
        key.removeprefix("platforms."): value for key, value in locked_uv.items() if key.startswith("platforms.")
    }
    assert set(platforms) == {"linux-x64", "linux-arm64", "macos-x64", "macos-arm64"}
    for platform in platforms.values():
        assert platform["url"].startswith("https://github.com/astral-sh/uv/releases/download/")
        assert platform["checksum"].startswith("sha256:")


def test_package_gate_installs_both_distributions_outside_the_checkout() -> None:
    tasks = tomllib.loads(read("mise.toml"))["tasks"]
    build_steps = tasks["build"]["run"]
    package_task = tasks["test:package"]["run"]

    assert build_steps[0] == "uv sync --locked"
    assert 'wheel="$project_root/$(find dist' in package_task
    assert 'sdist="$project_root/$(find dist' in package_task
    assert "for distribution in wheel sdist; do" in package_task
    assert 'uv venv --python 3.14 "$tmp/$distribution-venv"' in package_task
    assert (
        'VIRTUAL_ENV="$tmp/$distribution-venv" \\\n'
        '    uv sync --project "$project_root" --locked --offline --no-dev --no-install-project --active'
    ) in package_task
    assert 'uv pip install --offline --no-deps --python "$tmp/wheel-venv/bin/python" "$wheel"' in package_task
    assert 'uv pip install --offline --no-deps --python "$tmp/sdist-venv/bin/python" "$sdist"' in package_task
    assert '"$tmp/wheel-venv/bin/fkf" --version' in package_task
    assert '"$tmp/sdist-venv/bin/fkf" --version' in package_task
    assert 'uv pip check --python "$tmp/wheel-venv/bin/python"' in package_task
    assert 'uv pip check --python "$tmp/sdist-venv/bin/python"' in package_task
    assert package_task.index("uv sync --project") < package_task.index('cd "$tmp"')
    assert package_task.index('cd "$tmp"') < package_task.index("uv pip install")
    assert "uv tool install" not in package_task
    assert "uvx" not in package_task


def test_test_gate_uses_a_run_unique_pytest_temp_tree() -> None:
    test_task = tomllib.loads(read("mise.toml"))["tasks"]["test"]["run"]

    assert "fkf_test_home=$(mktemp -d)" in test_task
    assert '--basetemp="$fkf_test_home/pytest"' in test_task
    assert test_task.index("fkf_test_home=$(mktemp -d)") < test_task.index("--basetemp=")


def test_link_gate_scans_the_existing_working_tree_markdown_set(tmp_path: Path) -> None:
    repository = tmp_path / "repository"
    repository.mkdir()
    (repository / "docs").mkdir()
    (repository / "notes").mkdir()
    (repository / ".gitignore").write_text("ignored.md\n", encoding="utf-8")
    for relative in ("tracked.md", "deleted.md", "odd\nname.md", "docs/tracked.md"):
        (repository / relative).write_text(f"# {relative}\n", encoding="utf-8")
    (repository / "notes/new.md").write_text("# New\n", encoding="utf-8")
    (repository / "ignored.md").write_text("# Ignored\n", encoding="utf-8")
    (repository / "docs/untracked.md").write_text("# Docs\n", encoding="utf-8")

    git = shutil.which("git")
    bash = shutil.which("bash")
    assert git is not None
    assert bash is not None
    subprocess.run(  # noqa: S603 - the resolved local git executable operates only on the test repository.
        [git, "init", "-q"], check=True, cwd=repository
    )
    subprocess.run(  # noqa: S603 - the resolved local git executable operates only on the test repository.
        [git, "add", ".gitignore", "tracked.md", "deleted.md", "odd\nname.md", "docs/tracked.md"],
        check=True,
        cwd=repository,
    )
    (repository / "deleted.md").unlink()

    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    capture = tmp_path / "lychee-argv"
    lychee = fake_bin / "lychee"
    lychee.write_text('#!/bin/sh\nprintf \'%s\\0\' "$@" >"$LYCHEE_CAPTURE"\n', encoding="utf-8")
    lychee.chmod(0o700)
    command = tomllib.loads(read("mise.toml"))["tasks"]["check:links"]["run"]
    completed = subprocess.run(  # noqa: S603 - the resolved shell runs repository-owned code against test fakes.
        [bash, "-c", command],
        check=False,
        capture_output=True,
        cwd=repository,
        env={
            "HOME": os.fspath(tmp_path / "home"),
            "LYCHEE_CAPTURE": os.fspath(capture),
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
        timeout=10,
    )

    assert completed.returncode == 0, completed.stderr
    arguments = capture.read_bytes().removesuffix(b"\0").split(b"\0")
    assert arguments[:3] == [b"--offline", b"--no-progress", b"--"]
    assert {argument.decode() for argument in arguments[3:]} == {"tracked.md", "notes/new.md", "odd\nname.md"}


def test_go_build_and_documentation_modules_are_retired() -> None:
    assert list(ROOT.rglob("go.mod")) == []
    assert list(ROOT.rglob("go.sum")) == []

    for retired_path in (".golangci.yml", ".goreleaser.yml", "install.sh"):
        assert not (ROOT / retired_path).exists()


def test_zensical_owns_the_documentation_and_pages_boundary() -> None:
    config = tomllib.loads(read("zensical.toml"))["project"]
    theme = config["theme"]

    assert config["site_url"] == "https://fmind.github.io/fkf/"
    assert config["docs_dir"] == "docs"
    assert config["site_dir"] == "site"
    assert config["strict"] is True
    assert config["plugins"]["redirects"]["redirect_maps"] == {"index.md": "docs/index.md"}
    assert theme["font"] is False
    assert theme["logo"] == "fmind-logo.webp"
    assert theme["favicon"] == "fmind-logo.webp"
    assert "palette" not in theme
    assert "repo_url" not in config
    assert "content.action.edit" not in theme["features"]
    assert config["plugins"]["search"]["enabled"] is False
    assert config["extra"]["social"] == [
        {"icon": "lucide/git-branch", "link": "https://github.com/fmind/fkf", "name": "FKF on GitHub"}
    ]
    assert config["extra_javascript"] == ["javascripts/accessibility.js"]
    accessibility = read("docs/javascripts/accessibility.js")
    for contract in ('role", "button', 'aria-controls", "__navigation', ".inert =", 'event.key === " "'):
        assert contract in accessibility

    for published_path in ("docs/fkf.schema.json", "docs/fmind-logo.webp", "docs/third-party-notices.md"):
        assert (ROOT / published_path).is_file()

    exact_site_licenses = {
        "docs/third-party/zensical-0.0.59-LICENSE.txt": "ac044e6db7ba08069f635afc1759b0ae11a7d47f79144a4ccdd16fc94ba47d1e",
        "docs/third-party/lucide-LICENSE.txt": "b495047bd93a9b06913511076f504daba17d5bbeb3e0650f3bb53a4220329c57",
        "docs/third-party/zensical-javascript-LICENSE.txt": "96e02c0a35f47e9bd44133a881d8db8c6713d6bf2e165dad33ed4bf598700fa4",
    }
    for relative, expected_digest in exact_site_licenses.items():
        assert hashlib.sha256((ROOT / relative).read_bytes()).hexdigest() == expected_digest

    for retired_path in ("docs/content", "docs/static", "docs/hugo.yaml", "docs/public"):
        assert not (ROOT / retired_path).exists()

    dependabot = read(".github/dependabot.yml")
    assert "package-ecosystem: gomod" not in dependabot
    assert "directory: /docs" not in dependabot

    cd = read(".github/workflows/cd.yml")
    pages_job = cd.split("  pages:", 1)[1]
    assert pages_job.count("mise run check:docs") == 1
    assert "mise run docs:build" not in pages_job
    assert "path: site" in pages_job
    assert pages_job.index("Verify documentation") < pages_job.index("Upload Pages artifact")
    for retired_setting in ("HUGO_ENVIRONMENT", "--baseURL", "docs/public"):
        assert retired_setting not in pages_job


def test_documentation_gate_owns_the_exact_generated_route_contract() -> None:
    check_docs_steps = tomllib.loads(read("mise.toml"))["tasks"]["check:docs"]["run"]

    assert check_docs_steps[0] == "uv run zensical build --clean --strict"
    route_gate = check_docs_steps[1]
    expected_pages = {
        "docs/base/index.html",
        "docs/commands/index.html",
        "docs/context/index.html",
        "docs/getting-started/index.html",
        "docs/harnesses/index.html",
        "docs/identities/index.html",
        "docs/index.html",
        "docs/mcp/index.html",
        "docs/okf/index.html",
        "docs/privacy/index.html",
        "docs/schema/index.html",
        "docs/sources/index.html",
        "docs/uris-graph/index.html",
        "index.html",
        "third-party-notices/index.html",
    }
    required_assets = {
        "fkf.schema.json",
        "fmind-logo.webp",
        "third-party/lucide-LICENSE.txt",
        "third-party/zensical-0.0.59-LICENSE.txt",
        "third-party/zensical-javascript-LICENSE.txt",
    }

    assert set(re.findall(r'^    "([^"]*index\.html)",$', route_gate, re.MULTILINE)) == expected_pages
    assert (
        set(
            re.findall(
                r'^    "(fkf\.schema\.json|fmind-logo\.webp|third-party/[^"]+-LICENSE\.txt)",$',
                route_gate,
                re.MULTILINE,
            )
        )
        == required_assets
    )
    assert 'site.rglob("index.html")' in route_gate
    assert "actual_pages != expected_pages" in route_gate
    assert "not (site / relative).is_file()" in route_gate
    assert "lychee --offline" in check_docs_steps[2]


def test_zensical_accessibility_script_covers_the_mobile_toc() -> None:
    accessibility = read("docs/javascripts/accessibility.js")

    for contract in (
        'document.querySelector("#__toc")',
        'label.md-sidebar-button[for="__toc"]',
        '[data-md-type="toc"] nav.md-nav--secondary',
        'navigation.id = "__toc-navigation"',
        'trigger.setAttribute("aria-label", "On this page")',
        'trigger.setAttribute("aria-controls", navigation.id)',
        "navigation.inert = mobile && !toggle.checked",
        "document$.subscribe(enhanceToc)",
    ):
        assert contract in accessibility
    assert accessibility.count('event.key === " "') == 2
    assert 'event.key === "Enter"' not in accessibility


def test_ci_and_release_use_one_python_distribution_boundary() -> None:
    ci = read(".github/workflows/ci.yml")
    cd = read(".github/workflows/cd.yml")
    security = read(".github/workflows/security.yml")

    assert "workflow_call:" in ci
    assert "mise run all" in ci
    assert "fetch-depth: 100" in ci
    assert "matrix:" in ci
    assert "ubuntu-24.04" in ci
    assert "ubuntu-24.04-arm" in ci
    assert "macos-15-intel" in ci
    assert "macos-15" in ci

    # The lockfile pins project tools; pin the bootstrap executable too so every
    # platform interprets that lock under the same mise release.
    for workflow in (ci, cd, security):
        assert "version: 2026.9.1" in workflow

    for release_contract in (
        'tags: ["v*"]',
        "mise run build",
        "python-package-distributions",
        "pypa/gh-action-pypi-publish@",
        "name: pypi",
        "id-token: write",
        "actions/attest@",
        "dist/*.whl",
        "dist/*.tar.gz",
        "--draft",
        "gh release create",
        "needs: stage_github_release",
        "needs: publish_python_package",
        'gh release edit "$GITHUB_REF_NAME" --draft=false --verify-tag',
        "release $GITHUB_REF_NAME is already public with the exact artifacts",
        "release $GITHUB_REF_NAME is already public",
    ):
        assert release_contract in cd
    assert cd.count("run: scripts/verify-release-tag") == 3
    assert cd.count("run: scripts/verify-release-assets") == 2
    for retired_release in ("goreleaser", "dist/fkf_*.tar.gz", "Publish release binaries"):
        assert retired_release not in cd

    build_job = cd.split("  build_python_package:", 1)[1].split("  attest_python_package:", 1)[0]
    assert "permissions:\n      contents: read" in build_job
    assert "id-token: write" not in build_job
    assert "attestations: write" not in build_job
    assert "artifact-metadata: write" not in build_job
    assert "actions/attest@" not in build_job
    assert "sha256sum dist/*.whl dist/*.tar.gz > python-package.sha256" in build_job

    authority_jobs = (
        (
            cd.split("  attest_python_package:", 1)[1].split("  stage_github_release:", 1)[0],
            "Attest distribution provenance",
        ),
        (
            cd.split("  stage_github_release:", 1)[1].split("  publish_python_package:", 1)[0],
            "Create or verify draft release",
        ),
        (
            cd.split("  publish_python_package:", 1)[1].split("  publish_github_release:", 1)[0],
            "Publish distributions to PyPI",
        ),
        (
            cd.split("  publish_github_release:", 1)[1].split("  pages_candidate:", 1)[0],
            "Publish draft release",
        ),
    )
    for job, authority_step in authority_jobs:
        assert "Download distributions" in job
        assert "sha256sum --check --strict python-package.sha256" in job
        assert 'test "$(find dist -maxdepth 1 -type f | wc -l)" -eq 2' in job
        assert job.index("Download distributions") < job.index("Verify distribution hashes") < job.index(authority_step)

    stage_job = authority_jobs[1][0]
    assert stage_job.index("Checkout release notes") < stage_job.index("Download distributions")
    assert stage_job.index("Create or verify draft release") < stage_job.index("Verify staged release assets")

    pypi_job = cd.split("  publish_python_package:", 1)[1].split("  publish_github_release:", 1)[0]
    assert "skip-existing: true" in pypi_job
    assert "Verify PyPI distribution integrity" in pypi_job
    assert "https://pypi.org/pypi/fkf/" in pypi_job
    assert 'entry["filename"]' in pypi_job
    assert 'entry["digests"]["sha256"]' in pypi_job
    assert "hashlib.sha256" in pypi_job
    assert "remote == expected" in pypi_job
    assert "MAX_ATTEMPTS = 6" in pypi_job
    assert "time.sleep(RETRY_SECONDS)" in pypi_job
    assert "continue-on-error:" not in pypi_job
    assert (
        cd.index("Publish distributions to PyPI")
        < cd.index("Verify PyPI distribution integrity")
        < cd.index("  publish_github_release:")
    )

    publication_job = authority_jobs[3][0]
    assert (
        publication_job.index("Recheck remote tag before GitHub publication")
        < publication_job.index("Verify final release assets")
        < publication_job.index("Publish draft release")
    )

    assert "fetch-depth: 0" in security
    assert "gitleaks git --redact=100 --verbose" in security
    assert "trivy --config trivy.yaml fs ." in security


def test_pypi_verifier_retries_transient_and_partial_visibility_then_matches(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    dist = tmp_path / "dist"
    dist.mkdir()
    wheel = dist / "fkf-5.0.0-py3-none-any.whl"
    sdist = dist / "fkf-5.0.0.tar.gz"
    wheel.write_bytes(b"wheel")
    sdist.write_bytes(b"sdist")
    (dist / f"{wheel.name}.publish.attestation").write_text("attestation")
    expected = {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in (wheel, sdist)}
    responses = iter(
        [
            urllib.error.HTTPError(
                "https://pypi.org/pypi/fkf/5.0.0/json",
                404,
                "not found",
                Message(),
                io.BytesIO(),
            ),
            pypi_payload({wheel.name: expected[wheel.name]}),
            pypi_payload(expected),
        ]
    )
    requested_urls: list[str] = []
    sleeps: list[float] = []

    def urlopen(request: urllib.request.Request, *, timeout: float) -> io.BytesIO:
        requested_urls.append(request.full_url)
        assert timeout == 15
        response = next(responses)
        if isinstance(response, urllib.error.HTTPError):
            raise response
        return response

    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("GITHUB_REF_NAME", "v5.0.0")
    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    monkeypatch.setattr(time, "sleep", sleeps.append)

    run_pypi_verifier(tmp_path)

    assert requested_urls == [
        "https://pypi.org/pypi/fkf/5.0.0/json",
        "https://pypi.org/pypi/fkf/5.0.0/json",
        "https://pypi.org/pypi/fkf/5.0.0/json",
    ]
    assert sleeps == [5, 5]
    assert "filenames and SHA-256 digests match" in capsys.readouterr().out


def test_pypi_verifier_fails_immediately_on_conflicting_digest(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    dist = tmp_path / "dist"
    dist.mkdir()
    wheel = dist / "fkf-5.0.0-py3-none-any.whl"
    wheel.write_bytes(b"wheel")
    calls = 0
    sleeps: list[float] = []

    def urlopen(_request: urllib.request.Request, *, timeout: float) -> io.BytesIO:
        nonlocal calls
        calls += 1
        assert timeout == 15
        return pypi_payload({wheel.name: "0" * 64})

    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("GITHUB_REF_NAME", "v5.0.0")
    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    monkeypatch.setattr(time, "sleep", sleeps.append)

    with pytest.raises(SystemExit, match="conflicting_sha256"):
        run_pypi_verifier(tmp_path)

    assert calls == 1
    assert sleeps == []
