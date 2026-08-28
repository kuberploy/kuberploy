#!/usr/bin/env python3
"""Check source-version alignment and major-version action pins before release."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from datetime import datetime
from pathlib import Path

from chart_oci_digest import compact_json, parse_chart
from validate_semantics import yaml_scalar

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$")
ACTION = re.compile(r"^\s*-?\s*uses:\s*([^\s#]+)(?:\s+#\s*(\S+))?\s*$")
ALLOWED_ACTION_OWNERS = {"actions", "azure", "docker", "pnpm"}
RELEASE_COMPONENTS = ("api", "worker", "web", "migration", "builder-agent")
RELEASE_COMPONENT_CHARTS = (
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


def validate_builder_agent_runtime(dockerfile: str) -> None:
    if "openssh-client-default=10.3_p1-r0" not in dockerfile:
        raise SystemExit("builder-agent runtime must install the pinned SSH client used by Git SSH sources")
    if "adduser -S -D -H -u 65532 -G kuberploy kuberploy" not in dockerfile:
        raise SystemExit("builder-agent runtime must define the fixed UID used by the SSH client")
    cpu_only_buildx = "FROM docker.io/docker/buildx-bin:0.21.3 AS buildx"
    if cpu_only_buildx not in dockerfile or 'io.kuberploy.builder.buildx="0.21.3"' not in dockerfile:
        raise SystemExit("builder-agent runtime must use the selected CPU-only Buildx release")


def validate_installer_dependency_source(root: Path, version: str) -> None:
    installer = root / "charts/kuberploy-installer"
    metadata, _ = parse_chart(installer / "Chart.yaml")
    requirements = metadata.get("dependencies")
    if not isinstance(requirements, list) or len(requirements) != 2:
        raise SystemExit("installer Chart.yaml must declare exactly two dependencies")

    lock_lines = (installer / "Chart.lock").read_text(encoding="utf-8").splitlines()
    locked: list[dict[str, str]] = []
    index = 0
    if not lock_lines or lock_lines[index] != "dependencies:":
        raise SystemExit("installer Chart.lock has a non-canonical dependency list")
    index += 1
    while index < len(lock_lines) and lock_lines[index].startswith("- name: "):
        if index + 2 >= len(lock_lines):
            raise SystemExit("installer Chart.lock has a truncated dependency")
        name = lock_lines[index].removeprefix("- name: ")
        repository_match = re.fullmatch(r"  repository: (\S+)", lock_lines[index + 1])
        version_match = re.fullmatch(r"  version: (\S+)", lock_lines[index + 2])
        if not name or repository_match is None or version_match is None:
            raise SystemExit("installer Chart.lock has a non-canonical dependency")
        locked.append(
            {
                "name": name,
                "version": version_match.group(1),
                "repository": repository_match.group(1),
            }
        )
        index += 3
    if index + 2 != len(lock_lines):
        raise SystemExit("installer Chart.lock has unexpected or missing fields")
    digest_match = re.fullmatch(r"digest: (sha256:[a-f0-9]{64})", lock_lines[index])
    generated_match = re.fullmatch(r'generated: "[^"\n]+"', lock_lines[index + 1])
    if digest_match is None or generated_match is None:
        raise SystemExit("installer Chart.lock has invalid digest or generation metadata")

    expected_locked = [
        {
            "name": dependency["name"],
            "version": dependency["version"],
            "repository": dependency["repository"],
        }
        for dependency in requirements
    ]
    if locked != expected_locked:
        raise SystemExit("installer Chart.lock dependencies do not match Chart.yaml")
    expected_digest = "sha256:" + hashlib.sha256(
        compact_json([requirements, locked])
    ).hexdigest()
    if digest_match.group(1) != expected_digest:
        raise SystemExit("installer Chart.lock digest does not match Chart.yaml")

    epoch = (installer / "dependencies.source-date-epoch").read_text(encoding="utf-8")
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)\n", epoch) or int(epoch) > 4_102_444_800:
        raise SystemExit("installer dependency source-date epoch must be one supported canonical integer line")

    expected_names = [
        f"charts/kuberploy-argocd-{version}.tgz",
        f"charts/kuberploy-valkey-{version}.tgz",
    ]
    lines = (installer / "dependencies.lock").read_text(encoding="utf-8").splitlines()
    if len(lines) != len(expected_names):
        raise SystemExit("installer dependencies.lock must contain exactly two entries")
    for line, expected_name in zip(lines, expected_names, strict=True):
        match = re.fullmatch(r"([a-f0-9]{64})  (charts/[A-Za-z0-9._+-]+\.tgz)", line)
        if match is None or match.group(2) != expected_name:
            raise SystemExit("installer dependencies.lock has a non-canonical entry")

    preparer = (root / "scripts/helm/prepare-dependencies.sh").read_text(encoding="utf-8")
    required_controls = (
        "scripts/helm/package-installer-dependencies.py",
        "scripts/helm/replace-installer-dependencies.py",
        "dependencies.source-date-epoch",
        "dependencies.lock",
    )
    if any(control not in preparer for control in required_controls):
        raise SystemExit("installer dependency preparation lacks deterministic lock controls")
    if re.search(r"helm dependency (?:build|update)[^\n]*kuberploy-installer", preparer):
        raise SystemExit("installer wrapper dependencies must not use timestamp-bearing Helm packaging")


def validate_stable_qualification(root: Path, version: str) -> None:
    if "-" in version:
        return

    receipt_path = root / "release" / "qualifications" / f"{version}.json"
    if not receipt_path.is_file():
        raise SystemExit(
            f"stable release {version} requires a reviewed final-RC qualification receipt"
        )
    receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
    expected_keys = {
        "candidateVersion",
        "candidateTag",
        "candidateCommit",
        "qualificationReportSHA256",
        "qualificationCompletedAt",
        "status",
        "teardownStatus",
    }
    if set(receipt) != expected_keys:
        raise SystemExit("stable qualification receipt has unexpected or missing fields")
    candidate = receipt["candidateVersion"]
    if not isinstance(candidate, str) or not re.fullmatch(
        rf"{re.escape(version)}-rc\.[1-9][0-9]*", candidate
    ):
        raise SystemExit("stable qualification receipt does not name a final RC of this version")
    if receipt["candidateTag"] != f"v{candidate}":
        raise SystemExit("stable qualification receipt tag does not match its candidate version")
    if not isinstance(receipt["candidateCommit"], str) or not re.fullmatch(
        r"[a-f0-9]{40}", receipt["candidateCommit"]
    ):
        raise SystemExit("stable qualification receipt has an invalid candidate commit")
    if not isinstance(receipt["qualificationReportSHA256"], str) or not re.fullmatch(
        r"sha256:[a-f0-9]{64}", receipt["qualificationReportSHA256"]
    ):
        raise SystemExit("stable qualification receipt has an invalid report checksum")
    if not isinstance(receipt["qualificationCompletedAt"], str) or not re.fullmatch(
        r"20[0-9]{2}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T"
        r"(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]Z",
        receipt["qualificationCompletedAt"],
    ):
        raise SystemExit("stable qualification receipt has an invalid UTC completion time")
    try:
        datetime.strptime(receipt["qualificationCompletedAt"], "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as error:
        raise SystemExit(
            "stable qualification receipt has an invalid UTC completion time"
        ) from error
    if receipt["status"] != "passed" or receipt["teardownStatus"] != "passed":
        raise SystemExit("stable qualification and its teardown must both have passed")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--tag")
    args = parser.parse_args()

    chart = (args.root / "charts/kuberploy/Chart.yaml").read_text(encoding="utf-8")
    version = yaml_scalar(chart, ("version",))
    app_version = yaml_scalar(chart, ("appVersion",))
    if not SEMVER.fullmatch(version) or app_version != version:
        raise SystemExit("Chart version/appVersion must be one identical semantic version")
    builder_chart = (args.root / "charts/kuberploy-builder/Chart.yaml").read_text(encoding="utf-8")
    if yaml_scalar(builder_chart, ("name",)) != "kuberploy-builder":
        raise SystemExit("builder chart name must remain kuberploy-builder")
    if yaml_scalar(builder_chart, ("version",)) != version or yaml_scalar(builder_chart, ("appVersion",)) != version:
        raise SystemExit("builder chart version/appVersion must match the control-plane chart")
    for chart_name in RELEASE_COMPONENT_CHARTS:
        component_chart = (args.root / "charts" / chart_name / "Chart.yaml").read_text(encoding="utf-8")
        if yaml_scalar(component_chart, ("name",)) != chart_name:
            raise SystemExit(f"component chart identity mismatch: {chart_name}")
        if yaml_scalar(component_chart, ("version",)) != version:
            raise SystemExit(f"component chart version must match the control-plane chart: {chart_name}")
        if yaml_scalar(component_chart, ("kubeVersion",)) != yaml_scalar(chart, ("kubeVersion",)):
            raise SystemExit(f"component chart Kubernetes constraint mismatch: {chart_name}")
    web_version = json.loads((args.root / "web/package.json").read_text(encoding="utf-8"))["version"]
    if web_version != version:
        raise SystemExit("web package version must match chart version")
    web_proxy = (args.root / "web/nginx.conf.template").read_text(encoding="utf-8")
    if "location ~ ^/(readyz|v1|" not in web_proxy:
        raise SystemExit("web proxy must route API readiness and v1 paths to the API")
    metadata = json.loads((args.root / "release/metadata.json").read_text(encoding="utf-8"))
    if set(metadata) != {"version", "summary", "breakingChanges", "supportedUpgradeFrom"}:
        raise SystemExit("release metadata has unexpected or missing fields")
    if metadata["version"] != version:
        raise SystemExit("release metadata version must match chart version")
    if not isinstance(metadata["summary"], str) or not 1 <= len(metadata["summary"]) <= 500 or "\n" in metadata["summary"]:
        raise SystemExit("release metadata summary must be a single line of 1 to 500 characters")
    current_rc = re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-rc\.([0-9]+)", version)
    summary_rcs = re.findall(r"\bRC([0-9]+)\b", metadata["summary"], re.IGNORECASE)
    if current_rc and any(reference != current_rc.group(1) for reference in summary_rcs):
        raise SystemExit("release metadata summary references a different release candidate")
    if not isinstance(metadata["breakingChanges"], bool):
        raise SystemExit("release metadata breakingChanges must be boolean")
    semver_part = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?"
    range_match = re.fullmatch(rf">=({semver_part}) <({semver_part})", metadata["supportedUpgradeFrom"])
    if not range_match:
        raise SystemExit("release metadata supportedUpgradeFrom has an unsupported range syntax")
    lower = tuple(int(part) for part in range_match.group(1).split("-", 1)[0].split("."))
    upper = tuple(int(part) for part in range_match.group(2).split("-", 1)[0].split("."))
    if lower >= upper:
        raise SystemExit("release metadata supportedUpgradeFrom has an empty range")
    readme = (args.root / "README.md").read_text(encoding="utf-8")
    upgrade_truth = (
        "The final `0.1.0` migration baseline intentionally requires a fresh database\n"
        "> when replacing any older release-candidate schema history. Existing RC data\n"
        "> must be exported and restored through an operator-reviewed process; it is not\n"
        "> upgraded in place."
    )
    if upgrade_truth not in readme:
        raise SystemExit(
            "README must state the final baseline fresh-database boundary for older release candidates"
        )
    if "--reset-values" not in readme or "--reuse-values" not in readme:
        raise SystemExit(
            "README must document reset-values upgrades and reject reuse-values drift"
        )
    if args.tag and args.tag != f"v{version}":
        raise SystemExit(f"tag {args.tag} does not match source version v{version}")
    validate_stable_qualification(args.root, version)
    validate_installer_dependency_source(args.root, version)

    installer_fixtures = (
        ("managed-values.yaml", "postgresql"),
        ("adopted-values.yaml", "edge"),
    )
    for fixture_name, component in installer_fixtures:
        fixture = (
            args.root / "charts" / "kuberploy-installer" / "testdata" / fixture_name
        ).read_text(encoding="utf-8")
        if yaml_scalar(fixture, ("source", "valuesRevision")) != f"v{version}":
            raise SystemExit(
                f"installer {fixture_name} values tag must match v{version}"
            )
        if (
            yaml_scalar(
                fixture,
                ("components", component, "expectedPackageVersion"),
            )
            != version
        ):
            raise SystemExit(
                f"installer {fixture_name} package version must match {version}"
            )

    migration_root = args.root / "migrations" / "prisma" / "migrations"
    migrations = sorted(
        path
        for path in migration_root.glob("[0-9][0-9][0-9]_*")
        if path.is_dir() and (path / "migration.sql").is_file()
    )
    if not migrations or len({path.name[:3] for path in migrations}) != len(migrations):
        raise SystemExit("database migrations must have unique ordered three-digit prefixes")
    known_migration_names = {value for path in migrations for value in (path.name, path.name[:3])}
    summary_schemas = re.findall(
        r"\bSchema\s+([0-9]{3}(?:_[a-z0-9_]+)?)\b",
        metadata["summary"],
        re.IGNORECASE,
    )
    if any(reference not in known_migration_names for reference in summary_schemas):
        raise SystemExit("release metadata summary references a migration schema that is not shipped")
    migration_package = json.loads((args.root / "migrations/package.json").read_text(encoding="utf-8"))
    if migration_package.get("dependencies") != {
        "postgres": "3.4.7",
        "prisma": "7.9.1",
    }:
        raise SystemExit(
            "migration package must contain only exact Prisma CLI 7.9.1 and postgres.js 3.4.7"
        )
    if migration_package.get("allowScripts") != {
        "@prisma/engines@7.9.1": True,
        "prisma@7.9.1": True,
    }:
        raise SystemExit("migration package must approve only the pinned Prisma 7.9.1 install scripts")
    if "@prisma/client" in json.dumps(migration_package):
        raise SystemExit("migration-only package must not include Prisma Client")
    prisma_schema = (args.root / "migrations/prisma/schema.prisma").read_text(encoding="utf-8")
    if "generator client" in prisma_schema:
        raise SystemExit("migration-only Prisma schema must not generate a client")

    workflow = args.root / ".github/workflows/release.yml"
    workflow_text = workflow.read_text(encoding="utf-8")
    action_count = 0
    for workflow_path in sorted((args.root / ".github/workflows").glob("*.y*ml")):
        for number, line in enumerate(workflow_path.read_text(encoding="utf-8").splitlines(), start=1):
            match = ACTION.match(line)
            if not match:
                continue
            action_count += 1
            reference = match.group(1)
            if reference.startswith("./"):
                continue
            name, separator, action_version = reference.rpartition("@")
            if not separator or not re.fullmatch(r"v[1-9][0-9]*", action_version):
                raise SystemExit(
                    f"workflow action is not pinned to one major version at "
                    f"{workflow_path.name}:{number}: {reference}"
                )
            owner = name.split("/", 1)[0].lower()
            if owner not in ALLOWED_ACTION_OWNERS:
                raise SystemExit(
                    f"workflow uses a non-approved action owner at "
                    f"{workflow_path.name}:{number}: {owner}"
                )
    if action_count < 6:
        raise SystemExit("release workflow unexpectedly contains too few pinned actions")

    ci_workflow_text = (args.root / ".github/workflows/ci.yml").read_text(encoding="utf-8")
    required_ci_controls = (
        "  migration:\n",
        "    name: Prisma migration\n",
        "    runs-on: ubuntu-26.04\n",
        "        run: make prisma-migration-test\n",
    )
    missing_ci_controls = [control.strip() for control in required_ci_controls if control not in ci_workflow_text]
    if missing_ci_controls:
        raise SystemExit(
            "CI does not test the production Prisma migration image: "
            + ", ".join(missing_ci_controls)
        )
    if ci_workflow_text.count("        run: ./scripts/helm/prepare-dependencies.sh\n") != 2:
        raise SystemExit("CI must prepare deterministic Helm dependencies in both consuming jobs")
    checkout_count = ci_workflow_text.count("uses: actions/checkout@v7")
    if checkout_count == 0 or ci_workflow_text.count("persist-credentials: false") != checkout_count:
        raise SystemExit("every CI checkout must disable persisted Git credentials")
    if ci_workflow_text.count("runs-on:") != checkout_count or ci_workflow_text.count("runs-on: ubuntu-26.04") != checkout_count:
        raise SystemExit("every CI job must use the explicit Ubuntu 26.04 runner line")

    dependabot_text = (args.root / ".github/dependabot.yml").read_text(encoding="utf-8")

    def has_dependabot_entry(ecosystem: str, directory: str) -> bool:
        return re.search(
            rf"(?ms)^  - package-ecosystem: {re.escape(ecosystem)}\n"
            rf"(?:(?!^  - package-ecosystem:).)*?^    directory: {re.escape(directory)}$",
            dependabot_text,
        ) is not None

    required_dependabot_entries = (
        ("gomod", "/"),
        ("gomod", "/release/tools"),
        ("npm", "/web"),
        ("npm", "/migrations"),
        ("github-actions", "/"),
        ("docker", "/build/package"),
        ("docker", "/web"),
    )
    missing_dependabot_entries = [
        f"{ecosystem}:{directory}"
        for ecosystem, directory in required_dependabot_entries
        if not has_dependabot_entry(ecosystem, directory)
    ]
    if missing_dependabot_entries:
        raise SystemExit(
            "Dependabot does not cover every shipped dependency surface: "
            + ", ".join(missing_dependabot_entries)
        )

    required_release_controls = (
        "github.ref_protected == true",
        ".immutable == true",
        "kp_source_date_epoch=",
        "Reject an existing GitHub release",
        "Verify qualified immutable release candidate",
        "python3 release/validate_promotion.py",
        "release/qualifications/${kp_version}.json",
        '${kp_candidate_state}" == "${CANDIDATE_TAG},false,true,true',
        "Build and push native image by digest",
        "Assemble and verify image indexes",
        "Package and validate release artifacts",
        "Publish or verify readable image tags",
        "Publish or verify immutable chart set",
        "Repair readable image tags",
        "cmp --silent",
        'if [[ "${VERSION}" == *-* ]]',
        "kp_release_args+=(--prerelease)",
        'if [[ "${kp_expected_prerelease}" == "false" ]]',
        "kp_publish_args+=(--latest)",
        'false,${kp_expected_prerelease},true',
    )
    missing_controls = [control for control in required_release_controls if control not in workflow_text]
    if missing_controls:
        raise SystemExit(f"release workflow is missing fail-closed controls: {', '.join(missing_controls)}")
    if re.search(r"(?m)^    environment:\s*release\s*$", workflow_text):
        raise SystemExit("release workflow must not require protected environment approval")
    if "actions/attest" in workflow_text or "attestations: write" in workflow_text:
        raise SystemExit("release workflow must not publish attestations until a public verifier policy exists")
    if "/immutable-releases" in workflow_text:
        raise SystemExit("release workflow must not require a repository-administration API from GITHUB_TOKEN")
    if "docker/setup-qemu-action" in workflow_text or "setup-qemu" in workflow_text:
        raise SystemExit("release images must build natively without QEMU")

    def workflow_job(name: str) -> str:
        job = re.search(
            rf"(?ms)^  {re.escape(name)}:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
            workflow_text,
        )
        if not job:
            raise SystemExit(f"release workflow is missing the {name!r} job")
        return job.group("body")

    contract_job = workflow_job("release-contract")
    lifecycle_job = workflow_job("chart-lifecycle")
    gate_job = workflow_job("release-gate")
    build_job = workflow_job("build-images")
    assembly_job = workflow_job("assemble-images")
    publish_job = workflow_job("publish")
    fresh_k3s_job = workflow_job("fresh-k3s-install")
    repair_job = workflow_job("repair-image-tags")

    if "runs-on: ubuntu-26.04" not in contract_job:
        raise SystemExit("Go-heavy release contract must run on the full Ubuntu 26.04 VM")
    for name, body in (("chart-lifecycle", lifecycle_job), ("release-gate", gate_job)):
        if "runs-on: ubuntu-slim" not in body or not re.search(r"timeout-minutes:\s*(?:[1-9]|1[0-5])\s*$", body, re.MULTILINE):
            raise SystemExit(f"lightweight {name} job must fit the ubuntu-slim 15-minute limit")
    if "runs-on: ubuntu-slim" in assembly_job or "runs-on: ubuntu-slim" in publish_job:
        raise SystemExit("Docker index assembly and full release publication require full Ubuntu VMs")
    fresh_k3s_controls = (
        "needs: [release-gate, publish]",
        "runs-on: ubuntu-26.04",
        "kp_k3s_tag='v1.36.3+k3s1'",
        "sha256sum --check",
        "kubectl --context default get nodes",
        "Fresh K3s unexpectedly contains kuberploy-system",
        "oci://ghcr.io/kuberploy/charts/kuberploy-installer",
        "--values examples/installer/managed-platform-values.yaml",
        "--timeout 65m",
        ".immutable == true",
        'select(.name == "kuberploy-installer") | .ociDigest',
        'all(.items[]; .status.sync.status == "Synced" and .status.health.status == "Healthy")',
        "/v1/auth/bootstrap",
        'has("username") | not',
        'BootstrapConsumed',
        "/v1/auth/login",
        "job/kuberploy-installer-application-health --all-containers=true",
        "for _ in {1..120}; do",
        "trap kp_cleanup EXIT",
    )
    missing_fresh_k3s = [control for control in fresh_k3s_controls if control not in fresh_k3s_job]
    if missing_fresh_k3s:
        raise SystemExit(
            "fresh K3s release qualification lacks exact install, identity, or cleanup controls: "
            + ", ".join(missing_fresh_k3s)
        )

    if "runs-on: ${{ matrix.runner }}" not in build_job:
        raise SystemExit("native image builds must select the runner from the platform matrix")
    component_count = len(RELEASE_COMPONENTS)
    if build_job.count("runner: ubuntu-26.04\n") != component_count or build_job.count("runner: ubuntu-26.04-arm\n") != component_count:
        raise SystemExit("native image matrix must contain one amd64 and one arm64 GitHub-hosted runner per release component")
    if build_job.count("platform: linux/amd64") != component_count or build_job.count("platform: linux/arm64") != component_count:
        raise SystemExit("native image matrix must build every component for amd64 and arm64")
    for component in RELEASE_COMPONENTS:
        if build_job.count(f"component: {component}\n") != 2:
            raise SystemExit(f"native image matrix must build {component} exactly once per architecture")
    build_controls = (
        "Verify native runner architecture",
        "outputs: type=image,name=${{ env.REGISTRY_IMAGE }},push-by-digest=true,name-canonical=true,push=true",
        '"${REGISTRY_IMAGE}@${IMAGE_DIGEST}"',
        "--format '{{json .Image}}'",
        '.os == "linux" and .architecture == $architecture',
        "Upload platform digest",
        "image-digest-${{ matrix.component }}-${{ matrix.architecture }}",
    )
    missing_build_controls = [control for control in build_controls if control not in build_job]
    if missing_build_controls:
        raise SystemExit(f"native image builds lack digest or platform verification: {', '.join(missing_build_controls)}")
    if re.search(r"(?m)^\s+tags:\s", build_job):
        raise SystemExit("native child images must be pushed without mutable tags")

    assembly_controls = (
        "pattern: image-digest-*",
        "candidate-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}",
        "Candidate image index already exists; refusing to overwrite",
        "docker buildx imagetools create",
        '--format \'{{json .Manifest}}\'',
        '.mediaType == "application/vnd.oci.image.index.v1+json"',
        "(.manifests | length == 2)",
        '.platform.architecture == "amd64" and .digest == $amd64',
        '.platform.architecture == "arm64" and .digest == $arm64',
    )
    missing_assembly_controls = [control for control in assembly_controls if control not in assembly_job]
    if missing_assembly_controls:
        raise SystemExit(f"image index assembly lacks fail-closed controls: {', '.join(missing_assembly_controls)}")
    if workflow_text.count("candidate-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}") != 1:
        raise SystemExit("image indexes must use one loop-scoped per-attempt candidate tag template")
    for component in RELEASE_COMPONENTS:
        output_name = component.replace("-", "_")
        environment_name = component.replace("-", "_").upper()
        output = f"{output_name}_digest: ${{{{ steps.assemble.outputs.{output_name}_digest }}}}"
        consumer = f"{environment_name}_DIGEST: ${{{{ needs.assemble-images.outputs.{output_name}_digest }}}}"
        if output not in assembly_job or publish_job.count(consumer) != 2:
            raise SystemExit(f"release chart and manifest must consume the merged {component} image index digest")
    if 'kp_output_name="${kp_component//-/_}_digest"' not in assembly_job:
        raise SystemExit("image index output names must normalize hyphenated component names")
    repair_controls = (
        "inputs.repair_release_tag != ''",
        ".immutable == true",
        '.artifacts.images | length == (if .schemaVersion == "1.0.0" then 6 else 5 end)',
        'docker buildx imagetools create',
        '"${kp_reference}:${kp_version}"',
        '"${kp_reference}@${kp_digest}"',
    )
    missing_repair = [control for control in repair_controls if control not in repair_job]
    if missing_repair:
        raise SystemExit(f"image tag repair lacks immutable-release controls: {', '.join(missing_repair)}")

    def release_step(name: str) -> tuple[int, str]:
        step = re.search(
            rf"(?ms)^      - name: {re.escape(name)}\n(?P<body>.*?)(?=^      - name: |\Z)",
            workflow_text,
        )
        if not step:
            raise SystemExit(f"release workflow is missing the {name!r} step")
        return step.start(), step.group("body")

    _, native_build_body = release_step("Build and push native image by digest")
    if "          build-args: |" not in native_build_body:
        raise SystemExit("native image build arguments are not inputs to docker/build-push-action")
    if "SOURCE_DATE_EPOCH=${{ needs.release-gate.outputs.source_date_epoch }}" not in native_build_body:
        raise SystemExit("native image builds do not receive SOURCE_DATE_EPOCH as a Docker build argument")
    if not all(name in native_build_body for name in ("VERSION=", "REVISION=", "BUILD_DATE=")):
        raise SystemExit("native image builds do not receive the complete release identity")

    preflight_position, _ = release_step("Reject an existing GitHub release")
    build_position, _ = release_step("Build and push native image by digest")
    assembly_position, _ = release_step("Assemble and verify image indexes")
    local_position, local_body = release_step("Package and validate release artifacts")
    image_tag_position, image_tag_body = release_step("Publish or verify readable image tags")
    publish_position, publish_body = release_step("Publish or verify immutable chart set")
    github_position, _ = release_step("Create draft and publish GitHub Release")
    if not preflight_position < build_position < assembly_position < local_position < image_tag_position < publish_position < github_position:
        raise SystemExit("release gate, native builds, index assembly, local validation, and publication are out of order")
    local_controls = (
        "--builder-chart charts/kuberploy-builder",
        "--migration-image \"ghcr.io/kuberploy/kuberploy-migration@${MIGRATION_DIGEST}\"",
        "--builder-agent-image \"ghcr.io/kuberploy/kuberploy-builder-agent@${BUILDER_AGENT_DIGEST}\"",
        "--source-date-epoch \"${SOURCE_DATE_EPOCH}\"",
        "--created-at \"${CREATED_AT}\"",
        "release/package_chart_archive.py",
        "release/package_component_charts.py",
        "release/chart_oci_digest.py",
        "release/generate_manifest.py",
        "release/validate-manifest.sh",
        "release/create-checksums.sh",
        "sha256sum --check",
    )
    missing_local = [control for control in local_controls if control not in local_body]
    if missing_local or "helm push" in local_body or "helm pull" in local_body:
        raise SystemExit("all fallible local artifact generation and validation must precede OCI chart publication")
    image_tag_controls = (
        '"${kp_image}:${VERSION}"',
        "Readable image tag already points to different content",
        "docker buildx imagetools create --tag",
        '"${kp_image}@${kp_digest}"',
        '"${kp_published_digest}" == "${kp_digest}"',
    )
    missing_image_tags = [control for control in image_tag_controls if control not in image_tag_body]
    if missing_image_tags:
        raise SystemExit(f"readable image publication lacks fail-closed controls: {', '.join(missing_image_tags)}")
    recovery_controls = (
        "helm show chart",
        "helm pull",
        "helm push",
        "cmp --silent",
        ".artifacts.componentCharts[]",
        'kp_expected_reference="ghcr.io/kuberploy/charts/${kp_name}:${VERSION}"',
    )
    missing_recovery = [control for control in recovery_controls if control not in publish_body]
    if missing_recovery:
        raise SystemExit(f"OCI chart publication lacks fail-closed recovery controls: {', '.join(missing_recovery)}")
    if workflow_text.count("helm push") != 1:
        raise SystemExit("the release workflow must have exactly one guarded OCI chart push")

    dockerfiles = [
        args.root / "build/package/api.Dockerfile",
        args.root / "build/package/worker.Dockerfile",
        args.root / "build/package/migration.Dockerfile",
        args.root / "build/package/builder-agent.Dockerfile",
        args.root / "build/package/rfc2136-test-provider.Dockerfile",
        args.root / "web/Dockerfile",
    ]
    readable_image_version = re.compile(
        r"^[^\s@]+:v?\d+(?:\.\d+){0,2}(?:[-.][A-Za-z0-9][A-Za-z0-9.-]*)?$"
    )
    allowed_base_images = {
        "docker.io/alpine/helm:4.2",
        "docker.io/docker/buildx-bin:0.21.3",
        "docker.io/library/alpine:3.24",
        "docker.io/library/docker:29-dind",
        "docker.io/library/golang:1.26-alpine3.24",
        "docker.io/library/nginx:1.31-alpine",
        "docker.io/library/node:26.7.0-alpine",
        "docker.io/library/registry:3",
    }
    for dockerfile_path in dockerfiles:
        dockerfile = dockerfile_path.read_text(encoding="utf-8")
        from_lines = re.findall(r"(?m)^FROM\s+([^\s]+)", dockerfile)
        if not from_lines or any(not readable_image_version.fullmatch(reference) for reference in from_lines):
            raise SystemExit(f"release image base lacks an explicit readable version: {dockerfile_path}")
        if any("@sha256:" in reference for reference in from_lines):
            raise SystemExit(f"release image base uses an opaque digest selector: {dockerfile_path}")
        unsupported_base_images = sorted(set(from_lines) - allowed_base_images)
        if unsupported_base_images:
            raise SystemExit(
                f"release image base is outside the selected update lines: {dockerfile_path}: "
                + ", ".join(unsupported_base_images)
            )
        if ":latest" in dockerfile.lower():
            raise SystemExit(f"release Dockerfile contains a latest reference: {dockerfile_path}")
    validate_builder_agent_runtime(
        (args.root / "build/package/builder-agent.Dockerfile").read_text(encoding="utf-8")
    )
    values = (args.root / "charts/kuberploy/values.yaml").read_text(encoding="utf-8")
    if yaml_scalar(values, ("builder", "enabled")) != "false":
        raise SystemExit("source chart must keep the privileged builder boundary disabled")
    if yaml_scalar(values, ("builder", "builderAgentImage")) != "":
        raise SystemExit("source chart must not carry an unpublished builder-agent reference")
    if re.search(r"(?m)^dependencies:\s*", chart):
        raise SystemExit("builder dependency must be added only to the immutable release chart")

    print(version)


if __name__ == "__main__":
    main()
