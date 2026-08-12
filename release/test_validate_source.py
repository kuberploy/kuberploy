#!/usr/bin/env python3
"""Mutation tests for release workflow policy validation."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

from validate_source import validate_stable_qualification


def run_validator(root: Path, fixture: Path, workflow: str) -> subprocess.CompletedProcess[str]:
    (fixture / ".github/workflows/release.yml").write_text(workflow, encoding="utf-8")
    return subprocess.run(
        [sys.executable, str(root / "release/validate_source.py"), "--root", str(fixture)],
        check=False,
        capture_output=True,
        text=True,
    )


def main() -> None:
    root = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix="kuberploy-stable-qualification-") as temporary:
        qualification_root = Path(temporary)
        validate_stable_qualification(qualification_root, "0.1.0-rc.88")
        try:
            validate_stable_qualification(qualification_root, "0.1.0")
        except SystemExit as error:
            if "requires a reviewed final-RC" not in str(error):
                raise
        else:
            raise SystemExit("validator accepted a stable release without qualification")

        receipt_directory = qualification_root / "release/qualifications"
        receipt_directory.mkdir(parents=True)
        valid_receipt = {
            "candidateVersion": "0.1.0-rc.88",
            "candidateTag": "v0.1.0-rc.88",
            "candidateCommit": "a" * 40,
            "qualificationReportSHA256": f"sha256:{'b' * 64}",
            "qualificationCompletedAt": "2026-08-12T03:00:00Z",
            "status": "passed",
            "teardownStatus": "passed",
        }
        receipt_path = receipt_directory / "0.1.0.json"
        receipt_path.write_text(json.dumps(valid_receipt), encoding="utf-8")
        validate_stable_qualification(qualification_root, "0.1.0")
        invalid_receipt = valid_receipt | {"candidateVersion": "0.2.0-rc.1"}
        receipt_path.write_text(json.dumps(invalid_receipt), encoding="utf-8")
        try:
            validate_stable_qualification(qualification_root, "0.1.0")
        except SystemExit as error:
            if "final RC of this version" not in str(error):
                raise
        else:
            raise SystemExit("validator accepted a receipt for a different release line")

    workflow = (root / ".github/workflows/release.yml").read_text(encoding="utf-8")
    ci_workflow = (root / ".github/workflows/ci.yml").read_text(encoding="utf-8")
    dependabot = (root / ".github/dependabot.yml").read_text(encoding="utf-8")
    with tempfile.TemporaryDirectory(prefix="kuberploy-validate-source-") as temporary:
        fixture = Path(temporary)
        (fixture / ".github/workflows").mkdir(parents=True)
        (fixture / ".github/workflows/ci.yml").write_text(ci_workflow, encoding="utf-8")
        (fixture / ".github/dependabot.yml").write_text(dependabot, encoding="utf-8")
        (fixture / "release").mkdir()
        for name in ("build", "charts", "scripts", "web"):
            os.symlink(root / name, fixture / name, target_is_directory=True)
        (fixture / "migrations").mkdir()
        os.symlink(
            root / "migrations/prisma",
            fixture / "migrations/prisma",
            target_is_directory=True,
        )
        migration_package = (root / "migrations/package.json").read_text(encoding="utf-8")
        (fixture / "migrations/package.json").write_text(
            migration_package,
            encoding="utf-8",
        )
        os.symlink(root / "release/metadata.json", fixture / "release/metadata.json")

        baseline = run_validator(root, fixture, workflow)
        if baseline.returncode != 0:
            raise SystemExit(f"baseline release source was rejected: {baseline.stderr}")

        (fixture / ".github/workflows/ci.yml").write_text(
            ci_workflow.replace("actions/checkout@v7", "actions/checkout@main", 1),
            encoding="utf-8",
        )
        invalid_ci = run_validator(root, fixture, workflow)
        if invalid_ci.returncode == 0 or "not pinned to one major version" not in (
            invalid_ci.stdout + invalid_ci.stderr
        ):
            raise SystemExit("validator accepted a non-major action selector in ci.yml")
        (fixture / ".github/workflows/ci.yml").write_text(ci_workflow, encoding="utf-8")

        (fixture / ".github/workflows/ci.yml").write_text(
            ci_workflow.replace("        run: make prisma-migration-test\n", "", 1),
            encoding="utf-8",
        )
        missing_migration_ci = run_validator(root, fixture, workflow)
        if missing_migration_ci.returncode == 0 or "production Prisma migration image" not in (
            missing_migration_ci.stdout + missing_migration_ci.stderr
        ):
            raise SystemExit("validator accepted CI without the production migration-image test")
        (fixture / ".github/workflows/ci.yml").write_text(ci_workflow, encoding="utf-8")

        (fixture / ".github/dependabot.yml").write_text(
            dependabot.replace("    directory: /migrations\n", "    directory: /missing-migrations\n", 1),
            encoding="utf-8",
        )
        missing_dependabot = run_validator(root, fixture, workflow)
        if missing_dependabot.returncode == 0 or "every shipped dependency surface" not in (
            missing_dependabot.stdout + missing_dependabot.stderr
        ):
            raise SystemExit("validator accepted incomplete Dependabot coverage")
        (fixture / ".github/dependabot.yml").write_text(dependabot, encoding="utf-8")

        (fixture / "migrations/package.json").write_text(
            migration_package.replace('"prisma@7.9.1": true', '"prisma@7.9.1": false', 1),
            encoding="utf-8",
        )
        unsafe_prisma_scripts = run_validator(root, fixture, workflow)
        if unsafe_prisma_scripts.returncode == 0 or "approve only the pinned Prisma" not in (
            unsafe_prisma_scripts.stdout + unsafe_prisma_scripts.stderr
        ):
            raise SystemExit("validator accepted an altered Prisma install-script policy")
        (fixture / "migrations/package.json").write_text(
            migration_package,
            encoding="utf-8",
        )

        source_date_epoch = "SOURCE_DATE_EPOCH=${{ needs.release-gate.outputs.source_date_epoch }}"
        cases = (
            (
                "patch-level action selector",
                workflow.replace("actions/checkout@v7", "actions/checkout@v7.0.1", 1),
                "not pinned to one major version",
            ),
            (
                "commit action selector",
                workflow.replace(
                    "actions/checkout@v7",
                    "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
                    1,
                ),
                "not pinned to one major version",
            ),
            (
                "floating action selector",
                workflow.replace("actions/checkout@v7", "actions/checkout@main", 1),
                "not pinned to one major version",
            ),
            (
                "environment-only source date epoch",
                workflow.replace(source_date_epoch, "SOURCE_DATE_EPOCH:", 1),
                "SOURCE_DATE_EPOCH as a Docker build argument",
            ),
            (
                "repository administration API",
                workflow + "\n# /immutable-releases\n",
                "repository-administration API",
            ),
            (
                "unguarded chart reuse",
                workflow.replace("cmp --silent", "cmp", 1),
                "missing fail-closed controls",
            ),
            (
                "release candidates published as stable",
                workflow.replace("            kp_release_args+=(--prerelease)\n", "", 1),
                "missing fail-closed controls",
            ),
            (
                "readable image tag can overwrite different content",
                workflow.replace(
                    "                echo \"Readable image tag already points to different content: ${kp_tagged_image}.\" >&2\n",
                    "                echo \"Readable image tag exists.\" >&2\n",
                    1,
                ),
                "readable image publication lacks fail-closed controls",
            ),
            (
                "emulated arm build",
                workflow.replace("runner: ubuntu-26.04-arm\n", "runner: ubuntu-26.04\n", 1),
                "six amd64 and six arm64",
            ),
            (
                "mutable child image tag",
                workflow.replace("push-by-digest=true,", "", 1),
                "digest or platform verification",
            ),
            (
                "incomplete image index",
                workflow.replace("(.manifests | length == 2)", "(.manifests | length == 1)", 1),
                "image index assembly lacks fail-closed controls",
            ),
            (
                "child digest consumed by release",
                workflow.replace(
                    "API_DIGEST: ${{ needs.assemble-images.outputs.api_digest }}",
                    "API_DIGEST: ${{ needs.build-images.outputs.api_digest }}",
                    1,
                ),
                "merged api image index digest",
            ),
            (
                "missing native builder-agent build",
                workflow.replace("component: builder-agent\n", "component: missing-builder-agent\n", 1),
                "build builder-agent exactly once per architecture",
            ),
            (
                "builder child digest consumed by release",
                workflow.replace(
                    "BUILDER_AGENT_DIGEST: ${{ needs.assemble-images.outputs.builder_agent_digest }}",
                    "BUILDER_AGENT_DIGEST: ${{ needs.build-images.outputs.builder_agent_digest }}",
                    1,
                ),
                "merged builder-agent image index digest",
            ),
            (
                "hyphenated output not normalized",
                workflow.replace(
                    'kp_output_name="${kp_component//-/_}_digest"',
                    'kp_output_name="${kp_component}_digest"',
                    1,
                ),
                "normalize hyphenated component names",
            ),
            (
                "builder chart omitted from package",
                workflow.replace("            --builder-chart charts/kuberploy-builder\n", "", 1),
                "fallible local artifact generation",
            ),
            (
                "publication before local validation",
                workflow.replace(
                    "- name: Package and validate release artifacts",
                    "- name: temporary chart publication marker",
                    1,
                ).replace(
                    "- name: Publish or verify immutable chart set",
                    "- name: Package and validate release artifacts",
                    1,
                ).replace(
                    "- name: temporary chart publication marker",
                    "- name: Publish or verify immutable chart set",
                    1,
                ),
                "out of order",
            ),
        )
        for description, mutated, expected in cases:
            result = run_validator(root, fixture, mutated)
            combined = result.stdout + result.stderr
            if result.returncode == 0:
                raise SystemExit(f"validator accepted mutation: {description}")
            if expected not in combined:
                raise SystemExit(
                    f"validator rejected {description!r} for the wrong reason; "
                    f"expected {expected!r}, got: {combined.strip()}"
                )

    print("release source validator mutation tests passed")


if __name__ == "__main__":
    main()
