import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("certificate issuer catalog client", () => {
  it("uses exact scope and accepts only the safe readiness-gated projection", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          items: [
            {
              name: "kuberploy-letsencrypt-production",
              environment: "production",
              solverTypes: ["dns01", "http01"],
              source: "bootstrap",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      api.applicationCertificateIssuers(
        "application/id",
        "environment/id",
        "api.example.com",
      ),
    ).resolves.toEqual({
      items: [
        {
          name: "kuberploy-letsencrypt-production",
          environment: "production",
          solverTypes: ["dns01", "http01"],
          source: "bootstrap",
        },
      ],
    });
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/applications/application%2Fid/certificate-issuers?environmentId=environment%2Fid&hostname=api.example.com",
    );
  });

  it("rejects credential, zone, and account metadata from a compromised backend", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            items: [
              {
                name: "issuer",
                environment: "production",
                solverTypes: ["dns01-cloudflare"],
                source: "managed",
                revision: 1,
                email: "platform@example.com",
                dnsZones: ["example.com"],
                apiTokenSecretName: "cloudflare-token",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    await expect(
      api.applicationCertificateIssuers(
        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
        "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
        "api.example.com",
      ),
    ).rejects.toThrow("certificate issuer catalog response was invalid");
  });
});
