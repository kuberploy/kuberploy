import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const integration = {
  id: "integration/id",
  slug: "public-dns",
  name: "Public DNS",
  mode: "managed" as const,
  providerKind: "cloudflare" as const,
  txtOwnerId: "kuberploy.production",
  allowedDomainSuffixes: ["example.com"],
  syncPolicy: "upsert-only" as const,
  destructiveSyncConfirmed: false,
  credentialSecretRef: "external-dns-credentials",
  providerConfigRef: "cloudflare-provider",
  egressConfigRef: "internet-egress",
  environmentIds: ["environment/id"],
  createdBy: "user/id",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

describe("External DNS API client", () => {
  it("projects platform and scoped catalogs to bounded metadata", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: Array.from({ length: 101 }, (_, index) => ({
              ...integration,
              id: `integration-${index}`,
              rawCredential: "list-secret-leak",
              providerEndpoint: "https://private.provider.invalid",
            })),
            truncated: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: [
              {
                id: integration.id,
                slug: integration.slug,
                name: integration.name,
                mode: integration.mode,
                providerKind: integration.providerKind,
                allowedDomainSuffixes: integration.allowedDomainSuffixes,
                runtimeAvailable: true,
                credentialSecretRef: "catalog-secret-leak",
                txtOwnerId: "catalog-owner-leak",
                operatorProfileRef: "catalog-profile-leak",
              },
            ],
            truncated: false,
            configurationState: "configured",
            controllerReadiness: "ready",
            runtimeAvailable: true,
            providerObservation: "private-observation",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            configurationState: "configured",
            controllerReadiness: "ready",
            runtimeAvailable: true,
            detail: "No controller observation is available.",
            credentials: "status-secret-leak",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const list = await api.externalDNSIntegrations(25);
    const catalog = await api.applicationExternalDNSIntegrations(
      "application/id",
      "environment/id",
      25,
    );
    const status = await api.externalDNSStatus();

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/external-dns/integrations?limit=25",
      "/v1/applications/application%2Fid/external-dns-integrations?environmentId=environment%2Fid&limit=25",
      "/v1/external-dns/status",
    ]);
    expect(list.items).toHaveLength(25);
    expect(list.truncated).toBe(true);
    expect(catalog).toMatchObject({
      configurationState: "configured",
      controllerReadiness: "ready",
      runtimeAvailable: true,
    });
    expect(status).toMatchObject({
      controllerReadiness: "ready",
      runtimeAvailable: true,
    });
    expect(JSON.stringify({ list, catalog, status })).not.toMatch(
      /secret-leak|providerEndpoint|rawCredential|private-observation|"credentials"/,
    );
    expect(JSON.stringify(catalog)).not.toMatch(
      /credentialSecretRef|txtOwnerId|operatorProfileRef|providerConfigRef|egressConfigRef/,
    );
  });

  it("keeps caller-stable idempotency and exact structured input", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(integration), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(integration), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=external-dns-csrf; path=/";
    const input = {
      slug: integration.slug,
      name: integration.name,
      mode: integration.mode,
      providerKind: integration.providerKind,
      txtOwnerId: integration.txtOwnerId,
      allowedDomainSuffixes: integration.allowedDomainSuffixes,
      syncPolicy: integration.syncPolicy,
      destructiveSyncConfirmed: false,
      credentialSecretRef: integration.credentialSecretRef,
      providerConfigRef: integration.providerConfigRef,
      egressConfigRef: integration.egressConfigRef,
      environmentIds: integration.environmentIds,
    };

    await api.createExternalDNSIntegration(input, "create-stable-key-0001");
    await api.updateExternalDNSIntegration(
      "integration/id",
      input,
      "update-stable-key-0001",
    );

    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      "/v1/external-dns/integrations",
      "/v1/external-dns/integrations/integration%2Fid",
    ]);
    expect(
      fetchMock.mock.calls.map((call) =>
        new Headers((call[1] as RequestInit).headers).get("Idempotency-Key"),
      ),
    ).toEqual(["create-stable-key-0001", "update-stable-key-0001"]);
    expect(
      fetchMock.mock.calls.map((call) =>
        new Headers((call[1] as RequestInit).headers).get("X-CSRF-Token"),
      ),
    ).toEqual(["external-dns-csrf", "external-dns-csrf"]);
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual(
      input,
    );
  });
});
