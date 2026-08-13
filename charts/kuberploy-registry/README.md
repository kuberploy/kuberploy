# Kuberploy managed OCI registry chart

This optional chart deploys one persistent OCI Distribution 3.1.1 replica for
Kuberploy's managed-registry mode. The image uses the readable OCI Distribution
3.1.1 release selector in source and is replaced by the immutable
multi-platform identity during release packaging. The registry backend always
remains a ClusterIP Service. Its HTTPS exposure uses the shared Traefik edge and
cert-manager ClusterIssuer so node kubelets and isolated builders need no
NodePort, insecure-registry configuration, or custom node CA.

For ingress or LoadBalancer exposure, the same cert-manager Secret is mounted
read-only into Distribution. The backend therefore serves TLS directly, the
ClusterIP Service exposes TCP 443, and Traefik verifies a normal HTTPS backend.
Cluster DNS may resolve the public registry hostname to this ClusterIP for
in-cluster clients; kubelets continue resolving the public endpoint from the
node network. The control-plane chart grants build Pods only the exact registry
Pod selector and backend port, never broad access to Traefik or node addresses.

The default `enabled: false` renders no Kubernetes resources. An enabled
production render is fail-closed and requires:

```yaml
enabled: true
auth:
  mode: htpasswd
  existingSecret: registry-auth
  realm: kuberploy-registry
  secretRevision: initial
exposure:
  mode: ingress
  endpoint: registry.example.com
  ingressClassName: traefik
  secretName: registry-tls
  clusterIssuerName: kuberploy-letsencrypt-production
networkPolicy:
  enabled: true
  allowedNamespaces:
    - kuberploy
    - kuberploy-build
```

The existing Secret is operator/controller-created and must contain:

- `htpasswd`: bcrypt-formatted htpasswd data;
- `httpSecret`: a high-entropy Distribution HTTP secret shared across restarts.

The chart never generates a user, password, htpasswd file, or Secret. If the
Secret or either key is absent, Kubernetes prevents the registry Pod from
starting. Rotate the Secret out of band and change the non-secret
`auth.secretRevision` value to force a controlled `Recreate` rollout.

`auth.mode: testOnlyUnauthenticated` exists solely for isolated disposable
integration tests. It requires an empty `auth.existingSecret`, retains the
ClusterIP-only Service and NetworkPolicy, annotates the workload with a security
warning, emits a Helm note, and requires internal-only exposure.

Kubelet image pulls originate from the node/container-runtime network, so they
must use the HTTPS Ingress hostname rather than the registry's Kubernetes
Service DNS name. Workload namespaces select an authorized Kubernetes
`imagePullSecret`; the registry NetworkPolicy sees the proxied connection from
Traefik and does not grant broad node-CIDR ingress.

`exposure.mode: loadBalancer` creates a dedicated LoadBalancer Service for the
same shared Traefik pods while keeping the registry backend private. It supports
bounded provider annotations, `loadBalancerClass`, a requested IP, and required
source ranges. This is suitable for a private-network LB and does not install a
second ingress controller. Both public modes use an exact cert-manager
Certificate; `endpoint` may be a hostname or IPv4 address supported by the
selected admin-managed ClusterIssuer.

OCI registry records default to DNS-only when Cloudflare is the DNS provider.
Set `exposure.cloudflareProxied: true` only for bounded test images that fit
the Cloudflare plan's request-size limits. Kuberploy owns the resulting
`external-dns.alpha.kubernetes.io/cloudflare-proxied` annotation; arbitrary
annotation overrides remain rejected. Production registries should normally
use DNS-only or a private LoadBalancer.

The registry configuration uses the Distribution 3 path
`/etc/distribution/config.yml`, stores content under `/var/lib/registry`, and
mounts htpasswd at `/auth/htpasswd`. Manifest deletion by digest is enabled with
`storage.delete.enabled: true`.

## Storage and upgrades

The chart creates a PVC unless `persistence.existingClaim` is set. Size,
StorageClass, access mode, and the default Helm keep policy are configurable.
The single-replica Deployment uses `Recreate`, so two registry processes never
mount and write the filesystem backend during an upgrade.

## Garbage collection boundary

This chart deliberately renders no automatic garbage-collection Job or
CronJob. Distribution garbage collection is stop-the-world: Kuberploy's
lifecycle controller must block new writes, make the registry read-only or
fully stop it, verify that no registry process is using the PVC, run exactly one
offline GC operation, and restore service only after it finishes. Manifest
retention/protection decisions occur before that global reachability pass.

## Verification

`make registry-chart-test` runs the disabled/authenticated/test-only render,
schema, security, storage, and NetworkPolicy assertions. The opt-in
`make registry-cache-smoke` additionally proves image push, BuildKit
`mode=max` registry cache export/import and digest deletion against the local
Docker context selected by `KUBERPLOY_TEST_DOCKER_CONTEXT`; it also requires a
unique `KUBERPLOY_DOCKER_RUN_ID` and cleans only resources owned by that run.
`make registry-kubernetes-smoke` uses the explicit conforming-cluster inputs
documented in `LOCAL_TESTING.md`, creates synthetic bcrypt credentials outside
Helm, verifies unauthenticated rejection plus authenticated `/v2/` access and a
bound PVC, and deletes its exactly owned namespace.
