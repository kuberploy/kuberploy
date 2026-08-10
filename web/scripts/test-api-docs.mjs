import { createHash } from "node:crypto";
import { lstat, mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  buildApiDocs,
  SWAGGER_UI_ASSETS,
  SWAGGER_UI_DIST_VERSION,
} from "./build-api-docs.mjs";

const scriptPath = fileURLToPath(import.meta.url);
const projectRoot = resolve(dirname(scriptPath), "..");

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

async function verifyApiDocs(directory) {
  const expectedFiles = [
    ...SWAGGER_UI_ASSETS,
    "docs-manifest.json",
    "index.html",
  ].sort();
  const actualFiles = (await readdir(directory)).sort();
  assert(
    JSON.stringify(actualFiles) === JSON.stringify(expectedFiles),
    `Unexpected docs files: ${actualFiles.join(", ")}`,
  );

  for (const name of actualFiles) {
    const details = await lstat(join(directory, name));
    assert(
      details.isFile() && !details.isSymbolicLink(),
      `${name} is not a file.`,
    );
  }

  const manifest = JSON.parse(
    await readFile(join(directory, "docs-manifest.json"), "utf8"),
  );
  assert(manifest.schemaVersion === "1", "Unexpected docs manifest schema.");
  assert(
    manifest.swaggerUiDistVersion === SWAGGER_UI_DIST_VERSION,
    `Swagger UI is not pinned to ${SWAGGER_UI_DIST_VERSION}.`,
  );
  assert(
    manifest.files.length === expectedFiles.length - 1,
    "Docs manifest file count is incomplete.",
  );
  for (const file of manifest.files) {
    const bytes = await readFile(join(directory, file.path));
    assert(sha256(bytes) === file.sha256, `Digest mismatch for ${file.path}.`);
  }

  const index = await readFile(join(directory, "index.html"), "utf8");
  for (const reference of [
    "./swagger-ui.css",
    "./swagger-ui-bundle.js",
    "./swagger-ui-standalone-preset.js",
  ]) {
    assert(index.includes(reference), `Missing local asset ${reference}.`);
  }
  assert(index.includes('url: "/openapi.yaml"'), "OpenAPI URL is not local.");
  assert(
    index.includes('href="/openapi-agent.json"'),
    "Agent profile link is missing.",
  );
  assert(
    index.includes('href="/arazzo.yaml"'),
    "Arazzo workflow link is missing.",
  );
  assert(index.includes("validatorUrl: null"), "Remote validator is enabled.");
  assert(
    index.includes("queryConfigEnabled: false"),
    "Query-string config overrides are enabled.",
  );
  assert(
    !/(?:src|href)=["'](?:https?:)?\/\//i.test(index),
    "Docs index references a remote asset.",
  );
  assert(
    !/(?:unpkg|jsdelivr|cdnjs|validator\.swagger\.io)/i.test(index),
    "Docs index contains a runtime CDN or validator dependency.",
  );

  const nginx = await readFile(
    join(projectRoot, "nginx.conf.template"),
    "utf8",
  );
  assert(
    nginx.includes("absolute_redirect off;"),
    "/docs redirects must remain relative so forwarded host ports are preserved.",
  );
  const proxyResources = nginx.match(
    /location ~ \^\/\(([^)]+)\)\(\/\|\$\)/,
  )?.[1];
  assert(proxyResources, "Could not find the API proxy location.");
  assert(
    proxyResources.includes("openapi\\.yaml"),
    "/openapi.yaml is not proxied to the API.",
  );
  assert(
    proxyResources.includes("openapi-agent\\.json"),
    "/openapi-agent.json is not proxied to the API.",
  );
  assert(
    proxyResources.includes("arazzo\\.yaml"),
    "/arazzo.yaml is not proxied to the API.",
  );
  assert(
    !proxyResources.split("|").includes("docs"),
    "/docs is still proxied to the API.",
  );
  assert(
    nginx.includes("location /docs/"),
    "Static /docs location is missing.",
  );
}

const builtArgument = process.argv.indexOf("--built");
const requestedDirectory =
  builtArgument >= 0 && process.argv[builtArgument + 1]
    ? resolve(process.cwd(), process.argv[builtArgument + 1])
    : undefined;
let temporaryDirectory;

try {
  let docsDirectory = requestedDirectory;
  if (!docsDirectory) {
    temporaryDirectory = await mkdtemp(join(tmpdir(), "kuberploy-api-docs-"));
    const firstBuild = join(temporaryDirectory, "first");
    const secondBuild = join(temporaryDirectory, "second");
    await buildApiDocs(firstBuild);
    await buildApiDocs(secondBuild);
    const firstFiles = (await readdir(firstBuild)).sort();
    const secondFiles = (await readdir(secondBuild)).sort();
    assert(
      JSON.stringify(firstFiles) === JSON.stringify(secondFiles),
      "Repeated docs builds produced different file sets.",
    );
    for (const name of firstFiles) {
      const firstBytes = await readFile(join(firstBuild, name));
      const secondBytes = await readFile(join(secondBuild, name));
      assert(
        firstBytes.equals(secondBytes),
        `Repeated docs builds differ at ${name}.`,
      );
    }
    docsDirectory = firstBuild;
  }
  await verifyApiDocs(docsDirectory);
  process.stdout.write(
    `Verified self-hosted Swagger UI ${SWAGGER_UI_DIST_VERSION} in ${docsDirectory}\n`,
  );
} finally {
  if (temporaryDirectory) {
    await rm(temporaryDirectory, { recursive: true, force: true });
  }
}
