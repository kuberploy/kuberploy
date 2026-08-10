import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMemo, useRef } from "react";
import { useFieldArray, useForm, useWatch } from "react-hook-form";
import { ApiError, api, errorMessage } from "../api/client";
import type { SchedulingProfileRef } from "../api/types";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  PlaceholderBadge,
  StatusPill,
} from "../components/ui";
import { Icon } from "../components/Icon";
import {
  HealthProbeEditor,
  RuntimeProcessEditor,
} from "../components/GuidedConfigForm";
import { RuntimeSecretReferencePicker } from "../components/RuntimeSecretReferencePicker";
import { SchedulingProfilePicker } from "../components/SchedulingProfilePicker";
import {
  defaultGuidedProbes,
  validateGuidedProbes,
  validateGuidedRuntimeProcess,
  workloadProcessFromGuided,
  workloadProbesFromGuided,
  type GuidedProbes,
  type GuidedRuntimeProcess,
} from "../lib/configDraft";
import {
  isCanonicalImmutableImage,
  isCanonicalTaggedImage,
} from "../lib/imageReference";

type DeploymentForm = GuidedRuntimeProcess & {
  projectId: string;
  environmentId: string;
  applicationMode: "new" | "existing";
  applicationId: string;
  applicationName: string;
  image: string;
  replicas: number;
  port: number;
  cpuRequest: string;
  memoryRequest: string;
  cpuLimit: string;
  memoryLimit: string;
  schedulingProfile?: SchedulingProfileRef;
  probes: GuidedProbes;
  routeMode: "internal" | "manual" | "sslip";
  hostname: string;
  variables: Array<{ key: string; value: string }>;
  secretVariables: Array<{
    name: string;
    bindingId: string;
    bindingName: string;
    key: string;
    version: number;
  }>;
};

type StableApplicationReservation = { signature: string; key: string };

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function NewDeploymentPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const stableApplicationReservation =
    useRef<StableApplicationReservation | null>(null);
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
  });
  const applications = useQuery({
    queryKey: ["applications"],
    queryFn: api.applications,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const form = useForm<DeploymentForm>({
    defaultValues: {
      projectId: "",
      environmentId: "",
      applicationMode: "new",
      applicationId: "",
      applicationName: "",
      image: "",
      replicas: 1,
      commandYaml: "[]",
      argsYaml: "[]",
      terminationGracePeriodSeconds: undefined,
      port: 3000,
      cpuRequest: "50m",
      memoryRequest: "100Mi",
      cpuLimit: "",
      memoryLimit: "",
      probes: defaultGuidedProbes("http"),
      routeMode: "internal",
      hostname: "",
      variables: [],
      secretVariables: [],
    },
  });
  const variables = useFieldArray({ control: form.control, name: "variables" });
  const secretVariables = useFieldArray({
    control: form.control,
    name: "secretVariables",
  });
  const projectId = useWatch({ control: form.control, name: "projectId" });
  const applicationMode = useWatch({
    control: form.control,
    name: "applicationMode",
  });
  const applicationId = useWatch({
    control: form.control,
    name: "applicationId",
  });
  const environmentId = useWatch({
    control: form.control,
    name: "environmentId",
  });
  const image = useWatch({ control: form.control, name: "image" }) ?? "";
  const routeMode = useWatch({ control: form.control, name: "routeMode" });
  const secretVariableValues = useWatch({
    control: form.control,
    name: "secretVariables",
  });
  const probes = useWatch({ control: form.control, name: "probes" });
  const commandYaml = useWatch({ control: form.control, name: "commandYaml" });
  const argsYaml = useWatch({ control: form.control, name: "argsYaml" });
  const terminationGracePeriodSeconds = useWatch({
    control: form.control,
    name: "terminationGracePeriodSeconds",
  });
  const containerPort = useWatch({ control: form.control, name: "port" });
  const configuredPorts = [
    { name: "http", containerPort, protocol: "TCP" as const },
  ];
  const probeError = validateGuidedProbes(probes, configuredPorts);
  const processValues: GuidedRuntimeProcess = {
    commandYaml: commandYaml ?? "[]",
    argsYaml: argsYaml ?? "[]",
    terminationGracePeriodSeconds,
  };
  const processError = validateGuidedRuntimeProcess(processValues);
  const gitOpsReady =
    capabilities.data?.features?.git === true &&
    capabilities.data?.features?.argo === true;
  const sslipEnabled = capabilities.data?.features?.sslip === true;
  const imageTagResolutionEnabled =
    capabilities.data?.features?.imageTagResolution === true;
  const imageIsImmutable = isCanonicalImmutableImage(image);
  const imageIsTag = isCanonicalTaggedImage(image);
  const imageResolution = useMutation({
    mutationFn: (scope: {
      environmentId: string;
      applicationId: string;
      image: string;
    }) =>
      api.previewImageResolution(
        scope.environmentId,
        scope.applicationId,
        scope.image,
      ),
  });
  const imageResolutionIsCurrent = Boolean(
    imageIsTag &&
    imageResolution.data?.resolved === true &&
    imageResolution.data.requestedImage === image &&
    imageResolution.variables?.image === image &&
    imageResolution.variables?.applicationId === applicationId &&
    imageResolution.variables?.environmentId === environmentId,
  );
  const imageReady =
    imageIsImmutable || (imageTagResolutionEnabled && imageResolutionIsCurrent);
  const sslipScopeReady =
    applicationMode === "existing" && Boolean(applicationId && environmentId);
  const sslipHostname = useQuery({
    queryKey: ["application-sslip-hostname", applicationId, environmentId],
    queryFn: () => api.applicationSSLIPHostname(applicationId, environmentId),
    enabled: sslipEnabled && sslipScopeReady,
    retry: false,
  });
  const sslipRouteReady =
    routeMode !== "sslip" ||
    (sslipScopeReady && Boolean(sslipHostname.data?.hostname));

  const filteredEnvironments = useMemo(
    () =>
      environments.data?.items.filter(
        (environment) => environment.projectId === projectId,
      ) ?? [],
    [environments.data, projectId],
  );
  const filteredApplications = useMemo(
    () =>
      applications.data?.items.filter(
        (application) => application.projectId === projectId,
      ) ?? [],
    [applications.data, projectId],
  );

  const reserveApplication = useMutation({
    retry: retryNetworkOnce,
    mutationFn: async ({
      projectId,
      name,
    }: {
      projectId: string;
      name: string;
    }) => {
      const normalizedName = name.trim();
      if (!projectId || !normalizedName) {
        throw new Error(
          "Select a project and enter an application name first.",
        );
      }
      const signature = `${projectId}:${normalizedName}`;
      if (stableApplicationReservation.current?.signature !== signature) {
        stableApplicationReservation.current = {
          signature,
          key: crypto.randomUUID(),
        };
      }
      return api.createApplication(
        { projectId, name: normalizedName },
        stableApplicationReservation.current.key,
      );
    },
    onSuccess: async (application) => {
      await queryClient.invalidateQueries({ queryKey: ["applications"] });
      form.setValue("applicationId", application.id, { shouldValidate: true });
      form.setValue("applicationMode", "existing", { shouldValidate: true });
    },
  });

  const deploy = useMutation({
    mutationFn: async (values: DeploymentForm) => {
      if (!gitOpsReady) {
        throw new Error(
          "Protected Git and Argo CD must both report fresh readiness before a deployment can be committed.",
        );
      }
      if (
        !isCanonicalImmutableImage(values.image) &&
        (!isCanonicalTaggedImage(values.image) ||
          !imageTagResolutionEnabled ||
          !imageResolutionIsCurrent)
      ) {
        throw new Error(
          "A current server-resolved immutable digest preview is required for this image tag.",
        );
      }
      // Application identity is created explicitly before this mutation. That
      // makes every application-scoped preview available on a first release.
      const workloadProcess = workloadProcessFromGuided(values);
      const workloadProbes = workloadProbesFromGuided(values.probes, [
        { name: "http", containerPort: values.port, protocol: "TCP" },
      ]);
      if (
        values.applicationMode !== "existing" &&
        values.secretVariables.length
      ) {
        throw new Error(
          "Runtime-secret bindings belong to an existing application. Create the application before selecting a binding.",
        );
      }
      const secretEnvironment = values.secretVariables.map(
        ({ name, bindingId, bindingName, key, version }) => {
          if (
            !name.trim() ||
            !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
              bindingId,
            ) ||
            !bindingName ||
            !key ||
            !Number.isSafeInteger(version) ||
            version < 1
          ) {
            throw new Error(
              "Every secret environment reference must select a ready binding, its authorized key, and exact active version.",
            );
          }
          return {
            name: name.trim(),
            valueFrom: {
              secretBindingRef: {
                bindingId,
                name: bindingName,
                key,
                version,
              },
            },
          };
        },
      );
      if (
        values.routeMode === "sslip" &&
        (!sslipEnabled ||
          values.applicationMode !== "existing" ||
          !sslipHostname.data?.hostname)
      ) {
        throw new Error(
          "A fresh server-derived sslip.io hostname preview is required before deployment.",
        );
      }
      if (values.applicationMode !== "existing" || !values.applicationId) {
        throw new Error(
          "Create or select the application identity before committing a deployment.",
        );
      }
      const applicationId = values.applicationId;
      const env = [
        ...values.variables
          .filter(({ key }) => key.trim())
          .map(({ key, value }) => ({ name: key.trim(), value })),
        ...secretEnvironment,
      ];
      return api.createDeployment({
        environmentId: values.environmentId,
        applicationId,
        image: values.image,
        ...(isCanonicalTaggedImage(values.image) && imageResolution.data
          ? { expectedImmutableImage: imageResolution.data.immutableImage }
          : {}),
        runtime: {
          replicas: values.replicas,
          ...workloadProcess,
          ports: [
            {
              name: "http",
              containerPort: values.port,
              protocol: "TCP",
            },
          ],
          env,
          resources: {
            requests: {
              cpu: values.cpuRequest,
              memory: values.memoryRequest,
            },
            ...(values.cpuLimit || values.memoryLimit
              ? {
                  limits: {
                    ...(values.cpuLimit ? { cpu: values.cpuLimit } : {}),
                    ...(values.memoryLimit
                      ? { memory: values.memoryLimit }
                      : {}),
                  },
                }
              : {}),
          },
          ...(values.schedulingProfile
            ? { schedulingProfile: values.schedulingProfile }
            : {}),
          ...(workloadProbes ? { probes: workloadProbes } : {}),
        },
        route:
          values.routeMode === "sslip"
            ? {
                dnsMode: "sslip",
                pathPrefix: "/",
                tlsMode: "httpOnly",
              }
            : values.routeMode === "manual"
              ? {
                  hostname: values.hostname.trim(),
                  dnsMode: "manual",
                  pathPrefix: "/",
                  tlsMode: "httpOnly",
                }
              : undefined,
      });
    },
    onSuccess: async (operation) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["applications"] }),
        queryClient.invalidateQueries({ queryKey: ["deployments"] }),
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
      ]);
      await navigate({
        to: "/operations/$operationId",
        params: { operationId: operation.id },
      });
    },
  });

  const loadError = projects.error ?? environments.error ?? applications.error;
  const noScopes = !projects.isPending && !projects.data?.items.length;

  return (
    <div className="page page--narrow">
      <PageHeader
        eyebrow="Existing image"
        title="Create an immutable deployment"
        description="Use an exact OCI digest, or resolve an authorized existing-image tag before submission. Kuberploy commits only immutable desired state to Git."
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              projects.refetch(),
              environments.refetch(),
              applications.refetch(),
            ])
          }
        />
      ) : null}
      {noScopes ? (
        <EmptyState
          icon="layers"
          title="A project comes first"
          description="Create a project and an environment namespace before deploying an application."
          action={
            <Link to="/projects" className="button button--primary">
              Create workspace
            </Link>
          }
        />
      ) : (
        <form onSubmit={form.handleSubmit((values) => deploy.mutate(values))}>
          <Card className="form-card">
            <div className="form-card__heading">
              <span>01</span>
              <div>
                <h2>Placement</h2>
                <p>
                  Choose the project policy boundary and exact destination
                  environment.
                </p>
              </div>
            </div>
            <div className="form-grid">
              <Field
                label="Project"
                required
                error={form.formState.errors.projectId?.message}
              >
                <select
                  {...form.register("projectId", {
                    required: "Select a project.",
                    onChange: () => {
                      form.setValue("environmentId", "");
                      form.setValue("applicationId", "");
                      form.setValue("routeMode", "internal");
                    },
                  })}
                >
                  <option value="">Select project</option>
                  {projects.data?.items.map((project) => (
                    <option key={project.id} value={project.id}>
                      {project.name}
                    </option>
                  ))}
                </select>
              </Field>
              <Field
                label="Environment"
                required
                hint="Maps to one namespace and Argo CD project."
                error={form.formState.errors.environmentId?.message}
              >
                <select
                  disabled={!projectId}
                  {...form.register("environmentId", {
                    required: "Select an environment.",
                  })}
                >
                  <option value="">
                    {projectId
                      ? "Select environment"
                      : "Choose a project first"}
                  </option>
                  {filteredEnvironments.map((environment) => (
                    <option key={environment.id} value={environment.id}>
                      {environment.name} · {environment.namespace}
                    </option>
                  ))}
                </select>
              </Field>
            </div>
            <div className="form-grid form-grid--three">
              <Field
                label="CPU request"
                required
                hint="New services default to 50m."
              >
                <input
                  placeholder="50m"
                  {...form.register("cpuRequest", {
                    required: "Enter a CPU request.",
                  })}
                />
              </Field>
              <Field
                label="Memory request"
                required
                hint="New services default to 100Mi."
              >
                <input
                  placeholder="100Mi"
                  {...form.register("memoryRequest", {
                    required: "Enter a memory request.",
                  })}
                />
              </Field>
              <Field label="CPU limit" hint="Optional">
                <input placeholder="500m" {...form.register("cpuLimit")} />
              </Field>
              <Field label="Memory limit" hint="Optional">
                <input placeholder="512Mi" {...form.register("memoryLimit")} />
              </Field>
            </div>
          </Card>

          <Card className="form-card">
            <div className="form-card__heading">
              <span>02</span>
              <div>
                <h2>Application identity</h2>
                <p>
                  Create the durable logical identity before previewing image,
                  sslip.io, TLS, DNS, middleware, or secret configuration.
                </p>
              </div>
            </div>
            <div
              className="segmented-control"
              role="radiogroup"
              aria-label="Application identity mode"
            >
              <label>
                <input
                  type="radio"
                  value="new"
                  {...form.register("applicationMode", {
                    onChange: (event) => {
                      if (event.target.value === "new") {
                        form.setValue("routeMode", "internal");
                      }
                    },
                  })}
                />
                <span>New application</span>
              </label>
              <label>
                <input
                  type="radio"
                  value="existing"
                  {...form.register("applicationMode")}
                />
                <span>Existing application</span>
              </label>
            </div>
            {applicationMode === "new" ? (
              <div className="stack">
                <Field
                  label="Application name"
                  required
                  hint="Stable identity, independent of environment and release."
                  error={form.formState.errors.applicationName?.message}
                >
                  <input
                    placeholder="hello-api"
                    {...form.register("applicationName", {
                      required:
                        applicationMode === "new"
                          ? "Enter an application name."
                          : false,
                    })}
                  />
                </Field>
                <div>
                  <Button
                    type="button"
                    variant="secondary"
                    busy={reserveApplication.isPending}
                    onClick={() =>
                      reserveApplication.mutate({
                        projectId,
                        name: form.getValues("applicationName"),
                      })
                    }
                  >
                    Create application identity
                  </Button>
                  <p className="muted-copy">
                    This creates a recoverable application record, not a
                    workload. It remains available from Projects even if you
                    leave this deployment wizard.
                  </p>
                </div>
              </div>
            ) : (
              <Field
                label="Application"
                required
                error={form.formState.errors.applicationId?.message}
              >
                <select
                  {...form.register("applicationId", {
                    required:
                      applicationMode === "existing"
                        ? "Select an application."
                        : false,
                  })}
                >
                  <option value="">Select application</option>
                  {filteredApplications.map((application) => (
                    <option key={application.id} value={application.id}>
                      {application.name}
                    </option>
                  ))}
                </select>
              </Field>
            )}
            {reserveApplication.data ? (
              <div className="notice notice--success" role="status">
                <div>
                  <strong>Application identity created</strong>
                  <p>
                    Application-scoped previews are now enabled. You can deploy
                    here or configure another source from its application page.
                  </p>
                </div>
                <Link
                  to="/applications/$applicationId"
                  params={{ applicationId: reserveApplication.data.id }}
                  className="button button--secondary"
                >
                  Source options
                </Link>
              </div>
            ) : null}
            {reserveApplication.error ? (
              <ErrorPanel
                title="Application identity was not created"
                error={reserveApplication.error}
                onRetry={() =>
                  reserveApplication.mutate({
                    projectId,
                    name: form.getValues("applicationName"),
                  })
                }
              />
            ) : null}
          </Card>

          <Card className="form-card">
            <div className="form-card__heading form-card__heading--with-action">
              <span>03</span>
              <div>
                <h2>Secret environment references</h2>
                <p>
                  Select a scoped write-only binding and its exact active
                  version. This form never accepts or commits secret plaintext.
                </p>
              </div>
              <Button
                type="button"
                variant="secondary"
                disabled={
                  capabilities.data?.features?.secretBindings !== true ||
                  applicationMode !== "existing" ||
                  !applicationId ||
                  !environmentId
                }
                onClick={() =>
                  secretVariables.append({
                    name: "",
                    bindingId: "",
                    bindingName: "",
                    key: "",
                    version: 0,
                  })
                }
              >
                <Icon name="plus" /> Add reference
              </Button>
            </div>
            {secretVariables.fields.length ? (
              <div className="variable-list">
                {secretVariables.fields.map((field, index) => (
                  <div className="variable-row" key={field.id}>
                    <Field label={index === 0 ? "Environment name" : ""}>
                      <input
                        aria-label={`Secret variable ${index + 1} name`}
                        placeholder="DATABASE_PASSWORD"
                        {...form.register(`secretVariables.${index}.name`, {
                          onChange: () =>
                            form.setValue(`secretVariables.${index}.key`, ""),
                        })}
                      />
                    </Field>
                    <RuntimeSecretReferencePicker
                      index={index}
                      applicationId={
                        applicationMode === "existing"
                          ? applicationId
                          : undefined
                      }
                      environmentId={environmentId}
                      environmentName={
                        secretVariableValues?.[index]?.name ?? ""
                      }
                      value={{
                        bindingId:
                          secretVariableValues?.[index]?.bindingId ?? "",
                        bindingName:
                          secretVariableValues?.[index]?.bindingName ?? "",
                        key: secretVariableValues?.[index]?.key ?? "",
                        version: secretVariableValues?.[index]?.version ?? 0,
                      }}
                      enabled={
                        capabilities.data?.features?.secretBindings === true &&
                        applicationMode === "existing"
                      }
                      unavailableReason={
                        applicationMode !== "existing"
                          ? "Create the application first; runtime-secret bindings are scoped to an existing application and environment."
                          : "Runtime-secret references remain unavailable until the strict Sealed Secrets runtime is ready."
                      }
                      onChange={(reference) =>
                        form.setValue(`secretVariables.${index}`, {
                          name: form.getValues(`secretVariables.${index}.name`),
                          ...reference,
                        })
                      }
                    />
                    <button
                      type="button"
                      className="icon-button"
                      onClick={() => secretVariables.remove(index)}
                      aria-label={`Remove secret variable ${index + 1}`}
                    >
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <div className="inline-empty">No secret references.</div>
            )}
          </Card>

          <Card className="form-card">
            <div className="form-card__heading">
              <span>04</span>
              <div>
                <h2>Scheduling</h2>
                <p>
                  Select an administrator-managed immutable placement profile.
                  Kuberploy never mutates Nodes, taints, NodePools, or
                  NodeClasses.
                </p>
              </div>
            </div>
            <SchedulingProfilePicker
              environmentId={environmentId}
              value={form.watch("schedulingProfile")}
              enabled={capabilities.data?.features?.schedulingProfiles === true}
              onChange={(profile) =>
                form.setValue("schedulingProfile", profile, {
                  shouldDirty: true,
                })
              }
            />
          </Card>

          <Card className="form-card">
            <div className="form-card__heading">
              <span>05</span>
              <div>
                <h2>Artifact & runtime</h2>
                <p>
                  Exact digests submit directly. Authorized tags require a fresh
                  server-owned resolution preview for this application and
                  environment.
                </p>
              </div>
            </div>
            <Field
              label="Image digest or tag"
              required
              hint="Digest: registry.example.com/team/app@sha256:… · Tag: registry.example.com/team/app:release"
              error={form.formState.errors.image?.message}
            >
              <div className="input-with-icon">
                <Icon name="deploy" />
                <input
                  spellCheck={false}
                  autoCapitalize="none"
                  placeholder="ghcr.io/acme/hello:release"
                  {...form.register("image", {
                    required: "Enter an image digest or authorized tag.",
                    validate: (value) => {
                      if (isCanonicalImmutableImage(value)) return true;
                      if (isCanonicalTaggedImage(value)) return true;
                      return "Use a canonical registry/repository tag or a complete 64-character sha256 digest.";
                    },
                  })}
                />
              </div>
            </Field>
            {imageIsTag ? (
              <div className="notice notice--warning">
                <strong>Resolve this tag before deployment</strong>
                {applicationMode !== "existing" ? (
                  <p>
                    Tag resolution requires an existing application so the
                    server can enforce its exact registry policy. Create the
                    application first, or use an immutable digest.
                  </p>
                ) : !imageTagResolutionEnabled ? (
                  <p>
                    Image tag resolution is unavailable until an authorized
                    registry target and resolver runtime are ready.
                  </p>
                ) : (
                  <p>
                    The server selects the authorized registry target and
                    operator credential. This browser sends no registry or
                    authentication coordinates.
                  </p>
                )}
                <Button
                  type="button"
                  variant="secondary"
                  busy={imageResolution.isPending}
                  disabled={
                    !imageTagResolutionEnabled ||
                    applicationMode !== "existing" ||
                    !applicationId ||
                    !environmentId ||
                    imageResolution.isPending
                  }
                  onClick={() =>
                    imageResolution.mutate({
                      environmentId,
                      applicationId,
                      image,
                    })
                  }
                >
                  Resolve tag to digest
                </Button>
              </div>
            ) : null}
            {imageResolutionIsCurrent && imageResolution.data ? (
              <div className="notice notice--success" role="status">
                <strong>Immutable image resolved</strong>
                <p>
                  <code>{imageResolution.data.requestedImage}</code>
                  {" → "}
                  <code>{imageResolution.data.immutableImage}</code>
                </p>
              </div>
            ) : null}
            {imageIsTag && imageResolution.error ? (
              <div className="notice notice--error" role="alert">
                <strong>Tag could not be resolved</strong>
                <p>{errorMessage(imageResolution.error)}</p>
              </div>
            ) : null}
            <div className="form-grid">
              <Field
                label="Replicas"
                required
                error={form.formState.errors.replicas?.message}
              >
                <input
                  type="number"
                  min={1}
                  max={20}
                  {...form.register("replicas", {
                    required: true,
                    valueAsNumber: true,
                    min: {
                      value: 1,
                      message: "At least one replica is required.",
                    },
                    max: {
                      value: 20,
                      message: "The MVP limit is 20 replicas.",
                    },
                  })}
                />
              </Field>
              <Field
                label="Container port"
                required
                hint="The HTTP Service targets this port."
                error={form.formState.errors.port?.message}
              >
                <input
                  type="number"
                  min={1}
                  max={65535}
                  {...form.register("port", {
                    required: true,
                    valueAsNumber: true,
                    min: { value: 1, message: "Use a valid TCP port." },
                    max: { value: 65535, message: "Use a valid TCP port." },
                  })}
                />
              </Field>
            </div>
            <RuntimeProcessEditor
              value={processValues}
              onChange={(value) => {
                form.setValue("commandYaml", value.commandYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue("argsYaml", value.argsYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue(
                  "terminationGracePeriodSeconds",
                  value.terminationGracePeriodSeconds,
                  { shouldDirty: true, shouldTouch: true },
                );
              }}
            />
          </Card>

          <Card className="form-card">
            <div className="form-card__heading">
              <span>06</span>
              <div>
                <h2>Health checks</h2>
                <p>
                  Add startup, readiness, or liveness checks. All are optional;
                  Kubernetes defaults apply to empty timing fields.
                </p>
              </div>
            </div>
            <HealthProbeEditor
              value={probes}
              configuredPorts={configuredPorts}
              onChange={(value) =>
                form.setValue("probes", value, {
                  shouldDirty: true,
                  shouldTouch: true,
                })
              }
            />
          </Card>

          <Card className="form-card">
            <div className="form-card__heading form-card__heading--with-action">
              <span>07</span>
              <div>
                <h2>Ordinary environment values</h2>
                <p>
                  Visible in Git and rendered through an immutable ConfigMap.
                  Never place secrets here.
                </p>
              </div>
              <Button
                type="button"
                variant="secondary"
                onClick={() => variables.append({ key: "", value: "" })}
              >
                <Icon name="plus" /> Add value
              </Button>
            </div>
            {variables.fields.length ? (
              <div className="variable-list">
                {variables.fields.map((field, index) => (
                  <div className="variable-row" key={field.id}>
                    <Field label={index === 0 ? "Name" : ""}>
                      <input
                        aria-label={`Variable ${index + 1} name`}
                        placeholder="LOG_LEVEL"
                        spellCheck={false}
                        {...form.register(`variables.${index}.key`)}
                      />
                    </Field>
                    <Field label={index === 0 ? "Value" : ""}>
                      <input
                        aria-label={`Variable ${index + 1} value`}
                        placeholder="info"
                        {...form.register(`variables.${index}.value`)}
                      />
                    </Field>
                    <button
                      type="button"
                      className="icon-button"
                      onClick={() => variables.remove(index)}
                      aria-label={`Remove variable ${index + 1}`}
                    >
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <div className="inline-empty">
                No ordinary values. Secret bindings are added later through the
                write-only configuration flow.
              </div>
            )}
          </Card>

          <Card className="form-card">
            <div className="form-card__heading">
              <span>08</span>
              <div>
                <h2>Initial internet route</h2>
                <p>
                  Optional HTTP-only exposure through Traefik. TLS, DNS
                  automation, and middleware use the previewed configuration
                  flow after creation.
                </p>
              </div>
            </div>
            <div
              className="segmented-control"
              role="radiogroup"
              aria-label="Initial route mode"
            >
              <label>
                <input
                  type="radio"
                  value="internal"
                  {...form.register("routeMode")}
                />
                <span>Internal only</span>
              </label>
              <label>
                <input
                  type="radio"
                  value="manual"
                  {...form.register("routeMode")}
                />
                <span>Manual hostname</span>
              </label>
              <label>
                <input
                  type="radio"
                  value="sslip"
                  disabled={
                    !sslipEnabled ||
                    !sslipScopeReady ||
                    sslipHostname.isPending ||
                    Boolean(sslipHostname.error) ||
                    !sslipHostname.data?.hostname
                  }
                  {...form.register("routeMode")}
                />
                <span>Free sslip.io hostname</span>
              </label>
            </div>
            {routeMode === "manual" ? (
              <Field
                label="Hostname"
                hint="Kuberploy will use this exact hostname without managing DNS."
                error={form.formState.errors.hostname?.message}
              >
                <input
                  autoCapitalize="none"
                  spellCheck={false}
                  placeholder="hello.example.com"
                  {...form.register("hostname", {
                    required:
                      routeMode === "manual" ? "Enter a hostname." : false,
                    pattern: {
                      value:
                        /^(?=.{1,253}$)(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$/,
                      message: "Enter a valid DNS hostname.",
                    },
                  })}
                />
              </Field>
            ) : null}
            {routeMode === "sslip" ? (
              <Field
                label="sslip.io hostname"
                hint="Read-only and derived from the fresh public Traefik endpoint. The deployment request sends no hostname or IP."
              >
                <input
                  aria-label="sslip.io hostname"
                  value={sslipHostname.data?.hostname ?? ""}
                  readOnly
                  aria-readonly="true"
                />
              </Field>
            ) : null}
            {!sslipEnabled ? (
              <small>
                sslip.io remains unavailable until the platform observes a fresh
                public Traefik endpoint.
              </small>
            ) : applicationMode !== "existing" ? (
              <small>
                Select an existing application to preview its exact sslip.io
                hostname before deployment.
              </small>
            ) : sslipHostname.error ? (
              <small>{errorMessage(sslipHostname.error)}</small>
            ) : null}
            <div className="route-mode-row">
              <div>
                <StatusPill value="active" label="HTTP only · /" />
                <small>Supported during initial deployment</small>
              </div>
              <div>
                <PlaceholderBadge>
                  Let&apos;s Encrypt via config preview
                </PlaceholderBadge>
                <PlaceholderBadge>
                  Custom TLS via config preview
                </PlaceholderBadge>
                <PlaceholderBadge>
                  external-dns via config preview
                </PlaceholderBadge>
              </div>
            </div>
          </Card>

          <Card className="form-card form-card--muted">
            <div className="route-teaser">
              <span className="route-teaser__icon">
                <Icon name="route" />
              </span>
              <div>
                <h3>Advanced exposure stays preview-first</h3>
                <p>
                  After the deployment exists, upgrade the initial HTTP route
                  with TLS, DNS automation, and middleware in its shared Form /
                  Advanced YAML editor.
                </p>
              </div>
              <span className="placeholder-badge">Next step</span>
            </div>
          </Card>

          {deploy.error ? (
            <div className="notice notice--error" role="alert">
              <strong>Deployment was not accepted</strong>
              <p>{errorMessage(deploy.error)}</p>
            </div>
          ) : null}
          {!capabilities.isPending && !gitOpsReady ? (
            <div className="notice notice--warning" role="status">
              <strong>Protected GitOps is not ready</strong>
              <p>
                Deployment remains disabled until both the exact Git projection
                worker and protected Argo desired-state runtime are healthy.
              </p>
            </div>
          ) : null}
          <div className="form-actions">
            <Link to="/" className="button button--ghost">
              Cancel
            </Link>
            <Button
              type="submit"
              busy={deploy.isPending}
              disabled={Boolean(
                applicationMode !== "existing" ||
                !applicationId ||
                probeError ||
                processError ||
                !sslipRouteReady ||
                !gitOpsReady ||
                !imageReady,
              )}
            >
              Commit & deploy <Icon name="arrow" />
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}
