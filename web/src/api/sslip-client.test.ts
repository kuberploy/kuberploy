import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("sslip hostname API client", () => {
  it("sends only opaque application/environment IDs and projects the exact safe response", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          mode: "sslip",
          hostname: "api-203-0-113-10.sslip.io",
          source: "service-ip",
          observedAt: "2026-08-09T10:00:00Z",
          ip: "203.0.113.10",
          namespace: "private-namespace",
          providerPayload: { address: "203.0.113.10" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const preview = await api.applicationSSLIPHostname(
      "application/id",
      "environment/id",
    );

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/applications/application%2Fid/sslip-hostname?environmentId=environment%2Fid",
    );
    expect(preview).toEqual({
      mode: "sslip",
      hostname: "api-203-0-113-10.sslip.io",
      source: "service-ip",
      observedAt: "2026-08-09T10:00:00Z",
    });
    expect(JSON.stringify(preview)).not.toMatch(
      /private-namespace|providerPayload|"ip"|"address"/,
    );
  });
});
