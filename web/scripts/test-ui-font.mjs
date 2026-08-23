import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const entrypoint = readFileSync(join(webRoot, "src/main.tsx"), "utf8");
const styles = readFileSync(join(webRoot, "src/styles.css"), "utf8");
const fontStyles = readFileSync(
  join(webRoot, "node_modules/@fontsource-variable/noto-sans/index.css"),
  "utf8",
);
const packageJson = JSON.parse(
  readFileSync(join(webRoot, "package.json"), "utf8"),
);

assert.equal(
  packageJson.dependencies["@fontsource-variable/noto-sans"],
  "5.3.0",
);
assert.match(
  entrypoint,
  /import "@fontsource-variable\/noto-sans\/index\.css";/,
);
assert.doesNotMatch(entrypoint, /@fontsource-variable\/roboto/);
assert.match(
  styles,
  /--font-sans: "Noto Sans Variable", "Noto Sans", sans-serif;/,
);
assert.doesNotMatch(styles, /"Roboto Variable"/);
assert.match(fontStyles, /font-family: 'Noto Sans Variable';/);
assert.match(fontStyles, /font-style: normal;/);
assert.match(fontStyles, /font-weight: 100 900;/);

console.log("Verified self-hosted Noto Sans Variable normal 100-900 UI font");
