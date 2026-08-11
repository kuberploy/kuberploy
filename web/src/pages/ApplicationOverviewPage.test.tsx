import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { ApplicationOverviewPage } from "./ApplicationOverviewPage";

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
  useParams: () => ({ applicationId: "application-1" }),
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
  vi.spyOn(api, "application").mockResolvedValue({
    id: "application-1",
    projectId: "project-1",
    name: "Payments API",
  });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [{ id: "project-1", name: "Payments" }],
  });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment-1",
        projectId: "project-1",
        name: "Test",
        namespace: "test",
      },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
  vi.spyOn(api, "buildDefinitions").mockResolvedValue({
    items: [],
    nextCursor: null,
  });
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: { builds: false, builder: false, helmDeployments: false },
    capabilities: [],
  });
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-1",
    displayName: "User",
    role: "developer",
    authentication: { kind: "session" },
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("application source overview", () => {
  it("offers build, image, and Helm as peer first-use choices", async () => {
    const user = userEvent.setup();
    render(<ApplicationOverviewPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Payments API" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Overview" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByText("No deployment yet")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Source & build" }));
    expect(
      screen.getByRole("tab", { name: "GitHub / Dockerfile" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("tab", { name: "Existing image" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Helm / OCI" })).toBeInTheDocument();

    await user.click(screen.getByRole("tab", { name: "Helm / OCI" }));
    expect(
      screen.getByRole("combobox", { name: /Environment/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Helm applications are not ready"),
    ).toBeInTheDocument();
  });

  it("summarizes the active GitHub build source", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { builds: true, builder: true },
      capabilities: [
        {
          role: "developer",
          scopeType: "project",
          scopeId: "project-1",
          actions: ["build-definitions:read"],
        },
      ],
    });
    vi.mocked(api.buildDefinitions).mockResolvedValue({
      items: [
        {
          id: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          installationId: "installation-1",
          repositoryId: "repository-1",
          triggerRef: "refs/heads/main",
          contextPath: ".",
          dockerfilePath: "Dockerfile",
          platforms: ["linux/amd64"],
          registry: {
            targetId: "target-1",
            mode: "managed",
            server: "registry.example.com",
            repositoryPrefix: "payments",
          },
          buildArgs: [],
          secretFiles: [],
          sshFiles: [],
          cacheTrustLane: "main",
          cacheImports: 1,
          profile: {
            resource: "standard",
            timeoutSeconds: 900,
            egress: "registry-and-source",
          },
          maxAttempts: 3,
          definitionDigest: `sha256:${"b".repeat(64)}`,
          definitionGeneration: 1,
          enabled: true,
          createdAt: "2026-08-12T00:00:00Z",
          updatedAt: "2026-08-12T00:00:00Z",
        },
      ],
      nextCursor: null,
    });

    render(<ApplicationOverviewPage />, { wrapper: wrapper() });

    expect(await screen.findByText("GitHub / main")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Manage source/ }),
    ).toBeInTheDocument();
  });
});
