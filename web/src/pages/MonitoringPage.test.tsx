import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { MetricRangeResult } from "../api/types";
import { MonitoringPage, monitoringMetricCatalog } from "./MonitoringPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

function wrapperWithClient() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return {
    queryClient,
    Wrapper: ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  };
}

function resources() {
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [
      { id: "project-payments", name: "Payments", teamId: "team-commerce" },
      { id: "project-search", name: "Search", teamId: "team-discovery" },
    ],
  });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment-opaque-production",
        projectId: "project-payments",
        name: "Production",
        namespace: "payments-production",
      },
      {
        id: "environment-opaque-restricted",
        projectId: "project-search",
        name: "Restricted",
        namespace: "restricted-secret-namespace",
      },
    ],
  });
}

function availableMonitoring() {
  vi.spyOn(api, "monitoringStatus").mockResolvedValue({
    mode: "managed",
    status: "available",
    available: true,
    message: "Monitoring is available.",
    observedAt: "2026-08-09T00:05:00Z",
  });
}

function metricResult(metric: MetricRangeResult["metric"]): MetricRangeResult {
  const singleSeries =
    metric === "http-error-ratio" || metric === "http-latency-p95";
  return {
    metric,
    scope: "namespace",
    series: Array.from({ length: singleSeries ? 1 : 2 }, (_, index) => ({
      labels: { kuberploy_service: `service-${index + 1}` },
      samples: [{ timestamp: "2026-08-09T00:04:00Z", value: index + 1 }],
    })),
    observedAt: "2026-08-09T00:05:00Z",
  };
}

describe("monitoring dashboards", () => {
  it("queries only the closed catalog for an exactly covered namespace scope", async () => {
    resources();
    availableMonitoring();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: ["metrics:read"],
      features: { metrics: false, monitoring: false },
      capabilities: [
        {
          role: "viewer",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["metrics:read"],
        },
      ],
    });
    const metrics = vi
      .spyOn(api, "metricRange")
      .mockImplementation(async (input) => metricResult(input.metric));

    const { container } = render(<MonitoringPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Payments / Production" }),
    ).toBeInTheDocument();
    const selector = screen.getByRole("combobox", {
      name: "Monitoring scope",
    });
    await openSelect(selector);
    expect(
      screen.getByRole("option", {
        name: "Namespace · Payments / Production",
      }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Restricted/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: /Global/ }),
    ).not.toBeInTheDocument();
    expect(await screen.findByText("3.00 cores")).toBeInTheDocument();
    expect(
      container.querySelector('[data-slot="monitoring-content"]'),
    ).toHaveClass("min-w-0", "[&>*]:min-w-0");

    await waitFor(() => expect(metrics).toHaveBeenCalledTimes(7));
    const inputs = metrics.mock.calls.map(([input]) => input);
    expect(inputs.map((input) => input.metric).sort()).toEqual(
      monitoringMetricCatalog.map(({ key }) => key).sort(),
    );
    expect(
      inputs.every(
        (input) =>
          input.scopeType === "namespace" &&
          input.scopeId === "environment-opaque-production" &&
          input.stepSeconds === 60 &&
          input.to.getTime() - input.from.getTime() === 30 * 60_000,
      ),
    ).toBe(true);
    expect(inputs.some((input) => input.scopeId.includes("namespace"))).toBe(
      false,
    );
  });

  it("does not turn feature flags or coarse action unions into access", async () => {
    resources();
    availableMonitoring();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      actions: ["metrics:read"],
      features: { metrics: true, monitoring: true },
      capabilities: [
        {
          role: "viewer",
          scopeType: "application",
          scopeId: "application-payments",
          actions: ["metrics:read"],
        },
      ],
    });
    const metrics = vi.spyOn(api, "metricRange");

    render(<MonitoringPage />, { wrapper: wrapper() });

    expect(await screen.findByText("No monitoring scope")).toBeInTheDocument();
    expect(metrics).not.toHaveBeenCalled();
  });

  it("adds global queries only for an explicit platform-admin capability", async () => {
    const user = userEvent.setup();
    resources();
    availableMonitoring();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["metrics:read"],
        },
      ],
    });
    const metrics = vi
      .spyOn(api, "metricRange")
      .mockImplementation(async (input) => ({
        ...metricResult(input.metric),
        scope: input.scopeType,
      }));

    render(<MonitoringPage />, { wrapper: wrapper() });

    const selector = await screen.findByRole("combobox", {
      name: "Monitoring scope",
    });
    await openSelect(selector);
    expect(
      screen.getByRole("option", { name: "Global · Platform global" }),
    ).toBeInTheDocument();
    await selectOption(selector, "global:platform");

    expect(
      await screen.findByRole("heading", { name: "Platform global" }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(
        metrics.mock.calls.filter(
          ([input]) =>
            input.scopeType === "global" && input.scopeId === "platform",
        ),
      ).toHaveLength(7),
    );
  });

  it("reconciles a selected scope when access refresh removes it", async () => {
    const user = userEvent.setup();
    resources();
    availableMonitoring();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["metrics:read"],
        },
      ],
    });
    vi.spyOn(api, "metricRange").mockImplementation(async (input) =>
      metricResult(input.metric),
    );
    const { queryClient, Wrapper } = wrapperWithClient();

    render(<MonitoringPage />, { wrapper: Wrapper });
    const selector = await screen.findByRole("combobox", {
      name: "Monitoring scope",
    });
    await selectOption(selector, "namespace:environment-opaque-restricted");
    expect(selector).toHaveValue("namespace:environment-opaque-restricted");

    queryClient.setQueryData(["environments"], {
      items: [
        {
          id: "environment-opaque-production",
          projectId: "project-payments",
          name: "Production",
          namespace: "payments-production",
        },
      ],
    });

    await waitFor(() =>
      expect(selector).toHaveValue("namespace:environment-opaque-production"),
    );
    expect(
      screen.getByRole("heading", { name: "Payments / Production" }),
    ).toBeInTheDocument();
  });

  it("does not query metrics while monitoring is explicitly unavailable", async () => {
    resources();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "viewer",
          scopeType: "environment",
          scopeId: "environment-opaque-production",
          actions: ["metrics:read"],
        },
      ],
    });
    vi.spyOn(api, "monitoringStatus").mockResolvedValue({
      mode: "existing",
      status: "unavailable",
      available: false,
      message: "The provider is offline; values are unknown.",
    });
    const metrics = vi.spyOn(api, "metricRange");

    render(<MonitoringPage />, { wrapper: wrapper() });

    expect(
      await screen.findByText("Metrics are explicitly unavailable"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("The provider is offline; values are unknown."),
    ).toBeInTheDocument();
    expect(screen.getAllByText("—")).toHaveLength(7);
    expect(metrics).not.toHaveBeenCalled();
  });

  it("distinguishes query errors and empty series from a measured zero", async () => {
    resources();
    availableMonitoring();
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "viewer",
          scopeType: "environment",
          scopeId: "environment-opaque-production",
          actions: ["metrics:read"],
        },
      ],
    });
    vi.spyOn(api, "metricRange").mockImplementation(async (input) => {
      if (input.metric === "cpu-usage") throw new Error("gateway offline");
      return {
        metric: input.metric,
        scope: input.scopeType,
        series: [],
        observedAt: "2026-08-09T00:05:00Z",
      };
    });

    render(<MonitoringPage />, { wrapper: wrapper() });

    expect(await screen.findByText("Query failed")).toBeInTheDocument();
    expect(screen.getAllByText("No data")).toHaveLength(6);
    expect(
      screen.getByText(
        "The bounded gateway query failed; this is not reported as zero.",
      ),
    ).toBeInTheDocument();
    expect(screen.getAllByText("—")).toHaveLength(7);
  });
});
