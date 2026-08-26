import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
  StatusPill,
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
  const [gitEnvironmentId, setGitEnvironmentId] = useState<string | null>(null);
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
  const deployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
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
  const projectDeployments = useMemo(
    () =>
      deployments.data?.items.filter((deployment) =>
        projectApplications.some(
          (application) => application.id === deployment.applicationId,
        ),
      ) ?? [],
    [deployments.data?.items, projectApplications],
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
  useEffect(() => {
    if (
      gitEnvironmentId !== null &&
      !projectEnvironments.some(
        (environment) => environment.id === gitEnvironmentId,
      )
    ) {
      setGitEnvironmentId(null);
    }
  }, [gitEnvironmentId, projectEnvironments]);
  const loading = [projects, environments, applications, deployments].some(
    (query) => query.isPending,
  );
  const loadError =
    projects.error ??
    environments.error ??
    applications.error ??
    deployments.error;

  if (loadError) {
    return <ErrorPanel error={loadError} onRetry={() => location.reload()} />;
  }
  if (loading) return <Skeleton lines={9} />;
  if (!project) {
    return (
      <EmptyState
        title="Project unavailable"
        description="This project no longer exists or is outside your current access scope."
        action={
          <Link to="/projects" className="button button--secondary">
            Back to projects
          </Link>
        }
      />
    );
  }

  return (
    <div className="page">
      <div className="backline">
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
        className="page-tabs project-workspace-tabs"
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
        <div className="page-stack">
          {creatingEnvironment ? (
            <Card className="compact-form-card">
              <form
                className="inline-form"
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
                  <select {...form.register("protectionPolicy")}>
                    <option value="protected">Protected · pull request</option>
                    <option value="development">
                      Development · direct commit
                    </option>
                  </select>
                </Field>
                <Button type="submit" busy={createEnvironment.isPending}>
                  Create environment
                </Button>
              </form>
              {createEnvironment.error ? (
                <div className="form-error">
                  {errorMessage(createEnvironment.error)}
                </div>
              ) : null}
            </Card>
          ) : null}
          <Card className="environment-list-card">
            {projectEnvironments.length ? (
              <div className="environment-list">
                {projectEnvironments.map((environment) => (
                  <div className="environment-list__item" key={environment.id}>
                    <span className="scope-row__icon">
                      <Icon name="layers" />
                    </span>
                    <div className="environment-list__identity">
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
                      const appCount = new Set(
                        projectDeployments
                          .filter(
                            (deployment) =>
                              deployment.environmentId === environment.id,
                          )
                          .map((deployment) => deployment.applicationId),
                      ).size;
                      return (
                        <span className="environment-list__apps">
                          {appCount} App{appCount === 1 ? "" : "s"}
                        </span>
                      );
                    })()}
                    {capabilities.data?.features?.variableSets === true &&
                    hasActionAtEnvironment(
                      "deployment-config:read",
                      environment.id,
                    ) ? (
                      <Link
                        to="/environments/$environmentId/variables"
                        params={{ environmentId: environment.id }}
                        className="button button--secondary"
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
                            current === environment.id ? null : environment.id,
                          )
                        }
                      >
                        Git
                      </Button>
                    ) : null}
                    <Link
                      to="/projects/$projectId/environments/$environmentId"
                      params={{ projectId, environmentId: environment.id }}
                      className="button button--secondary"
                    >
                      Open <Icon name="arrow" />
                    </Link>
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
        </div>
      ) : null}

      {tab === "settings" ? (
        <div className="page-stack">
          {canManageAccess ? (
            <ProjectAccessPanel
              key={project.id}
              project={project}
              environments={projectEnvironments}
              applications={projectApplications}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("environments")}
            />
          ) : null}
          {showAutomation ? (
            <ProjectAutomationPanel
              key={project.id}
              project={project}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("environments")}
            />
          ) : null}
          {canDelete ? (
            <Card className="danger-zone">
              <div>
                <span className="eyebrow">Danger zone</span>
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
        </div>
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
    </div>
  );
}
