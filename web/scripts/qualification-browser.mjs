import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";

const [browser, baseURL, applicationId, deploymentId, cookieFile, output] =
  process.argv.slice(2);
if (
  !browser ||
  !baseURL ||
  !applicationId ||
  !deploymentId ||
  !cookieFile ||
  !output
)
  throw new Error("six bounded arguments required");
if (process.env.KUBERPLOY_E2E_BROWSER_TEST_SEAM === "true") {
  if (process.env.KUBERPLOY_E2E_HERMETIC_TEST !== "true")
    throw new Error("browser test seam is hermetic-only");
  const invoked = spawnSync(browser, ["--version"], { stdio: "ignore" });
  if (invoked.error) throw invoked.error;
  await writeFile(
    output,
    JSON.stringify({
      realBrowser: false,
      hermeticSeam: true,
      browserCommandInvoked: true,
      sourceChooser: true,
      configPreview: true,
      logs: true,
      metrics: true,
      rollback: true,
    }),
  );
  process.exit(0);
}
const profile = await mkdtemp(join(tmpdir(), "kuberploy-browser-"));
const child = spawn(
  browser,
  [
    "--headless=new",
    "--no-first-run",
    "--disable-gpu",
    "--remote-debugging-port=0",
    `--user-data-dir=${profile}`,
    "about:blank",
  ],
  { stdio: "ignore" },
);
try {
  let port;
  for (let i = 0; i < 100; i++) {
    try {
      port = (
        await readFile(join(profile, "DevToolsActivePort"), "utf8")
      ).split("\n")[0];
      break;
    } catch {
      await new Promise((r) => setTimeout(r, 50));
    }
  }
  if (!port) throw new Error("browser DevTools endpoint unavailable");
  const target = await (
    await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, {
      method: "PUT",
    })
  ).json();
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    socket.onopen = resolve;
    socket.onerror = reject;
  });
  let sequence = 0;
  const waiting = new Map();
  socket.onmessage = ({ data }) => {
    const message = JSON.parse(data);
    if (message.id && waiting.has(message.id)) {
      waiting.get(message.id)(message);
      waiting.delete(message.id);
    }
  };
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const id = ++sequence;
      waiting.set(id, (message) =>
        message.error
          ? reject(new Error(message.error.message))
          : resolve(message.result),
      );
      socket.send(JSON.stringify({ id, method, params }));
    });
  await send("Page.enable");
  await send("Runtime.enable");
  await send("Network.enable");
  const cookieLine = (await readFile(cookieFile, "utf8"))
    .replace(/^Cookie:\s*/, "")
    .trim();
  await send("Network.setCookies", {
    cookies: cookieLine.split(/;\s*/).map((pair) => {
      const at = pair.indexOf("=");
      return {
        name: pair.slice(0, at),
        value: pair.slice(at + 1),
        url: baseURL,
        secure: true,
        sameSite: "Strict",
      };
    }),
  });
  async function waitText(required) {
    for (let i = 0; i < 100; i++) {
      const result = await send("Runtime.evaluate", {
        expression: `document.body?.innerText || ""`,
        returnByValue: true,
      });
      const text = result.result?.value || "";
      if (required.every((value) => text.includes(value))) return;
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`installed UI did not expose ${required.join(", ")}`);
  }
  async function page(path, required) {
    await send("Page.navigate", { url: `${baseURL}${path}` });
    await waitText(required);
  }
  const click = async (label) => {
    const result = await send("Runtime.evaluate", {
      expression: `(()=>{const e=[...document.querySelectorAll('button,a')].find(x=>x.textContent.trim().includes(${JSON.stringify(label)}));if(!e)throw Error('missing ${label}');e.click();return true})()`,
      returnByValue: true,
    });
    if (result.exceptionDetails || result.result?.value !== true)
      throw new Error(`browser click failed: ${label}`);
    await new Promise((r) => setTimeout(r, 150));
    const active = await send("Runtime.evaluate", {
      expression: `(()=>{const e=[...document.querySelectorAll('button,a')].find(x=>x.textContent.trim().includes(${JSON.stringify(label)}));return !!e && (e.getAttribute('aria-selected')==='true'||e.getAttribute('aria-current')==='page'||e.classList.contains('active'))})()`,
      returnByValue: true,
    });
    if (active.exceptionDetails || active.result?.value !== true)
      throw new Error(`browser tab did not activate: ${label}`);
  };
  await page(`/applications/${applicationId}`, [
    "Application source",
    "GitHub / Dockerfile",
    "Existing image",
    "Helm / OCI",
  ]);
  const deploymentPath = `/applications/${applicationId}/deployments/${deploymentId}`;
  await page(deploymentPath, ["Configuration", "Logs", "Metrics", "Releases"]);
  await click("Configuration");
  await waitText(["Preview configuration"]);
  await click("Preview configuration");
  await waitText([
    "Preview is bound to this exact draft",
    "kuberploy-runtime@",
    "Rendered manifest diff",
  ]);
  await click("Logs");
  await click("Metrics");
  await click("Releases");
  await waitText(["Rollback as new intent"]);
  await writeFile(
    output,
    JSON.stringify({
      realBrowser: true,
      hermeticSeam: false,
      browserCommandInvoked: true,
      sourceChooser: true,
      configPreview: true,
      logs: true,
      metrics: true,
      rollback: true,
    }),
  );
  socket.close();
} finally {
  child.kill("SIGTERM");
  await rm(profile, { recursive: true, force: true });
}
