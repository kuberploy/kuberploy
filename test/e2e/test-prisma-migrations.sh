#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_suffix="${PPID}-$$"
kp_network="kuberploy-prisma-${kp_suffix}"
kp_postgres="kuberploy-prisma-pg-${kp_suffix}"
kp_delayed_postgres="kuberploy-prisma-delayed-pg-${kp_suffix}"
kp_waiter="kuberploy-prisma-waiter-${kp_suffix}"
kp_signal_runner="kuberploy-prisma-signal-${kp_suffix}"
kp_image="kuberploy-migration:test-${kp_suffix}"

kp_cleanup() {
  docker rm -f "${kp_waiter}" >/dev/null 2>&1 || true
  docker rm -f "${kp_signal_runner}" >/dev/null 2>&1 || true
  docker rm -f "${kp_delayed_postgres}" >/dev/null 2>&1 || true
  docker rm -f "${kp_postgres}" >/dev/null 2>&1 || true
  docker network rm "${kp_network}" >/dev/null 2>&1 || true
  docker image rm "${kp_image}" >/dev/null 2>&1 || true
}
trap kp_cleanup EXIT
trap 'printf "Prisma migration e2e failed at line %s\n" "${LINENO}" >&2' ERR

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
kp_second="$(docker run --rm --network "${kp_network}" \
  --env DATABASE_URL="${kp_fresh_url}&connection_limit=4&pool_timeout=10&socket_timeout=10" \
  "${kp_image}" 2>&1)"
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
kp_expected_counts=$'17\n111\n110\n105\n930\n15\n1'
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

# Exercise the ordered 003 -> 017 production upgrade, not only a fresh apply.
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
    "SELECT count(*) FROM _prisma_migrations WHERE finished_at IS NOT NULL AND rolled_back_at IS NULL")" == "17" ]]
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

# RC171 shipped migration 011 with one unnecessary all-row UPDATE after adding
# columns whose defaults already backfill existing rows. PostgreSQL rolled the
# failed migration back atomically, while Prisma durably retained the failed
# checksum and error. Reproduce that exact production state from schema 010,
# then prove the next migration image safely resolves only that known failure,
# preserves the failed history row, replays immutable 011 with the bounded shim,
# and removes that shim through additive migration 012.
docker exec "${kp_postgres}" createdb --username postgres rc170_template
docker exec "${kp_postgres}" psql --username postgres --dbname rc170_template --set ON_ERROR_STOP=1 --command '
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
kp_rc170_index=0
for kp_migration_name in \
  001_initial \
  002_team_access_grants \
  003_repair_protected_desired_revisions \
  004_helm_publication_prerequisites \
  005_helm_unchanged_project_receipt \
  006_remove_platform_self_upgrade \
  007_helm_publisher_adoption \
  008_qualify_helm_intent_validators \
  009_helm_application_continuation \
  010_helm_application_materialization_bridge; do
  kp_rc170_index=$((kp_rc170_index + 1))
  kp_migration_file="${kp_root}/migrations/prisma/migrations/${kp_migration_name}/migration.sql"
  kp_container_file="/tmp/rc170-${kp_migration_name}.sql"
  docker cp "${kp_migration_file}" "${kp_postgres}:${kp_container_file}"
  docker exec "${kp_postgres}" psql --username postgres --dbname rc170_template \
    --set ON_ERROR_STOP=1 --single-transaction --file "${kp_container_file}" >/dev/null
  if command -v sha256sum >/dev/null 2>&1; then
    kp_migration_checksum="$(sha256sum "${kp_migration_file}" | awk '{print $1}')"
  else
    kp_migration_checksum="$(shasum -a 256 "${kp_migration_file}" | awk '{print $1}')"
  fi
  kp_migration_id="10000000-0000-0000-0000-$(printf '%012d' "${kp_rc170_index}")"
  docker exec "${kp_postgres}" psql --username postgres --dbname rc170_template \
    --set ON_ERROR_STOP=1 --command "
      INSERT INTO _prisma_migrations(
        id,checksum,finished_at,migration_name,started_at,applied_steps_count
      ) VALUES(
        '${kp_migration_id}','${kp_migration_checksum}',now(),
        '${kp_migration_name}',now(),1
      );" >/dev/null
done

# Foreign-key targets are irrelevant to the immutable terminal-row regression;
# replica mode bypasses their triggers while every physical CHECK constraint is
# still enforced. A normal UPDATE afterwards must reproduce the RC171 failure.
docker exec "${kp_postgres}" psql --username postgres --dbname rc170_template \
  --set ON_ERROR_STOP=1 --command "
    SET session_replication_role=replica;
    INSERT INTO public.helm_protected_application_intents(
      id,release_revision_id,payload_intent_id,release_generation,
      project_id,environment_id,application_id,action,
      platform_binding_id,environment_binding_id,cluster_id,
      platform_target_ref,environment_target_ref,environment_revision,
      environment_generation,catalog_digest,planned_base_revision,
      payload_revision,payload_path,source_directory,application_path,
      operation,precondition,expected_etag,content,content_digest,
      intent_digest,commit_trailer,publisher_contract,publisher_config_digest,
      message,state,next_attempt_at,attempts,consecutive_failures,
      last_failure_code,lease_epoch,created_at,updated_at,completed_at,
      prerequisite_contract,prerequisite_epoch,
      original_publisher_config_digest,publisher_adoption_epoch,
      continuation_required,continuation_contract
    ) VALUES(
      '71000000-0000-4000-8000-000000000001',
      '71000000-0000-4000-8000-000000000002',
      '71000000-0000-4000-8000-000000000003',1,
      '71000000-0000-4000-8000-000000000004',
      '71000000-0000-4000-8000-000000000005',
      '71000000-0000-4000-8000-000000000006','publish',
      '71000000-0000-4000-8000-000000000007',
      '71000000-0000-4000-8000-000000000008',
      '71000000-0000-4000-8000-000000000009',
      'refs/heads/main','refs/heads/main',repeat('a',40),1,
      'sha256:'||repeat('b',64),repeat('c',40),repeat('d',40),
      'helm/payloads/test.yaml','apps/test','apps/test.yaml',
      'create','create-if-absent','',decode('00','hex'),
      'sha256:'||repeat('e',64),'sha256:'||repeat('f',64),
      'Kuberploy-Helm-Application-Intent: 71000000-0000-4000-8000-000000000001',
      'helm-protected-publisher.v1','sha256:'||repeat('1',64),
      'terminal recovery fixture','failed',now(),1,1,'fixture-failed',0,
      now(),now(),now(),'',0,'sha256:'||repeat('1',64),0,false,''
    );
    SET session_replication_role=origin;
    DO \$rc171\$
    BEGIN
      BEGIN
        UPDATE public.helm_protected_application_intents
           SET prerequisite_epoch=prerequisite_epoch+1;
        RAISE EXCEPTION 'terminal Application intent unexpectedly mutable';
      EXCEPTION WHEN check_violation THEN NULL;
      END;
    END
    \$rc171\$;
  " >/dev/null
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc170_template --tuples-only --no-align --command \
  "SELECT state||':'||prerequisite_epoch FROM public.helm_protected_application_intents WHERE id='71000000-0000-4000-8000-000000000001'")" == "failed:0" ]]

docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_recovery
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_resolved_recovery
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_shim_resume
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_post011_resume
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_concurrent
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_signal
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_signal_012
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_fresh_011
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_tampered_full
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_tampered_owner
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_tampered_lane_owner
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_active_original
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_reversed_rollback
docker exec "${kp_postgres}" createdb --username postgres --template fresh rc171_bad_012_chronology
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc170_proactive
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_wrong_failure
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_partial_failure
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_altered_shim
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_missing_log
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_wrong_steps
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_duplicate_failure
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_unrelated_rollback
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_residual_authority
docker exec "${kp_postgres}" createdb --username postgres --template rc170_template rc171_shim_overload
kp_rc171_failed_checksum='666baa4526942038b2a01ea91ffbbeda201ae9485035063b4aed21b83cc286ad'
kp_rc171_failed_logs=$'Migration name: 011_helm_application_cascade_preflight\nDatabase error code: 23514\nERROR: Terminal Helm protected Application intents are immutable\nPL/pgSQL function public.validate_helm_protected_application_intent() line 16 at RAISE'

kp_install_exact_rc171_shim() {
  local kp_database="$1"
  docker exec "${kp_postgres}" psql --username postgres --dbname "${kp_database}" \
    --set ON_ERROR_STOP=1 --command "
      CREATE FUNCTION public.kuberploy_rc171_terminal_noop_shim() RETURNS trigger
      LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp
      AS \$shim\$BEGIN
    IF OLD.state IN ('verified','failed','superseded') AND
       NEW IS NOT DISTINCT FROM OLD AND
       (SELECT pg_catalog.count(*)=3
          FROM pg_catalog.pg_attribute AS attribute
          JOIN pg_catalog.pg_class AS relation ON relation.oid=attribute.attrelid
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
         WHERE namespace.nspname='public'
           AND relation.relname='helm_protected_application_intents'
           AND attribute.attname IN ('cascade_required','cascade_receipt_id','cascade_contract')
           AND NOT attribute.attisdropped) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END;\$shim\$;
      CREATE TRIGGER aa_kuberploy_rc171_terminal_noop_shim
        BEFORE UPDATE ON public.helm_protected_application_intents
        FOR EACH ROW EXECUTE FUNCTION public.kuberploy_rc171_terminal_noop_shim();" >/dev/null
}

# Run the immutable published migration without the recovery entrypoint. This
# must reproduce the production P3018 row and transactionally roll back every
# migration-011 schema effect.
kp_recovery_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_recovery?schema=public"
if docker run --rm --network "${kp_network}" \
  --env DATABASE_URL="${kp_recovery_url}" \
  --entrypoint ./node_modules/.bin/prisma \
  "${kp_image}" migrate deploy --config prisma.config.ts >/dev/null 2>&1; then
  printf 'Published migration 011 unexpectedly accepted terminal Application history\n' >&2
  exit 1
fi
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_recovery --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='011_helm_application_cascade_preflight'
     AND checksum='${kp_rc171_failed_checksum}'
     AND finished_at IS NULL AND rolled_back_at IS NULL
     AND applied_steps_count=0;
  SELECT count(*) FROM information_schema.columns
   WHERE table_schema='public' AND table_name='helm_protected_application_intents'
     AND column_name LIKE 'cascade_%';
")" == $'1\n0' ]]

for kp_database in rc171_resolved_recovery rc171_shim_resume rc171_partial_failure rc171_altered_shim; do
  docker exec "${kp_postgres}" psql --username postgres --dbname "${kp_database}" \
    --set ON_ERROR_STOP=1 --command "
      INSERT INTO public._prisma_migrations(
        id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
      ) VALUES(
        '17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
        '011_helm_application_cascade_preflight',
        \$failure\$${kp_rc171_failed_logs}\$failure\$,now(),
        CASE WHEN '${kp_database}'='rc171_resolved_recovery' THEN now() ELSE NULL END,0
      );" >/dev/null
done
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_concurrent \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',
      \$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0
    );" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_wrong_failure \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000001',repeat('0',64),
      '011_helm_application_cascade_preflight',
      \$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0
    );" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_missing_log \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight','Database error code: 23514',now(),0);" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_wrong_steps \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),1);" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_duplicate_failure \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,applied_steps_count)
    VALUES
      ('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
       '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0),
      ('17100000-0000-4000-8000-000000000002','${kp_rc171_failed_checksum}',
       '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0);" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_active_original \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count)
    VALUES
      ('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
       '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now()-interval '2 seconds',NULL,0),
      ('17100000-0000-4000-8000-000000000002','${kp_rc171_failed_checksum}',
       '011_helm_application_cascade_preflight','',now()-interval '1 second',now(),0);" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_reversed_rollback \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),now()-interval '1 second',0);" >/dev/null
kp_012_checksum="$(shasum -a 256 "${kp_root}/migrations/prisma/migrations/012_recover_rc171_cascade_preflight/migration.sql" | awk '{print $1}')"
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_bad_012_chronology \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count)
    SELECT '17100000-0000-4000-8000-000000000001','${kp_012_checksum}',
      '012_recover_rc171_cascade_preflight','',prior.finished_at+(current_row.started_at-prior.finished_at)/2,
      current_row.started_at+interval '1 second',0
      FROM public._prisma_migrations AS prior
      JOIN public._prisma_migrations AS current_row
        ON current_row.migration_name='012_recover_rc171_cascade_preflight' AND current_row.finished_at IS NOT NULL
     WHERE prior.migration_name='011_helm_application_cascade_preflight' AND prior.finished_at IS NOT NULL;" >/dev/null
kp_010_checksum="$(shasum -a 256 "${kp_root}/migrations/prisma/migrations/010_helm_application_materialization_bridge/migration.sql" | awk '{print $1}')"
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_unrelated_rollback \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_010_checksum}',
      '010_helm_application_materialization_bridge','unrelated',now(),now(),0);" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_partial_failure \
  --set ON_ERROR_STOP=1 --command "
    ALTER TABLE public.helm_protected_application_intents
      ADD COLUMN cascade_required boolean NOT NULL DEFAULT false;" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_altered_shim \
  --set ON_ERROR_STOP=1 --command "
    CREATE FUNCTION public.kuberploy_rc171_terminal_noop_shim() RETURNS trigger
    LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp
    AS \$shim\$BEGIN RETURN NEW; END;\$shim\$;
    CREATE TRIGGER aa_kuberploy_rc171_terminal_noop_shim
      BEFORE UPDATE ON public.helm_protected_application_intents
      FOR EACH ROW EXECUTE FUNCTION public.kuberploy_rc171_terminal_noop_shim();" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_residual_authority \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0);
    CREATE FUNCTION public.validate_helm_application_cascade_gate() RETURNS trigger
    LANGUAGE plpgsql AS \$residual\$BEGIN RETURN NEW; END;\$residual\$;" >/dev/null
kp_install_exact_rc171_shim rc171_shim_resume
kp_install_exact_rc171_shim rc171_post011_resume
kp_install_exact_rc171_shim rc171_signal
kp_install_exact_rc171_shim rc171_signal_012
kp_install_exact_rc171_shim rc171_tampered_full
kp_install_exact_rc171_shim rc171_tampered_owner
kp_install_exact_rc171_shim rc171_tampered_lane_owner
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,rolled_back_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',
      \$failure\$${kp_rc171_failed_logs}\$failure\$,now(),now(),0
    );" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_shim_overload \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(id,checksum,migration_name,logs,started_at,applied_steps_count)
    VALUES('17100000-0000-4000-8000-000000000001','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight',\$failure\$${kp_rc171_failed_logs}\$failure\$,now(),0);" >/dev/null
kp_install_exact_rc171_shim rc171_shim_overload
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_shim_overload \
  --set ON_ERROR_STOP=1 --command "
    CREATE FUNCTION public.kuberploy_rc171_terminal_noop_shim(integer) RETURNS integer
    LANGUAGE sql IMMUTABLE SET search_path=pg_catalog,pg_temp AS 'SELECT \$1';" >/dev/null
docker cp "${kp_root}/migrations/prisma/migrations/011_helm_application_cascade_preflight/migration.sql" \
  "${kp_postgres}:/tmp/immutable-011.sql"
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_full \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_full \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight','',now(),0
    );
    ALTER FUNCTION public.helm_application_cascade_preflight_is_fresh(uuid) COST 101;" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_owner \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_owner \
  --set ON_ERROR_STOP=1 --command "
    DO \$owner\$ BEGIN
      IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='rc171_tampered_owner') THEN
        CREATE ROLE rc171_tampered_owner NOLOGIN;
      END IF;
    END \$owner\$;
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight','',now(),0
    );
    ALTER FUNCTION public.helm_application_cascade_preflight_is_fresh(uuid)
      OWNER TO rc171_tampered_owner;" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_lane_owner \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_tampered_lane_owner \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,migration_name,logs,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',
      '011_helm_application_cascade_preflight','',now(),0
    );
    ALTER TABLE public.helm_protected_application_intents
      OWNER TO rc171_tampered_owner;" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_post011_resume \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_post011_resume \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,finished_at,migration_name,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',now(),
      '011_helm_application_cascade_preflight',now(),1
    );" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,finished_at,migration_name,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',now(),
      '011_helm_application_cascade_preflight',now(),1
    );" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_fresh_011 \
  --set ON_ERROR_STOP=1 --command "
    SET session_replication_role=replica;
    DELETE FROM public.helm_protected_application_intents;
    SET session_replication_role=origin;" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_fresh_011 \
  --set ON_ERROR_STOP=1 --single-transaction --file /tmp/immutable-011.sql >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_fresh_011 \
  --set ON_ERROR_STOP=1 --command "
    INSERT INTO public._prisma_migrations(
      id,checksum,finished_at,migration_name,started_at,applied_steps_count
    ) VALUES(
      '17100000-0000-4000-8000-000000000011','${kp_rc171_failed_checksum}',now(),
      '011_helm_application_cascade_preflight',now(),1
    );" >/dev/null

kp_recovery_output="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_recovery_url}" "${kp_image}" 2>&1)"
grep -q 'Resolved the exact failed RC171 migration as rolled back' <<<"${kp_recovery_output}"
grep -q 'Installed the bounded RC171 terminal-row migration shim' <<<"${kp_recovery_output}"
grep -q 'successfully applied' <<<"${kp_recovery_output}"
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_recovery --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='011_helm_application_cascade_preflight'
     AND checksum='${kp_rc171_failed_checksum}'
     AND finished_at IS NULL AND rolled_back_at IS NOT NULL
     AND applied_steps_count=0;
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='011_helm_application_cascade_preflight'
     AND checksum='${kp_rc171_failed_checksum}'
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='012_recover_rc171_cascade_preflight'
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT state||':'||prerequisite_epoch||':'||cascade_required||':'||cascade_contract
    FROM public.helm_protected_application_intents
   WHERE id='71000000-0000-4000-8000-000000000001';
  SELECT count(*) FROM pg_proc proc JOIN pg_namespace namespace ON namespace.oid=proc.pronamespace
   WHERE namespace.nspname='public' AND proc.proname='kuberploy_rc171_terminal_noop_shim';
")" == $'1\n1\n1\nfailed:0:false:\n0' ]]
kp_recovery_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_recovery_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_recovery_second}"

for kp_database in rc171_resolved_recovery rc171_shim_resume rc170_proactive; do
  kp_resume_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/${kp_database}?schema=public"
  kp_resume_output="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_resume_url}" "${kp_image}" 2>&1)"
  if [[ "${kp_database}" != "rc171_shim_resume" ]]; then
    grep -q 'Installed the bounded RC171 terminal-row migration shim' <<<"${kp_resume_output}"
  fi
  [[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname "${kp_database}" --tuples-only --no-align --command "
    SELECT count(*) FROM public._prisma_migrations
     WHERE migration_name='011_helm_application_cascade_preflight'
       AND checksum='${kp_rc171_failed_checksum}'
       AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
    SELECT count(*) FROM public._prisma_migrations
     WHERE migration_name='012_recover_rc171_cascade_preflight'
       AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
    SELECT state||':'||prerequisite_epoch FROM public.helm_protected_application_intents
     WHERE id='71000000-0000-4000-8000-000000000001';
  ")" == $'1\n1\nfailed:0' ]]
done

# Crash after immutable 011 commits but before 012 starts: the successful 011
# row and exact shim are durable. A restart must apply only 012 and remove it.
kp_post011_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_post011_resume?schema=public"
kp_post011_output="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_post011_url}" "${kp_image}" 2>&1)"
grep -q 'Applying migration `012_recover_rc171_cascade_preflight`' <<<"${kp_post011_output}"
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_post011_resume --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='012_recover_rc171_cascade_preflight'
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT count(*) FROM pg_proc proc JOIN pg_namespace namespace ON namespace.oid=proc.pronamespace
   WHERE namespace.nspname='public' AND proc.proname='kuberploy_rc171_terminal_noop_shim';
")" == $'1\n0' ]]

# Published RC171 can successfully install 011 on an empty database without
# ever having the recovery shim. Exact successful history + full authority is
# sufficient for additive 012 to run and remain idempotent.
kp_fresh_011_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_fresh_011?schema=public"
kp_fresh_011_output="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_fresh_011_url}" "${kp_image}" 2>&1)"
grep -q 'Applying migration `012_recover_rc171_cascade_preflight`' <<<"${kp_fresh_011_output}"
kp_fresh_011_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_fresh_011_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_fresh_011_second}"

# Two migration Jobs may overlap during a Helm retry. The outer advisory lock
# serializes recovery, so exactly one resolves/replays and both converge.
kp_concurrent_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_concurrent?schema=public"
kp_concurrent_one="$(mktemp)"
kp_concurrent_two="$(mktemp)"
docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_concurrent_url}" "${kp_image}" >"${kp_concurrent_one}" 2>&1 &
kp_concurrent_pid_one=$!
docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_concurrent_url}" "${kp_image}" >"${kp_concurrent_two}" 2>&1 &
kp_concurrent_pid_two=$!
wait "${kp_concurrent_pid_one}"
wait "${kp_concurrent_pid_two}"
[[ "$(grep -h -c 'Resolved the exact failed RC171 migration as rolled back' "${kp_concurrent_one}" "${kp_concurrent_two}" | awk '{sum += $1} END {print sum}')" == "1" ]]
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_concurrent --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='011_helm_application_cascade_preflight'
     AND finished_at IS NULL AND rolled_back_at IS NOT NULL;
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name IN ('011_helm_application_cascade_preflight','012_recover_rc171_cascade_preflight')
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
")" == $'1\n2' ]]
rm -f "${kp_concurrent_one}" "${kp_concurrent_two}"

# Terminate a real Prisma child while immutable 011 is blocked on its first
# table lock. The SQL transaction must leave no schema residue; a restart must
# converge without leaking the runner's session advisory lock.
kp_signal_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_signal?schema=public"
docker exec --env PGAPPNAME=rc171-signal-lock "${kp_postgres}" \
  psql --username postgres --dbname rc171_signal --set ON_ERROR_STOP=1 \
  --command "BEGIN; LOCK TABLE public.helm_protected_application_intents IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(60); COMMIT;" \
  >/dev/null 2>&1 &
kp_signal_locker_pid=$!
kp_signal_lock_observed=false
for _ in $(seq 1 30); do
  if [[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal --tuples-only --no-align --command \
    "SELECT count(*) FROM pg_stat_activity WHERE application_name='rc171-signal-lock' AND wait_event_type='Timeout'")" == "1" ]]; then
    kp_signal_lock_observed=true
    break
  fi
  sleep 0.2
done
[[ "${kp_signal_lock_observed}" == "true" ]]
docker run --name "${kp_signal_runner}" --network "${kp_network}" \
  --env DATABASE_URL="${kp_signal_url}" "${kp_image}" >/dev/null 2>&1 &
kp_signal_runner_pid=$!
kp_signal_migration_observed=false
for _ in $(seq 1 60); do
  if [[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal --tuples-only --no-align --command \
    "SELECT count(*) FROM public._prisma_migrations WHERE migration_name='011_helm_application_cascade_preflight' AND finished_at IS NULL AND rolled_back_at IS NULL")" == "1" ]]; then
    kp_signal_migration_observed=true
    break
  fi
  sleep 0.2
done
[[ "${kp_signal_migration_observed}" == "true" ]]
docker kill --signal TERM "${kp_signal_runner}" >/dev/null
if wait "${kp_signal_runner_pid}"; then
  printf 'Terminated migration runner exited successfully\n' >&2
  exit 1
fi
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal --set ON_ERROR_STOP=1 \
  --command "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='rc171-signal-lock';" >/dev/null
wait "${kp_signal_locker_pid}" 2>/dev/null || true
docker rm "${kp_signal_runner}" >/dev/null
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal --tuples-only --no-align --command "
  SELECT count(*) IN (0,3) FROM information_schema.columns
   WHERE table_schema='public' AND table_name='helm_protected_application_intents'
     AND column_name LIKE 'cascade_%';
  SELECT pg_try_advisory_lock(hashtextextended('kuberploy-prisma-migration-runner-v1',0));
  SELECT pg_advisory_unlock(hashtextextended('kuberploy-prisma-migration-runner-v1',0));
")" == $'t\nt\nt' ]]
set +e
kp_signal_restart="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_signal_url}" "${kp_image}" 2>&1)"
kp_signal_restart_status=$?
set -e
if [[ "${kp_signal_restart_status}" -ne 0 ]]; then
  printf 'Interrupted migration restart failed:\n%s\n' "${kp_signal_restart}" >&2
  exit 1
fi
grep -q 'successfully applied' <<<"${kp_signal_restart}"
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='011_helm_application_cascade_preflight'
     AND finished_at IS NULL AND rolled_back_at IS NOT NULL
     AND logs LIKE '%Terminal Helm protected Application intents are immutable%';
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name IN ('011_helm_application_cascade_preflight','012_recover_rc171_cascade_preflight')
     AND finished_at IS NULL AND rolled_back_at IS NOT NULL AND COALESCE(logs,'')='';
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name IN ('011_helm_application_cascade_preflight','012_recover_rc171_cascade_preflight')
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT count(*) FROM pg_proc proc JOIN pg_namespace namespace ON namespace.oid=proc.pronamespace
   WHERE namespace.nspname='public' AND proc.proname='kuberploy_rc171_terminal_noop_shim';
")" == $'1\n1\n2\n0' ]]
kp_signal_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_signal_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_signal_second}"

# Force the opposite crash boundary: migration 012 commits its shim cleanup,
# then Prisma blocks while finishing its history row. SIGTERM must leave the
# empty active attempt recoverable, replay unchanged 012, and converge.
kp_signal_012_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/rc171_signal_012?schema=public"
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
  --set ON_ERROR_STOP=1 --command "
    CREATE FUNCTION public.kuberploy_test_pause_012_history() RETURNS trigger
    LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS \$pause\$
    BEGIN
      IF OLD.migration_name='012_recover_rc171_cascade_preflight' AND
         OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL THEN
        PERFORM pg_catalog.pg_sleep(60);
      END IF;
      RETURN NEW;
    END;
    \$pause\$;
    CREATE TRIGGER kuberploy_test_pause_012_history
      BEFORE UPDATE ON public._prisma_migrations
      FOR EACH ROW EXECUTE FUNCTION public.kuberploy_test_pause_012_history();" >/dev/null
docker run --name "${kp_signal_runner}" --network "${kp_network}" \
  --env DATABASE_URL="${kp_signal_012_url}" "${kp_image}" >/dev/null 2>&1 &
kp_signal_runner_pid=$!
kp_signal_012_postcommit_observed=false
for _ in $(seq 1 100); do
  if [[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 --tuples-only --no-align --command "
    SELECT
      (SELECT count(*) FROM public._prisma_migrations
        WHERE migration_name='012_recover_rc171_cascade_preflight'
          AND finished_at IS NULL AND rolled_back_at IS NULL)=1 AND
      (SELECT count(*) FROM pg_catalog.pg_proc AS proc
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
       WHERE namespace.nspname='public' AND proc.proname='kuberploy_rc171_terminal_noop_shim')=0 AND
      (SELECT count(*) FROM pg_catalog.pg_stat_activity
       WHERE datname='rc171_signal_012' AND wait_event_type='Timeout')>=1;")" == "t" ]]; then
    kp_signal_012_postcommit_observed=true
    break
  fi
  sleep 0.2
done
[[ "${kp_signal_012_postcommit_observed}" == "true" ]]
docker kill --signal KILL "${kp_signal_runner}" >/dev/null
if wait "${kp_signal_runner_pid}"; then
  printf 'Killed migration-012 runner exited successfully\n' >&2
  exit 1
fi
docker rm "${kp_signal_runner}" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
  --set ON_ERROR_STOP=1 --command "
    SELECT pg_catalog.pg_terminate_backend(pid)
      FROM pg_catalog.pg_stat_activity
     WHERE datname='rc171_signal_012' AND wait_event_type='Timeout';" >/dev/null
docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
  --set ON_ERROR_STOP=1 --command "
    DROP TRIGGER kuberploy_test_pause_012_history ON public._prisma_migrations;
    DROP FUNCTION public.kuberploy_test_pause_012_history();" >/dev/null
set +e
kp_signal_012_restart="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_signal_012_url}" "${kp_image}" 2>&1)"
kp_signal_012_restart_status=$?
set -e
if [[ "${kp_signal_012_restart_status}" -ne 0 ]]; then
  printf 'Interrupted migration-012 restart failed:\n%s\n' "${kp_signal_012_restart}" >&2
  docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 \
    --expanded --command "SELECT migration_name,checksum,COALESCE(logs,''),finished_at,rolled_back_at,applied_steps_count FROM public._prisma_migrations WHERE finished_at IS NULL ORDER BY started_at,id" >&2
  exit 1
fi
grep -q 'Resolved the exact interrupted migration 012 as rolled back' <<<"${kp_signal_012_restart}"
grep -q 'successfully applied' <<<"${kp_signal_012_restart}"
kp_signal_012_second="$(docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_signal_012_url}" "${kp_image}" 2>&1)"
grep -q 'No pending migrations to apply' <<<"${kp_signal_012_second}"
[[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname rc171_signal_012 --tuples-only --no-align --command "
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name='012_recover_rc171_cascade_preflight'
     AND finished_at IS NULL AND rolled_back_at IS NOT NULL AND COALESCE(logs,'')=''
     AND applied_steps_count=1;
  SELECT count(*) FROM public._prisma_migrations
   WHERE migration_name IN ('011_helm_application_cascade_preflight','012_recover_rc171_cascade_preflight')
     AND finished_at IS NOT NULL AND rolled_back_at IS NULL;
  SELECT count(*) FROM pg_catalog.pg_proc AS proc
    JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
   WHERE namespace.nspname='public' AND proc.proname='kuberploy_rc171_terminal_noop_shim';
")" == $'1\n2\n0' ]]

for kp_database in \
  rc171_wrong_failure rc171_partial_failure rc171_altered_shim \
  rc171_missing_log rc171_wrong_steps rc171_duplicate_failure \
  rc171_unrelated_rollback rc171_residual_authority rc171_shim_overload \
  rc171_tampered_full rc171_tampered_owner rc171_tampered_lane_owner \
  rc171_active_original rc171_reversed_rollback rc171_bad_012_chronology; do
  kp_failure_url="postgresql://postgres:kuberploy-test-only@${kp_postgres}:5432/${kp_database}?schema=public"
  kp_failure_rolled_back_before="$(docker exec "${kp_postgres}" psql --username postgres --dbname "${kp_database}" --tuples-only --no-align --command \
    "SELECT count(*) FROM public._prisma_migrations WHERE migration_name='011_helm_application_cascade_preflight' AND rolled_back_at IS NOT NULL")"
  if docker run --rm --network "${kp_network}" --env DATABASE_URL="${kp_failure_url}" "${kp_image}" >/dev/null 2>&1; then
    printf 'Migration image recovered an unrecognized, partial, or altered RC171 failure\n' >&2
    exit 1
  fi
  [[ "$(docker exec "${kp_postgres}" psql --username postgres --dbname "${kp_database}" --tuples-only --no-align --command \
    "SELECT count(*) FROM public._prisma_migrations WHERE migration_name='011_helm_application_cascade_preflight' AND rolled_back_at IS NOT NULL")" == "${kp_failure_rolled_back_before}" ]]
done

# Personal projects have no team, while team-owned projects retain an exact
# organization fence. Prove both legal rows and reject both substitutions at
# the database boundary rather than relying only on API validation.
docker exec "${kp_postgres}" psql --username postgres --dbname fresh --set ON_ERROR_STOP=1 --command "
  INSERT INTO users(id,display_name,role,issuer,subject) VALUES
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

printf 'Prisma migration image delayed database wait, fresh and 003-to-017 apply, RC171 recovery, self-upgrade retirement, declarative drift, old-writer fencing, personal/team scope authority, idempotency, native authority, and legacy rejection passed\n'
