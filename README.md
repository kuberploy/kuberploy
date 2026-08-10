# Kuberploy

[![CI](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml/badge.svg)](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kuberploy/kuberploy?include_prereleases)](https://github.com/kuberploy/kuberploy/releases)
[![License](https://img.shields.io/github/license/kuberploy/kuberploy)](LICENSE)

Kuberploy is an open-source, self-hosted platform for building and deploying
applications on Kubernetes. It combines a straightforward web experience with
a GitOps control plane: Git stores non-secret desired state, Argo CD reconciles
workloads, and PostgreSQL holds durable operations and recovery state.

> **Release status:** `0.1.0-rc.6` is a release candidate. Use a dedicated test
> cluster until the production qualification matrix is complete.

## Highlights

- Deploy an existing OCI image, build from GitHub, or publish an approved Helm
  application.
- Direct Git publication for development and protected pull-request publication
  for reviewed environments.
- Immutable image resolution, rollback, scheduling profiles, reusable Traefik
  middleware, VariableSet inheritance, runtime secrets, TLS, and DNS workflows.
- Team projects, scoped grants, service accounts, GitHub App installations, and
  copyable one-time invitation links—no email provider required.
- Bounded logs, events, metrics, audit history, rendered configuration previews,
  and release health checks.
- A single installer chart that bootstraps the required foundations and creates
  independently reconciled Argo CD Applications.

## How it works

1. You configure an application in the web UI or API. PostgreSQL keeps users,
   permissions, operations, and recovery state; Valkey only accelerates queued
   work and selected status reads.
2. Kuberploy can deploy an existing image, publish a Helm release, or build a
   GitHub repository and push the immutable image to your registry.
3. Kuberploy writes the application’s non-secret desired state to the GitOps
   repository—directly for development environments or through a pull request
   for protected environments.
4. Argo CD reads that Git repository and reconciles the application into
   Kubernetes. Optional Traefik, cert-manager, ExternalDNS, secrets, and
   monitoring integrations are used only when an administrator enables them.

The builder never deploys workloads and never receives runtime environment
values. Docker build arguments belong to the immutable build definition;
runtime environment values belong to the GitOps application configuration.

Secrets are referenced by identity and are never committed to Git. The DinD
builder is an explicit privileged boundary on selected builder nodes and never
mounts the host Docker socket. Optional integrations remain unavailable until
their exact runtime readiness is observed.

Read the full [architecture](ARCHITECTURE.md) and
[security policy](SECURITY.md) before operating a production deployment.

## Install

Requirements:

- Kubernetes `1.34`–`1.36`
- Helm `4.2.3`
- `kubectl`, `curl`, and `jq`
- a default StorageClass

Download the installer from the explicit release tag, then name the exact
kubeconfig and context it may change:

```bash
curl -fsSLo kuberploy-install.sh \
  https://raw.githubusercontent.com/kuberploy/kuberploy/v0.1.0-rc.6/scripts/install.sh
chmod +x kuberploy-install.sh

./kuberploy-install.sh \
  --version 0.1.0-rc.6 \
  --kubeconfig /absolute/path/to/kubeconfig \
  --context exact-context
```

The script installs the public OCI chart and the minimal managed control plane:
Argo CD, PostgreSQL, Valkey, API, worker, and web. It derives only the exact
Kubernetes API and GitHub repository CIDRs needed by the default NetworkPolicy.
DNS, ACME, GitHub App, registry, builder, and monitoring integrations remain
off until an administrator configures them.

`helm --wait` confirms bootstrap acceptance, not full platform readiness. The
script prints the Argo health, one-time bootstrap-token, and local UI commands.
See the [installer guide](charts/kuberploy-installer/README.md) for manual OCI
installation, managed/adopted modes, required Secrets, and ownership boundaries.

## Documentation

- [Development guide](DEVELOPMENT.md)
- [Architecture](ARCHITECTURE.md)
- [Installation details](charts/kuberploy-installer/README.md)
- [Kubernetes qualification](LOCAL_TESTING.md)
- [Dependency policy](DEPENDENCIES.md)
- [Contributing](CONTRIBUTING.md)
- [Security](SECURITY.md)
- [Support](SUPPORT.md)

## Contributing

Issues and pull requests are welcome. Start with
[DEVELOPMENT.md](DEVELOPMENT.md), run `make check`, and keep public examples
free of credentials, private infrastructure, and workstation-specific data.

## License

Kuberploy is licensed under the [Apache License 2.0](LICENSE).
