import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useMemo, useState } from "react";
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
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { projectOwnershipLabel } from "./ProjectsPage";

type ProjectTab = "services" | "environments" | "settings";
type EnvironmentForm = {
  name: string;
  protectionPolicy: "development" | "protected";
};

export function ProjectPage() {
  const { projectId } = useParams({ from: "/projects/$projectId" });
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<ProjectTab>("services");
  const [creatingEnvironment, setCreatingEnvironment] = useState(false);
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
  const form = useForm<EnvironmentForm>({
    defaultValues: { name: "", protectionPolicy: "protected" },
  });
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
        (capability.scopeType === "platform" ||
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
  const canManageAccess = Boolean(
    project && hasActionAtProject("access-grants:create", project),
  );
  const showAutomation = Boolean(
    project &&
    capabilities.data?.features?.serviceAccounts === true &&
    hasActionAtProject("access-grants:read", project),
  );
  const createEnvironment = useMutation({
    mutationFn: (value: EnvironmentForm) =>
      api.createEnvironment({ projectId, ...value }),
    onSuccess: async () => {
      form.reset();
      setCreatingEnvironment(false);
      await queryClient.invalidateQueries({ queryKey: ["environments"] });
    },
  });
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
          `${projectApplications.length} service${projectApplications.length === 1 ? "" : "s"} · ${projectEnvironments.length} environment${projectEnvironments.length === 1 ? "" : "s"}`
        }
        actions={
          tab === "services" ? (
            <Link to="/deploy" className="button button--primary">
              <Icon name="plus" /> Create service
            </Link>
          ) : tab === "environments" && canCreateEnvironment ? (
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
        {(["services", "environments", "settings"] as const).map((item) => (
          <button
            key={item}
            className={tab === item ? "active" : ""}
            aria-current={tab === item ? "page" : undefined}
            onClick={() => setTab(item)}
          >
            {item === "services"
              ? `Services (${projectApplications.length})`
              : item === "environments"
                ? `Environments (${projectEnvironments.length})`
                : "Access & automation"}
          </button>
        ))}
      </nav>

      {tab === "services" ? (
        projectApplications.length ? (
          <div className="service-card-grid">
            {projectApplications.map((application) => {
              const appDeployments = projectDeployments.filter(
                (deployment) => deployment.applicationId === application.id,
              );
              return (
                <Link
                  key={application.id}
                  to="/applications/$applicationId"
                  params={{ applicationId: application.id }}
                  className="service-card"
                >
                  <div className="service-card__topline">
                    <span className="service-card__icon">
                      {application.name.slice(0, 1).toUpperCase()}
                    </span>
                    <StatusPill
                      value={appDeployments.length ? "active" : "pending"}
                      label={
                        appDeployments.length ? "Deployed" : "Not deployed"
                      }
                    />
                  </div>
                  <div>
                    <h2>{application.name}</h2>
                    <p>
                      {application.description ??
                        `${appDeployments.length} deployment${appDeployments.length === 1 ? "" : "s"}`}
                    </p>
                  </div>
                  <div className="service-card__footer">
                    <span>
                      {appDeployments.length} deployment
                      {appDeployments.length === 1 ? "" : "s"}
                    </span>
                    <span>
                      Open service <Icon name="arrow" />
                    </span>
                  </div>
                </Link>
              );
            })}
          </div>
        ) : (
          <EmptyState
            icon="deploy"
            title="Create your first service"
            description="Deploy an existing image, connect a GitHub repository, or publish a Helm application."
            action={
              <Link to="/deploy" className="button button--primary">
                <Icon name="plus" /> Create service
              </Link>
            }
          />
        )
      ) : null}

      {tab === "environments" ? (
        <div className="page-stack">
          {creatingEnvironment ? (
            <Card className="compact-form-card">
              <form
                className="inline-form"
                onSubmit={form.handleSubmit((value) =>
                  createEnvironment.mutate(value),
                )}
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
                    <div>
                      <strong>{environment.name}</strong>
                      <small>
                        {environment.protectionPolicy === "development"
                          ? "Direct Git publication"
                          : "Protected pull-request publication"}
                      </small>
                    </div>
                    <code>{environment.namespace}</code>
                    <StatusPill value={environment.status ?? "active"} />
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
                  </div>
                ))}
              </div>
            ) : (
              <EmptyState
                compact
                title="No environments"
                description="Create development, staging, or production before deploying a service."
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
              project={project}
              environments={projectEnvironments}
              applications={projectApplications}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("services")}
            />
          ) : null}
          {showAutomation ? (
            <ProjectAutomationPanel
              project={project}
              capabilities={effectiveCapabilities}
              onClose={() => setTab("services")}
            />
          ) : null}
          {!canManageAccess && !showAutomation ? (
            <EmptyState
              title="Project settings are read-only"
              description="Your current role does not manage access grants or service accounts for this project."
            />
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
