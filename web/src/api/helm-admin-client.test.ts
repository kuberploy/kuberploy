import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const approval = {
  id: "11111111-1111-4111-8111-111111111111",
  revision: 1,
  sourceKind: "oci" as const,
  repository: "oci://registry.example.test/charts/payments",
  chartName: "payments",
  version: "1.2.3",
  manifestDigest: `sha256:${"a".repeat(64)}`,
  packageDigest: `sha256:${"b".repeat(64)}`,
  valuesSchemaDigest: `sha256:${"c".repeat(64)}`,
  rendererImage: `renderer@sha256:${"d".repeat(64)}`,
  rendererVersion: "4.2.3" as const,
  policyVersion: "external-helm-p0.v1" as const,
  documentsDigest: `sha256:${"e".repeat(64)}`,
  valuesSchema: {},
  defaultValuesYaml: "{}\n",
  createdAt: "2026-08-09T00:00:00Z",
  credential: "secret",
};

describe("platform Helm approval and rendered inventory clients", () => {
  it("uses exact routes, closed POST body, stable key, and redacted projections", async () => {
    const inventory = {
      releaseRevisionId: "22222222-2222-4222-8222-222222222222",
      generation: 2,
      manifestDigest: `sha256:${"f".repeat(64)}`,
      inventoryDigest: `sha256:${"1".repeat(64)}`,
      resourceCount: 1,
      previewBytes: 37,
      resources: [
        {
          apiVersion: "apps/v1",
          kind: "Deployment",
          namespace: "payments",
          name: "api",
          sanitizedYaml: "apiVersion: apps/v1\nkind: Deployment\n",
          previewOmitted: false,
          manifest: "secret",
        },
      ],
      renderedYaml: "secret",
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: [approval] }), { status: 200 }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(approval), { status: 200 }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(inventory), { status: 200 }),
      );
    vi.stubGlobal("fetch", fetchMock);
    const input = {
      sourceKind: "oci" as const,
      repository: approval.repository,
      version: approval.version,
      manifestDigest: approval.manifestDigest,
      packageDigest: approval.packageDigest,
      valuesSchemaDigest: approval.valuesSchemaDigest,
    };
    const listed = await api.platformHelmApprovals();
    const created = await api.createPlatformHelmApproval(
      { ...input, credentials: "x" } as typeof input,
      "stable-approval-key",
    );
    const preview = await api.helmRenderedPreview("app/id", "env/id");
    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/platform/helm/approvals",
      "/v1/platform/helm/approvals",
      "/v1/applications/app%2Fid/environments/env%2Fid/helm/rendered-preview",
    ]);
    expect(
      JSON.parse(String((fetchMock.mock.calls[1][1] as RequestInit).body)),
    ).toEqual(input);
    expect(
      new Headers((fetchMock.mock.calls[1][1] as RequestInit).headers).get(
        "Idempotency-Key",
      ),
    ).toBe("stable-approval-key");
    expect(JSON.stringify({ listed, created, preview })).not.toMatch(
      /credential|renderedYaml|manifest\"/,
    );
    expect(preview.resources).toEqual([
      {
        apiVersion: "apps/v1",
        kind: "Deployment",
        namespace: "payments",
        name: "api",
        sanitizedYaml: "apiVersion: apps/v1\nkind: Deployment\n",
        previewOmitted: false,
      },
    ]);
  });

  it("rejects a backend preview that hides raw bytes behind an omitted marker", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            releaseRevisionId: "22222222-2222-4222-8222-222222222222",
            generation: 1,
            manifestDigest: `sha256:${"a".repeat(64)}`,
            inventoryDigest: `sha256:${"b".repeat(64)}`,
            resourceCount: 1,
            previewBytes: 0,
            resources: [
              {
                apiVersion: "v1",
                kind: "Secret",
                namespace: "payments",
                name: "credentials",
                previewOmitted: true,
                sanitizedYaml: "raw-secret-must-not-pass",
              },
            ],
          }),
          { status: 200 },
        ),
      ),
    );
    await expect(api.helmRenderedPreview("app", "env")).rejects.toMatchObject({
      status: 502,
    });
  });
});
