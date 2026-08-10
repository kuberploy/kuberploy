import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const safeBinding = {
  id: "binding-safe",
  projectId: "project-safe",
  environmentId: "environment/opaque",
  repository: {
    provider: "github" as const,
    installationId: 42,
    repositoryId: 84,
    owner: "kuberploy",
    name: "application-gitops",
  },
  targetRef: "refs/heads/development",
  pathPrefix: "tenants/project-safe/environments/development",
  credentialMode: "github-app" as const,
  state: "waiting-for-git" as const,
  projectionGeneration: 0,
  parserVersion: "appconfig-v1",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

describe("environment Git binding API client", () => {
  it("encodes the environment identity and projects metadata only", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          ...safeBinding,
          remote: "https://github.com/private/repository.git",
          credentialSecret: "environment-git-credential",
          token: "provider-token",
          repository: {
            ...safeBinding.repository,
            cloneUrl: "https://github.com/private/repository.git",
            privateKey: "private-key",
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const binding = await api.environmentGitBinding("environment/opaque");

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/environments/environment%2Fopaque/git-binding",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    expect(binding).toEqual(safeBinding);
    expect(JSON.stringify(binding)).not.toMatch(
      /remote|cloneUrl|credentialSecret|token|privateKey/i,
    );
  });

  it("sends only opaque catalog IDs and the exact ref with a stable key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(safeBinding), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.createEnvironmentGitBinding(
      "environment/opaque",
      {
        installationId: "installation/opaque",
        repositoryId: "repository/opaque",
        targetRef: "refs/heads/development",
      },
      "environment-binding-key",
    );

    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/environments/environment%2Fopaque/git-binding");
    expect(options.method).toBe("POST");
    expect(new Headers(options.headers).get("Idempotency-Key")).toBe(
      "environment-binding-key",
    );
    expect(JSON.parse(String(options.body))).toEqual({
      installationId: "installation/opaque",
      repositoryId: "repository/opaque",
      targetRef: "refs/heads/development",
    });
  });
});
