import net from "node:net";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import postgres from "postgres";

const migrationRunnerLock = "kuberploy-prisma-migration-runner-v1";
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
    const child = spawn(command, args, { stdio: "inherit", env: process.env });
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
        terminateSelf(signal);
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

async function verifyExactHistory(sql) {
  const expected = await expectedMigrationChecksums();
  const rows = await sql`
    SELECT migration_name,checksum,finished_at,rolled_back_at,applied_steps_count
      FROM public._prisma_migrations
     ORDER BY migration_name,started_at,id
  `;
  if (rows.length !== expected.length) {
    throw new Error(
      `Database migration history has ${rows.length} row(s); release requires ${expected.length}`,
    );
  }
  for (let index = 0; index < expected.length; index += 1) {
    const row = rows[index];
    const migration = expected[index];
    if (
      row.migration_name !== migration.name ||
      row.checksum !== migration.checksum ||
      row.finished_at === null ||
      row.rolled_back_at !== null ||
      row.applied_steps_count !== 1
    ) {
      throw new Error("Database migration history does not match this Kuberploy release");
    }
  }
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
  await runChild(
    "./node_modules/.bin/prisma",
    ["migrate", "deploy", "--config", "prisma.config.ts"],
    "Prisma Migrate",
  );
  await verifyExactHistory(sql);
  await runChild(process.execPath, ["check-schema-drift.mjs"], "schema drift check");
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
