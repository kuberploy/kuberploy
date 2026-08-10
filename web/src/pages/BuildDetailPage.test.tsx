import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { BuildAttempt } from "../api/types";
import { BuildDetailPage } from "./BuildDetailPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
  useParams: () => ({ buildId: "attempt-safe" }),
}));

vi.mock("../components/BuildLogsPanel", () => ({
  BuildLogsPanel: ({ attemptId }: { attemptId: string }) => (
    <div>verified build logs for {attemptId}</div>
  ),
}));

const attempt: BuildAttempt = {
  id: "attempt-safe",
  definitionId: "definition-safe",
  projectId: "project-safe",
  applicationId: "application-safe",
  commitSha: "b".repeat(40),
  gitRef: "refs/heads/main",
  generation: 1,
  state: "running",
  executionAttempts: 1,
  maxAttempts: 3,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

afterEach(() => {
  cleanup();
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
});
