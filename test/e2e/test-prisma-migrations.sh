#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_suffix="${PPID}-$$"
kp_network="kuberploy-prisma-${kp_suffix}"
kp_postgres="kuberploy-prisma-pg-${kp_suffix}"
kp_image="kuberploy-migration:test-${kp_suffix}"

kp_cleanup() {
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
[[ "${kp_counts}" == $'1\n127\n81\n89\n876\n12\n2' ]]

docker exec "${kp_postgres}" createdb --username postgres legacy
docker exec "${kp_postgres}" psql --username postgres --dbname legacy \
  --command 'CREATE TABLE schema_migrations(version text PRIMARY KEY);' >/dev/null
kp_legacy_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/legacy?schema=public"
if docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_legacy_url}" "${kp_image}" >/dev/null 2>&1; then
  printf 'Prisma migration image accepted a nonempty pre-Prisma database\n' >&2
  exit 1
fi
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname legacy --tuples-only --no-align --command "SELECT to_regclass('public.users') IS NULL")" == "t" ]]

printf 'Prisma migration image fresh apply, idempotency, native authority, and legacy rejection passed\n'
