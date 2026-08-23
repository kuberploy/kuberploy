# Kuberploy Argo CD foundation

This independent chart installs the locked Argo CD 3.5.0 / chart 10.3.0
foundation or records adoption of an installer-verified compatible deployment.
It is restricted to the `argocd` namespace and is never an implicit dependency
of the Kuberploy control-plane release.

Managed mode uses an operator-selected compatible Argo CD image, retained CRDs,
restricted containers, tunable resource bounds, optional default-deny ingress
and egress, ClusterIP-only server access, no local admin, no default direct role,
and no Dex, notifications, commit server, bundled Redis, arbitrary plugins,
credentials, sidecars, or extra resources. Argo CD uses the separately managed
Valkey endpoint with an `argocd` ACL user and database 1. The installer must
deliver these existing Secrets without putting their contents in Git:

- `argocd-secret`, including a strong `server.secretkey`;
- `kuberploy-argocd-valkey-auth`, with `redis-username=argocd` and the exact
  `redis-password` also delivered as `argocd-password` to managed Valkey;
- the labelled repository Secret derived as
  `kuberploy-repo-<platform-binding-uuid-without-hyphens>`.

The optional bootstrap boundary creates one retained root `AppProject` and
`Application`. It is disabled by default and accepts only the verified platform
binding's cluster UUID, binding UUID, GitHub owner/repository, and fully
qualified branch ref. The chart—not a caller—derives the canonical
`https://github.com/<owner>/<repository>.git` remote, the fixed
`platform/argocd` directory, and the deterministic repository
credential annotation. The root source recursively reads that directory and
uses the exact in-cluster destination and automated sync policy checked by the
production prerequisite observer. This is the one exceptional direct write
needed to bootstrap GitOps. Once healthy, the root Application owns child
platform Applications; tenant workloads remain owned by their narrow generated
AppProjects.

When `bootstrap.enabled=false`, every other bootstrap value must remain empty.
Enabling it with partial identities, caller-selected URLs/paths/Secret names,
non-GitHub remotes, or malformed branch refs fails chart validation. Rendering
the root does not itself enable Kuberploy's Argo capability: the runtime must
still independently attest repository protection, materialize and observe the
exact GitHub App credential, and observe this Application as `Synced` and
`Healthy` at the verified platform revision.

An enabled bootstrap profile therefore has this closed shape:

```yaml
argoFoundation:
  bootstrap:
    enabled: true
    bindingID: 71111111-1111-4111-8111-111111111111
    repositoryOwner: kuberploy
    repositoryName: platform-gitops
    targetRevision: refs/heads/main
```

For that identity the rendered source is exactly
`https://github.com/kuberploy/platform-gitops.git`,
`platform/argocd`, and repository Secret
`kuberploy-repo-71111111111141118111111111111111`.

The repository/API CIDRs are optional infrastructure-hardening inputs. Empty
lists use the required dual-stack HTTPS route when NetworkPolicy is enabled;
nonempty lists narrow it. P0 repository transport is HTTPS-only; there is no
ambient SSH or unrestricted-port egress. Direct Argo
UI exposure is disabled because the Kuberploy API/UI remains the normal tenant
surface. A future explicitly configured Traefik route may proxy the authenticated
server without changing this chart's ClusterIP boundary.

Run `./test/e2e/render-argocd-chart.sh` to download and checksum the upstream
dependency, lint both modes, render deterministically, and run mutation tests.
