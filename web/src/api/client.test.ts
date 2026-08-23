import { afterEach, describe, expect, it, vi } from "vitest";
import {
  api,
  asCollection,
  normalizeDeployment,
  normalizeOperation,
} from "./client";
import type { OperationWire } from "./types";

afterEach(() => vi.unstubAllGlobals());

describe("typed API client", () => {
  it("accepts a caller-stable idempotency key when reserving an application", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ id: "app_1", projectId: "project_1", name: "API" }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.createApplication(
      { projectId: "project_1", name: "API" },
      "reservation-attempt-1",
    );

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/v1/applications");
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe(
      "reservation-attempt-1",
    );
  });

  it("uses the exact bootstrap contract and same-origin cookies", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "usr_1",
          displayName: "Example User",
          role: "platform-admin",
          createdAt: "2026-08-06T00:00:00Z",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=test-csrf; path=/";

    await api.bootstrap({
      token: "one-time",
      email: "admin@example.com",
      displayName: "Example User",
      password: "correct horse battery staple",
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/auth/bootstrap",
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        body: JSON.stringify({
          token: "one-time",
          email: "admin@example.com",
          displayName: "Example User",
          password: "correct horse battery staple",
        }),
      }),
    );
  });

  it("sends an immutable-image deployment with idempotency and supported HTTP route", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ id: "op_1", kind: "deploy", state: "queued" }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(crypto, "randomUUID").mockReturnValue(
      "00000000-0000-4000-8000-000000000000",
    );

    await api.createDeployment({
      applicationId: "app_1",
      environmentId: "env_1",
      image: `ghcr.io/kuberploy/hello@sha256:${"a".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
      route: {
        hostname: "hello.example.com",
        pathPrefix: "/",
        tlsMode: "httpOnly",
      },
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/v1/deployments");
    expect(headers.get("Idempotency-Key")).toBe(
      "00000000-0000-4000-8000-000000000000",
    );
    expect(headers.get("X-CSRF-Token")).toBe("test-csrf");
    expect(JSON.parse(String(init.body))).toMatchObject({
      runtime: {
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
      route: {
        hostname: "hello.example.com",
        pathPrefix: "/",
        tlsMode: "httpOnly",
      },
    });
  });

  it("sends the current Git bundle ETag for an existing deployment update", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ id: "op_etag", kind: "deploy", state: "queued" }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    const etag = `"sha256:${"b".repeat(64)}"`;

    await api.createDeployment(
      {
        applicationId: "app_1",
        environmentId: "env_1",
        image: `ghcr.io/kuberploy/hello@sha256:${"c".repeat(64)}`,
        runtime: {
          replicas: 1,
          ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
          resources: { requests: { cpu: "50m", memory: "100Mi" } },
        },
      },
      "deployment-attempt-1",
      etag,
    );

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(init.headers).get("If-Match")).toBe(etag);
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe(
      "deployment-attempt-1",
    );
  });

  it("sends the current Git bundle ETag for a rollback intent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "op_rollback",
          kind: "deploy",
          state: "queued",
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const etag = `"sha256:${"d".repeat(64)}"`;

    await api.rollbackDeployment(
      "deployment-1",
      "11111111-1111-4111-8111-111111111111",
      "rollback-attempt-1",
      etag,
    );

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(headers.get("If-Match")).toBe(etag);
    expect(headers.get("Idempotency-Key")).toBe("rollback-attempt-1");
  });

  it("previews a tag through the closed image-resolution contract and strips authority metadata", async () => {
    const digest = `registry.example.test/payments/api@sha256:${"a".repeat(64)}`;
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          requestedImage: "registry.example.test/payments/api:release",
          immutableImage: digest,
          resolved: true,
          targetId: "target-secret",
          profileId: "profile-secret",
          realmUrl: "https://auth.example.test/token",
          authorization: "Bearer leaked",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const preview = await api.previewImageResolution(
      "environment-1",
      "application-1",
      "registry.example.test/payments/api:release",
    );

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/deployments/image-resolution-preview",
    );
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({
      environmentId: "environment-1",
      applicationId: "application-1",
      image: "registry.example.test/payments/api:release",
    });
    expect(preview).toEqual({
      requestedImage: "registry.example.test/payments/api:release",
      immutableImage: digest,
      resolved: true,
    });
    expect(preview).not.toHaveProperty("targetId");
    expect(preview).not.toHaveProperty("profileId");
    expect(preview).not.toHaveProperty("authorization");
  });

  it("rejects a contradictory image-resolution response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            requestedImage: "registry.example.test/payments/api:release",
            immutableImage: `registry.example.test/payments/api@sha256:${"b".repeat(64)}`,
            resolved: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(
      api.previewImageResolution(
        "environment-1",
        "application-1",
        "registry.example.test/payments/api:release",
      ),
    ).rejects.toThrow("image resolution response was invalid");
  });

  it("sends the closed sslip route intent without a caller hostname or IP", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ id: "op_1", kind: "deploy", state: "queued" }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.createDeployment({
      applicationId: "app_1",
      environmentId: "env_1",
      image: `ghcr.io/kuberploy/hello@sha256:${"a".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
      route: {
        dnsMode: "sslip",
        pathPrefix: "/",
        tlsMode: "httpOnly",
      },
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const body = JSON.parse(String(init.body));
    expect(body.route).toEqual({
      dnsMode: "sslip",
      pathPrefix: "/",
      tlsMode: "httpOnly",
    });
    expect(body.route).not.toHaveProperty("hostname");
    expect(body.route).not.toHaveProperty("ip");
  });

  it("binds a tagged deployment to the previewed immutable image", async () => {
    const expectedImmutableImage = `registry.example.test/payments/api@sha256:${"e".repeat(64)}`;
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ id: "op_tag", kind: "deploy", state: "queued" }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.createDeployment({
      applicationId: "app_1",
      environmentId: "env_1",
      image: "registry.example.test/payments/api:release",
      expectedImmutableImage,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    });

    expect(
      JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body)),
    ).toMatchObject({
      image: "registry.example.test/payments/api:release",
      expectedImmutableImage,
    });
  });

  it("rejects missing or contradictory tag preview preconditions before fetch", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const runtime = {
      replicas: 1,
      ports: [{ name: "http", containerPort: 3000, protocol: "TCP" as const }],
      resources: { requests: { cpu: "50m", memory: "100Mi" } },
    };

    expect(() =>
      api.createDeployment({
        applicationId: "app_1",
        environmentId: "env_1",
        image: "registry.example.test/payments/api:release",
        runtime,
      }),
    ).toThrow("tag requires its previewed immutable-image precondition");
    expect(() =>
      api.createDeployment({
        applicationId: "app_1",
        environmentId: "env_1",
        image: `registry.example.test/payments/api@sha256:${"f".repeat(64)}`,
        expectedImmutableImage: `registry.example.test/payments/api@sha256:${"f".repeat(64)}`,
        runtime,
      }),
    ).toThrow("immutable image forbids it");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("uses the validate, ETag-bound preview, and one-time-token save contracts", async () => {
    const operation: OperationWire = {
      id: "01900000-0000-7000-8000-000000000070",
      kind: "deployment.git-write",
      status: "queued",
      targetType: "deployment",
      targetId: "01900000-0000-7000-8000-000000000071",
      requestId: "request-config",
      generation: 2,
      progress: [{ name: "git-write", status: "pending" }],
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ valid: true, diagnostics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            previewToken: "p".repeat(43),
            gitDiff: "+replicas: 2",
            renderedDiff: "+replicas: 2",
            renderIdentity: {
              contract: "appconfig-rendered-preview.v1",
              chartName: "kuberploy-runtime",
              chartVersion: "1.2.3",
              chartDigest: `sha256:${"a".repeat(64)}`,
              rendererImage: `docker.io/alpine/helm:4.2.3@sha256:${"b".repeat(64)}`,
              rendererVersion: "4.2.3",
              policyVersion: "external-helm-p0.v1",
            },
            renderIdentityDigest: `sha256:${"c".repeat(64)}`,
            semanticChanges: [],
            warnings: [],
            expiresAt: "2026-08-09T00:10:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(operation), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(crypto, "randomUUID").mockReturnValue(
      "00000000-0000-4000-8000-000000000072",
    );
    document.cookie = "kuberploy_csrf=config-csrf; path=/";
    const change = {
      mode: "jsonPatch" as const,
      patch: [
        { op: "replace" as const, path: "/spec/runtime/replicas", value: 2 },
      ],
    };
    const etag = `"cfg-sha256-${"a".repeat(64)}"`;
    await api.validateDeploymentConfig("deployment/id", change);
    const preview = await api.previewDeploymentConfig(
      "deployment/id",
      change,
      etag,
    );
    await api.saveDeploymentConfig(
      "deployment/id",
      change,
      etag,
      preview.previewToken,
      "00000000-0000-4000-8000-000000000072",
    );

    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "/v1/deployments/deployment%2Fid/config/validate",
      "/v1/deployments/deployment%2Fid/config/preview",
      "/v1/deployments/deployment%2Fid/config",
    ]);
    const validateHeaders = new Headers(
      (fetchMock.mock.calls[0]?.[1] as RequestInit).headers,
    );
    const previewHeaders = new Headers(
      (fetchMock.mock.calls[1]?.[1] as RequestInit).headers,
    );
    const saveHeaders = new Headers(
      (fetchMock.mock.calls[2]?.[1] as RequestInit).headers,
    );
    expect(validateHeaders.get("X-CSRF-Token")).toBe("config-csrf");
    expect(previewHeaders.get("If-Match")).toBe(etag);
    expect(saveHeaders.get("If-Match")).toBe(etag);
    expect(saveHeaders.get("Preview-Token")).toBe("p".repeat(43));
    expect(saveHeaders.get("Idempotency-Key")).toBe(
      "00000000-0000-4000-8000-000000000072",
    );
  });

  it("normalizes array and cursor collection responses", () => {
    expect(asCollection([{ id: "one" }])).toEqual({ items: [{ id: "one" }] });
    expect(
      asCollection({ items: [{ id: "two" }], nextCursor: "next" }),
    ).toEqual({
      items: [{ id: "two" }],
      nextCursor: "next",
    });
    expect(asCollection({ items: null })).toEqual({ items: [] });
  });

  it("builds only the named scoped metrics query contract", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          metric: "cpu-usage",
          scope: "service",
          series: [],
          observedAt: "2026-08-09T00:05:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.metricRange({
      scopeType: "service",
      scopeId: "deployment/id",
      metric: "cpu-usage",
      from: new Date("2026-08-09T00:00:00Z"),
      to: new Date("2026-08-09T00:05:00Z"),
      stepSeconds: 60,
    });

    const target = new URL(
      String(fetchMock.mock.calls[0]?.[0]),
      "https://kuberploy.example.test",
    );
    expect(target.pathname).toBe("/v1/metrics/query-range");
    expect(Object.fromEntries(target.searchParams)).toEqual({
      scopeType: "service",
      scopeId: "deployment/id",
      metric: "cpu-usage",
      from: "2026-08-09T00:00:00.000Z",
      to: "2026-08-09T00:05:00.000Z",
      step: "60s",
    });
    expect(target.searchParams.has("query")).toBe(false);
  });

  it("builds only bounded workload log and event snapshot queries", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [],
            sources: [],
            bytes: 0,
            truncated: false,
            observedAt: "2026-08-09T00:05:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [],
            truncated: false,
            observedAt: "2026-08-09T00:05:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await api.workloadLogs("deployment/id", {
      pod: "payments-api-7d9f7b8c9-x2k4m",
      revision: "42",
      container: "application",
      tailLines: 200,
      since: "2026-08-09T00:00:00Z",
      previous: true,
      limitBytes: 1_048_576,
    });
    await api.workloadEvents("deployment/id", { limit: 50 });

    const logTarget = new URL(
      String(fetchMock.mock.calls[0]?.[0]),
      "https://kuberploy.example.test",
    );
    expect(logTarget.pathname).toBe("/v1/workloads/deployment%2Fid/logs");
    expect(Object.fromEntries(logTarget.searchParams)).toEqual({
      pod: "payments-api-7d9f7b8c9-x2k4m",
      revision: "42",
      container: "application",
      tailLines: "200",
      since: "2026-08-09T00:00:00Z",
      previous: "true",
      limitBytes: "1048576",
    });
    expect(logTarget.searchParams.has("namespace")).toBe(false);
    expect(logTarget.searchParams.has("podId")).toBe(false);
    expect(logTarget.searchParams.has("selector")).toBe(false);

    const eventTarget = new URL(
      String(fetchMock.mock.calls[1]?.[0]),
      "https://kuberploy.example.test",
    );
    expect(eventTarget.pathname).toBe("/v1/workloads/deployment%2Fid/events");
    expect(Object.fromEntries(eventTarget.searchParams)).toEqual({
      limit: "50",
    });
  });

  it("sends optional team ownership without inventing a platform team", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "project_team",
            name: "Payments",
            slug: "payments",
            teamId: "team_payments",
            createdAt: "2026-08-06T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "project_platform",
            name: "Platform",
            slug: "platform",
            createdAt: "2026-08-06T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(crypto, "randomUUID")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000041")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000042");

    await api.createProject({
      name: "Payments",
      slug: "payments",
      teamId: "team_payments",
    });
    await api.createProject({ name: "Platform", slug: "platform" });

    const teamRequest = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const platformRequest = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(teamRequest.body))).toEqual({
      name: "Payments",
      slug: "payments",
      teamId: "team_payments",
    });
    expect(JSON.parse(String(platformRequest.body))).toEqual({
      name: "Platform",
      slug: "platform",
    });
  });

  it("normalizes the literal backend operation and deployment contracts", () => {
    const operationFixture: OperationWire = {
      id: "01900000-0000-7000-8000-000000000010",
      kind: "deployment.git-write",
      status: "running",
      targetType: "deployment",
      targetId: "01900000-0000-7000-8000-000000000011",
      requestId: "req_1",
      generation: 1,
      progress: [
        {
          name: "git-write",
          status: "running",
          detail: "preparing Git commit",
          startedAt: "2026-08-06T00:00:00Z",
        },
      ],
      createdAt: "2026-08-06T00:00:00Z",
      updatedAt: "2026-08-06T00:00:01Z",
    };
    const operation = normalizeOperation(operationFixture);
    expect(operation.state).toBe("running");
    expect(operation.target).toEqual({
      id: operationFixture.targetId,
      type: "deployment",
    });
    expect(operation.steps).toEqual([
      expect.objectContaining({
        name: "git-write",
        state: "running",
        message: "preparing Git commit",
      }),
    ]);

    const protectedOperation = normalizeOperation({
      ...operationFixture,
      pullRequest: {
        number: 42,
        url: "https://github.com/acme/platform/pull/42",
        state: "open",
        candidateRevision: "b".repeat(40),
      },
    });
    expect(protectedOperation.pullRequest).toEqual({
      number: 42,
      url: "https://github.com/acme/platform/pull/42",
      state: "open",
      candidateRevision: "b".repeat(40),
    });

    for (const pullRequest of [
      {
        number: 42,
        url: "https://github.com/acme/platform/pull/41",
        state: "open" as const,
        candidateRevision: "b".repeat(40),
      },
      {
        number: 42,
        url: "https://github.com/acme/platform/pull/42?redirect=evil",
        state: "open" as const,
        candidateRevision: "b".repeat(40),
      },
      {
        number: 42,
        url: "https://github.example/acme/platform/pull/42",
        state: "open" as const,
        candidateRevision: "b".repeat(40),
      },
      {
        number: 42,
        url: "https://github.com/acme/platform/pull/42",
        state: "open" as const,
        candidateRevision: "candidate-not-a-commit",
      },
    ]) {
      expect(
        normalizeOperation({ ...operationFixture, pullRequest }).pullRequest,
      ).toBeUndefined();
    }

    const deployment = normalizeDeployment({
      id: "01900000-0000-7000-8000-000000000011",
      environmentId: "01900000-0000-7000-8000-000000000012",
      applicationId: "01900000-0000-7000-8000-000000000013",
      image: `ghcr.io/kuberploy/hello@sha256:${"a".repeat(64)}`,
      replicas: 1,
      port: 8080,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
      state: "gitPending",
      operationId: operationFixture.id,
      desiredRevision: "abc123",
      observedRevision: "abc122",
      createdAt: "2026-08-06T00:00:00Z",
      updatedAt: "2026-08-06T00:00:01Z",
    });
    expect(deployment.status).toBe("gitPending");
    expect(deployment.configRevision).toBe("abc123");
    expect(deployment.observedRevision).toBe("abc122");
  });

  it("surfaces RFC 9457 details as an ApiError", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            title: "Precondition failed",
            detail: "The Git projection changed.",
            code: "StaleRevision",
          }),
          {
            status: 412,
            headers: { "Content-Type": "application/problem+json" },
          },
        ),
      ),
    );

    await expect(api.deployment("dep_1")).rejects.toEqual(
      expect.objectContaining({
        name: "ApiError",
        status: 412,
        message: "The Git projection changed.",
      }),
    );
  });

  it("surfaces path-level config validation diagnostics without discarding them", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            valid: false,
            diagnostics: [
              {
                code: "LockedField",
                pointer: "/spec/delivery",
                detail: "This field is release-owned.",
              },
            ],
          }),
          { status: 422, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(
      api.previewDeploymentConfig(
        "deployment-1",
        {
          mode: "jsonPatch",
          patch: [{ op: "replace", path: "/spec/delivery", value: {} }],
        },
        `"cfg-sha256-${"a".repeat(64)}"`,
      ),
    ).rejects.toEqual(
      expect.objectContaining({
        status: 422,
        message: "/spec/delivery: This field is release-owned.",
        problem: expect.objectContaining({
          errors: [expect.objectContaining({ code: "LockedField" })],
        }),
      }),
    );
  });
});
