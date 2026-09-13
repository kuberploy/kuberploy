import { createHash } from "node:crypto";
import { copyFile, mkdir, readFile, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const SWAGGER_UI_DIST_VERSION = "5.32.14";

export const SWAGGER_UI_ASSETS = [
  "LICENSE",
  "NOTICE",
  "favicon-16x16.png",
  "favicon-32x32.png",
  "oauth2-redirect.html",
  "oauth2-redirect.js",
  "swagger-ui-bundle.js",
  "swagger-ui-bundle.js.LICENSE.txt",
  "swagger-ui-standalone-preset.js",
  "swagger-ui-standalone-preset.js.LICENSE.txt",
  "swagger-ui.css",
  "swagger-ui.css.map",
];

const scriptPath = fileURLToPath(import.meta.url);
const projectRoot = resolve(dirname(scriptPath), "..");
const require = createRequire(import.meta.url);
const swaggerPackagePath = require.resolve("swagger-ui-dist/package.json");
const swaggerRoot = dirname(swaggerPackagePath);

function digest(algorithm, value, encoding = "hex") {
  return createHash(algorithm).update(value).digest(encoding);
}

function sri(value) {
  return `sha384-${digest("sha384", value, "base64")}`;
}

function renderIndex(integrity, versions) {
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <meta name="referrer" content="same-origin" />
    <title>Kuberploy API documentation</title>
    <link rel="icon" type="image/png" sizes="32x32" href="./favicon-32x32.png" />
    <link rel="icon" type="image/png" sizes="16x16" href="./favicon-16x16.png" />
    <link rel="stylesheet" href="./swagger-ui.css?v=${versions["swagger-ui.css"]}" integrity="${integrity["swagger-ui.css"]}" crossorigin="anonymous" />
    <style>
      html { box-sizing: border-box; overflow-y: scroll; }
      *, *::before, *::after { box-sizing: inherit; }
      body { margin: 0; background: #f7f9f6; }
      .contract-links { display: flex; gap: 1rem; padding: .65rem 1rem; background: #10231d; color: #d7e3dd; font: 600 13px/1.4 system-ui, sans-serif; }
      .contract-links a { color: #c6f6d5; }
      .swagger-ui .topbar { background: #08120f; }
      .swagger-ui .topbar .download-url-wrapper .select-label { color: #d7e3dd; }
      .swagger-ui .docs-choice { display: inline-block; min-width: 9rem; max-width: 100%; max-height: 10rem; overflow: auto; border: 1px solid #899d93; border-radius: 5px; background: #fff; color: #10231d; font: 400 14px/1.4 sans-serif; vertical-align: middle; }
      .swagger-ui .docs-choice:focus-visible { outline: 2px solid #126b4f; outline-offset: 2px; }
      .swagger-ui .docs-choice[aria-disabled="true"] { opacity: .6; }
      .swagger-ui .docs-choice [role="option"] { padding: .4rem .65rem; cursor: pointer; overflow-wrap: anywhere; }
      .swagger-ui .docs-choice [aria-selected="true"] { background: #d7eee3; font-weight: 600; }
      .swagger-ui .docs-choice:focus [data-active="true"] { outline: 2px solid #126b4f; outline-offset: -2px; }
      .swagger-ui .docs-choice-single { display: inline-block; padding: .4rem .65rem; border: 1px solid #d7e3dd; border-radius: 5px; }
      .docs-error { width: min(680px, calc(100% - 2rem)); margin: 5rem auto; padding: 2rem; border: 1px solid #d7e3dd; border-radius: 16px; background: #fff; box-shadow: 0 18px 55px rgb(8 18 15 / 10%); color: #10231d; font: 400 16px/1.6 system-ui, sans-serif; }
      .docs-error h1 { margin: 0 0 .5rem; font-size: 1.5rem; line-height: 1.25; }
      .docs-error p { margin: 0 0 1.25rem; color: #496159; }
      .docs-error button { border: 0; border-radius: 9px; padding: .7rem 1rem; background: #126b4f; color: #fff; font: 700 14px/1 system-ui, sans-serif; cursor: pointer; }
    </style>
  </head>
  <body>
    <nav class="contract-links" aria-label="Machine-readable API contracts">
      <span>Machine contracts:</span>
      <a href="/openapi-agent.json">agent profile</a>
      <a href="/arazzo.yaml">Arazzo workflows</a>
    </nav>
    <div id="swagger-ui"></div>
    <noscript>Kuberploy API documentation requires JavaScript.</noscript>
    <script src="./swagger-ui-bundle.js?v=${versions["swagger-ui-bundle.js"]}" integrity="${integrity["swagger-ui-bundle.js"]}" crossorigin="anonymous"></script>
    <script src="./swagger-ui-standalone-preset.js?v=${versions["swagger-ui-standalone-preset.js"]}" integrity="${integrity["swagger-ui-standalone-preset.js"]}" crossorigin="anonymous"></script>
    <script>
      window.addEventListener("load", function () {
        var root = document.getElementById("swagger-ui");
        function showError() {
          root.innerHTML = '<section class="docs-error" role="alert"><h1>API documentation could not load</h1><p>A cached or interrupted asset prevented the reference from starting. Reload to try again.</p><button type="button" id="docs-reload">Reload documentation</button></section>';
          document.getElementById("docs-reload").addEventListener("click", function () {
            window.location.reload();
          });
        }

        if (typeof window.SwaggerUIBundle !== "function" || !Array.isArray(window.SwaggerUIStandalonePreset)) {
          showError();
          return;
        }

        try {
          window.ui = window.SwaggerUIBundle({
            url: "/openapi.yaml",
            dom_id: "#swagger-ui",
            deepLinking: true,
            displayOperationId: true,
            displayRequestDuration: true,
            persistAuthorization: false,
            queryConfigEnabled: false,
            requestSnippetsEnabled: true,
            validatorUrl: null,
            presets: [window.SwaggerUIBundle.presets.apis, window.SwaggerUIStandalonePreset],
            plugins: [function (system) {
              var React = system.React;
              var choiceSequence = 0;
              function Choice(props) {
                var options = React.Children.toArray(props.children).filter(function (item) { return item && item.type === "option"; });
                var rawValue = props.value === undefined ? props.defaultValue : props.value;
                var selected = (props.multiple ? Array.from(rawValue || []) : [rawValue]).map(String);
                var activeState = React.useState(function () { return Math.max(0, options.findIndex(function (option) { return selected.indexOf(String(option.props.value)) !== -1; })); });
                var active = Math.min(activeState[0], Math.max(0, options.length - 1)), setActive = activeState[1];
                var list = React.useRef(null), identifier = React.useRef(null), search = React.useRef({ text: "", at: 0 });
                if (!identifier.current) identifier.current = "docs-choice-" + (++choiceSequence);
                function valueOf(option) { return String(option.props.value); }
                function change(values) {
                  if (props.disabled || !props.onChange) return;
                  var eventOptions = options.map(function (option) {
                    var value = valueOf(option);
                    return { value: value, selected: values.indexOf(value) !== -1, getAttribute: function (name) { return name === "value" ? value : null; } };
                  });
                  props.onChange({ target: { value: values[0] || "", options: eventOptions, selectedOptions: eventOptions.filter(function (option) { return option.selected; }), getAttribute: function (name) { return props[name]; } } });
                }
                function choose(index) {
                  if (props.disabled || !options[index]) return;
                  setActive(index);
                  var value = valueOf(options[index]);
                  change(props.multiple
                    ? options.map(valueOf).filter(function (item) { return item === value ? selected.indexOf(item) === -1 : selected.indexOf(item) !== -1; })
                    : [value]);
                  if (list.current) list.current.focus();
                }
                React.useEffect(function () {
                  if (!props.multiple && options.length === 1 && selected[0] !== valueOf(options[0])) change([valueOf(options[0])]);
                }, [props.value, props.multiple, options.length]);
                React.useEffect(function () {
                  if (props.multiple) return;
                  var index = options.findIndex(function (option) { return selected.indexOf(valueOf(option)) !== -1; });
                  if (index >= 0) setActive(index);
                }, [props.value]);
                function keyDown(event) {
                  if (props.disabled || !options.length) return;
                  var next = active;
                  if (event.key === "ArrowDown") next = Math.min(options.length - 1, active + 1);
                  else if (event.key === "ArrowUp") next = Math.max(0, active - 1);
                  else if (event.key === "Home") next = 0;
                  else if (event.key === "End") next = options.length - 1;
                  else if (event.key === " " || event.key === "Enter") { event.preventDefault(); choose(active); return; }
                  else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
                    var now = Date.now();
                    search.current.text = (now - search.current.at < 700 ? search.current.text : "") + event.key.toLowerCase();
                    search.current.at = now;
                    next = options.findIndex(function (option) { return String(option.props.children).toLowerCase().indexOf(search.current.text) === 0; });
                    if (next < 0) return;
                  } else return;
                  event.preventDefault();
                  setActive(next);
                  if (!props.multiple) choose(next);
                  var option = list.current && list.current.children[next];
                  if (option && option.scrollIntoView) option.scrollIntoView({ block: "nearest" });
                }
                if (!props.multiple && options.length === 1) return React.createElement("span", { id: props.id, className: "docs-choice-single", "aria-label": props["aria-label"] }, options[0].props.children);
                return React.createElement("div", {
                  id: props.id, ref: list, role: "listbox", tabIndex: props.disabled ? -1 : 0,
                  className: "docs-choice " + (props.className || ""), title: props.title,
                  "aria-label": props["aria-label"] || "Value", "aria-labelledby": props["aria-labelledby"],
                  "aria-controls": props["aria-controls"], "aria-disabled": !!props.disabled,
                  "aria-multiselectable": !!props.multiple, "aria-activedescendant": options.length ? identifier.current + "-" + active : undefined,
                  onKeyDown: keyDown
                }, options.map(function (option, index) {
                  return React.createElement("div", { key: index, id: identifier.current + "-" + index, role: "option", "aria-selected": selected.indexOf(valueOf(option)) !== -1, "data-active": index === active, onClick: function () { choose(index); } }, option.props.children);
                }));
              }
              function replaceSelect(element) {
                if (!React.isValidElement(element)) return element;
                if (element.type === "select") return React.createElement(Choice, Object.assign({ key: element.key }, element.props));
                return element.props.children === undefined ? element : React.cloneElement(element, undefined, React.Children.map(element.props.children, replaceSelect));
              }
              function customChoices(Original) {
                // Keep Swagger's own lifecycle, default values and change handlers.
                return class extends Original { render() { return replaceSelect(super.render()); } };
              }
              return { wrapComponents: {
                Select: customChoices,
                contentType: customChoices,
                ExamplesSelect: customChoices,
                schemes: customChoices,
                oauth2: customChoices,
                Topbar: customChoices,
                Servers: function (Original) {
                  return function InstallationServer(props) {
                    var first = props.servers.first();
                    var singleInstallation = props.servers.size === 1 && first.get("url") === "/" && !first.get("variables");
                    system.React.useEffect(function () {
                      if (singleInstallation && props.currentServer !== "/") props.setSelectedServer("/");
                    }, [singleInstallation, props.currentServer, props.setSelectedServer]);
                    return singleInstallation
                      ? system.React.createElement("div", { className: "servers", "data-installation-server": true },
                          system.React.createElement("code", null, "/"), " — this Kuberploy installation")
                      : system.React.createElement(Original, props);
                  };
                }
              } };
            }],
            layout: "StandaloneLayout"
          });
        } catch (error) {
          console.error("Kuberploy API documentation failed to start.", error);
          showError();
        }
      });
    </script>
  </body>
</html>
`;
}

export async function buildApiDocs(outputDirectory) {
  const packageDocument = JSON.parse(
    await readFile(swaggerPackagePath, "utf8"),
  );
  if (packageDocument.version !== SWAGGER_UI_DIST_VERSION) {
    throw new Error(
      `Expected swagger-ui-dist ${SWAGGER_UI_DIST_VERSION}, found ${packageDocument.version ?? "unknown"}.`,
    );
  }

  const output = resolve(outputDirectory);
  await mkdir(output, { recursive: true });

  const assetBytes = new Map();
  for (const name of SWAGGER_UI_ASSETS) {
    const source = join(swaggerRoot, name);
    const bytes = await readFile(source);
    assetBytes.set(name, bytes);
    await copyFile(source, join(output, name));
  }

  const integrity = Object.fromEntries(
    [
      "swagger-ui.css",
      "swagger-ui-bundle.js",
      "swagger-ui-standalone-preset.js",
    ].map((name) => [name, sri(assetBytes.get(name))]),
  );
  const versions = Object.fromEntries(
    [
      "swagger-ui.css",
      "swagger-ui-bundle.js",
      "swagger-ui-standalone-preset.js",
    ].map((name) => [
      name,
      digest("sha256", assetBytes.get(name)).slice(0, 16),
    ]),
  );
  const indexDocument = renderIndex(integrity, versions);
  await writeFile(join(output, "index.html"), indexDocument, "utf8");

  const files = [
    ...SWAGGER_UI_ASSETS.map((name) => ({
      path: name,
      sha256: digest("sha256", assetBytes.get(name)),
    })),
    {
      path: "index.html",
      sha256: digest("sha256", indexDocument),
    },
  ].sort((left, right) =>
    left.path < right.path ? -1 : left.path > right.path ? 1 : 0,
  );
  const manifest = {
    schemaVersion: "1",
    swaggerUiDistVersion: SWAGGER_UI_DIST_VERSION,
    files,
  };
  await writeFile(
    join(output, "docs-manifest.json"),
    `${JSON.stringify(manifest, null, 2)}\n`,
    "utf8",
  );

  return manifest;
}

if (process.argv[1] && resolve(process.argv[1]) === scriptPath) {
  const output = process.argv[2]
    ? resolve(process.cwd(), process.argv[2])
    : join(projectRoot, "dist", "docs");
  await buildApiDocs(output);
  process.stdout.write(
    `Built local Swagger UI ${SWAGGER_UI_DIST_VERSION} in ${output}\n`,
  );
}
