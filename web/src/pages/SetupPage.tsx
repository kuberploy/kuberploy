import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import {
  Card,
  ErrorPanel,
  PageHeader,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { Icon, type IconName } from "../components/Icon";

export function SetupPage() {
  const meta = useQuery({ queryKey: ["meta"], queryFn: api.meta });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
  });
  const monitoring = useQuery({
    queryKey: ["monitoring-status"],
    queryFn: api.monitoringStatus,
    retry: false,
  });
  const error = meta.error ?? capabilities.error;
  const sessionActions = [
    ...(capabilities.data?.actions ?? []),
    ...(capabilities.data?.capabilities?.flatMap(
      (capability) => capability.actions ?? [],
    ) ?? []),
  ].filter((action, index, actions) => actions.indexOf(action) === index);

  const checks: Array<{
    name: string;
    description: string;
    icon: IconName;
    state: string;
    label?: string;
  }> = [
    {
      name: "API contract",
      description: meta.data?.contractDigest
        ? `OpenAPI digest ${meta.data.contractDigest.slice(0, 18)}…`
        : "Contract digest is not reported yet.",
      icon: "code",
      state: meta.data ? "healthy" : "pending",
    },
    {
      name: "GitOps repository",
      description:
        "Desired-state binding and projection health is surfaced by the control plane.",
      icon: "git",
      state: featureState(capabilities.data?.features, ["gitops", "git"]),
    },
    {
      name: "Argo CD",
      description: "The only normal writer for application workloads.",
      icon: "refresh",
      state: featureState(capabilities.data?.features, ["argoCD", "argo"]),
    },
    {
      name: "Traefik edge",
      description: "HTTP routes, certificates, DNS intent, and middleware.",
      icon: "route",
      state: featureState(capabilities.data?.features, ["traefik", "edge"]),
    },
    {
      name: "Prometheus",
      description:
        monitoring.data?.message ??
        "Managed, existing, or explicitly disabled monitoring.",
      icon: "metrics",
      state: monitoring.data?.available
        ? "healthy"
        : monitoring.data?.mode === "disabled"
          ? "disabled"
          : monitoring.error
            ? "unavailable"
            : "pending",
      label: monitoring.data?.mode,
    },
    {
      name: "Builder",
      description: "Isolated image builds never mount the host Docker socket.",
      icon: "terminal",
      state: featureState(capabilities.data?.features, ["builder", "builds"]),
    },
  ];

  return (
    <div className="page">
      <PageHeader
        title="Setup & health"
        description="Runtime component status and effective session access."
        actions={
          <a className="button button--secondary" href="/docs">
            <Icon name="code" /> Open API docs
          </a>
        }
      />
      {error ? (
        <ErrorPanel
          error={error}
          onRetry={() =>
            void Promise.all([meta.refetch(), capabilities.refetch()])
          }
        />
      ) : null}
      {meta.isPending || capabilities.isPending ? (
        <Card>
          <Skeleton lines={10} />
        </Card>
      ) : (
        <>
          <section className="health-heading">
            <div>
              <h2>
                Kuberploy{" "}
                {meta.data?.platformVersion ??
                  meta.data?.version ??
                  "development"}
              </h2>
              <p>
                The API is reachable with an authenticated, same-origin session.
              </p>
            </div>
            <StatusPill value="healthy" label="Connected" />
          </section>
          <section className="system-list" aria-label="Platform components">
            {checks.map((check) => (
              <div className="system-list__row" key={check.name}>
                <span className="system-list__icon">
                  <Icon name={check.icon} />
                </span>
                <div>
                  <h3>{check.name}</h3>
                  <p>{check.description}</p>
                </div>
                <StatusPill
                  value={check.state}
                  label={check.label ?? undefined}
                />
              </div>
            ))}
          </section>
          <Card>
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Effective access</span>
                <h2>Session capabilities</h2>
              </div>
              <PlaceholderBadge>
                {sessionActions.length} reported
              </PlaceholderBadge>
            </div>
            <div className="capability-list">
              {sessionActions.map((action) => (
                <code key={action}>{action}</code>
              ))}
              {!sessionActions.length ? (
                <p>
                  No fine-grained capability list was returned. Server-side RBAC
                  still applies to every resource request.
                </p>
              ) : null}
            </div>
          </Card>
        </>
      )}
    </div>
  );
}

function featureState(
  features: Record<string, boolean> | undefined,
  names: string[],
): string {
  if (!features) return "pending";
  const found = names.find((name) => name in features);
  if (!found) return "pending";
  return features[found] ? "healthy" : "disabled";
}
