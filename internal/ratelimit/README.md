# API rate limiting

This package supplies two fixed-window limiters with identical closed request
semantics:

- `ValkeyLimiter` uses one atomic Lua operation and a schema-versioned,
  purpose-specific key. Subjects are SHA-256-derived opaque key components;
  user IDs, IP addresses, emails, tokens, and route input never appear in
  Valkey keys.
- `MemoryLimiter` is a bounded conservative per-process fallback for ordinary
  authenticated reads. It is not an allowed fallback for token issuance,
  secret writes, authentication exchanges, or other credential/high-risk
  operations.

Buckets, limits, costs, and windows are server-owned policy. The request model
rejects caller-controlled bucket names, control characters, excessive windows,
and unbounded counters. Local state evicts expired or least-recently-used
buckets at a fixed capacity.

The API applies the distributed limiter to bootstrap and invitation exchange,
invitation issuance, access-control changes, service-account and token
management, runtime-secret writes, and platform upgrades. It returns `429`
with a rounded-up `Retry-After` when the distributed decision denies a request.
If Valkey is unavailable, these endpoints return retryable `503` with
`Retry-After`; they never proceed through the memory fallback. Authenticated
buckets use the already-authorized user ID. Unauthenticated buckets use only
the transport peer address and deliberately ignore forwarding headers until a
separately configured trusted-proxy boundary exists. Authorization is always
evaluated independently and never cached here.
