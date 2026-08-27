import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("builder settings API client", () => {
  it("sends exact revision and stable idempotency key", async () => {
    const response = {
      revision: 1,
      nodeIsolation: false,
      maxConcurrentBuilders: 2,
      checkoutResources: {
        cpuRequest: "100m",
        memoryRequest: "128Mi",
        ephemeralStorageRequest: "1Gi",
        cpuLimit: "1",
        memoryLimit: "512Mi",
        ephemeralStorageLimit: "2Gi",
      },
      dindResources: {
        cpuRequest: "500m",
        memoryRequest: "1Gi",
        ephemeralStorageRequest: "10Gi",
        cpuLimit: "4",
        memoryLimit: "8Gi",
        ephemeralStorageLimit: "50Gi",
      },
      agentResources: {
        cpuRequest: "250m",
        memoryRequest: "256Mi",
        ephemeralStorageRequest: "1Gi",
        cpuLimit: "4",
        memoryLimit: "4Gi",
        ephemeralStorageLimit: "10Gi",
      },
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(response), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const { revision: _revision, ...input } = response;
    await api.updateBuilderPlatformSettings(0, input, "builder-settings-key");
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/v1/platform/builder-settings");
    expect(init.method).toBe("PUT");
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe(
      "builder-settings-key",
    );
    expect(JSON.parse(String(init.body))).toEqual({ revision: 0, ...input });
  });
});
