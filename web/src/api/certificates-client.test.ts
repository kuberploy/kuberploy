import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const safeMetadata = {
  id: "binding-safe",
  applicationId: "application/id",
  environmentId: "environment/id",
  name: "public-edge",
  state: "ready",
  activeVersion: 2,
  createdBy: "user-safe",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const adversarialDetail = {
  ...safeMetadata,
  certificatePem: "server-certificate-leak",
  privateKeyPem: "server-private-key-leak",
  targetSecretName: "server-secret-name",
  provider: "sealed-secrets",
  versions: [
    {
      number: 2,
      leafFingerprint: `sha256:${"a".repeat(64)}`,
      publicKeyFingerprint: `sha256:${"b".repeat(64)}`,
      dnsNames: ["api.example.test"],
      ipAddresses: [],
      notBefore: "2026-08-09T00:00:00Z",
      notAfter: "2026-11-09T00:00:00Z",
      createdBy: "user-safe",
      createdAt: "2026-08-09T00:00:00Z",
      secretVersionId: "internal-secret-version",
      ciphertext: "nested-private-leak",
    },
  ],
};

describe("certificate API client", () => {
  it("projects adversarial responses to public certificate metadata only", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              adversarialDetail,
              {
                ...safeMetadata,
                id: "cross-scope",
                environmentId: "different-environment",
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

    const list = await api.certificateBindings(
      "application/id",
      "environment/id",
    );
    const detail = await api.certificateBinding("binding/id");

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/applications/application%2Fid/certificate-bindings?environmentId=environment%2Fid",
    );
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/v1/certificate-bindings/binding%2Fid",
    );
    expect(list.items).toHaveLength(1);
    expect(detail.versions[0]?.dnsNames).toEqual(["api.example.test"]);
    expect(JSON.stringify({ list, detail })).not.toMatch(
      /certificatePem|privateKeyPem|targetSecretName|secretVersionId|ciphertext|server-.*leak|nested-private/,
    );
  });

  it("sends PEM only in mutation bodies with caller-stable idempotency", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(adversarialDetail), {
          status: 201,
          headers: { "Content-Type": "application/json" },
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
    document.cookie = "kuberploy_csrf=certificate-csrf; path=/";

    const certificatePem = "-----BEGIN CERTIFICATE-----\nrequest-only\n";
    const privateKeyPem = "-----BEGIN PRIVATE KEY-----\nrequest-only\n";
    await api.createCertificateBinding(
      "application/id",
      {
        environmentId: "environment/id",
        name: "public-edge",
        certificatePem,
        privateKeyPem,
      },
      "certificate-create-0001",
    );
    await api.rotateCertificateBinding(
      "binding/id",
      { expectedActiveVersion: 2, certificatePem, privateKeyPem },
      "certificate-rotate-0001",
    );
    await api.deleteCertificateBinding("binding/id", "certificate-delete-0001");

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/applications/application%2Fid/certificate-bindings",
      "/v1/certificate-bindings/binding%2Fid/versions",
      "/v1/certificate-bindings/binding%2Fid",
    ]);
    const [create, rotate, remove] = fetchMock.mock.calls.map(
      (call) => call[1] as RequestInit,
    );
    expect(new Headers(create.headers).get("Idempotency-Key")).toBe(
      "certificate-create-0001",
    );
    expect(new Headers(rotate.headers).get("Idempotency-Key")).toBe(
      "certificate-rotate-0001",
    );
    expect(new Headers(remove.headers).get("Idempotency-Key")).toBe(
      "certificate-delete-0001",
    );
    expect(JSON.parse(String(create.body))).toEqual({
      environmentId: "environment/id",
      name: "public-edge",
      certificatePem,
      privateKeyPem,
    });
    expect(JSON.parse(String(rotate.body))).toEqual({
      expectedActiveVersion: 2,
      certificatePem,
      privateKeyPem,
    });
    expect(remove.body).toBeUndefined();
  });
});
