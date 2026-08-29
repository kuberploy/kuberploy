import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const target = {
  id: "target/id",
  name: "Primary",
  mode: "managed" as const,
  endpoint: "registry.example.test",
  repositoryPrefix: "tenant",
  pullCredentialRef: "credentials/pull",
  pushCredentialRef: "credentials/push",
  cacheCredentialRef: "credentials/cache",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const policy = {
  registryTargetId: target.id,
  serviceId: "application/id",
  repository: "tenant/service",
  keepLastSuccessful: 10,
  minimumSafetyAgeSeconds: 86_400,
  cacheUnusedExpirySeconds: 604_800,
  cacheByteQuota: 10_737_418_240,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const plan = {
  id: "plan/id",
  registryTargetId: target.id,
  serviceId: "application/id",
  planDigest: `sha256:${"a".repeat(64)}`,
  state: "preview",
  policy,
  summary: {
    protectedManifests: 1,
    deletedManifests: 1,
    garbageCollectBlobs: 1,
    estimatedBytes: 100,
    cacheBytesBefore: 200,
    cacheBytesAfter: 100,
    cacheQuotaSatisfied: true,
  },
  items: [
    {
      ordinal: 0,
      repository: "tenant/service",
      resourceKind: "release-manifest",
      digest: `sha256:${"b".repeat(64)}`,
      disposition: "delete" as const,
      action: "delete-manifest",
      estimatedBytes: 10,
      reasons: ["retention-eligible"],
      state: "planned",
      updatedAt: "2026-08-09T00:05:00Z",
      providerPassword: "nested-plan-leak",
    },
  ],
  itemsTruncated: false,
  createdAt: "2026-08-09T00:05:00Z",
  snapshotToken: "private-snapshot-token",
  authorityToken: "private-authority-token",
  deleteCredential: "must-never-be-cached",
};

describe("registry API client", () => {
  it("projects adversarial responses to bounded metadata and exact scope", async () => {
    const inventory = {
      target: {
        ...target,
        password: "target-response-leak",
        credentials: { username: "private" },
      },
      policy: {
        ...policy,
        providerToken: "policy-response-leak",
      },
      inventory: {
        revision: "inventory-1",
        complete: true,
        repositories: Array.from(
          { length: 101 },
          (_, index) => `tenant/service-${index}`,
        ),
        repositoriesTruncated: false,
        observedAt: "2026-08-09T00:05:00Z",
        credentials: "inventory-response-leak",
      },
      catalogObservations: [],
      catalogTruncated: false,
      releases: Array.from({ length: 101 }, (_, index) => ({
        id: "release-1",
        repository: `tenant/service-${index}`,
        rootDigest: `sha256:${"c".repeat(64)}`,
        createdAt: "2026-08-09T00:00:00Z",
        availability: "present",
        manifest: "release-response-leak",
      })),
      releasesTruncated: false,
      cacheGenerations: [],
      cacheGenerationsTruncated: false,
      observedAt: "2026-08-09T00:05:00Z",
      manifests: [{ digest: "graph-response-leak" }],
      references: [{ digest: "reference-response-leak" }],
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: Array.from({ length: 101 }, (_, index) => ({
              ...target,
              id: `target-${index}`,
              password: "target-list-leak",
            })),
            truncated: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              inventory,
              {
                ...inventory,
                policy: { ...policy, serviceId: "other-application" },
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(plan), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    const targets = await api.registryTargets(25);
    const application = await api.applicationRegistry("application/id", 25);
    const cleanup = await api.registryCleanupPlan("plan/id");

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/registry-targets?limit=25",
      "/v1/applications/application%2Fid/registry?limit=25",
      "/v1/registry-cleanup-plans/plan%2Fid",
    ]);
    expect(targets.items).toHaveLength(25);
    expect(targets.truncated).toBe(true);
    expect(application.items).toHaveLength(1);
    expect(application.items[0]?.inventory?.repositories).toHaveLength(25);
    expect(application.items[0]?.inventory?.repositoriesTruncated).toBe(true);
    expect(application.items[0]?.releases).toHaveLength(25);
    expect(application.items[0]?.releasesTruncated).toBe(true);
    expect(JSON.stringify({ targets, application, cleanup })).not.toMatch(
      /password|"credentials":|providerToken|snapshotToken|authorityToken|deleteCredential|response-leak|private-snapshot|private-authority|"manifests":|"references":/,
    );
  });

  it("uses caller-stable idempotency for target, policy, preview, and execute mutations", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(target), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(target), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(policy), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(plan), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ ...plan, state: "succeeded" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=registry-csrf; path=/";
    const targetInput = {
      name: "Primary",
      mode: "managed" as const,
      endpoint: "registry.example.test",
      repositoryPrefix: "tenant",
      pullCredentialRef: "credentials/pull",
    };
    const policyInput = {
      repository: "tenant/service",
      keepLastSuccessful: 10,
      minimumSafetyAgeSeconds: 86_400,
      cacheUnusedExpirySeconds: 604_800,
      cacheByteQuota: 10_737_418_240,
    };

    await api.createRegistryTarget(targetInput, "target-key-stable");
    await api.updateRegistryTarget(
      "target/id",
      targetInput,
      "target-update-key-stable",
    );
    await api.putRegistryPolicy(
      "application/id",
      "target/id",
      policyInput,
      "policy-key-stable",
    );
    await api.previewRegistryCleanup(
      "application/id",
      "target/id",
      "preview-key-stable",
    );
    await api.executeRegistryCleanup(
      "plan/id",
      "plan/id",
      "execute-key-stable",
    );

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/registry-targets",
      "/v1/registry-targets/target%2Fid",
      "/v1/applications/application%2Fid/registry/policies/target%2Fid",
      "/v1/applications/application%2Fid/registry/cleanup-previews",
      "/v1/registry-cleanup-plans/plan%2Fid/executions",
    ]);
    expect(
      fetchMock.mock.calls.map((call) =>
        new Headers((call[1] as RequestInit).headers).get("Idempotency-Key"),
      ),
    ).toEqual([
      "target-key-stable",
      "target-update-key-stable",
      "policy-key-stable",
      "preview-key-stable",
      "execute-key-stable",
    ]);
    expect(
      fetchMock.mock.calls.map((call) =>
        new Headers((call[1] as RequestInit).headers).get("X-CSRF-Token"),
      ),
    ).toEqual(Array(5).fill("registry-csrf"));
    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body))).toEqual(
      policyInput,
    );
    expect(JSON.parse(String(fetchMock.mock.calls[3]?.[1]?.body))).toEqual({
      targetId: "target/id",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[4]?.[1]?.body))).toEqual({
      confirmation: "plan/id",
    });
  });

  it("projects project pull credentials without provider or Secret metadata", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          items: [
            {
              id: "11111111-1111-4111-8111-111111111111",
              projectId: "22222222-2222-4222-8222-222222222222",
              registryTargetId: "33333333-3333-4333-8333-333333333333",
              name: "Production",
              registryName: "Primary",
              registryServer: "registry.example.test",
              repositoryPrefix: "team",
              createdAt: "2026-08-10T00:00:00Z",
              updatedAt: "2026-08-10T00:00:00Z",
              pullCredentialRef: "must-not-survive",
              password: "must-not-survive",
            },
          ],
          availableTargets: [
            {
              id: "33333333-3333-4333-8333-333333333333",
              name: "Primary",
              server: "registry.example.test",
              repositoryPrefix: "team",
              sourceSecretRef: "must-not-survive",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const result = await api.projectRegistryPullCredentials("project/id");
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/projects/project%2Fid/registry-pull-credentials",
      expect.anything(),
    );
    expect(JSON.stringify(result)).not.toMatch(
      /pullCredentialRef|sourceSecretRef|password|must-not-survive/,
    );
    expect(result.items[0]?.name).toBe("Production");
  });

  it("sends the caller-stable idempotency key for pull-credential deletion", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await api.deleteProjectRegistryPullCredential(
      "project/id",
      "credential/id",
      "delete-pull-credential-0001",
    );

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(
      "/v1/projects/project%2Fid/registry-pull-credentials/credential%2Fid",
    );
    expect(init.method).toBe("DELETE");
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe(
      "delete-pull-credential-0001",
    );
  });
});
