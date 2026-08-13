import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api } from "../api/client";
import {
  EmptyState,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
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
    <div className="page">
      <PageHeader
        title="Overview"
        description="Projects, workloads, and delivery activity for this control plane."
        actions={
          <Link to="/deploy" className="button button--primary">
            <Icon name="plus" /> Deploy image
          </Link>
        }
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

      <section className="summary-strip" aria-label="Workspace summary">
        {[
          ["Projects", projects.data?.items.length ?? 0, "Git-backed scopes"],
          ["Applications", applications.data?.items.length ?? 0, "Workloads"],
          [
            "Deployments",
            deployments.data?.items.length ?? 0,
            "Environment bindings",
          ],
          ["In progress", activeOperations, "Operations"],
        ].map(([label, value, detail]) => (
          <div className="summary-strip__item" key={label}>
            <span>{label}</span>
            <strong>{loading ? "—" : value}</strong>
            <small>{detail}</small>
          </div>
        ))}
      </section>

      <section className="summary-strip" aria-label="Platform health">
        {[
          [
            "GitOps",
            dashboardFeatureState(capabilities.data?.featureStates, [
              "gitops",
              "git",
            ]),
          ],
          [
            "Argo CD",
            dashboardFeatureState(capabilities.data?.featureStates, [
              "argoCD",
              "argo",
            ]),
          ],
          [
            "Edge",
            dashboardFeatureState(capabilities.data?.featureStates, [
              "traefik",
              "edge",
            ]),
          ],
          [
            "Monitoring",
            monitoring.data?.available
              ? "healthy"
              : monitoring.data?.status === "unavailable"
                ? "unavailable"
                : monitoring.data?.mode === "disabled"
                  ? "disabled"
                  : "pending",
          ],
        ].map(([label, state]) => (
          <div className="summary-strip__item" key={label}>
            <span>{label}</span>
            <StatusPill value={state} />
            <small>Platform runtime</small>
          </div>
        ))}
      </section>

      <div className="console-grid">
        <section className="console-section">
          <div className="console-section__header">
            <div>
              <h2>Recent deployments</h2>
              <p>Latest desired state by application and environment.</p>
            </div>
            <Link to="/projects" className="text-link">
              View all <Icon name="arrow" />
            </Link>
          </div>
          {loading ? (
            <div className="console-section__body">
              <Skeleton lines={5} />
            </div>
          ) : deployments.data?.items.length ? (
            <div className="resource-list">
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
                    className="resource-row"
                  >
                    <span className="resource-row__mark">
                      {(application?.name ?? deployment.name ?? "A")
                        .slice(0, 2)
                        .toUpperCase()}
                    </span>
                    <span className="resource-row__main">
                      <strong>
                        {application?.name ??
                          deployment.name ??
                          `Deployment ${shortId(deployment.id)}`}
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
                    <span className="resource-row__time">
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
              title="No deployments yet"
              description="Deploy an immutable image digest to create the first Git-backed release."
              action={
                <Link to="/deploy" className="button button--secondary">
                  Deploy image
                </Link>
              }
              compact
            />
          )}
        </section>

        <section className="console-section">
          <div className="console-section__header">
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
    </div>
  );
}

function dashboardFeatureState(
  states: Record<string, "disabled" | "unavailable" | "healthy"> | undefined,
  names: string[],
) {
  const key = names.find((name) => states?.[name]);
  return key ? states![key]! : "pending";
}
