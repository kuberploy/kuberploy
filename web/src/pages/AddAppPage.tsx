import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import { Icon, type IconName } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  FormCard,
  FormCardHeading,
  Page,
  PageHeader,
  Skeleton,
  buttonVariants,
} from "../components/ui";
import {
  canCreateAppInEnvironment,
  canUseAppSource,
  type AppSourceKind,
} from "../lib/appCreationAccess";

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
    detail:
      "No automatic provider webhooks by default; trigger builds manually or by API with an App or Project key.",
    icon: "terminal",
  },
  {
    id: "helm",
    title: "Helm chart",
    description: "Publish a chart release through protected desired state.",
    detail:
      "Supports OCI, Helm repositories, public Git, raw values YAML, and rollback.",
    icon: "layers",
  },
];

type AppIdentityForm = { name: string };

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
      api.createApplication(
        { projectId, environmentId, name, sourceKind: source! },
        idempotencyKey,
      ),
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
    return (
      <Page narrow>
        <Skeleton lines={8} />
      </Page>
    );
  }
  if (
    !project.data ||
    !environment.data ||
    environment.data.projectId !== project.data.id
  ) {
    return (
      <Page narrow>
        <EmptyState
          title="Environment unavailable"
          description="The Project and Environment scope is no longer readable."
          action={
            <Link
              to="/projects"
              className={buttonVariants({ variant: "secondary" })}
            >
              Back to Projects
            </Link>
          }
        />
      </Page>
    );
  }
  if (
    !canCreateAppInEnvironment(
      capabilities.data,
      project.data,
      environment.data,
    )
  ) {
    return (
      <Page narrow>
        <EmptyState
          title="Add App is unavailable"
          description="Your current role cannot create an App identity with any available source in this Environment."
          action={
            <Link
              to="/projects/$projectId/environments/$environmentId"
              params={{ projectId, environmentId }}
              className={buttonVariants({ variant: "secondary" })}
            >
              Back to Environment
            </Link>
          }
        />
      </Page>
    );
  }
  const currentProject = project.data;
  const currentEnvironment = environment.data;

  return (
    <Page narrow>
      <nav
        className="flex items-center gap-2 mb-5 text-ink-faint text-meta [&_a]:inline-flex [&_a]:items-center [&_a]:gap-1.5 [&_a]:text-mint-dark [&_a_svg]:w-3 [&_a_svg]:transform-[rotate(180deg)] pointer-coarse:[&_a]:inline-flex pointer-coarse:[&_a]:min-h-8 pointer-coarse:[&_a]:items-center"
        aria-label="Breadcrumb"
      >
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
        className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-3 mb-5 to-760:grid-cols-[1fr]"
        role="radiogroup"
        aria-label="App source"
      >
        {sources.map((candidate) => {
          const available = canUseAppSource(
            candidate.id,
            capabilities.data,
            currentProject,
            currentEnvironment,
          );
          return (
            <button
              key={candidate.id}
              type="button"
              role="radio"
              aria-checked={source === candidate.id}
              aria-disabled={!available}
              disabled={!available}
              className="grid grid-cols-[40px_minmax(0,_1fr)_18px] items-start gap-4 min-h-[132px] p-5 border border-line rounded-[10px] text-ink text-left bg-surface cursor-pointer transition-[border-color,background] duration-(--motion-fast) ease-(--ease-standard) hover:border-line-strong hover:bg-surface-soft [&_[aria-checked='true']]:border-mint [&_[aria-checked='true']]:bg-mint-soft [&_[aria-checked='true']]:shadow-[inset_0_0_0_1px_var(--mint)] [&>span:nth-child(2)]:grid [&>span:nth-child(2)]:gap-1.5 [&_strong]:text-sm [&_small]:text-ink-soft [&_small]:text-xs [&_small]:not-italic [&_small]:leading-[1.45] [&_em]:text-xs [&_em]:not-italic [&_em]:leading-[1.45] [&_em]:text-ink-faint [&>svg]:w-[18px] [&>svg]:self-center [&>svg]:text-ink-faint"
              onClick={() => setSource(candidate.id)}
            >
              <span className="grid w-[38px] h-[38px] place-items-center border border-line rounded-lg text-mint-dark bg-surface-soft [&_svg]:w-[18px]">
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
        <FormCard className="mt-1">
          <FormCardHeading step="02">
            <div>
              <h2>Name this App</h2>
              <p>
                The identity is durable. No workload starts until the selected
                source setup succeeds.
              </p>
            </div>
          </FormCardHeading>
          <form
            className="grid grid-cols-[1fr_1fr_auto] items-end gap-3 to-580:grid-cols-[1fr]"
            onSubmit={form.handleSubmit(submit)}
          >
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
            <div className="col-[1_/_-1] text-tone-bad text-meta" role="alert">
              {errorMessage(createApp.error)}
            </div>
          ) : null}
        </FormCard>
      ) : null}
    </Page>
  );
}
