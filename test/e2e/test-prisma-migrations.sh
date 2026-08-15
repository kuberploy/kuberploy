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
kp_expected_counts=$'11\n110\n104\n101\n913\n13\n1'
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

# Exercise the ordered 003 -> 011 production upgrade, not only a fresh apply.
# Prisma owns the history rows; psql supplies the already-released SQL exactly
# as it existed before the new image starts.
docker exec "${kp_postgres}" createdb --username postgres upgrade
docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --set ON_ERROR_STOP=1 --command '
  CREATE TABLE _prisma_migrations (
    id varchar(36) PRIMARY KEY,
    checksum varchar(64) NOT NULL,
    finished_at timestamptz,
    migration_name varchar(255) NOT NULL,
    logs text,
    rolled_back_at timestamptz,
    started_at timestamptz NOT NULL DEFAULT now(),
    applied_steps_count integer NOT NULL DEFAULT 0
  );' >/dev/null
kp_migration_index=0
for kp_migration_name in \
  001_initial \
  002_team_access_grants \
  003_repair_protected_desired_revisions; do
  kp_migration_index=$((kp_migration_index + 1))
  kp_migration_file="${kp_root}/migrations/prisma/migrations/${kp_migration_name}/migration.sql"
  kp_container_file="/tmp/${kp_migration_name}.sql"
  docker cp "${kp_migration_file}" "${kp_postgres}:${kp_container_file}"
  docker exec "${kp_postgres}" psql --username postgres --dbname upgrade \
    --set ON_ERROR_STOP=1 --single-transaction --file "${kp_container_file}" >/dev/null
  if command -v sha256sum >/dev/null 2>&1; then
    kp_migration_checksum="$(sha256sum "${kp_migration_file}" | awk '{print $1}')"
  else
    kp_migration_checksum="$(shasum -a 256 "${kp_migration_file}" | awk '{print $1}')"
  fi
  kp_migration_id="00000000-0000-0000-0000-$(printf '%012d' "${kp_migration_index}")"
  docker exec "${kp_postgres}" psql --username postgres --dbname upgrade \
    --set ON_ERROR_STOP=1 --command "
      INSERT INTO _prisma_migrations(
        id,checksum,finished_at,migration_name,started_at,applied_steps_count
      ) VALUES(
        '${kp_migration_id}','${kp_migration_checksum}',now(),
        '${kp_migration_name}',now(),1
      );" >/dev/null
done

# Seed the removed pre-stable platform-upgrade state exactly as an old control
# plane could leave it. Migration 006 must retire mutable work without erasing
# terminal operation evidence.
docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --set ON_ERROR_STOP=1 --command "
  INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,lease_owner,lease_until) VALUES
    ('60000000-0000-4000-8000-000000000002','platform.upgrade','running','platform','60000000-0000-4000-8000-000000000012','running-upgrade',1,'legacy-upgrade-worker',now()+interval '5 minutes'),
    ('60000000-0000-4000-8000-000000000003','platform.upgrade','succeeded','platform','60000000-0000-4000-8000-000000000013','terminal-upgrade',1,NULL,NULL);
  INSERT INTO platform_upgrades(id,version,manifest_digest,manifest,state,operation_id,manifest_bytes) VALUES
    ('60000000-0000-4000-8000-000000000012','0.1.0-rc.1','sha256:'||repeat('b',64),'{}','running','60000000-0000-4000-8000-000000000002','{}'),
    ('60000000-0000-4000-8000-000000000013','0.1.0-rc.1','sha256:'||repeat('c',64),'{}','succeeded','60000000-0000-4000-8000-000000000003','{}');
  INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id) VALUES
    ('60000000-0000-4000-8000-000000000002','platform.upgrade','60000000-0000-4000-8000-000000000012',1,'running-upgrade');
" >/dev/null
kp_upgrade_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/upgrade?schema=public"
docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_upgrade_url}" "${kp_image}" >/dev/null
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT count(*) FROM _prisma_migrations WHERE finished_at IS NOT NULL AND rolled_back_at IS NULL")" == "11" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT to_regclass('public.helm_application_cascade_preflights') IS NOT NULL AND
          to_regclass('public.helm_application_cascade_observer_activations') IS NOT NULL AND
          to_regclass('public.helm_application_cascade_observation_jobs') IS NOT NULL AND
          to_regclass('public.helm_application_cascade_receipts') IS NOT NULL")" == "t" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT position('cascade-migration-replan-required' in
          pg_get_functiondef('public.validate_helm_application_continuation_receipt()'::regprocedure))>0")" == "t" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT to_regclass('public.platform_upgrades') IS NULL")" == "t" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT string_agg(status||':'||COALESCE(problem->>'code','none'),',' ORDER BY id) FROM operations WHERE kind='platform.upgrade'")" == "cancelled:FeatureRemoved,succeeded:none" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT (progress->0->>'name')||':'||(progress->0->>'status')||':'||(progress->0 ? 'finishedAt') FROM operations WHERE id='60000000-0000-4000-8000-000000000002'")" == "platform-upgrade:cancelled:true" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --tuples-only --no-align --command \
  "SELECT count(*) FROM outbox WHERE operation_id='60000000-0000-4000-8000-000000000002'")" == "0" ]]

# The 004 insertion fence must reject an old API/worker binary even after the
# one-time legacy cleanup has completed. Probe the same trigger function with
# the exact shared identity columns, then confirm both real table triggers exist.
docker exec "${kp_postgres}" psql --username postgres --dbname upgrade --set ON_ERROR_STOP=1 --command "
  CREATE TEMP TABLE old_helm_writer_probe(
    release_revision_id uuid, project_id uuid, environment_id uuid, application_id uuid,
    platform_binding_id uuid, environment_binding_id uuid, cluster_id uuid,
    environment_revision text, environment_generation bigint
  );
  CREATE TRIGGER old_helm_writer_probe_receipt
    BEFORE INSERT ON old_helm_writer_probe
    FOR EACH ROW EXECUTE FUNCTION require_helm_publication_prerequisite_receipt();
  DO \$\$
  BEGIN
    BEGIN
      INSERT INTO old_helm_writer_probe VALUES(
        '10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002',
        '10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000004',
        '10000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000006',
        '10000000-0000-4000-8000-000000000007','1111111111111111111111111111111111111111',1
      );
      RAISE EXCEPTION 'old Helm writer bypassed prerequisite receipt';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
  END
  \$\$;
  SELECT CASE WHEN count(*)=2 THEN 1 ELSE
    CAST(current_setting('kuberploy.missing_helm_receipt_triggers') AS integer) END
  FROM pg_trigger trigger
  JOIN pg_class relation ON relation.oid=trigger.tgrelid
  WHERE NOT trigger.tgisinternal
    AND relation.relname IN ('helm_protected_payload_intents','helm_protected_application_intents')
    AND trigger.tgname LIKE 'helm_protected_%_prerequisite_receipt';

  CREATE TEMP TABLE old_cascade_writer_probe(
    action text NOT NULL,cascade_required boolean NOT NULL DEFAULT false,
    cascade_receipt_id uuid,cascade_contract text NOT NULL DEFAULT '',
    release_revision_id uuid,payload_intent_id uuid,release_generation bigint,
    project_id uuid,environment_id uuid,application_id uuid,platform_binding_id uuid,
    environment_binding_id uuid,cluster_id uuid,platform_target_ref text,
    application_path text,expected_etag text
  );
  CREATE TRIGGER old_cascade_writer_probe_guard
    BEFORE INSERT ON old_cascade_writer_probe
    FOR EACH ROW EXECUTE FUNCTION validate_helm_application_cascade_gate();
  INSERT INTO old_cascade_writer_probe(action) VALUES('publish');
  DO \$\$
  BEGIN
    BEGIN
      INSERT INTO old_cascade_writer_probe(action) VALUES('delete');
      RAISE EXCEPTION 'old delete writer bypassed cascade authority';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
  END
  \$\$;
  SELECT CASE WHEN count(*)=1 THEN 1 ELSE
    CAST(current_setting('kuberploy.invalid_old_cascade_writer_rows') AS integer) END
  FROM old_cascade_writer_probe;
" >/dev/null
kp_upgrade_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_upgrade_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_upgrade_second}"

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

# The pre-stable migration history is squashed, so fresh databases enforce the
# environment-scoped AppProject identity directly instead of repairing an old
# release-candidate row later.
docker exec "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 --command "
  INSERT INTO projects(id,name,slug) VALUES
    ('11111111-1111-4111-8111-111111111111','Payments','payments');
  INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES
    ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111','Production','production','kp-payments-production','kp-payments-production');
  DO \$\$
  BEGIN
    BEGIN
      INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES
        ('33333333-3333-4333-8333-333333333333','11111111-1111-4111-8111-111111111111','Forged','forged','kp-payments-forged','kp-project-wide');
      RAISE EXCEPTION 'environment accepted a project-wide Argo identity';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
  END
  \$\$;
" >/dev/null
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

printf 'Prisma migration image delayed database wait, fresh and 003-to-011 apply, self-upgrade retirement, declarative drift, old-writer fencing, personal/team scope authority, idempotency, native authority, and legacy rejection passed\n'
