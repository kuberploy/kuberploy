import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const entry = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "tenant-production",
  lifecycle: "active",
  currentRevision: 1,
  revision: {
    number: 1,
    environment: "production",
    email: "admin@example.com",
    accountPrivateKeySecretName: "tenant-production-account",
    solver: "http01",
    specDigest: `sha256:${"a".repeat(64)}`,
    createdAt: "2026-08-09T00:00:00Z",
  },
  observation: {
    state: "pending",
    updatedAt: "2026-08-09T00:00:00Z",
  },
  createdAt: "2026-08-09T00:00:00Z",
};

describe("certificate issuer administration client", () => {
  it("submits only the closed admin profile and server-derived ACME environment", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(entry), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      api.createPlatformCertificateIssuer(
        "tenant-production",
        {
          environment: "production",
          email: "admin@example.com",
          accountPrivateKeySecretName: "tenant-production-account",
          solver: { type: "http01" },
        },
        "stable-idempotency-key",
      ),
    ).resolves.toEqual(entry);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/platform/certificate-issuers",
    );
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe(
      "stable-idempotency-key",
    );
    expect(init.body).toBe(
      JSON.stringify({
        name: "tenant-production",
        environment: "production",
        email: "admin@example.com",
        accountPrivateKeySecretName: "tenant-production-account",
        solver: { type: "http01" },
      }),
    );
    expect(String(init.body)).not.toContain("server");
  });

  it("rejects unknown credential material from a compromised admin backend", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            items: [
              {
                ...entry,
                revision: {
                  ...entry.revision,
                  apiToken: "plaintext-secret",
                },
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    await expect(api.platformCertificateIssuers()).rejects.toThrow(
      "certificate issuer administration response was invalid",
    );
  });
});
