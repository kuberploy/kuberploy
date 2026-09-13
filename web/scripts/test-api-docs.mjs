import { createHash } from "node:crypto";
import { lstat, mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { runInNewContext } from "node:vm";
import { JSDOM, VirtualConsole } from "jsdom";
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

async function verifySwaggerChoices(directory, config) {
  const errors = [];
  const virtualConsole = new VirtualConsole();
  virtualConsole.on("jsdomError", (error) => errors.push(error.message));
  virtualConsole.on("error", (...args) =>
    errors.push(args.map(String).join(" ")),
  );
  const dom = new JSDOM('<div id="swagger-ui"></div>', {
    url: "https://docs.example.test/docs/",
    runScripts: "dangerously",
    pretendToBeVisual: true,
    virtualConsole,
  });
  try {
    dom.window.CSS.escape = dom.window.CSS.escape.bind(dom.window.CSS);
    dom.window.matchMedia = () => ({
      matches: false,
      addEventListener() {},
      removeEventListener() {},
    });
    dom.window.eval(
      await readFile(join(directory, "swagger-ui-bundle.js"), "utf8"),
    );
    dom.window.eval(
      await readFile(
        join(directory, "swagger-ui-standalone-preset.js"),
        "utf8",
      ),
    );
    const bundle = dom.window.SwaggerUIBundle;
    const spec = {
      openapi: "3.0.3",
      info: { title: "Choice regression", version: "1" },
      servers: [{ url: "/" }],
      paths: {
        "/meta": {
          get: {
            parameters: [
              {
                name: "mode",
                in: "query",
                schema: { type: "string", enum: ["first", "second"] },
              },
              {
                name: "flavors",
                in: "query",
                schema: {
                  type: "array",
                  items: { type: "string", enum: ["vanilla", "chocolate"] },
                },
              },
            ],
            responses: {
              200: {
                description: "Choices",
                content: {
                  "application/json": {
                    schema: { type: "string" },
                    examples: {
                      first: { summary: "First example", value: "first" },
                      second: { summary: "Second example", value: "second" },
                    },
                  },
                  "application/problem+json": { schema: { type: "object" } },
                },
              },
              400: {
                description: "Only media type",
                content: {
                  "application/problem+json": { schema: { type: "object" } },
                },
              },
            },
          },
        },
      },
    };
    bundle({
      ...config,
      spec,
      url: undefined,
      docExpansion: "full",
      defaultModelsExpandDepth: -1,
      presets: [bundle.presets.apis, dom.window.SwaggerUIStandalonePreset],
    });
    const document = dom.window.document;
    async function waitFor(check, message) {
      const until = Date.now() + 5000;
      while (!check() && Date.now() < until)
        await new Promise((resolve) => setTimeout(resolve, 20));
      assert(check(), `${message} ${errors.join("; ")}`);
    }
    await waitFor(
      () => document.querySelector(".opblock"),
      "Swagger did not render the fixture operation.",
    );
    assert(
      !document.querySelector("select"),
      "Swagger rendered a native selector.",
    );
    assert(
      document.querySelector("[data-installation-server]"),
      "Installation server label disappeared.",
    );
    await waitFor(
      () => document.querySelector('[role="listbox"][aria-label="Media Type"]'),
      "Media controls did not render.",
    );
    const media = document.querySelector(
      '[role="listbox"][aria-label="Media Type"]',
    );
    assert(media, "Multiple media types have no accessible choice control.");
    const mediaOptions = media.querySelectorAll('[role="option"]');
    const examples = document.querySelector(
      '.examples-select [role="listbox"]',
    );
    assert(examples, "Named examples have no accessible choice control.");
    examples.querySelectorAll('[role="option"]')[1].click();
    await waitFor(
      () =>
        examples
          .querySelectorAll('[role="option"]')[1]
          .getAttribute("aria-selected") === "true",
      "Named example selection did not update Swagger.",
    );
    mediaOptions[1].click();
    await waitFor(
      () => mediaOptions[1].getAttribute("aria-selected") === "true",
      "Media selection did not update Swagger.",
    );
    assert(
      document.querySelector(".docs-choice-single"),
      "A single media type should remain readable.",
    );
    const mode = document.querySelector(
      '[data-param-name="mode"] [role="listbox"]',
    );
    assert(
      mode?.getAttribute("aria-disabled") === "true",
      "Enum editing should be disabled before Try it out.",
    );
    mode.querySelectorAll('[role="option"]')[2].click();
    assert(
      mode
        .querySelectorAll('[role="option"]')[2]
        .getAttribute("aria-selected") === "false",
      "Disabled enum changed value.",
    );
    document.querySelector(".try-out__btn").click();
    await waitFor(
      () => mode.getAttribute("aria-disabled") === "false",
      "Try it out did not enable enum editing.",
    );
    mode.focus();
    mode.dispatchEvent(
      new dom.window.KeyboardEvent("keydown", { key: "End", bubbles: true }),
    );
    await waitFor(
      () =>
        mode
          .querySelectorAll('[role="option"]')[2]
          .getAttribute("aria-selected") === "true",
      "Keyboard enum selection did not update Swagger.",
    );
    mode.dispatchEvent(
      new dom.window.KeyboardEvent("keydown", { key: "Home", bubbles: true }),
    );
    await waitFor(
      () =>
        mode
          .querySelectorAll('[role="option"]')[0]
          .getAttribute("aria-selected") === "true",
      "Optional enum could not be cleared.",
    );
    const multiple = document.querySelector(
      '[data-param-name="flavors"] [role="listbox"]',
    );
    assert(
      multiple?.getAttribute("aria-multiselectable") === "true",
      "Array enum lost multiselect semantics.",
    );
    const multipleOptions = multiple.querySelectorAll('[role="option"]');
    multipleOptions[1].click();
    await waitFor(
      () => multipleOptions[1].getAttribute("aria-selected") === "true",
      "First array value was not selected.",
    );
    multipleOptions[2].click();
    await waitFor(
      () =>
        multipleOptions[1].getAttribute("aria-selected") === "true" &&
        multipleOptions[2].getAttribute("aria-selected") === "true",
      "Array selection discarded an existing value.",
    );
    multiple.dispatchEvent(
      new dom.window.KeyboardEvent("keydown", { key: " ", bubbles: true }),
    );
    await waitFor(
      () =>
        multipleOptions[1].getAttribute("aria-selected") === "true" &&
        multipleOptions[2].getAttribute("aria-selected") === "false",
      "Keyboard did not toggle the active multiselect value.",
    );
    assert(
      !document.querySelector("select"),
      "Try it out introduced a native selector.",
    );
    assert(errors.length === 0, `Swagger choice errors: ${errors.join("; ")}`);
  } finally {
    dom.window.close();
  }
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
  for (const name of [
    "swagger-ui.css",
    "swagger-ui-bundle.js",
    "swagger-ui-standalone-preset.js",
  ]) {
    const bytes = await readFile(join(directory, name));
    const reference = `./${name}?v=${sha256(bytes).slice(0, 16)}`;
    assert(
      index.includes(reference),
      `Missing versioned local asset ${reference}.`,
    );
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
  assert(
    index.includes("API documentation could not load"),
    "Docs runtime error state is missing.",
  );
  assert(
    index.includes('typeof window.SwaggerUIBundle !== "function"') &&
      index.includes("!Array.isArray(window.SwaggerUIStandalonePreset)"),
    "Docs runtime asset guards are missing.",
  );

  let config;
  const window = {
    SwaggerUIBundle: Object.assign(
      (options) => {
        config = options;
      },
      { presets: { apis: [] } },
    ),
    SwaggerUIStandalonePreset: [],
    addEventListener: (_event, callback) => callback(),
  };
  const inlineScript = index.match(/<script>\s*([\s\S]*?)<\/script>/)?.[1];
  assert(inlineScript, "Docs initializer is missing.");
  runInNewContext(inlineScript, {
    window,
    document: { getElementById: () => ({}) },
    console,
  });
  const selected = [];
  const plugin = config.plugins[0]({
    React: {
      useEffect: (effect) => effect(),
      createElement: (type, props, ...children) => ({ type, props, children }),
    },
  });
  const Original = () => {};
  const Servers = plugin.wrapComponents.Servers(Original);
  const props = {
    servers: { size: 1, first: () => new Map([["url", "/"]]) },
    currentServer: "",
    setSelectedServer: (value) => selected.push(value),
  };
  const rendered = Servers(props);
  assert(
    rendered.type === "div" && rendered.props["data-installation-server"],
    "The only installation server should be a readable label.",
  );
  assert(
    selected.length === 1 && selected[0] === "/",
    "The relative installation server was not selected for API requests.",
  );
  assert(
    Servers({ ...props, servers: { ...props.servers, size: 2 } }).type ===
      Original,
    "Alternate server contracts lost their normal selection behavior.",
  );
  await verifySwaggerChoices(directory, config);

  const nginx = await readFile(
    join(projectRoot, "nginx.conf.template"),
    "utf8",
  );
  assert(
    nginx.includes("absolute_redirect off;"),
    "/docs redirects must remain relative so forwarded host ports are preserved.",
  );
  const robotsHeader = 'add_header X-Robots-Tag "noindex, nofollow" always;';
  assert(
    nginx.includes(robotsHeader),
    "The production web server must disable search indexing by default.",
  );
  for (const location of [
    "location = /healthz",
    "location = /docs/index.html",
    "location /docs/",
    "location /assets/",
    "location / {",
  ]) {
    const start = nginx.indexOf(location);
    const end = nginx.indexOf("\n  }", start);
    assert(start >= 0 && end > start, `Could not find ${location}.`);
    assert(
      nginx.slice(start, end).includes(robotsHeader),
      `${location} overrides response headers without preserving X-Robots-Tag.`,
    );
  }
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
