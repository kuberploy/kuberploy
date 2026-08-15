#!/usr/bin/env python3
"""Mutation tests for public-source local workstation exclusions."""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path


def validate(checker: Path, fixture: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(checker), "--root", str(fixture)],
        check=False,
        capture_output=True,
        text=True,
    )


def main() -> None:
    root = Path(__file__).resolve().parent.parent
    checker = root / "release/check_public_source.py"
    with tempfile.TemporaryDirectory(prefix="kuberploy-public-source-test-") as temporary:
        fixture = Path(temporary)
        subprocess.run(["git", "init", "-q", str(fixture)], check=True)
        tracked = fixture / "fixture.txt"
        tracked.write_text(
            "\n".join(
                (
                    "https://kuberploy.example.com",
                    "https://orbital.example.com",
                    "https://notorb" + ".local",
                    "https://orb" + ".local.example.com",
                )
            ),
            encoding="utf-8",
        )
        subprocess.run(["git", "-C", str(fixture), "add", "fixture.txt"], check=True)
        baseline = validate(checker, fixture)
        if baseline.returncode != 0:
            raise SystemExit(f"public-source validator rejected neutral domains: {baseline.stderr}")

        forbidden = (
            "https://kuberploy.k8s." + "OrB" + ".LoCaL",
            "https://" + "ORB" + ".LOCAL:8443",
            "https://hello.e2e." + "orb" + ".local/path",
            "local runtime: " + "Orb" + "Stack",
        )
        for value in forbidden:
            tracked.write_text(value + "\n", encoding="utf-8")
            result = validate(checker, fixture)
            output = result.stdout + result.stderr
            if result.returncode == 0 or "local workstation" not in output:
                raise SystemExit(f"public-source validator accepted local material: {value}")

    print("public source local-domain mutation tests passed")


if __name__ == "__main__":
    main()
