import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Project } from "../api/types";
import { EnvironmentGitBindingPanel } from "../components/EnvironmentGitBindingPanel";
import { Icon } from "../components/Icon";
import { ProjectAccessPanel } from "../components/ProjectAccessPanel";
import { ProjectAutomationPanel } from "../components/ProjectAutomationPanel";
import {
  Select,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Page,
  PageHeader,
  PageStack,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import { canDeleteProject } from "../lib/appCreationAccess";
import { projectOwnershipLabel } from "./ProjectsPage";

type ProjectTab = "environments" | "settings";
type EnvironmentForm = {
  name: string;
  protectionPolicy: "development" | "protected";
};

export function ProjectPage() {
  const { projectId } = useParams({ from: "/projects/$projectId" });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<ProjectTab>("environments");
  const [creatingEnvironment, setCreatingEnvironment] = useState(false);
  const [gitEnvironmentChoice, setGitEnvironmentId] = useState<string | null>(
    null,
  );
  const [deleteOpen, setDeleteOpen] = useState(false);
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
  });
  const teams = useQuery({ queryKey: ["teams"], queryFn: api.teams });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
  });
  const applications = useQuery({
    queryKey: ["applications"],
    queryFn: api.applications,
  });
  const form = useForm<EnvironmentForm>({
    defaultValues: { name: "", protectionPolicy: "protected" },
  });
  const environmentAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  const deleteAttempt = useRef<string | null>(null);
  const project = projects.data?.items.find((item) => item.id === projectId);
  const projectEnvironments = useMemo(
    () =>
      environments.data?.items.filter((item) => item.projectId === projectId) ??
      [],
    [environments.data?.items, projectId],
  );
  const projectApplications = useMemo(
    () =>
      applications.data?.items.filter((item) => item.projectId === projectId) ??
      [],
    [applications.data?.items, projectId],
  );
  const environmentAppQueries = useQueries({
    queries: projectEnvironments.map((environment) => ({
      queryKey: ["environment-apps", environment.id],
      queryFn: () => api.environmentApps(environment.id),
    })),
  });
  const environmentAppCounts = new Map(
    projectEnvironments.map((environment, index) => [
      environment.id,
      environmentAppQueries[index]?.data?.items.length ?? 0,
    ]),
  );
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const hasActionAtProject = (action: string, candidate: Project) =>
    effectiveCapabilities.some(
      (capability) =>
        capability.actions?.includes(action) &&
        ((capability.scopeType === "platform" &&
          capability.scopeId === "platform") ||
          (capability.scopeType === "team" &&
            capability.scopeId === candidate.teamId) ||
          (capability.scopeType === "project" &&
            capability.scopeId === candidate.id)),
    );
  const hasActionAtEnvironment = (action: string, environmentId: string) =>
    Boolean(project && hasActionAtProject(action, project)) ||
    effectiveCapabilities.some(
      (capability) =>
        capability.actions?.includes(action) &&
        capability.scopeType === "environment" &&
        capability.scopeId === environmentId,
    );
  const canCreateEnvironment = Boolean(
    project && hasActionAtProject("environments:create", project),
  );
  const canDelete = Boolean(
    project && canDeleteProject(capabilities.data, project),
  );
  const canManageAccess = Boolean(
    project && hasActionAtProject("access-grants:create", project),
  );
  const showAutomation = Boolean(
    project &&
    capabilities.data?.features?.serviceAccounts === true &&
    hasActionAtProject("access-grants:read", project),
  );
  const createEnvironment = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: { projectId: string } & EnvironmentForm;
      idempotencyKey: string;
    }) => api.createEnvironment(input, idempotencyKey),
    onSuccess: async (_value, input) => {
      if (input.input.projectId === projectId) {
        const current = form.getValues();
        const submitted = {
          name: input.input.name,
          protectionPolicy: input.input.protectionPolicy,
        };
        if (JSON.stringify(current) === JSON.stringify(submitted)) {
          if (environmentAttempt.current?.key === input.idempotencyKey) {
            environmentAttempt.current = null;
          }
          form.reset();
          setCreatingEnvironment(false);
        }
      }
      await queryClient.invalidateQueries({ queryKey: ["environments"] });
    },
  });
  const deleteProject = useMutation({
    mutationFn: (idempotencyKey: string) =>
      api.deleteProject(projectId, project?.name ?? "", idempotencyKey),
    onSuccess: async () => {
      deleteAttempt.current = null;
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["projects"] }),
        queryClient.invalidateQueries({ queryKey: ["project", projectId] }),
      ]);
      await navigate({ to: "/projects" });
    },
  });
  const submitEnvironment = (value: EnvironmentForm) => {
    const input = { projectId, ...value };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      environmentAttempt.current?.signature === signature
        ? environmentAttempt.current.key
        : crypto.randomUUID();
    environmentAttempt.current = { signature, key: idempotencyKey };
    createEnvironment.mutate({ input, idempotencyKey });
  };
  useEffect(() => {
    setTab("environments");
    form.reset({ name: "", protectionPolicy: "protected" });
    environmentAttempt.current = null;
    createEnvironment.reset();
    setCreatingEnvironment(false);
    setGitEnvironmentId(null);
    setDeleteOpen(false);
    deleteAttempt.current = null;
    deleteProject.reset();
  }, [projectId]);
  // The opened environment is a preference; whether the Git panel is actually
  // open is derived from the environments this render can see, so an
  // environment that disappears closes the panel in the same render.
  const gitEnvironmentId =
    gitEnvironmentChoice !== null &&
    projectEnvironments.some(
      (environment) => environment.id === gitEnvironmentChoice,
    )
      ? gitEnvironmentChoice
      : null;
  const loading =
    [projects, environments, applications].some((query) => query.isPending) ||
    environmentAppQueries.some((query) => query.isPending);
  const loadError =
    projects.error ??
    environments.error ??
    applications.error ??
    environmentAppQueries.find((query) => query.error)?.error;

  if (loadError) {
    return <ErrorPanel error={loadError} onRetry={() => location.reload()} />;
  }
  if (loading)
    return (
      <Page>
        <Skeleton lines={9} />
      </Page>
    );
  if (!project) {
    return (
      <Page>
        <EmptyState
          title="Project unavailable"
          description="This project no longer exists or is outside your current access scope."
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

  return (
    <Page>
      <div className="flex items-center gap-2 mb-5 text-ink-faint text-meta [&_a]:inline-flex [&_a]:items-center [&_a]:gap-1.5 [&_a]:text-mint-dark [&_a_svg]:w-3 [&_a_svg]:transform-[rotate(180deg)] pointer-coarse:[&_a]:inline-flex pointer-coarse:[&_a]:min-h-8 pointer-coarse:[&_a]:items-center">
        <Link to="/projects">
          <Icon name="arrow" /> Projects
        </Link>
      </div>
      <PageHeader
        eyebrow={projectOwnershipLabel(project, teams.data?.items ?? [])}
        title={project.name}
        description={
          project.description ??
          `${projectEnvironments.length} environment${projectEnvironments.length === 1 ? "" : "s"} · ${projectApplications.length} app${projectApplications.length === 1 ? "" : "s"}`
        }
        actions={
          tab === "environments" && canCreateEnvironment ? (
            <Button onClick={() => setCreatingEnvironment((value) => !value)}>
              <Icon name="plus" /> Environment
            </Button>
          ) : undefined
        }
      />

      <nav
        className="[&_button:focus-visible]:outline-[3px] [&_button:focus-visible]:outline-focus [&_button:focus-visible]:outline-offset-[2px] flex gap-6 mt-[-4px] mx-0 mb-5 border-b border-b-line [&_button]:relative [&_button]:pt-0 [&_button]:px-px [&_button]:pb-[11px] [&_button]:border-0 [&_button]:text-ink-faint [&_button]:bg-transparent [&_button]:cursor-pointer [&_button]:text-meta [&_button]:font-semibold [&_button]:pb-3 [&_button]:transition-[color] [&_button]:duration-(--motion-fast) [&_button]:ease-(--ease-standard) [&_button.active]:text-ink [&_button.active::after]:absolute [&_button.active::after]:right-0 [&_button.active::after]:bottom-[-1px] [&_button.active::after]:left-0 [&_button.active::after]:h-0.5 [&_button.active::after]:content-[''] [&_button.active::after]:bg-mint-dark [&_button.active::after]:origin-left [&_button.active::after]:animate-[tab-underline_var(--motion-base)_var(--ease-standard)] to-580:max-w-full to-580:gap-4 to-580:overflow-x-auto pointer-coarse:[&_button]:min-h-10 [&_button:hover:not(:disabled)]:text-ink mb-6"
        aria-label="Project sections"
      >
        {(["environments", "settings"] as const).map((item) => (
          <button
            key={item}
            className={tab === item ? "active" : ""}
            aria-current={tab === item ? "page" : undefined}
            onClick={() => setTab(item)}
          >
            {item === "environments"
              ? `Environments (${projectEnvironments.length})`
              : "Access & automation"}
          </button>
        ))}
      </nav>

      {tab === "environments" ? (
        <PageStack>
          {creatingEnvironment ? (
            <Card className="!p-5">
              <form
                className="grid grid-cols-[1fr_1fr_auto] items-end gap-3 to-580:grid-cols-[1fr]"
                onSubmit={form.handleSubmit(submitEnvironment)}
              >
                <Field
                  label="Environment name"
                  required
                  error={form.formState.errors.name?.message}
                >
                  <input
                    placeholder="Production"
                    {...form.register("name", {
                      required: "Enter an environment name.",
                    })}
                  />
                </Field>
                <Field label="Git publication" required>
                  <Select
                    {...form.register("protectionPolicy")}
                    value={form.watch("protectionPolicy")}
                  >
                    <option value="protected">Protected · pull request</option>
                    <option value="development">
                      Development · direct commit
                    </option>
                  </Select>
                </Field>
                <Button type="submit" busy={createEnvironment.isPending}>
                  Create environment
                </Button>
              </form>
              {createEnvironment.error ? (
                <div className="col-[1_/_-1] text-tone-bad text-meta">
                  {errorMessage(createEnvironment.error)}
                </div>
              ) : null}
            </Card>
          ) : null}
          <Card className="!p-0 overflow-hidden">
            {projectEnvironments.length ? (
              <div className="flex flex-col">
                {projectEnvironments.map((environment) => (
                  <div
                    className="grid grid-cols-[34px_minmax(0,_1fr)_minmax(0,_auto)_auto_minmax(0,_auto)_auto] items-center gap-3 py-4 px-5 border-b border-b-line last:border-b-0 to-1080:grid-cols-[34px_minmax(0,_1fr)_auto_auto] to-1080:[&>code]:col-[2_/_-1] [&>div]:grid [&>div]:gap-1 [&_strong]:text-meta [&_small]:text-ink-soft [&_small]:text-xs [&_code]:text-ink-soft [&_code]:text-xs [&>code]:min-w-0 [&>code]:overflow-hidden [&>code]:text-ellipsis [&>code]:whitespace-nowrap to-760:grid-cols-[34px_minmax(0,_1fr)_auto] to-760:[&>code]:col-[2_/_-1] to-760:[&>[data-slot='button']]:col-[span_1]"
                    key={environment.id}
                  >
                    <span className="grid w-[30px] h-[30px] place-items-center rounded-lg text-ink-soft bg-surface-soft text-meta font-bold [&_svg]:w-3.5">
                      <Icon name="layers" />
                    </span>
                    <div className="[&_a:hover_strong]:text-mint-dark [&_a:focus-visible_strong]:text-mint-dark pointer-coarse:[&_a]:inline-flex pointer-coarse:[&_a]:min-h-8 pointer-coarse:[&_a]:items-center">
                      <Link
                        to="/projects/$projectId/environments/$environmentId"
                        params={{ projectId, environmentId: environment.id }}
                      >
                        <strong>{environment.name}</strong>
                      </Link>
                      <small>
                        {environment.protectionPolicy === "development"
                          ? "Direct Git publication"
                          : "Protected pull-request publication"}
                      </small>
                    </div>
                    <code>{environment.namespace}</code>
                    <StatusPill value={environment.status ?? "active"} />
                    {(() => {
                      const appCount =
                        environmentAppCounts.get(environment.id) ?? 0;
                      return (
                        <span className="to-1080:col-[2] to-1080:justify-self-start text-ink-soft text-xs font-medium whitespace-nowrap to-760:col-[2]">
                          {appCount} App{appCount === 1 ? "" : "s"}
                        </span>
                      );
                    })()}
                    <div className="!flex items-center gap-3 justify-self-end">
                      {capabilities.data?.features?.variableSets === true &&
                      hasActionAtEnvironment(
                        "deployment-config:read",
                        environment.id,
                      ) ? (
                        <Link
                          to="/environments/$environmentId/variables"
                          params={{ environmentId: environment.id }}
                          className={buttonVariants({ variant: "secondary" })}
                        >
                          Variables
                        </Link>
                      ) : null}
                      {hasActionAtEnvironment(
                        "deployment-config:read",
                        environment.id,
                      ) ? (
                        <Button
                          variant="secondary"
                          onClick={() =>
                            setGitEnvironmentId((current) =>
                              current === environment.id
                                ? null
                                : environment.id,
                            )
                          }
                        >
                          Git
                        </Button>
                      ) : null}
                      <Link
                        to="/projects/$projectId/environments/$environmentId"
                        params={{ projectId, environmentId: environment.id }}
                        className={buttonVariants({ variant: "secondary" })}
                      >
                        Open <Icon name="arrow" />
                      </Link>
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <EmptyState
                compact
                title="No environments"
                description="Create development, staging, or production before adding an App."
              />
            )}
          </Card>
          {projectEnvironments
            .filter((environment) => environment.id === gitEnvironmentId)
            .map((environment) => (
              <EnvironmentGitBindingPanel
                key={environment.id}
                environment={environment}
                humanSession={me.data?.authentication?.kind === "session"}
                canManage={
                  hasActionAtEnvironment(
                    "deployment-config:write",
                    environment.id,
                  ) &&
                  hasActionAtEnvironment(
                    "build-definitions:write",
                    environment.id,
                  )
                }
                onClose={() => setGitEnvironmentId(null)}
              />
            ))}
        </PageStack>
      ) : null}

      {tab === "settings" ? (
        <PageStack>
          {canManageAccess ? (
            <ProjectAccessPanel
              key={`access-${project.id}`}
              project={project}
              environments={projectEnvironments}
              applications={projectApplications}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("environments")}
            />
          ) : null}
          {showAutomation ? (
            <ProjectAutomationPanel
              key={`automation-${project.id}`}
              project={project}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("environments")}
            />
          ) : null}
          {canDelete ? (
            <Card className="border-[color-mix(in_srgb,_var(--red)_28%,_var(--line))] border-l border-l-red">
              <div>
                <Eyebrow>Danger zone</Eyebrow>
                <h2>Delete project</h2>
                <p>
                  Delete this Project after removing its Environments, Apps,
                  active service accounts, secrets, and other owned resources.
                </p>
              </div>
              <Button variant="danger" onClick={() => setDeleteOpen(true)}>
                <Icon name="close" /> Delete Project
              </Button>
            </Card>
          ) : null}
          {!canManageAccess && !showAutomation ? (
            <EmptyState
              title="Project settings are read-only"
              description="Your current role does not manage access grants or service accounts for this project."
            />
          ) : null}
        </PageStack>
      ) : null}
      {deleteOpen ? (
        <ConfirmDialog
          title={`Delete ${project.name}?`}
          description="Only a Project with no Environments, Apps, active service accounts, secrets, or other owned resources can be deleted. Audit history remains."
          confirmLabel="Delete Project"
          confirmation={project.name}
          busy={deleteProject.isPending}
          error={deleteProject.error}
          icon="close"
          onCancel={() => {
            deleteProject.reset();
            deleteAttempt.current = null;
            setDeleteOpen(false);
          }}
          onConfirm={() => {
            const key = deleteAttempt.current ?? crypto.randomUUID();
            deleteAttempt.current = key;
            deleteProject.mutate(key);
          }}
        />
      ) : null}
    </Page>
  );
}
