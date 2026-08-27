import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { BuildAttempt, Environment, Operation } from "../api/types";
import { BuildPromotionPanel } from "./BuildPromotionPanel";

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@tanstack/react-router")>();
  return {
    ...actual,
    Link: ({ children }: PropsWithChildren) => <a href="/test">{children}</a>,
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const attempt: BuildAttempt = {
  id: "attempt-safe",
  definitionId: "definition-safe",
  projectId: "project-safe",
  applicationId: "application-safe",
  commitSha: "a".repeat(40),
  gitRef: "refs/heads/main",
  generation: 1,
  state: "succeeded",
  executionAttempts: 1,
  maxAttempts: 3,
  image: {
    reference: "registry.example.test/app",
    digest: "sha256:" + "b".repeat(64),
    platforms: ["linux/amd64"],
  },
  createdAt: "2026-08-16T00:00:00Z",
  updatedAt: "2026-08-16T00:05:00Z",
};

const environment: Environment = {
  id: "environment-safe",
  projectId: "project-safe",
  name: "Production",
  namespace: "production",
};

function renderPanel() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  const rendered = render(
    <QueryClientProvider client={queryClient}>
      <BuildPromotionPanel
        key={attempt.id}
        attempt={attempt}
        humanSession
        gitOpsReady
      />
    </QueryClientProvider>,
  );
  return { queryClient, rerender: rendered.rerender };
}

describe("BuildPromotionPanel", () => {
  it("does not bind a late success to a newer promotion draft", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "environments").mockResolvedValue({ items: [environment] });
    let resolvePromotion!: (operation: Operation) => void;
    vi.spyOn(api, "promoteBuildAttempt").mockImplementation(
      () => new Promise((resolve) => (resolvePromotion = resolve)),
    );
    renderPanel();

    // Wait for the environment to be listed: the select renders before the
    // environments query resolves.
    await screen.findByRole("option", { name: /Production/ });
    const [environmentSelect] = screen.getAllByRole("combobox");
    if (!environmentSelect) throw new Error("environment select missing");
    await user.selectOptions(environmentSelect, environment.id);
    await user.click(
      screen.getByRole("button", { name: /Promote verified build/i }),
    );
    const replicas = screen.getAllByRole("spinbutton")[0];
    if (!replicas) throw new Error("replicas input missing");
    await user.clear(replicas);
    await user.type(replicas, "2");

    resolvePromotion({ id: "operation-old-draft" } as Operation);

    await waitFor(() =>
      expect(screen.queryByText("Promotion accepted")).not.toBeInTheDocument(),
    );
    expect(replicas).toHaveValue(2);
  });

  it("clears an environment removed from the readable list", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "environments").mockResolvedValue({ items: [environment] });
    const { queryClient } = renderPanel();

    await screen.findByRole("option", { name: /Production/ });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Environment" }),
      environment.id,
    );
    queryClient.setQueryData(["environments"], { items: [] });

    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Environment" })).toHaveValue(
        "",
      ),
    );
  });
});
