# Kuberploy Sealed Secrets foundation

This independent release manages the locked Sealed Secrets 0.38.4 / chart
2.19.1 profile or records adoption of an installer-verified compatible
controller. It is restricted to the `sealed-secrets` namespace and is never an
implicit control-plane dependency.

Managed mode pins the controller by multi-platform digest, enables bounded
health probes and resources, uses restricted containers, keeps both services
ClusterIP-only, and can apply a wrapper-owned default-deny NetworkPolicy. The
upstream `system:authenticated` service-proxy binding is explicitly disabled.
Arbitrary objects, namespaces, commands, volumes, metadata injection, ingress,
public services, custom probes, and inline keys are forbidden.

Kuberploy always seals with strict namespace-and-name scope. The API derives
both values from the application environment; callers cannot select them. Only
Kuberploy-owned `SealedSecret` manifests belong in the protected GitOps tree,
and plaintext secret material is never committed.

The controller creates and rotates Kubernetes sealing-key Secrets labelled
`kuberploy.io/sealing-key=true`. Before the secret capability can be enabled,
the installer must prove encrypted, access-controlled backup and restore of all
those key Secrets. Keys are never accepted in chart values, release artifacts,
or Git. Losing every retained private key makes older sealed values
unrecoverable.

Run `./test/e2e/render-secret-controller-charts.sh` to checksum the dependency,
lint both modes, render deterministically, and execute mutation tests.
