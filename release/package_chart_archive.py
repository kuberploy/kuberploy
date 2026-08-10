#!/usr/bin/env python3
"""Create a byte-reproducible Helm chart archive."""

from __future__ import annotations

import argparse
import gzip
import os
import tarfile
from pathlib import Path

from validate_semantics import yaml_scalar


def package_chart_archive(source: Path, output: Path, source_date_epoch: int) -> None:
    """Write one chart archive with stable ordering, metadata, and gzip bytes."""
    if source_date_epoch < 0:
        raise SystemExit("source-date-epoch must be non-negative")
    if not source.is_dir():
        raise SystemExit(f"chart source is not a directory: {source}")
    if output.exists():
        raise SystemExit(f"chart archive already exists: {output}")

    chart_text = (source / "Chart.yaml").read_text(encoding="utf-8")
    chart_name = yaml_scalar(chart_text, ("name",))
    if not chart_name or "/" in chart_name or chart_name in {".", ".."}:
        raise SystemExit(f"invalid chart name: {chart_name!r}")

    paths = sorted(source.rglob("*"), key=lambda path: path.relative_to(source).as_posix())
    if not paths:
        raise SystemExit("chart source is empty")
    output.parent.mkdir(parents=True, exist_ok=True)

    with output.open("xb") as destination:
        with gzip.GzipFile(
            filename="",
            mode="wb",
            compresslevel=9,
            fileobj=destination,
            mtime=source_date_epoch,
        ) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                for path in paths:
                    relative = path.relative_to(source).as_posix()
                    member = tarfile.TarInfo(f"{chart_name}/{relative}")
                    member.mtime = source_date_epoch
                    member.uid = 0
                    member.gid = 0
                    member.uname = ""
                    member.gname = ""
                    if path.is_symlink():
                        raise SystemExit(f"chart archive does not allow symlinks: {path}")
                    if path.is_dir():
                        member.type = tarfile.DIRTYPE
                        member.mode = 0o755
                        archive.addfile(member)
                    elif path.is_file():
                        member.type = tarfile.REGTYPE
                        member.mode = 0o644
                        member.size = path.stat().st_size
                        with path.open("rb") as chart_file:
                            archive.addfile(member, chart_file)
                    else:
                        raise SystemExit(f"chart archive contains an unsupported file type: {path}")

    os.utime(output, (source_date_epoch, source_date_epoch))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    args = parser.parse_args()

    package_chart_archive(args.source, args.output, args.source_date_epoch)
    print(args.output)


if __name__ == "__main__":
    main()
