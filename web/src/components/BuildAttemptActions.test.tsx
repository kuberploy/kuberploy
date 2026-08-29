import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  BuildAttempt,
  Capability,
  Project,
} from "../api/types";
import { BuildAttemptActions } from "./BuildAttemptActions";

const project: Project = {
  id: "project-safe",
  name: "Payments",
  teamId: "team-safe",
};
const application: Application = {
  id: "application-safe",
  projectId: project.id,
  name: "API",
};
const attempt: BuildAttempt = {
  id: "attempt-safe",
  sourceId: "source-safe",
  projectId: project.id,
  applicationId: application.id,
  commitSha: "a".repeat(40),
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

function renderActions(
  capabilities: Capability[],
  humanSession = true,
  onUpdated = vi.fn(),
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <BuildAttemptActions
        attempt={attempt}
        application={application}
        project={project}
        capabilities={capabilities}
        humanSession={humanSession}
        onUpdated={onUpdated}
      />
    </QueryClientProvider>,
  );
  return onUpdated;
}

describe("build attempt controls", () => {
  it("does not expand environment authority or expose controls to automation", () => {
    const environmentGrant: Capability = {
      scopeType: "environment",
      scopeId: "environment-safe",
      actions: ["builds:cancel", "builds:retry"],
    };
    renderActions([environmentGrant]);
    expect(
      screen.queryByRole("button", { name: "Cancel build" }),
    ).not.toBeInTheDocument();

    cleanup();
    renderActions(
      [
        {
          scopeType: "project",
          scopeId: project.id,
          actions: ["builds:cancel"],
        },
      ],
      false,
    );
    expect(
      screen.queryByRole("button", { name: "Cancel build" }),
    ).not.toBeInTheDocument();
  });

  it("requires exact local ID confirmation and uses one generated key", async () => {
    const user = userEvent.setup();
    const updated = { ...attempt, state: "cancelling" as const };
    const cancel = vi
      .spyOn(api, "cancelBuildAttempt")
      .mockResolvedValue(updated);
    const onUpdated = renderActions([
      {
        scopeType: "project",
        scopeId: project.id,
        actions: ["builds:cancel"],
      },
    ]);

    await user.click(screen.getByRole("button", { name: "Cancel build" }));
    const confirm = screen.getByRole("button", { name: "Confirm cancel" });
    expect(confirm).toBeDisabled();
    const confirmation = screen.getByLabelText(
      new RegExp(`^Type ${attempt.id}`),
    );
    await user.type(confirmation, "wrong");
    expect(confirm).toBeDisabled();
    expect(cancel).not.toHaveBeenCalled();
    await user.clear(confirmation);
    await user.type(confirmation, attempt.id);
    await user.click(confirm);

    await waitFor(() => expect(cancel).toHaveBeenCalledTimes(1));
    expect(cancel).toHaveBeenCalledWith(attempt.id, expect.any(String));
    expect(cancel.mock.calls[0]?.[1]).toHaveLength(36);
    expect(onUpdated).toHaveBeenCalledWith(updated);
  });

  it("keeps a newer attempt confirmation after an older command completes", async () => {
    const user = userEvent.setup();
    let resolveCancel!: (value: BuildAttempt) => void;
    const updated = { ...attempt, state: "cancelling" as const };
    const cancel = vi.spyOn(api, "cancelBuildAttempt").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCancel = resolve;
        }),
    );
    const onUpdated = vi.fn();
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const view = render(
      <QueryClientProvider client={client}>
        <BuildAttemptActions
          attempt={attempt}
          application={application}
          project={project}
          capabilities={[
            {
              scopeType: "project",
              scopeId: project.id,
              actions: ["builds:cancel"],
            },
          ]}
          humanSession
          onUpdated={onUpdated}
        />
      </QueryClientProvider>,
    );

    await user.click(screen.getByRole("button", { name: "Cancel build" }));
    await user.type(
      screen.getByLabelText(new RegExp(`^Type ${attempt.id}`)),
      attempt.id,
    );
    await user.click(screen.getByRole("button", { name: "Confirm cancel" }));
    await waitFor(() => expect(cancel).toHaveBeenCalledOnce());

    const newerAttempt = { ...attempt, id: "attempt-newer" };
    view.rerender(
      <QueryClientProvider client={client}>
        <BuildAttemptActions
          attempt={newerAttempt}
          application={application}
          project={project}
          capabilities={[
            {
              scopeType: "project",
              scopeId: project.id,
              actions: ["builds:cancel"],
            },
          ]}
          humanSession
          onUpdated={onUpdated}
        />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Cancel build" }),
      ).toBeInTheDocument(),
    );

    resolveCancel(updated);
    await waitFor(() => expect(cancel).toHaveBeenCalledOnce());
    expect(onUpdated).not.toHaveBeenCalled();
    expect(
      screen.getByRole("button", { name: "Cancel build" }),
    ).toBeInTheDocument();
  });
});
