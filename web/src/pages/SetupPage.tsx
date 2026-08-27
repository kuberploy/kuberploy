import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import {
  Card,
  CardHeader,
  ErrorPanel,
  Eyebrow,
  Page,
  PageHeader,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
  buttonVariants,
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
      state: featureState(
        capabilities.data?.featureStates,
        capabilities.data?.features,
        ["gitops", "git"],
      ),
    },
    {
      name: "Argo CD",
      description: "The only normal writer for application workloads.",
      icon: "refresh",
      state: featureState(
        capabilities.data?.featureStates,
        capabilities.data?.features,
        ["argoCD", "argo"],
      ),
    },
    {
      name: "Traefik edge",
      description: "HTTP routes, certificates, DNS intent, and middleware.",
      icon: "route",
      state: featureState(
        capabilities.data?.featureStates,
        capabilities.data?.features,
        ["traefik", "edge"],
      ),
    },
    {
      name: "Prometheus",
      description:
        monitoring.data?.message ??
        "Managed, existing, or explicitly disabled monitoring.",
      icon: "metrics",
      state: monitoringState(
        monitoring.data,
        Boolean(monitoring.error),
        monitoring.isPending,
      ),
    },
    {
      name: "Builder",
      description: "Isolated image builds never mount the host Docker socket.",
      icon: "terminal",
      state: featureState(
        capabilities.data?.featureStates,
        capabilities.data?.features,
        ["builder", "builds"],
      ),
    },
  ];

  return (
    <Page>
      <PageHeader
        title="Setup & health"
        description="Runtime component status and effective session access."
        actions={
          <a className={buttonVariants({ variant: "secondary" })} href="/docs">
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
          <section className="flex items-center justify-between gap-5 py-4 px-0 border-y border-y-ink [&_h2]:m-0 [&_h2]:text-section [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta">
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
          <section className="mb-6" aria-label="Platform components">
            {checks.map((check) => (
              <div
                data-slot="system-row"
                className="grid grid-cols-[24px_minmax(0,_1fr)_auto] items-center gap-4 min-h-[60px] py-3 px-0 border-b border-b-line [&_h3]:m-0 [&_h3]:text-ink [&_h3]:text-sm [&_h3]:font-medium [&_p]:m-0 [&_p]:text-ink-faint [&_p]:text-meta [&_p]:leading-[1.5] to-620:grid-cols-[24px_minmax(0,_1fr)] to-620:py-3 to-620:px-0"
                key={check.name}
              >
                <span className="grid text-ink-soft place-items-center [&_svg]:w-[15px]">
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
            <CardHeader>
              <div>
                <Eyebrow>Effective access</Eyebrow>
                <h2>Session capabilities</h2>
              </div>
              <PlaceholderBadge>
                {sessionActions.length} reported
              </PlaceholderBadge>
            </CardHeader>
            <div className="flex flex-wrap gap-2 [&_code]:py-1.5 [&_code]:px-2 [&_code]:border [&_code]:border-line [&_code]:rounded-md [&_code]:text-mint-dark [&_code]:bg-surface-soft [&_code]:text-xs [&_p]:text-ink-faint [&_p]:text-meta">
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
    </Page>
  );
}

function monitoringState(
  status:
    | {
        mode?: string;
        status?: string;
        available?: boolean;
      }
    | undefined,
  failed: boolean,
  pending: boolean,
): "disabled" | "unavailable" | "healthy" | "pending" {
  if (failed) return "unavailable";
  if (pending) return "pending";
  if (status?.available || status?.status === "available") return "healthy";
  if (status?.status === "unavailable") return "unavailable";
  if (status?.mode === "disabled" || status?.status === "disabled")
    return "disabled";
  return "unavailable";
}

function featureState(
  states: Record<string, "disabled" | "unavailable" | "healthy"> | undefined,
  features: Record<string, boolean> | undefined,
  names: string[],
): string {
  const observed = names.find((name) => states?.[name]);
  if (observed) return states![observed]!;
  if (!features) return "pending";
  const found = names.find((name) => name in features);
  if (!found) return "pending";
  return features[found] ? "healthy" : "disabled";
}
