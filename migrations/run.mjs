import net from "node:net";
import { spawn } from "node:child_process";

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

const child = spawn(
  "./node_modules/.bin/prisma",
  ["migrate", "deploy", "--config", "prisma.config.ts"],
  { stdio: "inherit", env: process.env },
);
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => child.kill(signal));
}
child.once("error", (error) => {
  console.error(`Unable to start Prisma Migrate: ${error.message}`);
  process.exit(1);
});
child.once("exit", (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
