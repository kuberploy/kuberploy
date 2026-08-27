import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const revision = {
  id: "22222222-2222-4222-8222-222222222222",
  generation: 7,
  releaseName: "valkey",
  action: "deploy" as const,
  desiredEnabled: true,
  source: {
    kind: "git" as const,
    repositoryUrl: "https://github.com/valkey-io/valkey-helm.git",
    targetRevision: "main",
    path: "valkey",
  },
  valuesYaml: "replicaCount: 1\n",
  valuesDigest: `sha256:${"f".repeat(64)}`,
  state: "applied" as const,
  requestId: "request-safe",
  createdAt: "2026-08-25T00:00:00Z",
  updatedAt: "2026-08-25T00:00:01Z",
};

function jsonResponse(value: unknown, replay = false) {
  return new Response(JSON.stringify(value), {
    status: 200,
    headers: {
      "Content-Type": "application/json",
      "Idempotent-Replay": replay ? "true" : "false",
    },
  });
}

describe("direct Argo Helm App API client", () => {
  it("uses direct source and values for deploy, history, retry, disable, and rollback", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(revision))
      .mockResolvedValueOnce(jsonResponse({ items: [revision] }))
      .mockResolvedValueOnce(jsonResponse(revision, true))
      .mockResolvedValueOnce(jsonResponse({ ...revision, action: "retry" }))
      .mockResolvedValueOnce(
        jsonResponse({ ...revision, action: "disable", desiredEnabled: false }),
      )
      .mockResolvedValueOnce(jsonResponse({ ...revision, action: "rollback" }));
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=helm-csrf; path=/";
    const input = { source: revision.source, valuesYaml: revision.valuesYaml };

    await api.helmRelease("application/id", "environment/id");
    await api.helmReleaseHistory("application/id", "environment/id", 9);
    const deploy = await api.upsertHelmRelease(
      "application/id",
      "environment/id",
      input,
      "helm-upsert-stable-key",
    );
    await api.retryHelmRelease(
      "application/id",
      "environment/id",
      "helm-retry-stable-key",
    );
    await api.disableHelmRelease(
      "application/id",
      "environment/id",
      "helm-disable-stable-key",
    );
    await api.rollbackHelmRelease(
      "application/id",
      "environment/id",
      revision.id,
      "helm-rollback-stable-key",
    );

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/releases?limit=9",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/retry",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/disable",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/rollback",
    ]);
    expect(
      JSON.parse(String((fetchMock.mock.calls[2]?.[1] as RequestInit).body)),
    ).toEqual(input);
    expect(deploy.replayed).toBe(true);
  });

  it("rejects oversized values before fetch", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    expect(() =>
      api.upsertHelmRelease(
        "app",
        "env",
        { source: revision.source, valuesYaml: "é".repeat(131_073) },
        "stable-idempotency-key",
      ),
    ).toThrow(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
