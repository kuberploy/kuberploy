import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import net from "node:net";
import { spawn } from "node:child_process";
import postgres from "postgres";

const failedRC171Migration = "011_helm_application_cascade_preflight";
const failedRC171Checksum =
  "666baa4526942038b2a01ea91ffbbeda201ae9485035063b4aed21b83cc286ad";
const failedRC171LogFragments = [
  "Migration name: 011_helm_application_cascade_preflight",
  "Database error code: 23514",
  "Terminal Helm protected Application intents are immutable",
  "PL/pgSQL function public.validate_helm_protected_application_intent()",
];
const migrationRunnerLock = "kuberploy-prisma-migration-runner-v1";
const shimFunction = "kuberploy_rc171_terminal_noop_shim";
const shimTrigger = "aa_kuberploy_rc171_terminal_noop_shim";
const shimFunctionBody = `BEGIN
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
END;`;
const cascadeAuthoritySHA256 =
  "63395f03e16c78fbcc7d717c38f13540dcaf052320fa00174f36dc14639f84d0";

const databaseURL = process.env.DATABASE_URL;
if (!databaseURL) {
  console.error("DATABASE_URL is required");
  process.exit(2);
}

let endpoint;
try {
  endpoint = new URL(databaseURL);
} catch {
  console.error("DATABASE_URL must be a valid PostgreSQL URL");
  process.exit(2);
}
if (endpoint.protocol !== "postgres:" && endpoint.protocol !== "postgresql:") {
  console.error("DATABASE_URL must use postgres or postgresql");
  process.exit(2);
}
const configuredSchema = endpoint.searchParams.get("schema");
if (configuredSchema !== null && configuredSchema !== "public") {
  console.error("DATABASE_URL schema must be public");
  process.exit(2);
}
const runnerEndpoint = new URL(endpoint);
for (const prismaOnlyParameter of [
  "schema",
  "connection_limit",
  "pool_timeout",
  "socket_timeout",
  "pgbouncer",
  "statement_cache_size",
]) {
  runnerEndpoint.searchParams.delete(prismaOnlyParameter);
}

const waitText = process.env.KUBERPLOY_MIGRATION_DATABASE_WAIT_SECONDS ?? "480";
const waitSeconds = /^\d+$/.test(waitText) ? Number.parseInt(waitText, 10) : NaN;
if (!Number.isSafeInteger(waitSeconds) || waitSeconds < 1 || waitSeconds > 480) {
  console.error("KUBERPLOY_MIGRATION_DATABASE_WAIT_SECONDS must be between 1 and 480");
  process.exit(2);
}

const host = endpoint.hostname;
const port = endpoint.port ? Number.parseInt(endpoint.port, 10) : 5432;
const deadline = Date.now() + waitSeconds * 1000;

function probe() {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port });
    const finish = (ready) => {
      socket.destroy();
      resolve(ready);
    };
    socket.setTimeout(2000);
    socket.once("connect", () => finish(true));
    socket.once("timeout", () => finish(false));
    socket.once("error", () => finish(false));
  });
}

let announced = false;
while (!(await probe())) {
  if (!announced) {
    console.log("Waiting for the configured PostgreSQL endpoint");
    announced = true;
  }
  if (Date.now() >= deadline) {
    console.error("PostgreSQL did not become reachable before the migration deadline");
    process.exit(1);
  }
  await new Promise((resolve) => setTimeout(resolve, 2000));
}

let activeChild;
let terminationSignal;
let terminationTimer;
function terminateSelf(signal) {
  if (terminationTimer) {
    clearTimeout(terminationTimer);
    terminationTimer = undefined;
  }
  for (const registered of ["SIGINT", "SIGTERM"]) {
    process.removeAllListeners(registered);
  }
  process.exit(signal === "SIGINT" ? 130 : 143);
}
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    if (activeChild) {
      if (terminationSignal) {
        activeChild.kill("SIGKILL");
        return;
      }
      terminationSignal = signal;
      activeChild.kill(signal);
      terminationTimer = setTimeout(() => activeChild?.kill("SIGKILL"), 5000);
      return;
    }
    terminateSelf(signal);
  });
}

function runChild(command, args, label) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      stdio: "inherit",
      env: process.env,
    });
    activeChild = child;
    child.once("error", (error) => {
      activeChild = undefined;
      reject(new Error(`Unable to start ${label}: ${error.message}`));
    });
    child.once("exit", (code, signal) => {
      activeChild = undefined;
      if (terminationSignal) {
        terminateSelf(terminationSignal);
        return;
      }
      if (signal) {
        terminateSelf(terminationSignal ?? signal);
        return;
      }
      if (code !== 0) {
        reject(new Error(`${label} exited with status ${code ?? 1}`));
        return;
      }
      resolve();
    });
  });
}

function runPrisma(args) {
  return runChild("./node_modules/.bin/prisma", args, "Prisma Migrate");
}

function runSchemaDriftCheck() {
  return runChild(
    process.execPath,
    ["check-schema-drift.mjs"],
    "schema drift check",
  );
}

async function expectedMigrationChecksums() {
  const migrationsRoot = new URL("./prisma/migrations/", import.meta.url);
  const entries = await readdir(migrationsRoot, { withFileTypes: true });
  const migrations = entries
    .filter((entry) => entry.isDirectory())
    .sort((left, right) => left.name.localeCompare(right.name));
  const expected = [];
  for (const migration of migrations) {
    const source = await readFile(
      new URL(`${migration.name}/migration.sql`, migrationsRoot),
    );
    expected.push({
      name: migration.name,
      checksum: createHash("sha256").update(source).digest("hex"),
    });
  }
  return expected;
}

function isExactFailure(row) {
  const logs = row.logs ?? "";
  return (
    row.migration_name === failedRC171Migration &&
    row.checksum === failedRC171Checksum &&
    row.applied_steps_count === 0 &&
    failedRC171LogFragments.every((fragment) => logs.includes(fragment))
  );
}

function isInterruptedMigration(row) {
  return (
    row.checksum.length === 64 &&
    ((row.migration_name === failedRC171Migration &&
      row.applied_steps_count === 0) ||
      (row.migration_name === "012_recover_rc171_cascade_preflight" &&
        (row.applied_steps_count === 0 || row.applied_steps_count === 1))) &&
    (row.logs ?? "") === ""
  );
}

function isRecoverableFailure(row) {
  return isExactFailure(row) || isInterruptedMigration(row);
}

async function cascadeMarkers(sql) {
  const [markers] = await sql`
    SELECT
      (
        SELECT count(*)::integer
          FROM information_schema.columns
         WHERE table_schema='public'
           AND table_name='helm_protected_application_intents'
           AND column_name IN ('cascade_required','cascade_receipt_id','cascade_contract')
      ) AS cascade_column_count,
      pg_catalog.to_regclass('public.helm_application_cascade_preflights') IS NOT NULL
        AS cascade_preflights,
      pg_catalog.to_regclass('public.helm_application_cascade_adoption_receipts') IS NOT NULL
        AS cascade_adoptions,
      pg_catalog.to_regclass('public.helm_application_cascade_observer_activations') IS NOT NULL
        AS cascade_activations,
      pg_catalog.to_regclass('public.helm_application_cascade_observation_jobs') IS NOT NULL
        AS cascade_jobs,
      pg_catalog.to_regclass('public.helm_application_cascade_receipts') IS NOT NULL
        AS cascade_receipts,
      (
        SELECT count(*)::integer
          FROM pg_catalog.pg_proc AS proc
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
         WHERE namespace.nspname='public'
           AND proc.proname=ANY(ARRAY[
             'validate_helm_application_cascade_observer_activation',
             'helm_application_cascade_active_observer_is_exact',
             'activate_helm_application_cascade_observer',
             'helm_application_active_publisher_is_exact',
             'validate_helm_application_active_publisher_claim',
             'helm_application_cascade_preflight_is_fresh',
             'helm_application_cascade_expected_child_spec_digest',
             'helm_application_cascade_expected_root_spec_digest',
             'validate_helm_protected_cascade_lane',
             'validate_helm_application_cascade_gate',
             'validate_helm_application_cascade_exact_gate',
             'validate_helm_application_cascade_observation_job',
             'validate_helm_application_cascade_receipt',
             'validate_helm_application_cascade_preflight',
             'validate_helm_application_cascade_adoption_receipt',
             'validate_helm_application_cascade_adoption_postimage',
             'adopt_helm_application_cascade_preflight',
             'helm_application_cascade_observation_is_exact',
             'helm_application_cascade_is_exact'
           ])
      ) AS cascade_function_count,
      (
        SELECT count(*)::integer
          FROM pg_catalog.pg_trigger AS trigger
          JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
         WHERE namespace.nspname='public' AND NOT trigger.tgisinternal
           AND trigger.tgname=ANY(ARRAY[
             'helm_application_cascade_observer_activation_guard',
             'helm_protected_payload_active_publisher_claim',
             'helm_protected_application_active_publisher_claim',
             'helm_application_cascade_active_publisher_claim',
             'helm_protected_payload_cascade_lane_guard',
             'helm_protected_application_cascade_lane_guard',
             'helm_application_cascade_lane_guard',
             'helm_application_cascade_gate',
             'helm_application_cascade_exact_gate',
             'helm_application_cascade_observation_job_guard',
             'helm_application_cascade_receipt_guard',
             'helm_application_cascade_preflight_guard',
             'helm_application_cascade_adoption_receipt_guard',
             'helm_application_cascade_adoption_postimage'
           ])
      ) AS cascade_trigger_count,
      (
        SELECT count(*)::integer
          FROM pg_catalog.pg_class AS relation
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
         WHERE namespace.nspname='public'
           AND relation.relkind='i'
           AND relation.relname LIKE 'helm_application_cascade_%'
      ) AS cascade_index_count,
      (
        SELECT count(*)::integer
          FROM pg_catalog.pg_constraint AS constraint_row
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=constraint_row.connamespace
         WHERE namespace.nspname='public'
           AND constraint_row.conname LIKE 'helm_application_cascade_%'
      ) AS cascade_constraint_count,
      COALESCE(pg_catalog.strpos(pg_catalog.pg_get_functiondef(
        pg_catalog.to_regprocedure('public.validate_helm_application_continuation_receipt()')),
        'cascade-migration-replan-required')>0,false) AS continuation_rewritten,
      COALESCE(pg_catalog.strpos(pg_catalog.pg_get_functiondef(
        pg_catalog.to_regprocedure('public.validate_helm_protected_application_intent()')),
        'cascade.adopted_content_digest')>0,false) AS application_validator_rewritten
  `;
  return markers;
}

function hasAnyCascadeMarker(markers) {
  return (
    markers.cascade_column_count !== 0 ||
    markers.cascade_preflights ||
    markers.cascade_adoptions ||
    markers.cascade_activations ||
    markers.cascade_jobs ||
    markers.cascade_receipts ||
    markers.cascade_function_count !== 0 ||
    markers.cascade_trigger_count !== 0 ||
    markers.cascade_index_count !== 0 ||
    markers.cascade_constraint_count !== 0 ||
    markers.continuation_rewritten ||
    markers.application_validator_rewritten
  );
}

function hasCompleteCascadeSchema(markers) {
  return (
    markers.cascade_column_count === 3 &&
    markers.cascade_preflights &&
    markers.cascade_adoptions &&
    markers.cascade_activations &&
    markers.cascade_jobs &&
    markers.cascade_receipts &&
    markers.cascade_function_count === 19 &&
    markers.cascade_trigger_count === 14 &&
    markers.cascade_index_count > 0 &&
    markers.cascade_constraint_count > 0 &&
    markers.continuation_rewritten &&
    markers.application_validator_rewritten
  );
}

async function cascadeAuthorityFingerprint(sql) {
  const [result] = await sql`
    WITH target_relations AS (
      SELECT relation.oid,relation.relname,relation.relpersistence,
             relation.relrowsecurity,relation.relforcerowsecurity,relation.relacl,
             relation.relowner
        FROM pg_catalog.pg_class AS relation
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public' AND relation.relkind='r'
         AND relation.relname=ANY(ARRAY[
           'helm_application_cascade_preflights',
           'helm_application_cascade_adoption_receipts',
           'helm_application_cascade_observer_activations',
           'helm_application_cascade_observation_jobs',
           'helm_application_cascade_receipts'
         ])
    ), lane_relations AS (
      SELECT relation.oid,relation.relname,relation.relpersistence,
             relation.relrowsecurity,relation.relforcerowsecurity,relation.relacl,
             relation.relowner
        FROM pg_catalog.pg_class AS relation
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public' AND relation.relkind='r'
         AND relation.relname=ANY(ARRAY[
           'helm_protected_payload_intents',
           'helm_protected_application_intents'
         ])
    ), authority_objects AS (
      SELECT 'relation:'||relation.relname AS sort_key,
             pg_catalog.jsonb_build_object(
               'kind','relation','name',relation.relname,
               'persistence',relation.relpersistence,'rls',relation.relrowsecurity,
               'forceRls',relation.relforcerowsecurity,
               'ownerIsMigrator',relation.relowner=(
                 SELECT role.oid FROM pg_catalog.pg_roles AS role
                  WHERE role.rolname=CURRENT_USER
               ),
               'acl',(
                 SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                   'grantee',CASE WHEN privilege.grantee=0 THEN 'public'
                                  WHEN privilege.grantee=relation.relowner THEN 'owner'
                                  ELSE 'other' END,
                   'grantor',CASE WHEN privilege.grantor=relation.relowner THEN 'owner' ELSE 'other' END,
                   'privilege',privilege.privilege_type,'grantable',privilege.is_grantable
                 ) ORDER BY privilege.grantee=relation.relowner DESC,
                            privilege.grantee=0 DESC,privilege.privilege_type,privilege.is_grantable),'[]'::jsonb)
                   FROM pg_catalog.aclexplode(COALESCE(
                     relation.relacl,pg_catalog.acldefault('r',relation.relowner)
                   )) AS privilege
               )
             ) AS authority
        FROM (
          SELECT * FROM target_relations
          UNION ALL
          SELECT * FROM lane_relations
        ) AS relation
      UNION ALL
      SELECT 'column:'||relation.relname||':'||pg_catalog.lpad(attribute.attnum::text,4,'0'),
             pg_catalog.jsonb_build_object(
               'kind','column','relation',relation.relname,'number',attribute.attnum,
               'name',attribute.attname,
               'type',pg_catalog.format_type(attribute.atttypid,attribute.atttypmod),
               'notNull',attribute.attnotnull,'identity',attribute.attidentity,
               'generated',attribute.attgenerated,
               'collation',COALESCE(collation_row.collname,''),
               'default',COALESCE(pg_catalog.pg_get_expr(attribute_default.adbin,attribute_default.adrelid),'')
             )
        FROM (
          SELECT * FROM target_relations
          UNION ALL
          SELECT * FROM lane_relations
        ) AS relation
        JOIN pg_catalog.pg_attribute AS attribute ON attribute.attrelid=relation.oid
        LEFT JOIN pg_catalog.pg_attrdef AS attribute_default
          ON attribute_default.adrelid=attribute.attrelid AND attribute_default.adnum=attribute.attnum
        LEFT JOIN pg_catalog.pg_collation AS collation_row ON collation_row.oid=attribute.attcollation
       WHERE attribute.attnum>0 AND NOT attribute.attisdropped
      UNION ALL
      SELECT 'constraint:'||relation.relname||':'||constraint_row.conname,
             pg_catalog.jsonb_build_object(
               'kind','constraint','relation',relation.relname,'name',constraint_row.conname,
               'type',constraint_row.contype,'deferrable',constraint_row.condeferrable,
               'deferred',constraint_row.condeferred,'validated',constraint_row.convalidated,
               'definition',pg_catalog.pg_get_constraintdef(constraint_row.oid,true)
             )
        FROM pg_catalog.pg_constraint AS constraint_row
        JOIN pg_catalog.pg_class AS relation ON relation.oid=constraint_row.conrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public' AND relation.oid IN (
         SELECT oid FROM target_relations
         UNION ALL
         SELECT oid FROM lane_relations
       )
      UNION ALL
      SELECT 'index:'||index_relation.relname,
             pg_catalog.jsonb_build_object(
               'kind','index','name',index_relation.relname,
               'definition',pg_catalog.pg_get_indexdef(index_relation.oid)
             )
        FROM pg_catalog.pg_index AS index_row
        JOIN pg_catalog.pg_class AS index_relation ON index_relation.oid=index_row.indexrelid
        JOIN pg_catalog.pg_class AS relation ON relation.oid=index_row.indrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public' AND relation.oid IN (
         SELECT oid FROM target_relations
         UNION ALL
         SELECT oid FROM lane_relations
       )
      UNION ALL
      SELECT 'function:'||proc.proname||':'||pg_catalog.pg_get_function_identity_arguments(proc.oid),
             pg_catalog.jsonb_build_object(
               'kind','function','name',proc.proname,
               'arguments',pg_catalog.pg_get_function_identity_arguments(proc.oid),
               'result',pg_catalog.pg_get_function_result(proc.oid),
               'language',language.lanname,'securityDefiner',proc.prosecdef,
               'volatility',proc.provolatile,'parallel',proc.proparallel,
               'strict',proc.proisstrict,
               'ownerIsMigrator',proc.proowner=(
                 SELECT role.oid FROM pg_catalog.pg_roles AS role
                  WHERE role.rolname=CURRENT_USER
               ),
               'config',COALESCE(proc.proconfig::text,''),
               'acl',(
                 SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                   'grantee',CASE WHEN privilege.grantee=0 THEN 'public'
                                  WHEN privilege.grantee=proc.proowner THEN 'owner'
                                  ELSE 'other' END,
                   'grantor',CASE WHEN privilege.grantor=proc.proowner THEN 'owner' ELSE 'other' END,
                   'privilege',privilege.privilege_type,'grantable',privilege.is_grantable
                 ) ORDER BY privilege.grantee=proc.proowner DESC,
                            privilege.grantee=0 DESC,privilege.privilege_type,privilege.is_grantable),'[]'::jsonb)
                   FROM pg_catalog.aclexplode(COALESCE(
                     proc.proacl,pg_catalog.acldefault('f',proc.proowner)
                   )) AS privilege
               ),
               'definition',pg_catalog.pg_get_functiondef(proc.oid)
             )
        FROM pg_catalog.pg_proc AS proc
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
        JOIN pg_catalog.pg_language AS language ON language.oid=proc.prolang
       WHERE namespace.nspname='public' AND proc.proname=ANY(ARRAY[
         'validate_helm_application_continuation_receipt',
         'validate_helm_protected_application_intent',
         'validate_helm_application_cascade_observer_activation',
         'helm_application_cascade_active_observer_is_exact',
         'activate_helm_application_cascade_observer',
         'helm_application_active_publisher_is_exact',
         'validate_helm_application_active_publisher_claim',
         'helm_application_cascade_preflight_is_fresh',
         'helm_application_cascade_expected_child_spec_digest',
         'helm_application_cascade_expected_root_spec_digest',
         'validate_helm_protected_cascade_lane',
         'validate_helm_application_cascade_gate',
         'validate_helm_application_cascade_exact_gate',
         'validate_helm_application_cascade_observation_job',
         'validate_helm_application_cascade_receipt',
         'validate_helm_application_cascade_preflight',
         'validate_helm_application_cascade_adoption_receipt',
         'validate_helm_application_cascade_adoption_postimage',
         'adopt_helm_application_cascade_preflight',
         'helm_application_cascade_observation_is_exact',
         'helm_application_cascade_is_exact'
       ])
      UNION ALL
      SELECT 'trigger:'||relation.relname||':'||trigger.tgname,
             pg_catalog.jsonb_build_object(
               'kind','trigger','relation',relation.relname,'name',trigger.tgname,
               'enabled',trigger.tgenabled,
               'definition',pg_catalog.pg_get_triggerdef(trigger.oid,true)
             )
        FROM pg_catalog.pg_trigger AS trigger
        JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public' AND NOT trigger.tgisinternal
         AND trigger.tgname=ANY(ARRAY[
           'helm_application_cascade_observer_activation_guard',
           'helm_protected_payload_active_publisher_claim',
           'helm_protected_application_active_publisher_claim',
           'helm_application_cascade_active_publisher_claim',
           'helm_protected_payload_cascade_lane_guard',
           'helm_protected_application_cascade_lane_guard',
           'helm_application_cascade_lane_guard',
           'helm_application_cascade_gate',
           'helm_application_cascade_exact_gate',
           'helm_application_cascade_observation_job_guard',
           'helm_application_cascade_receipt_guard',
           'helm_application_cascade_preflight_guard',
           'helm_application_cascade_adoption_receipt_guard',
           'helm_application_cascade_adoption_postimage'
         ])
    )
    SELECT COALESCE(pg_catalog.jsonb_agg(authority ORDER BY sort_key)::text,'[]') AS canonical
      FROM authority_objects
  `;
  return createHash("sha256").update(result.canonical).digest("hex");
}

async function inspectShim(sql) {
  const rows = await sql`
    WITH shim_functions AS (
      SELECT proc.*
        FROM pg_catalog.pg_proc AS proc
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
       WHERE namespace.nspname='public' AND proc.proname=${shimFunction}
    ), exact_proc AS (
      SELECT * FROM shim_functions WHERE pronargs=0 AND proargtypes=''::pg_catalog.oidvector
    ), shim_triggers AS (
      SELECT trigger.*
        FROM pg_catalog.pg_trigger AS trigger
        JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public'
         AND trigger.tgname=${shimTrigger}
    ), exact_trigger AS (
      SELECT trigger.*
        FROM shim_triggers AS trigger
        JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
       WHERE namespace.nspname='public'
         AND relation.relname='helm_protected_application_intents'
    )
    SELECT
      (SELECT count(*)::integer FROM shim_functions) AS function_count,
      (SELECT count(*)::integer FROM shim_triggers) AS trigger_count,
      proc.oid IS NOT NULL AS function_exists,
      trigger.oid IS NOT NULL AS trigger_exists,
      proc.prosrc,
      proc.prosecdef,
      proc.provolatile,
      proc.prokind,
      proc.pronargs,
      proc.prorettype=pg_catalog.to_regtype('pg_catalog.trigger') AS returns_trigger,
      proc.proconfig,
      lang.lanname,
      trigger.tgenabled,
      trigger.tgisinternal,
      trigger.tgtype,
      trigger.tgnargs,
      trigger.tgqual IS NULL AS no_when_clause,
      trigger.tgfoid=proc.oid AS exact_function
    FROM (SELECT 1) AS singleton
    LEFT JOIN exact_proc AS proc ON true
    LEFT JOIN pg_catalog.pg_language AS lang ON lang.oid=proc.prolang
    LEFT JOIN exact_trigger AS trigger ON true
  `;
  if (rows.length !== 1) {
    throw new Error("Refusing RC171 recovery with an ambiguous migration shim");
  }
  return rows[0];
}

function shimIsExact(shim) {
  return (
    shim.function_count === 1 &&
    shim.trigger_count === 1 &&
    shim.function_exists &&
    shim.trigger_exists &&
    shim.prosrc === shimFunctionBody &&
    shim.prosecdef === false &&
    shim.provolatile === "v" &&
    shim.prokind === "f" &&
    shim.pronargs === 0 &&
    shim.returns_trigger === true &&
    shim.lanname === "plpgsql" &&
    Array.isArray(shim.proconfig) &&
    shim.proconfig.length === 1 &&
    shim.proconfig[0] === "search_path=pg_catalog, pg_temp" &&
    shim.tgenabled === "O" &&
    shim.tgisinternal === false &&
    shim.tgtype === 19 &&
    shim.tgnargs === 0 &&
    shim.no_when_clause === true &&
    shim.exact_function === true
  );
}

async function validateOrInstallShim(sql) {
  const before = await inspectShim(sql);
  if (before.function_count !== 0 || before.trigger_count !== 0) {
    if (!shimIsExact(before)) {
      throw new Error("Refusing RC171 recovery with an altered or partial migration shim");
    }
    return;
  }

  await sql.begin(async (transaction) => {
    await transaction`
      LOCK TABLE public.helm_protected_application_intents IN ACCESS EXCLUSIVE MODE
    `;
    const locked = await inspectShim(transaction);
    if (locked.function_count !== 0 || locked.trigger_count !== 0) {
      if (!shimIsExact(locked)) {
        throw new Error("Refusing RC171 recovery with an altered or partial migration shim");
      }
      return;
    }
    await transaction.unsafe(`
      CREATE FUNCTION public.${shimFunction}() RETURNS trigger
      LANGUAGE plpgsql
      SET search_path=pg_catalog,pg_temp
      AS $shim$${shimFunctionBody}$shim$;
      CREATE TRIGGER ${shimTrigger}
      BEFORE UPDATE ON public.helm_protected_application_intents
      FOR EACH ROW EXECUTE FUNCTION public.${shimFunction}();
    `);
    const installed = await inspectShim(transaction);
    if (!shimIsExact(installed)) {
      throw new Error("RC171 migration shim did not match its closed contract");
    }
  });
  console.log("Installed the bounded RC171 terminal-row migration shim");
}

async function prepareRC171Recovery(sql) {
  const [history] = await sql`
    SELECT pg_catalog.to_regclass('public._prisma_migrations')::text AS history_table
  `;
  if (!history.history_table) {
    return;
  }

  const allHistory = await sql`
    SELECT id::text,checksum,migration_name,logs,applied_steps_count,
           started_at,finished_at,rolled_back_at
      FROM public._prisma_migrations
     ORDER BY started_at,id
  `;
  const expectedAll = await expectedMigrationChecksums();
  const expectedByName = new Map(
    expectedAll.map((migration) => [migration.name, migration.checksum]),
  );
  for (const row of allHistory) {
    if (
      expectedByName.get(row.migration_name) !== row.checksum ||
      (row.finished_at !== null && row.rolled_back_at !== null) ||
      (row.rolled_back_at !== null && row.rolled_back_at < row.started_at)
    ) {
      throw new Error("Refusing RC171 recovery from an unexpected migration history row");
    }
    if (row.finished_at !== null) {
      const crashAttested011 =
        row.migration_name === failedRC171Migration &&
        row.applied_steps_count === 0;
      if (
        (row.applied_steps_count !== 1 && !crashAttested011) ||
        row.rolled_back_at !== null
      ) {
        throw new Error("Refusing RC171 recovery from a malformed successful migration");
      }
      continue;
    }
    if (!isRecoverableFailure(row)) {
      throw new Error("Refusing automatic recovery of an unrecognized failed migration");
    }
  }

  const activeFailures = allHistory.filter(
    (row) => row.finished_at === null && row.rolled_back_at === null,
  );
  if (activeFailures.some((row) => !isRecoverableFailure(row))) {
    throw new Error("Refusing automatic recovery of an unrecognized failed migration");
  }
  if (activeFailures.length > 1) {
    throw new Error("Refusing automatic recovery with multiple active failed migrations");
  }

  const successful = allHistory.filter(
    (row) => row.finished_at !== null && row.rolled_back_at === null,
  );
  const successfulNames = new Set(successful.map((row) => row.migration_name));
  if (
    successful.length > expectedAll.length ||
    expectedAll.some(
      (migration, index) =>
        index < successful.length
          ? !successfulNames.has(migration.name)
          : successfulNames.has(migration.name),
    )
  ) {
    throw new Error("Refusing RC171 recovery from a noncanonical migration prefix");
  }
  const successful011 = successful.filter(
    (row) => row.migration_name === failedRC171Migration,
  );
  const failed011Rows = allHistory.filter(
    (row) =>
      row.migration_name === failedRC171Migration && row.finished_at === null,
  );
  const exactFailed011Rows = failed011Rows.filter(isExactFailure);
  const interrupted011Rows = failed011Rows.filter(isInterruptedMigration);
  if (
    failed011Rows.length > 2 ||
    exactFailed011Rows.length > 1 ||
    interrupted011Rows.length > 1
  ) {
    throw new Error("Refusing RC171 recovery with duplicate failed migration 011 attempts");
  }
  if (
    successful011.some((row) => row.checksum !== failedRC171Checksum) ||
    successful011.length > 1
  ) {
    throw new Error("Refusing RC171 recovery from an unexpected migration 011 history");
  }
  const expectedBase = expectedAll.filter(
    (migration) => migration.name < failedRC171Migration,
  );
  const successfulBase = successful.filter(
    (row) => row.migration_name < failedRC171Migration,
  );
  if (
    successfulBase.length > expectedBase.length ||
    successfulBase.some((row) => expectedByName.get(row.migration_name) !== row.checksum)
  ) {
    throw new Error("Refusing RC171 recovery from an unexpected migration history");
  }

  const baseComplete = successfulBase.length === expectedBase.length;
  const interrupted012Rows = allHistory.filter(
    (row) =>
      row.migration_name === "012_recover_rc171_cascade_preflight" &&
      row.finished_at === null,
  );
  const successful012Rows = successful.filter(
    (row) => row.migration_name === "012_recover_rc171_cascade_preflight",
  );
  if (
    (failed011Rows.length !== 0 && !baseComplete) ||
    interrupted012Rows.length > 1 ||
    (interrupted012Rows.length !== 0 && successful011.length !== 1) ||
    (exactFailed011Rows.length === 1 &&
      interrupted011Rows.length === 1 &&
      (exactFailed011Rows[0].rolled_back_at === null ||
        exactFailed011Rows[0].started_at > interrupted011Rows[0].started_at ||
        exactFailed011Rows[0].rolled_back_at > interrupted011Rows[0].started_at)) ||
    (interrupted012Rows.length === 1 &&
      successful011.length === 1 &&
      interrupted012Rows[0].started_at < successful011[0].finished_at) ||
    (interrupted012Rows.length === 1 &&
      successful012Rows.length === 1 &&
      (interrupted012Rows[0].rolled_back_at === null ||
        interrupted012Rows[0].rolled_back_at > successful012Rows[0].started_at))
  ) {
    throw new Error("Refusing RC171 recovery from incoherent failure chronology");
  }
  if (
    successful011.some((row) => row.applied_steps_count === 0) &&
    interrupted011Rows.length !== 1
  ) {
    throw new Error("Refusing an unattested crash-applied migration 011 history row");
  }

  if (successful011.length === 1) {
    const shim = await inspectShim(sql);
    const successful012 = successful.some(
      (row) => row.migration_name === "012_recover_rc171_cascade_preflight",
    );
    if (successful012) {
      if (shim.function_count !== 0 || shim.trigger_count !== 0) {
        throw new Error("Refusing a completed recovery with a residual RC171 shim");
      }
    } else {
      const markers = await cascadeMarkers(sql);
      const shimAbsent = shim.function_count === 0 && shim.trigger_count === 0;
      const interruptedCleanup = interrupted012Rows[0];
      const cleanupShimIsExact =
        interruptedCleanup === undefined
          ? shimIsExact(shim) || shimAbsent
          : interruptedCleanup.applied_steps_count === 0
            ? shimIsExact(shim)
            : shimAbsent;
      if (
        !cleanupShimIsExact ||
        !hasCompleteCascadeSchema(markers) ||
        (await cascadeAuthorityFingerprint(sql)) !== cascadeAuthoritySHA256
      ) {
        throw new Error("Refusing RC171 recovery with an inexact migration 011 authority postimage");
      }
    }
    if (activeFailures.length === 1) {
      const [failure] = activeFailures;
      const markers = await cascadeMarkers(sql);
      if (
        failure.migration_name !== "012_recover_rc171_cascade_preflight" ||
        !isInterruptedMigration(failure) ||
        !hasCompleteCascadeSchema(markers) ||
        (failure.applied_steps_count === 0
          ? !shimIsExact(shim)
          : shim.function_count !== 0 || shim.trigger_count !== 0)
      ) {
        throw new Error("Refusing RC171 recovery with an unsafe interrupted migration 012");
      }
      await runPrisma([
        "migrate",
        "resolve",
        "--rolled-back",
        "012_recover_rc171_cascade_preflight",
        "--config",
        "prisma.config.ts",
      ]);
      const resolved = await sql`
        SELECT checksum,finished_at,rolled_back_at,applied_steps_count
          FROM public._prisma_migrations
         WHERE id=${failure.id}
      `;
      if (
        resolved.length !== 1 ||
        resolved[0].checksum !== failure.checksum ||
        resolved[0].finished_at !== null ||
        resolved[0].rolled_back_at === null ||
        resolved[0].applied_steps_count !== failure.applied_steps_count
      ) {
        throw new Error("Prisma did not preserve interrupted migration 012 as rolled back");
      }
      console.log("Resolved the exact interrupted migration 012 as rolled back");
    }
    return;
  }

  const protectedTable = await sql`
    SELECT pg_catalog.to_regclass('public.helm_protected_application_intents') IS NOT NULL
      AS exists
  `;
  const markers = await cascadeMarkers(sql);
  const shim = await inspectShim(sql);
  const postcommitInterrupted011 =
    activeFailures.length === 1 &&
    activeFailures[0].migration_name === failedRC171Migration &&
    isInterruptedMigration(activeFailures[0]) &&
    baseComplete &&
    shimIsExact(shim) &&
    hasCompleteCascadeSchema(markers) &&
    (await cascadeAuthorityFingerprint(sql)) === cascadeAuthoritySHA256;
  if (postcommitInterrupted011) {
    const [failure] = activeFailures;
    await runPrisma([
      "migrate",
      "resolve",
      "--applied",
      failedRC171Migration,
      "--config",
      "prisma.config.ts",
    ]);
    const attempts = await sql`
      SELECT id::text,checksum,finished_at,rolled_back_at,applied_steps_count,
             started_at
        FROM public._prisma_migrations
       WHERE migration_name=${failedRC171Migration}
       ORDER BY started_at,id
    `;
    const interrupted = attempts.filter((row) => row.id === failure.id);
    const applied = attempts.filter(
      (row) =>
        row.id !== failure.id &&
        row.checksum === failedRC171Checksum &&
        row.finished_at !== null &&
        row.rolled_back_at === null &&
        row.applied_steps_count === 0,
    );
    if (
      interrupted.length !== 1 ||
      interrupted[0].checksum !== failedRC171Checksum ||
      interrupted[0].finished_at !== null ||
      interrupted[0].rolled_back_at === null ||
      interrupted[0].applied_steps_count !== 0 ||
      applied.length !== 1 ||
      applied[0].started_at < interrupted[0].rolled_back_at ||
      attempts.some(
        (row) => row.finished_at === null && row.rolled_back_at === null,
      )
    ) {
      throw new Error("Prisma did not preserve the crash-attested migration 011 history");
    }
    console.log("Attested and recorded the fully committed migration 011 postimage");
    return;
  }
  if (hasAnyCascadeMarker(markers)) {
    throw new Error("Refusing RC171 recovery because migration 011 left schema effects");
  }
  if (!protectedTable[0].exists) {
    if (activeFailures.length !== 0) {
      throw new Error("Refusing RC171 recovery because the protected table is missing");
    }
    return;
  }
  if (successfulBase.length === 0) {
    throw new Error("Refusing RC171 recovery without the canonical base history");
  }

  if (
    (shim.function_count !== 0 || shim.trigger_count !== 0) &&
    !shimIsExact(shim)
  ) {
    throw new Error("Refusing RC171 recovery with an altered or partial migration shim");
  }

  if (activeFailures.length === 1) {
    const [failure] = activeFailures;
    await runPrisma([
      "migrate",
      "resolve",
      "--rolled-back",
      failedRC171Migration,
      "--config",
      "prisma.config.ts",
    ]);
    const resolved = await sql`
      SELECT checksum,finished_at,rolled_back_at,applied_steps_count
        FROM public._prisma_migrations
       WHERE id=${failure.id}
    `;
    if (
      resolved.length !== 1 ||
      resolved[0].checksum !== failedRC171Checksum ||
      resolved[0].finished_at !== null ||
      resolved[0].rolled_back_at === null ||
      resolved[0].applied_steps_count !== 0
    ) {
      throw new Error("Prisma did not preserve the failed RC171 migration as rolled back");
    }
    console.log("Resolved the exact failed RC171 migration as rolled back");
  }

  await validateOrInstallShim(sql);
}

const sql = postgres(runnerEndpoint.toString(), {
  max: 1,
  prepare: false,
  connect_timeout: Math.min(waitSeconds, 30),
});
try {
  await sql`
    SELECT pg_catalog.pg_advisory_lock(
      pg_catalog.hashtextextended(${migrationRunnerLock},0)
    )
  `;
  await prepareRC171Recovery(sql);
  await runPrisma(["migrate", "deploy", "--config", "prisma.config.ts"]);
  await runSchemaDriftCheck();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
} finally {
  try {
    await sql`
      SELECT pg_catalog.pg_advisory_unlock(
        pg_catalog.hashtextextended(${migrationRunnerLock},0)
      )
    `;
  } catch {
    // Connection loss releases the session-scoped advisory lock.
  }
  await sql.end({ timeout: 5 });
}
