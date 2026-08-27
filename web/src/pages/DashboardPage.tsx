import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api } from "../api/client";
import {
  EmptyState,
  ErrorPanel,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import { Icon } from "../components/Icon";
import { OperationTimeline } from "../components/OperationTimeline";
import { relativeTime, shortId } from "../lib/format";

export function DashboardPage() {
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const applications = useQuery({
    queryKey: ["applications"],
    queryFn: api.applications,
  });
  const deployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
    refetchInterval: 15_000,
  });
  const operations = useQuery({
    queryKey: ["operations"],
    queryFn: api.operations,
    refetchInterval: 5_000,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const monitoring = useQuery({
    queryKey: ["monitoring-status"],
    queryFn: api.monitoringStatus,
    retry: false,
  });

  const error =
    projects.error ??
    applications.error ??
    deployments.error ??
    operations.error;
  const loading = [projects, applications, deployments, operations].some(
    (query) => query.isPending,
  );
  const activeOperations =
    operations.data?.items.filter(
      (operation) =>
        !["succeeded", "healthy", "failed", "cancelled", "superseded"].includes(
          operation.state.toLowerCase(),
        ),
    ).length ?? 0;

  return (
    <Page>
      <PageHeader
        title="Overview"
        description="Projects, workloads, and delivery activity for this control plane."
      />

      {error && !loading ? (
        <ErrorPanel
          error={error}
          onRetry={() =>
            void Promise.all([
              projects.refetch(),
              applications.refetch(),
              deployments.refetch(),
              operations.refetch(),
            ])
          }
        />
      ) : null}

      <section
        className="grid grid-cols-[repeat(4,_minmax(0,_1fr))] mb-8 border-y border-y-line to-900:grid-cols-[repeat(2,_minmax(0,_1fr))]"
        aria-label="Workspace summary"
      >
        {[
          ["Projects", projects.data?.items.length ?? 0, "Git-backed scopes"],
          ["Applications", applications.data?.items.length ?? 0, "Workloads"],
          [
            "App instances",
            deployments.data?.items.length ?? 0,
            "Environment runtimes",
          ],
          ["In progress", activeOperations, "Operations"],
        ].map(([label, value, detail]) => (
          <div
            className="grid grid-cols-[minmax(0,_1fr)_auto] content-center gap-y-1 gap-x-4 min-h-[76px] py-4 px-5 border-r border-r-line first:pl-0 last:border-r-0 [&>span:not([data-slot='status-pill'])]:self-end [&>span:not([data-slot='status-pill'])]:text-ink [&>span:not([data-slot='status-pill'])]:text-meta [&>span:not([data-slot='status-pill'])]:font-medium [&>strong]:row-[1_/_3] [&>strong]:col-[2] [&>strong]:self-center [&>strong]:justify-self-end [&>strong]:text-ink [&>strong]:text-[26px] [&>strong]:tabular-nums [&>strong]:font-medium [&>strong]:tracking-[-0.02em] [&>strong]:leading-none [&>[data-slot='status-pill']]:row-[1_/_3] [&>[data-slot='status-pill']]:col-[2] [&>[data-slot='status-pill']]:self-center [&>[data-slot='status-pill']]:justify-self-end [&>small]:text-ink-faint [&>small]:text-xs [&>small]:leading-[1.4] to-900:[&:nth-child(2n)]:border-r-0 to-900:[&:nth-child(2n)]:pr-0 to-900:[&:nth-child(2n+1)]:pl-0 to-900:[&:nth-child(-n+2)]:border-b to-900:[&:nth-child(-n+2)]:border-b-line to-700:grid-cols-[minmax(0,_1fr)] to-700:gap-1 to-700:py-4 to-700:px-3 to-700:[&>strong]:row-[auto] to-700:[&>strong]:col-[1] to-700:[&>strong]:justify-self-start to-700:[&>[data-slot='status-pill']]:row-[auto] to-700:[&>[data-slot='status-pill']]:col-[1] to-700:[&>[data-slot='status-pill']]:justify-self-start to-700:[&>small]:order-3"
            key={label}
          >
            <span>{label}</span>
            <strong>{loading ? "—" : value}</strong>
            <small>{detail}</small>
          </div>
        ))}
      </section>

      <section
        className="grid grid-cols-[repeat(4,_minmax(0,_1fr))] mb-8 border-y border-y-line to-900:grid-cols-[repeat(2,_minmax(0,_1fr))]"
        aria-label="Platform health"
      >
        {[
          [
            "GitOps",
            dashboardFeatureState(
              capabilities.data?.featureStates,
              capabilities.data?.features,
              ["gitops", "git"],
              Boolean(capabilities.error),
            ),
          ],
          [
            "Argo CD",
            dashboardFeatureState(
              capabilities.data?.featureStates,
              capabilities.data?.features,
              ["argoCD", "argo"],
              Boolean(capabilities.error),
            ),
          ],
          [
            "Edge",
            dashboardFeatureState(
              capabilities.data?.featureStates,
              capabilities.data?.features,
              ["traefik", "edge"],
              Boolean(capabilities.error),
            ),
          ],
          [
            "Monitoring",
            dashboardMonitoringState(
              monitoring.data,
              Boolean(monitoring.error),
              monitoring.isPending,
            ),
          ],
        ].map(([label, state]) => (
          <div
            className="grid grid-cols-[minmax(0,_1fr)_auto] content-center gap-y-1 gap-x-4 min-h-[76px] py-4 px-5 border-r border-r-line first:pl-0 last:border-r-0 [&>span:not([data-slot='status-pill'])]:self-end [&>span:not([data-slot='status-pill'])]:text-ink [&>span:not([data-slot='status-pill'])]:text-meta [&>span:not([data-slot='status-pill'])]:font-medium [&>strong]:row-[1_/_3] [&>strong]:col-[2] [&>strong]:self-center [&>strong]:justify-self-end [&>strong]:text-ink [&>strong]:text-[26px] [&>strong]:tabular-nums [&>strong]:font-medium [&>strong]:tracking-[-0.02em] [&>strong]:leading-none [&>[data-slot='status-pill']]:row-[1_/_3] [&>[data-slot='status-pill']]:col-[2] [&>[data-slot='status-pill']]:self-center [&>[data-slot='status-pill']]:justify-self-end [&>small]:text-ink-faint [&>small]:text-xs [&>small]:leading-[1.4] to-900:[&:nth-child(2n)]:border-r-0 to-900:[&:nth-child(2n)]:pr-0 to-900:[&:nth-child(2n+1)]:pl-0 to-900:[&:nth-child(-n+2)]:border-b to-900:[&:nth-child(-n+2)]:border-b-line to-700:grid-cols-[minmax(0,_1fr)] to-700:gap-1 to-700:py-4 to-700:px-3 to-700:[&>strong]:row-[auto] to-700:[&>strong]:col-[1] to-700:[&>strong]:justify-self-start to-700:[&>[data-slot='status-pill']]:row-[auto] to-700:[&>[data-slot='status-pill']]:col-[1] to-700:[&>[data-slot='status-pill']]:justify-self-start to-700:[&>small]:order-3"
            key={label}
          >
            <span>{label}</span>
            <StatusPill value={state} />
            <small>Platform runtime</small>
          </div>
        ))}
      </section>

      <div className="grid grid-cols-[minmax(0,_1.55fr)_minmax(310px,_0.75fr)] items-start gap-8 to-900:grid-cols-[1fr] to-700:gap-6">
        <section className="min-w-0 border-t border-t-ink border-b border-b-line">
          <div className="flex items-center justify-between flex-wrap gap-y-2 gap-x-5 py-4 px-0 border-b border-b-line [&>div]:min-w-0 [&_h2]:m-0 [&_h2]:text-lead [&_h2]:font-semibold [&_h2]:tracking-[-0.015em] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-faint [&_p]:text-meta">
            <div>
              <h2>Recent Apps</h2>
              <p>Latest desired state by application and environment.</p>
            </div>
            <Link
              to="/projects"
              className="inline-flex items-center gap-1.5 py-0.5 px-0 border-0 rounded-sm text-mint-dark bg-transparent cursor-pointer text-meta font-medium whitespace-nowrap hover:underline hover:underline-offset-[3px] focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[3px] [&_svg]:w-3.5 [&_svg]:h-3.5 pointer-coarse:inline-flex pointer-coarse:min-h-8 pointer-coarse:items-center"
            >
              View all <Icon name="arrow" />
            </Link>
          </div>
          {loading ? (
            <div className="py-2 px-0">
              <Skeleton lines={5} />
            </div>
          ) : deployments.data?.items.length ? (
            <div className="flex flex-col">
              {deployments.data.items.slice(0, 5).map((deployment) => {
                const application = applications.data?.items.find(
                  (item) => item.id === deployment.applicationId,
                );
                return (
                  <Link
                    key={deployment.id}
                    to="/applications/$applicationId/deployments/$deploymentId"
                    params={{
                      applicationId: deployment.applicationId,
                      deploymentId: deployment.id,
                    }}
                    className="grid min-h-16 grid-cols-[36px_minmax(0,_1fr)_auto_auto_16px] items-center gap-3 py-2 px-3 border-b border-b-line rounded-lg transition-[background] duration-(--motion-fast) ease-(--ease-standard) last:border-b-0 hover:bg-surface-soft [&>svg]:w-4 [&>svg]:text-ink-faint to-580:grid-cols-[35px_minmax(0,_1fr)_auto] to-580:[&>svg]:hidden to-700:grid-cols-[36px_minmax(0,_1fr)_auto] to-700:gap-y-2 to-700:[&>svg:last-child]:hidden"
                  >
                    <span className="grid w-9 h-9 place-items-center border border-line rounded-lg text-ink-soft bg-surface-soft text-xs font-semibold tracking-[0.02em]">
                      {(application?.name ?? deployment.name ?? "A")
                        .slice(0, 2)
                        .toUpperCase()}
                    </span>
                    <span className="flex min-w-0 flex-col gap-1 [&_strong]:text-ink [&_strong]:text-sm [&_strong]:font-medium [&_strong]:tracking-[-0.01em] [&_small]:overflow-hidden [&_small]:text-ink-faint [&_small]:font-mono [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap">
                      <strong>
                        {application?.name ??
                          deployment.name ??
                          `App ${shortId(deployment.id)}`}
                      </strong>
                      <small>
                        {deployment.image ??
                          deployment.source?.reference ??
                          "Image resolving"}
                      </small>
                    </span>
                    <StatusPill
                      value={deployment.state ?? deployment.status ?? "pending"}
                    />
                    <span className="text-ink-faint text-xs tabular-nums text-right whitespace-nowrap to-580:hidden to-700:col-[2_/_-1] to-700:text-left">
                      {relativeTime(
                        deployment.updatedAt ?? deployment.createdAt,
                      )}
                    </span>
                    <Icon name="chevron" />
                  </Link>
                );
              })}
            </div>
          ) : (
            <EmptyState
              icon="deploy"
              title="No Apps running yet"
              description="Open a Project Environment to add its first App."
              compact
              action={
                <Link
                  to="/projects"
                  className={buttonVariants({ variant: "secondary" })}
                >
                  <Icon name="layers" /> Browse projects
                </Link>
              }
            />
          )}
        </section>

        <section className="min-w-0 border-t border-t-ink border-b border-b-line">
          <div className="flex items-center justify-between flex-wrap gap-y-2 gap-x-5 py-4 px-0 border-b border-b-line [&>div]:min-w-0 [&_h2]:m-0 [&_h2]:text-lead [&_h2]:font-semibold [&_h2]:tracking-[-0.015em] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-faint [&_p]:text-meta">
            <div>
              <h2>Operations</h2>
              <p>Recent Git publication and reconciliation work.</p>
            </div>
            {activeOperations ? (
              <StatusPill
                value="running"
                label={`${activeOperations} active`}
              />
            ) : null}
          </div>
          {loading ? (
            <Skeleton lines={5} />
          ) : (
            <OperationTimeline
              operations={operations.data?.items.slice(0, 6) ?? []}
              empty="No release operations have run yet."
            />
          )}
        </section>
      </div>
    </Page>
  );
}

function dashboardFeatureState(
  states: Record<string, "disabled" | "unavailable" | "healthy"> | undefined,
  features: Record<string, boolean> | undefined,
  names: string[],
  failed: boolean,
): "disabled" | "unavailable" | "healthy" | "pending" {
  if (failed) return "unavailable";
  const key = names.find((name) => states?.[name]);
  if (key) return states![key]!;
  if (!features) return "pending";
  const feature = names.find((name) => name in features);
  if (!feature) return "pending";
  return features[feature] ? "healthy" : "disabled";
}

function dashboardMonitoringState(
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
