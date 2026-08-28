import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import { useFieldArray, useForm, useWatch } from "react-hook-form";
import { ApiError, api, errorMessage } from "../api/client";
import {
  Select,
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  FormActions,
  FormCard,
  FormCardHeading,
  FormGrid,
  MutedCopy,
  Notice,
  Page,
  PageHeader,
  PlaceholderBadge,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import { Icon } from "../components/Icon";
import {
  HealthProbeEditor,
  RuntimeProcessEditor,
} from "../components/GuidedConfigForm";
import {
  SchedulingEditor,
  type SchedulingEditorValue,
} from "../components/SchedulingEditor";
import { RuntimeSecretReferencePicker } from "../components/RuntimeSecretReferencePicker";
import {
  defaultGuidedProbes,
  validateGuidedProbes,
  validateGuidedRuntimeProcess,
  workloadProcessFromGuided,
  workloadProbesFromGuided,
  workloadSchedulingFromGuided,
  type GuidedProbes,
  type GuidedRuntimeProcess,
} from "../lib/configDraft";
import {
  isCanonicalImmutableImage,
  isCanonicalTaggedImage,
} from "../lib/imageReference";

type DeploymentForm = GuidedRuntimeProcess &
  SchedulingEditorValue & {
    projectId: string;
    environmentId: string;
    applicationMode: "new" | "existing";
    applicationId: string;
    applicationName: string;
    image: string;
    replicas: number;
    workloadType: "Deployment" | "StatefulSet";
    strategyType: "RollingUpdate" | "Recreate" | "OnDelete";
    podManagementPolicy: "OrderedReady" | "Parallel";
    port: number;
    cpuRequest: string;
    memoryRequest: string;
    cpuLimit: string;
    memoryLimit: string;
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
type StableDeploymentAttempt = { signature: string; key: string };

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function NewDeploymentPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/deploy" });
  const queryClient = useQueryClient();
  const stableApplicationReservation =
    useRef<StableApplicationReservation | null>(null);
  const stableDeploymentAttempt = useRef<StableDeploymentAttempt | null>(null);
  const initialScopeApplied = useRef(false);
  const [reservedApplicationId, setReservedApplicationId] = useState("");
  const lastDeploymentProject = useRef("");
  const lastDeploymentScope = useRef("");
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
      workloadType: "Deployment",
      strategyType: "RollingUpdate",
      podManagementPolicy: "OrderedReady",
      commandYaml: "[]",
      argsYaml: "[]",
      workingDirectory: "",
      terminationGracePeriodSeconds: undefined,
      port: 3000,
      cpuRequest: "50m",
      memoryRequest: "100Mi",
      cpuLimit: "",
      memoryLimit: "",
      probes: defaultGuidedProbes("http"),
      nodeSelectorYaml: "{}",
      affinityYaml: "{}",
      topologySpreadYaml: "[]",
      tolerationsYaml: "[]",
      priorityClassName: "",
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
  const activeProjectId = useRef(projectId);
  activeProjectId.current = projectId;
  const applicationMode = useWatch({
    control: form.control,
    name: "applicationMode",
  });
  const activeApplicationMode = useRef(applicationMode);
  activeApplicationMode.current = applicationMode;
  const applicationName = useWatch({
    control: form.control,
    name: "applicationName",
  });
  const activeApplicationName = useRef(applicationName);
  activeApplicationName.current = applicationName;
  const applicationId = useWatch({
    control: form.control,
    name: "applicationId",
  });
  const workloadType = useWatch({
    control: form.control,
    name: "workloadType",
  });
  const scheduling = useWatch({
    control: form.control,
    name: [
      "nodeSelectorYaml",
      "affinityYaml",
      "topologySpreadYaml",
      "tolerationsYaml",
      "priorityClassName",
    ],
  });
  const environmentId = useWatch({
    control: form.control,
    name: "environmentId",
  });
  const existingDeploymentScope =
    applicationMode === "existing" && applicationId && environmentId
      ? `${applicationId}:${environmentId}`
      : "";
  const existingDeployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
    enabled: Boolean(existingDeploymentScope),
    retry: false,
  });
  const environmentGitBinding = useQuery({
    queryKey: ["environment-git-binding", environmentId],
    queryFn: () => api.environmentGitBinding(environmentId),
    enabled: Boolean(environmentId),
    retry: false,
  });
  const environmentGitBindingMissing =
    environmentGitBinding.error instanceof ApiError &&
    environmentGitBinding.error.status === 404;
  const environmentGitReady =
    !environmentId || environmentGitBinding.data?.state === "ready";
  const existingDeployment = useMemo(() => {
    if (!existingDeploymentScope) return undefined;
    return existingDeployments.data?.items
      .filter(
        (deployment) =>
          `${deployment.applicationId}:${deployment.environmentId}` ===
          existingDeploymentScope,
      )
      .sort((a, b) =>
        (b.updatedAt ?? b.createdAt ?? "").localeCompare(
          a.updatedAt ?? a.createdAt ?? "",
        ),
      )[0];
  }, [existingDeploymentScope, existingDeployments.data]);
  const existingGitBundle = useQuery({
    queryKey: ["deployment-config", existingDeployment?.id],
    queryFn: () => api.deploymentConfig(existingDeployment!.id),
    enabled: Boolean(existingDeployment?.id),
    retry: false,
  });
  useEffect(() => {
    if (lastDeploymentProject.current !== projectId) {
      if (lastDeploymentProject.current) {
        form.setValue("environmentId", "", { shouldDirty: true });
        form.setValue("applicationId", "", { shouldDirty: true });
        form.setValue("routeMode", "internal", { shouldDirty: true });
        form.setValue("hostname", "", { shouldDirty: true });
        form.setValue("nodeSelectorYaml", "{}", { shouldDirty: true });
        form.setValue("affinityYaml", "{}", { shouldDirty: true });
        form.setValue("topologySpreadYaml", "[]", { shouldDirty: true });
        form.setValue("tolerationsYaml", "[]", { shouldDirty: true });
        form.setValue("priorityClassName", "", { shouldDirty: true });
        variables.replace([]);
        secretVariables.replace([]);
        lastDeploymentScope.current = "";
        setReservedApplicationId("");
      }
      lastDeploymentProject.current = projectId;
    }
  }, [form, projectId, secretVariables, variables]);
  useEffect(() => {
    const scope =
      applicationId && environmentId ? `${applicationId}:${environmentId}` : "";
    if (
      scope &&
      lastDeploymentScope.current &&
      lastDeploymentScope.current !== scope
    ) {
      form.setValue("nodeSelectorYaml", "{}", { shouldDirty: true });
      form.setValue("affinityYaml", "{}", { shouldDirty: true });
      form.setValue("topologySpreadYaml", "[]", { shouldDirty: true });
      form.setValue("tolerationsYaml", "[]", { shouldDirty: true });
      form.setValue("priorityClassName", "", { shouldDirty: true });
      form.setValue("routeMode", "internal", { shouldDirty: true });
      form.setValue("hostname", "", { shouldDirty: true });
      variables.replace([]);
      secretVariables.replace([]);
    }
    if (scope) lastDeploymentScope.current = scope;
  }, [applicationId, environmentId, form, secretVariables, variables]);
  const image = useWatch({ control: form.control, name: "image" }) ?? "";
  const routeMode = useWatch({ control: form.control, name: "routeMode" });
  const secretVariableValues = useWatch({
    control: form.control,
    name: "secretVariables",
  });
  const probes = useWatch({ control: form.control, name: "probes" });
  const commandYaml = useWatch({ control: form.control, name: "commandYaml" });
  const argsYaml = useWatch({ control: form.control, name: "argsYaml" });
  const workingDirectory = useWatch({
    control: form.control,
    name: "workingDirectory",
  });
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
    workingDirectory: workingDirectory ?? "",
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
  const resetImageResolution = imageResolution.reset;
  useEffect(() => {
    // A tag preview is authority for one exact scope and image only. Do not
    // retain it if the operator changes scope and later returns to the same
    // values; the provider may have moved the tag in the meantime.
    resetImageResolution();
  }, [applicationId, environmentId, image, resetImageResolution]);
  const imageResolutionIsCurrent = Boolean(
    imageIsTag &&
    imageResolution.data?.resolved === true &&
    imageResolution.data.requestedImage === image &&
    imageResolution.variables?.image === image &&
    imageResolution.variables?.applicationId === applicationId &&
    imageResolution.variables?.environmentId === environmentId,
  );
  const imageResolutionErrorIsCurrent = Boolean(
    imageIsTag &&
    applicationMode === "existing" &&
    imageResolution.error &&
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

  useEffect(() => {
    if (
      initialScopeApplied.current ||
      !projects.isSuccess ||
      !environments.isSuccess ||
      (Boolean(search.applicationId) &&
        (!applications.isSuccess || applications.isFetching))
    ) {
      return;
    }
    if (!search.projectId || !search.environmentId) {
      initialScopeApplied.current = true;
      return;
    }
    const projectExists = projects.data.items.some(
      (project) => project.id === search.projectId,
    );
    const environmentMatches = environments.data.items.some(
      (environment) =>
        environment.id === search.environmentId &&
        environment.projectId === search.projectId,
    );
    if (!projectExists || !environmentMatches) {
      initialScopeApplied.current = true;
      return;
    }
    if (projectId !== search.projectId) {
      form.setValue("projectId", search.projectId);
      return;
    }
    if (
      !filteredEnvironments.some(
        (environment) => environment.id === search.environmentId,
      )
    ) {
      return;
    }
    form.setValue("environmentId", search.environmentId);
    if (search.applicationId) {
      const applicationMatches = applications.data?.items.some(
        (application) =>
          application.id === search.applicationId &&
          application.projectId === search.projectId,
      );
      if (!applicationMatches) {
        initialScopeApplied.current = true;
        return;
      }
      form.setValue("applicationMode", "existing");
      form.setValue("applicationId", search.applicationId);
      setReservedApplicationId(search.applicationId);
    }
    initialScopeApplied.current = true;
  }, [
    applications.data,
    applications.isFetching,
    applications.isSuccess,
    environments.data,
    environments.isSuccess,
    filteredEnvironments,
    form,
    projectId,
    projects.data,
    projects.isSuccess,
    search.environmentId,
    search.applicationId,
    search.projectId,
  ]);

  useEffect(() => {
    if (
      projectId &&
      projects.isSuccess &&
      !projects.data.items.some((project) => project.id === projectId)
    ) {
      form.setValue("projectId", "", { shouldDirty: true });
      form.setValue("environmentId", "", { shouldDirty: true });
      form.setValue("applicationId", "", { shouldDirty: true });
      form.setValue("routeMode", "internal", { shouldDirty: true });
      form.setValue("hostname", "", { shouldDirty: true });
      form.setValue("nodeSelectorYaml", "{}", { shouldDirty: true });
      form.setValue("affinityYaml", "{}", { shouldDirty: true });
      form.setValue("topologySpreadYaml", "[]", { shouldDirty: true });
      form.setValue("tolerationsYaml", "[]", { shouldDirty: true });
      form.setValue("priorityClassName", "", { shouldDirty: true });
      variables.replace([]);
      secretVariables.replace([]);
      lastDeploymentScope.current = "";
      setReservedApplicationId("");
      return;
    }
    if (
      environmentId &&
      environments.isSuccess &&
      !filteredEnvironments.some(
        (environment) => environment.id === environmentId,
      )
    ) {
      form.setValue("environmentId", "", { shouldDirty: true });
      form.setValue("routeMode", "internal", { shouldDirty: true });
      form.setValue("hostname", "", { shouldDirty: true });
      form.setValue("nodeSelectorYaml", "{}", { shouldDirty: true });
      form.setValue("affinityYaml", "{}", { shouldDirty: true });
      form.setValue("topologySpreadYaml", "[]", { shouldDirty: true });
      form.setValue("tolerationsYaml", "[]", { shouldDirty: true });
      form.setValue("priorityClassName", "", { shouldDirty: true });
      variables.replace([]);
      secretVariables.replace([]);
      lastDeploymentScope.current = "";
      return;
    }
    if (
      applicationMode === "existing" &&
      applicationId &&
      applicationId !== reservedApplicationId &&
      applications.isSuccess &&
      !filteredApplications.some(
        (application) => application.id === applicationId,
      )
    ) {
      form.setValue("applicationId", "", { shouldDirty: true });
      form.setValue("routeMode", "internal", { shouldDirty: true });
      form.setValue("hostname", "", { shouldDirty: true });
      form.setValue("nodeSelectorYaml", "{}", { shouldDirty: true });
      form.setValue("affinityYaml", "{}", { shouldDirty: true });
      form.setValue("topologySpreadYaml", "[]", { shouldDirty: true });
      form.setValue("tolerationsYaml", "[]", { shouldDirty: true });
      form.setValue("priorityClassName", "", { shouldDirty: true });
      variables.replace([]);
      secretVariables.replace([]);
      lastDeploymentScope.current = "";
      setReservedApplicationId("");
    }
  }, [
    applicationId,
    applicationMode,
    applications.data,
    applications.isSuccess,
    environments.isSuccess,
    environmentId,
    filteredApplications,
    filteredEnvironments,
    form,
    projectId,
    projects.data,
    projects.isSuccess,
    reservedApplicationId,
    secretVariables,
    variables,
  ]);

  const reserveApplication = useMutation({
    retry: retryNetworkOnce,
    mutationFn: async ({
      projectId,
      name,
      idempotencyKey,
    }: {
      projectId: string;
      name: string;
      idempotencyKey: string;
    }) => {
      const normalizedName = name.trim();
      if (!projectId || !normalizedName) {
        throw new Error(
          "Select a project and enter an application name first.",
        );
      }
      return api.createApplication(
        { projectId, name: normalizedName, sourceKind: "oci" },
        idempotencyKey,
      );
    },
    onSuccess: async (application, input) => {
      const signature = `${input.projectId}:${input.name.trim()}`;
      if (stableApplicationReservation.current?.signature === signature) {
        stableApplicationReservation.current = null;
      }
      await queryClient.invalidateQueries({ queryKey: ["applications"] });
      if (
        input.projectId !== activeProjectId.current ||
        application.projectId !== input.projectId ||
        activeApplicationMode.current !== "new" ||
        activeApplicationName.current.trim() !== input.name.trim()
      ) {
        return;
      }
      setReservedApplicationId(application.id);
      form.setValue("applicationId", application.id, { shouldValidate: true });
      form.setValue("applicationMode", "existing", { shouldValidate: true });
    },
  });
  const reserveApplicationIdentity = () => {
    const name = form.getValues("applicationName");
    const signature = `${projectId}:${name.trim()}`;
    const idempotencyKey =
      stableApplicationReservation.current?.signature === signature
        ? stableApplicationReservation.current.key
        : crypto.randomUUID();
    stableApplicationReservation.current = { signature, key: idempotencyKey };
    reserveApplication.mutate({ projectId, name, idempotencyKey });
  };

  const deploy = useMutation({
    mutationFn: async ({
      values,
      idempotencyKey,
    }: {
      values: DeploymentForm;
      idempotencyKey: string;
      draftSignature: string;
    }) => {
      if (!gitOpsReady) {
        throw new Error(
          "Protected Git and Argo CD must both report fresh readiness before an App can be deployed.",
        );
      }
      if (!environmentGitReady) {
        throw new Error(
          "Configure a ready Git authority for this Environment before deploying the App.",
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
      const workloadScheduling = workloadSchedulingFromGuided(values);
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
          "A fresh server-derived sslip.io hostname preview is required before deploying the App.",
        );
      }
      if (values.applicationMode !== "existing" || !values.applicationId) {
        throw new Error("Create or select the App identity before deployment.");
      }
      const applicationId = values.applicationId;
      const env = [
        ...values.variables
          .filter(({ key }) => key.trim())
          .map(({ key, value }) => ({ name: key.trim(), value })),
        ...secretEnvironment,
      ];
      return api.createDeployment(
        {
          environmentId: values.environmentId,
          applicationId,
          image: values.image,
          ...(isCanonicalTaggedImage(values.image) && imageResolution.data
            ? { expectedImmutableImage: imageResolution.data.immutableImage }
            : {}),
          runtime: {
            replicas: values.replicas,
            workloadType: values.workloadType,
            strategy: { type: values.strategyType },
            ...(values.workloadType === "StatefulSet"
              ? { podManagementPolicy: values.podManagementPolicy }
              : {}),
            ...workloadProcess,
            ...workloadScheduling,
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
        },
        idempotencyKey,
        existingGitBundle.data?.etag,
      );
    },
    onSuccess: async (operation, input) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["applications"] }),
        queryClient.invalidateQueries({ queryKey: ["deployments"] }),
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
      ]);
      if (JSON.stringify(form.getValues()) !== input.draftSignature) return;
      if (stableDeploymentAttempt.current?.key === input.idempotencyKey) {
        stableDeploymentAttempt.current = null;
      }
      await navigate({
        to: "/operations/$operationId",
        params: { operationId: operation.id },
      });
    },
  });

  const submitDeployment = (values: DeploymentForm) => {
    const signature = JSON.stringify(values);
    const idempotencyKey =
      stableDeploymentAttempt.current?.signature === signature
        ? stableDeploymentAttempt.current.key
        : crypto.randomUUID();
    stableDeploymentAttempt.current = { signature, key: idempotencyKey };
    deploy.mutate({
      values,
      idempotencyKey,
      draftSignature: signature,
    });
  };

  const loadError = projects.error ?? environments.error ?? applications.error;
  const gitBundlePending =
    Boolean(existingDeploymentScope) &&
    (!existingDeployments.isSuccess ||
      Boolean(existingDeployment && existingGitBundle.isPending));
  const gitBundleError =
    existingDeployments.error ?? existingGitBundle.error ?? null;
  const gitBundleReady =
    !existingDeploymentScope ||
    (existingDeployments.isSuccess &&
      (!existingDeployment || Boolean(existingGitBundle.data?.etag)));
  const noScopes = !projects.isPending && !projects.data?.items.length;

  return (
    <Page narrow>
      <PageHeader
        eyebrow="App"
        title="Add App from OCI image"
        description="Deploy an exact OCI digest, or resolve an authorized existing-image tag. Kuberploy commits only immutable desired state to Git."
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
            <Link
              to="/projects"
              className={buttonVariants({ variant: "primary" })}
            >
              Create workspace
            </Link>
          }
        />
      ) : (
        <form onSubmit={form.handleSubmit(submitDeployment)}>
          <FormCard>
            <FormCardHeading step="01">
              <div>
                <h2>Placement</h2>
                <p>
                  Choose the project policy boundary and exact destination
                  environment.
                </p>
              </div>
            </FormCardHeading>
            <FormGrid>
              <Field
                label="Project"
                required
                error={form.formState.errors.projectId?.message}
              >
                <Select
                  {...form.register("projectId", {
                    required: "Select a project.",
                    onChange: () => {
                      form.setValue("environmentId", "");
                      form.setValue("applicationId", "");
                      form.setValue("routeMode", "internal");
                    },
                  })}
                  value={form.watch("projectId")}
                >
                  <option value="">Select project</option>
                  {projects.data?.items.map((project) => (
                    <option key={project.id} value={project.id}>
                      {project.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label="Environment"
                required
                hint="Maps to one namespace and Argo CD project."
                error={form.formState.errors.environmentId?.message}
              >
                <Select
                  disabled={!projectId}
                  {...form.register("environmentId", {
                    required: "Select an environment.",
                  })}
                  value={form.watch("environmentId")}
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
                </Select>
              </Field>
            </FormGrid>
            <FormGrid columns={3}>
              <Field
                label="CPU request"
                required
                hint="New Apps default to 50m."
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
                hint="New Apps default to 100Mi."
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
            </FormGrid>
          </FormCard>

          <FormCard>
            <FormCardHeading step="02">
              <div>
                <h2>Application identity</h2>
                <p>
                  Create the durable logical identity before previewing image,
                  sslip.io, TLS, DNS, middleware, or secret configuration.
                </p>
              </div>
            </FormCardHeading>
            <div
              className="flex w-max p-1 border border-line rounded-[9px] bg-surface-soft [&_label]:cursor-pointer [&_input]:absolute [&_input]:w-px [&_input]:h-px [&_input]:opacity-0 [&_span]:block [&_span]:py-2 [&_span]:px-3 [&_span]:rounded-md [&_span]:text-ink-faint [&_span]:text-meta [&_span]:font-semibold [&_input:checked_+_span]:text-ink [&_input:checked_+_span]:bg-surface [&_input:checked_+_span]:shadow-[0_1px_4px_rgba(15_34_26_0.1)] pointer-coarse:[&_button]:min-h-10"
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
                        stableApplicationReservation.current = null;
                        reserveApplication.reset();
                        setReservedApplicationId("");
                        form.setValue("applicationId", "", {
                          shouldDirty: true,
                        });
                        form.setValue("hostname", "", { shouldDirty: true });
                        form.setValue("routeMode", "internal");
                        form.setValue("nodeSelectorYaml", "{}", {
                          shouldDirty: true,
                        });
                        form.setValue("affinityYaml", "{}", {
                          shouldDirty: true,
                        });
                        form.setValue("topologySpreadYaml", "[]", {
                          shouldDirty: true,
                        });
                        form.setValue("tolerationsYaml", "[]", {
                          shouldDirty: true,
                        });
                        form.setValue("priorityClassName", "", {
                          shouldDirty: true,
                        });
                        variables.replace([]);
                        secretVariables.replace([]);
                        lastDeploymentScope.current = "";
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
              <div className="grid gap-4">
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
                    onClick={reserveApplicationIdentity}
                  >
                    Create application identity
                  </Button>
                  <MutedCopy>
                    This creates a recoverable application record, not a
                    workload. It remains available from Projects even if you
                    leave this App setup.
                  </MutedCopy>
                </div>
              </div>
            ) : (
              <Field
                label="Application"
                required
                error={form.formState.errors.applicationId?.message}
              >
                <Select
                  {...form.register("applicationId", {
                    required:
                      applicationMode === "existing"
                        ? "Select an application."
                        : false,
                  })}
                  value={form.watch("applicationId")}
                >
                  <option value="">Select application</option>
                  {filteredApplications.map((application) => (
                    <option key={application.id} value={application.id}>
                      {application.name}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            {reservedApplicationId ? (
              <Notice tone="success" role="status">
                <div>
                  <strong>Application identity created</strong>
                  <p>
                    Application-scoped previews are now enabled. You can deploy
                    here or configure another source from its application page.
                  </p>
                </div>
                <Link
                  to="/applications/$applicationId"
                  params={{ applicationId: reservedApplicationId }}
                  className={buttonVariants({ variant: "secondary" })}
                >
                  Source options
                </Link>
              </Notice>
            ) : null}
            {reserveApplication.error ? (
              <ErrorPanel
                title="Application identity was not created"
                error={reserveApplication.error}
                onRetry={reserveApplicationIdentity}
              />
            ) : null}
          </FormCard>

          <FormCard>
            <FormCardHeading
              step="03"
              className="grid-cols-[38px_1fr_auto] page-to-580:grid-cols-[38px_1fr]"
            >
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
            </FormCardHeading>
            {secretVariables.fields.length ? (
              <div className="flex flex-col gap-2">
                {secretVariables.fields.map((field, index) => (
                  <div
                    className="grid grid-cols-[1fr_1.4fr_32px] items-end gap-3 [&_.icon-button]:mb-1 to-580:grid-cols-[1fr_32px] to-580:[&_.field:first-child]:col-[1] to-580:[&_.field:nth-child(2)]:col-[1] to-580:[&_.icon-button]:row-[1] to-580:[&_.icon-button]:col-[2]"
                    key={field.id}
                  >
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
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                      onClick={() => secretVariables.remove(index)}
                      aria-label={`Remove secret variable ${index + 1}`}
                    >
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
                No secret references.
              </div>
            )}
          </FormCard>

          <FormCard>
            <FormCardHeading step="04">
              <div>
                <h2>Artifact & runtime</h2>
                <p>
                  Exact digests submit directly. Authorized tags require a fresh
                  server-owned resolution preview for this application and
                  environment.
                </p>
              </div>
            </FormCardHeading>
            <Field
              label="Image digest or tag"
              required
              hint="Digest: registry.example.com/team/app@sha256:… · Tag: registry.example.com/team/app:release"
              error={form.formState.errors.image?.message}
            >
              <div className="relative [&_svg]:absolute [&_svg]:z-[1] [&_svg]:top-[11px] [&_svg]:left-[11px] [&_svg]:w-4 [&_svg]:text-ink-faint [&_input]:pl-10 [&_input]:font-mono">
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
              <Notice tone="warning">
                <strong>Resolve this tag before deploying the App</strong>
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
              </Notice>
            ) : null}
            {imageResolutionIsCurrent && imageResolution.data ? (
              <Notice tone="success" role="status">
                <strong>Immutable image resolved</strong>
                <p>
                  <code>{imageResolution.data.requestedImage}</code>
                  {" → "}
                  <code>{imageResolution.data.immutableImage}</code>
                </p>
              </Notice>
            ) : null}
            {imageResolutionErrorIsCurrent ? (
              <Notice tone="error" role="alert">
                <strong>Tag could not be resolved</strong>
                <p>{errorMessage(imageResolution.error)}</p>
              </Notice>
            ) : null}
            <FormGrid>
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
            </FormGrid>
            <FormGrid columns={3}>
              <Field label="Workload type" required>
                <Select
                  {...form.register("workloadType", {
                    onChange: (event) => {
                      const stateful = event.target.value === "StatefulSet";
                      form.setValue("strategyType", "RollingUpdate", {
                        shouldDirty: true,
                        shouldTouch: true,
                      });
                      if (stateful) {
                        form.setValue("podManagementPolicy", "OrderedReady", {
                          shouldDirty: true,
                          shouldTouch: true,
                        });
                      }
                    },
                  })}
                  value={form.watch("workloadType")}
                >
                  <option value="Deployment">Deployment</option>
                  <option value="StatefulSet">StatefulSet</option>
                </Select>
              </Field>
              <Field
                label={`${workloadType === "StatefulSet" ? "StatefulSet" : "Deployment"} strategy`}
                required
                hint="StatefulSets support rolling update or on-delete; Deployments support rolling update or recreate."
              >
                <Select
                  {...form.register("strategyType")}
                  value={form.watch("strategyType")}
                >
                  <option value="RollingUpdate">Rolling update</option>
                  {workloadType === "StatefulSet" ? (
                    <option value="OnDelete">On delete</option>
                  ) : (
                    <option value="Recreate">Recreate</option>
                  )}
                </Select>
              </Field>
              {workloadType === "StatefulSet" ? (
                <Field label="Pod management policy" required>
                  <Select
                    {...form.register("podManagementPolicy")}
                    value={form.watch("podManagementPolicy")}
                  >
                    <option value="OrderedReady">Ordered ready</option>
                    <option value="Parallel">Parallel</option>
                  </Select>
                </Field>
              ) : null}
            </FormGrid>
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
                  "workingDirectory",
                  value.workingDirectory ?? "",
                  { shouldDirty: true, shouldTouch: true },
                );
                form.setValue(
                  "terminationGracePeriodSeconds",
                  value.terminationGracePeriodSeconds,
                  { shouldDirty: true, shouldTouch: true },
                );
              }}
            />
          </FormCard>

          <FormCard>
            <FormCardHeading step="05">
              <div>
                <h2>Scheduling for this App</h2>
                <p>
                  Choose placement for this app without changing nodes, taints,
                  or cluster-wide scheduling policy.
                </p>
              </div>
            </FormCardHeading>
            <SchedulingEditor
              value={{
                nodeSelectorYaml: scheduling[0] ?? "{}",
                affinityYaml: scheduling[1] ?? "{}",
                topologySpreadYaml: scheduling[2] ?? "[]",
                tolerationsYaml: scheduling[3] ?? "[]",
                priorityClassName: scheduling[4] ?? "",
              }}
              applicationId={applicationId}
              onChange={(value) => {
                form.setValue("nodeSelectorYaml", value.nodeSelectorYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue("affinityYaml", value.affinityYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue("topologySpreadYaml", value.topologySpreadYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue("tolerationsYaml", value.tolerationsYaml, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
                form.setValue("priorityClassName", value.priorityClassName, {
                  shouldDirty: true,
                  shouldTouch: true,
                });
              }}
            />
            <p className="mt-1 mx-0 mb-0 text-ink-faint text-xs leading-[1.45]">
              Affinity, anti-affinity, and topology selectors are bound to this
              exact application identity.
            </p>
          </FormCard>

          <FormCard>
            <FormCardHeading step="06">
              <div>
                <h2>Health checks</h2>
                <p>
                  Add startup, readiness, or liveness checks. All are optional;
                  Kubernetes defaults apply to empty timing fields.
                </p>
              </div>
            </FormCardHeading>
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
          </FormCard>

          <FormCard>
            <FormCardHeading
              step="07"
              className="grid-cols-[38px_1fr_auto] page-to-580:grid-cols-[38px_1fr]"
            >
              <div>
                <h2>Runtime environment values</h2>
                <p>
                  Used only by the deployed App. Visible in Git and rendered
                  through an immutable ConfigMap; never passed to the image
                  builder. Never place secrets here.
                </p>
              </div>
              <Button
                type="button"
                variant="secondary"
                onClick={() => variables.append({ key: "", value: "" })}
              >
                <Icon name="plus" /> Add value
              </Button>
            </FormCardHeading>
            {variables.fields.length ? (
              <div className="flex flex-col gap-2">
                {variables.fields.map((field, index) => (
                  <div
                    className="grid grid-cols-[1fr_1.4fr_32px] items-end gap-3 [&_.icon-button]:mb-1 to-580:grid-cols-[1fr_32px] to-580:[&_.field:first-child]:col-[1] to-580:[&_.field:nth-child(2)]:col-[1] to-580:[&_.icon-button]:row-[1] to-580:[&_.icon-button]:col-[2]"
                    key={field.id}
                  >
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
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                      onClick={() => variables.remove(index)}
                      aria-label={`Remove variable ${index + 1}`}
                    >
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
                No ordinary values. Secret bindings are added later through the
                write-only configuration flow.
              </div>
            )}
          </FormCard>

          <FormCard>
            <FormCardHeading step="08">
              <div>
                <h2>Initial internet route</h2>
                <p>
                  Optional HTTP-only exposure through Traefik. TLS, DNS
                  automation, and middleware use the previewed configuration
                  flow after creation.
                </p>
              </div>
            </FormCardHeading>
            <div
              className="inline-flex p-1 border border-line rounded-[9px] bg-surface-soft [&_label]:cursor-pointer [&_input]:absolute [&_input]:w-px [&_input]:h-px [&_input]:opacity-0 [&_span]:block [&_span]:py-2 [&_span]:px-3 [&_span]:rounded-md [&_span]:text-ink-faint [&_span]:text-meta [&_span]:font-semibold [&_input:checked_+_span]:text-ink [&_input:checked_+_span]:bg-surface [&_input:checked_+_span]:shadow-[0_1px_4px_rgba(15_34_26_0.1)] pointer-coarse:[&_button]:min-h-10"
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
                hint="Read-only and derived from the fresh public Traefik endpoint. The App request sends no hostname or IP."
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
              <small className="mt-2 block text-ink-faint text-xs">
                sslip.io remains unavailable until the platform observes a fresh
                public Traefik endpoint.
              </small>
            ) : applicationMode !== "existing" ? (
              <small className="mt-2 block text-ink-faint text-xs">
                Select an existing application to preview its exact sslip.io
                hostname before deploying the App.
              </small>
            ) : sslipHostname.error ? (
              <small className="mt-2 block text-ink-faint text-xs">
                {errorMessage(sslipHostname.error)}
              </small>
            ) : null}
            <div className="flex items-center justify-between flex-wrap gap-4 mt-4 pt-4 border-t border-t-line [&>div]:flex [&>div]:min-w-0 [&>div]:items-center [&>div]:flex-wrap [&>div]:gap-2 [&_small]:text-ink-faint [&_small]:text-xs to-580:items-start to-580:flex-col to-580:[&>div:last-child]:items-start to-580:[&>div:last-child]:flex-wrap">
              <div>
                <StatusPill value="active" label="HTTP only · /" />
                <small>Supported during initial App deployment</small>
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
          </FormCard>

          <FormCard className="bg-surface-soft shadow-none">
            <div className="flex items-center gap-4 [&_div]:flex-1 [&_h3]:m-0 [&_h3]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-faint [&_p]:text-meta [&_p]:leading-[1.5] to-580:items-start to-580:flex-wrap to-580:[&_div]:min-w-[calc(100%_-_55px)]">
              <span className="grid w-[37px] h-[37px] flex-none place-items-center rounded-[10px] text-mint-dark bg-mint-soft [&_svg]:w-[18px]">
                <Icon name="route" />
              </span>
              <div>
                <h3>Advanced exposure stays preview-first</h3>
                <p>
                  After the App runtime exists, upgrade the initial HTTP route
                  with TLS, DNS automation, and middleware in its shared Form /
                  Advanced YAML editor.
                </p>
              </div>
              <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                Next step
              </span>
            </div>
          </FormCard>

          {deploy.error ? (
            <Notice tone="error" role="alert">
              <strong>App could not be deployed</strong>
              <p>{errorMessage(deploy.error)}</p>
            </Notice>
          ) : null}
          {!capabilities.isPending && !gitOpsReady ? (
            <Notice tone="warning" role="status">
              <strong>Protected GitOps is not ready</strong>
              <p>
                App deployment remains disabled until both the exact Git
                projection worker and protected Argo desired-state runtime are
                healthy.
              </p>
            </Notice>
          ) : null}
          {environmentId && environmentGitBindingMissing ? (
            <Notice tone="warning" role="status">
              <strong>Environment Git authority required</strong>
              <p>
                Configure Git for this Environment before deploying the App.
              </p>
              <Link
                to="/projects/$projectId"
                params={{ projectId }}
                className={buttonVariants({ variant: "secondary" })}
              >
                Open Environment Git settings
              </Link>
            </Notice>
          ) : null}
          {environmentId &&
          !environmentGitBindingMissing &&
          environmentGitBinding.error ? (
            <Notice tone="error" role="alert">
              <strong>Environment Git authority could not be checked</strong>
              <p>{errorMessage(environmentGitBinding.error)}</p>
            </Notice>
          ) : null}
          {environmentId &&
          !environmentGitBindingMissing &&
          !environmentGitBinding.error &&
          !environmentGitBinding.isPending &&
          !environmentGitReady ? (
            <Notice tone="warning" role="status">
              <strong>Environment Git authority is not ready</strong>
              <p>
                Current state: {environmentGitBinding.data?.state ?? "unknown"}.
                Wait for Git indexing to finish, then retry.
              </p>
            </Notice>
          ) : null}
          {existingDeploymentScope && gitBundlePending ? (
            <Notice tone="info" role="status">
              <strong>Loading current Git configuration</strong>
              <p>
                Existing applications use the current strong Git bundle ETag for
                a safe App update.
              </p>
            </Notice>
          ) : null}
          {existingDeploymentScope && gitBundleError ? (
            <Notice tone="error" role="alert">
              <strong>Current Git configuration is unavailable</strong>
              <p>{errorMessage(gitBundleError)}</p>
            </Notice>
          ) : null}
          <FormActions>
            <Link to="/" className={buttonVariants({ variant: "ghost" })}>
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
                environmentGitBinding.isPending ||
                !environmentGitReady ||
                !gitBundleReady ||
                !imageReady,
              )}
            >
              Commit & deploy <Icon name="arrow" />
            </Button>
          </FormActions>
        </form>
      )}
    </Page>
  );
}
