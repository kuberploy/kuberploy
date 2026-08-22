import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Capabilities, Project } from "../api/types";
import { Icon, type IconName } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
} from "../components/ui";

export type AppSourceKind = "oci" | "github" | "git-ssh" | "helm";

function sourceAvailable(source: AppSourceKind, capabilities?: Capabilities) {
  switch (source) {
    case "oci":
      return true;
    case "github":
      return (
        capabilities?.features?.builds === true &&
        capabilities.features.builder === true
      );
    case "git-ssh":
      return capabilities?.features?.gitSSHBuilds === true;
    case "helm":
      return capabilities?.features?.helmDeployments === true;
  }
}

const sources: Array<{
  id: AppSourceKind;
  title: string;
  description: string;
  detail: string;
  icon: IconName;
}> = [
  {
    id: "oci",
    title: "OCI image",
    description:
      "Run an existing image from a public or authenticated registry.",
    detail: "Resolve tags to immutable digests before GitOps publication.",
    icon: "deploy",
  },
  {
    id: "github",
    title: "GitHub App",
    description: "Build a Dockerfile from an installed GitHub repository.",
    detail: "Supports verified webhooks and automatic deploy triggers.",
    icon: "git",
  },
  {
    id: "git-ssh",
    title: "Git SSH",
    description: "Clone from any Git provider with a generated deploy key.",
    detail: "Manual or API-triggered builds; App or Project key scope.",
    icon: "terminal",
  },
  {
    id: "helm",
    title: "Helm chart",
    description: "Publish a chart release through protected desired state.",
    detail: "Supports OCI, Helm repositories, public Git, raw values YAML, and rollback.",
    icon: "layers",
  },
];

type AppIdentityForm = { name: string };

function canCreateApp(
  capabilities: Capabilities | undefined,
  project: Project,
  environmentId: string,
) {
  const grants = capabilities?.capabilities ?? [];
  const canCreateIdentity = grants.some(
    (capability) =>
      capability.actions?.includes("applications:create") &&
      ((capability.scopeType === "platform" &&
        capability.scopeId === "platform") ||
        (capability.scopeType === "team" &&
          capability.scopeId === project.teamId) ||
        (capability.scopeType === "project" &&
          capability.scopeId === project.id)),
  );
  const canCreateDeployment = grants.some(
    (capability) =>
      capability.actions?.includes("deployments:create") &&
      ((capability.scopeType === "platform" &&
        capability.scopeId === "platform") ||
        (capability.scopeType === "team" &&
          capability.scopeId === project.teamId) ||
        (capability.scopeType === "project" &&
          capability.scopeId === project.id) ||
        (capability.scopeType === "environment" &&
          capability.scopeId === environmentId)),
  );
  return canCreateIdentity && canCreateDeployment;
}

export function AddAppPage() {
  const { projectId, environmentId } = useParams({
    from: "/projects/$projectId/environments/$environmentId/apps/new",
  });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [source, setSource] = useState<AppSourceKind | null>(null);
  const stableAttempt = useRef<{ signature: string; key: string } | null>(null);
  const form = useForm<AppIdentityForm>({ defaultValues: { name: "" } });
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () =>
      api
        .projects()
        .then((items) => items.items.find((item) => item.id === projectId)),
  });
  const environment = useQuery({
    queryKey: ["environment", environmentId],
    queryFn: () => api.environment(environmentId),
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const createApp = useMutation({
    mutationFn: ({
      name,
      idempotencyKey,
    }: {
      name: string;
      idempotencyKey: string;
    }) =>
      api.createApplication({ projectId, environmentId, name }, idempotencyKey),
    onSuccess: async (application, input) => {
      if (stableAttempt.current?.key === input.idempotencyKey) {
        stableAttempt.current = null;
      }
      await queryClient.invalidateQueries({ queryKey: ["applications"] });
      if (!source) return;
      if (source === "oci") {
        await navigate({
          to: "/deploy",
          search: { projectId, environmentId, applicationId: application.id },
        });
        return;
      }
      await navigate({
        to: "/projects/$projectId/environments/$environmentId/apps/$applicationId",
        params: { projectId, environmentId, applicationId: application.id },
        search: {
          tab: "source",
          source,
          environmentId,
        },
      });
    },
  });
  const submit = ({ name }: AppIdentityForm) => {
    if (!source) return;
    const normalizedName = name.trim();
    const signature = `${projectId}:${environmentId}:${source}:${normalizedName}`;
    const idempotencyKey =
      stableAttempt.current?.signature === signature
        ? stableAttempt.current.key
        : crypto.randomUUID();
    stableAttempt.current = { signature, key: idempotencyKey };
    createApp.mutate({ name: normalizedName, idempotencyKey });
  };

  const loadError = project.error ?? environment.error ?? capabilities.error;
  if (loadError) {
    return <ErrorPanel error={loadError} onRetry={() => location.reload()} />;
  }
  if (project.isPending || environment.isPending || capabilities.isPending) {
    return <Skeleton lines={8} />;
  }
  if (
    !project.data ||
    !environment.data ||
    environment.data.projectId !== project.data.id
  ) {
    return (
      <EmptyState
        title="Environment unavailable"
        description="The Project and Environment scope is no longer readable."
        action={
          <Link to="/projects" className="button button--secondary">
            Back to Projects
          </Link>
        }
      />
    );
  }
  if (!canCreateApp(capabilities.data, project.data, environmentId)) {
    return (
      <EmptyState
        title="Add App is unavailable"
        description="Your current role cannot create both an App identity and its environment deployment."
        action={
          <Link
            to="/projects/$projectId/environments/$environmentId"
            params={{ projectId, environmentId }}
            className="button button--secondary"
          >
            Back to Environment
          </Link>
        }
      />
    );
  }

  return (
    <div className="page page--narrow">
      <nav className="backline" aria-label="Breadcrumb">
        <Link to="/projects/$projectId" params={{ projectId }}>
          <Icon name="arrow" /> {project.data.name}
        </Link>
        <span>/</span>
        <Link
          to="/projects/$projectId/environments/$environmentId"
          params={{ projectId, environmentId }}
        >
          {environment.data.name}
        </Link>
        <span>/</span>
        <span aria-current="page">Add App</span>
      </nav>
      <PageHeader
        eyebrow={`${project.data.name} · ${environment.data.name}`}
        title="Add App"
        description="Choose one source. Kuberploy creates the App in this Environment, then opens only the controls needed for that source."
      />

      <div
        className="app-source-grid"
        role="radiogroup"
        aria-label="App source"
      >
        {sources.map((candidate) => {
          const available = sourceAvailable(candidate.id, capabilities.data);
          return (
            <button
              key={candidate.id}
              type="button"
              role="radio"
              aria-checked={source === candidate.id}
              aria-disabled={!available}
              disabled={!available}
              className="app-source-option"
              onClick={() => setSource(candidate.id)}
            >
              <span className="app-source-option__icon">
                <Icon name={candidate.icon} />
              </span>
              <span>
                <strong>{candidate.title}</strong>
                <small>{candidate.description}</small>
                <em>
                  {available
                    ? candidate.detail
                    : "Unavailable in this installation."}
                </em>
              </span>
              <Icon name="chevron" />
            </button>
          );
        })}
      </div>

      {source ? (
        <Card className="form-card add-app-identity-card">
          <div className="form-card__heading">
            <span>02</span>
            <div>
              <h2>Name this App</h2>
              <p>
                The identity is durable. No workload starts until the selected
                source setup succeeds.
              </p>
            </div>
          </div>
          <form className="inline-form" onSubmit={form.handleSubmit(submit)}>
            <Field
              label="App name"
              required
              error={form.formState.errors.name?.message}
            >
              <input
                autoFocus
                placeholder="payments-api"
                {...form.register("name", {
                  required: "Enter an App name.",
                  validate: (value) =>
                    value.trim().length <= 100 ||
                    "Use no more than 100 characters.",
                })}
              />
            </Field>
            <Button type="submit" busy={createApp.isPending}>
              Continue with {sources.find((item) => item.id === source)?.title}
            </Button>
          </form>
          {createApp.error ? (
            <div className="form-error" role="alert">
              {errorMessage(createApp.error)}
            </div>
          ) : null}
        </Card>
      ) : null}
    </div>
  );
}
