# Kuberploy external-dns integration release

Install this disabled-by-default chart once per DNS integration as a protected
Argo Application/Helm release in `kuberploy-dns`. Each release independently
owns one exact external-dns 0.21.0 Deployment, provider credential reference,
zone/domain filters, reconciliation policy, health boundary, and unique TXT
owner ID. It is never a dependency of Traefik or the Kuberploy control plane.
The protected platform bootstrap owns the shared `kuberploy-dns` Namespace and
its restricted Pod Security labels; individual integrations deliberately do not
render or claim that shared Namespace, so multiple Helm releases cannot fight
over one resource.

The mandatory label selector has the form
`kuberploy.io/dns-integration=<integration>`; the runtime chart adds it only when
a route explicitly selects that integration. The hostname annotation is also
required, so manual routes remain invisible even though their host appears in
the Ingress spec. `upsert-only` is the default. `sync` additionally requires the
explicit `foundation.allowDestructiveSync=true` acknowledgement.

When NetworkPolicy hardening is enabled, managed mode requires bounded
Kubernetes API CIDRs. Provider egress defaults to dual-stack public access on
the configured provider ports with those API CIDRs excluded; optional provider
CIDRs narrow that route. Provider ports remain
explicit and bounded. Credentials are references to existing Secret keys or workload
references. Plaintext env values, ambient workload-identity annotations,
rendered Secrets, webhook sidecars, and arbitrary argument/container/volume
injection fail closed. The immutable profile ConfigMap binds the safe provider
kind plus exact credential-Secret, provider-config, and egress reference names;
the edge observer independently checks the provider argument, sole credential
Secret reference, Deployment identity, and exact profile data before readiness.

Use `testdata/managed-values.yaml` as a shape example, replacing documentation
ranges and identities. `./test/e2e/render-edge-chart.sh` verifies the upstream
checksum, render determinism, filters, RBAC, network boundaries, runtime
identity substitution, and dangerous mutations. The platform API/UI owns the
authorized integration catalog and route selection; live provider/zone and
cleanup behavior remain part of conforming-cluster qualification.
