import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Project, Team } from "../api/types";
import { Icon } from "../components/Icon";
import { EnvironmentGitBindingPanel } from "../components/EnvironmentGitBindingPanel";
import { ProjectAccessPanel } from "../components/ProjectAccessPanel";
import { ProjectAutomationPanel } from "../components/ProjectAutomationPanel";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";

type ProjectForm = { name: string; slug: string; teamId: string };
type EnvironmentForm = {
  projectId: string;
  name: string;
  slug: string;
  protectionPolicy: "development" | "protected";
};

export function projectOwnershipLabel(project: Project, teams: Team[]) {
  if (!project.teamId) return "Platform-only";
  return (
    teams.find((team) => team.id === project.teamId)?.name ?? "Team-scoped"
  );
}

export function ProjectsPage() {
  const queryClient = useQueryClient();
  const [projectFilter, setProjectFilter] = useState("");
  const [panel, setPanel] = useState<"project" | "environment" | null>(null);
  const [accessProjectId, setAccessProjectId] = useState<string | null>(null);
  const [automationProjectId, setAutomationProjectId] = useState<string | null>(
    null,
  );
  const [gitEnvironmentId, setGitEnvironmentId] = useState<string | null>(null);
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
  const projectForm = useForm<ProjectForm>({
    defaultValues: { name: "", slug: "", teamId: "" },
  });
  const environmentForm = useForm<EnvironmentForm>({
    defaultValues: {
      projectId: "",
      name: "",
      slug: "",
      protectionPolicy: "protected",
    },
  });

  const createProject = useMutation({
    mutationFn: api.createProject,
    onSuccess: async () => {
      projectForm.reset();
      setPanel(null);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });
  const createEnvironment = useMutation({
    mutationFn: api.createEnvironment,
    onSuccess: async () => {
      environmentForm.reset();
      setPanel(null);
      await queryClient.invalidateQueries({ queryKey: ["environments"] });
    },
  });

  const grouped = useMemo(
    () =>
      (projects.data?.items ?? []).map((project) => ({
        project,
        environments:
          environments.data?.items.filter(
            (environment) => environment.projectId === project.id,
          ) ?? [],
        applications:
          applications.data?.items.filter(
            (application) => application.projectId === project.id,
          ) ?? [],
      })),
    [applications.data, environments.data, projects.data],
  );
  const visibleProjects = useMemo(() => {
    const query = projectFilter.trim().toLocaleLowerCase();
    if (!query) return grouped;
    return grouped.filter(
      ({ project, applications: projectApplications }) =>
        project.name.toLocaleLowerCase().includes(query) ||
        projectApplications.some((application) =>
          application.name.toLocaleLowerCase().includes(query),
        ),
    );
  }, [grouped, projectFilter]);

  const teamsById = useMemo(
    () => new Map(teams.data?.items.map((team) => [team.id, team]) ?? []),
    [teams.data],
  );
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const variableSetsEnabled =
    capabilities.data?.features?.variableSets === true;
  const hasActionAtProject = (action: string, project: Project) =>
    effectiveCapabilities.some(
      (capability) =>
        capability.actions?.includes(action) &&
        (capability.scopeType === "platform" ||
          (capability.scopeType === "team" &&
            capability.scopeId === project.teamId) ||
          (capability.scopeType === "project" &&
            capability.scopeId === project.id)),
    );
  const hasActionAtEnvironment = (
    action: string,
    project: Project,
    environmentId: string,
  ) =>
    hasActionAtProject(action, project) ||
    effectiveCapabilities.some(
      (capability) =>
        capability.actions?.includes(action) &&
        capability.scopeType === "environment" &&
        capability.scopeId === environmentId,
    );
  const canCreatePlatformProject = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.actions?.includes("projects:create"),
  );
  const projectCreationTeams =
    teams.data?.items.filter((team) =>
      effectiveCapabilities.some(
        (capability) =>
          capability.actions?.includes("projects:create") &&
          (capability.scopeType === "platform" ||
            (capability.scopeType === "team" &&
              capability.scopeId === team.id)),
      ),
    ) ?? [];
  const environmentProjects =
    projects.data?.items.filter((project) =>
      hasActionAtProject("environments:create", project),
    ) ?? [];
  const canCreateProject =
    canCreatePlatformProject || projectCreationTeams.length > 0;
  const canManageProjectAccess = (project: Project) =>
    hasActionAtProject("access-grants:create", project);
  const canViewProjectAutomation = (project: Project) =>
    capabilities.data?.features?.serviceAccounts === true &&
    hasActionAtProject("access-grants:read", project);

  const loading = [
    me,
    teams,
    projects,
    environments,
    applications,
    deployments,
  ].some((query) => query.isPending);
  const loadError =
    me.error ??
    teams.error ??
    projects.error ??
    environments.error ??
    applications.error ??
    deployments.error;

  return (
    <div className="page">
      <PageHeader
        eyebrow="Workspace"
        title="Projects & environments"
        description="Organize logical applications, namespaces, and Argo CD policy boundaries without hiding the Kubernetes mapping."
        actions={
          <>
            {environmentProjects.length ? (
              <Button
                variant="secondary"
                onClick={() => setPanel("environment")}
              >
                <Icon name="plus" /> Environment
              </Button>
            ) : null}
            {canCreateProject ? (
              <Button onClick={() => setPanel("project")}>
                <Icon name="plus" /> Project
              </Button>
            ) : null}
          </>
        }
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              me.refetch(),
              capabilities.refetch(),
              projects.refetch(),
              teams.refetch(),
              environments.refetch(),
              applications.refetch(),
              deployments.refetch(),
            ])
          }
        />
      ) : null}

      {panel ? (
        <Card className="creation-panel">
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Create</span>
              <h2>{panel === "project" ? "New project" : "New environment"}</h2>
            </div>
            <button
              className="icon-button"
              onClick={() => setPanel(null)}
              aria-label="Close form"
            >
              <Icon name="close" />
            </button>
          </div>
          {panel === "project" ? (
            <form
              onSubmit={projectForm.handleSubmit((value) =>
                createProject.mutate({
                  name: value.name,
                  slug: value.slug || undefined,
                  teamId: value.teamId || undefined,
                }),
              )}
              className="inline-form inline-form--project-team"
            >
              <Field
                label="Name"
                required
                error={projectForm.formState.errors.name?.message}
              >
                <input
                  placeholder="Payments"
                  {...projectForm.register("name", {
                    required: "Enter a project name.",
                  })}
                />
              </Field>
              <Field
                label="Slug"
                hint="Optional; generated by the API when omitted."
              >
                <input
                  placeholder="payments"
                  {...projectForm.register("slug")}
                />
              </Field>
              <Field
                label="Team ownership"
                required={!canCreatePlatformProject}
                hint={
                  canCreatePlatformProject
                    ? "Platform-only projects remain visible only through platform permissions."
                    : "Your team members will share access to this project."
                }
                error={projectForm.formState.errors.teamId?.message}
              >
                <select
                  {...projectForm.register("teamId", {
                    required: canCreatePlatformProject
                      ? false
                      : "Select a team for this project.",
                  })}
                >
                  <option value="">
                    {canCreatePlatformProject ? "Platform-only" : "Select team"}
                  </option>
                  {projectCreationTeams.map((team) => (
                    <option key={team.id} value={team.id}>
                      {team.name}
                    </option>
                  ))}
                </select>
              </Field>
              <Button type="submit" busy={createProject.isPending}>
                Create project
              </Button>
              {createProject.error ? (
                <div className="form-error">
                  {errorMessage(createProject.error)}
                </div>
              ) : null}
            </form>
          ) : (
            <form
              className="inline-form inline-form--wide"
              onSubmit={environmentForm.handleSubmit((value) =>
                createEnvironment.mutate({
                  ...value,
                  slug: value.slug || undefined,
                }),
              )}
            >
              <Field
                label="Project"
                required
                error={environmentForm.formState.errors.projectId?.message}
              >
                <select
                  {...environmentForm.register("projectId", {
                    required: "Select a project.",
                  })}
                >
                  <option value="">Select project</option>
                  {environmentProjects.map((project) => (
                    <option key={project.id} value={project.id}>
                      {project.name}
                      {project.teamId && teamsById.has(project.teamId)
                        ? ` · ${teamsById.get(project.teamId)?.name}`
                        : ""}
                    </option>
                  ))}
                </select>
              </Field>
              <Field
                label="Name"
                required
                error={environmentForm.formState.errors.name?.message}
              >
                <input
                  placeholder="Development"
                  {...environmentForm.register("name", {
                    required: "Enter an environment name.",
                  })}
                />
              </Field>
              <div className="inline-empty">
                Kuberploy assigns the namespace and Argo CD project from this
                project and environment identity.
              </div>
              <Field label="Git publication" required>
                <select {...environmentForm.register("protectionPolicy")}>
                  <option value="protected">
                    Protected · pull request review
                  </option>
                  <option value="development">
                    Development · direct Git commit
                  </option>
                </select>
              </Field>
              <div className="inline-empty">
                This policy is immutable. Protected environments require a
                freshly verified branch policy and never deploy a candidate
                before its pull request is merged and indexed.
              </div>
              <input type="hidden" {...environmentForm.register("slug")} />
              <Button type="submit" busy={createEnvironment.isPending}>
                Create environment
              </Button>
              {createEnvironment.error ? (
                <div className="form-error">
                  {errorMessage(createEnvironment.error)}
                </div>
              ) : null}
            </form>
          )}
        </Card>
      ) : null}

      {!loading && grouped.length ? (
        <div className="project-toolbar">
          <input
            type="search"
            aria-label="Filter projects"
            placeholder="Filter projects or services…"
            value={projectFilter}
            onChange={(event) => setProjectFilter(event.target.value)}
          />
          <span>
            {visibleProjects.length} of {grouped.length} projects
          </span>
        </div>
      ) : null}

      {loading ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : visibleProjects.length ? (
        <div className="project-stack">
          {visibleProjects.map(
            ({
              project,
              environments: projectEnvironments,
              applications: projectApplications,
            }) => (
              <Card key={project.id} className="project-card">
                <div className="project-card__header">
                  <span className="project-avatar">
                    {project.name.slice(0, 2).toUpperCase()}
                  </span>
                  <div>
                    <div className="eyebrow">Project</div>
                    <h2>{project.name}</h2>
                    <p>
                      {project.description ??
                        `${projectApplications.length} applications across ${projectEnvironments.length} environments`}
                    </p>
                  </div>
                  <div className="project-card__badges">
                    <span className="project-owner">
                      <Icon name="user" />
                      {projectOwnershipLabel(project, teams.data?.items ?? [])}
                    </span>
                    <StatusPill value={project.status ?? "active"} />
                    {canManageProjectAccess(project) ? (
                      <Button
                        variant="secondary"
                        onClick={() => {
                          setAutomationProjectId(null);
                          setAccessProjectId((current) =>
                            current === project.id ? null : project.id,
                          );
                        }}
                      >
                        <Icon name="user" /> Access
                      </Button>
                    ) : null}
                    {canViewProjectAutomation(project) ? (
                      <Button
                        variant="secondary"
                        onClick={() => {
                          setAccessProjectId(null);
                          setAutomationProjectId((current) =>
                            current === project.id ? null : project.id,
                          );
                        }}
                      >
                        <Icon name="terminal" /> Automation
                      </Button>
                    ) : null}
                  </div>
                </div>
                <div className="project-card__body">
                  <div className="scope-column">
                    <h3>
                      Environments <span>{projectEnvironments.length}</span>
                    </h3>
                    {projectEnvironments.length ? (
                      projectEnvironments.map((environment) => (
                        <div className="scope-row" key={environment.id}>
                          <span className="scope-row__icon">
                            <Icon name="layers" />
                          </span>
                          <div>
                            <strong>{environment.name}</strong>
                            <small>
                              <code>{environment.namespace}</code> · Argo{" "}
                              {environment.argoProject ?? "project default"} ·{" "}
                              {environment.protectionPolicy === "development"
                                ? "direct Git"
                                : "PR protected"}
                            </small>
                          </div>
                          <StatusPill value={environment.status ?? "active"} />
                          {hasActionAtEnvironment(
                            "deployment-config:read",
                            project,
                            environment.id,
                          ) ? (
                            <>
                              {variableSetsEnabled ? (
                                <Link
                                  to="/environments/$environmentId/variables"
                                  params={{ environmentId: environment.id }}
                                  className="button button--secondary"
                                >
                                  <Icon name="terminal" /> Variables
                                </Link>
                              ) : null}
                              <Button
                                variant="secondary"
                                onClick={() => {
                                  setAccessProjectId(null);
                                  setAutomationProjectId(null);
                                  setGitEnvironmentId((current) =>
                                    current === environment.id
                                      ? null
                                      : environment.id,
                                  );
                                }}
                              >
                                <Icon name="git" /> Git
                              </Button>
                            </>
                          ) : null}
                        </div>
                      ))
                    ) : (
                      <p className="muted-copy">No namespace bindings yet.</p>
                    )}
                  </div>
                  <div className="scope-column">
                    <h3>
                      Applications <span>{projectApplications.length}</span>
                    </h3>
                    {projectApplications.length ? (
                      projectApplications.map((application) => {
                        const appDeployments =
                          deployments.data?.items.filter(
                            (deployment) =>
                              deployment.applicationId === application.id,
                          ) ?? [];
                        const firstDeployment = appDeployments[0];
                        return firstDeployment ? (
                          <Link
                            key={application.id}
                            to="/applications/$applicationId"
                            params={{
                              applicationId: application.id,
                            }}
                            className="scope-row scope-row--link"
                          >
                            <span className="scope-row__icon scope-row__icon--app">
                              {application.name.slice(0, 1).toUpperCase()}
                            </span>
                            <div>
                              <strong>{application.name}</strong>
                              <small>
                                {appDeployments.length} deployment
                                {appDeployments.length === 1 ? "" : "s"}
                              </small>
                            </div>
                            <Icon name="chevron" />
                          </Link>
                        ) : (
                          <Link
                            className="scope-row scope-row--link"
                            key={application.id}
                            to="/applications/$applicationId"
                            params={{ applicationId: application.id }}
                          >
                            <span className="scope-row__icon scope-row__icon--app">
                              {application.name.slice(0, 1).toUpperCase()}
                            </span>
                            <div>
                              <strong>{application.name}</strong>
                              <small>Not deployed</small>
                            </div>
                            <Icon name="chevron" />
                          </Link>
                        );
                      })
                    ) : (
                      <p className="muted-copy">No applications yet.</p>
                    )}
                  </div>
                </div>
                {(() => {
                  const environment = projectEnvironments.find(
                    (item) => item.id === gitEnvironmentId,
                  );
                  return environment ? (
                    <EnvironmentGitBindingPanel
                      environment={environment}
                      humanSession={me.data?.authentication?.kind === "session"}
                      canManage={
                        hasActionAtEnvironment(
                          "deployment-config:write",
                          project,
                          environment.id,
                        ) &&
                        hasActionAtEnvironment(
                          "build-definitions:write",
                          project,
                          environment.id,
                        )
                      }
                      onClose={() => setGitEnvironmentId(null)}
                    />
                  ) : null;
                })()}
                {accessProjectId === project.id ? (
                  <ProjectAccessPanel
                    project={project}
                    environments={projectEnvironments}
                    applications={projectApplications}
                    capabilities={effectiveCapabilities}
                    onClose={() => setAccessProjectId(null)}
                  />
                ) : null}
                {automationProjectId === project.id ? (
                  <ProjectAutomationPanel
                    project={project}
                    capabilities={effectiveCapabilities}
                    onClose={() => setAutomationProjectId(null)}
                  />
                ) : null}
              </Card>
            ),
          )}
        </div>
      ) : grouped.length ? (
        <EmptyState
          icon="layers"
          title="No matching project"
          description="Try another project or service name."
        />
      ) : (
        <EmptyState
          icon="layers"
          title="Create your first project"
          description="A project groups applications and the environment namespaces they may use."
          action={
            canCreateProject ? (
              <Button onClick={() => setPanel("project")}>
                <Icon name="plus" /> New project
              </Button>
            ) : undefined
          }
        />
      )}
    </div>
  );
}
