import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, extname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const sourceRoot = join(webRoot, "src");
const violations = [];
const forbiddenPhrases = [
  "Application identity",
  "New application",
  "Existing application",
  "Application name",
  "Create application identity",
  "Select application",
  "Application source",
  "Loading application",
  "Application sections",
  "Application policy",
  "Application environment",
  "Read applications",
  "Edit applications",
  "application workloads",
  "application series",
  "application scope",
  "application-wide",
  "application-scoped",
  "Application values",
  "applications through",
];

function scan(directory) {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      scan(path);
      continue;
    }
    if (![".ts", ".tsx"].includes(extname(path)) || /\.test\.[^.]+$/.test(path))
      continue;
    const source = readFileSync(path, "utf8");
    const normalized = source.toLowerCase();
    for (const phrase of forbiddenPhrases) {
      let offset = normalized.indexOf(phrase.toLowerCase());
      while (offset !== -1) {
        const line = source.slice(0, offset).split("\n").length;
        violations.push(`${relative(webRoot, path)}:${line}: ${phrase}`);
        offset = normalized.indexOf(
          phrase.toLowerCase(),
          offset + phrase.length,
        );
      }
    }
  }
}

scan(sourceRoot);
assert.deepEqual(
  violations,
  [],
  `User-facing copy must say App, not Application:\n${violations.join("\n")}`,
);

console.log("Verified user-facing App terminology");
