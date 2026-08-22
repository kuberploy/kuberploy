import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import { ProjectPage } from "./ProjectPage";

const routeParams = vi.hoisted(() => ({ projectId: "project_payments" }));

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    className,
    search,
    params,
  }: PropsWithChildren<{
    to: string;
    className?: string;
    search?: { projectId?: string; environmentId?: string };
    params?: Record<string, string>;
  }>) => (
    <a
      href={
        search
          ? `${to}?${new URLSearchParams(
              Object.entries(search).filter(
                (entry): entry is [string, string] => Boolean(entry[1]),
              ),
            )}`
          : Object.entries(params ?? {}).reduce(
              (path, [key, value]) => path.replace(`$${key}`, value),
              to,
            )
      }
      className={className}
    >
      {children}
    </a>
  ),
  useParams: () => routeParams,
}));

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

beforeEach(() => {
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user_admin",
    displayName: "Project admin",
    role: "project-admin",
    authentication: { kind: "session" },
  });
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: { serviceAccounts: true, variableSets: true },
    capabilities: [
      {
        role: "project-admin",
        scopeType: "project",
        scopeId: "project_payments",
        actions: [
          "environments:create",
          "applications:create",
          "deployments:create",
          "access-grants:read",
          "access-grants:create",
          "deployment-config:read",
        ],
      },
    ],
  });
  vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [
      { id: "project_payments", name: "Payments" },
      { id: "project_other", name: "Other" },
    ],
  });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment_production",
        projectId: "project_payments",
        name: "Production",
        namespace: "payments-production",
        protectionPolicy: "protected",
      },
    ],
  });
  vi.spyOn(api, "applications").mockResolvedValue({
    items: [
      {
        id: "application_api",
        projectId: "project_payments",
        name: "Payments API",
        description: "Handles payment requests",
      },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({
    items: [
      {
        id: "deployment_api",
        applicationId: "application_api",
        environmentId: "environment_production",
        image: "registry.example.com/payments@sha256:" + "a".repeat(64),
        runtime: {
          replicas: 1,
          ports: [{ name: "http", containerPort: 8080 }],
          resources: { requests: { cpu: "50m", memory: "100Mi" } },
        },
        status: "git-committed",
      },
    ],
  });
  vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
  vi.spyOn(api, "users").mockResolvedValue({ items: [] });
  vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [] });
});

afterEach(() => {
  cleanup();
  routeParams.projectId = "project_payments";
  vi.restoreAllMocks();
});

describe("project workspace", () => {
  it("makes environments the only path to Apps", async () => {
    const user = userEvent.setup();
    render(<ProjectPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Payments" }),
    ).toBeInTheDocument();
    expect(screen.getByText("payments-production")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Production" })).toHaveAttribute(
      "href",
      "/projects/project_payments/environments/environment_production",
    );
    expect(screen.getByText("1 App")).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: /add (service|app)/i }),
    ).toBeNull();
    expect(
      screen.getByRole("button", { name: "Environment" }),
    ).toBeInTheDocument();

    await user.click(
      screen.getByRole("button", { name: "Access & automation" }),
    );
    expect(
      await screen.findByRole("heading", { name: "Service accounts" }),
    ).toBeInTheDocument();
  });

  it("preserves a newer environment draft when the earlier create completes", async () => {
    let resolveCreate!: (value: {
      id: string;
      projectId: string;
      name: string;
      namespace: string;
      protectionPolicy: "protected";
    }) => void;
    const create = vi.spyOn(api, "createEnvironment").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const user = userEvent.setup();
    render(<ProjectPage />, { wrapper: wrapper() });

    await screen.findByRole("button", { name: "Environments (1)" });
    await user.click(screen.getByRole("button", { name: "Environment" }));
    const name = screen.getByRole("textbox", { name: "Environment name" });
    await user.type(name, "Staging");
    await user.click(
      screen.getByRole("button", { name: "Create environment" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    await user.clear(name);
    await user.type(name, "Newer staging");

    resolveCreate({
      id: "environment_staging",
      projectId: "project_payments",
      name: "Staging",
      namespace: "payments-staging",
      protectionPolicy: "protected",
    });
    await waitFor(() => expect(name).toHaveValue("Newer staging"));
  });

  it("clears a Git panel selection when its environment leaves access scope", async () => {
    vi.spyOn(api, "environmentGitBinding").mockRejectedValue(
      new ApiError(404, { title: "Not found" }),
    );
    const { queryClient, Wrapper } = (() => {
      const client = new QueryClient({
        defaultOptions: { queries: { retry: false } },
      });
      return {
        queryClient: client,
        Wrapper: ({ children }: PropsWithChildren) => (
          <QueryClientProvider client={client}>{children}</QueryClientProvider>
        ),
      };
    })();
    const user = userEvent.setup();
    render(<ProjectPage />, { wrapper: Wrapper });

    await screen.findByRole("button", { name: "Environments (1)" });
    await user.click(screen.getByRole("button", { name: "Git" }));
    expect(
      await screen.findByLabelText("Production Git authority"),
    ).toBeInTheDocument();

    queryClient.setQueryData(["environments"], { items: [] });

    await waitFor(() =>
      expect(screen.queryByLabelText("Production Git authority")).toBeNull(),
    );
  });

  it("resets workspace state when navigating to another project", async () => {
    const user = userEvent.setup();
    const view = render(<ProjectPage />, { wrapper: wrapper() });

    await screen.findByRole("heading", { name: "Payments" });
    await user.click(
      screen.getByRole("button", { name: "Access & automation" }),
    );

    routeParams.projectId = "project_other";
    view.rerender(<ProjectPage />);

    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Environments (0)" }),
      ).toHaveAttribute("aria-current", "page"),
    );
    expect(screen.getByRole("heading", { name: "Other" })).toBeInTheDocument();
  });
});
