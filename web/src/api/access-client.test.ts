import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("teams and GitHub access API client", () => {
  it("accepts an invitation without requiring a pre-session CSRF token", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "user_invited",
          displayName: "Ada Lovelace",
          role: "developer",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "kuberploy_csrf=existing-session-token; path=/";

    await api.acceptInvitation({
      token: "kp_invite_one_time",
      displayName: "Ada Lovelace",
      password: "developer password 123",
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/v1/auth/invitations/accept");
    expect(JSON.parse(String(init.body))).toEqual({
      token: "kp_invite_one_time",
      displayName: "Ada Lovelace",
      password: "developer password 123",
    });
    expect(headers.get("X-CSRF-Token")).toBeNull();
  });

  it("creates teams and memberships with idempotent exact request bodies", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "team_platform",
            name: "Platform engineering",
            slug: "platform-engineering",
            createdAt: "2026-08-06T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            teamId: "team_platform",
            userId: "user_2",
            role: "member",
            createdAt: "2026-08-06T00:01:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(crypto, "randomUUID")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000001")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000002");

    await api.createTeam({
      name: "Platform engineering",
      slug: "platform-engineering",
    });
    await api.addTeamMember("team_platform", {
      userId: "user_2",
      role: "member",
    });
    await api.removeTeamMember("team/platform", "user/2");

    const createInit = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const memberInit = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/v1/teams");
    expect(JSON.parse(String(createInit.body))).toEqual({
      name: "Platform engineering",
      slug: "platform-engineering",
    });
    expect(new Headers(createInit.headers).get("Idempotency-Key")).toBe(
      "00000000-0000-4000-8000-000000000001",
    );
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/v1/teams/team_platform/members",
    );
    expect(JSON.parse(String(memberInit.body))).toEqual({
      userId: "user_2",
      role: "member",
    });
    expect(new Headers(memberInit.headers).get("Idempotency-Key")).toBe(
      "00000000-0000-4000-8000-000000000002",
    );
    const removeInit = fetchMock.mock.calls[2]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[2]?.[0]).toBe(
      "/v1/teams/team%2Fplatform/members/user%2F2",
    );
    expect(removeInit.method).toBe("DELETE");
    expect(removeInit.body).toBeUndefined();
  });

  it("patches only the selected installation sharing decision", async () => {
    const installation = {
      id: "installation_1",
      githubInstallationId: 4815162342,
      accountLogin: "kuberploy",
      accountType: "Organization",
      ownerUserId: "user_1",
      visibility: "team",
      teamId: "team_platform",
      repositorySelection: "selected",
      repositoryCount: 3,
      createdAt: "2026-08-06T00:00:00Z",
      updatedAt: "2026-08-06T00:02:00Z",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(installation), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.updateGitHubInstallationSharing("installation/1", {
      visibility: "team",
      teamId: "team_platform",
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/github/installations/installation%2F1/sharing",
    );
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(String(init.body))).toEqual({
      visibility: "team",
      teamId: "team_platform",
    });
    expect(JSON.stringify(init.body)).not.toContain("token");
  });

  it("uses exact scoped grant targets and caller-stable idempotency keys", async () => {
    const grant = {
      id: "grant_1",
      subjectUserId: "user_2",
      role: "viewer",
      scopeType: "application",
      scopeId: "app/one",
      permissions: ["logs.read"],
      source: "explicit",
      createdBy: "user_1",
      createdAt: "2026-08-09T00:00:00Z",
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(grant), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await api.createProjectAccessGrant(
      "project/one",
      {
        subjectUserId: "user_2",
        role: "viewer",
        scopeType: "application",
        scopeId: "app/one",
        permissions: ["logs.read"],
      },
      "create-stable-key",
    );
    await api.deleteProjectAccessGrant(
      "project/one",
      "grant/one",
      "delete-stable-key",
    );

    const createInit = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const deleteInit = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/projects/project%2Fone/grants",
    );
    expect(new Headers(createInit.headers).get("Idempotency-Key")).toBe(
      "create-stable-key",
    );
    expect(JSON.parse(String(createInit.body))).toEqual({
      subjectUserId: "user_2",
      role: "viewer",
      scopeType: "application",
      scopeId: "app/one",
      permissions: ["logs.read"],
    });
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/v1/projects/project%2Fone/grants/grant%2Fone",
    );
    expect(deleteInit.method).toBe("DELETE");
    expect(new Headers(deleteInit.headers).get("Idempotency-Key")).toBe(
      "delete-stable-key",
    );
  });
});
