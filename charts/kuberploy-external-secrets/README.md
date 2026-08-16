# Kuberploy External Secrets foundation

This independent release manages the locked External Secrets Operator 2.8.0
profile or records adoption of an installer-verified compatible operator. It is
restricted to the `external-secrets` namespace and is never installed as a
control-plane subchart.

Managed mode pins one multi-platform image digest for the controller, webhook,
and certificate controller. It retains CRDs, requires two replicas and a PDB
for every component, uses restricted containers, keeps the webhook fail-closed,
and can apply optional default-deny NetworkPolicies. Only namespaced
`SecretStore` and `ExternalSecret` reconciliation is enabled. Cluster stores,
cluster external secrets, push secrets, generic targets, RBAC aggregation,
service-binding access, service-account token creation, arbitrary objects,
sidecars, environment injection, and inline credentials are forbidden.

Provider access defaults to dual-stack public HTTPS with the configured
Kubernetes API CIDRs excluded. Optional provider CIDRs narrow that
infrastructure rule. For a provider that needs another port, deploy an HTTPS
egress proxy instead of broadening the controller pod. Provider credentials
must be delivered through the provider's workload identity or an existing
Secret; they must never be put in Helm values or Git.

When NetworkPolicy hardening is enabled without explicit Kubernetes API CIDRs,
the outbound API route keeps the basic public port fallback, but the webhook's
API-server ingress is omitted rather than opened to the world. Supply the
cluster's API CIDRs when enabling that hardened webhook path.

Kuberploy owns generated objects and binds every `SecretStore` and
`ExternalSecret` to the exact team/project/environment/application scope. The
chart alone does not authorize tenants to create either resource.

Run `./test/e2e/render-secret-controller-charts.sh` to checksum the dependency,
lint both modes, render deterministically, and execute mutation tests.
