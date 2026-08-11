# Kuberploy

[![CI](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml/badge.svg)](https://github.com/kuberploy/kuberploy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kuberploy/kuberploy?include_prereleases)](https://github.com/kuberploy/kuberploy/releases)
[![License](https://img.shields.io/github/license/kuberploy/kuberploy)](LICENSE)

Kuberploy is an open-source, self-hosted platform for building and deploying
applications on Kubernetes. It combines a straightforward web experience with
a GitOps control plane: Git stores non-secret desired state, Argo CD reconciles
workloads, and PostgreSQL holds durable operations and recovery state.

> **Release status:** `0.1.0-rc.65` is a release candidate. Use a dedicated test
> cluster until the production qualification matrix is complete.
> This RC moves the reviewed `0.1.0` native PostgreSQL baseline into a dedicated
> Prisma migration Job and therefore requires a fresh database; pre-Prisma RC
> histories are rejected.

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

Each project can keep multiple named image pull credentials backed by
operator-managed registry profiles. A service selects public/no credential or
one project credential; that runtime choice is independent from GitHub builds,
webhooks, build cache/push credentials, and auto-deploy. Service configuration
also supports explicit Kubernetes `RollingUpdate` and `Recreate` strategies.

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
- `kubectl`
- a default StorageClass

Create an operator-owned values file from the installer contract, then install
the public OCI chart at one explicit version:

```bash
cp examples/installer/managed-platform-values.yaml installer-values.yaml
# Edit the exact API/repository CIDRs, component choices and public endpoint.
```

```bash
helm upgrade --install kuberploy-installer \
  oci://ghcr.io/kuberploy/charts/kuberploy-installer \
  --version 0.1.0-rc.65 \
  --namespace kuberploy-system --create-namespace \
  --kubeconfig /absolute/path/to/kubeconfig \
  --kube-context exact-context \
  --values installer-values.yaml \
  --server-side=true --force-conflicts \
  --wait
```

The installer owns the desired `Application.spec` fields while Argo CD also
updates those objects during reconciliation. Helm 4 upgrades therefore use
server-side apply with `--force-conflicts` to reclaim only the reviewed fields
rendered by the new installer version. This is required for repeat upgrades,
not only the first install.

The values file explicitly selects every managed or adopted component, exact
Kubernetes API and provider egress CIDRs, public endpoint, and pre-existing
Secret references. Traefik, PostgreSQL, Valkey, cert-manager, monitoring and
the control plane can be selected in the same installer release; integrations
that require third-party credentials remain off until their values and Secrets
are supplied by an administrator.

For installer-managed HTTPS, enable `publicEndpoint.tls` and provide the exact
TLS Secret name, production ClusterIssuer name, and a real Let's Encrypt account
email. The installer then configures cert-manager and the control-plane Ingress;
it rejects incomplete or dormant TLS settings before creating resources.

`helm --wait` confirms bootstrap acceptance, not full platform readiness. The
[installer guide](charts/kuberploy-installer/README.md) documents the values
contract, managed/adopted modes, required Secrets, and ownership boundaries.

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
