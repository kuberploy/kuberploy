import { selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type { BuildLogSnapshot, BuildLogStreamEvent } from "../api/types";
import { BuildLogsPanel } from "./BuildLogsPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

const observedAt = "2026-08-09T00:05:00Z";
const source = {
  id: `build_${"a".repeat(32)}`,
  ready: true,
  previous: false,
};
const snapshot: BuildLogSnapshot = {
  source,
  lines: [
    {
      type: "line",
      timestamp: observedAt,
      source,
      message: "snapshot output",
      truncated: false,
    },
  ],
  bytes: 15,
  truncated: false,
  observedAt,
};

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  readonly url: string;
  readonly withCredentials: boolean;
  onerror: ((event: Event) => unknown) | null = null;
  closed = false;
  private readonly listeners = new Map<string, EventListener[]>();

  constructor(url: string | URL, init?: EventSourceInit) {
    this.url = String(url);
    this.withCredentials = init?.withCredentials === true;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: EventListener) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close() {
    this.closed = true;
  }

  emit(type: string, event: BuildLogStreamEvent) {
    const message = new MessageEvent(type, { data: JSON.stringify(event) });
    for (const listener of this.listeners.get(type) ?? []) listener(message);
  }
}

describe("source-build log panel", () => {
  it("uses bounded selectors and presents source, status, reconnect and gaps", async () => {
    const user = userEvent.setup();
    const snapshotRequest = vi
      .spyOn(api, "buildLogSnapshot")
      .mockResolvedValue(snapshot);
    vi.stubGlobal("EventSource", FakeEventSource);

    render(<BuildLogsPanel attemptId="attempt-safe" />, { wrapper: wrapper() });

    expect(await screen.findByText("snapshot output")).toBeInTheDocument();
    expect(screen.getByText("build_aaaaaaaaaaaa")).toBeInTheDocument();
    expect(snapshotRequest).toHaveBeenCalledWith(
      "attempt-safe",
      expect.objectContaining({ tailLines: 200, limitBytes: 1_048_576 }),
    );

    await selectOption(
      screen.getByRole("combobox", { name: "Tail lines" }),
      "500",
    );
    await user.click(screen.getByRole("button", { name: "Follow live logs" }));
    expect(FakeEventSource.instances).toHaveLength(1);
    const stream = FakeEventSource.instances[0]!;
    expect(stream.withCredentials).toBe(true);
    expect(stream.url).toContain(
      "/v1/builds/attempt-safe/logs?follow=true&tailLines=500&limitBytes=1048576&since=",
    );

    stream.emit("status", {
      type: "status",
      status: { source, state: "active" },
      at: observedAt,
    });
    stream.emit("line", {
      type: "line",
      line: {
        type: "line",
        timestamp: observedAt,
        source,
        message: "live output",
        truncated: false,
        cursor: {
          sourceId: source.id,
          timestamp: observedAt,
          fingerprint: "b".repeat(64),
        },
      },
      at: observedAt,
    });
    stream.emit("gap", {
      type: "gap",
      gap: { source, droppedLines: 7 },
      at: observedAt,
    });

    expect(await screen.findByText("live output")).toBeInTheDocument();
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByText(/7 lines were dropped/)).toBeInTheDocument();

    stream.onerror?.(new Event("error"));
    expect(
      await screen.findByText(
        /browser will reconnect with the last opaque event cursor/,
      ),
    ).toBeInTheDocument();

    stream.emit("terminal", {
      type: "terminal",
      terminal: {
        code: "BuildCompleted",
        detail: "The build log stream has ended.",
      },
      at: observedAt,
    });
    await waitFor(() => expect(stream.closed).toBe(true));
    expect(screen.getByText("ended")).toBeInTheDocument();
    expect(
      screen.getByText("The build log stream has ended."),
    ).toBeInTheDocument();
  });

  it("does not claim a terminal build is still running when its live source is gone", async () => {
    vi.spyOn(api, "buildLogSnapshot").mockRejectedValue(
      new ApiError(404, {
        type: "about:blank",
        title: "Not found",
        status: 404,
        detail: "The requested build was not found.",
      }),
    );

    render(<BuildLogsPanel attemptId="completed-attempt" />, {
      wrapper: wrapper(),
    });

    expect(
      await screen.findByText(
        "The live build source has ended or was removed. Terminal build metadata remains available.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/continues running/i)).not.toBeInTheDocument();
  });
});
