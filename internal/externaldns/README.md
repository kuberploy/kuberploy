# Operational ExternalDNS integrations

Platform administrators create safe provider profiles through the API. A
managed profile stores only exact Kubernetes object names for credentials,
provider configuration, and egress policy. Credential values, provider JSON,
arbitrary arguments, endpoints, and Kubernetes YAML are never accepted.

When `KUBERPLOY_EXTERNAL_DNS_OPERATIONAL_ENABLED=true`, the worker derives the
current revision directly from PostgreSQL, renders a closed digest-pinned
ServiceAccount/ConfigMap/Deployment/RBAC bundle, and publishes it under
`clusters/<cluster>/argocd/platform/external-dns/<integration-id>.yaml` through
the protected platform Git CAS writer. The installer-owned recursive Argo root
Application applies that bundle. Publication receipts are durable; a managed
target cannot become API-ready before its current receipt and exact live
Deployment, provider Secret reference, provider ConfigMap reference, named
NetworkPolicy, policy, TXT owner, label filter, and domain filters are fresh.

Deactivation is a soft, audited lifecycle transition. The worker deletes only
the exact deterministic bundle preimage and records a dematerialization
receipt. Adopted profiles are never materialized: they remain observation-only
and must match an operator-approved static edge profile at the same revision.
