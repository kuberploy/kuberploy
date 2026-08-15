#!/usr/bin/env python3
"""Reject local/private material from the public source tree."""

from __future__ import annotations

import argparse
import re
import subprocess
from pathlib import Path


FORBIDDEN_PREFIXES = (
    ".secrets/",
    ".local/",
    ".checkpoints/",
    "deploy/",
    "scripts/" + "orb" + "stack/",
)
FORBIDDEN_TEXT = (
    (re.compile("orb" + "stack", re.IGNORECASE), "local workstation platform name"),
    (
        re.compile(
            r"(?<![A-Za-z0-9-])(?:[A-Za-z0-9-]+\.)*" + "orb" + r"\.local(?![A-Za-z0-9.-])",
            re.IGNORECASE,
        ),
        "local workstation cluster domain",
    ),
    (re.compile(r"/Users/[A-Za-z0-9._-]+/"), "macOS user home path"),
    (re.compile("(?:admin" + "chatmate|torqe" + "soft)\\.com", re.IGNORECASE), "private test domain"),
    (re.compile(r"\b4543" + r"722\b"), "private GitHub App identifier"),
    (re.compile(r"\bIv23liuw" + r"UJXB6lYoGl3q\b"), "private GitHub OAuth client identifier"),
)


def tracked_files(root: Path) -> list[Path]:
    result = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        check=True,
        capture_output=True,
    )
    return [Path(item.decode("utf-8")) for item in result.stdout.split(b"\0") if item]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    args = parser.parse_args()
    root = args.root.resolve()

    failures: list[str] = []
    for relative in tracked_files(root):
        name = relative.as_posix()
        path = root / relative
        if not path.is_file():
            continue
        if any(name == prefix.rstrip("/") or name.startswith(prefix) for prefix in FORBIDDEN_PREFIXES):
            failures.append(f"forbidden tracked local path: {name}")
            continue
        try:
            content = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        for pattern, description in FORBIDDEN_TEXT:
            if pattern.search(content):
                failures.append(f"{description} in tracked file: {name}")

    if failures:
        raise SystemExit("public source validation failed:\n- " + "\n- ".join(sorted(set(failures))))
    print("public source contains no tracked local/private material")


if __name__ == "__main__":
    main()
