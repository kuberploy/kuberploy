import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const safeMetadata = {
  id: "binding-safe",
  applicationId: "application/id",
  environmentId: "environment/id",
  name: "database-credentials",
  provider: "external-secrets",
  state: "ready",
  activeVersion: 3,
  createdBy: "user-safe",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const adversarialDetail = {
  ...safeMetadata,
  values: { password: "server-must-never-return-this" },
  providerRevision: "private-provider-revision",
  ciphertext: "private-ciphertext",
  versions: [
    {
      id: "version-safe",
      number: 3,
      state: "active",
      deliveries: [
        {
          sourceKey: "password",
          kind: "environment",
          environmentName: "DATABASE_PASSWORD",
          ciphertext: "nested-private-ciphertext",
        },
      ],
      values: { password: "nested-server-leak" },
      providerRevision: "nested-provider-revision",
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:05:00Z",
    },
  ],
};

describe("runtime-secret API client", () => {
  it("uses opaque scopes and projects every read response to safe metadata", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                ...safeMetadata,
                values: { password: "list-response-leak" },
                manifestDigest: "private-manifest-digest",
              },
              {
                ...safeMetadata,
                id: "binding-cross-scope",
                environmentId: "different-environment",
                name: "must-not-cross-scope",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(adversarialDetail), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    const list = await api.runtimeSecretBindings(
      "application/id",
      "environment/id",
    );
    const detail = await api.runtimeSecretBinding("binding/id");

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/applications/application%2Fid/secret-bindings?environmentId=environment%2Fid",
    );
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/v1/secret-bindings/binding%2Fid",
    );
    expect(list.items).toHaveLength(1);
    expect(JSON.stringify({ list, detail })).not.toMatch(
      /values|ciphertext|providerRevision|manifestDigest|server.*leak|private-/,
    );
    expect(detail.versions[0]?.deliveries).toEqual([
      {
        sourceKey: "password",
        kind: "environment",
        environmentName: "DATABASE_PASSWORD",
      },
    ]);
  });

  it("keeps caller-stable idempotency for create replay, rotate CAS, and delete", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(adversarialDetail), {
          status: 201,
          headers: {
            "Content-Type": "application/json",
            "Idempotent-Replay": "false",
          },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(adversarialDetail), {
          status: 201,
          headers: {
            "Content-Type": "application/json",
            "Idempotent-Replay": "true",
          },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(adversarialDetail), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=runtime-secret-csrf; path=/";
    const values = { password: "raw UTF-8 ; $(not-a-shell)" };
    const deliveries = [
      {
        sourceKey: "password",
        kind: "file" as const,
        filePath: "/var/run/secrets/kuberploy/database/password",
        fileMode: 256 as const,
      },
    ];
    const createInput = {
      environmentId: "environment/id",
      name: "database-credentials",
      provider: "sealed-secrets" as const,
      deliveries,
      values,
    };

    const first = await api.createRuntimeSecretBinding(
      "application/id",
      createInput,
      "create-stable-key-0001",
    );
    const replay = await api.createRuntimeSecretBinding(
      "application/id",
      createInput,
      "create-stable-key-0001",
    );
    const rotated = await api.rotateRuntimeSecretBinding(
      "binding/id",
      { expectedActiveVersion: 3, deliveries, values },
      "rotate-stable-key-0001",
    );
    await api.deleteRuntimeSecretBinding(
      "binding/id",
      "delete-stable-key-0001",
    );

    expect(JSON.stringify({ first, replay, rotated })).not.toContain(
      "raw UTF-8",
    );
    expect(JSON.stringify({ first, replay, rotated })).not.toContain("values");
    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/applications/application%2Fid/secret-bindings",
      "/v1/applications/application%2Fid/secret-bindings",
      "/v1/secret-bindings/binding%2Fid/versions",
      "/v1/secret-bindings/binding%2Fid",
    ]);
    const [firstInit, replayInit, rotateInit, deleteInit] =
      fetchMock.mock.calls.map((call) => call[1] as RequestInit);
    expect(new Headers(firstInit.headers).get("Idempotency-Key")).toBe(
      "create-stable-key-0001",
    );
    expect(new Headers(replayInit.headers).get("Idempotency-Key")).toBe(
      "create-stable-key-0001",
    );
    expect(new Headers(rotateInit.headers).get("Idempotency-Key")).toBe(
      "rotate-stable-key-0001",
    );
    expect(new Headers(deleteInit.headers).get("Idempotency-Key")).toBe(
      "delete-stable-key-0001",
    );
    expect(JSON.parse(String(firstInit.body))).toEqual(createInput);
    expect(JSON.parse(String(rotateInit.body))).toEqual({
      expectedActiveVersion: 3,
      deliveries,
      values,
    });
    expect(deleteInit.method).toBe("DELETE");
    expect(deleteInit.body).toBeUndefined();
  });
});
