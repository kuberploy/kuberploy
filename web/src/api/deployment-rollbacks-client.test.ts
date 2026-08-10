import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

function response(value: unknown) {
  return new Response(JSON.stringify(value), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("deployment rollback API client", () => {
  it("sends only exact source identity and strips private history fields", async () => {
    const sourceOperationId = "11111111-1111-4111-8111-111111111111";
    const candidate = {
      sourceOperationId,
      generation: 4,
      image: `registry.example.test/team/api@sha256:${"a".repeat(64)}`,
      artifactAssurance: "managed-release-verified" as const,
      managedReleaseVerified: true,
      createdAt: "2026-08-09T00:00:00Z",
      rawYaml: "must-not-cross-client-boundary",
      registryPull: { credential: "must-not-cross" },
    };
    const operation = {
      id: "22222222-2222-4222-8222-222222222222",
      kind: "deployment.git-write",
      status: "queued",
      targetType: "deployment",
      targetId: "33333333-3333-4333-8333-333333333333",
      requestId: "request",
      generation: 5,
      progress: [],
      createdAt: "2026-08-09T00:01:00Z",
      updatedAt: "2026-08-09T00:01:00Z",
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(response({ items: [candidate] }))
      .mockResolvedValueOnce(response(operation));
    vi.stubGlobal("fetch", fetchMock);

    const catalog = await api.deploymentRollbackSources("deployment/id", 500);
    const accepted = await api.rollbackDeployment(
      "deployment/id",
      sourceOperationId,
      "deployment-rollback-stable-key",
    );

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/deployments/deployment%2Fid/rollback-sources?limit=100",
      "/v1/deployments/deployment%2Fid/rollback",
    ]);
    const mutation = fetchMock.mock.calls[1][1] as RequestInit;
    expect(new Headers(mutation.headers).get("Idempotency-Key")).toBe(
      "deployment-rollback-stable-key",
    );
    expect(JSON.parse(String(mutation.body))).toEqual({ sourceOperationId });
    expect(JSON.stringify(catalog)).not.toMatch(
      /rawYaml|registryPull|credential|must-not-cross/,
    );
    expect(accepted.id).toBe(operation.id);
  });

  it("drops candidates whose assurance fields contradict each other", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        response({
          items: [
            {
              sourceOperationId: "11111111-1111-4111-8111-111111111111",
              generation: 1,
              image: `registry.example.test/team/api@sha256:${"a".repeat(64)}`,
              artifactAssurance: "external-digest-unverified",
              managedReleaseVerified: true,
              createdAt: "2026-08-09T00:00:00Z",
            },
          ],
        }),
      ),
    );
    await expect(
      api.deploymentRollbackSources("deployment", 10),
    ).resolves.toEqual({ items: [] });
  });
});
