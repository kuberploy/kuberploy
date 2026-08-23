import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

const safeBinding = {
  id: "binding-safe",
  repository: {
    provider: "github" as const,
    installationId: 42,
    repositoryId: 84,
    owner: "kuberploy",
    name: "platform-gitops",
  },
  targetRef: "refs/heads/platform",
  pathPrefix: "platform",
  state: "waiting-for-git" as const,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

describe("platform Argo Git binding API client", () => {
  it("projects responses to the exact metadata-only shape", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          ...safeBinding,
          remote: "https://github.com/private/repository.git",
          githubAppId: 177,
          credentialMode: "github-app",
          credentialSecret: "argo-repository-secret",
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

    const binding = await api.platformArgoGitBinding();

    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/platform/argo/git-binding",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    expect(binding).toEqual(safeBinding);
    expect(JSON.stringify(binding)).not.toMatch(
      /remote|cloneUrl|githubAppId|credential|secret|token|privateKey/i,
    );
  });

  it("sends only opaque catalog IDs and the exact branch ref with a caller-stable key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(safeBinding), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.createPlatformArgoGitBinding(
      {
        installationId: "installation/opaque",
        repositoryId: "repository/opaque",
        targetRef: "refs/heads/platform",
      },
      "platform-binding-key",
    );

    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/platform/argo/git-binding");
    expect(options.method).toBe("POST");
    expect(new Headers(options.headers).get("Idempotency-Key")).toBe(
      "platform-binding-key",
    );
    expect(JSON.parse(String(options.body))).toEqual({
      installationId: "installation/opaque",
      repositoryId: "repository/opaque",
      targetRef: "refs/heads/platform",
    });
  });
});
