import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { SetupPage } from "./SetupPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Setup page", () => {
  it("reports and renders each effective action only once", async () => {
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "dev",
      apiVersion: "v1",
      contractDigest: `sha256:${"a".repeat(64)}`,
      bootstrapRequired: false,
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: ["projects:read", "teams:read"],
      features: {},
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["projects:read", "teams:read"],
        },
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-1",
          actions: ["projects:read"],
        },
      ],
    });
    vi.spyOn(api, "monitoringStatus").mockResolvedValue({
      mode: "disabled",
      available: false,
      message: "Monitoring is disabled.",
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <SetupPage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("2 reported")).toBeVisible();
    expect(screen.getAllByText("projects:read")).toHaveLength(1);
    expect(screen.getAllByText("teams:read")).toHaveLength(1);
  });

  it("distinguishes configured-but-unavailable runtimes from disabled features", async () => {
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "dev",
      bootstrapRequired: false,
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: [],
      features: { git: false, argo: false, edge: false, builds: false },
      featureStates: {
        git: "unavailable",
        argo: "unavailable",
        edge: "disabled",
        builds: "disabled",
      },
    });
    vi.spyOn(api, "monitoringStatus").mockResolvedValue({
      mode: "managed",
      status: "unavailable",
      available: false,
      message: "Monitoring identity is not ready.",
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <SetupPage />
      </QueryClientProvider>,
    );

    expect(
      (await screen.findAllByText("Unavailable")).length,
    ).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("Disabled").length).toBeGreaterThanOrEqual(2);
    const prometheus = screen
      .getByText("Prometheus")
      .closest<HTMLElement>('[data-slot="system-row"]');
    expect(prometheus).not.toBeNull();
    expect(within(prometheus!).getByText("Unavailable")).toBeVisible();
    expect(within(prometheus!).queryByText("Pending")).not.toBeInTheDocument();
  });
});
