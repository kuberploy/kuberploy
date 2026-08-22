#!/usr/bin/env python3
"""Mutation tests for exact stable promotion validation."""

from __future__ import annotations

import json
import subprocess
import tempfile
from pathlib import Path

from validate_promotion import validate


def run(root: Path, *arguments: str) -> None:
    subprocess.run(("git", "-C", str(root), *arguments), check=True, stdout=subprocess.DEVNULL)


def write_tree(root: Path, version: str, extra: str = "") -> None:
    (root / "charts/kuberploy-installer").mkdir(parents=True, exist_ok=True)
    (root / "release").mkdir(exist_ok=True)
    (root / "release/metadata.json").write_text(
        json.dumps(
            {
                "version": version,
                "summary": "Qualified release",
                "breakingChanges": False,
                "supportedUpgradeFrom": ">=0.1.0 <0.2.0",
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (root / "version.txt").write_text(f"version={version}\n{extra}", encoding="utf-8")
    (root / "charts/kuberploy-installer/Chart.lock").write_text(
        "dependencies:\n"
        f"- name: kuberploy-valkey\n  version: {version}\n"
        f"digest: sha256:{'a' * 64}\n"
        'generated: "2026-08-14T13:37:13Z"\n',
        encoding="utf-8",
    )
    (root / "charts/kuberploy-installer/dependencies.lock").write_text(
        f"{'b' * 64}  charts/kuberploy-argocd-{version}.tgz\n"
        f"{'c' * 64}  charts/kuberploy-valkey-{version}.tgz\n",
        encoding="utf-8",
    )


def commit(root: Path, message: str) -> str:
    run(root, "add", ".")
    run(root, "commit", "-m", message)
    return subprocess.check_output(("git", "-C", str(root), "rev-parse", "HEAD"), text=True).strip()


def expect_reject(root: Path, candidate: str, expected: str) -> None:
    try:
        validate(root, candidate, "HEAD")
    except SystemExit as error:
        if expected not in str(error):
            raise AssertionError(f"wrong rejection: {error}") from error
    else:
        raise AssertionError("promotion validator accepted a mutation")


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="kuberploy-promotion-test-") as temporary:
        root = Path(temporary)
        run(root, "init", "-q")
        run(root, "config", "user.name", "Kuberploy Test")
        run(root, "config", "user.email", "test@example.test")
        write_tree(root, "0.1.0-rc.308")
        candidate = commit(root, "candidate")

        write_tree(root, "0.1.0")
        receipt = root / "release/qualifications/0.1.0.json"
        receipt.parent.mkdir(parents=True)
        receipt.write_text("{}\n", encoding="utf-8")
        (root / "charts/kuberploy-installer/Chart.lock").write_text(
            (root / "charts/kuberploy-installer/Chart.lock").read_text().replace("a" * 64, "d" * 64),
            encoding="utf-8",
        )
        (root / "charts/kuberploy-installer/dependencies.lock").write_text(
            (root / "charts/kuberploy-installer/dependencies.lock").read_text()
            .replace("b" * 64, "e" * 64)
            .replace("c" * 64, "f" * 64),
            encoding="utf-8",
        )
        stable = commit(root, "stable")
        validate(root, candidate, stable)

        (root / "version.txt").write_text("version=0.1.0\nunqualified change\n", encoding="utf-8")
        commit(root, "mutate source")
        expect_reject(root, candidate, "non-version source change")
        run(root, "reset", "--hard", stable)

        (root / "unexpected.txt").write_text("unexpected\n", encoding="utf-8")
        commit(root, "add file")
        expect_reject(root, candidate, "added or removed files")
        run(root, "reset", "--hard", stable)

        metadata = json.loads((root / "release/metadata.json").read_text(encoding="utf-8"))
        metadata["summary"] = "Unqualified summary"
        (root / "release/metadata.json").write_text(json.dumps(metadata) + "\n", encoding="utf-8")
        commit(root, "mutate metadata")
        expect_reject(root, candidate, "metadata changed")

    print("stable promotion validator mutation tests passed")


if __name__ == "__main__":
    main()
