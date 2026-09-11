# Database migrations

Kuberploy uses Prisma CLI 7.9.1 to maintain and deploy the PostgreSQL schema.
The backend remains Go with `pgx`; Prisma Client is neither generated nor
shipped.

`prisma/schema.prisma` is the readable declarative source for tables, columns,
scalar types, primary keys, unique constraints, and indexes. The pre-stable
baseline and append-only post-stable deployment history are SQL under
`prisma/migrations/<NNN_name>/migration.sql`. PostgreSQL foreign-key authority
fences, functions, triggers, CHECK and deferred constraints, expression
indexes, and other database-owned guards belong in that SQL because Prisma
cannot represent them losslessly. Do not replace them with application-only
checks.

Before the first stable release:

1. Edit `prisma/schema.prisma` for the declarative part of the change.
2. Fold matching SQL and native PostgreSQL authority into
   `prisma/migrations/001_initial/migration.sql`.
3. Apply it to a fresh disposable PostgreSQL 18 database.
4. Review `npm run pull:print`.
5. Update the baseline checksum assertion in `embed_test.go`.
6. Run `npm run format`, `npm run validate`, `npm run check:drift`,
   `make prisma-migration-test`, and the normal Go, chart, and release gates.

`001_initial` is the replaceable pre-stable `0.1.0` baseline. Release-candidate
databases are disposable and must start fresh when this baseline changes.
After `0.1.0` becomes stable, freeze this checksum. Add the next ordered
three-digit migration for every later change, review its SQL, bump
`migrations.CurrentSchema`, and prove upgrade compatibility. Remove
`pg_dump`'s psql-only `\\restrict` /
`\\unrestrict` transport lines and its empty `search_path` session directive;
Prisma executes migration SQL directly and owns `_prisma_migrations`.

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
