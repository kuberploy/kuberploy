import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMemo, useRef, useState } from "react";
import { api } from "../api/client";
import type { Project } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../components/shadcn/dialog";
import { canCreateAppInEnvironment } from "../lib/appCreationAccess";

function environmentAction(
  capabilities: Awaited<ReturnType<typeof api.capabilities>> | undefined,
  project: Project,
  environmentId: string,
  action: string,
) {
  return (capabilities?.capabilities ?? []).some(
    (capability) =>
      capability.actions?.includes(action) &&
      ((capability.scopeType === "platform" &&
        capability.scopeId === "platform") ||
        (capability.scopeType === "team" &&
          capability.scopeId === project.teamId) ||
        (capability.scopeType === "project" &&
          capability.scopeId === project.id) ||
        (capability.scopeType === "environment" &&
          capability.scopeId === environmentId)),
  );
}

export function EnvironmentPage() {
  const { projectId = "", environmentId = "" } = useParams({ strict: false });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [cloneOpen, setCloneOpen] = useState(false);
  const [cloneName, setCloneName] = useState("");
  const [cloneProtectionPolicy, setCloneProtectionPolicy] = useState<
    "inherit" | "development" | "protected"
  >("inherit");
  const cloneAttempt = useRef<{ signature: string; key: string } | null>(null);
  const environment = useQuery({
    queryKey: ["environment", environmentId],
    queryFn: () => api.environment(environmentId),
    enabled: Boolean(environmentId),
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const appPlacements = useQuery({
    queryKey: ["environment-apps", environmentId],
    queryFn: () => api.environmentApps(environmentId),
    enabled: Boolean(environmentId),
  });
  const deployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });

  const project = projects.data?.items.find((item) => item.id === projectId);
  const environmentApps = useMemo(
    () => appPlacements.data?.items ?? [],
    [appPlacements.data?.items],
  );
  const environmentDeployments = useMemo(
    () =>
      (deployments.data?.items ?? []).filter(
        (deployment) =>
          deployment.environmentId === environmentId &&
          environmentApps.some(
            (placement) => placement.applicationId === deployment.applicationId,
          ),
      ),
    [deployments.data?.items, environmentApps, environmentId],
  );

  const loading = [
    environment,
    projects,
    appPlacements,
    deployments,
    capabilities,
  ].some((query) => query.isPending);
  const loadError =
    environment.error ??
    projects.error ??
    appPlacements.error ??
    deployments.error ??
    capabilities.error;
  const cloneEnvironment = useMutation({
    mutationFn: (input: {
      name: string;
      protectionPolicy?: "development" | "protected";
      idempotencyKey: string;
    }) =>
      api.cloneEnvironment(
        environmentId,
        {
          name: input.name,
          protectionPolicy: input.protectionPolicy,
        },
        input.idempotencyKey,
      ),
    onSuccess: async (result, input) => {
      if (cloneAttempt.current?.key === input.idempotencyKey) {
        cloneAttempt.current = null;
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["environments"] }),
        queryClient.invalidateQueries({
          queryKey: ["environment-apps", result.environment.id],
        }),
      ]);
      setCloneOpen(false);
      await navigate({
        to: "/projects/$projectId/environments/$environmentId",
        params: {
          projectId,
          environmentId: result.environment.id,
        },
      });
    },
  });
  const submitClone = () => {
    const name = cloneName.trim();
    if (!name || cloneEnvironment.isPending) return;
    const protectionPolicy =
      cloneProtectionPolicy === "inherit" ? undefined : cloneProtectionPolicy;
    const signature = JSON.stringify({ name, protectionPolicy });
    const idempotencyKey =
      cloneAttempt.current?.signature === signature
        ? cloneAttempt.current.key
        : crypto.randomUUID();
    cloneAttempt.current = { signature, key: idempotencyKey };
    cloneEnvironment.mutate({ name, protectionPolicy, idempotencyKey });
  };

  if (loadError) {
    return (
      <ErrorPanel
        error={loadError}
        onRetry={() =>
          void Promise.all([
            environment.refetch(),
            projects.refetch(),
            appPlacements.refetch(),
            deployments.refetch(),
            capabilities.refetch(),
          ])
        }
      />
    );
  }
  if (loading) return <Skeleton lines={8} />;
  if (
    !project ||
    !environment.data ||
    environment.data.projectId !== project.id
  ) {
    return (
      <EmptyState
        title="Environment unavailable"
        description="This environment no longer exists or is outside this project and your current access scope."
        action={
          <Link to="/projects" className="button button--secondary">
            Back to projects
          </Link>
        }
      />
    );
  }

  const canAddApp = canCreateAppInEnvironment(
    capabilities.data,
    project,
    environment.data,
  );
  const canCreateEnvironment = environmentAction(
    capabilities.data,
    project,
    environment.data.id,
    "environments:create",
  );
  return (
    <div className="page">
      <nav className="backline" aria-label="Breadcrumb">
        <Link to="/projects">
          <Icon name="arrow" /> Projects
        </Link>
        <span>/</span>
        <Link to="/projects/$projectId" params={{ projectId: project.id }}>
          {project.name}
        </Link>
        <span>/</span>
        <span aria-current="page">{environment.data.name}</span>
      </nav>

      <PageHeader
        eyebrow="Environment"
        title={environment.data.name}
        description={`${environment.data.namespace} · ${
          environment.data.protectionPolicy === "development"
            ? "Direct Git publication"
            : "Protected pull-request publication"
        } · ${environmentApps.length} App${environmentApps.length === 1 ? "" : "s"}`}
        actions={
          <>
            <StatusPill value={environment.data.status ?? "active"} />
            <Button
              variant="secondary"
              disabled={!canCreateEnvironment}
              onClick={() => {
                setCloneName(`${environment.data.name} copy`);
                setCloneProtectionPolicy("inherit");
                setCloneOpen(true);
              }}
              title={
                canCreateEnvironment
                  ? "Clone Apps as stopped drafts."
                  : "You do not have permission to create environments."
              }
            >
              <Icon name="layers" /> Clone Environment
            </Button>
            {canAddApp ? (
              <Link
                to="/projects/$projectId/environments/$environmentId/apps/new"
                params={{
                  projectId: project.id,
                  environmentId: environment.data.id,
                }}
                className="button button--primary"
              >
                <Icon name="plus" /> Add App
              </Link>
            ) : null}
          </>
        }
      />

      <Card className="environment-list-card">
        {environmentApps.length ? (
          <div aria-label="Apps">
            {environmentApps.map((placement) => {
              const appDeployments = environmentDeployments.filter(
                (deployment) =>
                  deployment.applicationId === placement.applicationId,
              );
              const deployment = appDeployments[0];
              return (
                <Link
                  key={placement.applicationId}
                  to="/projects/$projectId/environments/$environmentId/apps/$applicationId"
                  params={{
                    projectId: project.id,
                    environmentId: environment.data.id,
                    applicationId: placement.applicationId,
                  }}
                  className="scope-row scope-row--link"
                >
                  <span className="scope-row__icon scope-row__icon--app">
                    {placement.applicationName.slice(0, 1).toUpperCase()}
                  </span>
                  <div>
                    <strong>{placement.applicationName}</strong>
                    <small>
                      {appDeployments.length
                        ? `${appDeployments.length} running instance${appDeployments.length === 1 ? "" : "s"} in ${environment.data.name}`
                        : `Stopped draft · review settings before start`}
                    </small>
                  </div>
                  <StatusPill
                    value={deployment?.state ?? deployment?.status ?? "stopped"}
                    label={deployment ? undefined : "Stopped / draft"}
                  />
                </Link>
              );
            })}
          </div>
        ) : (
          <EmptyState
            compact
            icon="apps"
            title="No Apps yet"
            description="Add an App to this environment when its source and runtime settings are ready."
          />
        )}
      </Card>
      <Dialog
        open={cloneOpen}
        onOpenChange={(open) => {
          if (!cloneEnvironment.isPending) setCloneOpen(open);
        }}
      >
        <DialogContent className="max-w-none sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Clone {environment.data.name}</DialogTitle>
            <DialogDescription>
              Apps become stopped drafts. Kuberploy creates no workloads and
              copies no secret values; review each App before starting it.
            </DialogDescription>
          </DialogHeader>
          <label className="field">
            <span className="field__label">New environment name *</span>
            <input
              autoFocus
              value={cloneName}
              maxLength={100}
              onChange={(event) => setCloneName(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter") submitClone();
              }}
            />
          </label>
          <label className="field">
            <span className="field__label">Protection policy</span>
            <select
              value={cloneProtectionPolicy}
              onChange={(event) =>
                setCloneProtectionPolicy(
                  event.target.value as typeof cloneProtectionPolicy,
                )
              }
            >
              <option value="inherit">Inherit source policy</option>
              <option value="development">Development · direct Git</option>
              <option value="protected">Protected · pull request</option>
            </select>
          </label>
          {cloneEnvironment.error ? (
            <ErrorPanel
              error={cloneEnvironment.error}
              title="Environment was not cloned"
            />
          ) : null}
          <DialogFooter>
            <Button
              variant="secondary"
              disabled={cloneEnvironment.isPending}
              onClick={() => setCloneOpen(false)}
            >
              Cancel
            </Button>
            <Button
              busy={cloneEnvironment.isPending}
              disabled={!cloneName.trim()}
              onClick={submitClone}
            >
              Clone as stopped drafts
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
