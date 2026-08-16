# Kuberploy PostgreSQL foundation

This independent chart provides the small, single-instance managed PostgreSQL
18.4 profile or records adoption of an operator-managed compatible endpoint. It
is fixed to `kuberploy-postgresql`, uses the exact multi-platform official image
digest, a retained RWO PVC, data checksums, SCRAM host authentication,
optional default-deny NetworkPolicy, no service-account token, restricted UID 70, a
read-only root filesystem, and bounded resources/configuration.

The chart never creates or prints credentials. Managed mode requires the
existing `kuberploy-postgresql-auth` Secret with `username`, `password`, and
`database`. The installer separately delivers the complete connection URL in
the `kuberploy-system` Secret consumed by API, worker, and the Helm migration Job.

This profile is for small installations and recovery testing. It deliberately
does not claim HA, TLS, backup, point-in-time recovery, or automatic major
upgrade. Operators needing those production properties should select adopted
mode and use a managed PostgreSQL/operator endpoint that passes version,
connectivity, TLS, migration, and restore preflight. The retained PVC is not a
backup and deletion remains an explicit operator action.

Run `./test/e2e/render-postgresql-chart.sh` for deterministic render and
mutation checks. A live lifecycle test requires an explicitly selected
non-production cluster.
