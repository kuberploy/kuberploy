import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, extname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const sourceRoot = join(webRoot, "src");
const nativeSelects = [];
const labelWrappedSelects = [];

function scan(directory) {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      scan(path);
      continue;
    }
    if (extname(path) !== ".tsx") continue;
    const source = readFileSync(path, "utf8");
    if (/<\/?select(?:\s|>)/.test(source)) {
      nativeSelects.push(relative(webRoot, path));
    }
    if (/<label\b(?:(?!<\/label>)[\s\S])*?<Select\b/.test(source)) {
      labelWrappedSelects.push(relative(webRoot, path));
    }
  }
}

scan(sourceRoot);
assert.deepEqual(
  nativeSelects,
  [],
  `Visible native <select> controls are forbidden; use shared Select: ${nativeSelects.join(", ")}`,
);
assert.deepEqual(
  labelWrappedSelects,
  [],
  `Custom Select cannot be nested in <label>; use Field or htmlFor: ${labelWrappedSelects.join(", ")}`,
);

console.log("Verified all visible selects use shared styled Select");
