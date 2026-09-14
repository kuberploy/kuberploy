import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type { BuildAttempt, BuildLogSnapshot } from "../api/types";
import { BuildDetailPage } from "./BuildDetailPage";

const routeParams = vi.hoisted(() => ({ buildId: "attempt-safe" }));

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
  useParams: () => routeParams,
}));

const attempt: BuildAttempt = {
  id: "attempt-safe",
  sourceId: "source-safe",
  projectId: "project-safe",
  applicationId: "application-safe",
  commitSha: "b".repeat(40),
  gitRef: "refs/heads/main",
  generation: 1,
  state: "failed",
  executionAttempts: 1,
  maxAttempts: 3,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const secondAttempt: BuildAttempt = {
  ...attempt,
  id: "attempt-other",
  state: "running",
};

const logSnapshot: BuildLogSnapshot = {
  source: { id: `build_${"a".repeat(32)}`, ready: true, previous: false },
  lines: [
    {
      type: "line",
      source: { id: `build_${"a".repeat(32)}`, ready: true, previous: false },
      message: "Builder output is ready",
      truncated: false,
    },
  ],
  bytes: 23,
  truncated: false,
  observedAt: "2026-08-09T00:05:00Z",
};

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  routeParams.buildId = "attempt-safe";
  vi.restoreAllMocks();
});

describe("build detail log availability", () => {
  it("surfaces a capability lookup failure and retries it", async () => {
    const user = userEvent.setup();
    const capabilities = vi
      .spyOn(api, "capabilities")
      .mockRejectedValueOnce(new Error("capabilities unavailable"))
      .mockResolvedValue({ features: { builds: false, buildLogs: false } });
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-safe",
      displayName: "Build observer",
      role: "viewer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "buildAttempt").mockResolvedValue(attempt);
    vi.spyOn(api, "application").mockResolvedValue({
      id: "application-safe",
      projectId: "project-safe",
      name: "API",
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project-safe", name: "Payments" }],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <BuildDetailPage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("capabilities unavailable")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(
      await screen.findByText("Source builds are not ready"),
    ).toBeVisible();
    expect(capabilities).toHaveBeenCalledTimes(2);
  });

  it("waits through preparation and recovers when the running builder source appears", async () => {
    vi.useFakeTimers();
    const logs = vi
      .spyOn(api, "buildLogSnapshot")
      .mockRejectedValueOnce(
        new ApiError(404, {
          title: "Source not found",
          status: 404,
          detail: "Builder Pod is not available yet.",
        }),
      )
      .mockResolvedValue(logSnapshot);
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-safe",
      displayName: "Build observer",
      role: "viewer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { buildLogs: true },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: ["builds:read", "logs:read"],
        },
      ],
    });
    vi.spyOn(api, "buildAttempt").mockResolvedValue({
      ...attempt,
      state: "queued",
    });
    vi.spyOn(api, "application").mockResolvedValue({
      id: "application-safe",
      projectId: "project-safe",
      name: "API",
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project-safe", name: "Payments" }],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <BuildDetailPage />
      </QueryClientProvider>,
    );
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(
      screen.getByRole("heading", { name: "Waiting for a builder" }),
    ).toBeInTheDocument();
    expect(logs).not.toHaveBeenCalled();
    act(() =>
      queryClient.setQueryData(["build-attempt", "attempt-safe"], {
        ...attempt,
        state: "preparing",
      }),
    );
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(
      screen.getByRole("heading", { name: "Preparing the builder" }),
    ).toBeInTheDocument();
    expect(logs).not.toHaveBeenCalled();
    vi.mocked(api.buildAttempt).mockResolvedValue({
      ...attempt,
      state: "running",
    });
    act(() =>
      queryClient.setQueryData(["build-attempt", "attempt-safe"], {
        ...attempt,
        state: "running",
      }),
    );
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(logs).toHaveBeenCalledTimes(1);
    expect(
      screen.getByRole("heading", { name: "Waiting for build logs" }),
    ).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(5_020));
    expect(screen.getByText("Builder output is ready")).toBeInTheDocument();
    expect(logs).toHaveBeenCalledTimes(2);
  });

  it("shows logs with exact builds.read and logs.read without expanding to definition access", async () => {
    vi.spyOn(api, "buildLogSnapshot").mockResolvedValue(logSnapshot);
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-safe",
      displayName: "Build observer",
      role: "viewer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { builds: false, builder: false, buildLogs: true },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: ["builds:read", "logs:read"],
        },
      ],
    });
    vi.spyOn(api, "buildAttempt").mockResolvedValue(attempt);
    vi.spyOn(api, "application").mockResolvedValue({
      id: "application-safe",
      projectId: "project-safe",
      name: "API",
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project-safe", name: "Payments", teamId: "team-safe" }],
    });
    const definitionRead = vi.spyOn(api, "buildDefinition");
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <BuildDetailPage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("Builder output is ready"),
    ).toBeInTheDocument();
    expect(definitionRead).not.toHaveBeenCalled();
  });

  it("does not carry a retry notice into another build route", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-safe",
      displayName: "Build operator",
      role: "developer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { builds: true, builder: true, buildLogs: false },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: ["builds:read", "builds:retry"],
        },
      ],
    });
    vi.spyOn(api, "buildAttempt").mockImplementation(async (id) =>
      id === attempt.id ? attempt : secondAttempt,
    );
    vi.spyOn(api, "application").mockResolvedValue({
      id: "application-safe",
      projectId: "project-safe",
      name: "API",
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project-safe", name: "Payments", teamId: "team-safe" }],
    });
    vi.spyOn(api, "retryBuildAttempt").mockResolvedValue({
      ...attempt,
      id: "attempt-retry",
      state: "queued",
    });
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });

    const view = render(
      <QueryClientProvider client={queryClient}>
        <BuildDetailPage />
      </QueryClientProvider>,
    );

    await screen.findByRole("heading", { name: /Build attempt-safe/ });
    await user.click(screen.getByRole("button", { name: /Retry build/ }));
    await user.type(
      screen.getByRole("textbox", { name: /Type attempt-safe/ }),
      attempt.id,
    );
    await user.click(screen.getByRole("button", { name: "Confirm retry" }));
    expect(await screen.findByText("Retry queued")).toBeInTheDocument();

    routeParams.buildId = secondAttempt.id;
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <BuildDetailPage />
      </QueryClientProvider>,
    );

    await waitFor(() =>
      expect(screen.queryByText("Retry queued")).not.toBeInTheDocument(),
    );
    expect(
      await screen.findByRole("heading", { name: /Build attempt-othe/ }),
    ).toBeInTheDocument();
  });
});
