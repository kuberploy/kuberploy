# ADR 0002: Network egress and untrusted render boundary

- Status: Accepted for MVP foundation
- Date: 2026-08-06

## Decision

All user-configurable URLs enter through one validated endpoint policy. Git, OCI, registry, chart, metrics and secret-provider adapters must:

- accept only an explicit scheme supported by that adapter;
- reject embedded credentials, URL fragments and ambiguous host syntax;
- resolve DNS through a controlled resolver and reject loopback, link-local, multicast, metadata, cluster-control-plane and private ranges unless an administrator-approved integration explicitly allows the exact destination;
- revalidate the connected address after redirects and DNS changes;
- enforce redirect, response-size, decompression, timeout, concurrency and retry budgets;
- use adapter-specific egress NetworkPolicy and proxy settings;
- never forward credentials across host or scheme changes;
- log only a redacted canonical endpoint and decision reason.

YAML, JSON Schema and Helm/chart inputs are size- and count-bounded before parsing. Rendering occurs in an uncredentialed worker sandbox with a read-only filesystem, bounded CPU/memory/time/output, no host mounts and no network by default. Successful cached output is keyed by exact input digests and never bypasses current authorization or Git-head checks.

The conforming-cluster test profile may allow exact in-cluster Git, registry and ACME test Services. Those exceptions are explicit test configuration and are not inherited by production profiles.
