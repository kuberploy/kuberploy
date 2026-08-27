import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMemo, useRef, useState } from "react";
import { api } from "../api/client";
import type { Project } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  EmptyState,
  ErrorPanel,
  FieldLabel,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import {
  canCreateAppInEnvironment,
  canDeleteEnvironment,
} from "../lib/appCreationAccess";

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
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [cloneName, setCloneName] = useState("");
  const [cloneProtectionPolicy, setCloneProtectionPolicy] = useState<
    "inherit" | "development" | "protected"
  >("inherit");
  const cloneAttempt = useRef<{ signature: string; key: string } | null>(null);
  const deleteAttempt = useRef<string | null>(null);
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
  const deleteEnvironment = useMutation({
    mutationFn: (idempotencyKey: string) =>
      api.deleteEnvironment(
        environmentId,
        environment.data?.name ?? "",
        idempotencyKey,
      ),
    onSuccess: async () => {
      deleteAttempt.current = null;
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["environments"] }),
        queryClient.invalidateQueries({
          queryKey: ["environment", environmentId],
        }),
      ]);
      await navigate({ to: "/projects/$projectId", params: { projectId } });
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
  if (loading)
    return (
      <Page>
        <Skeleton lines={8} />
      </Page>
    );
  if (
    !project ||
    !environment.data ||
    environment.data.projectId !== project.id
  ) {
    return (
      <Page>
        <EmptyState
          title="Environment unavailable"
          description="This environment no longer exists or is outside this project and your current access scope."
          action={
            <Link
              to="/projects"
              className={buttonVariants({ variant: "secondary" })}
            >
              Back to projects
            </Link>
          }
        />
      </Page>
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
  const canDelete = canDeleteEnvironment(capabilities.data, project);
  return (
    <Page>
      <nav
        className="flex items-center gap-2 mb-5 text-ink-faint text-meta [&_a]:inline-flex [&_a]:items-center [&_a]:gap-1.5 [&_a]:text-mint-dark [&_a_svg]:w-3 [&_a_svg]:transform-[rotate(180deg)] pointer-coarse:[&_a]:inline-flex pointer-coarse:[&_a]:min-h-8 pointer-coarse:[&_a]:items-center"
        aria-label="Breadcrumb"
      >
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
            {canDelete ? (
              <Button variant="danger" onClick={() => setDeleteOpen(true)}>
                <Icon name="close" /> Delete Environment
              </Button>
            ) : null}
            {canAddApp ? (
              <Link
                to="/projects/$projectId/environments/$environmentId/apps/new"
                params={{
                  projectId: project.id,
                  environmentId: environment.data.id,
                }}
                className={buttonVariants({ variant: "primary" })}
              >
                <Icon name="plus" /> Add App
              </Link>
            ) : null}
          </>
        }
      />

      <Card className="!p-0 overflow-hidden">
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
                  className="grid min-h-[54px] grid-cols-[32px_1fr_auto] items-center gap-3 py-2 px-1.5 border-t border-t-line [&>svg]:w-[13px] [&>svg]:text-ink-faint [&_div]:min-w-0 [&_strong]:block [&_strong]:text-meta [&_small]:block [&_small]:mt-1 [&_small]:overflow-hidden [&_small]:text-ink-faint [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap [&_small]:leading-[1.5] hover:bg-surface-soft"
                >
                  <span className="grid w-[30px] h-[30px] place-items-center rounded-lg text-meta font-bold [&_svg]:w-3.5 text-tone-info bg-tone-info-surface">
                    {placement.applicationName.slice(0, 1).toUpperCase()}
                  </span>
                  <div>
                    <strong>{placement.applicationName}</strong>
                    <small>
                      {deployment?.state === "stopped"
                        ? "Stopped draft · review settings before start"
                        : appDeployments.length
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
            action={
              canAddApp ? (
                <Link
                  to="/projects/$projectId/environments/$environmentId/apps/new"
                  params={{
                    projectId: project.id,
                    environmentId: environment.data.id,
                  }}
                  className={buttonVariants({ variant: "primary" })}
                >
                  <Icon name="plus" /> Add App
                </Link>
              ) : undefined
            }
          />
        )}
      </Card>
      <Dialog
        open={cloneOpen}
        onOpenChange={(open) => {
          if (!cloneEnvironment.isPending) setCloneOpen(open);
        }}
      >
        <DialogContent className="max-w-none">
          <DialogHeader>
            <DialogTitle>Clone {environment.data.name}</DialogTitle>
            <DialogDescription>
              Apps become stopped drafts. Kuberploy creates no workloads and
              copies no secret values; review each App before starting it.
            </DialogDescription>
          </DialogHeader>
          <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
            <FieldLabel>New environment name *</FieldLabel>
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
          <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
            <FieldLabel>Protection policy</FieldLabel>
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
      {deleteOpen ? (
        <ConfirmDialog
          title={`Delete ${environment.data.name}?`}
          description="Only an Environment with no deployments, Git binding, releases, variables, certificates, or integrations can be deleted. Audit history remains."
          confirmLabel="Delete Environment"
          confirmation={environment.data.name}
          busy={deleteEnvironment.isPending}
          error={deleteEnvironment.error}
          icon="close"
          onCancel={() => {
            deleteEnvironment.reset();
            setDeleteOpen(false);
          }}
          onConfirm={() => {
            const key = deleteAttempt.current ?? crypto.randomUUID();
            deleteAttempt.current = key;
            deleteEnvironment.mutate(key);
          }}
        />
      ) : null}
    </Page>
  );
}
