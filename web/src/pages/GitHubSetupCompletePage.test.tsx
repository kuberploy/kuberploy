import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, type PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { GitHubSetupCompletePage } from "./GitHubSetupCompletePage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
}));

const linked = {
  installation: {
    id: "installation-safe",
    githubInstallationId: 42,
    accountLogin: "example",
    accountType: "Organization",
    ownerUserId: "user-safe",
    visibility: "private" as const,
    repositorySelection: "selected",
    repositoryCount: 1,
    createdAt: "2026-08-09T00:00:00Z",
    updatedAt: "2026-08-09T00:00:00Z",
  },
  repositories: [],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <GitHubSetupCompletePage />
      </QueryClientProvider>
    </StrictMode>,
  );
}

describe("GitHub setup completion", () => {
  it("posts exactly once under Strict Mode and never reads or stores a handoff", async () => {
    const storageWrite = vi.spyOn(Storage.prototype, "setItem");
    const complete = vi
      .spyOn(api, "completeGitHubSetup")
      .mockResolvedValue(linked);
    renderPage();

    expect(
      await screen.findByText("GitHub installation linked"),
    ).toBeInTheDocument();
    expect(complete).toHaveBeenCalledTimes(1);
    expect(complete).toHaveBeenCalledWith(expect.any(String));
    expect(complete.mock.calls[0]?.[0]).toHaveLength(36);
    expect(storageWrite).not.toHaveBeenCalled();
    expect(document.body.textContent).not.toMatch(
      /handoff[_=-][A-Za-z0-9_-]{20}/i,
    );
  });

  it("reuses the same idempotency key for an explicit retry", async () => {
    const user = userEvent.setup();
    const complete = vi
      .spyOn(api, "completeGitHubSetup")
      .mockRejectedValueOnce(new Error("network unavailable"))
      .mockResolvedValueOnce(linked);
    renderPage();

    expect(
      await screen.findByText("Installation was not linked"),
    ).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: "Retry same handoff" }),
    );
    await waitFor(() => expect(complete).toHaveBeenCalledTimes(2));
    expect(complete.mock.calls[1]?.[0]).toBe(complete.mock.calls[0]?.[0]);
    expect(
      await screen.findByText("GitHub installation linked"),
    ).toBeInTheDocument();
  });
});
