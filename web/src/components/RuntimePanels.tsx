import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, type WorkloadLogOptions } from "../api/client";
import { formatDate } from "../lib/format";
import { Card, EmptyState, PlaceholderBadge, Skeleton, StatusPill } from "./ui";
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
    <div className="runtime-source-list" aria-label="Log sources">
      {sources.map((source) => {
        const status = sourceStatus(source, statuses);
        return (
          <div className="runtime-source" key={logSourceKey(source)}>
            <div className="runtime-source__identity">
              <strong>{source.podName}</strong>
              <code>{source.container}</code>
            </div>
            <div className="runtime-source__meta">
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
    <div className="runtime-log-filters" aria-label="Log source filters">
      {fields.map(({ field, label, mergedLabel }) => (
        <label key={field}>
          <span>{label}</span>
          <select
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
          </select>
        </label>
      ))}
      <PlaceholderBadge>
        {filters.pod || filters.revision || filters.container
          ? "Exact source view"
          : "Merged deployment view"}
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
    retry: false,
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
    retry: false,
  });
  const logs = hasLogFilter ? filteredLogs : mergedLogs;
  const events = useQuery({
    queryKey: ["workload-events", workload?.id, boundedEventOptions],
    queryFn: () => api.workloadEvents(workload!.id, boundedEventOptions),
    enabled: Boolean(workload),
    retry: false,
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
    <Card className="runtime-panel">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Kubernetes live source</span>
          <h2>Deployment logs</h2>
        </div>
        <PlaceholderBadge>
          {workloads.isPending
            ? "Loading"
            : workload
              ? "Bounded snapshots"
              : "Unavailable"}
        </PlaceholderBadge>
      </div>
      <p className="panel-description">
        Deployment-wide logs and events are scoped through the Kuberploy API.
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
                ? "Deployment workload unavailable"
                : "No deployment workload"
          }
          description={
            workloads.error
              ? "The scoped log gateway is not ready for this deployment. The workload continues running."
              : runtimeInventoryMissing
                ? "The selected deployment was not returned by the scoped runtime inventory. No other workload was selected as a fallback."
                : "The application has no deployment workload available for a bounded runtime snapshot."
          }
          action={
            <PlaceholderBadge>
              {workloads.error ? "API unavailable" : "No data"}
            </PlaceholderBadge>
          }
          compact
        />
      ) : (
        <div className="runtime-sections">
          <div className="runtime-workload-summary">
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

          <section
            className="runtime-section"
            aria-labelledby="log-lines-title"
          >
            <div className="runtime-section__header">
              <div>
                <span className="eyebrow">
                  {hasLogFilter
                    ? "Exact authorized source filter"
                    : "All controlled Pod sources"}
                </span>
                <h3 id="log-lines-title">
                  {filters.pod
                    ? "Exact Pod snapshot"
                    : hasLogFilter
                      ? "Source-filtered snapshot"
                      : "Deployment-wide snapshot"}
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
                description="The scoped log gateway is not ready for this deployment. The workload continues running."
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
                    className="log-viewer"
                    role="log"
                    aria-label="Deployment log snapshot"
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
                          className="log-source-identity"
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
                      <div className="log-gap">
                        Snapshot truncated at the configured byte limit.
                      </div>
                    ) : null}
                  </div>
                ) : (
                  <EmptyState
                    icon="logs"
                    title="No log lines yet"
                    description="The deployment returned an empty bounded snapshot. Sources are shown above when Kubernetes reported them."
                    action={<PlaceholderBadge>No data</PlaceholderBadge>}
                    compact
                  />
                )}
              </>
            )}
          </section>

          <section
            className="runtime-section"
            aria-labelledby="runtime-events-title"
          >
            <div className="runtime-section__header">
              <div>
                <span className="eyebrow">Deployment, ReplicaSet & Pod</span>
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
                description="The scoped event gateway could not return this deployment snapshot. The workload continues running."
                action={<PlaceholderBadge>API unavailable</PlaceholderBadge>}
                compact
              />
            ) : events.data?.items.length ? (
              <div
                className="runtime-event-list"
                role="list"
                aria-label="Kubernetes event snapshot"
              >
                {events.data.items.map((event) => (
                  <article
                    className="runtime-event"
                    key={event.id}
                    role="listitem"
                  >
                    <div className="runtime-event__head">
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
                    <div className="runtime-event__object">
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
                  <div className="runtime-truncation-notice">
                    Event snapshot truncated at the configured item limit.
                  </div>
                ) : null}
              </div>
            ) : (
              <EmptyState
                icon="deploy"
                title="No Kubernetes events"
                description="The deployment, its ReplicaSets, and exact Pods returned no events in this bounded snapshot."
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
    <Card className="metric-placeholder">
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
      <div className="sparkline-placeholder" aria-hidden="true">
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
    <div className="metrics-panel">
      <div className="metrics-panel__head">
        <div>
          <span className="eyebrow">Scoped Prometheus gateway</span>
          <h2>Service metrics</h2>
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
      <div className="metric-placeholder-grid">
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
      <div className="notice">
        <Icon name="metrics" />
        <div>
          <strong>
            {available
              ? "Monitoring is connected; no application series were returned."
              : "Metrics are explicitly unavailable"}
          </strong>
          <p>
            {status.data?.message ??
              "Configure managed kube-prometheus-stack or an existing compatible endpoint. Missing data is never displayed as zero."}
          </p>
        </div>
      </div>
    </div>
  );
}
