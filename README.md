# Kuberploy

[![CI](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml/badge.svg)](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kuberploy/kuberploy?include_prereleases)](https://github.com/kuberploy/kuberploy/releases)
[![License](https://img.shields.io/github/license/kuberploy/kuberploy)](LICENSE)

Kuberploy is an open-source, self-hosted platform for building and deploying
applications on Kubernetes. It combines a straightforward web experience with
a GitOps control plane: Git stores non-secret desired state, Argo CD reconciles
workloads, and PostgreSQL holds durable operations and recovery state.

> **Release status:** `0.1.0-rc.1` is a release candidate. Use a dedicated test
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

## Architecture

```text
Browser / API
      │
      ▼
PostgreSQL ──► Valkey work signal ──► Kuberploy worker
                                         │
                                         ▼
Git desired state ──► Argo CD ──► Kubernetes workloads
                                         │
                                         ▼
                              Traefik / cert-manager / DNS
```

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
- storage classes and provider credentials required by the components you enable

Copy the managed or adopted installer example, replace every placeholder, and
keep credentials in pre-created Kubernetes Secrets:

```bash
cp charts/kuberploy-installer/testdata/managed-values.yaml installer-values.yaml

helm upgrade --install kuberploy-installer charts/kuberploy-installer \
  --namespace kuberploy-system \
  --create-namespace \
  --values installer-values.yaml \
  --wait
```

`helm --wait` confirms the bootstrap resources, not full platform readiness.
After installation, require every selected Argo CD Application to be `Synced`
and `Healthy`, then check Kuberploy's **Setup & health** page. See the
[installer guide](charts/kuberploy-installer/README.md) for managed/adopted
modes, required Secrets, and ownership boundaries.

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
