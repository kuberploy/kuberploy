import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { EnvironmentPage } from "./EnvironmentPage";

const routeParams = vi.hoisted(() => ({
  projectId: "project-payments",
  environmentId: "environment-production",
}));
const navigate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
    className,
  }: PropsWithChildren<{
    to: string;
    params?: Record<string, string>;
    className?: string;
  }>) => {
    const href = Object.entries(params ?? {}).reduce(
      (path, [key, value]) => path.replace(`$${key}`, value),
      to,
    );
    return (
      <a href={href} className={className}>
        {children}
      </a>
    );
  },
  useParams: () => routeParams,
  useNavigate: () => navigate,
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
  vi.spyOn(api, "environment").mockResolvedValue({
    id: "environment-production",
    projectId: "project-payments",
    name: "Production",
    namespace: "payments-production",
    protectionPolicy: "protected",
    status: "active",
  });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [
      { id: "project-payments", name: "Payments", teamId: "team-platform" },
      { id: "project-other", name: "Other" },
    ],
  });
  vi.spyOn(api, "environmentApps").mockResolvedValue({
    items: [
      {
        applicationId: "application-api",
        environmentId: "environment-production",
        projectId: "project-payments",
        applicationName: "Payments API",
        applicationSlug: "payments-api",
        state: "active",
        desiredState: "running",
        createdAt: "2026-08-23T00:00:00Z",
        updatedAt: "2026-08-23T00:00:00Z",
      },
      {
        applicationId: "application-worker",
        environmentId: "environment-production",
        projectId: "project-payments",
        applicationName: "Payments Worker",
        applicationSlug: "payments-worker",
        state: "draft",
        desiredState: "stopped",
        createdAt: "2026-08-23T00:00:00Z",
        updatedAt: "2026-08-23T00:00:00Z",
      },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({
    items: [
      {
        id: "deployment-api-production",
        applicationId: "application-api",
        environmentId: "environment-production",
        image: `registry.example.com/payments@sha256:${"a".repeat(64)}`,
        runtime: {
          replicas: 1,
          ports: [{ name: "http", containerPort: 8080 }],
          resources: { requests: { cpu: "50m", memory: "100Mi" } },
        },
        state: "healthy",
      },
      {
        id: "deployment-worker-staging",
        applicationId: "application-worker",
        environmentId: "environment-staging",
        image: `registry.example.com/worker@sha256:${"b".repeat(64)}`,
        runtime: {
          replicas: 1,
          ports: [{ name: "http", containerPort: 8080 }],
          resources: { requests: { cpu: "50m", memory: "100Mi" } },
        },
        state: "healthy",
      },
    ],
  });
  vi.spyOn(api, "capabilities").mockResolvedValue({
    capabilities: [
      {
        role: "project-admin",
        scopeType: "project",
        scopeId: "project-payments",
        actions: [
          "environments:create",
          "applications:create",
          "deployments:create",
        ],
      },
    ],
  });
});

afterEach(() => {
  cleanup();
  routeParams.projectId = "project-payments";
  routeParams.environmentId = "environment-production";
  navigate.mockReset();
  vi.restoreAllMocks();
});

describe("environment App workspace", () => {
  it("enforces Project to Environment to App navigation", async () => {
    render(<EnvironmentPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Production" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("navigation", { name: "Breadcrumb" }),
    ).toHaveTextContent("Projects/Payments/Production");
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Clone Environment" }),
    ).toBeEnabled();
    expect(screen.getByRole("link", { name: "Add App" })).toHaveAttribute(
      "href",
      "/projects/project-payments/environments/environment-production/apps/new",
    );

    expect(screen.getByRole("link", { name: /Payments API/ })).toHaveAttribute(
      "href",
      "/projects/project-payments/environments/environment-production/apps/application-api",
    );
    expect(
      screen.getByRole("link", { name: /Payments Worker/ }),
    ).toHaveAttribute(
      "href",
      "/projects/project-payments/environments/environment-production/apps/application-worker",
    );
    expect(screen.getByText("Stopped / draft")).toBeInTheDocument();
    expect(screen.queryByText("Other App")).not.toBeInTheDocument();
    expect(screen.queryByText(/Service/)).not.toBeInTheDocument();
    expect(api.environment).toHaveBeenCalledWith("environment-production");
  });

  it("hides Add App when App identity creation is only environment-scoped", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      capabilities: [
        {
          role: "developer",
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["applications:create", "deployments:create"],
        },
      ],
    });

    render(<EnvironmentPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Production" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Add App" })).toBeNull();
    expect(
      screen.getByRole("button", { name: "Clone Environment" }),
    ).toBeDisabled();
  });

  it("keeps loading and empty states explicit", async () => {
    let resolveApplications!: (value: { items: [] }) => void;
    vi.mocked(api.environmentApps).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveApplications = resolve;
        }),
    );

    render(<EnvironmentPage />, { wrapper: wrapper() });

    expect(screen.getByLabelText("Loading")).toBeInTheDocument();
    resolveApplications({ items: [] });
    expect(await screen.findByText("No Apps yet")).toBeInTheDocument();
    expect(
      screen.getByText(/Add an App to this environment/),
    ).toBeInTheDocument();
  });

  it("clones through a dialog as stopped drafts", async () => {
    const user = userEvent.setup();
    const clone = vi.spyOn(api, "cloneEnvironment").mockResolvedValue({
      environment: {
        id: "environment-production-copy",
        projectId: "project-payments",
        name: "Production copy",
        namespace: "payments-production-copy",
      },
      appPlacements: [],
    });
    render(<EnvironmentPage />, { wrapper: wrapper() });

    await user.click(
      await screen.findByRole("button", { name: "Clone Environment" }),
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "Apps become stopped drafts",
    );
    const name = screen.getByLabelText("New environment name *");
    await user.clear(name);
    await user.type(name, "Production copy");
    await user.click(
      screen.getByRole("button", { name: "Clone as stopped drafts" }),
    );

    expect(clone).toHaveBeenCalledWith(
      "environment-production",
      { name: "Production copy", protectionPolicy: undefined },
      expect.any(String),
    );
    expect(navigate).toHaveBeenCalledWith({
      to: "/projects/$projectId/environments/$environmentId",
      params: {
        projectId: "project-payments",
        environmentId: "environment-production-copy",
      },
    });
  });

  it("shows API errors through shared error UI", async () => {
    vi.mocked(api.environment).mockRejectedValue(
      new Error("environment lookup failed"),
    );

    render(<EnvironmentPage />, { wrapper: wrapper() });

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "environment lookup failed",
    );
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});
