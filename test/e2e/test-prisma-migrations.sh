#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_suffix="${PPID}-$$"
kp_network="kuberploy-prisma-${kp_suffix}"
kp_postgres="kuberploy-prisma-pg-${kp_suffix}"
kp_delayed_postgres="kuberploy-prisma-delayed-pg-${kp_suffix}"
kp_waiter="kuberploy-prisma-waiter-${kp_suffix}"
kp_image="kuberploy-migration:test-${kp_suffix}"

kp_cleanup() {
  docker rm -f "${kp_waiter}" >/dev/null 2>&1 || true
  docker rm -f "${kp_delayed_postgres}" >/dev/null 2>&1 || true
  docker rm -f "${kp_postgres}" >/dev/null 2>&1 || true
  docker network rm "${kp_network}" >/dev/null 2>&1 || true
}
trap kp_cleanup EXIT

docker build \
  --file "${kp_root}/build/package/migration.Dockerfile" \
  --tag "${kp_image}" \
  --build-arg VERSION=test \
  --build-arg REVISION=test \
  --build-arg BUILD_DATE=1970-01-01T00:00:00Z \
  "${kp_root}" >/dev/null

docker network create "${kp_network}" >/dev/null

if docker run --rm \
  --env DATABASE_URL='https://postgres.invalid/example' \
  "${kp_image}" >/dev/null 2>&1; then
  printf 'Prisma migration image accepted a non-PostgreSQL URL\n' >&2
  exit 1
fi
if docker run --rm \
  --env DATABASE_URL='postgresql://postgres:unused@postgres.invalid/example' \
  --env KUBERPLOY_MIGRATION_DATABASE_WAIT_SECONDS='60seconds' \
  "${kp_image}" >/dev/null 2>&1; then
  printf 'Prisma migration image accepted a non-canonical wait duration\n' >&2
  exit 1
fi

kp_delayed_url="postgresql://postgres:kuberploy-test-only@${kp_delayed_postgres}:5432/postgres?schema=public"
docker run --detach \
  --name "${kp_waiter}" \
  --network "${kp_network}" \
  --env DATABASE_URL="${kp_delayed_url}" \
  --env KUBERPLOY_MIGRATION_DATABASE_WAIT_SECONDS=60 \
  "${kp_image}" >/dev/null
kp_waiter_announced=false
for _ in $(seq 1 30); do
  if docker logs "${kp_waiter}" 2>&1 | grep -q 'Waiting for the configured PostgreSQL endpoint'; then
    kp_waiter_announced=true
    break
  fi
  [[ "$(docker inspect --format '{{.State.Running}}' "${kp_waiter}")" == "true" ]] || {
    docker logs "${kp_waiter}" >&2
    printf 'Migration image exited before announcing its database wait\n' >&2
    exit 1
  }
  sleep 1
done
[[ "${kp_waiter_announced}" == "true" ]] || {
  docker logs "${kp_waiter}" >&2
  printf 'Migration image did not announce its database wait in time\n' >&2
  exit 1
}
docker run --detach --rm \
  --name "${kp_delayed_postgres}" \
  --network "${kp_network}" \
  --env POSTGRES_PASSWORD=kuberploy-test-only \
  --tmpfs /var/lib/postgresql \
  docker.io/library/postgres:18 >/dev/null
[[ "$(docker wait "${kp_waiter}")" == "0" ]]
docker logs "${kp_waiter}" 2>&1 | grep -q 'successfully applied'

docker run --detach --rm \
  --name "${kp_postgres}" \
  --network "${kp_network}" \
  --env POSTGRES_PASSWORD=kuberploy-test-only \
  --tmpfs /var/lib/postgresql \
  docker.io/library/postgres:18 >/dev/null

for _ in $(seq 1 60); do
  if docker exec "${kp_postgres}" pg_isready --username postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "${kp_postgres}" pg_isready --username postgres >/dev/null

docker exec "${kp_postgres}" createdb --username postgres fresh
kp_fresh_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/fresh?schema=public"
docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_fresh_url}" "${kp_image}" >/dev/null
kp_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_fresh_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_second}"

kp_counts="$(docker exec "${kp_postgres}" psql --username postgres --dbname fresh --tuples-only --no-align --command "
  SELECT count(*) FROM _prisma_migrations WHERE finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r';
  SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public';
  SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal;
  SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname='public' AND c.contype='c';
  SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname='public' AND c.condeferrable;
  SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND i.indexprs IS NOT NULL;
")"
kp_expected_counts=$'6\n102\n65\n70\n735\n10\n2'
if [[ "${kp_counts}" != "${kp_expected_counts}" ]]; then
  printf 'Unexpected fresh-schema authority counts:\n%s\n' "${kp_counts}" >&2
  exit 1
fi

docker run --rm --network "${kp_network}" \
  --env DATABASE_URL="${kp_fresh_url}" \
  --entrypoint ./node_modules/.bin/prisma \
  "${kp_image}" validate --config prisma.config.ts >/dev/null
docker run --rm --network "${kp_network}" \
  --env DATABASE_URL="${kp_fresh_url}" \
  --entrypoint node \
  "${kp_image}" check-schema-drift.mjs >/dev/null

# Personal projects have no team, while team-owned projects retain an exact
# organization fence. Prove both legal rows and reject both substitutions at
# the database boundary rather than relying only on API validation.
docker exec "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 --command "
  INSERT INTO users(id,login,role,issuer,subject) VALUES
    ('10000000-0000-4000-8000-000000000001','scope-admin','platform-admin','test','scope-admin');
  INSERT INTO teams(id,name,slug,created_by) VALUES
    ('10000000-0000-4000-8000-000000000002','Scope team','scope-team','10000000-0000-4000-8000-000000000001');
  INSERT INTO projects(id,name,slug,team_id) VALUES
    ('10000000-0000-4000-8000-000000000003','Personal','personal',NULL),
    ('10000000-0000-4000-8000-000000000004','Team owned','team-owned','10000000-0000-4000-8000-000000000002');
  INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES
    ('10000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000003','Personal','personal','kp-personal','kp-personal'),
    ('10000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000004','Team','team','kp-team','kp-team');
  INSERT INTO applications(id,project_id,name,slug) VALUES
    ('10000000-0000-4000-8000-000000000007','10000000-0000-4000-8000-000000000003','Personal API','personal-api'),
    ('10000000-0000-4000-8000-000000000008','10000000-0000-4000-8000-000000000004','Team API','team-api');
  INSERT INTO secret_bindings(id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,created_by,created_at,updated_at) VALUES
    ('10000000-0000-4000-8000-000000000009',NULL,'10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000007','kp-personal','personal-secret','sealed-secrets','provisioning','10000000-0000-4000-8000-000000000001',now(),now()),
    ('10000000-0000-4000-8000-000000000010','10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000008','kp-team','team-secret','sealed-secrets','provisioning','10000000-0000-4000-8000-000000000001',now(),now());
  DO \$\$
  BEGIN
    BEGIN
      INSERT INTO secret_bindings(id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,created_by,created_at,updated_at)
      VALUES('10000000-0000-4000-8000-000000000011','10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000007','kp-personal','forged-team','sealed-secrets','provisioning','10000000-0000-4000-8000-000000000001',now(),now());
      RAISE EXCEPTION 'personal project accepted a team organization';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
      INSERT INTO secret_bindings(id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,state,created_by,created_at,updated_at)
      VALUES('10000000-0000-4000-8000-000000000012',NULL,'10000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000008','kp-team','forged-personal','sealed-secrets','provisioning','10000000-0000-4000-8000-000000000001',now(),now());
      RAISE EXCEPTION 'team-owned project accepted an empty organization';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
  END
  \$\$;
" >/dev/null

# Prove migration 003 repairs a real preexisting project-wide AppProject value
# and is idempotent when its SQL is replayed by an operator during recovery.
docker exec "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 --command "
  INSERT INTO projects(id,name,slug) VALUES
    ('11111111-1111-4111-8111-111111111111','Payments','payments');
  INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES
    ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111','Production','production','kp-payments-production','kp-p-11111111111141118111111111111111');
" >/dev/null
docker exec --interactive "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 \
  <"${kp_root}/migrations/prisma/migrations/003_environment_scoped_argo_projects/migration.sql" >/dev/null
docker exec --interactive "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 \
  <"${kp_root}/migrations/prisma/migrations/003_environment_scoped_argo_projects/migration.sql" >/dev/null
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname fresh --tuples-only --no-align --command "SELECT argo_project FROM environments WHERE id='22222222-2222-4222-8222-222222222222'")" == "kp-payments-production" ]]

docker exec "${kp_postgres}" createdb --username postgres legacy
docker exec "${kp_postgres}" psql --username postgres --dbname legacy \
  --command 'CREATE TABLE schema_migrations(version text PRIMARY KEY);' >/dev/null
kp_legacy_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/legacy?schema=public"
if docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_legacy_url}" "${kp_image}" >/dev/null 2>&1; then
  printf 'Prisma migration image accepted a nonempty pre-Prisma database\n' >&2
  exit 1
fi
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname legacy --tuples-only --no-align --command "SELECT to_regclass('public.users') IS NULL")" == "t" ]]

printf 'Prisma migration image delayed database wait, fresh apply, declarative drift, personal/team scope authority, idempotency, native authority, and legacy rejection passed\n'
