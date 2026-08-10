# Kuberploy cert-manager release

This disabled-by-default wrapper is the independent protected `cert-manager`
release. It either manages the checksum-locked cert-manager v1.21.1 chart or
adopts explicitly confirmed compatible CRDs, and can create separately approved
Let's Encrypt production and staging ClusterIssuers. Both issuers may coexist;
each application route selects one, HTTP-only routes create no certificate, and
custom certificate Secrets remain namespace-local runtime resources.

Each enabled issuer requires its exact Let's Encrypt directory URL, account
email, and account-key Secret name. HTTP-01 is always the final fallback and is
locked to the configured IngressClass. Temporary solver Ingresses carry
`external-dns.alpha.kubernetes.io/ingress-hostname-source: annotation-only` and
no ExternalDNS hostname annotation. This is ExternalDNS's
[documented mechanism](https://kubernetes-sigs.github.io/external-dns/latest/docs/annotations/annotations/#external-dnsalphakubernetesioingress-hostname-source)
for suppressing host extraction from `Ingress.spec.rules` and `spec.tls`; the
unofficial `external-dns.alpha.kubernetes.io/exclude` convention is not used.
cert-manager's supported
[`ingressTemplate`](https://cert-manager.io/docs/configuration/acme/http01/#ingresstemplate)
copies this metadata to each temporary solver Ingress.

Administrators may add up to sixteen closed Cloudflare DNS-01 profiles to either
issuer. Every profile requires a unique name, one or more disjoint exact DNS
zones, and only a pre-existing API-token Secret name/key reference. DNS-01
solvers precede HTTP-01, so matching zones use DNS-01 and all other names retain
the HTTP-01 fallback. The chart accepts no token value, arbitrary solver YAML,
webhook provider, or ambient cloud identity. `testdata/dns01-values.yaml` shows
the exact supported shape.

The managed profile uses exact v1.21.1 image tags, retained CRDs, two controller,
webhook, and cainjector replicas, PDBs, restricted Pod Security, resource
defaults, dedicated service accounts, and wrapper-owned NetworkPolicies. API
server CIDRs are required and cannot be all-address ranges. Only the controller
has public TCP/443 egress for ACME; this chart never receives certificate private
keys in values and never renders custom TLS Secrets.

Adoption does not claim the existing `cert-manager` Namespace. The operator or
the adopted installation must already enforce the restricted Pod Security
labels there before this release writes its non-secret profile.

Use `testdata/managed-values.yaml` as a shape example, replacing its documentation
CIDR and email. Fetch/verify the dependency and run the full render suite with
`./test/e2e/render-edge-chart.sh`. This release is not a dependency of the
Kuberploy control-plane or Traefik releases.

Live adoption/version probes, issuer health observation, route-level Certificate
creation, and Secret materialization belong to the edge controller/runtime
integration still to be implemented.

ClusterIssuers and their account/credential references remain protected
platform resources reconciled by the bootstrap release. Ordinary workload Git
and application namespaces are not an issuer mutation boundary. Dynamic issuer
catalog entries must use a separate protected platform-admin writer rather than
granting tenants access to these ClusterIssuers.
