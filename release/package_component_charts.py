#!/usr/bin/env python3
"""Stage release-only copies of the independently published Helm charts."""

from __future__ import annotations

import argparse
import hashlib
import re
import shutil
import ssl
import subprocess
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

from package_chart_archive import package_chart_archive
from validate_semantics import yaml_scalar

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$")
DIGEST_REFERENCE = re.compile(r"^[^\s@]+@sha256:[a-f0-9]{64}$")
SHA256 = re.compile(r"^[a-f0-9]{64}$")
SAFE_FILENAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}\.tgz$")
MAX_UPSTREAM_BYTES = 128 * 1024 * 1024
ALLOWED_UPSTREAM_HOSTS = {"github.com", "traefik.github.io", "charts.jetstack.io"}

COMPONENT_CHARTS = (
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


class HTTPSOnlyRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        if urllib.parse.urlsplit(newurl).scheme != "https":
            raise ValueError("upstream chart redirect must remain HTTPS")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def reject_links(directory: Path) -> None:
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise SystemExit(f"release chart source may not contain links: {path}")


def replace_scalar(path: Path, key: str, value: str) -> None:
    text = path.read_text(encoding="utf-8")
    pattern = re.compile(rf"(?m)^{re.escape(key)}:\s*.*$")
    text, count = pattern.subn(f'{key}: "{value}"', text)
    if count != 1:
        raise SystemExit(f"Chart.yaml must contain exactly one {key}")
    path.write_text(text, encoding="utf-8")


def locked_upstreams(source: Path) -> list[tuple[str, str, str]]:
    lock = source / "testdata" / "upstream-artifacts.lock"
    has_dependencies = "dependencies:" in (source / "Chart.yaml").read_text(encoding="utf-8")
    if not lock.exists():
        if has_dependencies:
            raise SystemExit(f"dependency chart lacks an upstream artifact lock: {source.name}")
        return []
    if not has_dependencies:
        raise SystemExit(f"upstream lock exists for a chart without dependencies: {source.name}")

    entries: list[tuple[str, str, str]] = []
    for line in lock.read_text(encoding="utf-8").splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        fields = line.split()
        if len(fields) != 3:
            raise SystemExit(f"malformed upstream artifact lock: {lock}")
        checksum, filename, url = fields
        parsed = urllib.parse.urlsplit(url)
        if not SHA256.fullmatch(checksum) or not SAFE_FILENAME.fullmatch(filename):
            raise SystemExit(f"malformed upstream artifact identity: {lock}")
        if parsed.scheme != "https" or parsed.hostname not in ALLOWED_UPSTREAM_HOSTS or parsed.username or parsed.password:
            raise SystemExit(f"upstream artifact URL uses an unapproved HTTPS host: {url}")
        entries.append((checksum, filename, url))
    if len(entries) != 1:
        raise SystemExit(f"dependency chart must lock exactly one upstream artifact: {source.name}")
    return entries


def fetch(url: str, destination: Path, expected_sha256: str) -> None:
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    opener = urllib.request.build_opener(HTTPSOnlyRedirect, urllib.request.HTTPSHandler(context=context))
    request = urllib.request.Request(url, headers={"User-Agent": "kuberploy-release/1"})
    size = 0
    parsed = urllib.parse.urlsplit(url)
    parts = parsed.path.strip("/").split("/")
    gh = shutil.which("gh")
    github_release = (
        gh is not None
        and parsed.hostname == "github.com"
        and not parsed.query
        and not parsed.fragment
        and len(parts) == 6
        and parts[2:4] == ["releases", "download"]
        and parts[5] == destination.name
    )
    downloaded = False
    if github_release:
        repository = f"{parts[0]}/{parts[1]}"
        try:
            subprocess.run(
                [gh, "release", "download", parts[4], "--repo", repository, "--pattern", destination.name,
                 "--dir", str(destination.parent), "--clobber"],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                text=True,
            )
            downloaded = True
        except subprocess.CalledProcessError:
            destination.unlink(missing_ok=True)
    if not downloaded:
        try:
            with opener.open(request, timeout=60) as response, destination.open("xb") as output:
                final = urllib.parse.urlsplit(response.geturl())
                if final.scheme != "https":
                    raise ValueError("upstream chart response must remain HTTPS")
                content_length = response.headers.get("Content-Length")
                if content_length is not None and int(content_length) > MAX_UPSTREAM_BYTES:
                    raise ValueError("upstream chart is larger than the release limit")
                while chunk := response.read(1024 * 1024):
                    size += len(chunk)
                    if size > MAX_UPSTREAM_BYTES:
                        raise ValueError("upstream chart is larger than the release limit")
                    output.write(chunk)
        except Exception:
            destination.unlink(missing_ok=True)
            raise
    if not destination.is_file() or destination.stat().st_size > MAX_UPSTREAM_BYTES:
        destination.unlink(missing_ok=True)
        raise ValueError("upstream chart is absent or larger than the release limit")
    digest = hashlib.sha256()
    with destination.open("rb") as artifact:
        while chunk := artifact.read(1024 * 1024):
            digest.update(chunk)
    if digest.hexdigest() != expected_sha256:
        destination.unlink(missing_ok=True)
        raise ValueError(f"upstream chart checksum mismatch for {destination.name}")


def stage_installer_dependencies(destination_root: Path, destination: Path, version: str, source_date_epoch: int) -> None:
    chart = destination / "Chart.yaml"
    chart_text = chart.read_text(encoding="utf-8")
    chart_text, count = re.subn(r'(?m)^(    version:)\s*.*$', rf'\1 {version}', chart_text)
    if count != 2:
        raise SystemExit("installer must declare exactly two nested dependency versions")
    chart.write_text(chart_text, encoding="utf-8")

    # Helm computes the dependency metadata digest. The generated dependency
    # archive is then replaced by our byte-reproducible archive, so no Helm
    # timestamp or tar implementation enters the published artifact identity.
    try:
        subprocess.run(
            ["helm", "dependency", "update", str(destination), "--skip-refresh"],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
    except FileNotFoundError as error:
        raise SystemExit("helm is required to lock the installer dependency") from error
    except subprocess.CalledProcessError as error:
        raise SystemExit(f"could not lock installer dependency: {error.stderr.strip()}") from error

    dependencies: list[tuple[str, str]] = []
    for component in ("kuberploy-argocd", "kuberploy-valkey"):
        dependency_name = f"{component}-{version}.tgz"
        dependency = destination / "charts" / dependency_name
        dependency.unlink(missing_ok=True)
        package_chart_archive(destination_root / component, dependency, source_date_epoch)
        dependencies.append((hashlib.sha256(dependency.read_bytes()).hexdigest(), dependency_name))

    lock = destination / "Chart.lock"
    lock_text = lock.read_text(encoding="utf-8")
    generated = datetime.fromtimestamp(source_date_epoch, tz=timezone.utc).isoformat().replace("+00:00", "Z")
    lock_text, count = re.subn(r'(?m)^generated:\s*.*$', f'generated: "{generated}"', lock_text)
    if count != 1:
        raise SystemExit("installer dependency lock lacks one generated timestamp")
    lock.write_text(lock_text, encoding="utf-8")
    (destination / "dependencies.lock").write_text(
        "".join(f"{checksum}  charts/{name}\n" for checksum, name in dependencies),
        encoding="utf-8",
    )


def stage_chart(root: Path, destination_root: Path, name: str, version: str, builder_image: str, source_date_epoch: int) -> Path:
    source = root / "charts" / name
    if not source.is_dir() or yaml_scalar((source / "Chart.yaml").read_text(encoding="utf-8"), ("name",)) != name:
        raise SystemExit(f"invalid component chart source: {name}")
    reject_links(source)

    destination = destination_root / name
    if destination.exists():
        raise SystemExit(f"component chart destination already exists: {destination}")
    shutil.copytree(source, destination, ignore=shutil.ignore_patterns("testdata", "charts"))
    replace_scalar(destination / "Chart.yaml", "version", version)
    source_app_version = yaml_scalar((source / "Chart.yaml").read_text(encoding="utf-8"), ("appVersion",))
    source_version = yaml_scalar((source / "Chart.yaml").read_text(encoding="utf-8"), ("version",))
    if source_app_version == source_version:
        replace_scalar(destination / "Chart.yaml", "appVersion", version)

    if name == "kuberploy-builder":
        values = destination / "values.yaml"
        text = values.read_text(encoding="utf-8")
        text, count = re.subn(r'(?m)^builderAgentImage:\s*.*$', f'builderAgentImage: "{builder_image}"', text)
        if count != 1 or yaml_scalar(text, ("enabled",)) != "false":
            raise SystemExit("standalone builder release must be disabled and pin one agent image")
        values.write_text(text, encoding="utf-8")

    if name == "kuberploy-installer":
        stage_installer_dependencies(destination_root, destination, version, source_date_epoch)
        return destination

    upstreams = locked_upstreams(source)
    if upstreams:
        dependencies = destination / "charts"
        dependencies.mkdir(mode=0o755)
        dependency_lock = (root / "DEPENDENCIES.md").read_text(encoding="utf-8")
        for checksum, filename, url in upstreams:
            if checksum not in dependency_lock:
                raise SystemExit(f"upstream checksum is absent from DEPENDENCIES.md: {name}")
            fetch(url, dependencies / filename, checksum)
    return destination


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--builder-agent-image", required=True)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    args = parser.parse_args()

    if not SEMVER.fullmatch(args.version):
        raise SystemExit("component chart release version must be semantic version text")
    if not DIGEST_REFERENCE.fullmatch(args.builder_agent_image) or ":latest" in args.builder_agent_image.lower():
        raise SystemExit("builder agent image must be an immutable digest reference")
    if args.source_date_epoch < 0:
        raise SystemExit("source-date-epoch must be non-negative")
    if args.destination.exists():
        raise SystemExit(f"component chart destination already exists: {args.destination}")
    args.destination.mkdir(parents=True)
    # Stage the independently published wrappers before the installer so its
    # nested packages are byte-identical to the standalone release artifacts.
    for name in tuple(item for item in COMPONENT_CHARTS if item != "kuberploy-installer") + ("kuberploy-installer",):
        stage_chart(args.root, args.destination, name, args.version, args.builder_agent_image, args.source_date_epoch)
    print(args.destination)


if __name__ == "__main__":
    main()
