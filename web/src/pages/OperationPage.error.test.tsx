import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { OperationPage } from "./OperationPage";

const routeParams = vi.hoisted(() => ({ operationId: "operation-1" }));

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params: _params,
  }: PropsWithChildren<{ to: string; params?: unknown }>) => (
    <a href={to}>{children}</a>
  ),
  useParams: () => routeParams,
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("operation deployment lookup", () => {
  it("surfaces deployment lookup failures and retries the lookup", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "operation").mockResolvedValue({
      id: "operation-1",
      kind: "deployment.git-write",
      status: "succeeded",
      state: "succeeded",
      targetType: "deployment",
      targetId: "deployment-1",
      requestId: "request-1",
      generation: 1,
      progress: [],
      createdAt: "2026-09-14T00:00:00Z",
      updatedAt: "2026-09-14T00:00:01Z",
    } as never);
    const deployment = vi
      .spyOn(api, "deployment")
      .mockRejectedValueOnce(new Error("deployment lookup unavailable"))
      .mockResolvedValue({
        id: "deployment-1",
        applicationId: "application-1",
        environmentId: "environment-1",
      } as never);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={client}>
        <OperationPage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Could not load App details")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(deployment).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("link", { name: /Open App/ })).toBeVisible();
  });
});
