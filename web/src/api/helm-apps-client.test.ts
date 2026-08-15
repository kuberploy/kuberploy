import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const approval = {
  id: "11111111-1111-4111-8111-111111111111",
  revision: 3,
  repository: "oci://registry.example.test/charts/payments",
  version: "1.2.3",
  manifestDigest: `sha256:${"a".repeat(64)}`,
  packageDigest: `sha256:${"b".repeat(64)}`,
  valuesSchemaDigest: `sha256:${"c".repeat(64)}`,
  rendererImage: `registry.example.test/renderer@sha256:${"d".repeat(64)}`,
  rendererVersion: "4.2.3" as const,
  policyVersion: "external-helm-p0.v1" as const,
  documentsDigest: `sha256:${"e".repeat(64)}`,
  valuesSchema: { type: "object" },
  defaultValuesYaml: "replicaCount: 2\n",
  createdAt: "2026-08-09T00:00:00Z",
  credentials: "must-not-cross-client-boundary",
};

const revision = {
  id: "22222222-2222-4222-8222-222222222222",
  generation: 7,
  releaseName: "payments-production",
  action: "update" as const,
  desiredEnabled: true,
  approval: { id: approval.id, revision: approval.revision },
  renderCommandId: "33333333-3333-4333-8333-333333333333",
  valuesDigest: `sha256:${"f".repeat(64)}`,
  intentDigest: `sha256:${"1".repeat(64)}`,
  requestId: "request-safe",
  createdAt: "2026-08-09T00:05:00Z",
  privateWriteBase: "must-not-cross-client-boundary",
};

const status = {
  revision,
  phase: "application-pending" as const,
  renderState: "succeeded" as const,
  payloadState: "ready",
  cascadeState: "verified",
  cascadeObservationState: "pending",
  applicationState: "pending",
  credential: "must-not-cross-client-boundary",
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

describe("approved Helm application API client", () => {
  it("uses all eight exact routes, closed inputs, and caller-stable replay keys", async () => {
    const preview = {
      approval: { id: approval.id, revision: approval.revision },
      normalizedValuesYaml: "replicaCount: 3\n",
      valuesDigest: `sha256:${"2".repeat(64)}`,
      effectiveValues: { replicaCount: 3 },
      changedPaths: ["/replicaCount"],
      rendererArguments: ["--post-renderer", "attacker"],
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ items: [approval] }))
      .mockResolvedValueOnce(jsonResponse(preview))
      .mockResolvedValueOnce(jsonResponse(status))
      .mockResolvedValueOnce(jsonResponse({ items: [status] }))
      .mockResolvedValueOnce(jsonResponse(revision, true))
      .mockResolvedValueOnce(jsonResponse({ ...revision, action: "retry" }))
      .mockResolvedValueOnce(jsonResponse({ ...revision, action: "disable" }))
      .mockResolvedValueOnce(jsonResponse({ ...revision, action: "rollback" }));
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=helm-csrf; path=/";
    const input = {
      approvalId: approval.id,
      approvalRevision: approval.revision,
      valuesYaml: "replicaCount: 3\n",
    };

    const catalog = await api.helmApprovals(
      "application/id",
      "environment/id",
      12,
    );
    const validated = await api.previewHelmValues(
      "application/id",
      "environment/id",
      input,
    );
    const head = await api.helmRelease("application/id", "environment/id");
    const history = await api.helmReleaseHistory(
      "application/id",
      "environment/id",
      9,
    );
    const upsert = await api.upsertHelmRelease(
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
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/approvals?limit=12",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/values-preview",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/releases?limit=9",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/retry",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/disable",
      "/v1/applications/application%2Fid/environments/environment%2Fid/helm/release/rollback",
    ]);
    expect(
      fetchMock.mock.calls
        .slice(4)
        .map((call) =>
          new Headers((call[1] as RequestInit).headers).get("Idempotency-Key"),
        ),
    ).toEqual([
      "helm-upsert-stable-key",
      "helm-retry-stable-key",
      "helm-disable-stable-key",
      "helm-rollback-stable-key",
    ]);
    const bodies = fetchMock.mock.calls.map((call) =>
      (call[1] as RequestInit).body
        ? JSON.parse(String((call[1] as RequestInit).body))
        : undefined,
    );
    expect(bodies[1]).toEqual(input);
    expect(bodies[4]).toEqual(input);
    expect(bodies[5]).toEqual({});
    expect(bodies[6]).toEqual({});
    expect(bodies[7]).toEqual({ sourceRevisionId: revision.id });
    expect(upsert.replayed).toBe(true);
    expect(head.cascadeState).toBe("verified");
    expect(head.cascadeObservationState).toBe("pending");
    expect(history.items[0]?.cascadeState).toBe("verified");
    expect(history.items[0]?.cascadeObservationState).toBe("pending");
    expect(
      JSON.stringify({ catalog, validated, head, history, upsert }),
    ).not.toMatch(
      /credentials|privateWriteBase|credential|rendererArguments|must-not-cross/,
    );
  });

  it("rejects noncanonical limits and oversized UTF-8 values before fetch", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    expect(() => api.helmApprovals("app", "env", 0)).toThrow(ApiError);
    expect(() =>
      api.previewHelmValues("app", "env", {
        approvalId: approval.id,
        approvalRevision: 1,
        valuesYaml: "é".repeat(131_073),
      }),
    ).toThrow(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
