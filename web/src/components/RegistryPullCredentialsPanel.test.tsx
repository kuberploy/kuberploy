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
      .mockRejectedValueOnce(new Error("response lost"))
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
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(select).toHaveValue("public");
    expect(
      await screen.findByText("Pull strategy was not saved"),
    ).toBeVisible();
    await userEvent.selectOptions(select, "credential-backup");
    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(save.mock.calls[1]?.[2]).toBe(save.mock.calls[0]?.[2]);
    expect(
      screen.getByText(/does not enable or disable GitHub builds/i),
    ).toBeVisible();
  });

  it("preserves a newer credential draft when the earlier create completes", async () => {
    vi.spyOn(api, "projectRegistryPullCredentials").mockResolvedValue({
      items: [],
      availableTargets: [
        {
          id: "target-1",
          name: "Primary",
          server: "registry.example.test",
          repositoryPrefix: "team",
        },
      ],
    });
    vi.spyOn(api, "applicationRegistryPullSelection").mockResolvedValue({
      applicationId: "application-1",
      type: "public",
    });
    let resolveCreate!: (
      value: Awaited<
        ReturnType<typeof api.createProjectRegistryPullCredential>
      >,
    ) => void;
    const create = vi
      .spyOn(api, "createProjectRegistryPullCredential")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveCreate = resolve;
          }),
      );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();
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

    const name = await screen.findByLabelText("Credential name");
    await user.type(name, "First credential");
    await user.selectOptions(screen.getByLabelText("Registry"), "target-1");
    await user.click(
      screen.getByRole("button", { name: "Add project credential" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    await user.clear(name);
    await user.type(name, "Newer credential");

    resolveCreate(
      {} as Awaited<ReturnType<typeof api.createProjectRegistryPullCredential>>,
    );
    await waitFor(() => expect(name).toHaveValue("Newer credential"));
  });

  it("ignores a completed create from a previous application scope", async () => {
    vi.spyOn(api, "projectRegistryPullCredentials").mockResolvedValue({
      items: [],
      availableTargets: [
        {
          id: "target-1",
          name: "Primary",
          server: "registry.example.test",
          repositoryPrefix: "team",
        },
      ],
    });
    vi.spyOn(api, "applicationRegistryPullSelection").mockImplementation(
      async (applicationId) => ({ applicationId, type: "public" }),
    );
    let resolveCreate!: (
      value: Awaited<
        ReturnType<typeof api.createProjectRegistryPullCredential>
      >,
    ) => void;
    const create = vi
      .spyOn(api, "createProjectRegistryPullCredential")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveCreate = resolve;
          }),
      );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();
    const view = render(
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

    const name = await screen.findByLabelText("Credential name");
    await user.type(name, "First credential");
    await user.selectOptions(screen.getByLabelText("Registry"), "target-1");
    await user.click(
      screen.getByRole("button", { name: "Add project credential" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledOnce());

    view.rerender(
      <QueryClientProvider client={client}>
        <RegistryPullCredentialsPanel
          application={{
            id: "application-2",
            projectId: "project-2",
            name: "Billing API",
          }}
          project={{ id: "project-2", name: "Billing" }}
          enabled
          canManage
        />
      </QueryClientProvider>,
    );
    const newerName = await screen.findByLabelText("Credential name");
    await user.type(newerName, "First credential");
    await user.selectOptions(screen.getByLabelText("Registry"), "target-1");

    resolveCreate(
      {} as Awaited<ReturnType<typeof api.createProjectRegistryPullCredential>>,
    );
    await waitFor(() => expect(newerName).toHaveValue("First credential"));
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
      .mockRejectedValueOnce(new Error("network unavailable"))
      .mockResolvedValueOnce(undefined);
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
    expect(
      screen.getByRole("alertdialog", {
        name: /Remove project pull credential Production/,
      }),
    ).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(remove).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await userEvent.click(
      screen.getByRole("button", { name: "Remove credential" }),
    );
    await waitFor(() =>
      expect(remove).toHaveBeenCalledWith(
        "project-1",
        "credential-primary",
        expect.any(String),
      ),
    );
    const firstKey = remove.mock.calls[0]?.[2];
    expect(firstKey).toEqual(expect.any(String));
    await waitFor(() =>
      expect(screen.getByText("Credential was not removed")).toBeVisible(),
    );
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await userEvent.click(
      screen.getByRole("button", { name: "Remove credential" }),
    );
    await waitFor(() => expect(remove).toHaveBeenCalledTimes(2));
    expect(remove.mock.calls[1]?.[2]).toBe(firstKey);
  });

  it("restores the current project credential when a strategy save fails", async () => {
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
      type: "project-credential",
      projectCredentialId: "credential-primary",
    });
    const save = vi
      .spyOn(api, "putApplicationRegistryPullSelection")
      .mockRejectedValue(new Error("response lost"));
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
    await waitFor(() => expect(select).toHaveValue("credential-primary"));
    await userEvent.selectOptions(select, "public");
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(select).toHaveValue("credential-primary");
  });

  it("keeps a removed current credential visible until the operator chooses a replacement", async () => {
    vi.spyOn(api, "projectRegistryPullCredentials").mockResolvedValue({
      items: [],
      availableTargets: [],
    });
    vi.spyOn(api, "applicationRegistryPullSelection").mockResolvedValue({
      applicationId: "application-1",
      type: "project-credential",
      projectCredentialId: "credential-removed",
    });
    const save = vi
      .spyOn(api, "putApplicationRegistryPullSelection")
      .mockResolvedValue({
        applicationId: "application-1",
        type: "public",
      });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const user = userEvent.setup();
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
      await screen.findByRole("option", {
        name: "Current project credential unavailable — choose another",
      }),
    ).toBeVisible();
    expect(screen.getByText("Credential unavailable")).toBeVisible();
    await user.selectOptions(select, "public");
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save).toHaveBeenCalledWith(
      "application-1",
      { type: "public" },
      expect.any(String),
    );
  });
});
