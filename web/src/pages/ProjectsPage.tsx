import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Project, Team } from "../api/types";
import { Icon } from "../components/Icon";
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
  const projectAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
  const environmentAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);

  const createProject = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: { name: string; slug?: string; teamId?: string };
      idempotencyKey: string;
    }) => api.createProject(input, idempotencyKey),
    onSuccess: async (_value, input) => {
      if (projectAttempt.current?.key === input.idempotencyKey) {
        const current = projectForm.getValues();
        const submitted = {
          name: input.input.name,
          slug: input.input.slug ?? "",
          teamId: input.input.teamId ?? "",
        };
        if (JSON.stringify(current) === JSON.stringify(submitted)) {
          projectAttempt.current = null;
          projectForm.reset();
          if (panel === "project") setPanel(null);
        }
      }
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });
  const createEnvironment = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: {
        projectId: string;
        name: string;
        slug?: string;
        protectionPolicy: EnvironmentForm["protectionPolicy"];
      };
      idempotencyKey: string;
    }) => api.createEnvironment(input, idempotencyKey),
    onSuccess: async (_value, input) => {
      if (environmentAttempt.current?.key === input.idempotencyKey) {
        const current = environmentForm.getValues();
        const submitted = {
          projectId: input.input.projectId,
          name: input.input.name,
          slug: input.input.slug ?? "",
          protectionPolicy: input.input.protectionPolicy,
        };
        if (JSON.stringify(current) === JSON.stringify(submitted)) {
          environmentAttempt.current = null;
          environmentForm.reset();
          if (panel === "environment") setPanel(null);
        }
      }
      await queryClient.invalidateQueries({ queryKey: ["environments"] });
    },
  });

  const submitProject = (value: ProjectForm) => {
    const input = {
      name: value.name,
      slug: value.slug || undefined,
      teamId: value.teamId || undefined,
    };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      projectAttempt.current?.signature === signature
        ? projectAttempt.current.key
        : crypto.randomUUID();
    projectAttempt.current = { signature, key: idempotencyKey };
    createProject.mutate({ input, idempotencyKey });
  };

  const submitEnvironment = (value: EnvironmentForm) => {
    const input = { ...value, slug: value.slug || undefined };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      environmentAttempt.current?.signature === signature
        ? environmentAttempt.current.key
        : crypto.randomUUID();
    environmentAttempt.current = { signature, key: idempotencyKey };
    createEnvironment.mutate({ input, idempotencyKey });
  };

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
  const hasActionAtProject = (action: string, project: Project) =>
    effectiveCapabilities.some(
      (capability) =>
        capability.actions?.includes(action) &&
        ((capability.scopeType === "platform" &&
          capability.scopeId === "platform") ||
          (capability.scopeType === "team" &&
            capability.scopeId === project.teamId) ||
          (capability.scopeType === "project" &&
            capability.scopeId === project.id)),
    );
  const canCreatePlatformProject = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes("projects:create"),
  );
  const projectCreationTeams =
    teams.data?.items.filter((team) =>
      effectiveCapabilities.some(
        (capability) =>
          capability.actions?.includes("projects:create") &&
          ((capability.scopeType === "platform" &&
            capability.scopeId === "platform") ||
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
        title="Projects"
        description="Choose a project to manage its services and environments."
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
              onSubmit={projectForm.handleSubmit(submitProject)}
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
              onSubmit={environmentForm.handleSubmit(submitEnvironment)}
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
        <div className="project-grid">
          {visibleProjects.map(
            ({
              project,
              environments: projectEnvironments,
              applications: projectApplications,
            }) => (
              <Card key={project.id} className="project-index-card">
                <Link
                  to="/projects/$projectId"
                  params={{ projectId: project.id }}
                  className="project-index-card__link"
                >
                  <div className="project-index-card__heading">
                    <span className="project-avatar">
                      {project.name.slice(0, 2).toUpperCase()}
                    </span>
                    <div>
                      <h2>{project.name}</h2>
                      <p>
                        {project.description ??
                          `${projectApplications.length} service${projectApplications.length === 1 ? "" : "s"}`}
                      </p>
                    </div>
                    <Icon name="chevron" />
                  </div>
                  <dl className="project-index-card__stats">
                    <div>
                      <dt>Services</dt>
                      <dd>{projectApplications.length}</dd>
                    </div>
                    <div>
                      <dt>Environments</dt>
                      <dd>{projectEnvironments.length}</dd>
                    </div>
                    <div>
                      <dt>Deployments</dt>
                      <dd>
                        {deployments.data?.items.filter((deployment) =>
                          projectApplications.some(
                            (application) =>
                              application.id === deployment.applicationId,
                          ),
                        ).length ?? 0}
                      </dd>
                    </div>
                  </dl>
                  <div className="project-index-card__footer">
                    <span className="project-owner">
                      <Icon name="user" />
                      {projectOwnershipLabel(project, teams.data?.items ?? [])}
                    </span>
                    <StatusPill value={project.status ?? "active"} />
                  </div>
                </Link>
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
