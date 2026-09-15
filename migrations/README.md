# Database migrations

Kuberploy uses Prisma CLI 7.9.1 to maintain and deploy the PostgreSQL schema.
The backend remains Go with `pgx`; Prisma Client is neither generated nor
shipped.

`prisma/schema.prisma` is the readable declarative source for tables, columns,
scalar types, primary keys, unique constraints, and indexes. The published
baseline and append-only deployment history are SQL under
`prisma/migrations/<NNN_name>/migration.sql`. PostgreSQL foreign-key authority
fences, functions, triggers, CHECK and deferred constraints, expression
indexes, and other database-owned guards belong in that SQL because Prisma
cannot represent them losslessly. Do not replace them with application-only
checks.

For every schema change, including release candidates that preserve existing data:

1. Edit `prisma/schema.prisma` for the declarative part of the change.
2. Add the next ordered three-digit migration containing matching SQL and native
   PostgreSQL authority. Never rewrite a published migration.
3. Apply the full history to a fresh disposable PostgreSQL 18 database and prove
   an upgrade from the previous schema with representative retained data.
4. Review `npm run pull:print`.
5. Bump `migrations.CurrentSchema` and update history assertions in `embed_test.go`,
   preserving the published baseline checksum.
6. Run `npm run format`, `npm run validate`, `npm run check:drift`,
   `make prisma-migration-test`, and the normal Go, chart, and release gates.

`001_initial` is the published baseline. `0.1.0-rc.483` squashed every prior
release-candidate migration (including the former
`002_secret_history_retention`) back into `001_initial` one final time before
stable release — see
[`docs/adr/0010-baseline-reset-before-stable.md`](../docs/adr/0010-baseline-reset-before-stable.md).
Upgrading in place from any earlier `0.1.0-rc.*` install is not supported;
those installs must start from a fresh database. From `0.1.0-rc.483` onward,
release candidates use append-only upgrades to preserve existing installation
data, same as before the reset. Remove
`pg_dump`'s psql-only `\\restrict` /
`\\unrestrict` transport lines and its empty `search_path` session directive;
Prisma executes migration SQL directly and owns `_prisma_migrations`.

The squashed baseline retains immutable secret deliveries, lifecycle events,
and mutation receipts when a deleted binding's App or Environment is removed.
Native insertion guards validate and lock exact existing binding and version
identities; immutable update/delete guards remain in force. No historical
rows, credentials, or parent resources are rewritten by the migration.

Every migration after this baseline runs atomically before API/worker rollout.
Existing processes can continue reads and writes during rollout on the schema
they know; a binary that knows only an older migration name refuses to start
against a newer one. Do not downgrade the database or restore dropped
constraints after a later migration has removed them. Recover using a chart
and binary that support the same schema (or a later compatible schema),
preserving the database.

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
