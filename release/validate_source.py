#!/usr/bin/env python3
"""Check source-version alignment and major-version action pins before release."""

from __future__ import annotations

import argparse
import json
import re
from pathlib import Path

from validate_semantics import yaml_scalar

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$")
ACTION = re.compile(r"^\s*-?\s*uses:\s*([^\s#]+)(?:\s+#\s*(\S+))?\s*$")
ALLOWED_ACTION_OWNERS = {"actions", "azure", "docker", "pnpm"}
RELEASE_COMPONENTS = ("api", "worker", "web", "upgrader", "builder-agent")
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
    metadata = json.loads((args.root / "release/metadata.json").read_text(encoding="utf-8"))
    if set(metadata) != {"version", "summary", "breakingChanges", "supportedUpgradeFrom"}:
        raise SystemExit("release metadata has unexpected or missing fields")
    if metadata["version"] != version:
        raise SystemExit("release metadata version must match chart version")
    if not isinstance(metadata["summary"], str) or not 1 <= len(metadata["summary"]) <= 500 or "\n" in metadata["summary"]:
        raise SystemExit("release metadata summary must be a single line of 1 to 500 characters")
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
    if args.tag and args.tag != f"v{version}":
        raise SystemExit(f"tag {args.tag} does not match source version v{version}")

    migrations = sorted((args.root / "migrations").glob("[0-9][0-9][0-9]_*.sql"))
    if not migrations or len({path.name[:3] for path in migrations}) != len(migrations):
        raise SystemExit("database migrations must have unique ordered three-digit prefixes")

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

    required_release_controls = (
        "github.ref_protected == true",
        "environment: release",
        ".immutable == true",
        "kp_source_date_epoch=",
        "Reject an existing GitHub release",
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
    if "actions/attest" in workflow_text or "attestations: write" in workflow_text:
        raise SystemExit("release workflow must not publish attestations until the upgrader verifies them")
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
    repair_job = workflow_job("repair-image-tags")

    if "runs-on: ubuntu-26.04" not in contract_job:
        raise SystemExit("Go-heavy release contract must run on the full Ubuntu 26.04 VM")
    for name, body in (("chart-lifecycle", lifecycle_job), ("release-gate", gate_job)):
        if "runs-on: ubuntu-slim" not in body or not re.search(r"timeout-minutes:\s*(?:[1-9]|1[0-5])\s*$", body, re.MULTILINE):
            raise SystemExit(f"lightweight {name} job must fit the ubuntu-slim 15-minute limit")
    if "runs-on: ubuntu-slim" in assembly_job or "runs-on: ubuntu-slim" in publish_job:
        raise SystemExit("Docker index assembly and full release publication require full Ubuntu VMs")

    if "runs-on: ${{ matrix.runner }}" not in build_job:
        raise SystemExit("native image builds must select the runner from the platform matrix")
    if build_job.count("runner: ubuntu-26.04\n") != 5 or build_job.count("runner: ubuntu-26.04-arm\n") != 5:
        raise SystemExit("native image matrix must contain five amd64 and five arm64 GitHub-hosted runners")
    if build_job.count("platform: linux/amd64") != 5 or build_job.count("platform: linux/arm64") != 5:
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
        "environment: release",
        ".immutable == true",
        ".artifacts.images | length == 5",
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
        "--builder-agent-image \"ghcr.io/kuberploy/kuberploy-builder-agent@${BUILDER_AGENT_DIGEST}\"",
        "--source-date-epoch \"${SOURCE_DATE_EPOCH}\"",
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
        args.root / "build/package/upgrader.Dockerfile",
        args.root / "build/package/builder-agent.Dockerfile",
        args.root / "build/package/rfc2136-test-provider.Dockerfile",
        args.root / "web/Dockerfile",
    ]
    readable_image_version = re.compile(
        r"^[^\s@]+:v?\d+\.\d+(?:\.\d+)?(?:[-.][A-Za-z0-9][A-Za-z0-9.-]*)?$"
    )
    for dockerfile_path in dockerfiles:
        dockerfile = dockerfile_path.read_text(encoding="utf-8")
        from_lines = re.findall(r"(?m)^FROM\s+([^\s]+)", dockerfile)
        if not from_lines or any(not readable_image_version.fullmatch(reference) for reference in from_lines):
            raise SystemExit(f"release image base lacks an explicit readable version: {dockerfile_path}")
        if any("@sha256:" in reference for reference in from_lines):
            raise SystemExit(f"release image base uses an opaque digest selector: {dockerfile_path}")
        if ":latest" in dockerfile.lower():
            raise SystemExit(f"release Dockerfile contains a latest reference: {dockerfile_path}")
    worker_dockerfile = (args.root / "build/package/worker.Dockerfile").read_text(encoding="utf-8")
    if "/usr/local/bin/kuberploy-upgrade-runner" not in worker_dockerfile:
        raise SystemExit("worker image does not contain the dedicated upgrade runner")

    values = (args.root / "charts/kuberploy/values.yaml").read_text(encoding="utf-8")
    if yaml_scalar(values, ("upgrade", "runnerExecutable")) != "/usr/local/bin/kuberploy-upgrade-runner":
        raise SystemExit("chart upgrade runner executable is not the release contract path")
    if yaml_scalar(values, ("builder", "enabled")) != "false":
        raise SystemExit("source chart must keep the privileged builder boundary disabled")
    if yaml_scalar(values, ("builder", "builderAgentImage")) != "":
        raise SystemExit("source chart must not carry an unpublished builder-agent reference")
    if re.search(r"(?m)^dependencies:\s*", chart):
        raise SystemExit("builder dependency must be added only to the immutable release chart")

    print(version)


if __name__ == "__main__":
    main()
