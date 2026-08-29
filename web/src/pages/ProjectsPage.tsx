import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Project, Team } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Page,
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
    <Page>
      <PageHeader
        eyebrow="Workspace"
        title="Projects"
        description="Choose a project, then an environment, to manage Apps."
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
        <Card className="mb-5 py-5 px-6 border-mint-line">
          <CardHeader>
            <div>
              <Eyebrow>Create</Eyebrow>
              <h2>{panel === "project" ? "New project" : "New environment"}</h2>
            </div>
            <button
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              onClick={() => setPanel(null)}
              aria-label="Close form"
            >
              <Icon name="close" />
            </button>
          </CardHeader>
          {panel === "project" ? (
            <form
              onSubmit={projectForm.handleSubmit(submitProject)}
              className="grid items-end gap-3 to-580:grid-cols-[1fr] grid-cols-[1fr_1fr_minmax(180px,_0.85fr)_auto] to-1120:grid-cols-[1fr_1fr] to-1120:[&_[data-slot='button']]:w-max"
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
                <Select
                  {...projectForm.register("teamId", {
                    required: canCreatePlatformProject
                      ? false
                      : "Select a team for this project.",
                  })}
                  value={projectForm.watch("teamId")}
                >
                  <option value="">
                    {canCreatePlatformProject ? "Platform-only" : "Select team"}
                  </option>
                  {projectCreationTeams.map((team) => (
                    <option key={team.id} value={team.id}>
                      {team.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Button type="submit" busy={createProject.isPending}>
                Create project
              </Button>
              {createProject.error ? (
                <div className="col-[1_/_-1] text-tone-bad text-meta">
                  {errorMessage(createProject.error)}
                </div>
              ) : null}
            </form>
          ) : (
            <form
              className="grid items-end gap-3 to-580:grid-cols-[1fr] grid-cols-[1fr_1fr_1fr_1fr_auto] to-1120:grid-cols-[1fr_1fr] to-1120:[&_[data-slot='button']]:w-max"
              onSubmit={environmentForm.handleSubmit(submitEnvironment)}
            >
              <Field
                label="Project"
                required
                error={environmentForm.formState.errors.projectId?.message}
              >
                <Select
                  {...environmentForm.register("projectId", {
                    required: "Select a project.",
                  })}
                  value={environmentForm.watch("projectId")}
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
                </Select>
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
              <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
                Kuberploy assigns the namespace and Argo CD project from this
                project and environment identity.
              </div>
              <Field label="Git publication" required>
                <Select
                  {...environmentForm.register("protectionPolicy")}
                  value={environmentForm.watch("protectionPolicy")}
                >
                  <option value="protected">
                    Protected · pull request review
                  </option>
                  <option value="development">
                    Development · direct Git commit
                  </option>
                </Select>
              </Field>
              <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
                This policy cannot be changed after creation. Protected
                environments require a freshly verified branch policy and never
                deploy a candidate before its pull request is merged and
                indexed.
              </div>
              <input type="hidden" {...environmentForm.register("slug")} />
              <Button type="submit" busy={createEnvironment.isPending}>
                Create environment
              </Button>
              {createEnvironment.error ? (
                <div className="col-[1_/_-1] text-tone-bad text-meta">
                  {errorMessage(createEnvironment.error)}
                </div>
              ) : null}
            </form>
          )}
        </Card>
      ) : null}

      {!loading && grouped.length ? (
        <div className="flex items-center gap-4 mb-5 [&_input]:max-w-[560px] [&_input]:bg-surface [&_span]:ml-[auto] [&_span]:text-ink-soft [&_span]:text-xs [&_span]:whitespace-nowrap to-760:items-stretch to-760:flex-col to-760:[&_input]:max-w-[none] to-760:[&_span]:ml-0">
          <input
            type="search"
            aria-label="Filter projects"
            placeholder="Filter projects or Apps…"
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
        <div className="grid grid-cols-[repeat(auto-fill,_minmax(min(100%,_340px),_1fr))] gap-4 to-700:grid-cols-[minmax(0,_1fr)]">
          {visibleProjects.map(
            ({
              project,
              environments: projectEnvironments,
              applications: projectApplications,
            }) => (
              <Card
                key={project.id}
                className="!p-0 overflow-hidden transition-[border-color,box-shadow] duration-(--motion-fast) ease-(--ease-standard) hover:border-line-strong hover:shadow-[0_3px_12px_rgba(24_24_27_0.07)]"
              >
                <Link
                  to="/projects/$projectId"
                  params={{ projectId: project.id }}
                  className="grid min-h-[220px] grid-rows-[auto_1fr_auto] focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[-3px]"
                >
                  <div className="grid grid-cols-[40px_minmax(0,_1fr)_18px] items-start gap-3 p-5 [&>svg]:w-4 [&>svg]:mt-2 [&>svg]:text-ink-faint [&_h2]:m-0 [&_h2]:text-section [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_h2]:leading-[1.3] [&_p]:[display:-webkit-box] [&_p]:min-h-[calc(2_*_1.45em)] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:overflow-hidden [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.45] [&_p]:[-webkit-box-orient:vertical] [&_p]:line-clamp-2">
                    <span className="grid w-11 h-11 place-items-center border border-mint-line rounded-panel text-mint-dark bg-mint-soft text-[11px] font-bold">
                      {project.name.slice(0, 2).toUpperCase()}
                    </span>
                    <div>
                      <h2>{project.name}</h2>
                      <p>
                        {project.description ??
                          `${projectApplications.length} App${projectApplications.length === 1 ? "" : "s"}`}
                      </p>
                    </div>
                    <Icon name="chevron" />
                  </div>
                  <dl className="grid grid-cols-[repeat(3,_1fr)] my-0 mx-5 border-y border-y-line [&>div]:grid [&>div]:gap-1 [&>div]:py-4 [&>div]:px-2 [&>div]:text-center [&_dt]:text-ink-faint [&_dt]:text-xs [&_dd]:m-0 [&_dd]:text-lg [&_dd]:tabular-nums [&_dd]:font-semibold [&_dd]:leading-[1.2]">
                    <div>
                      <dt>Apps</dt>
                      <dd>{projectApplications.length}</dd>
                    </div>
                    <div>
                      <dt>Environments</dt>
                      <dd>{projectEnvironments.length}</dd>
                    </div>
                    <div>
                      <dt>App instances</dt>
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
                  <div className="flex items-center justify-between gap-3 py-4 px-5">
                    <span className="inline-flex max-w-[210px] items-center gap-1.5 overflow-hidden py-0 px-2 border border-line rounded-full text-ink-soft bg-surface-soft font-semibold text-ellipsis whitespace-nowrap min-h-7 text-xs [&_svg]:w-3 [&_svg]:flex-none">
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
          description="Try another project or App name."
          action={
            <Button variant="secondary" onClick={() => setProjectFilter("")}>
              Clear filter
            </Button>
          }
        />
      ) : (
        <EmptyState
          icon="layers"
          title="Create your first project"
          description="A project groups environments and their Apps."
          action={
            canCreateProject ? (
              <Button onClick={() => setPanel("project")}>
                <Icon name="plus" /> New project
              </Button>
            ) : undefined
          }
        />
      )}
    </Page>
  );
}
