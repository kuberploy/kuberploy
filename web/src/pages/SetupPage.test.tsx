import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
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
});
