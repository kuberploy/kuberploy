import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const environmentId = "33333333-3333-4333-8333-333333333333";
const projectId = "22222222-2222-4222-8222-222222222222";
const bindingId = "11111111-1111-4111-8111-111111111111";
const projectPath = `tenants/${projectId}/variables.yaml`;
const environmentPath = `tenants/${projectId}/environments/${environmentId}/variables.yaml`;
const document = {
  apiVersion: "variables.kuberploy.io/v1alpha1",
  kind: "VariableSet",
  values: {},
};

describe("VariableSet API client", () => {
  it("preserves raw YAML and sends only server-scoped preview authority", async () => {
    const rawYaml =
      'apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  FLAG: "true" # keep this comment\n';
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          previewToken: "p".repeat(43),
          scope: "project",
          path: projectPath,
          gitDiff: "+  FLAG: true",
          document: { ...document, values: { FLAG: "true" } },
          diagnostics: [],
          expiresAt: "2026-08-09T01:00:00Z",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.previewVariableSet(
      environmentId,
      "project",
      rawYaml,
      '"sha256:' + "a".repeat(64) + '"',
      projectPath,
    );

    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = new Headers(options.headers);
    expect(path).toBe(
      `/v1/environments/${environmentId}/variable-sets/project/preview`,
    );
    expect(headers.get("If-Match")).toBe('"sha256:' + "a".repeat(64) + '"');
    expect(JSON.parse(String(options.body))).toEqual({ rawYaml });
    expect(String(options.body)).not.toMatch(/publicationMode|path|bindingId/i);
  });

  it("omits create If-Match and saves with only the preview token and stable key", async () => {
    const rawYaml =
      "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n";
    const operation = {
      id: "operation-safe",
      kind: "variable-set.git-write",
      status: "queued",
      targetType: "environment",
      targetId: environmentId,
      requestId: "request-safe",
      generation: 1,
      progress: [{ name: "git-write", status: "pending" }],
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            previewToken: "q".repeat(43),
            scope: "environment",
            path: environmentPath,
            gitDiff: "+values: {}",
            document,
            diagnostics: [],
            expiresAt: "2026-08-09T01:00:00Z",
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

    await api.previewVariableSet(
      environmentId,
      "environment",
      rawYaml,
      undefined,
      environmentPath,
    );
    await api.saveVariableSet(
      environmentId,
      "environment",
      rawYaml,
      "q".repeat(43),
      "stable-variable-save-key",
    );

    const previewHeaders = new Headers(
      (fetchMock.mock.calls[0]?.[1] as RequestInit).headers,
    );
    expect(previewHeaders.has("If-Match")).toBe(false);
    const [savePath, saveOptions] = fetchMock.mock.calls[1] as [
      string,
      RequestInit,
    ];
    const saveHeaders = new Headers(saveOptions.headers);
    expect(savePath).toBe(
      `/v1/environments/${environmentId}/variable-sets/environment`,
    );
    expect(saveHeaders.get("Preview-Token")).toBe("q".repeat(43));
    expect(saveHeaders.get("Idempotency-Key")).toBe("stable-variable-save-key");
    expect(JSON.parse(String(saveOptions.body))).toEqual({ rawYaml });
    expect(String(saveOptions.body)).not.toMatch(/publicationMode|path|etag/i);
  });

  it("rejects sensitive extras and a substituted project preview path", async () => {
    const revision = "b".repeat(40);
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                scope: "project",
                bindingId,
                projectId,
                environmentId,
                path: projectPath,
                present: false,
                indexedRevision: revision,
                credential: "provider-secret",
              },
              {
                scope: "environment",
                bindingId,
                projectId,
                environmentId,
                path: environmentPath,
                present: false,
                indexedRevision: revision,
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            previewToken: "r".repeat(43),
            scope: "project",
            path: "tenants/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/variables.yaml",
            gitDiff: "+values: {}",
            document,
            diagnostics: [],
            expiresAt: "2026-08-09T01:00:00Z",
            secret: "should-never-cross",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.variableSets(environmentId)).rejects.toMatchObject({
      status: 502,
    });
    await expect(
      api.previewVariableSet(
        environmentId,
        "project",
        "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n",
        undefined,
        projectPath,
      ),
    ).rejects.toMatchObject({ status: 502 });
  });
});
