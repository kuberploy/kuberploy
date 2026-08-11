import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { ProjectPage } from "./ProjectPage";

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
  useParams: () => ({ projectId: "project_payments" }),
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
          "access-grants:read",
          "access-grants:create",
          "deployment-config:read",
        ],
      },
    ],
  });
  vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [{ id: "project_payments", name: "Payments" }],
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
      },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
  vi.spyOn(api, "projectAccessGrants").mockResolvedValue({ items: [] });
  vi.spyOn(api, "users").mockResolvedValue({ items: [] });
  vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [] });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("project workspace", () => {
  it("keeps services, environments, and administration in focused tabs", async () => {
    const user = userEvent.setup();
    render(<ProjectPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Payments" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /Payments API/ }),
    ).toBeInTheDocument();
    expect(screen.queryByText("payments-production")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Environments (1)" }));
    expect(screen.getByText("payments-production")).toBeInTheDocument();
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
});
