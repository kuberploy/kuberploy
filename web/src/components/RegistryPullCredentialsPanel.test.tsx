import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { RegistryPullCredentialsPanel } from "./RegistryPullCredentialsPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("RegistryPullCredentialsPanel", () => {
  it("selects one of multiple project credentials without mixing builder settings", async () => {
    vi.spyOn(api, "projectRegistryPullCredentials").mockResolvedValue({
      items: [
        {
          id: "credential-primary",
          projectId: "project-1",
          registryTargetId: "target-1",
          name: "Production",
          registryName: "Primary",
          registryServer: "registry.example.test",
          repositoryPrefix: "team",
          createdAt: "2026-08-10T00:00:00Z",
          updatedAt: "2026-08-10T00:00:00Z",
        },
        {
          id: "credential-backup",
          projectId: "project-1",
          registryTargetId: "target-2",
          name: "Backup",
          registryName: "Backup",
          registryServer: "backup.example.test",
          repositoryPrefix: "team",
          createdAt: "2026-08-10T00:00:00Z",
          updatedAt: "2026-08-10T00:00:00Z",
        },
      ],
      availableTargets: [],
    });
    vi.spyOn(api, "applicationRegistryPullSelection").mockResolvedValue({
      applicationId: "application-1",
      type: "public",
    });
    const save = vi
      .spyOn(api, "putApplicationRegistryPullSelection")
      .mockResolvedValue({
        applicationId: "application-1",
        type: "project-credential",
        projectCredentialId: "credential-backup",
      });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <RegistryPullCredentialsPanel
          application={{
            id: "application-1",
            projectId: "project-1",
            name: "API",
          }}
          project={{ id: "project-1", name: "Payments" }}
          enabled
          canManage
        />
      </QueryClientProvider>,
    );
    const select = await screen.findByLabelText("Pull strategy");
    expect(
      await screen.findByRole("option", { name: /Production/ }),
    ).toBeVisible();
    expect(screen.getByRole("option", { name: /Backup/ })).toBeVisible();
    await userEvent.selectOptions(select, "credential-backup");
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        "application-1",
        {
          type: "project-credential",
          projectCredentialId: "credential-backup",
        },
        expect.any(String),
      ),
    );
    expect(
      screen.getByText(/does not enable or disable GitHub builds/i),
    ).toBeVisible();
  });

  it("requires explicit confirmation before removing a project credential", async () => {
    vi.spyOn(api, "projectRegistryPullCredentials").mockResolvedValue({
      items: [
        {
          id: "credential-primary",
          projectId: "project-1",
          registryTargetId: "target-1",
          name: "Production",
          registryName: "Primary",
          registryServer: "registry.example.test",
          repositoryPrefix: "team",
          createdAt: "2026-08-10T00:00:00Z",
          updatedAt: "2026-08-10T00:00:00Z",
        },
      ],
      availableTargets: [],
    });
    vi.spyOn(api, "applicationRegistryPullSelection").mockResolvedValue({
      applicationId: "application-1",
      type: "public",
    });
    const remove = vi
      .spyOn(api, "deleteProjectRegistryPullCredential")
      .mockResolvedValue(undefined);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <RegistryPullCredentialsPanel
          application={{
            id: "application-1",
            projectId: "project-1",
            name: "API",
          }}
          project={{ id: "project-1", name: "Payments" }}
          enabled
          canManage
        />
      </QueryClientProvider>,
    );

    await userEvent.click(
      await screen.findByRole("button", { name: "Remove" }),
    );
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("Production"));
    expect(remove).not.toHaveBeenCalled();
    confirm.mockReturnValue(true);
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(remove).toHaveBeenCalledWith("project-1", "credential-primary"),
    );
  });
});
