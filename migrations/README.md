# Database migrations

Kuberploy uses Prisma CLI 7.9.1 to maintain and deploy the PostgreSQL schema.
The backend remains Go with `pgx`; Prisma Client is neither generated nor
shipped.

`prisma/schema.prisma` is the readable declarative source for tables, columns,
scalar types, primary keys, unique constraints, and indexes. The append-only
post-stable deployment history is SQL under
`prisma/migrations/<NNN_name>/migration.sql`. PostgreSQL foreign-key authority
fences, functions, triggers, CHECK and deferred constraints, expression
indexes, and other database-owned guards belong in that SQL because Prisma
cannot represent them losslessly. Do not replace them with application-only
checks.

For a schema change:

1. Edit `prisma/schema.prisma` for the declarative part of the change.
2. Add the next ordered three-digit migration directory and review its SQL.
   Add the native PostgreSQL authority SQL that Prisma cannot express.
3. Apply it to a fresh disposable PostgreSQL 18 database with
   `npm run deploy`.
4. Run `npm run pull` against that disposable database and review the schema
   diff. `npm run pull:print` is available for a non-writing inspection.
5. Bump `migrations.CurrentSchema` in `embed.go`.
6. Run `npm run format`, `npm run validate`, `npm run check:drift`,
   `make prisma-migration-test`, and the normal Go, chart, and release gates.

`001_initial` is the reviewed final `0.1.0` baseline. It includes every
release-candidate schema correction and therefore intentionally requires a
fresh database for installations created from an older RC migration history.
While `0.1.0` remains pre-stable, regenerate this one schema-only PostgreSQL 18
baseline from the current authoritative schema instead of adding incremental
release-candidate migrations. Remove `pg_dump`'s psql-only `\\restrict` /
`\\unrestrict` transport lines and its empty `search_path` session directive;
Prisma executes migration SQL directly and owns `_prisma_migrations`.
After `0.1.0` is stable, this checksum and history cannot be rewritten and every
schema change must use a new ordered migration.

The baseline separates presentation `display_name` from local-auth `email`.
Fresh installs ask for an administrator email and display name separately;
invitation records bind an email, while invitees choose their own display
name. This release supports local email/password authentication only; SSO/OIDC
is future scope. The API does not accept display names as login identifiers.

The schema uses `relationMode = "prisma"` only to prevent introspection from
inventing invalid one-to-one relations for Kuberploy's overlapping composite
foreign-key fences. The application does not use Prisma relation emulation;
the actual PostgreSQL foreign keys remain authoritative native migration SQL.
The Helm pre-install/pre-upgrade migration Job runs `prisma migrate deploy` and
then compares Prisma's introspected declarative schema with the checked-in
schema. Unsupported structural drift fails the Job before API or worker
rollout; API and worker startup also verify the exact completed migration names
and checksums.

The migration image waits for the configured PostgreSQL TCP endpoint for at
most 480 seconds before invoking Prisma. This covers blank-cluster dependency
convergence without masking a missing database: the Kubernetes Job keeps a hard
active deadline, and Prisma or SQL failures remain terminal. Its pre-sync
NetworkPolicy permits only kube-dns and the configured PostgreSQL targets.
