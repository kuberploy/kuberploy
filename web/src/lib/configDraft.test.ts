import { describe, expect, it } from "vitest";
import { parse, stringify } from "yaml";
import {
  applyGuidedConfig,
  defaultGuidedProbes,
  defaultConfigYaml,
  guidedConfigFromYaml,
  validateGuidedRuntimeProcess,
  validateYaml,
  validateGuidedProbes,
  workloadProcessFromGuided,
  workloadProbesFromGuided,
} from "./configDraft";
import { defaultGuidedTraefikMiddleware } from "./traefikMiddleware";

const source = `apiVersion: config.kuberploy.io/v1alpha1
kind: AppConfig
metadata:
  name: hello
spec:
  # This comment must survive a guided edit.
  runtime:
    replicas: 1
    ports:
      - name: http
        containerPort: 3000
        protocol: TCP
    resources:
      requests:
        cpu: 50m
        memory: 100Mi
    env:
      - name: DATABASE_PASSWORD
        valueFrom:
          secretBindingRef:
            bindingId: 44444444-4444-7444-8444-444444444444
            name: database
            key: password
            version: 3
    command: [server]
    schedulingProfile:
      profileId: 55555555-5555-4555-8555-555555555555
      revision: 2
      specDigest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      assignmentsDigest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    nodeSelector:
      kubernetes.io/arch: amd64
    affinity:
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms:
            - matchExpressions:
                - key: karpenter.sh/capacity-type
                  operator: In
                  values: [on-demand]
    topologySpreadConstraints: []
    tolerations:
      - key: workload.kuberploy.io/class
        operator: Equal
        value: application
        effect: NoSchedule
  routes: []
`;

describe("shared AppConfig draft", () => {
  it("reads guided values from the same YAML document", () => {
    const guided = guidedConfigFromYaml(source);
    expect(guided).toMatchObject({
      replicas: 1,
      strategyType: "RollingUpdate",
      ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
      cpuRequest: "50m",
      memoryRequest: "100Mi",
      tlsMode: "httpOnly",
      dnsMode: "manual",
      schedulingProfile: {
        profileId: "55555555-5555-4555-8555-555555555555",
        revision: 2,
        specDigest:
          "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        assignmentsDigest:
          "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      },
      secretVariables: [
        {
          name: "DATABASE_PASSWORD",
          bindingId: "44444444-4444-7444-8444-444444444444",
          bindingName: "database",
          key: "password",
          version: 3,
        },
      ],
    });
  });

  it("round-trips the Kubernetes deployment strategy", () => {
    const guided = guidedConfigFromYaml(source);
    const updated = applyGuidedConfig(source, {
      ...guided,
      strategyType: "Recreate",
    });
    expect(guidedConfigFromYaml(updated).strategyType).toBe("Recreate");
    expect(parse(updated)).toMatchObject({
      spec: { runtime: { strategy: { type: "Recreate" } } },
    });
  });

  it("preserves comments and advanced fields while updating guided paths", () => {
    const guided = guidedConfigFromYaml(source);
    const updated = applyGuidedConfig(source, {
      ...guided,
      replicas: 3,
      host: "hello.e2e.k8s.orb.local",
      tlsMode: "letsencrypt",
      issuerRef: "local-acme",
      dnsMode: "externalDns",
      dnsIntegrationRef: "local-dns",
      middlewares: [
        defaultGuidedTraefikMiddleware("headers", "secure-headers"),
        defaultGuidedTraefikMiddleware("compress", "compress"),
      ],
      middlewareRefs: ["secure-headers", "compress"],
    });

    expect(updated).toContain("# This comment must survive a guided edit.");
    expect(
      (parse(updated) as { spec: { runtime: { command: string[] } } }).spec
        .runtime.command,
    ).toEqual(["server"]);
    expect(updated).toContain("requiredDuringSchedulingIgnoredDuringExecution");
    expect(updated).toContain("workload.kuberploy.io/class");
    expect(updated).toContain("secretBindingRef");
    expect(updated).not.toContain("value: super-secret");
    expect(updated).toContain("replicas: 3");
    expect(updated).toContain("hello.e2e.k8s.orb.local");
    expect(updated).toContain("secure-headers");
    expect(validateYaml(updated)).toBeNull();
  });

  it("rejects legacy and incomplete secret references without coercion", () => {
    const legacy = source.replace("version: 3", "version: v3");
    expect(() => guidedConfigFromYaml(legacy)).toThrow(/string versions/i);
    expect(validateYaml(legacy)).toMatch(/string versions/i);

    const missingIdentity = source.replace(
      "            bindingId: 44444444-4444-7444-8444-444444444444\n",
      "",
    );
    expect(() => guidedConfigFromYaml(missingIdentity)).toThrow(
      /immutable binding UUID/i,
    );
  });

  it("round-trips only the exact immutable custom-certificate reference", () => {
    const guided = guidedConfigFromYaml(source);
    const configured = applyGuidedConfig(source, {
      ...guided,
      host: "api.example.test",
      tlsMode: "customCertificate",
      certificateRef: {
        bindingId: "55555555-5555-7555-8555-555555555555",
        name: "public-edge",
        version: 4,
      },
    });
    const parsed = parse(configured) as {
      spec: {
        routes: Array<{
          tls: { secretRef: Record<string, unknown> };
        }>;
      };
    };
    expect(parsed.spec.routes[0]?.tls.secretRef).toEqual({
      bindingId: "55555555-5555-7555-8555-555555555555",
      name: "public-edge",
      version: 4,
    });
    expect(guidedConfigFromYaml(configured).certificateRef).toEqual({
      bindingId: "55555555-5555-7555-8555-555555555555",
      name: "public-edge",
      version: 4,
    });

    parsed.spec.routes[0]!.tls.secretRef =
      "caller-secret-name" as unknown as Record<string, unknown>;
    const legacy = stringify(parsed);
    expect(() => guidedConfigFromYaml(legacy)).toThrow(/Legacy string Secret/i);
    expect(() =>
      applyGuidedConfig(configured, {
        ...guidedConfigFromYaml(configured),
        certificateRef: null,
      }),
    ).toThrow(/Choose one exact ready certificate/i);
  });

  it("round-trips sslip as a closed DNS mode without caller IP or integration fields", () => {
    const guided = guidedConfigFromYaml(source);
    const configured = applyGuidedConfig(source, {
      ...guided,
      host: "api-203-0-113-10.sslip.io",
      dnsMode: "sslip",
      dnsIntegrationRef: "must-not-be-used",
      dnsTtl: 30,
    });
    const parsed = parse(configured) as {
      spec: {
        routes: Array<{
          host: string;
          dns: Record<string, unknown>;
        }>;
      };
    };
    expect(parsed.spec.routes[0]).toMatchObject({
      host: "api-203-0-113-10.sslip.io",
      dns: { mode: "sslip" },
    });
    expect(Object.keys(parsed.spec.routes[0]!.dns)).toEqual(["mode"]);
    expect(guidedConfigFromYaml(configured).dnsMode).toBe("sslip");
    expect(configured).not.toContain("must-not-be-used");
  });

  it("reports duplicate YAML keys before preview", () => {
    expect(validateYaml("kind: AppConfig\nkind: Other\n")).toMatch(
      /Map keys must be unique/,
    );
  });

  it("writes explicit safe requests for every new service draft", () => {
    const draft = defaultConfigYaml({ name: "api", port: 8080 });
    expect(draft).toContain("cpu: 50m");
    expect(draft).toContain("memory: 100Mi");
    expect(guidedConfigFromYaml(draft).ports[0]?.containerPort).toBe(8080);
  });

  it("keeps CPU and memory limits independently optional", () => {
    const guided = guidedConfigFromYaml(source);
    const cpuOnly = applyGuidedConfig(source, {
      ...guided,
      cpuLimit: "500m",
      memoryLimit: "",
    });
    expect(cpuOnly).toContain("cpu: 500m");
    expect(cpuOnly).not.toMatch(/limits:\s*\n\s*cpu: 500m\s*\n\s*memory:/);
  });

  it("round-trips container argv literally without shell splitting", () => {
    const command = [
      "/bin/sh",
      "-c",
      'printf "%s" "$HOME; $(id)"',
      "argument with spaces",
    ];
    const args = ["--", "*.yaml", "semi;colon", "ไทย"];
    const guided = guidedConfigFromYaml(source);
    const updated = applyGuidedConfig(source, {
      ...guided,
      commandYaml: JSON.stringify(command),
      argsYaml: JSON.stringify(args),
      terminationGracePeriodSeconds: 45,
    });
    const runtime = (
      parse(updated) as { spec: { runtime: Record<string, unknown> } }
    ).spec.runtime;

    expect(runtime.command).toEqual(command);
    expect(runtime.args).toEqual(args);
    expect(runtime.terminationGracePeriodSeconds).toBe(45);
    const reparsed = guidedConfigFromYaml(updated);
    expect(parse(reparsed.commandYaml)).toEqual(command);
    expect(parse(reparsed.argsYaml)).toEqual(args);
    expect(reparsed.terminationGracePeriodSeconds).toBe(45);
  });

  it("omits empty process overrides so image defaults remain authoritative", () => {
    const guided = guidedConfigFromYaml(source);
    const updated = applyGuidedConfig(source, {
      ...guided,
      commandYaml: "# keep image ENTRYPOINT\n[]",
      argsYaml: "",
      terminationGracePeriodSeconds: undefined,
    });
    const runtime = (
      parse(updated) as { spec: { runtime: Record<string, unknown> } }
    ).spec.runtime;

    expect(runtime).not.toHaveProperty("command");
    expect(runtime).not.toHaveProperty("args");
    expect(runtime).not.toHaveProperty("terminationGracePeriodSeconds");
  });

  it("rejects adversarial process vectors and grace periods locally", () => {
    const valid = { commandYaml: "[]", argsYaml: "[]" };
    const cases: Array<
      [
        string,
        typeof valid & { terminationGracePeriodSeconds?: number },
        RegExp,
      ]
    > = [
      [
        "shell scalar",
        { ...valid, commandYaml: "/bin/sh -c 'echo owned'" },
        /YAML list, never a shell string/i,
      ],
      [
        "mapping",
        { ...valid, argsYaml: "argument: --unsafe" },
        /YAML list, never a shell string/i,
      ],
      [
        "non-string entry",
        { ...valid, commandYaml: '["server", 7]' },
        /non-empty, NUL-free UTF-8 strings/i,
      ],
      [
        "NUL entry",
        { ...valid, argsYaml: JSON.stringify(["bad\u0000argument"]) },
        /NUL-free/i,
      ],
      [
        "oversized entry",
        { ...valid, argsYaml: JSON.stringify(["x".repeat(4097)]) },
        /at most 4096 bytes/i,
      ],
      [
        "too many command entries",
        {
          ...valid,
          commandYaml: JSON.stringify(
            Array.from({ length: 65 }, (_, index) => `arg-${index}`),
          ),
        },
        /at most 64 entries/i,
      ],
      [
        "too many argument entries",
        {
          ...valid,
          argsYaml: JSON.stringify(
            Array.from({ length: 129 }, (_, index) => `arg-${index}`),
          ),
        },
        /at most 128 entries/i,
      ],
      [
        "invalid Unicode entry",
        { ...valid, argsYaml: JSON.stringify(["bad\ud800argument"]) },
        /UTF-8 strings/i,
      ],
      [
        "combined byte budget",
        {
          ...valid,
          commandYaml: JSON.stringify(
            Array.from({ length: 17 }, () => "x".repeat(4096)),
          ),
        },
        /at most 65536 bytes in total/i,
      ],
      [
        "zero grace period",
        { ...valid, terminationGracePeriodSeconds: 0 },
        /integer from 1 to 3600/i,
      ],
      [
        "fractional grace period",
        { ...valid, terminationGracePeriodSeconds: 1.5 },
        /integer from 1 to 3600/i,
      ],
      [
        "excessive grace period",
        { ...valid, terminationGracePeriodSeconds: 3601 },
        /integer from 1 to 3600/i,
      ],
    ];

    for (const [label, values, expected] of cases) {
      expect(validateGuidedRuntimeProcess(values), label).toMatch(expected);
      expect(() => workloadProcessFromGuided(values), label).toThrow(expected);
    }
  });

  it("accepts the exact process vector and grace boundaries", () => {
    const command = Array.from({ length: 64 }, (_, index) => `cmd-${index}`);
    const args = Array.from({ length: 128 }, (_, index) => `arg-${index}`);
    expect(
      workloadProcessFromGuided({
        commandYaml: JSON.stringify(command),
        argsYaml: JSON.stringify(args),
        terminationGracePeriodSeconds: 3600,
      }),
    ).toEqual({ command, args, terminationGracePeriodSeconds: 3600 });
    expect(
      workloadProcessFromGuided({
        commandYaml: '["server"]',
        argsYaml: "[]",
        terminationGracePeriodSeconds: 1,
      }),
    ).toEqual({ command: ["server"], terminationGracePeriodSeconds: 1 });
  });

  it("round-trips typed startup, readiness, and liveness probes", () => {
    const withProbes = source.replace(
      "  routes: []",
      `    probes:
      startup:
        tcpSocket:
          port: 3000
        failureThreshold: 30
      readiness:
        httpGet:
          path: /ready
          port: http
          scheme: HTTPS
        periodSeconds: 5
        successThreshold: 2
      liveness:
        exec:
          command: [/bin/check, --live]
        timeoutSeconds: 2
  routes: []`,
    );
    const guided = guidedConfigFromYaml(withProbes);

    expect(guided.probes).toMatchObject({
      startup: { mode: "tcpSocket", port: "3000", failureThreshold: 30 },
      readiness: {
        mode: "httpGet",
        httpPath: "/ready",
        httpScheme: "HTTPS",
        port: "http",
        periodSeconds: 5,
        successThreshold: 2,
      },
      liveness: {
        mode: "exec",
        timeoutSeconds: 2,
      },
    });

    const updated = applyGuidedConfig(withProbes, {
      ...guided,
      replicas: 2,
    });
    const reparsed = guidedConfigFromYaml(updated);
    expect(reparsed.probes).toEqual(guided.probes);
    expect(updated).toContain("port: 3000");
    expect(updated).toContain("port: http");
    expect(updated).toContain("- --live");
  });

  it("leaves probes absent by default and removes all-disabled probes", () => {
    const draft = defaultConfigYaml({ name: "api" });
    const guided = guidedConfigFromYaml(draft);
    expect(guided.probes).toEqual(defaultGuidedProbes("http"));
    expect(applyGuidedConfig(draft, guided)).not.toContain("probes:");
    expect(workloadProbesFromGuided(guided.probes)).toBeUndefined();
  });

  it("rejects malformed exec command YAML before it reaches preview", () => {
    const probes = defaultGuidedProbes();
    probes.readiness = {
      ...probes.readiness,
      mode: "exec",
      execCommandYaml: "command: /bin/check",
    };

    expect(validateGuidedProbes(probes)).toMatch(
      /exec command must be a YAML array/i,
    );
    expect(() =>
      applyGuidedConfig(source, {
        ...guidedConfigFromYaml(source),
        probes,
      }),
    ).toThrow(/exec command must be a YAML array/i);
  });

  it("validates probe ports and timing bounds without inventing defaults", () => {
    const probes = defaultGuidedProbes();
    probes.liveness = {
      ...probes.liveness,
      mode: "httpGet",
      port: "http",
      httpPath: "/live",
      timeoutSeconds: 2,
    };
    expect(workloadProbesFromGuided(probes)).toEqual({
      liveness: {
        httpGet: { path: "/live", port: "http" },
        timeoutSeconds: 2,
      },
    });
    expect(
      validateGuidedProbes(probes, [
        { name: "metrics", containerPort: 9090, protocol: "TCP" },
      ]),
    ).toMatch(/must match a configured TCP container port/i);

    probes.liveness.successThreshold = 2;
    expect(validateGuidedProbes(probes)).toMatch(
      /success threshold must be an integer from 1 to 1/i,
    );
  });

  it("preserves Advanced-only middleware fields and additional routes during unrelated Guided edits", () => {
    const advanced = source.replace(
      "  routes: []",
      `  middlewares:
    - name: advanced-headers
      spec:
        headers:
          hostsProxyHeaders:
            - X-Trusted-Proxy
  routes:
    - id: primary
      host: api.example.com
      path: /
      port: http
      ingressClassName: traefik
      tls:
        mode: httpOnly
      middlewareRefs: [advanced-headers]
    - id: metrics
      host: metrics.example.com
      path: /metrics
      port: http
      tls:
        mode: httpOnly`,
    );
    const guided = guidedConfigFromYaml(advanced);
    expect(guided.middlewareGuidedIssue).toMatch(/Advanced YAML/i);

    const updated = applyGuidedConfig(advanced, { ...guided, replicas: 2 });
    const parsed = parse(updated) as {
      spec: {
        middlewares: unknown[];
        routes: Array<Record<string, unknown>>;
        runtime: { replicas: number };
      };
    };
    expect(parsed.spec.runtime.replicas).toBe(2);
    expect(parsed.spec.middlewares).toEqual(
      (parse(advanced) as { spec: { middlewares: unknown[] } }).spec
        .middlewares,
    );
    expect(parsed.spec.routes[0]).toMatchObject({
      id: "primary",
      ingressClassName: "traefik",
      middlewareRefs: ["advanced-headers"],
    });
    expect(parsed.spec.routes[1]).toEqual(
      (parse(advanced) as { spec: { routes: unknown[] } }).spec.routes[1],
    );
  });

  it("round-trips ordered typed middleware definitions and route references", () => {
    const draft = defaultConfigYaml({ name: "api" });
    const guided = guidedConfigFromYaml(draft);
    const configured = applyGuidedConfig(draft, {
      ...guided,
      host: "api.example.com",
      middlewares: [
        defaultGuidedTraefikMiddleware("rateLimit", "api-rate"),
        defaultGuidedTraefikMiddleware("headers", "secure-headers"),
        defaultGuidedTraefikMiddleware("compress", "response-compress"),
      ],
      middlewareRefs: ["secure-headers", "api-rate", "response-compress"],
    });
    const reparsed = guidedConfigFromYaml(configured);
    expect(reparsed.middlewareGuidedIssue).toBe("");
    expect(reparsed.middlewares.map(({ name }) => name)).toEqual([
      "api-rate",
      "secure-headers",
      "response-compress",
    ]);
    expect(reparsed.middlewareRefs).toEqual([
      "secure-headers",
      "api-rate",
      "response-compress",
    ]);
    expect(applyGuidedConfig(configured, reparsed)).toEqual(configured);
  });

  it("routes middleware graphs with more than one chain to Advanced without deleting data", () => {
    const draft = defaultConfigYaml({ name: "api" });
    const guided = guidedConfigFromYaml(draft);
    const configured = applyGuidedConfig(draft, {
      ...guided,
      host: "api.example.com",
      middlewares: [
        defaultGuidedTraefikMiddleware("headers", "secure-headers"),
      ],
      middlewareRefs: ["secure-headers"],
    });
    const document = parse(configured) as {
      spec: { routes: Array<Record<string, unknown>> };
    };
    document.spec.routes.push({
      id: "metrics",
      host: "metrics.example.com",
      path: "/metrics",
      port: "http",
      tls: { mode: "httpOnly" },
      middlewareRefs: ["secure-headers"],
    });
    const multiRoute = stringify(document);
    const multiRouteGuided = guidedConfigFromYaml(multiRoute);
    expect(multiRouteGuided.middlewareGuidedIssue).toMatch(
      /additional routes/i,
    );

    const updated = applyGuidedConfig(multiRoute, {
      ...multiRouteGuided,
      replicas: 4,
    });
    const parsed = parse(updated) as {
      spec: {
        middlewares: unknown[];
        routes: Array<Record<string, unknown>>;
      };
    };
    expect(parsed.spec.middlewares).toEqual(
      (parse(multiRoute) as { spec: { middlewares: unknown[] } }).spec
        .middlewares,
    );
    expect(parsed.spec.routes[1]?.middlewareRefs).toEqual(["secure-headers"]);
  });

  it("refuses invalid Guided middleware instead of emitting a lossy draft", () => {
    const draft = defaultConfigYaml({ name: "api" });
    const guided = guidedConfigFromYaml(draft);
    expect(() =>
      applyGuidedConfig(draft, {
        ...guided,
        middlewares: [
          {
            name: "office",
            kind: "ipAllowList",
            config: { sourceRange: ["10.0.0.0/999"] },
          },
        ],
      }),
    ).toThrow(/explicit CIDR/i);
  });
});
