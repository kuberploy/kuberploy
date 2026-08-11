# Database migrations

Kuberploy uses Prisma CLI 7.9.1 as a migration-only engine. The backend remains
Go with `pgx`; Prisma Client is neither generated nor shipped.

The authoritative history is native PostgreSQL SQL under
`prisma/migrations/<NNN_name>/migration.sql`. PostgreSQL functions, triggers,
CHECK and deferred constraints, expression indexes, and other database-owned
guards belong in that SQL. Do not replace them with application-only checks.

For a schema change:

1. Add the next immutable three-digit migration directory and SQL file.
2. Apply it to a fresh disposable PostgreSQL 18 database with
   `npm run deploy`.
3. Review `npm run pull` against that database. It prints the introspected
   model without replacing the minimal migration-only schema.
4. Bump `migrations.CurrentSchema` in `embed.go`.
5. Run `make prisma-migration-test` plus the normal Go, chart, and release
   gates.

`schema.prisma` is deliberately minimal. Print-only introspection is a
drift-review surface and does not supersede native SQL features that Prisma
cannot represent. The Helm
pre-install/pre-upgrade migration Job runs `prisma migrate deploy`; API and
worker startup only verify the exact completed migration names and checksums.

The migration image waits for the configured PostgreSQL TCP endpoint for at
most 480 seconds before invoking Prisma. This covers blank-cluster dependency
convergence without masking a missing database: the Kubernetes Job keeps a hard
active deadline, and Prisma or SQL failures remain terminal. Its pre-sync
NetworkPolicy permits only kube-dns and the configured PostgreSQL targets.
