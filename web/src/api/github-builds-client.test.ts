import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";
import type {
  BuildAttempt,
  BuildDefinition,
  CreateBuildDefinition,
} from "./types";

afterEach(() => vi.unstubAllGlobals());

const definition: BuildDefinition = {
  sourceKind: "github",
  id: "definition-safe",
  projectId: "project-safe",
  applicationId: "application/id",
  installationId: "installation/id",
  repositoryId: "repository/id",
  triggerRef: "refs/heads/main",
  contextPath: ".",
  dockerfilePath: "Dockerfile",
  platforms: ["linux/amd64", "linux/arm64"],
  registry: {
    targetId: "target/id",
    mode: "managed",
    server: "registry.example.test",
    repositoryPrefix: "tenant",
  },
  buildArgs: [{ name: "APP_ENV", value: "production" }],
  secretFiles: [{ id: "npmrc", path: "/run/secrets/npmrc" }],
  sshFiles: [{ id: "git", path: "/run/secrets/git" }],
  cacheTrustLane: "protected",
  cacheImports: 2,
  profile: {
    resource: "small",
    timeoutSeconds: 900,
    egress: "github-registry",
  },
  maxAttempts: 3,
  sourceDigest: `sha256:${"a".repeat(64)}`,
  sourceRevision: 1,
  enabled: true,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

const attempt: BuildAttempt = {
  id: "attempt-safe",
  sourceId: definition.id,
  projectId: definition.projectId,
  applicationId: definition.applicationId,
  commitSha: "b".repeat(40),
  gitRef: "refs/heads/main",
  generation: 7,
  state: "succeeded",
  executionAttempts: 1,
  maxAttempts: 3,
  image: {
    reference: "registry.example.test/tenant/api:build-7",
    digest: `sha256:${"c".repeat(64)}`,
    platforms: ["linux/amd64", "linux/arm64"],
  },
  cacheReuse: "hit",
  warnings: ["ColdBuild"],
  cacheReference: "registry.example.test/tenant/api:cache-generation-7",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

describe("GitHub source-build API client", () => {
  it("promotes with only environment, runtime, and closed route intent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "operation-promote",
          kind: "deployment.git-write",
          status: "queued",
          targetType: "deployment",
          targetId: "deployment-promote",
          requestId: "request-promote",
          generation: 1,
          progress: [{ name: "git-write", status: "pending" }],
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=promotion-csrf; path=/";
    const input = {
      environmentId: "environment/id",
      runtime: {
        replicas: 1,
        ports: [
          { name: "http", containerPort: 8080, protocol: "TCP" as const },
        ],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
      route: {
        dnsMode: "sslip" as const,
        pathPrefix: "/" as const,
        tlsMode: "httpOnly" as const,
      },
    };
    const operation = await api.promoteBuildAttempt(
      "attempt/id",
      input,
      "promotion-key-0001",
      '"sha256:' + "a".repeat(64) + '"',
    );
    expect(operation.id).toBe("operation-promote");
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/builds/attempt%2Fid/promote",
    );
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(headers.get("Idempotency-Key")).toBe("promotion-key-0001");
    expect(headers.get("If-Match")).toBe('"sha256:' + "a".repeat(64) + '"');
    expect(JSON.parse(String(init.body))).toEqual(input);
    expect(String(init.body)).not.toMatch(
      /applicationId|projectId|image|releaseId|registryTargetId|repository|digest|namespace/,
    );
  });

  it("projects adversarial catalogs and build responses to exact safe fields", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                id: "installation/id",
                githubInstallationId: 42,
                accountLogin: "example",
                accountType: "Organization",
                ownerUserId: "user-safe",
                visibility: "private",
                repositorySelection: "selected",
                repositoryCount: 1,
                createdAt: "2026-08-09T00:00:00Z",
                updatedAt: "2026-08-09T00:00:00Z",
                privateKey: "installation-private-key-leak",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                id: "repository/id",
                githubRepositoryId: 84,
                installationId: "installation/id",
                ownerId: 21,
                ownerLogin: "example",
                name: "api",
                lifecycle: "active",
                checkoutToken: "repository-token-leak",
              },
              {
                id: "repository-cross-scope",
                githubRepositoryId: 85,
                installationId: "other-installation",
                ownerId: 21,
                ownerLogin: "example",
                name: "private",
                lifecycle: "active",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            ...definition,
            registryCredential: "registry-credential-leak",
            execution: { serviceAccount: "private-builder" },
            profile: { ...definition.profile, token: "profile-token-leak" },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                ...attempt,
                plan: { checkoutUrl: "private-checkout-url" },
                logs: ["raw-build-log-leak"],
                sourceToken: "source-token-leak",
                image: { ...attempt.image, registryPassword: "password-leak" },
              },
              { ...attempt, id: "cross-scope", applicationId: "other-app" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const installations = await api.githubInstallations();
    const repositories =
      await api.githubInstallationRepositories("installation/id");
    const definitions = await api.buildDefinitions("application/id");
    const attempts = await api.buildAttempts("application/id", 25);

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/github/installations",
      "/v1/github/installations/installation%2Fid/repositories",
      "/v1/applications/application%2Fid/source",
      "/v1/applications/application%2Fid/builds?limit=25",
    ]);
    expect(repositories.items).toHaveLength(1);
    expect(definitions.items).toHaveLength(1);
    expect(attempts.items).toHaveLength(1);
    expect(definitions.items[0]?.buildArgs).toEqual([
      { name: "APP_ENV", value: "" },
    ]);
    expect(attempts.items[0]?.cacheReuse).toBe("hit");
    expect(
      JSON.stringify({ installations, repositories, definitions, attempts }),
    ).not.toMatch(
      /privateKey|checkoutToken|registryCredential|serviceAccount|profile-token|checkoutUrl|raw-build-log|sourceToken|registryPassword|password-leak|token-leak/,
    );
  });

  it("drops an unknown cache reuse value instead of expanding the closed projection", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(
            JSON.stringify({ ...attempt, cacheReuse: "raw-buildkit-output" }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        ),
    );

    expect((await api.buildAttempt(attempt.id)).cacheReuse).toBeUndefined();
  });

  it("uses caller-stable keys and sends only the closed mutation schema", async () => {
    const state = "s".repeat(64);
    const destination = `https://github.com/apps/kuberploy/installations/new?state=${state}`;
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            authorizationUrl: destination,
            state,
            expiresAt: "2026-08-09T00:05:00Z",
            providerToken: "must-not-be-returned",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            installation: {
              id: "installation/id",
              githubInstallationId: 42,
              accountLogin: "example",
              accountType: "Organization",
              ownerUserId: "user-safe",
              visibility: "private",
              repositorySelection: "selected",
              repositoryCount: 1,
              createdAt: "2026-08-09T00:00:00Z",
              updatedAt: "2026-08-09T00:00:00Z",
              privateKey: "must-not-be-cached",
            },
            repositories: [
              {
                id: "repository/id",
                githubRepositoryId: 84,
                installationId: "installation/id",
                ownerId: 21,
                ownerLogin: "example",
                name: "api",
                lifecycle: "active",
                checkoutToken: "must-not-be-cached",
              },
            ],
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(definition), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ ...attempt, state: "cancelling" }), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            ...attempt,
            id: "retry-safe",
            generation: 8,
            state: "queued",
          }),
          { status: 202, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=github-build-csrf; path=/";

    const setupDestination = await api.beginGitHubSetup(
      { returnKey: "source-builds" },
      "github-setup-key-0001",
    );
    const linked = await api.completeGitHubSetup("github-link-key-0001");
    const createInput = {
      installationId: definition.installationId,
      repositoryId: definition.repositoryId,
      registryTargetId: definition.registry.targetId,
      triggerRef: definition.triggerRef,
      contextPath: definition.contextPath,
      dockerfilePath: definition.dockerfilePath,
      platforms: definition.platforms,
      buildArgs: definition.buildArgs,
      secretFiles: definition.secretFiles,
      sshFiles: definition.sshFiles,
      cacheTrustLane: definition.cacheTrustLane,
      cacheImports: definition.cacheImports,
      profile: definition.profile,
      maxAttempts: definition.maxAttempts,
      sourceToken: "caller-injected-token",
      execution: { namespace: "caller-injected" },
    } as CreateBuildDefinition & Record<string, unknown>;
    await api.createBuildDefinition(
      "application/id",
      createInput,
      "build-create-key-0001",
    );
    await api.cancelBuildAttempt("attempt/id", "build-cancel-key-0001");
    await api.retryBuildAttempt("attempt/id", "build-retry-key-0001");

    expect(setupDestination).toBe(destination);
    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/github/installations/authorize",
      "/v1/github/installations/link",
      "/v1/applications/application%2Fid/source",
      "/v1/builds/attempt%2Fid/cancel",
      "/v1/builds/attempt%2Fid/retry",
    ]);
    expect(
      fetchMock.mock.calls.map((call) =>
        new Headers((call[1] as RequestInit).headers).get("Idempotency-Key"),
      ),
    ).toEqual([
      "github-setup-key-0001",
      "github-link-key-0001",
      "build-create-key-0001",
      "build-cancel-key-0001",
      "build-retry-key-0001",
    ]);
    const createBody = JSON.parse(
      String((fetchMock.mock.calls[2]?.[1] as RequestInit).body),
    ) as Record<string, unknown>;
    expect(createBody).not.toHaveProperty("secretFiles");
    expect(createBody).not.toHaveProperty("sshFiles");
    expect(createBody).not.toHaveProperty("sourceToken");
    expect(createBody).not.toHaveProperty("execution");
    expect((fetchMock.mock.calls[3]?.[1] as RequestInit).body).toBeUndefined();
    expect((fetchMock.mock.calls[1]?.[1] as RequestInit).body).toBeUndefined();
    expect((fetchMock.mock.calls[4]?.[1] as RequestInit).body).toBeUndefined();
    expect(JSON.stringify(linked)).not.toMatch(/privateKey|checkoutToken/);
  });

  it("builds only the bounded opaque source-build log contract", async () => {
    const observedAt = "2026-08-09T00:05:00Z";
    const source = {
      id: `build_${"a".repeat(32)}`,
      ready: true,
      previous: true,
      namespace: "must-not-survive",
      podName: "must-not-survive",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          source,
          lines: [
            {
              type: "line",
              timestamp: observedAt,
              source,
              message: "safe output",
              truncated: false,
              cursor: {
                sourceId: source.id,
                timestamp: observedAt,
                fingerprint: "b".repeat(64),
                podUID: "must-not-survive",
              },
              container: "must-not-survive",
            },
          ],
          bytes: 11,
          truncated: false,
          observedAt,
          logReference: "must-not-survive",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const snapshot = await api.buildLogSnapshot("attempt/id", {
      tailLines: 25,
      since: observedAt,
      previous: true,
      limitBytes: 4096,
    });
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/builds/attempt%2Fid/logs?follow=false&tailLines=25&limitBytes=4096&since=2026-08-09T00%3A05%3A00Z&previous=true",
    );
    expect(JSON.stringify(snapshot)).not.toMatch(
      /namespace|podName|podUID|container|logReference|must-not-survive/,
    );

    const streamURL = api.buildLogStreamURL("attempt/id", {
      tailLines: 50_000,
      limitBytes: -1,
      previous: true,
      since: observedAt,
    });
    expect(streamURL).toBe(
      "/v1/builds/attempt%2Fid/logs?follow=true&tailLines=2000&limitBytes=1&since=2026-08-09T00%3A05%3A00Z",
    );
  });

  it("rejects a provider redirect outside the exact GitHub App install path", async () => {
    const state = "s".repeat(64);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            authorizationUrl: `https://evil.example.test/steal?state=${state}`,
            state,
            expiresAt: "2026-08-09T00:05:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(
      api.beginGitHubSetup(
        { returnKey: "source-builds" },
        "github-setup-key-0002",
      ),
    ).rejects.toEqual(
      expect.objectContaining({
        status: 502,
        message: expect.not.stringContaining("evil.example.test"),
      }),
    );
  });

  it("accepts only the exact same-origin existing-installation continuation", async () => {
    const state = "s".repeat(64);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            authorizationUrl: `${window.location.origin}/v1/github/installations/setup?installation_id=152576900&setup_action=update&state=${state}`,
            state,
            expiresAt: "2026-08-09T00:05:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(
      api.beginGitHubSetup(
        { returnKey: "source-builds", existingInstallationId: 152576900 },
        "github-setup-key-existing-0001",
      ),
    ).resolves.toContain("/v1/github/installations/setup?");
    expect(
      JSON.parse(String(vi.mocked(fetch).mock.calls[0]?.[1]?.body)),
    ).toEqual({
      returnKey: "source-builds",
      existingInstallationId: 152576900,
    });
  });
});
