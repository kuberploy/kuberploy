import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { ProjectsPage } from "./ProjectsPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    className,
  }: PropsWithChildren<{ to: string; className?: string }>) => (
    <a href={to} className={className}>
      {children}
    </a>
  ),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("project team ownership", () => {
  it("defaults environments to protected review and sends development only when selected", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Platform admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["environments:create"],
        },
      ],
    });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project_payments", name: "Payments" }],
    });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    const createEnvironment = vi
      .spyOn(api, "createEnvironment")
      .mockImplementation(async (input) => ({
        id: `environment_${input.name.toLowerCase()}`,
        projectId: input.projectId,
        name: input.name,
        namespace: `payments-${input.name.toLowerCase()}`,
        protectionPolicy: input.protectionPolicy,
      }));
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<ProjectsPage />, { wrapper: Wrapper });

    await user.click(
      await screen.findByRole("button", { name: "Environment" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project_payments",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Name" }),
      "Production",
    );
    expect(
      screen.getByRole("combobox", { name: "Git publication" }),
    ).toHaveValue("protected");
    await user.click(
      screen.getByRole("button", { name: "Create environment" }),
    );
    await waitFor(() => expect(createEnvironment).toHaveBeenCalledOnce());
    expect(createEnvironment.mock.calls[0]?.[0]).toEqual({
      projectId: "project_payments",
      name: "Production",
      slug: undefined,
      protectionPolicy: "protected",
    });

    await user.click(screen.getByRole("button", { name: "Environment" }));
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project_payments",
    );
    await user.type(screen.getByRole("textbox", { name: "Name" }), "Preview");
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Git publication" }),
      "development",
    );
    await user.click(
      screen.getByRole("button", { name: "Create environment" }),
    );
    await waitFor(() => expect(createEnvironment).toHaveBeenCalledTimes(2));
    expect(createEnvironment.mock.calls[1]?.[0]).toEqual({
      projectId: "project_payments",
      name: "Preview",
      slug: undefined,
      protectionPolicy: "development",
    });
  });

  it("preserves a newer environment draft when the earlier create completes", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Platform admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["environments:create"],
        },
      ],
    });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project_payments", name: "Payments" }],
    });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    let resolveCreate!: (
      value: Awaited<ReturnType<typeof api.createEnvironment>>,
    ) => void;
    const createEnvironment = vi
      .spyOn(api, "createEnvironment")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveCreate = resolve;
          }),
      );
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsPage />
      </QueryClientProvider>,
    );

    await user.click(
      await screen.findByRole("button", { name: "Environment" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project_payments",
    );
    const name = screen.getByRole("textbox", { name: "Name" });
    await user.type(name, "First staging");
    await user.click(
      screen.getByRole("button", { name: "Create environment" }),
    );
    await waitFor(() => expect(createEnvironment).toHaveBeenCalledOnce());
    await user.clear(name);
    await user.type(name, "Newer staging");

    resolveCreate({} as Awaited<ReturnType<typeof api.createEnvironment>>);
    await waitFor(() => expect(name).toHaveValue("Newer staging"));
  });

  it("lets an admin choose ownership and never renders an unavailable team ID", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Platform admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: [
            "projects:create",
            "environments:create",
            "access-grants:create",
          ],
        },
      ],
    });
    vi.spyOn(api, "teams").mockResolvedValue({
      items: [
        {
          id: "team_visible",
          name: "Payments team",
          slug: "payments-team",
          createdAt: "2026-08-06T00:00:00Z",
        },
      ],
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [
        {
          id: "project_visible",
          name: "Billing",
          slug: "billing",
          teamId: "team_visible",
        },
        {
          id: "project_restricted",
          name: "Restricted project",
          slug: "restricted-project",
          teamId: "team_unavailable_secret_id",
        },
        {
          id: "project_platform",
          name: "Platform services",
          slug: "platform-services",
        },
      ],
    });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    const createProject = vi
      .spyOn(api, "createProject")
      .mockImplementation(async (input) => ({
        id: `project_${input.name.toLowerCase().replaceAll(" ", "_")}`,
        name: input.name,
        slug: input.slug,
        teamId: input.teamId,
      }));

    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<ProjectsPage />, { wrapper: Wrapper });

    expect(await screen.findByText("Payments team")).toBeInTheDocument();
    expect(screen.getByText("Team-scoped")).toBeInTheDocument();
    expect(screen.getByText("Platform-only")).toBeInTheDocument();
    expect(
      screen.queryByText("team_unavailable_secret_id"),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Project" }));
    const ownership = screen.getByRole("combobox", {
      name: /team ownership/i,
    });
    expect(ownership).toHaveValue("");
    expect(
      screen.getByRole("option", { name: "Platform-only" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("option", { name: "Payments team" }),
    ).toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: /^name/i }), "Orders");
    await user.selectOptions(ownership, "team_visible");
    await user.click(screen.getByRole("button", { name: /create project/i }));
    await waitFor(() => expect(createProject).toHaveBeenCalledOnce());
    expect(createProject.mock.calls[0]?.[0]).toEqual({
      name: "Orders",
      slug: undefined,
      teamId: "team_visible",
    });

    await user.click(screen.getByRole("button", { name: "Project" }));
    await user.type(
      screen.getByRole("textbox", { name: /^name/i }),
      "Control plane",
    );
    await user.click(screen.getByRole("button", { name: /create project/i }));
    await waitFor(() => expect(createProject).toHaveBeenCalledTimes(2));
    expect(createProject.mock.calls[1]?.[0]).toEqual({
      name: "Control plane",
      slug: undefined,
      teamId: undefined,
    });
  });

  it("requires a developer to place a new project in an accessible team", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_developer",
      displayName: "Developer",
      role: "developer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "developer",
          scopeType: "team",
          scopeId: "team_product",
          actions: ["projects:create", "environments:create"],
        },
      ],
    });
    vi.spyOn(api, "teams").mockResolvedValue({
      items: [
        {
          id: "team_product",
          name: "Product team",
          slug: "product-team",
          createdAt: "2026-08-06T00:00:00Z",
        },
      ],
    });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    const createProject = vi.spyOn(api, "createProject").mockResolvedValue({
      id: "project_storefront",
      name: "Storefront",
      slug: "storefront",
      teamId: "team_product",
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<ProjectsPage />, { wrapper: Wrapper });

    await screen.findByText("Create your first project");
    await user.click(screen.getByRole("button", { name: "Project" }));
    await user.type(
      screen.getByRole("textbox", { name: /^name/i }),
      "Storefront",
    );
    await user.click(screen.getByRole("button", { name: /create project/i }));

    expect(
      await screen.findByText("Select a team for this project."),
    ).toBeInTheDocument();
    expect(createProject).not.toHaveBeenCalled();

    await user.selectOptions(
      screen.getByRole("combobox", { name: /team ownership/i }),
      "team_product",
    );
    await user.click(screen.getByRole("button", { name: /create project/i }));
    await waitFor(() => expect(createProject).toHaveBeenCalledOnce());
    expect(createProject.mock.calls[0]?.[0]).toEqual({
      name: "Storefront",
      slug: undefined,
      teamId: "team_product",
    });
  });

  it("keeps the project index compact and sends project administration to the workspace", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Project admin",
      role: "project-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { serviceAccounts: true },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project_payments",
          actions: [
            "access-grants:read",
            "access-grants:create",
            "access-grants:delete",
          ],
        },
      ],
    });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project_payments", name: "Payments" }],
    });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    render(<ProjectsPage />, { wrapper: Wrapper });

    expect(
      await screen.findByRole("heading", { name: "Payments" }),
    ).toBeInTheDocument();
    expect(screen.getByText("0 Apps")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Automation" })).toBeNull();
    expect(screen.getByRole("link", { name: /Payments/ })).toHaveAttribute(
      "href",
      "/projects/$projectId",
    );
  });
});
