import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { GitHubInstallationsPanel } from "./GitHubInstallationsPanel";

beforeEach(() => {
  vi.spyOn(api, "githubInstallations").mockResolvedValue({
    items: [],
    nextCursor: undefined,
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPanel(
  props: Partial<Parameters<typeof GitHubInstallationsPanel>[0]> = {},
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const navigate = vi.fn();
  render(
    <QueryClientProvider client={queryClient}>
      <GitHubInstallationsPanel
        featureEnabled
        humanSession
        navigate={navigate}
        {...props}
      />
    </QueryClientProvider>,
  );
  return { queryClient, navigate };
}

describe("GitHub installation setup UI", () => {
  it("keeps setup disabled when capability discovery says unavailable", async () => {
    renderPanel({ featureEnabled: false });

    expect(
      screen.getByText("GitHub App setup is not enabled"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Install GitHub App" }),
    ).not.toBeInTheDocument();
    expect(api.githubInstallations).not.toHaveBeenCalled();
  });

  it("never exposes setup mutation controls to a service-account session", async () => {
    renderPanel({ humanSession: false });

    expect(
      await screen.findByText("No linked GitHub installation"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Install GitHub App" }),
    ).not.toBeInTheDocument();
    expect(screen.getByText("Human session required")).toBeInTheDocument();
  });

  it("navigates with an imperative one-shot response outside React Query and storage", async () => {
    const user = userEvent.setup();
    const storageWrite = vi.spyOn(Storage.prototype, "setItem");
    const destination =
      "https://github.com/apps/kuberploy/installations/new?state=" +
      "s".repeat(64);
    vi.spyOn(api, "beginGitHubSetup").mockResolvedValue(destination);
    const { queryClient, navigate } = renderPanel();

    await screen.findByText("No linked GitHub installation");
    await user.click(
      screen.getByRole("button", { name: "Install GitHub App" }),
    );

    await waitFor(() => expect(navigate).toHaveBeenCalledWith(destination));
    expect(api.beginGitHubSetup).toHaveBeenCalledWith(
      { returnKey: "source-builds" },
      expect.any(String),
    );
    expect(queryClient.getMutationCache().getAll()).toHaveLength(0);
    expect(storageWrite).not.toHaveBeenCalled();
    expect(document.body.textContent).not.toContain("s".repeat(64));
  });
});
