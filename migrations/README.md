# Database migrations

Kuberploy uses Prisma CLI 7.9.1 to maintain and deploy the PostgreSQL schema.
The backend remains Go with `pgx`; Prisma Client is neither generated nor
shipped.

`prisma/schema.prisma` is the readable declarative source for tables, columns,
scalar types, primary keys, unique constraints, and indexes. The immutable
post-stable deployment history is SQL under
`prisma/migrations/<NNN_name>/migration.sql`. PostgreSQL foreign-key authority
fences, functions, triggers, CHECK and deferred constraints, expression
indexes, and other database-owned guards belong in that SQL because Prisma
cannot represent them losslessly. Do not replace them with application-only
checks.

For a schema change:

1. Edit `prisma/schema.prisma` for the declarative part of the change.
2. Add the next immutable three-digit migration directory and review its SQL.
   Add the native PostgreSQL authority SQL that Prisma cannot express.
3. Apply it to a fresh disposable PostgreSQL 18 database with
   `npm run deploy`.
4. Run `npm run pull` against that disposable database and review the schema
   diff. `npm run pull:print` is available for a non-writing inspection.
5. Bump `migrations.CurrentSchema` in `embed.go`.
6. Run `npm run format`, `npm run validate`, `npm run check:drift`,
   `make prisma-migration-test`, and the normal Go, chart, and release gates.

`001_initial` is the final pre-stable `0.1.0` baseline. It was regenerated from
the fully qualified release-candidate schema after RC86. Databases created by
older release candidates must be recreated; Kuberploy never guesses across a
changed pre-stable migration checksum. After `0.1.0` is stable, this baseline is
immutable and every change must use a new ordered migration.

The schema uses `relationMode = "prisma"` only to prevent introspection from
inventing invalid one-to-one relations for Kuberploy's overlapping composite
foreign-key fences. The application does not use Prisma relation emulation;
the actual PostgreSQL foreign keys remain authoritative native migration SQL.
The Helm pre-install/pre-upgrade migration Job runs `prisma migrate deploy`;
API and worker startup only verify the exact completed migration names and
checksums.

The migration image waits for the configured PostgreSQL TCP endpoint for at
most 480 seconds before invoking Prisma. This covers blank-cluster dependency
convergence without masking a missing database: the Kubernetes Job keeps a hard
active deadline, and Prisma or SQL failures remain terminal. Its pre-sync
NetworkPolicy permits only kube-dns and the configured PostgreSQL targets.
