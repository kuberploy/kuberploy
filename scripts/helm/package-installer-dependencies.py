#!/usr/bin/env python3
"""Build and verify deterministic source-checkout installer dependencies."""

from __future__ import annotations

import argparse
import hashlib
import re
import shutil
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "release"))

from package_chart_archive import package_chart_archive  # noqa: E402
from package_component_charts import locked_upstreams, reject_links, replace_scalar  # noqa: E402
from validate_semantics import yaml_scalar  # noqa: E402

SHA256 = re.compile(r"^[a-f0-9]{64}$")
EPOCH = re.compile(r"^(0|[1-9][0-9]*)\n$")
COMPONENTS = ("kuberploy-argocd", "kuberploy-valkey")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def source_date_epoch(path: Path) -> int:
    text = path.read_text(encoding="utf-8")
    if not EPOCH.fullmatch(text):
        raise SystemExit("installer dependency source-date epoch must be one canonical integer line")
    value = int(text)
    if value > 4_102_444_800:
        raise SystemExit("installer dependency source-date epoch is outside the supported range")
    return value


def expected_lock(path: Path, version: str) -> dict[str, str]:
    lines = path.read_text(encoding="utf-8").splitlines()
    expected_names = [f"charts/{component}-{version}.tgz" for component in COMPONENTS]
    if len(lines) != len(expected_names):
        raise SystemExit("installer dependencies.lock must contain exactly two entries")
    result: dict[str, str] = {}
    for line, expected_name in zip(lines, expected_names, strict=True):
        match = re.fullmatch(r"([a-f0-9]{64})  (charts/[A-Za-z0-9._+-]+\.tgz)", line)
        if match is None or match.group(2) != expected_name or not SHA256.fullmatch(match.group(1)):
            raise SystemExit("installer dependencies.lock has a non-canonical entry")
        result[Path(expected_name).name] = match.group(1)
    return result


def stage_wrapper(root: Path, temporary: Path, component: str, version: str) -> Path:
    source = root / "charts" / component
    reject_links(source)
    destination = temporary / component
    shutil.copytree(source, destination, ignore=shutil.ignore_patterns("testdata", "charts"))
    chart = destination / "Chart.yaml"
    source_chart = (source / "Chart.yaml").read_text(encoding="utf-8")
    source_version = yaml_scalar(source_chart, ("version",))
    if yaml_scalar(source_chart, ("name",)) != component or source_version != version:
        raise SystemExit(f"installer wrapper source identity mismatch: {component}")
    replace_scalar(chart, "version", version)
    if yaml_scalar(source_chart, ("appVersion",)) == source_version:
        replace_scalar(chart, "appVersion", version)

    upstreams = locked_upstreams(source)
    if len(upstreams) != 1:
        raise SystemExit(f"installer wrapper must have one locked upstream: {component}")
    checksum, filename, _ = upstreams[0]
    upstream = source / "charts" / filename
    if not upstream.is_file() or sha256(upstream) != checksum:
        raise SystemExit(f"locked upstream dependency is absent or changed: {component}/{filename}")
    nested = destination / "charts"
    nested.mkdir(mode=0o755)
    shutil.copyfile(upstream, nested / filename)
    return destination


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument("--lock", required=True, type=Path)
    parser.add_argument("--source-date-epoch-file", required=True, type=Path)
    args = parser.parse_args()

    root = args.root.resolve()
    if args.destination.exists():
        raise SystemExit("installer dependency destination must not already exist")
    version = yaml_scalar(
        (root / "charts/kuberploy-installer/Chart.yaml").read_text(encoding="utf-8"),
        ("version",),
    )
    lock = expected_lock(args.lock, version)
    epoch = source_date_epoch(args.source_date_epoch_file)

    with tempfile.TemporaryDirectory(prefix="kuberploy-installer-dependencies-") as temporary_value:
        temporary = Path(temporary_value)
        packages = temporary / "packages"
        packages.mkdir()
        for component in COMPONENTS:
            staged = stage_wrapper(root, temporary / "staged", component, version)
            package = packages / f"{component}-{version}.tgz"
            package_chart_archive(staged, package, epoch)
            actual = sha256(package)
            if actual != lock[package.name]:
                raise SystemExit(
                    f"installer dependency checksum mismatch for {package.name}: "
                    f"expected {lock[package.name]}, got {actual}"
                )
        shutil.copytree(packages, args.destination)


if __name__ == "__main__":
    main()
