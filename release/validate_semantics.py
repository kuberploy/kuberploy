#!/usr/bin/env python3
"""Validate relationships that JSON Schema cannot express."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import tarfile
from datetime import datetime
from pathlib import Path, PurePosixPath

from generate_manifest import migration_identity, sha256


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
        key = match.group(2)
        path = tuple([stack[level] for level in sorted(stack)] + [key])
        raw = (match.group(3) or "").strip()
        if path == wanted:
            if not raw:
                raise ValueError(f"YAML path {'.'.join(wanted)} has no scalar value")
            if raw.startswith('"'):
                return str(json.loads(raw))
            return raw.strip("'")
        if not raw:
            stack[indent] = key
    raise ValueError(f"YAML path not found: {'.'.join(wanted)}")


def chart_documents(package: Path) -> tuple[str, str, str, str]:
    with tarfile.open(package, "r:gz") as archive:
        members = archive.getmembers()
        for member in members:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts:
                raise ValueError(f"unsafe chart archive member: {member.name}")
            if member.issym() or member.islnk():
                raise ValueError(f"chart archive may not contain links: {member.name}")
        root_charts = [
            member
            for member in members
            if len(PurePosixPath(member.name).parts) == 2
            and PurePosixPath(member.name).name == "Chart.yaml"
        ]
        if len(root_charts) != 1:
            raise ValueError("chart archive must contain exactly one root Chart.yaml")
        root = PurePosixPath(root_charts[0].name).parts[0]
        expected = (
            f"{root}/Chart.yaml",
            f"{root}/values.yaml",
            f"{root}/charts/kuberploy-builder/Chart.yaml",
            f"{root}/charts/kuberploy-builder/values.yaml",
        )
        metadata_members = [
            member
            for member in members
            if PurePosixPath(member.name).name in {"Chart.yaml", "values.yaml"}
        ]
        if sorted(member.name for member in metadata_members) != sorted(expected):
            raise ValueError("chart archive must contain only the expected root and builder chart metadata")
        documents = []
        by_name = {member.name: member for member in metadata_members}
        for name in expected:
            member = by_name[name]
            if not member.isfile():
                raise ValueError(f"chart metadata is not a regular file: {name}")
            document = archive.extractfile(member)
            if document is None:
                raise ValueError(f"could not read chart metadata: {name}")
            documents.append(document.read().decode("utf-8"))
        if len(documents) != 4:
            raise ValueError("chart metadata files are not regular files")
        return documents[0], documents[1], documents[2], documents[3]


def component_chart_documents(package: Path, expected_name: str) -> tuple[str, str, dict[str, str]]:
    with tarfile.open(package, "r:gz") as archive:
        members = archive.getmembers()
        by_name = {member.name: member for member in members}
        if len(by_name) != len(members):
            raise ValueError(f"component chart contains duplicate archive paths: {expected_name}")
        for member in members:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != expected_name:
                raise ValueError(f"unsafe component chart archive member: {member.name}")
            if member.issym() or member.islnk():
                raise ValueError(f"component chart archive may not contain links: {member.name}")
            if "testdata" in path.parts:
                raise ValueError(f"component chart contains release test fixtures: {member.name}")
        chart_name = f"{expected_name}/Chart.yaml"
        values_name = f"{expected_name}/values.yaml"
        schema_name = f"{expected_name}/values.schema.json"
        templates_prefix = f"{expected_name}/templates/"
        for required in (chart_name, values_name, schema_name):
            if required not in by_name or not by_name[required].isfile():
                raise ValueError(f"component chart lacks required regular file: {required}")
        if not any(member.isfile() and member.name.startswith(templates_prefix) for member in members):
            raise ValueError(f"component chart lacks templates: {expected_name}")

        def read_text(name: str) -> str:
            stream = archive.extractfile(by_name[name])
            if stream is None:
                raise ValueError(f"could not read component chart file: {name}")
            return stream.read().decode("utf-8")

        dependency_digests: dict[str, str] = {}
        for member in members:
            path = PurePosixPath(member.name)
            if len(path.parts) == 3 and path.parts[:2] == (expected_name, "charts") and member.isfile():
                stream = archive.extractfile(member)
                if stream is None:
                    raise ValueError(f"could not read component dependency: {member.name}")
                dependency_digests[path.name] = hashlib.sha256(stream.read()).hexdigest()
        return read_text(chart_name), read_text(values_name), dependency_digests


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--asset-dir", required=True, type=Path)
    parser.add_argument("--expected-tag")
    parser.add_argument("--expected-commit")
    args = parser.parse_args()

    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    tag = manifest["release"]["tag"]
    version = manifest["release"]["version"]
    commit = manifest["source"]["commit"]
    require(tag == f"v{version}", "release tag/version mismatch")
    if args.expected_tag:
        require(tag == args.expected_tag, "manifest tag does not match the requested tag")
    if args.expected_commit:
        require(commit == args.expected_commit, "manifest commit does not match the checked-out commit")
    require(
        manifest["$schema"]
        == f"https://raw.githubusercontent.com/{manifest['source']['repository']}/{commit}/release/release-manifest.schema.json",
        "manifest schema URI is not pinned to its source commit",
    )
    require(
        manifest["release"]["notesUrl"]
        == f"https://github.com/{manifest['source']['repository']}/releases/tag/{tag}",
        "release notes URL does not match repository and tag",
    )
    require(1 <= len(manifest["release"]["summary"]) <= 500, "release summary length is invalid")
    require(isinstance(manifest["release"]["breakingChanges"], bool), "breakingChanges must be boolean")
    release_metadata = json.loads((args.root / "release/metadata.json").read_text(encoding="utf-8"))
    require(manifest["release"]["summary"] == release_metadata["summary"], "release summary differs from source metadata")
    require(
        manifest["release"]["breakingChanges"] == release_metadata["breakingChanges"],
        "breaking-change flag differs from source metadata",
    )
    created = datetime.fromisoformat(manifest["release"]["createdAt"].replace("Z", "+00:00"))
    require(created.tzinfo is not None, "createdAt must be timezone-aware")
    require(all(value == version for value in manifest["versions"].values()), "component versions are not identical")
    range_match = re.fullmatch(
            r">=(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*) "
            r"<(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)",
            manifest["compatibility"]["supportedUpgradeFrom"],
        )
    require(range_match is not None, "supported upgrade range uses an unsupported syntax")
    require(
        manifest["compatibility"]["supportedUpgradeFrom"] == release_metadata["supportedUpgradeFrom"],
        "supported upgrade range differs from source metadata",
    )

    source_chart = (args.root / "charts/kuberploy/Chart.yaml").read_text(encoding="utf-8")
    require(yaml_scalar(source_chart, ("version",)) == version, "source chart version does not match release")
    require(yaml_scalar(source_chart, ("appVersion",)) == version, "source chart appVersion does not match release")
    kube_constraint = yaml_scalar(source_chart, ("kubeVersion",))
    require(kube_constraint == manifest["compatibility"]["kubernetes"]["constraint"], "Kubernetes constraint mismatch")
    web_version = json.loads((args.root / "web/package.json").read_text(encoding="utf-8"))["version"]
    require(web_version == version, "web package version does not match release")

    current, minimum, migrations_digest = migration_identity(args.root / "migrations")
    database = manifest["compatibility"]["database"]
    require(database["currentSchema"] == current, "current database schema mismatch")
    require(database["minimumUpgradeableSchema"] == minimum, "minimum database schema mismatch")
    require(database["migrationSetSha256"] == migrations_digest, "migration set digest mismatch")
    require(manifest["dependencyLock"]["sha256"] == sha256(args.root / "DEPENDENCIES.md"), "dependency lock digest mismatch")

    components = ("api", "worker", "web", "upgrader", "builder-agent")
    expected_references = {component: f"ghcr.io/kuberploy/kuberploy-{component}" for component in components}
    images = manifest["artifacts"]["images"]
    require([image["component"] for image in images] == list(components), "image order/components mismatch")
    for image in images:
        require(image["reference"] == expected_references[image["component"]], f"unexpected {image['component']} image reference")
        require(image["platforms"] == ["linux/amd64", "linux/arm64"], "image platforms mismatch")

    chart = manifest["artifacts"]["chart"]
    package = args.asset_dir / chart["package"]
    require(package.is_file(), f"chart package is missing: {package}")
    require(chart["packageSha256"] == sha256(package), "chart package digest mismatch")
    require(chart["version"] == version, "chart artifact version mismatch")
    require(
        chart["ociReference"] == f"ghcr.io/kuberploy/charts/kuberploy:{version}",
        "chart OCI reference mismatch",
    )
    packaged_chart, packaged_values, builder_chart, builder_values = chart_documents(package)
    require(yaml_scalar(packaged_chart, ("version",)) == version, "packaged chart version mismatch")
    require(yaml_scalar(packaged_chart, ("appVersion",)) == version, "packaged appVersion mismatch")
    dependency = (
        "dependencies:\n"
        "  - name: kuberploy-builder\n"
        "    alias: builder\n"
        f"    version: {version}\n"
        '    repository: "file://charts/kuberploy-builder"\n'
        "    condition: builder.enabled\n"
    )
    require(packaged_chart.endswith(dependency), "packaged chart builder dependency mismatch")
    require(yaml_scalar(packaged_values, ("global", "requireImageDigest")) == "true", "release chart does not require digests")
    require(yaml_scalar(packaged_values, ("builder", "enabled")) == "false", "release chart enables privileged builder boundary")
    require(yaml_scalar(builder_chart, ("name",)) == "kuberploy-builder", "embedded builder chart name mismatch")
    require(yaml_scalar(builder_chart, ("version",)) == version, "embedded builder chart version mismatch")
    require(yaml_scalar(builder_chart, ("appVersion",)) == version, "embedded builder appVersion mismatch")
    require(yaml_scalar(builder_values, ("enabled",)) == "false", "embedded builder chart is enabled by default")
    for image in images:
        if image["component"] == "upgrader":
            packaged_reference = yaml_scalar(packaged_values, ("upgrade", "image", "reference"))
        elif image["component"] == "builder-agent":
            packaged_reference = yaml_scalar(packaged_values, ("builder", "builderAgentImage"))
            embedded_reference = yaml_scalar(builder_values, ("builderAgentImage",))
            require(
                embedded_reference == f"{image['reference']}@{image['digest']}",
                "embedded builder-agent image mismatch",
            )
        else:
            packaged_reference = yaml_scalar(packaged_values, ("components", image["component"], "image", "reference"))
        require(packaged_reference == f"{image['reference']}@{image['digest']}", f"packaged {image['component']} image mismatch")

    component_names = (
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
    component_charts = manifest["artifacts"]["componentCharts"]
    require([item["name"] for item in component_charts] == list(component_names), "component chart order/names mismatch")
    builder_image = next(image for image in images if image["component"] == "builder-agent")
    for item in component_charts:
        name = item["name"]
        component_package = args.asset_dir / item["package"]
        require(component_package.is_file(), f"component chart package is missing: {component_package}")
        require(item["package"] == f"{name}-{version}.tgz", f"component chart package name mismatch: {name}")
        require(item["packageSha256"] == sha256(component_package), f"component chart package digest mismatch: {name}")
        require(item["version"] == version, f"component chart version mismatch: {name}")
        require(
            item["ociReference"] == f"ghcr.io/kuberploy/charts/{name}:{version}",
            f"component chart OCI reference mismatch: {name}",
        )
        component_metadata, component_values, dependency_digests = component_chart_documents(component_package, name)
        require(yaml_scalar(component_metadata, ("name",)) == name, f"packaged component chart name mismatch: {name}")
        require(yaml_scalar(component_metadata, ("version",)) == version, f"packaged component chart version mismatch: {name}")
        require(
            yaml_scalar(component_metadata, ("kubeVersion",)) == kube_constraint,
            f"component chart Kubernetes constraint mismatch: {name}",
        )
        source = args.root / "charts" / name
        lock = source / "testdata" / "upstream-artifacts.lock"
        if name == "kuberploy-installer":
            dependency_name = f"kuberploy-argocd-{version}.tgz"
            argo_package = args.asset_dir / dependency_name
            require(argo_package.is_file(), "standalone Argo CD package is missing for installer verification")
            expected_dependency_digest = sha256(argo_package).removeprefix("sha256:")
            require(
                dependency_digests == {dependency_name: expected_dependency_digest},
                "installer nested Argo CD package differs from the standalone release artifact",
            )
            annotation = re.search(
                r'(?m)^  kuberploy\.io/argocd-wrapper-sha256:\s*"?([a-f0-9]{64})"?\s*$',
                component_metadata,
            )
            require(
                annotation is not None and annotation.group(1) == expected_dependency_digest,
                "installer nested Argo CD digest annotation mismatch",
            )
            require(
                re.search(rf'(?m)^    version:\s*"?{re.escape(version)}"?\s*$', component_metadata) is not None,
                "installer nested Argo CD dependency version mismatch",
            )
        elif lock.exists():
            entries = [line.split() for line in lock.read_text(encoding="utf-8").splitlines() if line.strip() and not line.lstrip().startswith("#")]
            require(len(entries) == 1 and len(entries[0]) == 3, f"component chart upstream lock is invalid: {name}")
            checksum, filename, _ = entries[0]
            require(dependency_digests == {filename: checksum}, f"component chart dependency bytes mismatch: {name}")
        else:
            require(not dependency_digests, f"component chart unexpectedly vendors dependencies: {name}")
        if name == "kuberploy-builder":
            require(yaml_scalar(component_values, ("enabled",)) == "false", "standalone builder chart is enabled by default")
            require(
                yaml_scalar(component_values, ("builderAgentImage",))
                == f"{builder_image['reference']}@{builder_image['digest']}",
                "standalone builder-agent image mismatch",
            )

    require(":latest" not in json.dumps(manifest).lower(), "manifest contains a floating latest reference")


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, OSError, json.JSONDecodeError, tarfile.TarError) as error:
        raise SystemExit(f"release manifest semantic validation failed: {error}") from error
