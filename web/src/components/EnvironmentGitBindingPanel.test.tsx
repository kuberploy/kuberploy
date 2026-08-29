import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type { EnvironmentGitBinding } from "../api/types";
import { EnvironmentGitBindingPanel } from "./EnvironmentGitBindingPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const environment = {
  id: "environment-opaque",
  projectId: "project-opaque",
  name: "Development",
  slug: "development",
  namespace: "kp-payments-development",
};

const created: EnvironmentGitBinding = {
  id: "binding-safe",
  projectId: environment.projectId,
  environmentId: environment.id,
  repository: {
    provider: "github",
    installationId: 42,
    repositoryId: 84,
    owner: "kuberploy",
    name: "application-gitops",
  },
  targetRef: "refs/heads/development",
  pathPrefix: "tenants/payments/environments/development",
  credentialMode: "github-app",
  state: "waiting-for-git",
  projectionGeneration: 0,
  parserVersion: "appconfig-v1",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

function renderPanel(input?: { humanSession?: boolean; canManage?: boolean }) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <EnvironmentGitBindingPanel
        environment={environment}
        humanSession={input?.humanSession ?? true}
        canManage={input?.canManage ?? true}
        onClose={() => undefined}
      />
    </QueryClientProvider>,
  );
  return client;
}

describe("environment Git authority panel", () => {
  it("requires explicit confirmation and submits only verified opaque authority", async () => {
    vi.spyOn(api, "environmentGitBinding")
      .mockRejectedValueOnce(new ApiError(404, { title: "Not found" }))
      .mockResolvedValue(created);
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
          name: "application-gitops",
          lifecycle: "active",
        },
        {
          id: "removed-repository",
          githubRepositoryId: 85,
          installationId: "installation-opaque",
          ownerId: 21,
          ownerLogin: "kuberploy",
          name: "removed",
          lifecycle: "removed",
        },
      ],
      nextCursor: undefined,
    });
    const create = vi
      .spyOn(api, "createEnvironmentGitBinding")
      .mockResolvedValue(created);
    const randomUUID = vi
      .spyOn(crypto, "randomUUID")
      .mockReturnValue("01900000-0000-7000-8000-000000000001");
    renderPanel();

    const confirmation = await screen.findByRole("checkbox", {
      name: /environment Git authority/i,
    });
    const submit = screen.getByRole("button", {
      name: /create Git authority/i,
    });
    expect(submit).toBeDisabled();
    expect(screen.queryByRole("option", { name: /removed/i })).toBeNull();
    await waitFor(() => expect(confirmation).toBeEnabled());
    await userEvent.click(confirmation);
    await waitFor(() => expect(submit).toBeEnabled());
    await userEvent.click(submit);

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create).toHaveBeenCalledWith(
      environment.id,
      {
        installationId: "installation-opaque",
        repositoryId: "repository-opaque",
        targetRef: "refs/heads/main",
      },
      "01900000-0000-7000-8000-000000000001",
    );
    expect(randomUUID).toHaveBeenCalledTimes(1);
    expect(
      await screen.findByText("kuberploy/application-gitops"),
    ).toBeVisible();
    expect(screen.getByText("development")).toBeVisible();
    expect(screen.queryByText("refs/heads/development")).toBeNull();
    expect(screen.getByText(created.pathPrefix)).toBeVisible();
  });

  it("never loads provider catalogs or mutates for automation/read-only callers", async () => {
    vi.spyOn(api, "environmentGitBinding").mockRejectedValue(
      new ApiError(404, { title: "Not found" }),
    );
    const installations = vi.spyOn(api, "githubInstallations");
    const repositories = vi.spyOn(api, "githubInstallationRepositories");
    const create = vi.spyOn(api, "createEnvironmentGitBinding");
    renderPanel({ humanSession: false, canManage: true });

    expect(
      await screen.findByText(/interactive project administrator/i),
    ).toBeVisible();
    expect(installations).not.toHaveBeenCalled();
    expect(repositories).not.toHaveBeenCalled();
    expect(create).not.toHaveBeenCalled();
    expect(screen.queryByRole("form")).toBeNull();
  });

  it("renders only the existing binding's safe metadata", async () => {
    vi.spyOn(api, "environmentGitBinding").mockResolvedValue({
      ...created,
      indexedRevision: "0123456789abcdef0123456789abcdef01234567",
      projectionGeneration: 7,
      state: "ready",
    });
    const installations = vi.spyOn(api, "githubInstallations");
    renderPanel({ canManage: false });

    expect(
      await screen.findByText("kuberploy/application-gitops"),
    ).toBeVisible();
    expect(screen.getByText("0123456789ab")).toBeVisible();
    expect(screen.getByText("7")).toBeVisible();
    expect(screen.queryByText(/provider-token|private-key/i)).toBeNull();
    expect(installations).not.toHaveBeenCalled();
  });
});
