import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { ApiError, api, type BuildLogOptions } from "../api/client";
import type {
  BuildLogLine,
  BuildLogSource,
  BuildLogStreamEvent,
} from "../api/types";
import { formatDate, shortId } from "../lib/format";
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  Eyebrow,
  PlaceholderBadge,
  Skeleton,
  buttonVariants,
} from "./ui";

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
    <Card className="mb-5">
      <CardHeader>
        <div>
          <Eyebrow>Verified builder source</Eyebrow>
          <h2>Build logs</h2>
        </div>
        <PlaceholderBadge>{displayState}</PlaceholderBadge>
      </CardHeader>
      <p className="mt-[-12px] mx-0 mb-5 text-ink-faint text-meta">
        Live and snapshot reads use the immutable attempt identity. Kubernetes
        names, selectors, containers, UIDs, and stored log references remain
        server-owned.
      </p>

      <div
        className="grid grid-cols-[minmax(120px,_160px)_minmax(120px,_160px)_minmax(170px,_1fr)_auto_auto] items-end gap-3 my-4 mx-0 to-760:grid-cols-[1fr]"
        aria-label="Build log selectors"
      >
        <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
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
        <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
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
        <label className="flex items-center gap-2 min-h-[34px] text-ink-soft text-meta">
          <input
            type="checkbox"
            checked={previous}
            onChange={(event) => setPrevious(event.target.checked)}
            disabled={following}
          />
          Previous agent container
        </label>
        <button
          className={buttonVariants({ variant: "secondary" })}
          type="button"
          onClick={() => void snapshot.refetch()}
          disabled={following || snapshot.isFetching}
        >
          Refresh snapshot
        </button>
        <Button
          variant={following ? "danger" : "primary"}
          type="button"
          onClick={() => setFollowing((value) => !value)}
        >
          {following ? "Stop live logs" : "Follow live logs"}
        </Button>
      </div>

      <div
        className="grid grid-cols-[repeat(3,_minmax(0,_1fr))] gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&>div]:py-3 [&>div]:px-3 [&>div]:border [&>div]:border-line [&>div]:rounded-lg [&>div]:bg-surface-soft [&_span]:text-ink-faint [&_span]:text-xs [&_span]:uppercase [&_span]:tracking-[0.08em] [&_code]:break-words [&_code]:text-meta [&_strong]:break-words [&_strong]:text-meta to-760:grid-cols-[1fr]"
        role="status"
      >
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

      {streamDetail ? (
        <div className="!block !py-2 !px-3 text-[#ffd694] bg-[#241f14]">
          {streamDetail}
        </div>
      ) : null}
      {droppedLines > 0 ? (
        <div className="!block !py-2 !px-3 text-[#ffd694] bg-[#241f14]">
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
          className="max-h-[560px] overflow-auto py-2 px-0 border border-[#25342e] rounded-[9px] text-[#d5e4dd] bg-[#0c1511] font-mono text-meta [&>div]:grid [&>div]:gap-3 [&>div]:py-1.5 [&>div]:px-3 [&>div]:border-b [&>div]:border-b-[rgba(255,_255,_255,_0.035)] [&_time]:text-ink-soft [&_span]:overflow-hidden [&_span]:text-[#67d4a9] [&_span]:text-ellipsis [&_code]:whitespace-pre-wrap [&_code_em]:text-[#ffd694] [&_code_em]:not-italic to-580:[&>div]:grid-cols-[90px_1fr] to-580:[&_code]:col-[1_/_-1] [&>div]:grid-cols-[150px_minmax(0,_1fr)]"
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
            <div className="!block !py-2 !px-3 text-[#ffd694] bg-[#241f14]">
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
