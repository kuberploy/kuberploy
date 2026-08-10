# RFC 2136 qualification authority

This package is a repository-owned, qualification-only authoritative DNS
server. It exists so the conforming-cluster workflow can prove an actual
ExternalDNS reconciliation without borrowing a developer's public DNS zone.
It is not part of the Kuberploy API or worker images and is not a production
DNS server.

`cmd/kuberploy-rfc2136-test-provider` serves UDP and TCP on port 5353 as a
non-root process. UPDATE is accepted only for the configured zone, only with
the exact configured TSIG key, and only with HMAC-SHA256. The server supports
bounded A, AAAA, CNAME, and TXT data, which covers the ExternalDNS qualification
contract. Queries outside the zone and unsigned, weakly signed, oversized, or
unsupported updates fail closed.

The cluster workflow must supply a cluster-pullable immutable image reference
for `build/package/rfc2136-test-provider.Dockerfile`. The workflow creates a
run-owned TSIG Secret and never records the secret in evidence. The image
reference, zone, Service, NetworkPolicy, managed ExternalDNS integration, DNS
answer, and cleanup inventory are all bound to the qualification run. A local
Docker tag or an unpinned image is not accepted as live evidence.

Required environment variables:

- `KUBERPLOY_RFC2136_ZONE`
- `KUBERPLOY_RFC2136_NAMESERVER` (inside the zone)
- `KUBERPLOY_RFC2136_TSIG_NAME`
- `KUBERPLOY_RFC2136_TSIG_SECRET` (canonical padded base64, 16–64 decoded bytes)

Optional variables are `KUBERPLOY_RFC2136_LISTEN_ADDRESS` (default
`0.0.0.0:5353`) and `KUBERPLOY_RFC2136_TTL_SECONDS` (1–300, default 30).
