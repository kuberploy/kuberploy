import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { DashboardPage } from "./DashboardPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: PropsWithChildren) => <a href="/deploy">{children}</a>,
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("dashboard platform health", () => {
  it("distinguishes healthy, unavailable, and disabled dependencies", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "operations").mockResolvedValue({ items: [] });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: [],
      features: {},
      featureStates: {
        gitops: "healthy",
        argoCD: "unavailable",
        edge: "disabled",
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
        <DashboardPage />
      </QueryClientProvider>,
    );

    const health = await screen.findByRole("region", {
      name: "Platform health",
    });
    expect(screen.getByText("App instances")).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Recent Apps" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("No Apps running yet")).toBeInTheDocument();
    expect(screen.queryByText("Deployments")).toBeNull();
    await waitFor(() => {
      expect(health).toHaveTextContent("GitOpsHealthy");
      expect(health).toHaveTextContent("Argo CDUnavailable");
      expect(health).toHaveTextContent("EdgeDisabled");
      expect(health).toHaveTextContent("MonitoringUnavailable");
    });
  });

  it("falls back to feature flags and fails closed on status errors", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "operations").mockResolvedValue({ items: [] });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: [],
      features: { gitops: true, argoCD: false, edge: false },
    });
    vi.spyOn(api, "monitoringStatus").mockRejectedValue(
      new Error("monitoring unavailable"),
    );
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <DashboardPage />
      </QueryClientProvider>,
    );

    const health = await screen.findByRole("region", {
      name: "Platform health",
    });
    await waitFor(() => {
      expect(health).toHaveTextContent("GitOpsHealthy");
      expect(health).toHaveTextContent("Argo CDDisabled");
      expect(health).toHaveTextContent("EdgeDisabled");
      expect(health).toHaveTextContent("MonitoringUnavailable");
    });
  });
});
