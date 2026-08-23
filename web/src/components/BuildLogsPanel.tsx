import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { ApiError, api, type BuildLogOptions } from "../api/client";
import type {
  BuildLogLine,
  BuildLogSource,
  BuildLogStreamEvent,
} from "../api/types";
import { formatDate, shortId } from "../lib/format";
import { Card, EmptyState, PlaceholderBadge, Skeleton } from "./ui";

const tailChoices = [100, 200, 500, 1_000] as const;
const lookbackChoices = [
  { value: 15, label: "15 minutes" },
  { value: 60, label: "1 hour" },
  { value: 360, label: "6 hours" },
  { value: 1_440, label: "24 hours" },
] as const;
const maximumVisibleLines = 2_000;

function unavailableDescription(error: unknown) {
  if (
    error instanceof ApiError &&
    (error.status === 404 || error.status === 410)
  ) {
    return "The live build source has ended or was removed. Terminal build metadata remains available.";
  }
  return "The exact live builder source could not be verified. Terminal build metadata remains available.";
}

function sinceTimestamp(minutes: number) {
  return new Date(Date.now() - minutes * 60_000)
    .toISOString()
    .replace(".000Z", "Z");
}

function streamEvent(raw: string): BuildLogStreamEvent | undefined {
  if (!raw || raw.length > 300_000) return undefined;
  try {
    const parsed = JSON.parse(raw) as BuildLogStreamEvent;
    return ["line", "status", "gap", "heartbeat", "terminal"].includes(
      parsed.type,
    )
      ? parsed
      : undefined;
  } catch {
    return undefined;
  }
}

export function BuildLogsPanel({ attemptId }: { attemptId: string }) {
  const [tailLines, setTailLines] = useState<number>(200);
  const [lookbackMinutes, setLookbackMinutes] = useState<number>(60);
  const [previous, setPrevious] = useState(false);
  const [following, setFollowing] = useState(false);
  const [streamLines, setStreamLines] = useState<BuildLogLine[]>([]);
  const [streamSource, setStreamSource] = useState<BuildLogSource>();
  const [streamState, setStreamState] = useState<
    "idle" | "connecting" | "active" | "reconnecting" | "ended"
  >("idle");
  const [streamDetail, setStreamDetail] = useState("");
  const [droppedLines, setDroppedLines] = useState(0);
  const snapshotOptions = useMemo<BuildLogOptions>(
    () => ({
      tailLines,
      limitBytes: 1 << 20,
      previous,
      since: sinceTimestamp(lookbackMinutes),
    }),
    [lookbackMinutes, previous, tailLines],
  );
  const snapshot = useQuery({
    queryKey: ["build-logs", attemptId, tailLines, lookbackMinutes, previous],
    queryFn: () =>
      api.buildLogSnapshot(attemptId, {
        ...snapshotOptions,
        since: sinceTimestamp(lookbackMinutes),
      }),
    retry: false,
  });

  useEffect(() => {
    setStreamLines([]);
    setStreamSource(undefined);
    setDroppedLines(0);
    setStreamDetail("");
    setStreamState("idle");
  }, [attemptId]);

  useEffect(() => {
    if (!following) {
      setStreamState("idle");
      return;
    }
    setStreamLines([]);
    setDroppedLines(0);
    setStreamDetail("");
    setStreamState("connecting");
    const source = new EventSource(
      api.buildLogStreamURL(attemptId, {
        tailLines,
        limitBytes: 1 << 20,
        since: sinceTimestamp(lookbackMinutes),
      }),
      { withCredentials: true },
    );
    let terminal = false;
    const receive = (message: MessageEvent<string>) => {
      const event = streamEvent(message.data);
      if (!event) {
        setStreamState("reconnecting");
        setStreamDetail("A malformed stream frame was ignored.");
        return;
      }
      if (event.line) {
        setStreamSource(event.line.source);
        setStreamLines((current) =>
          [...current, event.line!].slice(-maximumVisibleLines),
        );
      }
      if (event.status) {
        setStreamSource(event.status.source);
        setStreamState(event.status.state);
        setStreamDetail(event.status.reason ?? "");
      }
      if (event.gap) {
        setStreamSource(event.gap.source);
        setDroppedLines((current) => current + event.gap!.droppedLines);
      }
      if (event.terminal) {
        terminal = true;
        setStreamState("ended");
        setStreamDetail(event.terminal.detail.slice(0, 256));
        source.close();
      }
    };
    for (const type of ["line", "status", "gap", "heartbeat", "terminal"]) {
      source.addEventListener(type, receive as EventListener);
    }
    source.onerror = () => {
      if (!terminal) {
        setStreamState("reconnecting");
        setStreamDetail(
          "Connection interrupted; the browser will reconnect with the last opaque event cursor.",
        );
      }
    };
    return () => source.close();
  }, [attemptId, following, lookbackMinutes, tailLines]);

  const lines = following ? streamLines : (snapshot.data?.lines ?? []);
  const source = following ? streamSource : snapshot.data?.source;
  const displayState = following
    ? streamState
    : snapshot.isPending
      ? "loading"
      : snapshot.error
        ? "unavailable"
        : "snapshot";

  return (
    <Card className="build-log-panel">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Verified builder source</span>
          <h2>Build logs</h2>
        </div>
        <PlaceholderBadge>{displayState}</PlaceholderBadge>
      </div>
      <p className="panel-description">
        Live and snapshot reads use the immutable attempt identity. Kubernetes
        names, selectors, containers, UIDs, and stored log references remain
        server-owned.
      </p>

      <div className="build-log-controls" aria-label="Build log selectors">
        <label className="field">
          <span>Tail lines</span>
          <select
            value={tailLines}
            onChange={(event) => setTailLines(Number(event.target.value))}
            disabled={following}
          >
            {tailChoices.map((value) => (
              <option key={value} value={value}>
                {value.toLocaleString()}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Lookback</span>
          <select
            value={lookbackMinutes}
            onChange={(event) => setLookbackMinutes(Number(event.target.value))}
            disabled={following}
          >
            {lookbackChoices.map((choice) => (
              <option key={choice.value} value={choice.value}>
                {choice.label}
              </option>
            ))}
          </select>
        </label>
        <label className="build-log-previous">
          <input
            type="checkbox"
            checked={previous}
            onChange={(event) => setPrevious(event.target.checked)}
            disabled={following}
          />
          Previous agent container
        </label>
        <button
          className="button button--secondary"
          type="button"
          onClick={() => void snapshot.refetch()}
          disabled={following || snapshot.isFetching}
        >
          Refresh snapshot
        </button>
        <button
          className={
            following ? "button button--danger" : "button button--primary"
          }
          type="button"
          onClick={() => setFollowing((value) => !value)}
        >
          {following ? "Stop live logs" : "Follow live logs"}
        </button>
      </div>

      <div className="build-log-source-status" role="status">
        <div>
          <span>Opaque source</span>
          <code>{source ? shortId(source.id, 18) : "Not resolved"}</code>
        </div>
        <div>
          <span>Source state</span>
          <strong>
            {source ? (source.ready ? "Ready" : "Starting") : "Unknown"}
          </strong>
        </div>
        <div>
          <span>Mode</span>
          <strong>
            {following
              ? "Live with reconnect"
              : previous
                ? "Previous"
                : "Current"}
          </strong>
        </div>
      </div>

      {streamDetail ? <div className="log-gap">{streamDetail}</div> : null}
      {droppedLines > 0 ? (
        <div className="log-gap">
          {droppedLines.toLocaleString()} lines were dropped under client or
          server backpressure. Reload a bounded snapshot for continuity.
        </div>
      ) : null}

      {!following && snapshot.isPending ? (
        <Skeleton lines={7} />
      ) : !following && snapshot.error ? (
        <EmptyState
          icon="logs"
          title="Build logs unavailable"
          description={unavailableDescription(snapshot.error)}
          action={<PlaceholderBadge>API unavailable</PlaceholderBadge>}
          compact
        />
      ) : lines.length ? (
        <div
          className="log-viewer build-log-viewer"
          role="log"
          aria-live="polite"
        >
          {lines.map((line, index) => (
            <div
              key={`${line.cursor?.fingerprint ?? line.timestamp ?? "line"}-${index}`}
            >
              <time dateTime={line.timestamp}>
                {line.timestamp ? formatDate(line.timestamp) : "No timestamp"}
              </time>
              <code>
                {line.message}
                {line.truncated ? <em> [line truncated]</em> : null}
              </code>
            </div>
          ))}
          {!following && snapshot.data?.truncated ? (
            <div className="log-gap">
              Snapshot truncated at the configured byte limit.
            </div>
          ) : null}
        </div>
      ) : (
        <EmptyState
          icon="logs"
          title={
            following ? "Waiting for build output" : "No build log lines yet"
          }
          description={
            following
              ? "The verified stream is connected or reconnecting; output will appear here."
              : "The exact builder source returned an empty bounded snapshot."
          }
          compact
        />
      )}
    </Card>
  );
}
