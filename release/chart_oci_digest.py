#!/usr/bin/env python3
"""Predict the OCI manifest digest produced by Helm 4 for a chart package."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from datetime import datetime
from pathlib import Path

from validate_semantics import yaml_scalar

CONFIG_MEDIA_TYPE = "application/vnd.cncf.helm.config.v1+json"
CHART_MEDIA_TYPE = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
REQUIRED_TOP_LEVEL = {
    "apiVersion",
    "name",
    "description",
    "type",
    "version",
    "appVersion",
    "kubeVersion",
}
OPTIONAL_TOP_LEVEL = {"annotations", "dependencies"}


def compact_json(value: object) -> bytes:
    # Match Go's encoding/json defaults used by Helm, including HTML escaping.
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    encoded = encoded.replace("&", r"\u0026").replace("<", r"\u003c").replace(">", r"\u003e")
    encoded = encoded.replace("\u2028", r"\u2028").replace("\u2029", r"\u2029")
    return encoded.encode("utf-8")


def digest(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


def parse_chart(path: Path) -> tuple[dict[str, object], dict[str, str]]:
    text = path.read_text(encoding="utf-8")
    top_level = {
        match.group(1)
        for line in text.splitlines()
        if (match := re.fullmatch(r"([A-Za-z][A-Za-z0-9]*):(?:\s*.*)?", line))
    }
    if not REQUIRED_TOP_LEVEL.issubset(top_level) or not top_level.issubset(REQUIRED_TOP_LEVEL | OPTIONAL_TOP_LEVEL):
        difference = sorted(top_level.symmetric_difference(REQUIRED_TOP_LEVEL))
        raise SystemExit(f"unsupported Chart.yaml metadata fields: {', '.join(difference)}")

    annotation_lines: list[str] = []
    in_annotations = False
    for line in text.splitlines():
        if line == "annotations:":
            in_annotations = True
            continue
        if in_annotations and line and not line.startswith(" "):
            in_annotations = False
        if in_annotations and line.strip():
            annotation_lines.append(line)
    annotations: dict[str, str] = {}
    for line in annotation_lines:
        match = re.fullmatch(r"  ([A-Za-z0-9][A-Za-z0-9./_-]*):\s+([^#\s][^#]*?)\s*", line)
        if not match:
            raise SystemExit(f"unsupported Chart.yaml annotation syntax: {line}")
        annotations[match.group(1)] = match.group(2).strip('"\'')
    dependencies: list[dict[str, str]] = []
    dependency: dict[str, str] | None = None
    in_dependencies = False
    for line in text.splitlines():
        if line == "dependencies:":
            in_dependencies = True
            continue
        if in_dependencies and line and not line.startswith(" "):
            break
        if not in_dependencies or not line.strip():
            continue
        match = re.fullmatch(r"  - name:\s+([^#\s][^#]*?)\s*", line)
        if match:
            if dependency is not None:
                dependencies.append(dependency)
            dependency = {"name": match.group(1).strip('"\'')}
            continue
        match = re.fullmatch(r"    ([A-Za-z][A-Za-z0-9]*):\s+([^#\s][^#]*?)\s*", line)
        if not match or dependency is None or match.group(1) not in {"alias", "version", "repository", "condition"}:
            raise SystemExit(f"unsupported Chart.yaml dependency syntax: {line}")
        key = match.group(1)
        if key in dependency:
            raise SystemExit(f"duplicate Chart.yaml dependency key: {key}")
        dependency[key] = match.group(2).strip('"\'')
    if dependency is not None:
        dependencies.append(dependency)
    if ("dependencies" in top_level) != bool(dependencies):
        raise SystemExit("Chart.yaml dependencies must be a non-empty list when declared")

    normalized_dependencies: list[dict[str, str]] = []
    for item in dependencies:
        if not {"name", "version", "repository"}.issubset(item):
            raise SystemExit("Chart.yaml dependency lacks name, version, or repository")
        normalized: dict[str, str] = {
            "name": item["name"],
            "version": item["version"],
            "repository": item["repository"],
        }
        if "condition" in item:
            normalized["condition"] = item["condition"]
        if "alias" in item:
            normalized["alias"] = item["alias"]
        normalized_dependencies.append(normalized)

    scalar = lambda key: yaml_scalar(text, (key,))
    metadata: dict[str, object] = {
        "name": scalar("name"),
        "version": scalar("version"),
        "description": scalar("description"),
        "apiVersion": scalar("apiVersion"),
        "appVersion": scalar("appVersion"),
    }
    if annotations:
        metadata["annotations"] = dict(sorted(annotations.items()))
    metadata["kubeVersion"] = scalar("kubeVersion")
    if normalized_dependencies:
        metadata["dependencies"] = normalized_dependencies
    metadata["type"] = scalar("type")
    return metadata, annotations


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--chart", required=True, type=Path)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--created-at", required=True)
    args = parser.parse_args()

    try:
        created_at = datetime.fromisoformat(args.created_at.replace("Z", "+00:00"))
    except ValueError as error:
        raise SystemExit(f"invalid created-at timestamp: {error}") from error
    if created_at.tzinfo is None or created_at.isoformat(timespec="seconds").replace("+00:00", "Z") != args.created_at:
        raise SystemExit("created-at must be a canonical UTC RFC3339 timestamp")

    metadata, chart_annotations = parse_chart(args.chart)
    package = args.package.read_bytes()
    config = compact_json(metadata)
    config_descriptor = {
        "mediaType": CONFIG_MEDIA_TYPE,
        "digest": digest(config),
        "size": len(config),
    }
    chart_descriptor = {
        "mediaType": CHART_MEDIA_TYPE,
        "digest": digest(package),
        "size": len(package),
    }
    annotations = {
        "org.opencontainers.image.created": args.created_at,
        "org.opencontainers.image.description": str(metadata["description"]),
        "org.opencontainers.image.title": str(metadata["name"]),
        "org.opencontainers.image.version": str(metadata["version"]),
        **chart_annotations,
    }
    manifest = {
        "schemaVersion": 2,
        "config": config_descriptor,
        "layers": [chart_descriptor],
        "annotations": dict(sorted(annotations.items())),
    }
    print(digest(compact_json(manifest)))


if __name__ == "__main__":
    main()
