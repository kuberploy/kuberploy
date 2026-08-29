import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capabilities } from "../api/types";
import { SourceBuildsPage } from "./SourceBuildsPage";

beforeEach(() => {
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-safe",
    displayName: "Admin",
    role: "platform-admin",
    authentication: { kind: "session" },
  });
  vi.spyOn(api, "githubInstallations").mockResolvedValue({
    items: [],
    nextCursor: null,
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage(response: Capabilities) {
  vi.spyOn(api, "capabilities").mockResolvedValue(response);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SourceBuildsPage />
    </QueryClientProvider>,
  );
}

describe("Git provider settings", () => {
  it("contains provider connections but no App source or build workspace", async () => {
    renderPage({
      features: { githubAppSetup: true, builds: true, builder: true },
      capabilities: [
        {
          scopeType: "platform",
          scopeId: "platform",
          actions: ["github-installations:setup"],
        },
      ],
    });

    expect(await screen.findByText("Git providers")).toBeInTheDocument();
    expect(
      await screen.findByText("GitHub App installations"),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Build application")).toBeNull();
    expect(screen.queryByText("App source")).toBeNull();
    expect(screen.queryByText("Attempt history")).toBeNull();
    expect(screen.queryByText("Auto-deploy policies")).toBeNull();
  });

  it("explains that App-owned settings stay outside the provider page", async () => {
    renderPage({ features: { githubAppSetup: false }, capabilities: [] });

    expect(
      await screen.findByText(
        /Repository, branch, build, and deployment settings belong to each App/i,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Git SSH already works from each App/i),
    ).toBeInTheDocument();
  });
});
