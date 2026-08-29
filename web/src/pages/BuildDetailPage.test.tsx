import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { BuildAttempt } from "../api/types";
import { BuildDetailPage } from "./BuildDetailPage";

vi.mock("../components/BuildLogsPanel", () => ({
  BuildLogsPanel: ({ attemptId }: { attemptId: string }) => (
    <div>verified build logs for {attemptId}</div>
  ),
}));

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

afterEach(() => {
  cleanup();
  routeParams.buildId = "attempt-safe";
  vi.restoreAllMocks();
});

describe("build detail log availability", () => {
  it("shows logs with exact builds.read and logs.read without expanding to definition access", async () => {
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
      await screen.findByText("verified build logs for attempt-safe"),
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
