import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, ApiError, type WorkloadLogOptions } from "../api/client";
import { formatDate } from "../lib/format";
import {
  Select,
  Card,
  CardHeader,
  EmptyState,
  Eyebrow,
  Field,
  Notice,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "./ui";
import { Icon } from "./Icon";
import type {
  LogSource,
  LogSourceStatus,
  MetricKey,
  MetricRangeResult,
  MetricSample,
} from "../api/types";

const boundedLogOptions = { tailLines: 200, limitBytes: 1_048_576 } as const;
const boundedEventOptions = { limit: 50 } as const;
const runtimeViewRetryDelay = 1_000;
const runtimeViewRetryLimit = 180;

export function retryTransientRuntimeView(
  failureCount: number,
  error: Error,
  workloadState: string | undefined,
) {
  return (
    error instanceof ApiError &&
    error.status === 404 &&
    workloadState !== "stopped" &&
    workloadState !== "failed" &&
    failureCount < runtimeViewRetryLimit
  );
}

type LogFilters = {
  pod: string;
  revision: string;
  container: string;
};

type LogFilterState = LogFilters & { deploymentId: string };

function logSourceKey(source: LogSource) {
  return `${source.podId}:${source.container}:${source.previous ? "previous" : "current"}`;
}

function sourceStatus(source: LogSource, statuses: LogSourceStatus[] = []) {
  const status = statuses.find(
    (candidate) => logSourceKey(candidate.source) === logSourceKey(source),
  );
  if (status) {
    return {
      value: status.state,
      label: status.reason ? `${status.state}: ${status.reason}` : status.state,
    };
  }
  if (source.terminating) return { value: "pending", label: "Terminating" };
  if (source.ready) return { value: "ready", label: "Ready" };
  return { value: "pending", label: "Not ready" };
}

function LogSources({
  sources,
  statuses,
}: {
  sources: LogSource[];
  statuses?: LogSourceStatus[];
}) {
  if (!sources.length) return null;
  return (
    <div className="grid gap-2" aria-label="Log sources">
      {sources.map((source) => {
        const status = sourceStatus(source, statuses);
        return (
          <div
            className="grid grid-cols-[minmax(180px,_0.8fr)_minmax(260px,_1.4fr)_auto] items-center gap-3 py-2 px-3 border border-line rounded-lg bg-surface-soft to-580:grid-cols-[minmax(0,_1fr)_auto]"
            key={logSourceKey(source)}
          >
            <div className="min-w-0 [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-ellipsis [&_strong]:mb-1 [&_strong]:text-meta [&_code]:block [&_code]:overflow-hidden [&_code]:text-ellipsis [&_code]:text-mint-dark [&_code]:text-xs">
              <strong>{source.podName}</strong>
              <code>{source.container}</code>
            </div>
            <div className="min-w-0 flex flex-wrap gap-y-1 gap-x-3 text-ink-faint text-xs [&_span:first-child]:break-words to-580:col-[1_/_-1]">
              <span>Pod source ID {source.podId}</span>
              <span>Revision {source.revision ?? "not reported"}</span>
              <span>
                {source.containerKind === "init"
                  ? "Init container"
                  : "Container"}
                {` · restart ${source.restartCount}`}
                {source.previous ? " · previous instance" : ""}
              </span>
            </div>
            <StatusPill value={status.value} label={status.label} />
          </div>
        );
      })}
    </div>
  );
}

function sourceMatchesFilters(source: LogSource, filters: LogFilters) {
  return (
    (!filters.pod || source.podName === filters.pod) &&
    (!filters.revision || source.revision === filters.revision) &&
    (!filters.container || source.container === filters.container)
  );
}

function sourceFilterValues(
  sources: LogSource[],
  filters: LogFilters,
  field: keyof LogFilters,
) {
  const values = sources
    .filter((source) => {
      const candidate = { ...filters, [field]: "" };
      return sourceMatchesFilters(source, candidate);
    })
    .map((source) => {
      if (field === "pod") return source.podName;
      if (field === "revision") return source.revision ?? "";
      return source.container;
    })
    .filter((value) =>
      field === "revision" ? /^[1-9][0-9]{0,18}$/.test(value) : Boolean(value),
    );
  return [...new Set(values)].sort((left, right) =>
    left.localeCompare(right, undefined, { numeric: true }),
  );
}

function LogSourceFilters({
  sources,
  filters,
  onChange,
}: {
  sources: LogSource[];
  filters: LogFilters;
  onChange: (field: keyof LogFilters, value: string) => void;
}) {
  if (!sources.length) return null;
  const fields: Array<{
    field: keyof LogFilters;
    label: string;
    mergedLabel: string;
  }> = [
    { field: "pod", label: "Pod", mergedLabel: "All controlled Pods" },
    { field: "revision", label: "Revision", mergedLabel: "All revisions" },
    {
      field: "container",
      label: "Container",
      mergedLabel: "Automatic container",
    },
  ];
  return (
    <div
      className="grid grid-cols-[repeat(3,_minmax(0,_1fr))_auto] items-end gap-3 [&_label]:grid [&_label]:gap-1.5 [&_label]:min-w-0 [&_label_>_span]:text-ink-faint [&_label_>_span]:text-xs [&_label_>_span]:font-semibold [&_[role='combobox']]:min-h-[34px] [&_[role='combobox']]:text-xs to-580:grid-cols-[1fr]"
      aria-label="Log source filters"
    >
      {fields.map(({ field, label, mergedLabel }) => (
        <Field key={field} label={label}>
          <Select
            aria-label={label}
            value={filters[field]}
            onChange={(event) => onChange(field, event.target.value)}
          >
            <option value="">{mergedLabel}</option>
            {sourceFilterValues(sources, filters, field).map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </Select>
        </Field>
      ))}
      <PlaceholderBadge>
        {filters.pod || filters.revision || filters.container
          ? "Exact source view"
          : "Merged App view"}
      </PlaceholderBadge>
    </div>
  );
}

export function LogsPanel({
  applicationId,
  deploymentId,
}: {
  applicationId: string;
  deploymentId: string;
}) {
  const workloads = useQuery({
    queryKey: ["workloads", applicationId],
    queryFn: () => api.workloads(applicationId),
    retry: false,
  });
  const workload = workloads.data?.items.find(
    (candidate) => candidate.id === deploymentId,
  );
  const [storedFilters, setStoredFilters] = useState<LogFilterState>({
    deploymentId,
    pod: "",
    revision: "",
    container: "",
  });
  const requestedFilters: LogFilters =
    storedFilters.deploymentId === deploymentId
      ? storedFilters
      : { pod: "", revision: "", container: "" };
  const mergedLogs = useQuery({
    queryKey: ["workload-logs", workload?.id, boundedLogOptions],
    queryFn: () => api.workloadLogs(workload!.id, boundedLogOptions),
    enabled: Boolean(workload),
    retry: (failureCount, error) =>
      retryTransientRuntimeView(failureCount, error, workload?.state),
    retryDelay: runtimeViewRetryDelay,
  });
  const filters = requestedFilters;
  const filteredLogOptions: WorkloadLogOptions = {
    ...boundedLogOptions,
    ...(filters.pod ? { pod: filters.pod } : {}),
    ...(filters.revision ? { revision: filters.revision } : {}),
    ...(filters.container ? { container: filters.container } : {}),
  };
  const hasLogFilter = Boolean(
    filters.pod || filters.revision || filters.container,
  );
  const filteredLogs = useQuery({
    queryKey: ["workload-logs", workload?.id, filteredLogOptions],
    queryFn: () => api.workloadLogs(workload!.id, filteredLogOptions),
    enabled: Boolean(workload) && hasLogFilter,
    retry: (failureCount, error) =>
      retryTransientRuntimeView(failureCount, error, workload?.state),
    retryDelay: runtimeViewRetryDelay,
  });
  const logs = hasLogFilter ? filteredLogs : mergedLogs;
  const events = useQuery({
    queryKey: ["workload-events", workload?.id, boundedEventOptions],
    queryFn: () => api.workloadEvents(workload!.id, boundedEventOptions),
    enabled: Boolean(workload),
    retry: (failureCount, error) =>
      retryTransientRuntimeView(failureCount, error, workload?.state),
    retryDelay: runtimeViewRetryDelay,
  });
  const lines = logs.data?.lines ?? [];
  const runtimeInventoryMissing =
    Boolean(workloads.data?.items.length) && workload === undefined;
  const updateFilter = (field: keyof LogFilters, value: string) =>
    setStoredFilters({
      deploymentId,
      ...filters,
      [field]: value,
    });

  return (
    <Card className="p-6 to-580:p-4">
      <CardHeader>
        <div>
          <Eyebrow>Kubernetes live source</Eyebrow>
          <h2>App logs</h2>
        </div>
        <PlaceholderBadge>
          {workloads.isPending
            ? "Loading"
            : workload
              ? "Bounded snapshots"
              : "Unavailable"}
        </PlaceholderBadge>
      </CardHeader>
      <p className="mt-[-12px] mx-0 mb-5 text-ink-faint text-meta">
        App-wide logs and events are scoped through the Kuberploy API.
        Kubernetes live mode does not promise retention after Pod deletion or
        log rotation.
      </p>
      {workloads.isPending ? (
        <Skeleton lines={7} />
      ) : !workload ? (
        <EmptyState
          icon="logs"
          title={
            workloads.error
              ? "Logs unavailable"
              : runtimeInventoryMissing
                ? "App runtime unavailable"
                : "No App runtime"
          }
          description={
            workloads.error
              ? "The scoped log gateway is not ready for this App. The workload continues running."
              : runtimeInventoryMissing
                ? "The selected App runtime was not returned by the scoped runtime inventory. No other workload was selected as a fallback."
                : "The App has no workload available for a bounded runtime snapshot."
          }
          action={
            <PlaceholderBadge>
              {workloads.error ? "API unavailable" : "No data"}
            </PlaceholderBadge>
          }
          compact
        />
      ) : (
        <div className="grid gap-5">
          <div className="grid grid-cols-[minmax(180px,_1.5fr)_minmax(160px,_1fr)_90px_auto] items-center gap-4 py-3 px-4 border border-line rounded-[9px] bg-surface-soft [&>div]:min-w-0 [&_span]:block [&_span]:mb-1 [&_span]:text-ink-faint [&_span]:text-xs [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-ink [&_strong]:text-meta [&_strong]:text-ellipsis [&_code]:block [&_code]:overflow-hidden [&_code]:text-ink [&_code]:text-meta [&_code]:text-ellipsis to-580:grid-cols-[1fr]">
            <div>
              <span>Deployment</span>
              <strong>{workload.name}</strong>
            </div>
            <div>
              <span>Namespace</span>
              <code>{workload.namespace}</code>
            </div>
            <div>
              <span>Replicas</span>
              <strong>{workload.replicas}</strong>
            </div>
            <StatusPill value={workload.state} />
          </div>

          <LogSourceFilters
            sources={mergedLogs.data?.sources ?? []}
            filters={filters}
            onChange={updateFilter}
          />

          <section className="grid gap-3" aria-labelledby="log-lines-title">
            <div className="flex items-end justify-between gap-4 [&_h3]:mt-0.5 [&_h3]:mx-0 [&_h3]:mb-0 [&_h3]:text-meta [&>span]:text-ink-faint [&>span]:text-xs to-580:items-start to-580:flex-col">
              <div>
                <Eyebrow>
                  {hasLogFilter
                    ? "Exact authorized source filter"
                    : "All controlled Pod sources"}
                </Eyebrow>
                <h3 id="log-lines-title">
                  {filters.pod
                    ? "Exact Pod snapshot"
                    : hasLogFilter
                      ? "Source-filtered snapshot"
                      : "App-wide snapshot"}
                </h3>
              </div>
              {logs.data ? (
                <span>
                  {logs.data.bytes.toLocaleString()} bytes · observed{" "}
                  {formatDate(logs.data.observedAt)}
                </span>
              ) : null}
            </div>
            {logs.isPending ? (
              <Skeleton lines={7} />
            ) : logs.error ? (
              <EmptyState
                icon="logs"
                title="Logs unavailable"
                description="The scoped log gateway is not ready for this App. The workload continues running."
                action={<PlaceholderBadge>API unavailable</PlaceholderBadge>}
                compact
              />
            ) : (
              <>
                <LogSources
                  sources={mergedLogs.data?.sources ?? []}
                  statuses={mergedLogs.data?.sourceStatuses}
                />
                {lines.length ? (
                  <div
                    className="max-h-[560px] overflow-auto py-2 px-0 border border-[#25342e] rounded-[9px] text-[#d5e4dd] bg-[#0c1511] font-mono text-meta [&>div]:grid [&>div]:grid-cols-[125px_minmax(190px,_280px)_1fr] [&>div]:gap-3 [&>div]:py-1.5 [&>div]:px-3 [&>div]:border-b [&>div]:border-b-[rgba(255,_255,_255,_0.035)] [&_time]:text-ink-soft [&_span]:overflow-hidden [&_span]:text-[#67d4a9] [&_span]:text-ellipsis [&_code]:whitespace-pre-wrap [&_code_em]:text-[#ffd694] [&_code_em]:not-italic to-580:[&>div]:grid-cols-[90px_1fr] to-580:[&_code]:col-[1_/_-1]"
                    role="log"
                    aria-label="App log snapshot"
                  >
                    {lines.map((line, index) => (
                      <div
                        key={`${line.cursor?.fingerprint ?? line.timestamp ?? "line"}-${index}`}
                      >
                        <time dateTime={line.timestamp}>
                          {line.timestamp
                            ? formatDate(line.timestamp)
                            : "No timestamp"}
                        </time>
                        <span
                          className="[&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-ellipsis [&_small]:block [&_small]:overflow-hidden [&_small]:text-ellipsis [&_small]:mt-0.5 [&_small]:text-ink-soft [&_small]:text-xs"
                          title={`Pod source ID ${line.source.podId}`}
                        >
                          <strong>
                            {line.source.podName}/{line.source.container}
                          </strong>
                          <small>
                            revision {line.source.revision ?? "not reported"}
                          </small>
                        </span>
                        <code>
                          {line.message}
                          {line.truncated ? <em> [line truncated]</em> : null}
                        </code>
                      </div>
                    ))}
                    {logs.data?.truncated ? (
                      <div className="!block !py-2 !px-3 text-[#ffd694] bg-[#241f14]">
                        Snapshot truncated at the configured byte limit.
                      </div>
                    ) : null}
                  </div>
                ) : (
                  <EmptyState
                    icon="logs"
                    title="No log lines yet"
                    description="The App returned an empty bounded snapshot. Sources are shown above when Kubernetes reported them."
                    action={<PlaceholderBadge>No data</PlaceholderBadge>}
                    compact
                  />
                )}
              </>
            )}
          </section>

          <section
            className="grid gap-3"
            aria-labelledby="runtime-events-title"
          >
            <div className="flex items-end justify-between gap-4 [&_h3]:mt-0.5 [&_h3]:mx-0 [&_h3]:mb-0 [&_h3]:text-meta [&>span]:text-ink-faint [&>span]:text-xs to-580:items-start to-580:flex-col">
              <div>
                <Eyebrow>Deployment, ReplicaSet & Pod</Eyebrow>
                <h3 id="runtime-events-title">Kubernetes events</h3>
              </div>
              {events.data ? (
                <span>Observed {formatDate(events.data.observedAt)}</span>
              ) : null}
            </div>
            {events.isPending ? (
              <Skeleton lines={4} />
            ) : events.error ? (
              <EmptyState
                icon="deploy"
                title="Kubernetes events unavailable"
                description="The scoped event gateway could not return this App snapshot. The workload continues running."
                action={<PlaceholderBadge>API unavailable</PlaceholderBadge>}
                compact
              />
            ) : events.data?.items.length ? (
              <div
                className="overflow-hidden border border-line rounded-[9px]"
                role="list"
                aria-label="Kubernetes event snapshot"
              >
                {events.data.items.map((event) => (
                  <article
                    className="grid gap-2 py-3 px-3 border-b border-b-line last:border-b-0 [&_p]:m-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.55] [&_p]:whitespace-pre-wrap"
                    key={event.id}
                    role="listitem"
                  >
                    <div className="flex items-center gap-2 [&_strong]:text-meta [&_time]:ml-[auto] [&_time]:text-ink-faint [&_time]:text-xs to-580:flex-wrap to-580:[&_time]:w-full to-580:[&_time]:ml-0">
                      <StatusPill
                        value={
                          event.type === "Warning"
                            ? "degraded"
                            : event.type === "Normal"
                              ? "ready"
                              : "unknown"
                        }
                        label={event.type}
                      />
                      <strong>{event.reason}</strong>
                      <time dateTime={event.lastSeen}>
                        {event.lastSeen
                          ? formatDate(event.lastSeen)
                          : "Time not reported"}
                      </time>
                    </div>
                    <div className="flex items-center gap-2 [&_code]:text-mint-dark [&_code]:text-xs [&_span]:text-ink-faint [&_span]:text-xs to-580:flex-wrap">
                      <code>
                        {event.objectKind}/{event.objectName}
                      </code>
                      <span>
                        {event.count === 1
                          ? "1 occurrence"
                          : `${event.count} occurrences`}
                      </span>
                    </div>
                    <p>
                      {event.message}
                      {event.messageTruncated ? " [message truncated]" : ""}
                    </p>
                  </article>
                ))}
                {events.data.truncated ? (
                  <div className="py-2 px-3 text-tone-warn bg-tone-warn-surface text-xs">
                    Event snapshot truncated at the configured item limit.
                  </div>
                ) : null}
              </div>
            ) : (
              <EmptyState
                icon="deploy"
                title="No Kubernetes events"
                description="The App workload, its ReplicaSets, and exact Pods returned no events in this bounded snapshot."
                action={<PlaceholderBadge>No data</PlaceholderBadge>}
                compact
              />
            )}
          </section>
        </div>
      )}
    </Card>
  );
}

const metricCards: Array<{
  key: MetricKey;
  label: string;
  format: (value: number) => string;
}> = [
  {
    key: "cpu-usage",
    label: "CPU usage",
    format: (value) => `${value.toFixed(value < 1 ? 3 : 2)} cores`,
  },
  {
    key: "memory-working-set",
    label: "Memory working set",
    format: (value) => `${(value / 1024 / 1024).toFixed(1)} MiB`,
  },
  {
    key: "http-request-rate",
    label: "Request rate",
    format: (value) => `${value.toFixed(2)} req/s`,
  },
  {
    key: "http-error-ratio",
    label: "5xx response ratio",
    format: (value) => `${(value * 100).toFixed(2)}%`,
  },
];

function latestMetricValue(result?: MetricRangeResult) {
  const values = result?.series
    .map((series) => series.samples.at(-1)?.value)
    .filter((value): value is number => value !== undefined);
  if (!values?.length) return undefined;
  return values.length === 1
    ? values[0]
    : values.reduce((sum, value) => sum + value, 0);
}

function MetricCard({
  deploymentId,
  enabled,
  metric,
  label,
  format,
}: {
  deploymentId: string;
  enabled: boolean;
  metric: MetricKey;
  label: string;
  format: (value: number) => string;
}) {
  const timeBucket = Math.floor(Date.now() / 300_000) * 300_000;
  const query = useQuery({
    queryKey: ["service-metric", deploymentId, metric, timeBucket],
    queryFn: () =>
      api.metricRange({
        scopeType: "service",
        scopeId: deploymentId,
        metric,
        from: new Date(timeBucket - 30 * 60_000),
        to: new Date(timeBucket),
        stepSeconds: 60,
      }),
    enabled,
    retry: false,
    refetchInterval: 30_000,
  });
  const value = latestMetricValue(query.data);
  const samples = query.data?.series[0]?.samples.slice(-7) ?? [];
  const maximum = Math.max(...samples.map((sample) => sample.value), 0);
  const sparklineSamples: Array<MetricSample | undefined> = samples.length
    ? samples
    : Array.from({ length: 7 }, () => undefined);

  return (
    <Card className="min-h-[145px] p-4 [&>div:first-child]:flex [&>div:first-child]:items-center [&>div:first-child]:justify-between [&>div:first-child]:gap-1.5 [&_span]:text-ink-soft [&_span]:text-xs [&_span]:font-semibold [&_strong]:block [&_strong]:mt-5 [&_strong]:text-ink-faint [&_strong]:text-[25px]">
      <div>
        <span>{label}</span>
        <PlaceholderBadge>
          {!enabled
            ? "Unavailable"
            : query.isPending
              ? "Loading"
              : query.error
                ? "Query failed"
                : value === undefined
                  ? "No data"
                  : "Live"}
        </PlaceholderBadge>
      </div>
      <strong>{value === undefined ? "—" : format(value)}</strong>
      <div
        className="flex h-[34px] items-end gap-1.5 mt-2 [&_i]:flex-1 [&_i]:rounded-[2px_2px_0_0] [&_i]:bg-surface-soft [&_i:nth-child(1)]:h-[25%] [&_i:nth-child(2)]:h-[47%] [&_i:nth-child(3)]:h-[36%] [&_i:nth-child(4)]:h-[68%] [&_i:nth-child(5)]:h-[50%] [&_i:nth-child(6)]:h-[82%] [&_i:nth-child(7)]:h-[61%]"
        aria-hidden="true"
      >
        {sparklineSamples.map((sample, index) => (
          <i
            key={sample ? sample.timestamp : `empty-${index}`}
            style={
              sample && maximum > 0
                ? {
                    height: `${Math.max(6, (sample.value / maximum) * 100)}%`,
                  }
                : undefined
            }
          />
        ))}
      </div>
    </Card>
  );
}

export function MetricsPanel({ deploymentId }: { deploymentId: string }) {
  const status = useQuery({
    queryKey: ["monitoring-status"],
    queryFn: api.monitoringStatus,
    retry: false,
  });
  const available =
    status.data?.available === true ||
    ["healthy", "ready", "connected"].includes(
      status.data?.status?.toLowerCase() ?? "",
    );
  return (
    <div className="[&_h2]:m-0 [&_h2]:text-ink [&_h2]:text-section [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_h2]:leading-[1.3]">
      <div className="flex items-end justify-between gap-5 mb-4 [&_h2]:text-[20px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta">
        <div>
          <Eyebrow>Scoped Prometheus gateway</Eyebrow>
          <h2>App metrics</h2>
          <p>
            Named, bounded queries only. Tenant users never receive arbitrary
            PromQL access.
          </p>
        </div>
        <StatusPill
          value={
            available
              ? "available"
              : status.data?.mode === "disabled"
                ? "disabled"
                : "pending"
          }
          label={
            status.isPending
              ? "Checking"
              : available
                ? "Connected"
                : status.error
                  ? "Unavailable"
                  : (status.data?.mode ?? "No data")
          }
        />
      </div>
      <div className="grid grid-cols-[repeat(4,_1fr)] gap-3 to-1120:grid-cols-[repeat(2,_1fr)] to-580:grid-cols-[1fr]">
        {metricCards.map((metric) => (
          <MetricCard
            key={metric.key}
            deploymentId={deploymentId}
            enabled={available}
            metric={metric.key}
            label={metric.label}
            format={metric.format}
          />
        ))}
      </div>
      <Notice>
        <Icon name="metrics" />
        <div>
          <strong>
            {available
              ? "Monitoring is connected; no App series were returned."
              : "Metrics are explicitly unavailable"}
          </strong>
          <p>
            {status.data?.message ??
              "Configure managed kube-prometheus-stack or an existing compatible endpoint. Missing data is never displayed as zero."}
          </p>
        </div>
      </Notice>
    </div>
  );
}
