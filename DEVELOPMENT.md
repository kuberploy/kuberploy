# Developing Kuberploy

This guide covers local development and repository verification. Cluster
qualification is intentionally separate because it can mutate real external
systems; read [LOCAL_TESTING.md](LOCAL_TESTING.md) before running it.

## Toolchain

Exact supported tool versions are declared in [`mise.toml`](mise.toml):

- Go `1.26.5`
- Node.js `26.7.0`
- pnpm `11.20.0`
- Helm `4.2.3`
- kubectl `1.36.3`
- yq `4.53.3` for chart assertions
- ripgrep and jq for repository/render checks

Install them and the web dependencies:

```bash
mise install
corepack enable
pnpm --dir web install --frozen-lockfile
```

## Common commands

```bash
make test          # Go and web tests
make web-build     # production web build
make helm-lint     # chart lint and render suites
make check         # complete local verification
```

Useful focused targets are listed by `make help`. Backend packages can also be
tested directly with `go test ./internal/<package>`, and web tests with
`pnpm --dir web test --run <file>`.

## Repository layout

```text
api/                       OpenAPI and workflow contracts
charts/                    installer, control-plane, runtime, and dependency charts
cmd/                       API, worker, and supporting binaries
docs/adr/                  durable architecture decisions
examples/                  public, non-secret configuration examples
internal/                  control-plane implementation
migrations/                stable PostgreSQL baseline and future ordered migrations
release/                   deterministic release packaging and validation
schema/                    public configuration schemas
scripts/                   development and qualification commands
test/e2e/                  hermetic render and harness tests
web/                       React, Tailwind, and shadcn-based interface
```

## Local and private files

Never commit workstation state, credentials, provider account details, test
domains, kubeconfigs, private keys, or private deployment values. Keep them in
the ignored locations intended for local work:

```text
.secrets/       credentials and private test configuration
.local/         generated binaries and local runtime state
.checkpoints/   temporary progress or recovery notes
```

Public examples must use reserved example domains and synthetic identifiers.
Operator-facing releases use readable semantic versions. Cryptographic digests
remain valid only where they prove content integrity or immutable OCI content;
they are not presented as product versions.

## Kubernetes testing

The local harness never trusts an ambient kubectl context. Cluster-facing
commands require an absolute kubeconfig path and an exact context/API server:

```bash
export KUBECONFIG=/absolute/path/to/test-kubeconfig
export KUBERPLOY_TEST_CONTEXT=<exact-context>
export KUBERPLOY_TEST_SERVER=https://api.example.test:6443
export KUBERPLOY_E2E_RUN_ID=<unique-run-id>

make kubernetes-preflight
make kubernetes-smoke
```

Third-party integrations such as public DNS and ACME are configuration-tested
by default. A live provider qualification is opt-in and requires explicit
operator credentials and cleanup authority. The complete safety, inventory,
and teardown contract is documented in [LOCAL_TESTING.md](LOCAL_TESTING.md).

## Making changes

Keep these surfaces synchronized when a contract changes:

- Go domain and HTTP types
- OpenAPI and Arazzo documents
- JSON and Helm values schemas
- PostgreSQL migration and memory-store parity
- web API types and UI behavior
- chart render and adversarial tests

Run `gofmt`, Prettier, `go vet`, focused race tests, and `make check` in
proportion to the change. Never weaken a fail-closed readiness check merely to
make a local fixture pass.

## Database schema changes

`migrations/prisma/migrations/001_initial/migration.sql` is the final reviewed
`0.1.0` baseline, regenerated after RC86 qualification. Release-candidate
databases from before this reset must be recreated. Do not edit the baseline
after the first stable release. Add
every later schema change as the next ordered, immutable native SQL migration,
review the print-only `npm --prefix migrations run pull` output against the
migrated disposable database, and bump `migrations.CurrentSchema` in the same
change. Prisma is used only as the migration engine: PostgreSQL functions,
triggers, deferred constraints, expression indexes, and checks remain
authoritative native SQL; Kuberploy does not use Prisma Client.

The dedicated `kuberploy-migration` Helm hook runs `prisma migrate deploy`
before install or upgrade. API and worker startup only verify the exact Prisma
history and never mutate the schema. The verifier rejects pre-Prisma RC
histories instead of guessing an unsafe upgrade path; the first Prisma RC must
start with a fresh database.

Integration tests that set `KUBERPLOY_TEST_DATABASE_URL` must use a disposable
database and prove a fresh apply, an idempotent second migration run, and the
locked native PostgreSQL authority inventory.

## Releases

Release metadata lives in [`release/metadata.json`](release/metadata.json).
Reviewed `v<semantic-version>` tags trigger the release workflow, which validates
the source, builds native architecture images, packages charts deterministically,
publishes artifacts, and creates the GitHub prerelease or release. GitHub Actions
are referenced by major version so Dependabot can update supported action majors
without embedding commit identifiers in workflow files.

See [`release/README.md`](release/README.md) for the complete release contract.
