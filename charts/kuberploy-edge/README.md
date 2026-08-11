# Kuberploy Traefik edge release

This standalone chart is the protected `kuberploy-edge` release. It manages the
locked Traefik chart in its Helm release namespace, or records an explicitly
confirmed compatible Traefik adoption. The installer and standalone installs
place it in the shared `kuberploy-system` namespace. It never owns cert-manager,
external-dns, tenant Ingresses, or the Kuberploy control plane; those have
independent Argo Applications and upgrade lifecycles.

The managed profile installs two explicit-version Traefik 3.7.10 replicas behind a
LoadBalancer Service, with a PDB, topology spread, restricted Pod Security,
dedicated upstream RBAC/service account, a non-default IngressClass, disabled
dashboard, safe Kubernetes providers, JSON access logs, TLS 1.2 minimum, and
NetworkPolicy. Only ports 80 and 443 are public. A separate ClusterIP metrics
Service exposes port 9100 only to the protected monitoring namespace, and an
exact ServiceMonitor there retains only Traefik request-count and latency
histogram series.

The wrapper passes Traefik's standard scheduling values through, including
`traefik.nodeSelector`, `traefik.tolerations`, `traefik.affinity`,
`traefik.topologySpreadConstraints`, and `traefik.priorityClassName`. This
supports dedicated or tainted ingress nodes without installing another ingress
controller. The default remains portable and uses hostname spreading.

Traefik backend egress is limited to namespaces carrying the immutable platform
label `kuberploy.io/runtime-namespace=true`. The environment controller must
apply that protected label when it owns a runtime namespace. Kubernetes API
CIDRs are required operator input and reject all-address ranges; determine the
actual API Service/endpoint addresses before install:

```yaml
edge:
  networkPolicy:
    kubeAPIServerCIDRs:
      - 10.43.0.1/32
```

HTTP-only, Let's Encrypt, and custom certificates are route-level choices and
may coexist. This chart advertises all three capabilities, but creates no
Certificate, application TLS Secret, global default certificate, or TLSStore.
The separate cert-manager release supplies approved issuers; the runtime chart
materializes per-route resources in application namespaces.

sslip.io hostnames are optional and disabled when `edge.traefik.sslip` is
omitted. They are intended for testing and convenience, not an availability
contract for production domains. Enable automatic selection only when the
LoadBalancer Service reports a literal public IPv4 address:

```yaml
edge:
  traefik:
    sslip:
      mode: auto-first-ip
```

`auto-first-ip` deterministically selects the first canonical public IPv4 from
the Service status. Private, shared, loopback, link-local, documentation,
benchmark, multicast, and reserved ranges are rejected. The profile ConfigMap
contains an empty `sslipStaticPublicIPv4`, exactly matching the runtime profile
attestation contract.

AWS ALB and many cloud LoadBalancers report a changing DNS hostname instead of
a literal address, so automatic mode remains unavailable. An operator may use
`verified-static-ip` only for an address whose stability they control, such as
an explicitly allocated NLB/EIP design:

```yaml
edge:
  traefik:
    sslip:
      mode: verified-static-ip
      staticPublicIPv4: "<operator-owned-public-ipv4>"
```

The observer does not trust that value by itself: every poll must find it as a
literal Service address or in the current IPv4 answers for the Service's
LoadBalancer hostname. Ordinary ALB addresses can rotate and should not be
treated as static. Multi-zone NLB designs must assess the availability impact
of binding a convenience hostname to one address; use an owned DNS name for
production traffic. `staticPublicIPv4` is rejected in automatic mode and is
required in verified-static mode.

Fetch/verify the locked dependency and render the mutation suite with
`./test/e2e/render-edge-chart.sh`. Install only into its protected namespace:

```sh
helm dependency build charts/kuberploy-edge
helm upgrade --install kuberploy-edge charts/kuberploy-edge \
  --namespace kuberploy-system --create-namespace \
  -f operator-edge-values.yaml
```

Adoption requires `managed=false`, `adoptExisting=true`, and
`crdProviderConfirmed=true`. Adoption deliberately does not claim an existing
Namespace; the protected `kuberploy-system` Namespace must already exist and carry
restricted Pod Security labels. Live version/CRD/IngressClass health observation
and managed-hostPort mode remain controller integration work.
