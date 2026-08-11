import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

const schemaPath = new URL("./prisma/schema.prisma", import.meta.url);

function canonicalSchema(value) {
  const lines = value.replaceAll("\r\n", "\n").split("\n");
  while (lines.length > 0 && (lines[0].startsWith("//") || lines[0] === "")) {
    lines.shift();
  }
  return lines.join("\n").trim();
}

const expected = canonicalSchema(readFileSync(schemaPath, "utf8"));
const observed = canonicalSchema(
  execFileSync(
    "./node_modules/.bin/prisma",
    ["db", "pull", "--print", "--config", "prisma.config.ts"],
    { encoding: "utf8", maxBuffer: 16 * 1024 * 1024, stdio: ["ignore", "pipe", "ignore"] },
  ),
);

if (expected !== observed) {
  throw new Error(
    "prisma/schema.prisma differs from the migrated PostgreSQL schema; run npm run pull against a disposable fully migrated database and review the diff",
  );
}

process.stdout.write("Prisma declarative schema matches the migrated PostgreSQL database.\n");
