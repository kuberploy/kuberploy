#!/usr/bin/env python3
"""Require a stable release tree to be an exact mechanical RC promotion."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class TreeEntry:
    mode: str
    kind: str
    object_id: str


def git(root: Path, *arguments: str) -> bytes:
    return subprocess.check_output(("git", "-C", str(root), *arguments))


def tree(root: Path, revision: str) -> dict[str, TreeEntry]:
    result: dict[str, TreeEntry] = {}
    for record in git(root, "ls-tree", "-r", "-z", "--full-tree", revision).split(b"\0"):
        if not record:
            continue
        metadata, raw_path = record.split(b"\t", 1)
        mode, kind, object_id = metadata.decode("ascii").split(" ")
        path = raw_path.decode("utf-8")
        result[path] = TreeEntry(mode=mode, kind=kind, object_id=object_id)
    return result


def blob(root: Path, revision: str, path: str) -> bytes:
    return git(root, "show", f"{revision}:{path}")


def metadata(root: Path, revision: str) -> dict[str, object]:
    value = json.loads(blob(root, revision, "release/metadata.json"))
    if not isinstance(value, dict):
        raise SystemExit("release metadata must be an object")
    return value


def validate_chart_lock(candidate: bytes, stable: bytes, candidate_version: str, stable_version: str) -> None:
    candidate_text = candidate.decode("utf-8")
    stable_text = stable.decode("utf-8")
    expected = candidate_text.replace(candidate_version, stable_version)
    candidate_digest = re.search(r"(?m)^digest: sha256:[a-f0-9]{64}$", expected)
    stable_digest = re.search(r"(?m)^digest: sha256:[a-f0-9]{64}$", stable_text)
    if candidate_digest is None or stable_digest is None:
        raise SystemExit("installer Chart.lock lacks one canonical digest")
    expected = expected.replace(candidate_digest.group(0), stable_digest.group(0), 1)
    if stable_text != expected:
        raise SystemExit("stable installer Chart.lock changed beyond version and digest")


def validate_dependency_lock(candidate: bytes, stable: bytes, candidate_version: str, stable_version: str) -> None:
    candidate_lines = candidate.decode("utf-8").splitlines()
    stable_lines = stable.decode("utf-8").splitlines()
    if len(candidate_lines) != 2 or len(stable_lines) != 2:
        raise SystemExit("installer dependencies.lock must contain exactly two entries")
    pattern = re.compile(r"^[a-f0-9]{64}  charts/(kuberploy-(?:argocd|valkey))-(.+)\.tgz$")
    for candidate_line, stable_line in zip(candidate_lines, stable_lines, strict=True):
        candidate_match = pattern.fullmatch(candidate_line)
        stable_match = pattern.fullmatch(stable_line)
        if candidate_match is None or stable_match is None:
            raise SystemExit("installer dependencies.lock contains a non-canonical entry")
        if candidate_match.group(1) != stable_match.group(1):
            raise SystemExit("stable installer dependency identity changed")
        if candidate_match.group(2) != candidate_version or stable_match.group(2) != stable_version:
            raise SystemExit("stable installer dependency version is not an exact promotion")


def validate(root: Path, candidate_revision: str, stable_revision: str) -> None:
    candidate_metadata = metadata(root, candidate_revision)
    stable_metadata = metadata(root, stable_revision)
    candidate_version = candidate_metadata.get("version")
    stable_version = stable_metadata.get("version")
    if not isinstance(stable_version, str) or "-" in stable_version:
        raise SystemExit("promotion target must be a stable semantic version")
    if not isinstance(candidate_version, str) or not re.fullmatch(
        rf"{re.escape(stable_version)}-rc\.[1-9][0-9]*", candidate_version
    ):
        raise SystemExit("promotion source must be an RC on the stable version line")
    if candidate_metadata | {"version": stable_version} != stable_metadata:
        raise SystemExit("stable release metadata changed beyond its version")

    candidate_tree = tree(root, candidate_revision)
    stable_tree = tree(root, stable_revision)
    receipt = f"release/qualifications/{stable_version}.json"
    expected_paths = set(candidate_tree) | {receipt}
    if set(stable_tree) != expected_paths:
        raise SystemExit("stable tree added or removed files beyond its qualification receipt")
    if stable_tree[receipt].mode != "100644" or stable_tree[receipt].kind != "blob":
        raise SystemExit("stable qualification receipt must be one regular tracked file")

    generated = {
        "charts/kuberploy-installer/Chart.lock": validate_chart_lock,
        "charts/kuberploy-installer/dependencies.lock": validate_dependency_lock,
    }
    for path, candidate_entry in candidate_tree.items():
        stable_entry = stable_tree[path]
        if (candidate_entry.mode, candidate_entry.kind) != (stable_entry.mode, stable_entry.kind):
            raise SystemExit(f"stable promotion changed tracked file type or mode: {path}")
        candidate_body = blob(root, candidate_revision, path)
        stable_body = blob(root, stable_revision, path)
        if path == "release/metadata.json":
            continue
        if path in generated:
            generated[path](candidate_body, stable_body, candidate_version, stable_version)
            continue
        expected = candidate_body.replace(candidate_version.encode(), stable_version.encode())
        if stable_body != expected:
            raise SystemExit(f"stable promotion contains a non-version source change: {path}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--candidate", required=True)
    parser.add_argument("--stable", default="HEAD")
    args = parser.parse_args()
    root = args.root.resolve()
    validate(root, args.candidate, args.stable)
    print("stable source is an exact mechanical RC promotion")


if __name__ == "__main__":
    main()
