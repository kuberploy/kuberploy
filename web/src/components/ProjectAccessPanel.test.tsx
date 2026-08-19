import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { AccessGrant, Capability } from "../api/types";
import { ProjectAccessPanel } from "./ProjectAccessPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const grant: AccessGrant = {
  id: "11111111-1111-4111-8111-111111111111",
  subjectUserId: "22222222-2222-4222-8222-222222222222",
  role: "viewer",
  scopeType: "application",
  scopeId: "33333333-3333-4333-8333-333333333333",
  permissions: ["logs.read"],
  source: "explicit",
  createdBy: "44444444-4444-4444-8444-444444444444",
  createdAt: "2026-08-09T00:00:00Z",
};

const organizationCapability: Capability = {
  resource: "team",
  scope: "team-1",
  role: "organization-admin",
  scopeType: "team",
  scopeId: "team-1",
  actions: [
    "access-grants:read",
    "access-grants:create",
    "access-grants:delete",
  ],
};

const projectCapability: Capability = {
  resource: "project",
  scope: "project-1",
  role: "project-admin",
  scopeType: "project",
  scopeId: "project-1",
  actions: [
    "access-grants:read",
    "access-grants:create",
    "access-grants:delete",
  ],
};

const forgedPlatformCapability: Capability = {
  role: "platform-admin",
  scopeType: "platform",
  scopeId: "another-platform",
  actions: [
    "access-grants:read",
    "access-grants:create",
    "access-grants:delete",
  ],
};

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

describe("project access management", () => {
  it("sends an exact application scope with optional viewer logs permission", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    const create = vi
      .spyOn(api, "createProjectAccessGrant")
      .mockResolvedValue(grant);
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[
          {
            id: grant.scopeId,
            projectId: "project-1",
            name: "API",
          },
        ]}
        capabilities={[organizationCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    await screen.findByText("No explicit grants for this project.");
    await user.type(
      screen.getByRole("textbox", { name: /exact user id/i }),
      grant.subjectUserId!,
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Role" }),
      "viewer",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Exact scope" }),
      `application:${grant.scopeId}`,
    );
    await user.click(screen.getByRole("checkbox", { name: /logs.read/i }));
    await user.click(screen.getByRole("button", { name: "Add grant" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0]?.[0]).toBe("project-1");
    expect(create.mock.calls[0]?.[1]).toEqual({
      subjectUserId: grant.subjectUserId,
      role: "viewer",
      scopeType: "application",
      scopeId: grant.scopeId,
      permissions: ["logs.read"],
    });
    expect(create.mock.calls[0]?.[2]).toEqual(expect.any(String));
  });

  it("preserves a newer grant draft when the earlier create completes", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    let resolveCreate!: (value: AccessGrant) => void;
    const create = vi.spyOn(api, "createProjectAccessGrant").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[organizationCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    await screen.findByText("No explicit grants for this project.");
    const userId = screen.getByRole("textbox", { name: /exact user id/i });
    await user.type(userId, grant.subjectUserId!);
    await user.click(screen.getByRole("button", { name: "Add grant" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    await user.clear(userId);
    await user.type(userId, "55555555-5555-4555-8555-555555555555");

    resolveCreate(grant);
    await waitFor(() =>
      expect(userId).toHaveValue("55555555-5555-4555-8555-555555555555"),
    );
  });

  it("ignores a grant completion after project scope changes", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    let resolveCreate!: (value: AccessGrant) => void;
    const create = vi.spyOn(api, "createProjectAccessGrant").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const user = userEvent.setup();
    const { rerender } = render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[organizationCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    const oldSubject = grant.subjectUserId!;
    const userId = await screen.findByRole("textbox", {
      name: /exact user id/i,
    });
    await user.type(userId, oldSubject);
    await user.click(screen.getByRole("button", { name: "Add grant" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());

    rerender(
      <ProjectAccessPanel
        project={{ id: "project-2", name: "Billing", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[organizationCapability]}
        onClose={() => undefined}
      />,
    );
    const newUserId = await screen.findByRole("textbox", {
      name: /exact user id/i,
    });
    await user.type(newUserId, oldSubject);

    resolveCreate(grant);
    await waitFor(() => expect(newUserId).toHaveValue(oldSubject));
    expect(create.mock.calls[0]?.[0]).toBe("project-1");
  });

  it("assigns one exact team subject without sending a user subject", async () => {
    const teamGrant: AccessGrant = {
      ...grant,
      subjectUserId: undefined,
      subjectTeamId: "55555555-5555-4555-8555-555555555555",
      scopeType: "project",
      scopeId: "project-1",
    };
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({
      items: [
        {
          id: teamGrant.subjectTeamId!,
          name: "Backend",
          slug: "backend",
          createdAt: "2026-08-09T00:00:00Z",
        },
      ],
    });
    const create = vi
      .spyOn(api, "createProjectAccessGrant")
      .mockResolvedValue(teamGrant);
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[organizationCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    await user.selectOptions(
      await screen.findByRole("combobox", { name: "Subject type" }),
      "team",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Team" }),
      teamGrant.subjectTeamId!,
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Role" }),
      "viewer",
    );
    await user.click(screen.getByRole("button", { name: "Add grant" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0]?.[1]).toEqual({
      subjectTeamId: teamGrant.subjectTeamId,
      role: "viewer",
      scopeType: "project",
      scopeId: "project-1",
      permissions: [],
    });
  });

  it("requires typing the immutable grant ID before revocation", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [grant] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    const remove = vi
      .spyOn(api, "deleteProjectAccessGrant")
      .mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments" }}
        environments={[]}
        applications={[
          { id: grant.scopeId, projectId: "project-1", name: "API" },
        ]}
        capabilities={[projectCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    await user.click(await screen.findByRole("button", { name: "Remove" }));
    const revoke = screen.getByRole("button", { name: "Revoke exact grant" });
    expect(revoke).toBeDisabled();
    await user.type(
      screen.getByRole("textbox", { name: "Exact grant ID confirmation" }),
      grant.id,
    );
    expect(revoke).toBeEnabled();
    await user.click(revoke);
    await waitFor(() => expect(remove).toHaveBeenCalledOnce());
    expect(remove.mock.calls[0]?.slice(0, 2)).toEqual(["project-1", grant.id]);
    expect(remove.mock.calls[0]?.[2]).toEqual(expect.any(String));
  });

  it("uses the shared dialog escape path for revocation", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [grant] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[
          { id: grant.scopeId, projectId: "project-1", name: "API" },
        ]}
        capabilities={[projectCapability]}
        onClose={onClose}
      />,
      { wrapper: wrapper() },
    );

    await user.click(await screen.findByRole("button", { name: "Remove" }));
    expect(screen.getByRole("alertdialog")).toHaveAccessibleName(
      "Confirm the exact grant",
    );
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("reuses the idempotency key when an unchanged create attempt is retried", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    const create = vi
      .spyOn(api, "createProjectAccessGrant")
      .mockRejectedValueOnce(new Error("connection interrupted"))
      .mockResolvedValue(grant);
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments" }}
        environments={[]}
        applications={[]}
        capabilities={[projectCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    await user.type(
      await screen.findByRole("textbox", { name: /exact user id/i }),
      grant.subjectUserId!,
    );
    await user.click(screen.getByRole("button", { name: "Add grant" }));
    await screen.findByText("connection interrupted");
    await user.click(screen.getByRole("button", { name: "Add grant" }));
    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    expect(create.mock.calls[0]?.[2]).toBe(create.mock.calls[1]?.[2]);
  });

  it("does not advertise organization-level delegation to a project admin", async () => {
    const organizationGrant: AccessGrant = {
      ...grant,
      role: "organization-admin",
      scopeType: "team",
      scopeId: "team-1",
    };
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({
      items: [organizationGrant],
    });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    const user = userEvent.setup();
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[projectCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    const role = await screen.findByRole("combobox", { name: "Role" });
    expect(role).not.toHaveTextContent("Organization admin");
    expect(
      screen.queryByRole("button", { name: "Remove" }),
    ).not.toBeInTheDocument();
    await user.selectOptions(role, "project-admin");
    expect(screen.getByRole("combobox", { name: "Exact scope" })).toHaveValue(
      "project:project-1",
    );
  });

  it("does not treat a platform capability with the wrong scope ID as global", async () => {
    vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    render(
      <ProjectAccessPanel
        project={{ id: "project-1", name: "Payments", teamId: "team-1" }}
        environments={[]}
        applications={[]}
        capabilities={[forgedPlatformCapability]}
        onClose={() => undefined}
      />,
      { wrapper: wrapper() },
    );

    const scope = await screen.findByRole("combobox", { name: "Exact scope" });
    expect(scope.querySelectorAll("option")).toHaveLength(0);
    expect(
      screen.getByRole("combobox", { name: "Role" }).querySelectorAll("option"),
    ).toHaveLength(0);
  });
});
