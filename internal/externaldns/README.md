# Operational ExternalDNS integrations

Platform administrators create safe provider profiles through the API. A
managed profile stores only exact Kubernetes object names for credentials,
provider configuration, and egress policy. Credential values, provider JSON,
arbitrary arguments, endpoints, and Kubernetes YAML are never accepted.

When `KUBERPLOY_EXTERNAL_DNS_OPERATIONAL_ENABLED=true`, the worker derives the
current revision directly from PostgreSQL, renders a closed digest-pinned
ServiceAccount/ConfigMap/Deployment/RBAC bundle, and publishes it under
`platform/argocd/platform/external-dns/<integration-id>.yaml` through
the protected platform Git CAS writer. The installer-owned recursive Argo root
Application applies that bundle. Publication receipts are durable; a managed
target cannot become API-ready before its current receipt and exact live
Deployment, provider Secret reference, provider ConfigMap reference, named
NetworkPolicy, policy, TXT owner, label filter, and domain filters are fresh.

Deactivation is a soft, audited lifecycle transition. The worker deletes only
the exact deterministic bundle preimage and records a dematerialization
receipt. Adopted profiles are never materialized: they remain observation-only
and must match an operator-approved static edge profile at the same revision.

## Cloudflare subdomain configuration

Cloudflare's ExternalDNS provider applies `--domain-filter` when discovering
zones as well as when selecting record names. If an integration permits only
`apps.example.com` but Cloudflare hosts the parent zone `example.com`, configure
the parent zone ID explicitly. Keep the integration's allowed suffix set to
`apps.example.com`; widening it to the entire parent zone is unnecessary.

Create the provider ConfigMap in the configured managed ExternalDNS namespace
and select its name in **Provider config reference**:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cloudflare-provider
  namespace: kuberploy-system
data:
  EXTERNAL_DNS_ZONE_ID_FILTER: "replace-with-cloudflare-zone-id"
```

The zone ID is provider configuration, not an API credential. The separate
credential Secret supplies the Cloudflare API token. The controller retains
the exact integration label filter, allowed DNS suffixes and TXT ownership.
When replacing provider configuration, create a new ConfigMap and update the
integration reference so the managed controller rolls out with the new values.
Changing a ConfigMap in place does not refresh environment variables in an
already running Pod.
