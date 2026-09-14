import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
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
    vi.spyOn(api, "operations").mockResolvedValue({
      items: [],
      truncated: false,
    });
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
    expect(api.operations).toHaveBeenCalledWith(50);
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
    vi.spyOn(api, "operations").mockResolvedValue({
      items: [],
      truncated: false,
    });
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

  it("clears stale workspace data after a refresh failure and retries it", async () => {
    const staleDeployment = {
      id: "deployment-stale",
      applicationId: "application-stale",
      environmentId: "environment-stale",
      name: "Stale App",
      image: "registry.example/stale@sha256:abc",
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080 }],
        resources: { requests: { cpu: "50m", memory: "64Mi" } },
      },
    };
    const deployments = vi
      .spyOn(api, "deployments")
      .mockResolvedValueOnce({ items: [staleDeployment] })
      .mockRejectedValueOnce(new Error("deployments unavailable"))
      .mockResolvedValue({ items: [] });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({
      items: [
        {
          id: "application-stale",
          projectId: "project-stale",
          name: "Stale App",
        },
      ],
    });
    vi.spyOn(api, "operations").mockResolvedValue({
      items: [],
      truncated: false,
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: [],
      features: {},
    });
    vi.spyOn(api, "monitoringStatus").mockResolvedValue({
      mode: "disabled",
      available: false,
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const user = userEvent.setup();

    render(
      <QueryClientProvider client={queryClient}>
        <DashboardPage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Stale App")).toBeVisible();
    await queryClient.refetchQueries({ queryKey: ["deployments"] });
    expect(await screen.findByText("deployments unavailable")).toBeVisible();
    expect(screen.queryByText("Stale App")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(screen.queryByText("deployments unavailable")).toBeNull(),
    );
    expect(await screen.findByText("No Apps running yet")).toBeVisible();
    expect(deployments).toHaveBeenCalledTimes(3);
  });
});
