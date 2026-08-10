import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  EventSnapshot,
  LogSnapshot,
  LogSource,
  Workload,
} from "../api/types";
import { LogsPanel } from "./RuntimePanels";

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

const targetWorkload: Workload = {
  id: "deployment-target",
  name: "payments-api",
  kind: "Deployment",
  namespace: "payments-production",
  replicas: 3,
  revision: "42",
  state: "healthy",
};

const otherWorkload: Workload = {
  ...targetWorkload,
  id: "deployment-other",
  name: "payments-api-other-environment",
  namespace: "payments-staging",
  revision: "41",
};

const source: LogSource = {
  podId: "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  podName: "payments-api-7d9f7b8c9-x2k4m",
  container: "application",
  containerKind: "regular",
  restartCount: 2,
  revision: "42",
  ready: true,
  terminating: false,
  previous: false,
};

const logSnapshot: LogSnapshot = {
  lines: [
    {
      type: "line",
      timestamp: "2026-08-09T00:04:00Z",
      source,
      message: "server is ready",
      truncated: false,
      cursor: {
        sourceId: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        timestamp: "2026-08-09T00:04:00Z",
        fingerprint: "a".repeat(64),
      },
    },
  ],
  sources: [source],
  sourceStatuses: [{ source, state: "ended", reason: "PodDeleted" }],
  bytes: 15,
  truncated: false,
  observedAt: "2026-08-09T00:05:00Z",
};

const eventSnapshot: EventSnapshot = {
  items: [
    {
      id: "event-1",
      type: "Warning",
      reason: "FailedScheduling",
      message: "No matching node was available.",
      messageTruncated: false,
      objectKind: "Pod",
      objectName: source.podName,
      count: 3,
      firstSeen: "2026-08-09T00:01:00Z",
      lastSeen: "2026-08-09T00:03:00Z",
    },
  ],
  truncated: false,
  observedAt: "2026-08-09T00:05:00Z",
};

function panel() {
  return (
    <LogsPanel applicationId="application-1" deploymentId={targetWorkload.id} />
  );
}

describe("deployment runtime panel", () => {
  it("selects the exact deployment and presents nested log source identity and events", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "workloads").mockResolvedValue({
      items: [otherWorkload, targetWorkload],
    });
    const logs = vi.spyOn(api, "workloadLogs").mockResolvedValue(logSnapshot);
    const events = vi
      .spyOn(api, "workloadEvents")
      .mockResolvedValue(eventSnapshot);

    render(panel(), { wrapper: wrapper() });

    expect(
      await screen.findByText(`Pod source ID ${source.podId}`),
    ).toBeInTheDocument();
    const sources = screen.getByLabelText("Log sources");
    expect(within(sources).getByText(source.podName)).toBeInTheDocument();
    expect(within(sources).getByText(source.container)).toBeInTheDocument();
    expect(screen.getByText(`Revision ${source.revision}`)).toBeInTheDocument();
    expect(
      screen.getByText(`${source.podName}/${source.container}`),
    ).toBeInTheDocument();
    expect(screen.getByText("revision 42")).toBeInTheDocument();
    expect(screen.getByText("server is ready")).toBeInTheDocument();
    expect(screen.getByText("FailedScheduling")).toBeInTheDocument();
    expect(screen.getByText(`Pod/${source.podName}`)).toBeInTheDocument();
    expect(
      screen.getByText("No matching node was available."),
    ).toBeInTheDocument();

    expect(logs).toHaveBeenCalledWith(targetWorkload.id, {
      tailLines: 200,
      limitBytes: 1_048_576,
    });
    expect(events).toHaveBeenCalledWith(targetWorkload.id, { limit: 50 });

    await user.selectOptions(
      screen.getByRole("combobox", { name: "Pod" }),
      source.podName,
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Revision" }),
      source.revision!,
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Container" }),
      source.container,
    );

    expect(screen.getByText("Exact Pod snapshot")).toBeInTheDocument();
    await waitFor(() =>
      expect(logs).toHaveBeenLastCalledWith(targetWorkload.id, {
        tailLines: 200,
        limitBytes: 1_048_576,
        pod: source.podName,
        revision: source.revision,
        container: source.container,
      }),
    );
  });

  it("settles to a safe empty state without calling scoped runtime endpoints", async () => {
    vi.spyOn(api, "workloads").mockResolvedValue({ items: [] });
    const logs = vi.spyOn(api, "workloadLogs");
    const events = vi.spyOn(api, "workloadEvents");

    render(panel(), { wrapper: wrapper() });

    expect(
      await screen.findByText("No deployment workload"),
    ).toBeInTheDocument();
    expect(logs).not.toHaveBeenCalled();
    expect(events).not.toHaveBeenCalled();
  });

  it("fails closed when the scoped workload inventory errors", async () => {
    vi.spyOn(api, "workloads").mockRejectedValue(
      new Error("inventory offline"),
    );
    const logs = vi.spyOn(api, "workloadLogs");
    const events = vi.spyOn(api, "workloadEvents");

    render(panel(), { wrapper: wrapper() });

    expect(await screen.findByText("Logs unavailable")).toBeInTheDocument();
    expect(screen.getByText("API unavailable")).toBeInTheDocument();
    expect(logs).not.toHaveBeenCalled();
    expect(events).not.toHaveBeenCalled();
  });

  it("keeps the empty event snapshot visible when only logs fail", async () => {
    vi.spyOn(api, "workloads").mockResolvedValue({ items: [targetWorkload] });
    vi.spyOn(api, "workloadLogs").mockRejectedValue(new Error("logs offline"));
    vi.spyOn(api, "workloadEvents").mockResolvedValue({
      items: [],
      truncated: false,
      observedAt: "2026-08-09T00:05:00Z",
    });

    render(panel(), { wrapper: wrapper() });

    expect(await screen.findByText("Logs unavailable")).toBeInTheDocument();
    expect(screen.getByText("No Kubernetes events")).toBeInTheDocument();
  });

  it("keeps an empty log snapshot visible when only events fail", async () => {
    vi.spyOn(api, "workloads").mockResolvedValue({ items: [targetWorkload] });
    vi.spyOn(api, "workloadLogs").mockResolvedValue({
      lines: [],
      sources: [],
      bytes: 0,
      truncated: false,
      observedAt: "2026-08-09T00:05:00Z",
    });
    vi.spyOn(api, "workloadEvents").mockRejectedValue(
      new Error("events offline"),
    );

    render(panel(), { wrapper: wrapper() });

    expect(await screen.findByText("No log lines yet")).toBeInTheDocument();
    expect(
      screen.getByText("Kubernetes events unavailable"),
    ).toBeInTheDocument();
  });
});
