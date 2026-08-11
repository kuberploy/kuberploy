#!/usr/bin/env python3
"""Generate the canonical, deterministic Kuberploy release manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from datetime import datetime
from pathlib import Path

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$")
SHA = re.compile(r"^[a-f0-9]{40}$")
DIGEST = re.compile(r"^sha256:[a-f0-9]{64}$")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return "sha256:" + digest.hexdigest()


def migration_identity(directory: Path) -> tuple[str, str, str]:
    migrations = sorted(
        path for path in (directory / "prisma" / "migrations").glob("[0-9][0-9][0-9]_*")
        if path.is_dir() and (path / "migration.sql").is_file()
    )
    if not migrations:
        raise SystemExit("no ordered Prisma SQL migrations found")
    digest = hashlib.sha256()
    for path in migrations:
        body = path / "migration.sql"
        digest.update(path.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(body.read_bytes())
        digest.update(b"\0")
    return migrations[-1].name, migrations[0].name, "sha256:" + digest.hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--repository", default="kuberploy/kuberploy")
    parser.add_argument("--commit", required=True)
    parser.add_argument("--created-at", required=True)
    parser.add_argument("--notes-url", required=True)
    parser.add_argument("--kubernetes-constraint", required=True)
    for component in ("api", "worker", "web", "migration", "upgrader", "builder-agent"):
        parser.add_argument(f"--{component}-reference", required=True)
        parser.add_argument(f"--{component}-digest", required=True)
    parser.add_argument("--summary", required=True)
    parser.add_argument("--breaking-changes", action="store_true")
    parser.add_argument("--supported-upgrade-from", required=True)
    parser.add_argument("--chart-oci-reference", required=True)
    parser.add_argument("--chart-oci-digest", required=True)
    parser.add_argument("--chart-package", required=True, type=Path)
    parser.add_argument(
        "--component-chart",
        action="append",
        nargs=3,
        metavar=("NAME", "PACKAGE", "OCI_DIGEST"),
        required=True,
    )
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    if not args.tag.startswith("v") or not SEMVER.fullmatch(args.tag[1:]):
        raise SystemExit(f"tag must be a semantic vMAJOR.MINOR.PATCH release: {args.tag}")
    version = args.tag[1:]
    if not SHA.fullmatch(args.commit):
        raise SystemExit("source commit must be a lowercase 40-character Git SHA")
    try:
        created = datetime.fromisoformat(args.created_at.replace("Z", "+00:00"))
    except ValueError as error:
        raise SystemExit(f"invalid created-at timestamp: {error}") from error
    if created.tzinfo is None:
        raise SystemExit("created-at timestamp must include a timezone")

    if not 1 <= len(args.summary) <= 500:
        raise SystemExit("release summary must contain 1 to 500 characters")
    semver_part = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?"
    range_match = re.fullmatch(rf">=({semver_part}) <({semver_part})", args.supported_upgrade_from)
    if not range_match:
        raise SystemExit("supported-upgrade-from must use exact semantic version range syntax")
    lower = tuple(int(part) for part in range_match.group(1).split("-", 1)[0].split("."))
    upper = tuple(int(part) for part in range_match.group(2).split("-", 1)[0].split("."))
    if lower >= upper:
        raise SystemExit("supported-upgrade-from lower bound must be less than its upper bound")

    images = []
    components = (
        ("api", "api"),
        ("worker", "worker"),
        ("web", "web"),
        ("migration", "migration"),
        ("upgrader", "upgrader"),
        ("builder_agent", "builder-agent"),
    )
    for argument_name, component in components:
        reference = getattr(args, f"{argument_name}_reference")
        digest = getattr(args, f"{argument_name}_digest")
        if not DIGEST.fullmatch(digest):
            raise SystemExit(f"invalid {component} image digest")
        images.append(
            {
                "component": component,
                "reference": reference,
                "digest": digest,
                "platforms": ["linux/amd64", "linux/arm64"],
            }
        )
    if not DIGEST.fullmatch(args.chart_oci_digest):
        raise SystemExit("invalid chart OCI digest")

    component_chart_names = (
        "kuberploy-argocd",
        "kuberploy-installer",
        "kuberploy-builder",
        "kuberploy-cert-manager",
        "kuberploy-edge",
        "kuberploy-external-dns",
        "kuberploy-external-secrets",
        "kuberploy-monitoring",
        "kuberploy-postgresql",
        "kuberploy-registry",
        "kuberploy-runtime",
        "kuberploy-sealed-secrets",
        "kuberploy-valkey",
    )
    if [entry[0] for entry in args.component_chart] != list(component_chart_names):
        raise SystemExit("component charts must use the exact canonical order")
    component_charts = []
    for name, package_value, oci_digest in args.component_chart:
        package = Path(package_value)
        if package.name != f"{name}-{version}.tgz" or not package.is_file():
            raise SystemExit(f"invalid or missing component chart package: {name}")
        if not DIGEST.fullmatch(oci_digest):
            raise SystemExit(f"invalid component chart OCI digest: {name}")
        component_charts.append(
            {
                "name": name,
                "version": version,
                "ociReference": f"ghcr.io/kuberploy/charts/{name}:{version}",
                "ociDigest": oci_digest,
                "package": package.name,
                "packageSha256": sha256(package),
            }
        )

    current_schema, minimum_schema, migrations_digest = migration_identity(args.root / "migrations")
    dependency_lock = args.root / "DEPENDENCIES.md"
    manifest = {
        "$schema": f"https://raw.githubusercontent.com/{args.repository}/{args.commit}/release/release-manifest.schema.json",
        "schemaVersion": "1.0.0",
        "release": {
            "tag": args.tag,
            "version": version,
            "createdAt": args.created_at,
            "notesUrl": args.notes_url,
            "summary": args.summary,
            "breakingChanges": args.breaking_changes,
        },
        "source": {"repository": args.repository, "commit": args.commit},
        "versions": {
            name: version
            for name in ("kuberploy", "api", "worker", "web", "migration", "upgrader", "builderAgent", "chart")
        },
        "compatibility": {
            "supportedUpgradeFrom": args.supported_upgrade_from,
            "kubernetes": {
                "constraint": args.kubernetes_constraint,
                "testedMinors": ["1.34", "1.35", "1.36"],
            },
            "database": {
                "engine": "postgresql",
                "currentSchema": current_schema,
                "minimumUpgradeableSchema": minimum_schema,
                "migrationSetSha256": migrations_digest,
                "strategy": "prisma-migrate-deploy-with-advisory-lock",
                "rollbackPolicy": (
                    "Roll back only to a control-plane release whose manifest accepts the current database schema; "
                    "Helm never rolls back tenant workloads."
                ),
            },
        },
        "artifacts": {
            "images": images,
            "chart": {
                "name": "kuberploy",
                "version": version,
                "ociReference": args.chart_oci_reference,
                "ociDigest": args.chart_oci_digest,
                "package": args.chart_package.name,
                "packageSha256": sha256(args.chart_package),
            },
            "componentCharts": component_charts,
        },
        "dependencyLock": {"file": "DEPENDENCIES.md", "sha256": sha256(dependency_lock)},
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
