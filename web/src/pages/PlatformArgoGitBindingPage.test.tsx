import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type { PlatformArgoGitBinding, Principal } from "../api/types";
import { PlatformArgoGitBindingPage } from "./PlatformArgoGitBindingPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const admin: Principal = {
  id: "admin",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" },
};

const created: PlatformArgoGitBinding = {
  id: "binding-safe",
  repository: {
    provider: "github",
    installationId: 42,
    repositoryId: 84,
    owner: "kuberploy",
    name: "platform-gitops",
  },
  targetRef: "refs/heads/main",
  pathPrefix: "platform",
  state: "waiting-for-git",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <PlatformArgoGitBindingPage />
    </QueryClientProvider>,
  );
  return client;
}

describe("platform Argo Git authority page", () => {
  it("requires explicit confirmation and sends only the selected catalog authority", async () => {
    vi.spyOn(api, "me").mockResolvedValue(admin);
    vi.spyOn(api, "platformArgoGitBinding")
      .mockRejectedValueOnce(new ApiError(404, { title: "Not found" }))
      .mockResolvedValue(created);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { argoCD: true },
    });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [
        {
          id: "installation-opaque",
          githubInstallationId: 42,
          accountLogin: "kuberploy",
          accountType: "Organization",
          ownerUserId: "admin",
          visibility: "private",
          repositorySelection: "selected",
          repositoryCount: 1,
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
      nextCursor: undefined,
    });
    vi.spyOn(api, "githubInstallationRepositories").mockResolvedValue({
      items: [
        {
          id: "repository-opaque",
          githubRepositoryId: 84,
          installationId: "installation-opaque",
          ownerId: 21,
          ownerLogin: "kuberploy",
          name: "platform-gitops",
          lifecycle: "active",
        },
      ],
      nextCursor: undefined,
    });
    const create = vi
      .spyOn(api, "createPlatformArgoGitBinding")
      .mockRejectedValueOnce(new Error("response lost"))
      .mockResolvedValue(created);
    renderPage();

    const confirmation = await screen.findByRole("checkbox", {
      name: /immutable protected platform Git authority/i,
    });
    const submit = screen.getByRole("button", {
      name: /Create immutable authority/i,
    });
    expect(submit).toBeDisabled();
    await waitFor(() => expect(confirmation).toBeEnabled());
    await userEvent.click(confirmation);
    await waitFor(() => expect(submit).toBeEnabled());
    await userEvent.click(submit);

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    await userEvent.click(submit);
    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    expect(create.mock.calls[1]?.[1]).toBe(create.mock.calls[0]?.[1]);
    expect(create).toHaveBeenCalledWith(
      {
        installationId: "installation-opaque",
        repositoryId: "repository-opaque",
        targetRef: "refs/heads/main",
      },
      expect.any(String),
    );
    expect(await screen.findByText("kuberploy/platform-gitops")).toBeVisible();
    expect(screen.getByText("main")).toBeVisible();
    expect(screen.queryByText("refs/heads/main")).toBeNull();
    expect(
      await screen.findByText("Authority recorded; Argo is ready"),
    ).toBeVisible();
    expect(
      screen.queryByText("Authority recorded; Argo remains fail-closed"),
    ).toBeNull();
    expect(screen.queryByLabelText(/secret|credential|clone url/i)).toBeNull();
  });

  it("does not inspect or mutate authority for automation principals", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      ...admin,
      authentication: {
        kind: "service-account",
        serviceAccountId: "service-account",
        tokenId: "token",
        scopes: ["app.read"],
        expiresAt: "2026-08-10T00:00:00Z",
      },
    });
    const get = vi.spyOn(api, "platformArgoGitBinding");
    const installations = vi.spyOn(api, "githubInstallations");
    renderPage();

    expect(
      await screen.findByText("Interactive platform administrator required"),
    ).toBeVisible();
    expect(get).not.toHaveBeenCalled();
    expect(installations).not.toHaveBeenCalled();
  });
});
