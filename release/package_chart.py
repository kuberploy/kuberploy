#!/usr/bin/env python3
"""Create a release-only chart with immutable images and a disabled builder boundary."""

from __future__ import annotations

import argparse
import json
import re
import shutil
from pathlib import Path

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$")
DIGEST_REF = re.compile(r"^[^\s@]+@sha256:[a-f0-9]{64}$")


def yaml_paths(lines: list[str], replacements: dict[tuple[str, ...], str]) -> list[str]:
    stack: dict[int, str] = {}
    seen: set[tuple[str, ...]] = set()
    output: list[str] = []
    key_re = re.compile(r"^( *)([A-Za-z][A-Za-z0-9]*):(?:\s*(.*))?$")
    for line in lines:
        match = key_re.match(line.rstrip("\n"))
        if not match:
            output.append(line)
            continue
        indent = len(match.group(1))
        for level in [level for level in stack if level >= indent]:
            del stack[level]
        parents = [stack[level] for level in sorted(stack)]
        key = match.group(2)
        path = tuple(parents + [key])
        if path in replacements:
            output.append(f"{' ' * indent}{key}: {replacements[path]}\n")
            seen.add(path)
        else:
            output.append(line)
        if not (match.group(3) or "").strip():
            stack[indent] = key
    missing = sorted(".".join(path) for path in replacements if path not in seen)
    if missing:
        raise SystemExit(f"chart values paths not found: {', '.join(missing)}")
    return output


def replace_chart_version(path: Path, version: str) -> None:
    text = path.read_text(encoding="utf-8")
    text, version_count = re.subn(r"(?m)^version:\s*.*$", f"version: {version}", text)
    text, app_count = re.subn(r"(?m)^appVersion:\s*.*$", f'appVersion: "{version}"', text)
    if version_count != 1 or app_count != 1:
        raise SystemExit("Chart.yaml must contain exactly one version and appVersion")
    path.write_text(text, encoding="utf-8")


def yaml_scalar(text: str, wanted: tuple[str, ...]) -> str:
    stack: dict[int, str] = {}
    key_re = re.compile(r"^( *)([A-Za-z][A-Za-z0-9]*):(?:\s*(.*))?$")
    for line in text.splitlines():
        match = key_re.match(line)
        if not match:
            continue
        indent = len(match.group(1))
        for level in [level for level in stack if level >= indent]:
            del stack[level]
        path = tuple([stack[level] for level in sorted(stack)] + [match.group(2)])
        raw = (match.group(3) or "").strip()
        if path == wanted:
            if not raw:
                raise SystemExit(f"chart values path has no scalar: {'.'.join(wanted)}")
            if raw.startswith('"'):
                return str(json.loads(raw))
            return raw.strip("'")
        if not raw:
            stack[indent] = match.group(2)
    raise SystemExit(f"chart values path not found: {'.'.join(wanted)}")


def add_builder_dependency(path: Path, version: str) -> None:
    text = path.read_text(encoding="utf-8")
    if re.search(r"(?m)^dependencies:\s*", text):
        raise SystemExit("source Chart.yaml must not define release-only dependencies")
    if not text.endswith("\n"):
        text += "\n"
    text += (
        "dependencies:\n"
        "  - name: kuberploy-builder\n"
        "    alias: builder\n"
        f"    version: {version}\n"
        '    repository: "file://charts/kuberploy-builder"\n'
        "    condition: builder.enabled\n"
    )
    path.write_text(text, encoding="utf-8")


def allow_inherited_global(schema_path: Path) -> None:
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    properties = schema.get("properties")
    if not isinstance(properties, dict) or "global" in properties:
        raise SystemExit("builder values schema has an unexpected properties contract")
    properties["global"] = {
        "type": "object",
        "additionalProperties": False,
        "required": ["testRun", "requireImageDigest"],
        "properties": {
            "testRun": {
                "type": "string",
                "maxLength": 20,
                "pattern": "^(?:|[a-z0-9][a-z0-9-]{0,19})$",
            },
            "requireImageDigest": {"type": "boolean"},
        },
    }
    schema_path.write_text(json.dumps(schema, indent=2) + "\n", encoding="utf-8")


def reject_source_links(directory: Path) -> None:
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise SystemExit(f"release chart source may not contain links: {path}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--builder-chart", required=True, type=Path)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument("--version", required=True)
    for component in ("api", "worker", "web", "migration"):
        parser.add_argument(f"--{component}-image", required=True)
    parser.add_argument("--builder-agent-image", required=True)
    args = parser.parse_args()

    if not SEMVER.fullmatch(args.version):
        raise SystemExit(f"release version is not semantic version text: {args.version}")
    images = {name: getattr(args, f"{name}_image") for name in ("api", "worker", "web", "migration")}
    images["builder-agent"] = args.builder_agent_image
    for name, reference in images.items():
        if not DIGEST_REF.fullmatch(reference):
            raise SystemExit(f"{name} image must be reference@sha256:<64hex>")
        if ":latest" in reference.lower():
            raise SystemExit(f"{name} image uses a forbidden latest tag")

    if args.destination.exists():
        raise SystemExit(f"destination already exists: {args.destination}")
    for required in ("Chart.yaml", "values.yaml", "values.schema.json", "templates"):
        if not (args.builder_chart / required).exists():
            raise SystemExit(f"builder chart is missing required path: {args.builder_chart / required}")
    reject_source_links(args.source)
    reject_source_links(args.builder_chart)

    builder_metadata = (args.builder_chart / "Chart.yaml").read_text(encoding="utf-8")
    if yaml_scalar(builder_metadata, ("name",)) != "kuberploy-builder":
        raise SystemExit("embedded builder chart must be named kuberploy-builder")
    if re.search(r"(?m)^dependencies:\s*", builder_metadata):
        raise SystemExit("embedded builder chart may not carry nested dependencies")

    source_values = (args.source / "values.yaml").read_text(encoding="utf-8")
    builder_values = (args.builder_chart / "values.yaml").read_text(encoding="utf-8")
    if yaml_scalar(source_values, ("builder", "enabled")) != "false":
        raise SystemExit("release control-plane chart must keep builder.enabled=false")
    if yaml_scalar(builder_values, ("enabled",)) != "false":
        raise SystemExit("embedded builder chart must remain disabled by default")

    shutil.copytree(args.source, args.destination)
    replace_chart_version(args.destination / "Chart.yaml", args.version)
    add_builder_dependency(args.destination / "Chart.yaml", args.version)

    embedded_builder = args.destination / "charts" / "kuberploy-builder"
    embedded_builder.parent.mkdir(parents=True, exist_ok=False)
    shutil.copytree(args.builder_chart, embedded_builder, ignore=shutil.ignore_patterns("testdata"))
    replace_chart_version(embedded_builder / "Chart.yaml", args.version)
    allow_inherited_global(embedded_builder / "values.schema.json")

    embedded_values_path = embedded_builder / "values.yaml"
    embedded_values_path.write_text(
        "".join(
            yaml_paths(
                embedded_values_path.read_text(encoding="utf-8").splitlines(True),
                {("builderAgentImage",): json.dumps(args.builder_agent_image)},
            )
        ),
        encoding="utf-8",
    )

    values_path = args.destination / "values.yaml"
    replacements = {
        ("global", "requireImageDigest"): "true",
        ("builder", "builderAgentImage"): json.dumps(args.builder_agent_image),
    }
    for name, reference in images.items():
        if name == "builder-agent":
            continue
        replacements[("components", name, "image", "reference")] = json.dumps(reference)
    values_path.write_text(
        "".join(yaml_paths(values_path.read_text(encoding="utf-8").splitlines(True), replacements)),
        encoding="utf-8",
    )
    print(args.destination)


if __name__ == "__main__":
    main()
