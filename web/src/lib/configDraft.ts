import { parse, parseDocument, stringify } from "yaml";
import type {
  CertificateBindingReference,
  WorkloadProbe,
  WorkloadProbePort,
  WorkloadProbes,
  WorkloadRuntime,
  SchedulingProfileRef,
} from "../api/types";
import {
  guidedTraefikMiddlewaresToValue,
  guidedTraefikMiddlewareState,
  validateGuidedTraefikMiddlewares,
  type GuidedTraefikMiddleware,
} from "./traefikMiddleware";

export type GuidedPort = {
  name: string;
  containerPort: number;
  servicePort?: number;
  protocol: "TCP" | "UDP";
};

export type GuidedProbeMode = "disabled" | "httpGet" | "tcpSocket" | "exec";

export type GuidedProbe = {
  mode: GuidedProbeMode;
  httpPath: string;
  httpScheme: "" | "HTTP" | "HTTPS";
  port: string;
  execCommandYaml: string;
  initialDelaySeconds?: number;
  periodSeconds?: number;
  timeoutSeconds?: number;
  successThreshold?: number;
  failureThreshold?: number;
};

export type GuidedProbes = {
  startup: GuidedProbe;
  readiness: GuidedProbe;
  liveness: GuidedProbe;
};

export type GuidedRuntimeProcess = {
  commandYaml: string;
  argsYaml: string;
  terminationGracePeriodSeconds?: number;
};

export type GuidedConfig = GuidedRuntimeProcess & {
  replicas: number;
  strategyType: "RollingUpdate" | "Recreate";
  ports: GuidedPort[];
  cpuRequest: string;
  memoryRequest: string;
  cpuLimit: string;
  memoryLimit: string;
  variables: Array<{ name: string; value: string }>;
  secretVariables: Array<{
    name: string;
    bindingId: string;
    bindingName: string;
    key: string;
    version: number;
  }>;
  schedulingProfile?: SchedulingProfileRef;
  probes: GuidedProbes;
  host: string;
  path: string;
  tlsMode: "httpOnly" | "letsencrypt" | "customCertificate";
  redirectHttp: boolean;
  issuerRef: string;
  certificateRef: CertificateBindingReference | null;
  dnsMode: "manual" | "externalDns" | "sslip";
  dnsIntegrationRef: string;
  dnsTtl: number;
  middlewares: GuidedTraefikMiddleware[];
  middlewareRefs: string[];
  middlewareGuidedIssue: string;
};

export function defaultGuidedProbe(port = "http"): GuidedProbe {
  return {
    mode: "disabled",
    httpPath: "/healthz",
    httpScheme: "",
    port,
    execCommandYaml: "- /bin/true",
  };
}

export function defaultGuidedProbes(port = "http"): GuidedProbes {
  return {
    startup: defaultGuidedProbe(port),
    readiness: defaultGuidedProbe(port),
    liveness: defaultGuidedProbe(port),
  };
}

function stringValue(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

function numberValue(value: unknown, fallback: number): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function booleanValue(value: unknown, fallback: boolean): boolean {
  return typeof value === "boolean" ? value : fallback;
}

const secretBindingIDPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const secretBindingNamePattern = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/;
const secretBindingKeyPattern = /^[A-Za-z0-9._-]+$/;

function exactGuidedSecretReference(value: Record<string, unknown>): {
  bindingId: string;
  bindingName: string;
  key: string;
  version: number;
} {
  const bindingId = value.bindingId;
  const name = value.name;
  const key = value.key;
  const version = value.version;
  if (
    typeof bindingId !== "string" ||
    !secretBindingIDPattern.test(bindingId) ||
    typeof name !== "string" ||
    name.length > 63 ||
    !secretBindingNamePattern.test(name) ||
    typeof key !== "string" ||
    key.length > 253 ||
    !secretBindingKeyPattern.test(key) ||
    typeof version !== "number" ||
    !Number.isSafeInteger(version) ||
    version < 1
  ) {
    throw new Error(
      "Secret references require an immutable binding UUID, reviewed binding name, key, and positive integer active version. Legacy string versions are not accepted.",
    );
  }
  return { bindingId, bindingName: name, key, version };
}

function exactGuidedCertificateReference(
  value: unknown,
): CertificateBindingReference {
  if (!isObject(value)) {
    throw new Error(
      "Custom certificates require an exact immutable bindingId, name, and positive integer version. Legacy string Secret names are not accepted; use Advanced YAML to inspect the original value.",
    );
  }
  const bindingId = value.bindingId;
  const name = value.name;
  const version = value.version;
  if (
    typeof bindingId !== "string" ||
    !secretBindingIDPattern.test(bindingId) ||
    typeof name !== "string" ||
    name.length > 63 ||
    !secretBindingNamePattern.test(name) ||
    typeof version !== "number" ||
    !Number.isSafeInteger(version) ||
    version < 1
  ) {
    throw new Error(
      "Custom certificates require an exact immutable binding UUID, reviewed DNS-label name, and positive integer version. Caller-selected Kubernetes Secret names are not accepted.",
    );
  }
  return { bindingId, name, version };
}

function exactGuidedSecretVariable(
  value: GuidedConfig["secretVariables"][number],
) {
  if (
    !value.name.trim() &&
    !value.bindingId &&
    !value.bindingName &&
    !value.key &&
    !value.version
  ) {
    return null;
  }
  const reference = exactGuidedSecretReference({
    bindingId: value.bindingId,
    name: value.bindingName,
    key: value.key,
    version: value.version,
  });
  if (!value.name.trim()) {
    throw new Error(
      "Secret references require an environment variable destination.",
    );
  }
  return { name: value.name.trim(), ...reference };
}

function yamlFragment(value: unknown, empty: "{}" | "[]"): string {
  if (value === undefined || value === null) return empty;
  return stringify(value).trim();
}

function parseFragment(
  value: string,
  kind: "array" | "object",
  label: string,
): unknown {
  const parsed = parse(value || (kind === "array" ? "[]" : "{}"), {
    uniqueKeys: true,
  }) as unknown;
  const valid = kind === "array" ? Array.isArray(parsed) : isObject(parsed);
  if (!valid) throw new Error(`${label} must be a YAML ${kind}.`);
  return parsed;
}

function hasUnpairedSurrogate(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (next < 0xdc00 || next > 0xdfff) return true;
      index += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return true;
    }
  }
  return false;
}

function parseRuntimeStringList(
  yaml: string,
  label: string,
  maximum: number,
): string[] | undefined {
  if (new TextEncoder().encode(yaml).length > 262_144) {
    throw new Error(`${label} YAML must be at most 262144 bytes.`);
  }
  let parsed: unknown;
  try {
    parsed = parse(yaml.trim() ? yaml : "[]", {
      uniqueKeys: true,
      maxAliasCount: 20,
    }) as unknown;
  } catch {
    throw new Error(`${label} must be a valid YAML list.`);
  }
  if (!Array.isArray(parsed)) {
    throw new Error(`${label} must be a YAML list, never a shell string.`);
  }
  if (parsed.length === 0) return undefined;
  if (parsed.length > maximum) {
    throw new Error(
      `${label} must contain at most ${maximum} entries, or use [] to keep the image default.`,
    );
  }
  if (
    parsed.some(
      (entry) =>
        typeof entry !== "string" ||
        entry.length === 0 ||
        entry.includes("\u0000") ||
        hasUnpairedSurrogate(entry) ||
        new TextEncoder().encode(entry).length > 4096,
    )
  ) {
    throw new Error(
      `${label} entries must be non-empty, NUL-free UTF-8 strings of at most 4096 bytes each.`,
    );
  }
  return parsed as string[];
}

export function workloadProcessFromGuided(
  values: GuidedRuntimeProcess,
): Pick<WorkloadRuntime, "command" | "args" | "terminationGracePeriodSeconds"> {
  const command = parseRuntimeStringList(
    values.commandYaml,
    "Container command",
    64,
  );
  const args = parseRuntimeStringList(
    values.argsYaml,
    "Container arguments",
    128,
  );
  const totalBytes = [...(command ?? []), ...(args ?? [])].reduce(
    (total, entry) => total + new TextEncoder().encode(entry).length,
    0,
  );
  if (totalBytes > 65_536) {
    throw new Error(
      "Container command and arguments must be at most 65536 bytes in total.",
    );
  }
  const grace = values.terminationGracePeriodSeconds;
  if (
    grace !== undefined &&
    (!Number.isInteger(grace) || grace < 1 || grace > 3600)
  ) {
    throw new Error(
      "Termination grace period must be an integer from 1 to 3600 seconds.",
    );
  }
  return {
    ...(command ? { command } : {}),
    ...(args ? { args } : {}),
    ...(grace !== undefined ? { terminationGracePeriodSeconds: grace } : {}),
  };
}

export function validateGuidedRuntimeProcess(
  values: GuidedRuntimeProcess,
): string | null {
  try {
    workloadProcessFromGuided(values);
    return null;
  } catch (error) {
    return error instanceof Error
      ? error.message
      : "Invalid container process configuration.";
  }
}

function isObject(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function probePortValue(value: unknown, fallback: string): string {
  return typeof value === "string" ||
    (typeof value === "number" && Number.isInteger(value))
    ? String(value)
    : fallback;
}

function optionalIntegerValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value)
    ? value
    : undefined;
}

function guidedProbeFromValue(
  value: unknown,
  defaultPort: string,
): GuidedProbe {
  const probe = isObject(value) ? value : {};
  const httpGet = isObject(probe.httpGet) ? probe.httpGet : null;
  const tcpSocket = isObject(probe.tcpSocket) ? probe.tcpSocket : null;
  const exec = isObject(probe.exec) ? probe.exec : null;
  const mode: GuidedProbeMode = httpGet
    ? "httpGet"
    : tcpSocket
      ? "tcpSocket"
      : exec
        ? "exec"
        : "disabled";
  const portSource = httpGet?.port ?? tcpSocket?.port;
  const command = Array.isArray(exec?.command) ? exec.command : ["/bin/true"];
  return {
    mode,
    httpPath: stringValue(httpGet?.path, "/healthz"),
    httpScheme:
      httpGet?.scheme === "HTTP" || httpGet?.scheme === "HTTPS"
        ? httpGet.scheme
        : "",
    port: probePortValue(portSource, defaultPort),
    execCommandYaml: yamlFragment(command, "[]"),
    initialDelaySeconds: optionalIntegerValue(probe.initialDelaySeconds),
    periodSeconds: optionalIntegerValue(probe.periodSeconds),
    timeoutSeconds: optionalIntegerValue(probe.timeoutSeconds),
    successThreshold: optionalIntegerValue(probe.successThreshold),
    failureThreshold: optionalIntegerValue(probe.failureThreshold),
  };
}

function parseProbePort(value: string, label: string): WorkloadProbePort {
  const trimmed = value.trim();
  if (/^[0-9]+$/.test(trimmed)) {
    const port = Number(trimmed);
    if (Number.isInteger(port) && port >= 1 && port <= 65535) return port;
  } else if (
    trimmed.length <= 15 &&
    /^[a-z](?:[-a-z0-9]*[a-z0-9])?$/.test(trimmed)
  ) {
    return trimmed;
  }
  throw new Error(
    `${label} port must be a configured TCP port name or a number from 1 to 65535.`,
  );
}

function boundedProbeInteger(
  value: number | undefined,
  minimum: number,
  maximum: number,
  label: string,
): number | undefined {
  if (value === undefined) return undefined;
  if (!Number.isInteger(value) || value < minimum || value > maximum) {
    throw new Error(
      `${label} must be an integer from ${minimum} to ${maximum}.`,
    );
  }
  return value;
}

function workloadProbeFromGuided(
  phase: keyof GuidedProbes,
  value: GuidedProbe,
): WorkloadProbe | undefined {
  if (value.mode === "disabled") return undefined;
  const label = `${phase[0]?.toUpperCase()}${phase.slice(1)} probe`;
  const probe: WorkloadProbe = {};
  if (value.mode === "httpGet") {
    if (
      !value.httpPath.startsWith("/") ||
      new TextEncoder().encode(value.httpPath).length > 2048 ||
      /[\u0000-\u001f]/.test(value.httpPath)
    ) {
      throw new Error(
        `${label} HTTP path must begin with /, contain no control characters, and be at most 2048 bytes.`,
      );
    }
    probe.httpGet = {
      path: value.httpPath,
      port: parseProbePort(value.port, label),
      ...(value.httpScheme ? { scheme: value.httpScheme } : {}),
    };
  } else if (value.mode === "tcpSocket") {
    probe.tcpSocket = { port: parseProbePort(value.port, label) };
  } else {
    const command = parseFragment(
      value.execCommandYaml,
      "array",
      `${label} exec command`,
    ) as unknown[];
    if (
      command.length < 1 ||
      command.length > 32 ||
      command.some(
        (argument) =>
          typeof argument !== "string" ||
          !argument ||
          argument.includes("\u0000") ||
          new TextEncoder().encode(argument).length > 4096,
      ) ||
      command.reduce<number>(
        (total, argument) =>
          total +
          (typeof argument === "string"
            ? new TextEncoder().encode(argument).length
            : 0),
        0,
      ) >
        16 * 1024
    ) {
      throw new Error(
        `${label} exec command must be a YAML list of 1 to 32 non-empty string arguments, limited to 4096 bytes each and 16384 bytes total.`,
      );
    }
    probe.exec = { command: command as string[] };
  }
  const timing = {
    initialDelaySeconds: boundedProbeInteger(
      value.initialDelaySeconds,
      0,
      3600,
      `${label} initial delay`,
    ),
    periodSeconds: boundedProbeInteger(
      value.periodSeconds,
      1,
      300,
      `${label} period`,
    ),
    timeoutSeconds: boundedProbeInteger(
      value.timeoutSeconds,
      1,
      300,
      `${label} timeout`,
    ),
    successThreshold: boundedProbeInteger(
      value.successThreshold,
      1,
      phase === "readiness" ? 100 : 1,
      `${label} success threshold`,
    ),
    failureThreshold: boundedProbeInteger(
      value.failureThreshold,
      1,
      100,
      `${label} failure threshold`,
    ),
  };
  for (const [key, timingValue] of Object.entries(timing)) {
    if (timingValue !== undefined)
      (probe as Record<string, unknown>)[key] = timingValue;
  }
  return probe;
}

export function workloadProbesFromGuided(
  values: GuidedProbes,
  configuredPorts?: GuidedPort[],
): WorkloadProbes | undefined {
  const probes: WorkloadProbes = {};
  for (const phase of ["startup", "readiness", "liveness"] as const) {
    const probe = workloadProbeFromGuided(phase, values[phase]);
    if (!probe) continue;
    const reference = probe.httpGet?.port ?? probe.tcpSocket?.port;
    if (
      reference !== undefined &&
      configuredPorts &&
      !configuredPorts.some(
        (port) =>
          port.protocol === "TCP" &&
          (typeof reference === "string"
            ? port.name === reference
            : port.containerPort === reference),
      )
    ) {
      throw new Error(
        `${phase[0]?.toUpperCase()}${phase.slice(1)} probe port must match a configured TCP container port.`,
      );
    }
    probes[phase] = probe;
  }
  return Object.keys(probes).length ? probes : undefined;
}

export function validateGuidedProbes(
  values: GuidedProbes,
  configuredPorts?: GuidedPort[],
): string | null {
  try {
    workloadProbesFromGuided(values, configuredPorts);
    return null;
  } catch (error) {
    return error instanceof Error ? error.message : "Invalid health probe.";
  }
}

export function guidedConfigFromYaml(rawYaml: string): GuidedConfig {
  const document = parseDocument(rawYaml, { uniqueKeys: true });
  if (document.errors.length)
    throw new Error(document.errors[0]?.message ?? "Invalid YAML");
  const value = document.toJS() as Record<string, unknown> | null;
  const spec = (value?.spec ?? {}) as Record<string, unknown>;
  const runtime = (spec.runtime ?? {}) as Record<string, unknown>;
  const resources = (runtime.resources ?? {}) as Record<string, unknown>;
  const requests = (resources.requests ?? {}) as Record<string, unknown>;
  const limits = (resources.limits ?? {}) as Record<string, unknown>;
  const ports = Array.isArray(runtime.ports)
    ? (runtime.ports as Array<Record<string, unknown>>)
    : [];
  const env = Array.isArray(runtime.env)
    ? (runtime.env as Array<Record<string, unknown>>)
    : [];
  const routes = Array.isArray(spec.routes)
    ? (spec.routes as Array<Record<string, unknown>>)
    : [];
  const route = routes[0] ?? {};
  const tls = (route.tls ?? {}) as Record<string, unknown>;
  const dns = (route.dns ?? {}) as Record<string, unknown>;
  const schedulingProfile = isObject(runtime.schedulingProfile)
    ? runtime.schedulingProfile
    : undefined;
  const guidedPorts: GuidedPort[] =
    ports.length > 0
      ? ports.map((port, index) => ({
          name: stringValue(port.name, index === 0 ? "http" : "port"),
          containerPort: numberValue(port.containerPort, 3000),
          servicePort:
            typeof port.servicePort === "number" ? port.servicePort : undefined,
          protocol: port.protocol === "UDP" ? "UDP" : "TCP",
        }))
      : [
          {
            name: "http",
            containerPort: 3000,
            protocol: "TCP",
          },
        ];
  const defaultProbePort =
    guidedPorts.find((port) => port.protocol === "TCP")?.name || "http";
  const probes = isObject(runtime.probes) ? runtime.probes : {};
  const middlewareState = guidedTraefikMiddlewareState(
    spec.middlewares,
    route.middlewareRefs,
  );
  const additionalRouteUsesMiddleware = routes
    .slice(1)
    .some(
      (item) =>
        isObject(item) &&
        Array.isArray(item.middlewareRefs) &&
        item.middlewareRefs.length > 0,
    );
  const middlewareGuidedIssue =
    middlewareState.issue ||
    (additionalRouteUsesMiddleware
      ? "Guided middleware editing cannot safely update definitions referenced by additional routes. The original YAML is preserved; use Advanced YAML to inspect or change the complete route graph."
      : "");

  return {
    replicas: numberValue(runtime.replicas, 1),
    strategyType:
      isObject(runtime.strategy) && runtime.strategy.type === "Recreate"
        ? "Recreate"
        : "RollingUpdate",
    commandYaml: yamlFragment(runtime.command, "[]"),
    argsYaml: yamlFragment(runtime.args, "[]"),
    terminationGracePeriodSeconds:
      typeof runtime.terminationGracePeriodSeconds === "number" &&
      Number.isFinite(runtime.terminationGracePeriodSeconds)
        ? runtime.terminationGracePeriodSeconds
        : undefined,
    ports: guidedPorts,
    cpuRequest: stringValue(requests.cpu, "50m"),
    memoryRequest: stringValue(requests.memory, "100Mi"),
    cpuLimit: stringValue(limits.cpu),
    memoryLimit: stringValue(limits.memory),
    variables: env.flatMap((item) =>
      typeof item.name === "string" && typeof item.value === "string"
        ? [{ name: item.name, value: item.value }]
        : [],
    ),
    secretVariables: env.flatMap((item) => {
      const valueFrom = isObject(item.valueFrom) ? item.valueFrom : {};
      if (!("secretBindingRef" in valueFrom)) return [];
      if (
        typeof item.name !== "string" ||
        !isObject(valueFrom.secretBindingRef)
      ) {
        throw new Error(
          "Secret references require an environment variable name and an exact immutable binding reference.",
        );
      }
      const ref = exactGuidedSecretReference(valueFrom.secretBindingRef);
      return [{ name: item.name, ...ref }];
    }),
    schedulingProfile:
      schedulingProfile &&
      typeof schedulingProfile.profileId === "string" &&
      typeof schedulingProfile.revision === "number" &&
      typeof schedulingProfile.specDigest === "string" &&
      typeof schedulingProfile.assignmentsDigest === "string"
        ? {
            profileId: schedulingProfile.profileId,
            revision: schedulingProfile.revision,
            specDigest: schedulingProfile.specDigest,
            assignmentsDigest: schedulingProfile.assignmentsDigest,
          }
        : undefined,
    probes: {
      startup: guidedProbeFromValue(probes.startup, defaultProbePort),
      readiness: guidedProbeFromValue(probes.readiness, defaultProbePort),
      liveness: guidedProbeFromValue(probes.liveness, defaultProbePort),
    },
    host: stringValue(route.host),
    path: stringValue(route.path, "/"),
    tlsMode:
      tls.mode === "letsencrypt" || tls.mode === "customCertificate"
        ? tls.mode
        : "httpOnly",
    redirectHttp: booleanValue(tls.redirectHttp, true),
    issuerRef: stringValue(tls.issuerRef),
    certificateRef:
      tls.mode === "customCertificate"
        ? exactGuidedCertificateReference(tls.secretRef)
        : null,
    dnsMode:
      dns.mode === "externalDns" || dns.mode === "sslip" ? dns.mode : "manual",
    dnsIntegrationRef: stringValue(dns.integrationRef),
    dnsTtl: numberValue(dns.ttl, 300),
    middlewares: middlewareState.definitions,
    middlewareRefs: middlewareState.refs,
    middlewareGuidedIssue,
  };
}

export function applyGuidedConfig(
  rawYaml: string,
  values: GuidedConfig,
): string {
  const document = parseDocument(rawYaml, { uniqueKeys: true });
  if (document.errors.length)
    throw new Error(document.errors[0]?.message ?? "Invalid YAML");
  const process = workloadProcessFromGuided(values);
  const probes = workloadProbesFromGuided(values.probes, values.ports);
  const currentValue = document.toJS() as Record<string, unknown> | null;
  const currentSpec = isObject(currentValue?.spec) ? currentValue.spec : {};
  const currentRoutes = Array.isArray(currentSpec.routes)
    ? currentSpec.routes
    : [];
  const currentRoute = isObject(currentRoutes[0]) ? currentRoutes[0] : {};
  const currentMiddlewareState = guidedTraefikMiddlewareState(
    currentSpec.middlewares,
    currentRoute.middlewareRefs,
  );

  if (!values.middlewareGuidedIssue) {
    if (currentMiddlewareState.issue) {
      throw new Error(
        "The current middleware configuration contains fields Guided cannot represent. It is preserved; use Advanced YAML to change it.",
      );
    }
    const middlewareError = validateGuidedTraefikMiddlewares(
      values.middlewares,
      values.middlewareRefs,
    );
    if (middlewareError) throw new Error(middlewareError);
    const currentDefinitions = guidedTraefikMiddlewaresToValue(
      currentMiddlewareState.definitions,
    );
    const nextDefinitions = guidedTraefikMiddlewaresToValue(values.middlewares);
    if (
      JSON.stringify(currentDefinitions) !== JSON.stringify(nextDefinitions)
    ) {
      document.setIn(["spec", "middlewares"], nextDefinitions);
    }
  }

  document.setIn(["spec", "runtime", "replicas"], values.replicas);
  document.setIn(["spec", "runtime", "strategy", "type"], values.strategyType);
  if (process.command)
    document.setIn(["spec", "runtime", "command"], process.command);
  else document.deleteIn(["spec", "runtime", "command"]);
  if (process.args) document.setIn(["spec", "runtime", "args"], process.args);
  else document.deleteIn(["spec", "runtime", "args"]);
  if (process.terminationGracePeriodSeconds !== undefined) {
    document.setIn(
      ["spec", "runtime", "terminationGracePeriodSeconds"],
      process.terminationGracePeriodSeconds,
    );
  } else {
    document.deleteIn(["spec", "runtime", "terminationGracePeriodSeconds"]);
  }
  document.setIn(
    ["spec", "runtime", "ports"],
    values.ports.map((port) => ({
      name: port.name.trim(),
      containerPort: port.containerPort,
      ...(port.servicePort ? { servicePort: port.servicePort } : {}),
      protocol: port.protocol,
    })),
  );
  document.setIn(
    ["spec", "runtime", "resources", "requests", "cpu"],
    values.cpuRequest,
  );
  document.setIn(
    ["spec", "runtime", "resources", "requests", "memory"],
    values.memoryRequest,
  );
  if (values.cpuLimit || values.memoryLimit) {
    document.setIn(["spec", "runtime", "resources", "limits"], {
      ...(values.cpuLimit ? { cpu: values.cpuLimit } : {}),
      ...(values.memoryLimit ? { memory: values.memoryLimit } : {}),
    });
  } else {
    document.deleteIn(["spec", "runtime", "resources", "limits"]);
  }
  document.setIn(
    ["spec", "runtime", "env"],
    [
      ...values.variables
        .filter(({ name }) => name.trim())
        .map(({ name, value }) => ({ name: name.trim(), value })),
      ...values.secretVariables
        .map(exactGuidedSecretVariable)
        .filter((value) => value !== null)
        .map(({ name, bindingId, bindingName, key, version }) => ({
          name,
          valueFrom: {
            secretBindingRef: {
              bindingId,
              name: bindingName,
              key,
              version,
            },
          },
        })),
    ],
  );
  if (values.schedulingProfile)
    document.setIn(
      ["spec", "runtime", "schedulingProfile"],
      values.schedulingProfile,
    );
  else document.deleteIn(["spec", "runtime", "schedulingProfile"]);
  if (probes) document.setIn(["spec", "runtime", "probes"], probes);
  else document.deleteIn(["spec", "runtime", "probes"]);

  if (!values.host.trim()) {
    if (currentRoutes.length <= 1) {
      document.setIn(["spec", "routes"], []);
    } else if (stringValue(currentRoute.host)) {
      throw new Error(
        "Guided cannot remove only the first of multiple public routes. The routes are preserved; use Advanced YAML.",
      );
    }
  } else {
    const tls: Record<string, unknown> = { mode: values.tlsMode };
    if (values.tlsMode === "letsencrypt") {
      tls.issuerRef = values.issuerRef;
      tls.redirectHttp = values.redirectHttp;
    }
    if (values.tlsMode === "customCertificate") {
      if (!values.certificateRef) {
        throw new Error(
          "Choose one exact ready certificate binding and immutable active version before saving custom TLS.",
        );
      }
      tls.secretRef = exactGuidedCertificateReference(values.certificateRef);
      tls.redirectHttp = values.redirectHttp;
    }
    const dns: Record<string, unknown> = { mode: values.dnsMode };
    if (values.dnsMode === "externalDns") {
      dns.integrationRef = values.dnsIntegrationRef;
      dns.ttl = values.dnsTtl;
    }
    document.setIn(["spec", "routes", 0, "host"], values.host.trim());
    document.setIn(["spec", "routes", 0, "path"], values.path || "/");
    document.setIn(
      ["spec", "routes", 0, "port"],
      values.ports[0]?.name || "http",
    );
    document.setIn(["spec", "routes", 0, "dns"], dns);
    document.setIn(["spec", "routes", 0, "tls"], tls);
    if (!values.middlewareGuidedIssue) {
      document.setIn(
        ["spec", "routes", 0, "middlewareRefs"],
        values.middlewareRefs,
      );
    }
  }
  return document.toString();
}

export function defaultConfigYaml(input: {
  name: string;
  image?: string;
  replicas?: number;
  port?: number;
}): string {
  const [repository, digest] = (input.image ?? "").split("@");
  const document = parseDocument(`apiVersion: config.kuberploy.io/v1alpha1
kind: AppConfig
metadata:
  name: ${input.name}
spec:
  delivery:
    mode: image
    release:
      repository: ${repository || "registry.example.com/team/application"}
      digest: ${digest || "sha256:replace-with-an-immutable-digest"}
  runtime:
    replicas: ${input.replicas ?? 1}
    ports:
      - name: http
        containerPort: ${input.port ?? 3000}
        protocol: TCP
    resources:
      requests:
        cpu: 50m
        memory: 100Mi
    env: []
  routes: []
`);
  return document.toString();
}

export function validateYaml(rawYaml: string): string | null {
  const document = parseDocument(rawYaml, { uniqueKeys: true });
  if (document.errors[0]) return document.errors[0].message;
  try {
    // Guided and Advanced share one document. Reject an inexact secret
    // reference before preview so switching modes can never coerce or silently
    // discard a legacy string version or caller-typed identity.
    guidedConfigFromYaml(rawYaml);
    return null;
  } catch (error) {
    return error instanceof Error ? error.message : "Invalid AppConfig";
  }
}
