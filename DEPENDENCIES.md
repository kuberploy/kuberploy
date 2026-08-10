# Kuberploy dependency baseline

Status: verified release baseline for 2026-08-10. Prereleases are excluded. Release tags were checked against the upstream project, npm/Go registries were checked for direct packages, chart metadata and package checksums were checked against the publishers' chart indexes, and selected image tags were checked for multi-architecture support.

This file records the readable versions selected for the release. Package-manager and Helm lock files retain integrity metadata, and CI produces checksums, an SBOM, and provenance for release artifacts; opaque hashes are not used as dependency version names.

## Selection rule

Kuberploy tracks the newest **mutually compatible stable** release of every direct dependency. For toolchains that publish a production LTS line, the newest stable LTS patch is used. The result is pinned to an explicit readable version. No installer, manifest, build Job or Argo Application may use a floating `latest` tag or resolve "newest" at runtime.

An upstream chart or application bundle is treated as one tested dependency unit. Its transitive versions remain those in the upstream lock unless Kuberploy has a specific security reason to override them and reruns the complete compatibility matrix. Making every transitive package independently newest would destroy the compatibility testing supplied by the parent release.

## P0 Kubernetes and platform services

| Component                 |                                                                                 Selected stable version | Delivery decision                                                                                                                                                                                                                                           |
| ------------------------- | ------------------------------------------------------------------------------------------------------: | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Kubernetes                |                                 [1.36.3](https://github.com/kubernetes/kubernetes/releases/tag/v1.36.3) | Newest test target. Kuberploy does not provision the cluster; the intended initial support window is 1.34-1.36. The patch lanes current on the audit date are 1.34.10, 1.35.7 and 1.36.3, as published by the official `stable-1.x.txt` endpoints.          |
| Helm CLI/SDK              |                                               [4.2.3](https://github.com/helm/helm/releases/tag/v4.2.3) | Used by installer and controlled render workers. Argo CD's upstream image embeds its own tested Helm patch.                                                                                                                                                 |
| Argo CD                   |                                        [3.5.0](https://github.com/argoproj/argo-cd/releases/tag/v3.5.0) | Official `argo-cd` chart [10.3.0](https://github.com/argoproj/argo-helm/releases/tag/argo-cd-10.3.0), which packages Argo CD 3.5.0.                                                                                                                         |
| Traefik                   |                                       [3.7.10](https://github.com/traefik/traefik/releases/tag/v3.7.10) | Official chart [41.1.1](https://github.com/traefik/traefik-helm-chart/releases/tag/v41.1.1) with an explicit, tested 3.7.10 image version override; see the mismatch note below.                                                                            |
| cert-manager              |                             [1.21.1](https://github.com/cert-manager/cert-manager/releases/tag/v1.21.1) | Official chart `v1.21.1`; CRDs and controller upgrade together.                                                                                                                                                                                             |
| external-dns              |                          [0.21.0](https://github.com/kubernetes-sigs/external-dns/releases/tag/v0.21.0) | Official chart `1.21.1`; provider and annotation migrations are tested because the application remains pre-1.0.                                                                                                                                             |
| Valkey                    |                                         [9.1.1](https://github.com/valkey-io/valkey/releases/tag/9.1.1) | Official chart [0.11.0](https://github.com/valkey-io/valkey-helm/releases/tag/valkey-0.11.0), `appVersion: 9.1.1`; hardened Kuberploy values replace development-oriented chart defaults.                                                                   |
| PostgreSQL                |                                                   [18.4](https://www.postgresql.org/docs/release/18.4/) | Server/image uses the explicit `18.4-alpine3.24` version. A lightweight managed profile is for small installations; production can adopt a compatible managed service with tested backup/restore.                                                           |
| OCI Distribution registry |                               [3.1.1](https://github.com/distribution/distribution/releases/tag/v3.1.1) | Lightweight bundled managed-registry data plane. Kuberploy owns its hardened manifests, TLS/auth wiring, persistent storage and lifecycle controller instead of depending on an unmaintained third-party chart. External-registry mode does not install it. |
| External Secrets Operator |                       [2.8.0](https://github.com/external-secrets/external-secrets/releases/tag/v2.8.0) | Official chart [2.8.0](https://github.com/external-secrets/external-secrets/releases/tag/helm-chart-2.8.0); default production secret backend.                                                                                                              |
| Sealed Secrets            |                           [0.38.4](https://github.com/bitnami-labs/sealed-secrets/releases/tag/v0.38.4) | Official chart [2.19.1](https://github.com/bitnami-labs/sealed-secrets/releases/tag/helm-v2.19.1); self-contained alternative with strict namespace/name scope.                                                                                             |
| kube-prometheus-stack     | [88.1.5](https://github.com/prometheus-community/helm-charts/releases/tag/kube-prometheus-stack-88.1.5) | Pin the complete parent chart. It contains Prometheus 3.13.2, Alertmanager 0.33.1, Prometheus Operator 0.93.0 and the Grafana chart/app pairing described below.                                                                                            |
| Prometheus                |                                 [3.13.2](https://github.com/prometheus/prometheus/releases/tag/v3.13.2) | Locked transitively by kube-prometheus-stack 88.1.5; do not override independently.                                                                                                                                                                         |
| Alertmanager              |                               [0.33.1](https://github.com/prometheus/alertmanager/releases/tag/v0.33.1) | Locked transitively by kube-prometheus-stack 88.1.5.                                                                                                                                                                                                        |
| Grafana                   |                                       [13.1.2](https://github.com/grafana/grafana/releases/tag/v13.1.2) | Optional admin UI; kube-prometheus-stack 88.1.5 locks Grafana chart 12.10.3 to this app version.                                                                                                                                                            |

Kubernetes integration runs against operator-supplied conforming clusters in
the supported 1.34-1.36 window. The exact kubeconfig and context are explicit
runtime inputs and no development-cluster identity is part of the dependency
lock. An operator-selected Docker/Buildx engine may be used for development
builds and cache checks; it is not an integration target.

P0 uses Kubernetes Pod Security Admission plus native `ValidatingAdmissionPolicy`/CEL. Kyverno is therefore not a required runtime dependency. Its current stable [1.18.2](https://github.com/kyverno/kyverno/releases/tag/v1.18.2) supports Kubernetes only through 1.35, so an optional Kyverno adapter is disabled on Kubernetes 1.36 until a compatible stable release passes Kuberploy's tests.

## External convenience DNS

[`sslip.io`](https://sslip.io/) is an optional third-party DNS convenience
service, not a bundled controller or availability dependency for ordinary
routes. When explicitly selected, Kuberploy derives a hostname only from a
freshly observed public Traefik IPv4 address. Hostname-only load balancers need
an operator-pinned static IPv4 that remains present in live DNS answers; a
dynamic ALB is rejected. The feature is intended for demos, tests and
back-office services. Operators who need ownership, SLA, dynamic targets or
production DNS use their own domain and the external-dns integration.

## P0 builder toolchain

| Component            |                                         Selected stable version | Delivery decision                                                                                                                                                                                                                       |
| -------------------- | --------------------------------------------------------------: | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Docker Engine / DinD | [29.7.1](https://docs.docker.com/engine/release-notes/29/#2971) | Custom Kuberploy builder image starts an isolated DinD daemon in the approved privileged builder namespace; it never mounts the host Docker socket.                                                                                     |
| Docker Buildx        | [0.36.1](https://github.com/docker/buildx/releases/tag/v0.36.1) | Installed as an exact CLI plugin in the builder image.                                                                                                                                                                                  |
| BuildKit             | [0.32.2](https://github.com/moby/buildkit/releases/tag/v0.32.2) | Buildx uses the `docker-container` driver with this exact version instead of silently using Docker Engine's older embedded BuildKit. P0 uses its registry cache importer/exporter; the same version is the P1 rootless engine baseline. |
| Git                  |                                  [2.54.0](https://git-scm.com/) | Alpine package `2.54.0-r0`, installed explicitly in worker and builder-agent images for Git writer operations.                                                                                                                          |

The official Docker 29.7.1 image bundles Buildx 0.36.0 and the engine embeds BuildKit 0.32.0. Kuberploy deliberately supplies Buildx 0.36.1 and launches BuildKit 0.32.2 as the separately pinned builder. Rootless DinD still requires a privileged Kubernetes container in the documented Docker/ARC pattern; rootless UID mapping does not turn that Pod into an unprivileged workload.

The selected builder image is `docker.io/library/docker:29.7.1-dind`; its OCI index includes an `arm64/linux` manifest. The separately selected BuildKit image is `docker.io/moby/buildkit:v0.32.2`.

## Product implementation and API toolchain

| Component                    |                                                                                                                                                                                                                                                                                                              Selected stable version | Role                                                                                                                                                      |
| ---------------------------- | -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------: | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Go                           |                                                                                                                                                                                                                                                                                                [1.26.5](https://go.dev/dl/#go1.26.5) | Control-plane build and runtime toolchain.                                                                                                                |
| `valkey-go`                  |                                                                                                                                                                                                                                                                [1.0.76](https://github.com/valkey-io/valkey-go/releases/tag/v1.0.76) | Valkey Streams, the bounded Operation cache and distributed limiter behind Kuberploy's internal interfaces. The MVP does not claim a Pub/Sub status path. |
| `pgx`                        |                                                                                                                                                                                                                                                                                  [5.10.0](https://github.com/jackc/pgx/tree/v5.10.0) | PostgreSQL protocol/driver and pool.                                                                                                                      |
| `miekg/dns`                  |                                                                                                                                                                                                                                                                          [1.1.72](https://github.com/miekg/dns/releases/tag/v1.1.72) | Qualification-only TSIG/RFC 2136 authoritative server and client; it is not linked into the production API or worker binaries.                            |
| `jsonschema`                 |                                                                                                                                                                                                                                                           [6.0.3](https://github.com/santhosh-tekuri/jsonschema/releases/tag/v6.0.3) | Server-side Draft 2020-12 AppConfig validation.                                                                                                           |
| `yaml` (Go)                  |                                                                                                                                                                                                                                                                         [3.0.5](https://github.com/yaml/go-yaml/releases/tag/v3.0.5) | Bounded server-side YAML document parsing with duplicate-key detection.                                                                                   |
| Node.js                      |                                                                                                                                                                                                                                                                             [26.7.0](https://nodejs.org/en/blog/release/v26.7.0) | Reproducible production frontend build on the explicitly selected Node 26 line.                                                                          |
| pnpm                         |                                                                                                                                                                                                                                                                        [11.20.0](https://github.com/pnpm/pnpm/releases/tag/v11.20.0) | Frontend package manager with a committed frozen lockfile.                                                                                                |
| TypeScript                   |                                                                                                                                                                                                                                                                            [7.0.2](https://www.npmjs.com/package/typescript/v/7.0.2) | Strict UI and generated API-client types.                                                                                                                 |
| React / React DOM            |                                                                                                                                                                                                                                                                     [19.2.8](https://github.com/facebook/react/releases/tag/v19.2.8) | Web UI. Both packages stay on the same exact version.                                                                                                     |
| Vite                         |                                                                                                                                                                                                                                                                          [8.2.1](https://github.com/vitejs/vite/releases/tag/v8.2.1) | Web UI build/dev tooling.                                                                                                                                 |
| TanStack React Query         |                                                                                                                                                                                                                                          [5.101.4](https://github.com/TanStack/query/releases/tag/%40tanstack/react-query%405.101.4) | Server-state requests and bounded polling.                                                                                                                |
| TanStack React Router        |                                                                                                                                                                                                                                      [1.170.22](https://github.com/TanStack/router/releases/tag/%40tanstack/react-router%401.170.22) | Typed client routing.                                                                                                                                     |
| React Hook Form              |                                                                                                                                                                                                                                                    [7.84.0](https://github.com/react-hook-form/react-hook-form/releases/tag/v7.84.0) | Guided configuration forms.                                                                                                                               |
| Monaco Editor                |                                                                                                                                                                                                                                                            [0.56.0](https://github.com/microsoft/monaco-editor/releases/tag/v0.56.0) | Advanced YAML editor.                                                                                                                                     |
| Tailwind CSS                 |                                                                                                                                                                                                                                                             [4.3.3](https://github.com/tailwindlabs/tailwindcss/releases/tag/v4.3.3) | UI styling; `@tailwindcss/vite` stays on the identical version.                                                                                           |
| shadcn CLI                   |                                                                                                                                                                                                                                                               [4.16.1](https://github.com/shadcn-ui/ui/releases/tag/shadcn%404.16.1) | Generation-only tool. Generated components are reviewed source, not an ambient runtime dependency.                                                        |
| YAML                         |                                                                                                                                                                                                                                                                                  [2.9.0](https://www.npmjs.com/package/yaml/v/2.9.0) | Browser-side advanced YAML parsing and serialization.                                                                                                     |
| Vitest / jsdom               |                                                                                                                                                                                                                     [4.1.10](https://www.npmjs.com/package/vitest/v/4.1.10) / [30.0.1](https://www.npmjs.com/package/jsdom/v/30.0.1) | UI unit and DOM test runtime.                                                                                                                             |
| Testing Library              | DOM [10.4.1](https://www.npmjs.com/package/@testing-library/dom/v/10.4.1), React [16.3.2](https://www.npmjs.com/package/@testing-library/react/v/16.3.2), jest-dom [7.0.0](https://www.npmjs.com/package/@testing-library/jest-dom/v/7.0.0), user-event [14.6.3](https://www.npmjs.com/package/@testing-library/user-event/v/14.6.3) | Direct UI test packages.                                                                                                                                  |
| Vite React plugin / Prettier |                                                                                                                                                                                                        [6.0.5](https://www.npmjs.com/package/@vitejs/plugin-react/v/6.0.5) / [3.9.6](https://www.npmjs.com/package/prettier/v/3.9.6) | Frontend compilation and deterministic formatting.                                                                                                        |
| OpenAPI Specification        |                                                                                                                                                                                                                                                                                   [3.2.0](https://spec.openapis.org/oas/v3.2.0.html) | Public API description, limited to the feature subset proven by contract tests across supported clients.                                                  |
| Swagger UI                   |                                                                                                                                                                                                                                                           [5.32.12](https://github.com/swagger-api/swagger-ui/releases/tag/v5.32.12) | Self-hosted human API documentation; this line supports OpenAPI 3.2.0.                                                                                    |
| Arazzo Specification         |                                                                                                                                                                                                                                                                                [1.1.0](https://spec.openapis.org/arazzo/v1.1.0.html) | Machine-readable multi-operation workflows.                                                                                                               |

Every additional Go module, npm package, code generator and test tool selected during implementation must follow the same rule and be present in `go.sum`, `pnpm-lock.yaml` or the release tool lock. This table does not authorize adding a package merely because it is current.

`@types/node` is explicitly locked to 26.2.0 for the selected Node 26 runtime. `@types/react` 19.2.18 and `@types/react-dom` 19.2.4 are the current stable matching React 19 types. The current `web/package.json` direct runtime and development dependencies otherwise match the stable registry tags recorded above.

## Verified chart artifacts

The following SHA-256 values are the chart package checksums published by the official repositories and independently reproduced from the downloaded packages. Chart `appVersion` is recorded separately from any intentional Kuberploy image override.

| Chart                        |                App version | Package SHA-256                                                    |
| ---------------------------- | -------------------------: | ------------------------------------------------------------------ |
| argo-cd 10.3.0               |                      3.5.0 | `d08882d22d0c76e3174e005cc09abe300c70ba556aec76725a4410d172b9c1f3` |
| traefik 41.1.1               |                      3.7.9 | `42cf5c2a30a3630adb7cefa1ec5b84dfef0105599cd217c7574bd77c6ad369ee` |
| cert-manager v1.21.1         |                    v1.21.1 | `c27101f3f3e2349fb4a9e704316105bf7b52ad73b8c8257d3498ef7f2f6a4adc` |
| external-dns 1.21.1          |                     0.21.0 | `5dd033a4b872bf641860695705ee460031d0bc695f114bf8926fee6736814e19` |
| valkey 0.11.0                |                      9.1.1 | `6aa9f2e423642cae84ed6a9798cdfd0faf2e347290ce7b3e4c393333a79743f8` |
| external-secrets 2.8.0       |                     v2.8.0 | `251e4615013c6d2f9ade5cedf1cd8615613f286bfc381e44fb005f197e611ecd` |
| sealed-secrets 2.19.1        |                     0.38.4 | `f2a3aa61638f1e23ef963be311e8d5ea75878d8e01ae3846285965b6dd985337` |
| kube-prometheus-stack 88.1.5 | Prometheus Operator 0.93.0 | `b558a852552f809ccce66d5677ca1a55c8010470c44a01dbdc4ab3f678bcdc90` |
| loki 18.7.3                  |                      3.7.5 | `e592bd9eb4a51d8a34c2f4b9f2d68601da9753968f10619970da2e181db8cccb` |

The chart-declared Kubernetes constraints are compatible with Kuberploy's 1.34-1.36 window: Argo CD, Traefik, kube-prometheus-stack and Loki require at least 1.25; cert-manager requires at least 1.22; External Secrets requires at least 1.19; and Sealed Secrets requires at least 1.16. external-dns and the Valkey chart do not publish a `kubeVersion` constraint, so Kuberploy's own three-lane install/upgrade tests remain authoritative for them. Argo CD 3.5 itself officially tests Kubernetes 1.33-1.36.

## Verified service and build images

The image selectors below use readable version tags. Their published indexes
include both `linux/amd64` and `linux/arm64`, matching Kuberploy's release
platform contract.

| Purpose                               | Versioned image                                                                        |
| ------------------------------------- | -------------------------------------------------------------------------------------- |
| Argo CD                               | `quay.io/argoproj/argocd:v3.5.0`                                                       |
| External Secrets Operator             | `ghcr.io/external-secrets/external-secrets:v2.8.0`                                     |
| Sealed Secrets                        | `docker.io/bitnami/sealed-secrets-controller:0.38.4`                                   |
| PostgreSQL                            | `docker.io/library/postgres:18.4-alpine3.24`                                           |
| Valkey                                | `docker.io/valkey/valkey:9.1.1`                                                        |
| Managed OCI registry                  | `docker.io/library/registry:3.1.1`                                                     |
| Alpine runtime                        | `docker.io/library/alpine:3.24.1`                                                      |
| Go builder                            | `docker.io/library/golang:1.26.5-alpine3.24`                                           |
| Node builder                          | `docker.io/library/node:26.7.0-alpine`                                                 |
| nginx stable runtime                  | `docker.io/library/nginx:1.31.3-alpine`                                                |
| API, worker and builder-agent runtime | Alpine `3.24.1`, with CA certificates `20260611-r0` and Git `2.54.0-r0` where required |
| Helm 4.2.3 upgrader runtime           | `docker.io/alpine/helm:4.2.3`                                                          |

Every runtime base uses a readable version tag. Generated Kuberploy application
releases still record content digests as deployment integrity, but dependency
versions are never represented by opaque hashes.

## GitHub Actions

| Action                       | Locked major version |
| ---------------------------- | -------------------: |
| `actions/checkout`           |                    7 |
| `actions/setup-go`           |                    7 |
| `actions/setup-node`         |                    7 |
| `pnpm/action-setup`          |                    6 |
| `Azure/setup-helm`           |                    5 |
| `docker/setup-buildx-action` |                    4 |
| `docker/login-action`        |                    4 |
| `docker/build-push-action`   |                    7 |
| `actions/upload-artifact`    |                    7 |
| `actions/download-artifact`  |                    8 |

Workflow policy accepts only the exact `@vN` major-tag form from approved action owners. Minor, patch, branch, floating, and unversioned selectors are rejected by the release source validator.

## P1 optional observability

| Component |                                      Selected stable version | Delivery decision                                                                                                                                                                        |
| --------- | -----------------------------------------------------------: | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Loki      | [3.7.5](https://github.com/grafana/loki/releases/tag/v3.7.5) | Official community chart [18.7.3](https://github.com/grafana-community/helm-charts/releases/tag/loki-18.7.3); retained logging remains optional and behind the scoped Kuberploy gateway. |

## Known compatibility exceptions

These are why the release rule says "latest compatible stable set" instead of independently installing every highest version:

1. Kyverno 1.18.2 does not support Kubernetes 1.36. Kuberploy keeps Kubernetes 1.36.3 and uses native admission policy in P0.
2. Traefik chart 41.1.1 defaults to Traefik 3.7.9 while the latest security-fixed patch is 3.7.10. Kuberploy pins the parent chart and explicitly tests and locks the 3.7.10 image override.
3. Argo CD 3.5.0 embeds Helm 4.2.1 even though the standalone Helm patch is 4.2.3. Kuberploy accepts the application release's upstream-tested embedded binary and tests the external Helm 4.2.3 renderer for equivalent output.
4. Docker 29.7.1 embeds older Buildx/BuildKit patches. The builder image and Buildx driver select the current stable versions explicitly.
5. Node.js 26.7.0 is the explicit production and development runtime selected for this release.
6. Alpine 3.24 packages Git 2.54.0-r0. Kuberploy uses that reviewed package
   instead of an unversioned runtime image; upgrading Git requires a new explicit
   Alpine package version and the normal release qualification.

## Update and lock workflow

1. Automated discovery opens one or a small related set of dependency changes with official release notes.
2. CI regenerates package/chart locks, exact OCI digests, checksums, license inventory, SBOM and provenance metadata.
3. CI runs unit/contract tests, chart render tests, fresh install, upgrade and rollback plus Kubernetes 1.34/1.35/1.36 end-to-end profiles.
4. Security, multi-tenancy, DinD isolation, GitOps reconciliation, TLS/DNS and metrics/log access smoke tests must pass.
5. Merge creates a new Kuberploy dependency lock. Running clusters change only through an explicitly selected Kuberploy/platform Git upgrade.
