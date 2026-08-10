import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("service account API client", () => {
  it("uses exact encoded routes, request bodies, CSRF, and caller-stable idempotency keys", async () => {
    const account = {
      id: "service/account",
      projectId: "project/one",
      name: "release-bot",
      role: "developer",
      createdBy: "user_1",
      createdAt: "2026-08-09T00:00:00Z",
    };
    const tokenRecord = {
      id: "token/one",
      serviceAccountId: account.id,
      name: "production deploy",
      prefix: "kp_sa_abcdefgh",
      scopes: ["app.read", "app.edit"],
      expiresAt: "2026-09-01T00:00:00Z",
      createdBy: "user_1",
      createdAt: "2026-08-09T00:01:00Z",
    };
    const json = { "Content-Type": "application/json" };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: [account] }), {
          status: 200,
          headers: json,
        }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify(account), { status: 201, headers: json }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: [tokenRecord] }), {
          status: 200,
          headers: json,
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            tokenRecord,
            token: `kp_sa_${"x".repeat(43)}`,
          }),
          { status: 201, headers: json },
        ),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=service-account-csrf; path=/";

    await api.serviceAccounts("project/one");
    await api.createServiceAccount(
      "project/one",
      { name: "release-bot", role: "developer" },
      "create-account-key",
    );
    await api.disableServiceAccount("service/account", "disable-account-key");
    await api.serviceAccountTokens("service/account");
    await api.createServiceAccountToken(
      "service/account",
      {
        name: "production deploy",
        scopes: ["app.read", "app.edit"],
        expiresAt: "2026-09-01T00:00:00Z",
      },
      "issue-token-key",
    );
    await api.revokeServiceAccountToken(
      "service/account",
      "token/one",
      "revoke-token-key",
    );

    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "/v1/projects/project%2Fone/service-accounts",
      "/v1/projects/project%2Fone/service-accounts",
      "/v1/service-accounts/service%2Faccount",
      "/v1/service-accounts/service%2Faccount/tokens",
      "/v1/service-accounts/service%2Faccount/tokens",
      "/v1/service-accounts/service%2Faccount/tokens/token%2Fone",
    ]);
    const calls = fetchMock.mock.calls.map((call) => call[1] as RequestInit);
    expect(calls.map((call) => call.method)).toEqual([
      undefined,
      "POST",
      "DELETE",
      undefined,
      "POST",
      "DELETE",
    ]);
    expect(JSON.parse(String(calls[1]?.body))).toEqual({
      name: "release-bot",
      role: "developer",
    });
    expect(JSON.parse(String(calls[4]?.body))).toEqual({
      name: "production deploy",
      scopes: ["app.read", "app.edit"],
      expiresAt: "2026-09-01T00:00:00Z",
    });
    expect(
      [calls[1], calls[2], calls[4], calls[5]].map((call) =>
        new Headers(call?.headers).get("Idempotency-Key"),
      ),
    ).toEqual([
      "create-account-key",
      "disable-account-key",
      "issue-token-key",
      "revoke-token-key",
    ]);
    for (const call of [calls[1], calls[2], calls[4], calls[5]]) {
      expect(new Headers(call?.headers).get("X-CSRF-Token")).toEqual(
        expect.any(String),
      );
    }
    expect(String(calls[4]?.body)).not.toContain("kp_sa_");
  });
});
