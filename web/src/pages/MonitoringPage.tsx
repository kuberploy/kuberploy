import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api/client";
import type { MetricKey, MetricRangeResult, Project } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Card,
  EmptyState,
  ErrorPanel,
  PageHeader,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { formatDate } from "../lib/format";
import {
  hasGlobalMetricsAccess,
  monitoringEnvironments,
} from "../lib/monitoringAccess";

type MetricDefinition = {
  key: MetricKey;
  label: string;
  description: string;
  aggregation: "sum" | "single-series";
  format: (value: number) => string;
};

export const monitoringMetricCatalog: MetricDefinition[] = [
  {
    key: "cpu-usage",
    label: "CPU usage",
    description: "Common-timestamp total across returned service series.",
    aggregation: "sum",
    format: (value) => `${value.toFixed(value < 1 ? 3 : 2)} cores`,
  },
  {
    key: "memory-working-set",
    label: "Memory working set",
    description: "Common-timestamp total across returned service series.",
    aggregation: "sum",
    format: (value) => `${(value / 1024 / 1024).toFixed(1)} MiB`,
  },
  {
    key: "replicas-ready",
    label: "Ready replicas",
    description: "Ready replicas reported at one common sample timestamp.",
    aggregation: "sum",
    format: (value) =>
      value.toLocaleString(undefined, { maximumFractionDigits: 2 }),
  },
  {
    key: "container-restarts",
    label: "Container restarts",
    description: "Total restart counters at one common sample timestamp.",
    aggregation: "sum",
    format: (value) =>
      value.toLocaleString(undefined, { maximumFractionDigits: 2 }),
  },
  {
    key: "http-request-rate",
    label: "HTTP request rate",
    description: "Common-timestamp total across returned service series.",
    aggregation: "sum",
    format: (value) => `${value.toFixed(2)} req/s`,
  },
  {
    key: "http-error-ratio",
    label: "HTTP 5xx ratio",
    description:
      "Shown only when the gateway returns one series; ratios are not summed.",
    aggregation: "single-series",
    format: (value) => `${(value * 100).toFixed(2)}%`,
  },
  {
    key: "http-latency-p95",
    label: "HTTP latency p95",
    description:
      "Shown only when the gateway returns one series; percentiles are not merged.",
    aggregation: "single-series",
    format: (value) => `${(value * 1000).toFixed(1)} ms`,
  },
];

type DashboardScope =
  | {
      key: string;
      type: "namespace";
      id: string;
      title: string;
      detail: string;
    }
  | {
      key: "global:platform";
      type: "global";
      id: "platform";
      title: "Platform global";
      detail: "Every authorized platform recording-rule series";
    };

function commonTimestampTotal(result: MetricRangeResult): number | undefined {
  if (!result.series.length) return undefined;
  const firstTimestamps = result.series[0]?.samples.map(
    (sample) => sample.timestamp,
  );
  const latestCommonTimestamp = firstTimestamps
    ?.filter((timestamp) =>
      result.series.every((series) =>
        series.samples.some((sample) => sample.timestamp === timestamp),
      ),
    )
    .sort()
    .at(-1);
  if (!latestCommonTimestamp) return undefined;
  return result.series.reduce(
    (total, series) =>
      total +
      (series.samples.find(
        (sample) => sample.timestamp === latestCommonTimestamp,
      )?.value ?? 0),
    0,
  );
}

function singleSeriesLatest(result: MetricRangeResult): number | undefined {
  if (result.series.length !== 1) return undefined;
  return result.series[0]?.samples.at(-1)?.value;
}

function ScopeMetricCard({
  definition,
  scope,
  enabled,
}: {
  definition: MetricDefinition;
  scope: DashboardScope;
  enabled: boolean;
}) {
  const timeBucket = Math.floor(Date.now() / 300_000) * 300_000;
  const query = useQuery({
    queryKey: [
      "monitoring-dashboard-metric",
      scope.type,
      scope.id,
      definition.key,
      timeBucket,
    ],
    queryFn: () =>
      api.metricRange({
        scopeType: scope.type,
        scopeId: scope.id,
        metric: definition.key,
        from: new Date(timeBucket - 30 * 60_000),
        to: new Date(timeBucket),
        stepSeconds: 60,
      }),
    enabled,
    retry: false,
    refetchInterval: 30_000,
  });
  const value = query.data
    ? definition.aggregation === "sum"
      ? commonTimestampTotal(query.data)
      : singleSeriesLatest(query.data)
    : undefined;
  const seriesCount = query.data?.series.length ?? 0;
  const cannotAggregate =
    definition.aggregation === "single-series" && seriesCount > 1;
  const state = !enabled
    ? "Unavailable"
    : query.isPending
      ? "Loading"
      : query.error
        ? "Query failed"
        : cannotAggregate
          ? "Not aggregated"
          : value === undefined
            ? "No data"
            : "Live";

  return (
    <Card className="monitoring-metric-card">
      <div className="monitoring-metric-card__head">
        <div>
          <span>{definition.key}</span>
          <h3>{definition.label}</h3>
        </div>
        <PlaceholderBadge>{state}</PlaceholderBadge>
      </div>
      <strong>
        {cannotAggregate
          ? `${seriesCount} series`
          : value === undefined
            ? "—"
            : definition.format(value)}
      </strong>
      <p>{definition.description}</p>
      <small>
        {query.error
          ? "The bounded gateway query failed; this is not reported as zero."
          : query.data
            ? `${seriesCount} series · observed ${formatDate(query.data.observedAt)}`
            : "No metric snapshot loaded."}
      </small>
    </Card>
  );
}

function MonitoringDashboard({
  scope,
  enabled,
}: {
  scope: DashboardScope;
  enabled: boolean;
}) {
  return (
    <>
      <div className="monitoring-scope-summary">
        <div>
          <span className="eyebrow">
            {scope.type === "global" ? "Platform scope" : "Namespace scope"}
          </span>
          <h2>{scope.title}</h2>
          <p>{scope.detail}</p>
        </div>
        <StatusPill
          value={enabled ? "available" : "disabled"}
          label={enabled ? "Querying catalog" : "Metrics unavailable"}
        />
      </div>
      <div className="monitoring-metric-grid">
        {monitoringMetricCatalog.map((definition) => (
          <ScopeMetricCard
            key={definition.key}
            definition={definition}
            scope={scope}
            enabled={enabled}
          />
        ))}
      </div>
    </>
  );
}

function projectLabel(projects: Project[], projectId: string) {
  return (
    projects.find((project) => project.id === projectId)?.name ?? "Project"
  );
}

export function MonitoringPage() {
  const [selectedScopeKey, setSelectedScopeKey] = useState("");
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    retry: false,
  });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    retry: false,
  });
  const monitoring = useQuery({
    queryKey: ["monitoring-status"],
    queryFn: api.monitoringStatus,
    retry: false,
  });
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const accessibleEnvironments = monitoringEnvironments(
    effectiveCapabilities,
    projects.data?.items ?? [],
    environments.data?.items ?? [],
  );
  const scopes: DashboardScope[] = accessibleEnvironments.map(
    (environment) => ({
      key: `namespace:${environment.id}`,
      type: "namespace",
      id: environment.id,
      title: `${projectLabel(projects.data?.items ?? [], environment.projectId)} / ${environment.name}`,
      detail: `Kubernetes namespace ${environment.namespace}`,
    }),
  );
  if (hasGlobalMetricsAccess(effectiveCapabilities)) {
    scopes.push({
      key: "global:platform",
      type: "global",
      id: "platform",
      title: "Platform global",
      detail: "Every authorized platform recording-rule series",
    });
  }
  const selectedScope =
    scopes.find((scope) => scope.key === selectedScopeKey) ?? scopes[0];
  const loading =
    capabilities.isPending || projects.isPending || environments.isPending;
  const loadError =
    capabilities.error ?? projects.error ?? environments.error ?? null;
  const monitoringAvailable = monitoring.data?.available === true;

  return (
    <div className="page">
      <PageHeader
        eyebrow="Bounded recording-rule catalog"
        title="Monitoring"
        description="Namespace and platform views use named metrics and opaque authorized scope IDs. Free-form PromQL is never sent by this UI."
        actions={
          <StatusPill
            value={monitoringAvailable ? "available" : "disabled"}
            label={
              monitoring.isPending
                ? "Checking monitoring"
                : monitoringAvailable
                  ? "Monitoring connected"
                  : "Monitoring unavailable"
            }
          />
        }
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          title="Could not resolve monitoring access"
          onRetry={() =>
            void Promise.all([
              capabilities.refetch(),
              projects.refetch(),
              environments.refetch(),
            ])
          }
        />
      ) : null}

      {loading ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : loadError ? null : !selectedScope ? (
        <EmptyState
          icon="metrics"
          title="No monitoring scope"
          description="An effective metrics:read grant covering a project, environment, or namespace is required. Global metrics require an explicit platform-admin capability."
          action={<PlaceholderBadge>Access not granted</PlaceholderBadge>}
        />
      ) : (
        <div className="monitoring-dashboard">
          <Card className="monitoring-scope-picker">
            <div>
              <Icon name="metrics" />
              <span>
                <strong>Dashboard scope</strong>
                <small>
                  Only scopes covered by effective grants are listed.
                </small>
              </span>
            </div>
            <label>
              <span>Scope</span>
              <select
                aria-label="Monitoring scope"
                value={selectedScope.key}
                onChange={(event) => setSelectedScopeKey(event.target.value)}
              >
                {scopes.map((scope) => (
                  <option key={scope.key} value={scope.key}>
                    {scope.type === "global" ? "Global · " : "Namespace · "}
                    {scope.title}
                  </option>
                ))}
              </select>
            </label>
          </Card>

          {monitoring.error || !monitoringAvailable ? (
            <div className="notice notice--warning" role="status">
              <Icon name="metrics" />
              <div>
                <strong>Metrics are explicitly unavailable</strong>
                <p>
                  {monitoring.data?.message ??
                    "No healthy Prometheus-compatible query boundary is currently available."}
                </p>
              </div>
            </div>
          ) : null}

          <MonitoringDashboard
            scope={selectedScope}
            enabled={monitoringAvailable}
          />
        </div>
      )}
    </div>
  );
}
