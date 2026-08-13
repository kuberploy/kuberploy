import type {
  AccessGrant,
  AccessRole,
  AccessScopeType,
  ApiMeta,
  AuditEventList,
  AssignedMiddlewareProfile,
  MiddlewareProfileAssignment,
  MiddlewareProfileEntry,
  MiddlewareProfileSpec,
  Application,
  BuildArgument,
  BuildAttempt,
  BuildLogLine,
  BuildLogSnapshot,
  BuildDefinition,
  BuildFileReference,
  BuildProfile,
  CertificateBindingDetail,
  CertificateBindingMetadata,
  CertificateIssuerAdminEntry,
  CertificateIssuerCatalog,
  CertificateIssuerMutation,
  Capabilities,
  Collection,
  ConfigBundle,
  ConfigChange,
  ConfigDiagnostic,
  ConfigPreview,
  ConfigValidation,
  CreateBuildDefinition,
  CreateCertificateBinding,
  CreateDeployment,
  ImageResolutionPreview,
  PromoteBuildAttempt,
  Deployment,
  DeploymentRollbackCandidate,
  DeploymentStatus,
  Environment,
  EnvironmentGitBinding,
  ExternalDNSCatalog,
  ExternalDNSCatalogItem,
  ExternalDNSIntegration,
  ExternalDNSIntegrationInput,
  ExternalDNSStatus,
  EventSnapshot,
  GitHubInstallation,
  GitHubRepository,
  HelmApproval,
  CreateHelmApproval,
  HelmMutationResult,
  HelmReleaseRevision,
  HelmReleaseStatus,
  HelmRenderedPreview,
  HelmValuesInput,
  HelmValuesPreview,
  LinkedGitHubSetup,
  LogSnapshot,
  LatestPlatformRelease,
  MonitoringStatus,
  MetricKey,
  MetricRangeResult,
  Operation,
  OperationWire,
  Principal,
  ProblemDetail,
  Project,
  PlatformArgoGitBinding,
  PlatformUpgrade,
  CreatePlatformArgoGitBinding,
  CreateEnvironmentGitBinding,
  ApplicationRegistryTarget,
  RegistryCacheGeneration,
  RegistryCatalogObservation,
  RegistryCleanupItem,
  RegistryCleanupPlan,
  RegistryPolicy,
  RegistryPolicyInput,
  RegistryRelease,
  RegistryTarget,
  RegistryTargetInput,
  ProjectRegistryPullCredential,
  ProjectRegistryPullCredentialCatalog,
  ApplicationRegistryPullSelection,
  CreateRuntimeSecretBinding,
  RotateRuntimeSecretBinding,
  RotateCertificateBinding,
  RuntimeSecretBindingDetail,
  RuntimeSecretBindingMetadata,
  RuntimeSecretDelivery,
  ServiceAccount,
  ServiceAccountRole,
  ServiceAccountToken,
  ServiceAccountTokenIssue,
  SSLIPHostnamePreview,
  AutomationScope,
  Team,
  TeamMember,
  User,
  UserInvitation,
  Workload,
  AutoDeployPolicy,
  AutoDeployPolicyRevision,
  AutoDeployRun,
  CreateAutoDeployPolicy,
  ReviseAutoDeployPolicy,
  VariableSetPreview,
  VariableSetScope,
  VariableSetSnapshot,
} from "./types";
import {
  isCanonicalImmutableImage,
  isCanonicalTaggedImage,
} from "../lib/imageReference";

export class ApiError extends Error {
  readonly status: number;
  readonly problem?: ProblemDetail;

  constructor(status: number, problem?: ProblemDetail) {
    super(
      problem?.detail ??
        problem?.title ??
        `Request failed with status ${status}`,
    );
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

type RequestOptions = Omit<RequestInit, "body"> & {
  body?: unknown;
  responseMetadata?: (response: Response) => void;
};

export type WorkloadLogOptions = {
  pod?: string;
  revision?: string;
  container?: string;
  tailLines?: number;
  since?: string;
  previous?: boolean;
  limitBytes?: number;
};

export type WorkloadEventOptions = {
  limit?: number;
};

export type BuildLogOptions = {
  tailLines?: number;
  since?: string;
  previous?: boolean;
  limitBytes?: number;
};

let responseCsrfToken: string | undefined;

function csrfToken(): string | undefined {
  if (responseCsrfToken) return responseCsrfToken;
  if (typeof document === "undefined") return undefined;
  const encoded = document.cookie
    .split(";")
    .map((entry) => entry.trim().split("="))
    .find(([name]) => name === "kuberploy_csrf")
    ?.slice(1)
    .join("=");
  return encoded ? decodeURIComponent(encoded) : undefined;
}

async function request<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const { body, responseMetadata, ...requestOptions } = options;
  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (body !== undefined) headers.set("Content-Type", "application/json");
  const method = options.method?.toUpperCase() ?? "GET";
  if (
    !["GET", "HEAD", "OPTIONS"].includes(method) &&
    path !== "/v1/auth/bootstrap" &&
    path !== "/v1/auth/invitations/accept"
  ) {
    const token = csrfToken();
    if (token) headers.set("X-CSRF-Token", token);
  }

  let response: Response;
  try {
    response = await fetch(path, {
      ...requestOptions,
      credentials: "same-origin",
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (cause) {
    throw new ApiError(0, {
      title: "Control plane unavailable",
      detail:
        cause instanceof Error
          ? cause.message
          : "The API could not be reached.",
      retryable: true,
    });
  }

  if (!response.ok) {
    const payload = (await response.json().catch(() => undefined)) as
      ProblemDetail | ConfigValidation | undefined;
    let problem = payload as ProblemDetail | undefined;
    if (
      payload &&
      "valid" in payload &&
      payload.valid === false &&
      payload.diagnostics.length > 0
    ) {
      const first = payload.diagnostics[0];
      problem = {
        title: "Configuration validation failed",
        detail: `${first.pointer ? `${first.pointer}: ` : ""}${first.detail}`,
        code: "ConfigValidationFailed",
        errors: payload.diagnostics,
      };
    }
    throw new ApiError(response.status, problem);
  }

  responseCsrfToken = response.headers.get("X-CSRF-Token") ?? responseCsrfToken;
  responseMetadata?.(response);

  if (response.status === 204) return undefined as T;
  const data = (await response.json()) as T;

  if (
    typeof data === "object" &&
    data !== null &&
    "documents" in data &&
    !("etag" in data)
  ) {
    Object.assign(data, { etag: response.headers.get("ETag") ?? "" });
  }

  return data;
}

function idempotencyHeaders(): HeadersInit {
  return { "Idempotency-Key": crypto.randomUUID() };
}

export function asCollection<T>(
  value:
    | Collection<T>
    | { items: T[] | null; nextCursor?: string | null }
    | T[]
    | null
    | undefined,
): Collection<T> {
  if (!value) return { items: [] };
  if (Array.isArray(value)) return { items: value };
  return { ...value, items: Array.isArray(value.items) ? value.items : [] };
}

export function normalizeOperation(operation: OperationWire): Operation {
  const state = operation.status ?? operation.state ?? "queued";
  const steps =
    operation.steps ??
    operation.progress?.map((step, index) => ({
      id: `${index}-${step.name}`,
      name: step.name,
      state: step.status,
      message: step.detail,
      startedAt: step.startedAt,
      completedAt: step.finishedAt,
    }));
  return {
    ...operation,
    pullRequest: safePullRequestPublication(operation.pullRequest),
    status: state,
    state,
    steps,
    completedAt: operation.completedAt ?? operation.finishedAt,
    target:
      operation.target ??
      (operation.targetId
        ? { id: operation.targetId, type: operation.targetType }
        : undefined),
  };
}

function safePullRequestPublication(
  value: OperationWire["pullRequest"],
): OperationWire["pullRequest"] {
  if (
    !value ||
    !Number.isSafeInteger(value.number) ||
    value.number < 1 ||
    (value.state !== "open" && value.state !== "closed") ||
    !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(value.candidateRevision)
  ) {
    return undefined;
  }
  try {
    const url = new URL(value.url);
    const path = url.pathname.match(
      /^\/[A-Za-z0-9-]+\/[A-Za-z0-9_.-]+\/pull\/([1-9][0-9]*)$/,
    );
    if (
      url.protocol !== "https:" ||
      url.hostname !== "github.com" ||
      url.username !== "" ||
      url.password !== "" ||
      url.search !== "" ||
      url.hash !== "" ||
      !path ||
      Number(path[1]) !== value.number
    ) {
      return undefined;
    }
  } catch {
    return undefined;
  }
  return value;
}

export function normalizeDeployment(deployment: Deployment): Deployment {
  return {
    ...deployment,
    status: deployment.status ?? deployment.state,
    configRevision: deployment.configRevision ?? deployment.desiredRevision,
  };
}

type GitHubSetupAuthorizationWire = {
  authorizationUrl: string;
  state: string;
  expiresAt: string;
};

function githubSetupDestination(
  response: GitHubSetupAuthorizationWire,
): string {
  let destination: URL;
  try {
    destination = new URL(response.authorizationUrl);
  } catch {
    throw new ApiError(502, {
      title: "GitHub setup response rejected",
      detail: "The provider authorization destination was not valid.",
    });
  }
  const state = destination.searchParams.getAll("state");
  const keys = [...destination.searchParams.keys()];
  const githubInstall =
    destination.origin === "https://github.com" &&
    /^\/apps\/[a-z0-9-]+\/installations\/new$/.test(destination.pathname) &&
    keys.length === 1 &&
    keys[0] === "state";
  const existingInstall =
    destination.origin === window.location.origin &&
    destination.pathname === "/v1/github/installations/setup" &&
    keys.length === 3 &&
    keys.every((key) =>
      ["installation_id", "setup_action", "state"].includes(key),
    ) &&
    /^\d+$/.test(destination.searchParams.get("installation_id") ?? "") &&
    destination.searchParams.get("setup_action") === "update";
  if (
    destination.username !== "" ||
    destination.password !== "" ||
    destination.hash !== "" ||
    (!githubInstall && !existingInstall) ||
    state.length !== 1 ||
    state[0].length < 64 ||
    state[0].length > 4096 ||
    state[0] !== response.state
  ) {
    throw new ApiError(502, {
      title: "GitHub setup response rejected",
      detail: "The provider authorization destination failed validation.",
    });
  }
  return destination.toString();
}

function safeGitHubInstallation(
  installation: GitHubInstallation,
): GitHubInstallation {
  return {
    id: installation.id,
    githubInstallationId: installation.githubInstallationId,
    accountLogin: installation.accountLogin,
    accountType: installation.accountType,
    ownerUserId: installation.ownerUserId,
    visibility: installation.visibility,
    teamId: installation.teamId,
    repositorySelection: installation.repositorySelection,
    repositoryCount: installation.repositoryCount,
    createdAt: installation.createdAt,
    updatedAt: installation.updatedAt,
  };
}

function safeGitHubRepository(repository: GitHubRepository): GitHubRepository {
  return {
    id: repository.id,
    githubRepositoryId: repository.githubRepositoryId,
    installationId: repository.installationId,
    ownerId: repository.ownerId,
    ownerLogin: repository.ownerLogin,
    name: repository.name,
    lifecycle: repository.lifecycle,
  };
}

function safePlatformArgoGitBinding(
  binding: PlatformArgoGitBinding,
): PlatformArgoGitBinding {
  return {
    id: binding.id,
    clusterId: binding.clusterId,
    repository: {
      provider: "github",
      installationId: binding.repository.installationId,
      repositoryId: binding.repository.repositoryId,
      owner: binding.repository.owner,
      name: binding.repository.name,
    },
    targetRef: binding.targetRef,
    pathPrefix: binding.pathPrefix,
    state: binding.state,
    targetHeadRevision: binding.targetHeadRevision,
    targetHeadObservedAt: binding.targetHeadObservedAt,
    createdAt: binding.createdAt,
    updatedAt: binding.updatedAt,
  };
}

function safeEnvironmentGitBinding(
  binding: EnvironmentGitBinding,
): EnvironmentGitBinding {
  return {
    id: binding.id,
    projectId: binding.projectId,
    environmentId: binding.environmentId,
    repository: {
      provider: "github",
      installationId: binding.repository.installationId,
      repositoryId: binding.repository.repositoryId,
      owner: binding.repository.owner,
      name: binding.repository.name,
    },
    targetRef: binding.targetRef,
    pathPrefix: binding.pathPrefix,
    credentialMode: "github-app",
    state: binding.state,
    targetHeadRevision: binding.targetHeadRevision,
    indexedRevision: binding.indexedRevision,
    projectionGeneration: binding.projectionGeneration,
    parserVersion: binding.parserVersion,
    targetHeadObservedAt: binding.targetHeadObservedAt,
    indexedAt: binding.indexedAt,
    createdAt: binding.createdAt,
    updatedAt: binding.updatedAt,
  };
}

const variableUUIDPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const variableRevisionPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const variableETagPattern = /^"sha256:[0-9a-f]{64}"$/;
const variableNamePattern = /^[A-Za-z_][A-Za-z0-9_]*$/;

function variableProjectionRejected(detail: string): never {
  throw new ApiError(502, {
    title: "VariableSet response rejected",
    detail,
    code: "VariableSetProjectionInvalid",
  });
}

function plainRecord(value: unknown): Record<string, unknown> | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    Array.isArray(value) ||
    Object.getPrototypeOf(value) !== Object.prototype
  )
    return undefined;
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, allowed: readonly string[]) {
  return Object.keys(value).every((key) => allowed.includes(key));
}

function safeVariableDocument(value: unknown): Record<string, unknown> {
  const document = plainRecord(value);
  const values = plainRecord(document?.values);
  if (
    !document ||
    !values ||
    !exactKeys(document, ["apiVersion", "kind", "values"]) ||
    document.apiVersion !== "variables.kuberploy.io/v1alpha1" ||
    document.kind !== "VariableSet" ||
    Object.keys(values).length > 256 ||
    Object.entries(values).some(
      ([name, entry]) =>
        !variableNamePattern.test(name) ||
        typeof entry !== "string" ||
        entry.length > 4096,
    )
  ) {
    return variableProjectionRejected(
      "The parsed VariableSet document was not the closed ordinary-value schema.",
    );
  }
  return {
    apiVersion: "variables.kuberploy.io/v1alpha1",
    kind: "VariableSet",
    values: Object.fromEntries(Object.entries(values)),
  };
}

function safeVariableSnapshot(
  value: unknown,
  expectedEnvironmentId: string,
  expectedScope: VariableSetScope,
): VariableSetSnapshot {
  const snapshot = plainRecord(value);
  if (
    !snapshot ||
    !exactKeys(snapshot, [
      "scope",
      "bindingId",
      "projectId",
      "environmentId",
      "path",
      "present",
      "etag",
      "rawYaml",
      "document",
      "indexedRevision",
    ]) ||
    snapshot.scope !== expectedScope ||
    typeof snapshot.bindingId !== "string" ||
    !variableUUIDPattern.test(snapshot.bindingId) ||
    typeof snapshot.projectId !== "string" ||
    !variableUUIDPattern.test(snapshot.projectId) ||
    snapshot.environmentId !== expectedEnvironmentId ||
    !variableUUIDPattern.test(expectedEnvironmentId) ||
    typeof snapshot.path !== "string" ||
    typeof snapshot.present !== "boolean" ||
    typeof snapshot.indexedRevision !== "string" ||
    !variableRevisionPattern.test(snapshot.indexedRevision)
  ) {
    return variableProjectionRejected(
      "The VariableSet snapshot identity or revision was invalid.",
    );
  }
  const expectedPath =
    expectedScope === "project"
      ? `tenants/${snapshot.projectId}/variables.yaml`
      : `tenants/${snapshot.projectId}/environments/${expectedEnvironmentId}/variables.yaml`;
  if (snapshot.path !== expectedPath || snapshot.path.length > 1024) {
    return variableProjectionRejected(
      "The VariableSet response did not use the exact server-derived path.",
    );
  }
  if (!snapshot.present) {
    if ("etag" in snapshot || "rawYaml" in snapshot || "document" in snapshot) {
      return variableProjectionRejected(
        "An absent VariableSet carried ambiguous document material.",
      );
    }
    return {
      scope: expectedScope,
      bindingId: snapshot.bindingId,
      projectId: snapshot.projectId,
      environmentId: expectedEnvironmentId,
      path: expectedPath,
      present: false,
      indexedRevision: snapshot.indexedRevision,
    };
  }
  if (
    typeof snapshot.etag !== "string" ||
    !variableETagPattern.test(snapshot.etag) ||
    typeof snapshot.rawYaml !== "string" ||
    snapshot.rawYaml.length < 1 ||
    snapshot.rawYaml.length > 131072
  ) {
    return variableProjectionRejected(
      "A present VariableSet did not carry a bounded exact ETag and raw document.",
    );
  }
  return {
    scope: expectedScope,
    bindingId: snapshot.bindingId,
    projectId: snapshot.projectId,
    environmentId: expectedEnvironmentId,
    path: expectedPath,
    present: true,
    etag: snapshot.etag,
    rawYaml: snapshot.rawYaml,
    document: safeVariableDocument(snapshot.document),
    indexedRevision: snapshot.indexedRevision,
  };
}

function safeVariableSnapshots(
  value: unknown,
  environmentId: string,
): Collection<VariableSetSnapshot> {
  const collection = plainRecord(value);
  if (
    !collection ||
    !exactKeys(collection, ["items"]) ||
    !Array.isArray(collection.items) ||
    collection.items.length !== 2
  ) {
    return variableProjectionRejected(
      "The VariableSet response must contain exactly project and environment sources.",
    );
  }
  const project = safeVariableSnapshot(
    collection.items[0],
    environmentId,
    "project",
  );
  const environment = safeVariableSnapshot(
    collection.items[1],
    environmentId,
    "environment",
  );
  if (
    project.bindingId !== environment.bindingId ||
    project.projectId !== environment.projectId ||
    project.indexedRevision !== environment.indexedRevision
  ) {
    return variableProjectionRejected(
      "The two VariableSet sources were not from one exact indexed binding snapshot.",
    );
  }
  return { items: [project, environment] };
}

function safeVariableDiagnostic(value: unknown): ConfigDiagnostic {
  const diagnostic = plainRecord(value);
  if (
    !diagnostic ||
    !exactKeys(diagnostic, ["code", "detail", "pointer", "line", "column"]) ||
    typeof diagnostic.code !== "string" ||
    diagnostic.code.length < 1 ||
    diagnostic.code.length > 128 ||
    typeof diagnostic.detail !== "string" ||
    diagnostic.detail.length > 512 ||
    ("pointer" in diagnostic && typeof diagnostic.pointer !== "string") ||
    ("line" in diagnostic &&
      (!Number.isSafeInteger(diagnostic.line) ||
        Number(diagnostic.line) < 1)) ||
    ("column" in diagnostic &&
      (!Number.isSafeInteger(diagnostic.column) ||
        Number(diagnostic.column) < 1))
  ) {
    return variableProjectionRejected(
      "A VariableSet diagnostic exceeded the closed bounded response schema.",
    );
  }
  return {
    code: diagnostic.code,
    detail: diagnostic.detail,
    pointer:
      typeof diagnostic.pointer === "string" ? diagnostic.pointer : undefined,
    line: typeof diagnostic.line === "number" ? diagnostic.line : undefined,
    column:
      typeof diagnostic.column === "number" ? diagnostic.column : undefined,
  };
}

function safeVariablePreview(
  value: unknown,
  expectedEnvironmentId: string,
  expectedScope: VariableSetScope,
  expectedPath: string,
): VariableSetPreview {
  const preview = plainRecord(value);
  const pathPattern =
    expectedScope === "project"
      ? /^tenants\/[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\/variables\.yaml$/i
      : new RegExp(
          `^tenants/[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}/environments/${expectedEnvironmentId}/variables\\.yaml$`,
          "i",
        );
  if (
    !preview ||
    !exactKeys(preview, [
      "previewToken",
      "scope",
      "path",
      "gitDiff",
      "document",
      "diagnostics",
      "expiresAt",
    ]) ||
    typeof preview.previewToken !== "string" ||
    !/^[A-Za-z0-9_-]{43}$/.test(preview.previewToken) ||
    preview.scope !== expectedScope ||
    !variableUUIDPattern.test(expectedEnvironmentId) ||
    typeof preview.path !== "string" ||
    preview.path.length > 1024 ||
    !pathPattern.test(preview.path) ||
    preview.path !== expectedPath ||
    typeof preview.gitDiff !== "string" ||
    preview.gitDiff.length > 65536 ||
    !Array.isArray(preview.diagnostics) ||
    preview.diagnostics.length > 64 ||
    typeof preview.expiresAt !== "string" ||
    preview.expiresAt.length > 64 ||
    !Number.isFinite(Date.parse(preview.expiresAt))
  ) {
    return variableProjectionRejected(
      "The VariableSet preview identity, token, diff, or expiry was invalid.",
    );
  }
  return {
    previewToken: preview.previewToken,
    scope: expectedScope,
    path: preview.path,
    gitDiff: preview.gitDiff,
    document: safeVariableDocument(preview.document),
    diagnostics: preview.diagnostics.map(safeVariableDiagnostic),
    expiresAt: preview.expiresAt,
  };
}

function safeBuildArgument(argument: BuildArgument): BuildArgument {
  return { name: argument.name, value: argument.value };
}

function safeBuildFileReference(
  reference: BuildFileReference,
): BuildFileReference {
  return { id: reference.id, path: reference.path };
}

function safeBuildProfile(profile: BuildProfile): BuildProfile {
  return {
    resource: profile.resource,
    timeoutSeconds: profile.timeoutSeconds,
    egress: profile.egress,
  };
}

function safeBuildDefinition(definition: BuildDefinition): BuildDefinition {
  return {
    id: definition.id,
    projectId: definition.projectId,
    applicationId: definition.applicationId,
    installationId: definition.installationId,
    repositoryId: definition.repositoryId,
    triggerRef: definition.triggerRef,
    contextPath: definition.contextPath,
    dockerfilePath: definition.dockerfilePath,
    platforms: [...(definition.platforms ?? [])].slice(0, 2),
    registry: {
      targetId: definition.registry.targetId,
      mode: definition.registry.mode,
      server: definition.registry.server,
      repositoryPrefix: definition.registry.repositoryPrefix,
    },
    buildArgs: (definition.buildArgs ?? []).slice(0, 64).map(safeBuildArgument),
    secretFiles: (definition.secretFiles ?? [])
      .slice(0, 32)
      .map(safeBuildFileReference),
    sshFiles: (definition.sshFiles ?? [])
      .slice(0, 8)
      .map(safeBuildFileReference),
    cacheTrustLane: definition.cacheTrustLane,
    cacheImports: definition.cacheImports,
    profile: safeBuildProfile(definition.profile),
    maxAttempts: definition.maxAttempts,
    definitionDigest: definition.definitionDigest,
    definitionGeneration: definition.definitionGeneration,
    enabled: definition.enabled,
    createdAt: definition.createdAt,
    updatedAt: definition.updatedAt,
  };
}

function safeBuildAttempt(attempt: BuildAttempt): BuildAttempt {
  return {
    id: attempt.id,
    definitionId: attempt.definitionId,
    projectId: attempt.projectId,
    applicationId: attempt.applicationId,
    commitSha: attempt.commitSha,
    gitRef: attempt.gitRef,
    generation: attempt.generation,
    state: attempt.state,
    executionAttempts: attempt.executionAttempts,
    maxAttempts: attempt.maxAttempts,
    image: attempt.image
      ? {
          reference: attempt.image.reference,
          digest: attempt.image.digest,
          platforms: [...(attempt.image.platforms ?? [])].slice(0, 2),
        }
      : undefined,
    cacheReuse: [
      "not-requested",
      "unavailable",
      "hit",
      "miss",
      "unknown",
    ].includes(attempt.cacheReuse ?? "")
      ? attempt.cacheReuse
      : undefined,
    warnings: [...(attempt.warnings ?? [])].slice(0, 8),
    cacheReference: attempt.cacheReference,
    failureCode: attempt.failureCode,
    cancelRequestedAt: attempt.cancelRequestedAt,
    startedAt: attempt.startedAt,
    completedAt: attempt.completedAt,
    createdAt: attempt.createdAt,
    updatedAt: attempt.updatedAt,
  };
}

function safeBuildLogLine(line: BuildLogLine): BuildLogLine {
  return {
    type: "line",
    timestamp: line.timestamp,
    source: {
      id: line.source.id,
      ready: line.source.ready === true,
      previous: line.source.previous === true,
    },
    message: String(line.message ?? "").slice(0, 262_144),
    truncated: line.truncated === true,
    cursor: line.cursor
      ? {
          sourceId: line.cursor.sourceId,
          timestamp: line.cursor.timestamp,
          fingerprint: line.cursor.fingerprint,
        }
      : undefined,
  };
}

function safeBuildLogSnapshot(snapshot: BuildLogSnapshot): BuildLogSnapshot {
  return {
    source: {
      id: snapshot.source.id,
      ready: snapshot.source.ready === true,
      previous: snapshot.source.previous === true,
    },
    lines: (snapshot.lines ?? []).slice(0, 2_000).map(safeBuildLogLine),
    bytes: Math.max(0, Math.min(5 << 20, Number(snapshot.bytes) || 0)),
    truncated: snapshot.truncated === true,
    observedAt: snapshot.observedAt,
  };
}

function buildLogQuery(options: BuildLogOptions, follow: boolean): string {
  const query = new URLSearchParams();
  query.set("follow", String(follow));
  query.set(
    "tailLines",
    String(Math.max(1, Math.min(2_000, Math.trunc(options.tailLines ?? 200)))),
  );
  query.set(
    "limitBytes",
    String(
      Math.max(1, Math.min(5 << 20, Math.trunc(options.limitBytes ?? 1 << 20))),
    ),
  );
  if (options.since) query.set("since", options.since);
  if (!follow && options.previous) query.set("previous", "true");
  return query.toString();
}

function safeCreateBuildDefinition(
  input: CreateBuildDefinition,
): CreateBuildDefinition {
  return {
    installationId: input.installationId,
    repositoryId: input.repositoryId,
    registryTargetId: input.registryTargetId,
    triggerRef: input.triggerRef,
    contextPath: input.contextPath,
    dockerfilePath: input.dockerfilePath,
    platforms: [...input.platforms].slice(0, 2),
    buildArgs: input.buildArgs?.slice(0, 64).map(safeBuildArgument),
    cacheTrustLane: input.cacheTrustLane,
    cacheImports: input.cacheImports,
    profile: safeBuildProfile(input.profile),
    maxAttempts: input.maxAttempts,
  };
}

function safeRuntimeSecretDelivery(
  delivery: RuntimeSecretDelivery,
): RuntimeSecretDelivery {
  return delivery.kind === "environment"
    ? {
        sourceKey: delivery.sourceKey,
        kind: "environment",
        environmentName: delivery.environmentName,
      }
    : {
        sourceKey: delivery.sourceKey,
        kind: "file",
        filePath: delivery.filePath,
        fileMode: delivery.fileMode,
      };
}

function safeRuntimeSecretMetadata(
  binding: RuntimeSecretBindingMetadata,
): RuntimeSecretBindingMetadata {
  return {
    id: binding.id,
    applicationId: binding.applicationId,
    environmentId: binding.environmentId,
    name: binding.name,
    provider: binding.provider,
    state: binding.state,
    activeVersion: binding.activeVersion,
    createdBy: binding.createdBy,
    createdAt: binding.createdAt,
    updatedAt: binding.updatedAt,
    deleteStartedAt: binding.deleteStartedAt,
    deletedAt: binding.deletedAt,
  };
}

function safeRuntimeSecretDetail(
  binding: RuntimeSecretBindingDetail,
): RuntimeSecretBindingDetail {
  return {
    ...safeRuntimeSecretMetadata(binding),
    versions: binding.versions.map((version) => ({
      id: version.id,
      number: version.number,
      state: version.state,
      deliveries: version.deliveries.map(safeRuntimeSecretDelivery),
      failureCode: version.failureCode,
      stagedAt: version.stagedAt,
      readinessObservedAt: version.readinessObservedAt,
      activatedAt: version.activatedAt,
      retainedAt: version.retainedAt,
      createdAt: version.createdAt,
      updatedAt: version.updatedAt,
    })),
  };
}

function safeCertificateMetadata(
  binding: CertificateBindingMetadata,
): CertificateBindingMetadata {
  return {
    id: binding.id,
    applicationId: binding.applicationId,
    environmentId: binding.environmentId,
    name: binding.name,
    state: binding.state,
    activeVersion: binding.activeVersion,
    createdBy: binding.createdBy,
    createdAt: binding.createdAt,
    updatedAt: binding.updatedAt,
    deleteStartedAt: binding.deleteStartedAt,
    deletedAt: binding.deletedAt,
  };
}

function safeCertificateDetail(
  binding: CertificateBindingDetail,
): CertificateBindingDetail {
  return {
    ...safeCertificateMetadata(binding),
    versions: (binding.versions ?? []).slice(0, 256).map((version) => ({
      number: version.number,
      leafFingerprint: version.leafFingerprint,
      publicKeyFingerprint: version.publicKeyFingerprint,
      dnsNames: (version.dnsNames ?? []).slice(0, 128),
      ipAddresses: (version.ipAddresses ?? []).slice(0, 128),
      notBefore: version.notBefore,
      notAfter: version.notAfter,
      createdBy: version.createdBy,
      createdAt: version.createdAt,
    })),
  };
}

function safeSSLIPHostnamePreview(
  preview: SSLIPHostnamePreview,
): SSLIPHostnamePreview {
  return {
    mode: "sslip",
    hostname: preview.hostname,
    source: preview.source,
    observedAt: preview.observedAt,
  };
}

function safeCertificateIssuerCatalog(
  value: unknown,
): CertificateIssuerCatalog {
  const catalog = plainRecord(value);
  if (
    !catalog ||
    !exactKeys(catalog, ["items"]) ||
    !Array.isArray(catalog.items) ||
    catalog.items.length > 100
  ) {
    throw new Error("The certificate issuer catalog response was invalid.");
  }
  const seen = new Set<string>();
  const items = catalog.items.map((raw) => {
    const item = plainRecord(raw);
    const solverTypes = item?.solverTypes;
    if (
      !item ||
      !exactKeys(item, [
        "name",
        "environment",
        "solverTypes",
        "source",
        "revision",
      ]) ||
      typeof item.name !== "string" ||
      !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(item.name) ||
      item.name.length > 253 ||
      seen.has(item.name) ||
      (item.environment !== "production" && item.environment !== "staging") ||
      !Array.isArray(solverTypes) ||
      solverTypes.length < 1 ||
      solverTypes.length > 2 ||
      solverTypes.some(
        (solver) =>
          solver !== "http01" &&
          solver !== "dns01" &&
          solver !== "dns01-cloudflare",
      ) ||
      new Set(solverTypes).size !== solverTypes.length ||
      (item.source !== "bootstrap" && item.source !== "managed") ||
      (item.source === "bootstrap" && item.revision !== undefined) ||
      (item.source === "managed" &&
        (!Number.isSafeInteger(item.revision) || Number(item.revision) < 1))
    ) {
      throw new Error("The certificate issuer catalog response was invalid.");
    }
    seen.add(item.name);
    return {
      name: item.name,
      environment:
        item.environment as CertificateIssuerCatalog["items"][number]["environment"],
      solverTypes: [
        ...solverTypes,
      ] as CertificateIssuerCatalog["items"][number]["solverTypes"],
      source:
        item.source as CertificateIssuerCatalog["items"][number]["source"],
      ...(item.source === "managed" ? { revision: Number(item.revision) } : {}),
    };
  });
  return { items };
}

function safeCertificateIssuerAdminEntry(
  value: unknown,
): CertificateIssuerAdminEntry {
  const entry = plainRecord(value);
  const revision = plainRecord(entry?.revision);
  const observation = plainRecord(entry?.observation);
  const dnsZones = revision?.dnsZones;
  const lifecycle = entry?.lifecycle;
  const solver = revision?.solver;
  const isDate = (candidate: unknown) =>
    typeof candidate === "string" && !Number.isNaN(Date.parse(candidate));
  const isSecretName = (candidate: unknown) =>
    typeof candidate === "string" &&
    /^[a-z0-9](?:[-a-z0-9]{0,251}[a-z0-9])?$/.test(candidate);
  if (
    !entry ||
    !revision ||
    !observation ||
    !exactKeys(entry, [
      "id",
      "name",
      "lifecycle",
      "currentRevision",
      "revision",
      "observation",
      "createdAt",
      "deactivatedAt",
    ]) ||
    !exactKeys(revision, [
      "number",
      "environment",
      "email",
      "accountPrivateKeySecretName",
      "solver",
      "dnsZones",
      "apiTokenSecretName",
      "apiTokenSecretKey",
      "specDigest",
      "createdAt",
    ]) ||
    !exactKeys(observation, [
      "state",
      "observedGeneration",
      "reason",
      "observedAt",
      "updatedAt",
    ]) ||
    typeof entry.id !== "string" ||
    !variableUUIDPattern.test(entry.id) ||
    typeof entry.name !== "string" ||
    !/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(entry.name) ||
    (lifecycle !== "active" && lifecycle !== "deactivated") ||
    !Number.isSafeInteger(entry.currentRevision) ||
    Number(entry.currentRevision) < 1 ||
    !Number.isSafeInteger(revision.number) ||
    revision.number !== entry.currentRevision ||
    (revision.environment !== "production" &&
      revision.environment !== "staging") ||
    typeof revision.email !== "string" ||
    revision.email.length < 3 ||
    revision.email.length > 254 ||
    !revision.email.includes("@") ||
    !isSecretName(revision.accountPrivateKeySecretName) ||
    (solver !== "http01" && solver !== "dns01-cloudflare") ||
    typeof revision.specDigest !== "string" ||
    !/^sha256:[0-9a-f]{64}$/.test(revision.specDigest) ||
    !isDate(revision.createdAt) ||
    (observation.state !== "pending" &&
      observation.state !== "ready" &&
      observation.state !== "degraded") ||
    (observation.observedGeneration !== undefined &&
      (!Number.isSafeInteger(observation.observedGeneration) ||
        Number(observation.observedGeneration) < 1)) ||
    (observation.reason !== undefined &&
      (typeof observation.reason !== "string" ||
        observation.reason.length > 1024)) ||
    (observation.observedAt !== undefined && !isDate(observation.observedAt)) ||
    !isDate(observation.updatedAt) ||
    !isDate(entry.createdAt) ||
    (lifecycle === "active" && entry.deactivatedAt !== undefined) ||
    (lifecycle === "deactivated" && !isDate(entry.deactivatedAt)) ||
    (solver === "http01" &&
      (dnsZones !== undefined ||
        revision.apiTokenSecretName !== undefined ||
        revision.apiTokenSecretKey !== undefined)) ||
    (solver === "dns01-cloudflare" &&
      (!Array.isArray(dnsZones) ||
        dnsZones.length < 1 ||
        dnsZones.length > 64 ||
        dnsZones.some(
          (zone) =>
            typeof zone !== "string" ||
            zone.length > 253 ||
            !/^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/.test(zone),
        ) ||
        new Set(dnsZones).size !== dnsZones.length ||
        !isSecretName(revision.apiTokenSecretName) ||
        typeof revision.apiTokenSecretKey !== "string" ||
        !/^[A-Za-z0-9._-]{1,253}$/.test(revision.apiTokenSecretKey)))
  ) {
    throw new Error(
      "The certificate issuer administration response was invalid.",
    );
  }
  return {
    id: entry.id,
    name: entry.name,
    lifecycle,
    currentRevision: Number(entry.currentRevision),
    revision: {
      number: Number(revision.number),
      environment: revision.environment,
      email: revision.email,
      accountPrivateKeySecretName: revision.accountPrivateKeySecretName,
      solver,
      ...(solver === "dns01-cloudflare"
        ? {
            dnsZones: [...(dnsZones as string[])],
            apiTokenSecretName: revision.apiTokenSecretName as string,
            apiTokenSecretKey: revision.apiTokenSecretKey as string,
          }
        : {}),
      specDigest: revision.specDigest,
      createdAt: revision.createdAt,
    },
    observation: {
      state: observation.state,
      ...(observation.observedGeneration !== undefined
        ? { observedGeneration: Number(observation.observedGeneration) }
        : {}),
      ...(observation.reason !== undefined
        ? { reason: observation.reason as string }
        : {}),
      ...(observation.observedAt !== undefined
        ? { observedAt: observation.observedAt as string }
        : {}),
      updatedAt: observation.updatedAt,
    },
    createdAt: entry.createdAt,
    ...(entry.deactivatedAt !== undefined
      ? { deactivatedAt: entry.deactivatedAt as string }
      : {}),
  } as CertificateIssuerAdminEntry;
}

function safeCertificateIssuerMutation(input: CertificateIssuerMutation) {
  const base = {
    environment: input.environment,
    email: input.email,
    accountPrivateKeySecretName: input.accountPrivateKeySecretName,
  };
  return input.solver.type === "http01"
    ? { ...base, solver: { type: "http01" as const } }
    : {
        ...base,
        solver: {
          type: "dns01-cloudflare" as const,
          dnsZones: [...input.solver.dnsZones],
          apiTokenSecretName: input.solver.apiTokenSecretName,
          apiTokenSecretKey: input.solver.apiTokenSecretKey,
        },
      };
}

function safeImageResolutionPreview(
  preview: ImageResolutionPreview,
): ImageResolutionPreview {
  const requestedIsImmutable = isCanonicalImmutableImage(
    preview.requestedImage,
  );
  const requestedIsTag = isCanonicalTaggedImage(preview.requestedImage);
  if (
    (!requestedIsImmutable && !requestedIsTag) ||
    !isCanonicalImmutableImage(preview.immutableImage) ||
    typeof preview.resolved !== "boolean" ||
    (preview.resolved
      ? !requestedIsTag || preview.requestedImage === preview.immutableImage
      : !requestedIsImmutable ||
        preview.requestedImage !== preview.immutableImage)
  ) {
    throw new Error("The image resolution response was invalid.");
  }
  return {
    requestedImage: preview.requestedImage,
    immutableImage: preview.immutableImage,
    resolved: preview.resolved,
  };
}

function safeCreateDeploymentInput(input: CreateDeployment): CreateDeployment {
  const immutable = isCanonicalImmutableImage(input.image);
  const tagged = isCanonicalTaggedImage(input.image);
  if (
    (!immutable && !tagged) ||
    (immutable && input.expectedImmutableImage !== undefined) ||
    (tagged && !isCanonicalImmutableImage(input.expectedImmutableImage ?? ""))
  ) {
    throw new Error(
      "A tag requires its previewed immutable-image precondition, while an immutable image forbids it.",
    );
  }
  return {
    environmentId: input.environmentId,
    applicationId: input.applicationId,
    image: input.image,
    ...(tagged ? { expectedImmutableImage: input.expectedImmutableImage } : {}),
    runtime: input.runtime,
    ...(input.route ? { route: input.route } : {}),
  };
}

function safeRegistryTarget(target: RegistryTarget): RegistryTarget {
  return {
    id: target.id,
    name: target.name,
    mode: target.mode,
    endpoint: target.endpoint,
    repositoryPrefix: target.repositoryPrefix,
    pullCredentialRef: target.pullCredentialRef,
    pushCredentialRef: target.pushCredentialRef,
    cacheCredentialRef: target.cacheCredentialRef,
    createdAt: target.createdAt,
    updatedAt: target.updatedAt,
  };
}

function safeProjectRegistryPullCredential(
  value: ProjectRegistryPullCredential,
): ProjectRegistryPullCredential {
  return {
    id: value.id,
    projectId: value.projectId,
    registryTargetId: value.registryTargetId,
    name: value.name,
    registryName: value.registryName,
    registryServer: value.registryServer,
    repositoryPrefix: value.repositoryPrefix,
    createdAt: value.createdAt,
    updatedAt: value.updatedAt,
  };
}

function safeExternalDNSIntegration(
  integration: ExternalDNSIntegration,
): ExternalDNSIntegration {
  return {
    id: integration.id,
    slug: integration.slug,
    name: integration.name,
    mode: integration.mode,
    providerKind: integration.providerKind,
    txtOwnerId: integration.txtOwnerId,
    allowedDomainSuffixes: [...(integration.allowedDomainSuffixes ?? [])],
    syncPolicy: integration.syncPolicy,
    destructiveSyncConfirmed: integration.destructiveSyncConfirmed === true,
    credentialSecretRef: integration.credentialSecretRef,
    providerConfigRef: integration.providerConfigRef,
    egressConfigRef: integration.egressConfigRef,
    operatorProfileRef: integration.operatorProfileRef,
    environmentIds: [...(integration.environmentIds ?? [])],
    createdBy: integration.createdBy,
    createdAt: integration.createdAt,
    updatedAt: integration.updatedAt,
    runtimeRevision: integration.runtimeRevision,
    lifecycle: integration.lifecycle,
    deactivatedAt: integration.deactivatedAt,
    protectedGitState: integration.protectedGitState,
    protectedGitRevision: integration.protectedGitRevision,
    protectedGitObservedAt: integration.protectedGitObservedAt,
  };
}

function safeExternalDNSCatalogItem(
  item: ExternalDNSCatalogItem,
  catalogReady: boolean,
): ExternalDNSCatalogItem {
  return {
    id: item.id,
    slug: item.slug,
    name: item.name,
    mode: item.mode,
    providerKind: item.providerKind,
    allowedDomainSuffixes: [...(item.allowedDomainSuffixes ?? [])],
    runtimeRevision: item.runtimeRevision,
    runtimeAvailable: catalogReady && item.runtimeAvailable === true,
  };
}

function safeExternalDNSCatalog(
  response: ExternalDNSCatalog,
  limit: number,
): ExternalDNSCatalog {
  const items = response.items ?? [];
  const ready =
    response.controllerReadiness === "ready" &&
    response.runtimeAvailable === true;
  return {
    items: items
      .slice(0, limit)
      .map((item) => safeExternalDNSCatalogItem(item, ready)),
    truncated: response.truncated === true || items.length > limit,
    configurationState:
      response.configurationState === "configured" ? "configured" : "empty",
    controllerReadiness: ready ? "ready" : "unobserved",
    runtimeAvailable: ready,
  };
}

function safeExternalDNSStatus(status: ExternalDNSStatus): ExternalDNSStatus {
  const ready =
    status.controllerReadiness === "ready" && status.runtimeAvailable === true;
  return {
    configurationState:
      status.configurationState === "configured" ? "configured" : "empty",
    controllerReadiness: ready ? "ready" : "unobserved",
    runtimeAvailable: ready,
    detail: status.detail,
  };
}

function safeRegistryPolicy(policy: RegistryPolicy): RegistryPolicy {
  return {
    registryTargetId: policy.registryTargetId,
    serviceId: policy.serviceId,
    repository: policy.repository,
    keepLastSuccessful: policy.keepLastSuccessful,
    minimumSafetyAgeSeconds: policy.minimumSafetyAgeSeconds,
    cacheKeepGenerations: policy.cacheKeepGenerations,
    cacheUnusedExpirySeconds: policy.cacheUnusedExpirySeconds,
    cacheByteQuota: policy.cacheByteQuota,
    createdAt: policy.createdAt,
    updatedAt: policy.updatedAt,
  };
}

function safeRegistryCatalogObservation(
  observation: RegistryCatalogObservation,
): RegistryCatalogObservation {
  return {
    repository: observation.repository,
    revision: observation.revision,
    complete: observation.complete,
    observedAt: observation.observedAt,
    manifestCount: observation.manifestCount,
    blobCount: observation.blobCount,
  };
}

function safeRegistryRelease(release: RegistryRelease): RegistryRelease {
  return {
    id: release.id,
    repository: release.repository,
    rootDigest: release.rootDigest,
    createdAt: release.createdAt,
    succeededAt: release.succeededAt,
    availability: release.availability,
    availabilityObservedAt: release.availabilityObservedAt,
  };
}

function safeRegistryCacheGeneration(
  generation: RegistryCacheGeneration,
): RegistryCacheGeneration {
  return {
    id: generation.id,
    repository: generation.repository,
    platformSet: generation.platformSet,
    trustLane: generation.trustLane,
    cacheSchema: generation.cacheSchema,
    buildDefinitionHash: generation.buildDefinitionHash,
    generation: generation.generation,
    rootDigest: generation.rootDigest,
    sizeBytes: generation.sizeBytes,
    state: generation.state,
    activeImports: generation.activeImports,
    activeExports: generation.activeExports,
    createdAt: generation.createdAt,
    completedAt: generation.completedAt,
    lastUsedAt: generation.lastUsedAt,
  };
}

function boundedRegistryLimit(value: number) {
  if (!Number.isFinite(value)) return 50;
  return Math.min(100, Math.max(1, Math.trunc(value)));
}

function boundedDeploymentRollbackLimit(value: number) {
  if (!Number.isFinite(value)) return 25;
  return Math.min(100, Math.max(1, Math.trunc(value)));
}

function safeDeploymentRollbackCandidate(
  value: DeploymentRollbackCandidate,
): DeploymentRollbackCandidate | undefined {
  if (
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
      value.sourceOperationId,
    ) ||
    !Number.isSafeInteger(value.generation) ||
    value.generation < 1 ||
    !/^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$/.test(value.image) ||
    (value.artifactAssurance !== "managed-release-verified" &&
      value.artifactAssurance !== "external-digest-unverified") ||
    value.managedReleaseVerified !==
      (value.artifactAssurance === "managed-release-verified") ||
    Number.isNaN(Date.parse(value.createdAt))
  ) {
    return undefined;
  }
  return {
    sourceOperationId: value.sourceOperationId,
    generation: value.generation,
    image: value.image,
    artifactAssurance: value.artifactAssurance,
    managedReleaseVerified: value.managedReleaseVerified,
    createdAt: value.createdAt,
  };
}

function safeApplicationRegistryTarget(
  value: ApplicationRegistryTarget,
  limit: number,
): ApplicationRegistryTarget {
  const repositories = value.inventory?.repositories ?? [];
  const catalogObservations = value.catalogObservations ?? [];
  const releases = value.releases ?? [];
  const cacheGenerations = value.cacheGenerations ?? [];
  return {
    target: safeRegistryTarget(value.target),
    policy: safeRegistryPolicy(value.policy),
    inventory: value.inventory
      ? {
          revision: value.inventory.revision,
          complete: value.inventory.complete,
          repositories: repositories.slice(0, limit),
          repositoriesTruncated:
            value.inventory.repositoriesTruncated === true ||
            repositories.length > limit,
          observedAt: value.inventory.observedAt,
        }
      : undefined,
    catalogObservations: catalogObservations
      .slice(0, limit)
      .map(safeRegistryCatalogObservation),
    catalogTruncated:
      value.catalogTruncated === true || catalogObservations.length > limit,
    releases: releases.slice(0, limit).map(safeRegistryRelease),
    releasesTruncated:
      value.releasesTruncated === true || releases.length > limit,
    cacheGenerations: cacheGenerations
      .slice(0, limit)
      .map(safeRegistryCacheGeneration),
    cacheGenerationsTruncated:
      value.cacheGenerationsTruncated === true ||
      cacheGenerations.length > limit,
    observedAt: value.observedAt,
  };
}

function safeRegistryCleanupItem(
  item: RegistryCleanupItem,
): RegistryCleanupItem {
  return {
    ordinal: item.ordinal,
    repository: item.repository,
    resourceKind: item.resourceKind,
    digest: item.digest,
    disposition: item.disposition,
    action: item.action,
    estimatedBytes: item.estimatedBytes,
    reasons: [...(item.reasons ?? [])],
    state: item.state,
    providerMessage: item.providerMessage,
    updatedAt: item.updatedAt,
  };
}

function safeRegistryCleanupPlan(
  plan: RegistryCleanupPlan,
): RegistryCleanupPlan {
  const items = plan.items ?? [];
  return {
    id: plan.id,
    registryTargetId: plan.registryTargetId,
    serviceId: plan.serviceId,
    planDigest: plan.planDigest,
    state: plan.state,
    policy: safeRegistryPolicy(plan.policy),
    summary: {
      protectedManifests: plan.summary.protectedManifests,
      deletedManifests: plan.summary.deletedManifests,
      garbageCollectBlobs: plan.summary.garbageCollectBlobs,
      estimatedBytes: plan.summary.estimatedBytes,
      cacheBytesBefore: plan.summary.cacheBytesBefore,
      cacheBytesAfter: plan.summary.cacheBytesAfter,
      cacheQuotaSatisfied: plan.summary.cacheQuotaSatisfied,
    },
    items: items.slice(0, 100).map(safeRegistryCleanupItem),
    itemsTruncated: plan.itemsTruncated === true || items.length > 100,
    createdAt: plan.createdAt,
    claimedAt: plan.claimedAt,
    completedAt: plan.completedAt,
    failure: plan.failure,
  };
}

const maximumHelmValuesBytes = 262_144;

function boundedHelmLimit(value: number) {
  if (!Number.isInteger(value) || value < 1 || value > 100) {
    throw new ApiError(400, {
      title: "Invalid Helm history limit",
      detail: "The Helm result limit must be an integer from 1 through 100.",
    });
  }
  return value;
}

function safeHelmApproval(value: HelmApproval): HelmApproval {
  return {
    id: value.id,
    revision: value.revision,
    repository: value.repository,
    version: value.version,
    manifestDigest: value.manifestDigest,
    packageDigest: value.packageDigest,
    valuesSchemaDigest: value.valuesSchemaDigest,
    rendererImage: value.rendererImage,
    rendererVersion: value.rendererVersion,
    policyVersion: value.policyVersion,
    documentsDigest: value.documentsDigest,
    valuesSchema: value.valuesSchema,
    defaultValuesYaml: value.defaultValuesYaml,
    createdAt: value.createdAt,
  };
}

function safeAssignedMiddlewareProfile(
  value: AssignedMiddlewareProfile,
): AssignedMiddlewareProfile {
  return {
    profileId: value.profileId,
    name: value.name,
    revision: value.revision,
    specDigest: value.specDigest,
    assignmentsDigest: value.assignmentsDigest,
    spec: structuredClone(value.spec),
  };
}

function safeHelmRenderedPreview(
  value: HelmRenderedPreview,
): HelmRenderedPreview {
  const resources = Array.isArray(value.resources) ? value.resources : [];
  if (
    !Number.isSafeInteger(value.generation) ||
    value.generation < 1 ||
    !Number.isSafeInteger(value.resourceCount) ||
    value.resourceCount < 0 ||
    value.resourceCount > 128 ||
    !Number.isSafeInteger(value.previewBytes) ||
    value.previewBytes < 0 ||
    value.previewBytes > 262_144 ||
    resources.length !== value.resourceCount
  ) {
    throw new ApiError(502, {
      title: "Invalid rendered inventory",
      detail:
        "The rendered inventory response failed its bounded consistency checks.",
    });
  }
  let sanitizedBytes = 0;
  for (const resource of resources) {
    const yaml = resource.sanitizedYaml;
    if (
      typeof resource.previewOmitted !== "boolean" ||
      (resource.previewOmitted && yaml !== undefined && yaml !== "") ||
      (!resource.previewOmitted &&
        (typeof yaml !== "string" ||
          yaml.length === 0 ||
          new TextEncoder().encode(yaml).byteLength > 32_768))
    ) {
      throw new ApiError(502, {
        title: "Invalid rendered preview",
        detail: "A rendered resource violated the sanitized YAML boundary.",
      });
    }
    if (!resource.previewOmitted && yaml) {
      sanitizedBytes += new TextEncoder().encode(yaml).byteLength;
    }
  }
  if (sanitizedBytes !== value.previewBytes) {
    throw new ApiError(502, {
      title: "Invalid rendered preview",
      detail: "The rendered preview byte count failed its consistency check.",
    });
  }
  return {
    releaseRevisionId: value.releaseRevisionId,
    generation: value.generation,
    manifestDigest: value.manifestDigest,
    inventoryDigest: value.inventoryDigest,
    resourceCount: value.resourceCount,
    previewBytes: value.previewBytes,
    resources: resources.map((resource) => ({
      apiVersion: resource.apiVersion,
      kind: resource.kind,
      namespace: resource.namespace,
      name: resource.name,
      sanitizedYaml: resource.previewOmitted
        ? undefined
        : resource.sanitizedYaml,
      previewOmitted: Boolean(resource.previewOmitted),
    })),
  };
}

function safeCreateHelmApproval(value: CreateHelmApproval): CreateHelmApproval {
  return {
    repository: value.repository,
    version: value.version,
    manifestDigest: value.manifestDigest,
    packageDigest: value.packageDigest,
    valuesSchemaDigest: value.valuesSchemaDigest,
  };
}

function safeHelmReleaseRevision(
  value: HelmReleaseRevision,
): HelmReleaseRevision {
  return {
    id: value.id,
    generation: value.generation,
    releaseName: value.releaseName,
    action: value.action,
    desiredEnabled: value.desiredEnabled,
    parentRevisionId: value.parentRevisionId,
    rollbackSourceRevisionId: value.rollbackSourceRevisionId,
    approval: { id: value.approval.id, revision: value.approval.revision },
    renderCommandId: value.renderCommandId,
    valuesDigest: value.valuesDigest,
    intentDigest: value.intentDigest,
    requestId: value.requestId,
    createdAt: value.createdAt,
  };
}

function safeHelmReleaseStatus(value: HelmReleaseStatus): HelmReleaseStatus {
  return {
    revision: safeHelmReleaseRevision(value.revision),
    phase: value.phase,
    renderState: value.renderState,
    payloadIntentId: value.payloadIntentId,
    payloadState: value.payloadState,
    payloadRevision: value.payloadRevision,
    applicationIntentId: value.applicationIntentId,
    applicationState: value.applicationState,
    applicationRevision: value.applicationRevision,
    failureCode: value.failureCode,
  };
}

function safeHelmValuesInput(value: HelmValuesInput): HelmValuesInput {
  if (
    new TextEncoder().encode(value.valuesYaml).byteLength >
    maximumHelmValuesBytes
  ) {
    throw new ApiError(413, {
      title: "Helm values are too large",
      detail: "values.yaml must not exceed 262144 UTF-8 bytes.",
    });
  }
  return {
    approvalId: value.approvalId,
    approvalRevision: value.approvalRevision,
    valuesYaml: value.valuesYaml,
  };
}

function helmTargetPath(applicationId: string, environmentId: string) {
  return `/v1/applications/${encodeURIComponent(applicationId)}/environments/${encodeURIComponent(environmentId)}/helm`;
}

function helmMutation(
  path: string,
  method: "POST" | "PUT",
  body: unknown,
  idempotencyKey: string,
): Promise<HelmMutationResult> {
  let replayed = false;
  return request<HelmReleaseRevision>(path, {
    method,
    headers: { "Idempotency-Key": idempotencyKey },
    body,
    responseMetadata: (response) => {
      replayed = response.headers.get("Idempotent-Replay") === "true";
    },
  }).then((revision) => ({
    revision: safeHelmReleaseRevision(revision),
    replayed,
  }));
}

export const api = {
  meta: () => request<ApiMeta>("/v1/meta"),
  me: () => request<Principal>("/v1/me"),
  capabilities: () => request<Capabilities>("/v1/capabilities"),
  auditEvents: (
    query: {
      targetType?: string;
      targetId?: string;
      action?: string;
      limit?: number;
    } = {},
  ) => {
    const parameters = new URLSearchParams();
    if (query.targetType) parameters.set("targetType", query.targetType);
    if (query.targetId) parameters.set("targetId", query.targetId);
    if (query.action) parameters.set("action", query.action);
    if (query.limit) parameters.set("limit", String(query.limit));
    const suffix = parameters.size ? `?${parameters.toString()}` : "";
    return request<AuditEventList>(`/v1/audit-events${suffix}`);
  },
  bootstrap: (input: {
    token: string;
    displayName: string;
    password: string;
  }) => request<User>("/v1/auth/bootstrap", { method: "POST", body: input }),
  login: (input: { login: string; password: string }) =>
    request<User>("/v1/auth/login", { method: "POST", body: input }),
  acceptInvitation: (input: {
    token: string;
    displayName: string;
    password: string;
  }) =>
    request<User>("/v1/auth/invitations/accept", {
      method: "POST",
      body: input,
    }),
  logout: () => request<void>("/v1/auth/logout", { method: "POST" }),

  users: () =>
    request<Collection<User> | User[]>("/v1/users").then(asCollection),
  createInvitation: (input: { displayName: string }) =>
    request<UserInvitation>("/v1/users/invitations", {
      method: "POST",
      headers: idempotencyHeaders(),
      body: input,
    }),
  teams: () =>
    request<Collection<Team> | Team[]>("/v1/teams").then(asCollection),
  createTeam: (input: { name: string; slug?: string }) =>
    request<Team>("/v1/teams", {
      method: "POST",
      headers: idempotencyHeaders(),
      body: input,
    }),
  teamMembers: (teamId: string) =>
    request<Collection<TeamMember> | TeamMember[]>(
      `/v1/teams/${encodeURIComponent(teamId)}/members`,
    ).then(asCollection),
  addTeamMember: (
    teamId: string,
    input: { userId: string; role: "owner" | "member" },
  ) =>
    request<TeamMember>(`/v1/teams/${encodeURIComponent(teamId)}/members`, {
      method: "POST",
      headers: idempotencyHeaders(),
      body: input,
    }),
  removeTeamMember: (teamId: string, userId: string) =>
    request<void>(
      `/v1/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}`,
      { method: "DELETE" },
    ),
  githubInstallations: () =>
    request<Collection<GitHubInstallation> | GitHubInstallation[]>(
      "/v1/github/installations",
    ).then((response) => {
      const collection = asCollection(response);
      return {
        items: collection.items.slice(0, 500).map(safeGitHubInstallation),
        nextCursor: collection.nextCursor,
      };
    }),
  beginGitHubSetup: (
    input: {
      returnKey: string;
      expectedAccountId?: number;
      existingInstallationId?: number;
    },
    idempotencyKey: string,
  ) =>
    request<GitHubSetupAuthorizationWire>(
      "/v1/github/installations/authorize",
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: {
          returnKey: input.returnKey,
          expectedAccountId: input.expectedAccountId,
          existingInstallationId: input.existingInstallationId,
        },
      },
    ).then(githubSetupDestination),
  githubInstallationRepositories: (installationId: string) =>
    request<Collection<GitHubRepository> | GitHubRepository[]>(
      `/v1/github/installations/${encodeURIComponent(installationId)}/repositories`,
    ).then((response) => {
      const collection = asCollection(response);
      return {
        items: collection.items
          .slice(0, 500)
          .map(safeGitHubRepository)
          .filter((repository) => repository.installationId === installationId),
        nextCursor: collection.nextCursor,
      };
    }),
  completeGitHubSetup: (idempotencyKey: string) =>
    request<LinkedGitHubSetup>("/v1/github/installations/link", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
    }).then((response) => ({
      installation: safeGitHubInstallation(response.installation),
      repositories: (response.repositories ?? [])
        .slice(0, 500)
        .map(safeGitHubRepository)
        .filter(
          (repository) =>
            repository.installationId === response.installation.id,
        ),
    })),
  updateGitHubInstallationSharing: (
    installationId: string,
    input: { visibility: "private" | "team"; teamId?: string },
  ) =>
    request<GitHubInstallation>(
      `/v1/github/installations/${encodeURIComponent(installationId)}/sharing`,
      { method: "PATCH", body: input },
    ),
  platformArgoGitBinding: () =>
    request<PlatformArgoGitBinding>("/v1/platform/argo/git-binding").then(
      safePlatformArgoGitBinding,
    ),
  createPlatformArgoGitBinding: (
    input: CreatePlatformArgoGitBinding,
    idempotencyKey: string,
  ) =>
    request<PlatformArgoGitBinding>("/v1/platform/argo/git-binding", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: {
        installationId: input.installationId,
        repositoryId: input.repositoryId,
        targetRef: input.targetRef,
      },
    }).then(safePlatformArgoGitBinding),
  environmentGitBinding: (environmentId: string) =>
    request<EnvironmentGitBinding>(
      `/v1/environments/${encodeURIComponent(environmentId)}/git-binding`,
    ).then(safeEnvironmentGitBinding),
  createEnvironmentGitBinding: (
    environmentId: string,
    input: CreateEnvironmentGitBinding,
    idempotencyKey: string,
  ) =>
    request<EnvironmentGitBinding>(
      `/v1/environments/${encodeURIComponent(environmentId)}/git-binding`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: {
          installationId: input.installationId,
          repositoryId: input.repositoryId,
          targetRef: input.targetRef,
        },
      },
    ).then(safeEnvironmentGitBinding),
  variableSets: (environmentId: string) =>
    request<unknown>(
      `/v1/environments/${encodeURIComponent(environmentId)}/variable-sets`,
    ).then((value) => safeVariableSnapshots(value, environmentId)),
  previewVariableSet: (
    environmentId: string,
    scope: VariableSetScope,
    rawYaml: string,
    etag: string | undefined,
    expectedPath: string,
  ) =>
    request<unknown>(
      `/v1/environments/${encodeURIComponent(environmentId)}/variable-sets/${scope}/preview`,
      {
        method: "POST",
        headers: etag ? { "If-Match": etag } : undefined,
        body: { rawYaml },
      },
    ).then((value) =>
      safeVariablePreview(value, environmentId, scope, expectedPath),
    ),
  saveVariableSet: (
    environmentId: string,
    scope: VariableSetScope,
    rawYaml: string,
    previewToken: string,
    idempotencyKey: string,
  ) =>
    request<OperationWire>(
      `/v1/environments/${encodeURIComponent(environmentId)}/variable-sets/${scope}`,
      {
        method: "PUT",
        headers: {
          "Preview-Token": previewToken,
          "Idempotency-Key": idempotencyKey,
        },
        body: { rawYaml },
      },
    ).then(normalizeOperation),

  projects: () =>
    request<Collection<Project> | Project[]>("/v1/projects").then(asCollection),
  createProject: (input: { name: string; slug?: string; teamId?: string }) =>
    request<Project>("/v1/projects", {
      method: "POST",
      headers: idempotencyHeaders(),
      body: input,
    }),
  projectAccessGrants: (projectId: string) =>
    request<Collection<AccessGrant> | AccessGrant[]>(
      `/v1/projects/${encodeURIComponent(projectId)}/grants`,
    ).then(asCollection),
  createProjectAccessGrant: (
    projectId: string,
    input: (
      | {
          subjectUserId: string;
          subjectTeamId?: never;
        }
      | {
          subjectTeamId: string;
          subjectUserId?: never;
        }
    ) & {
      role: Exclude<AccessRole, "platform-admin">;
      scopeType: Exclude<AccessScopeType, "platform">;
      scopeId: string;
      permissions?: Array<"logs.read">;
    },
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<AccessGrant>(
      `/v1/projects/${encodeURIComponent(projectId)}/grants`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  deleteProjectAccessGrant: (
    projectId: string,
    grantId: string,
    idempotencyKey: string,
  ) =>
    request<void>(
      `/v1/projects/${encodeURIComponent(projectId)}/grants/${encodeURIComponent(grantId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": idempotencyKey },
      },
    ),
  serviceAccounts: (projectId: string) =>
    request<Collection<ServiceAccount> | ServiceAccount[]>(
      `/v1/projects/${encodeURIComponent(projectId)}/service-accounts`,
    ).then(asCollection),
  createServiceAccount: (
    projectId: string,
    input: { name: string; role: ServiceAccountRole },
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<ServiceAccount>(
      `/v1/projects/${encodeURIComponent(projectId)}/service-accounts`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  disableServiceAccount: (
    serviceAccountId: string,
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<void>(
      `/v1/service-accounts/${encodeURIComponent(serviceAccountId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": idempotencyKey },
      },
    ),
  serviceAccountTokens: (serviceAccountId: string) =>
    request<Collection<ServiceAccountToken> | ServiceAccountToken[]>(
      `/v1/service-accounts/${encodeURIComponent(serviceAccountId)}/tokens`,
    ).then(asCollection),
  createServiceAccountToken: (
    serviceAccountId: string,
    input: { name: string; scopes: AutomationScope[]; expiresAt: string },
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<ServiceAccountTokenIssue>(
      `/v1/service-accounts/${encodeURIComponent(serviceAccountId)}/tokens`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  revokeServiceAccountToken: (
    serviceAccountId: string,
    tokenId: string,
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<void>(
      `/v1/service-accounts/${encodeURIComponent(serviceAccountId)}/tokens/${encodeURIComponent(tokenId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": idempotencyKey },
      },
    ),
  environments: () =>
    request<Collection<Environment> | Environment[]>("/v1/environments").then(
      asCollection,
    ),
  environment: (id: string) =>
    request<Environment>(`/v1/environments/${encodeURIComponent(id)}`),
  assignedMiddlewareProfiles: (environmentId: string, applicationId: string) =>
    request<Collection<AssignedMiddlewareProfile>>(
      `/v1/middlewares?environmentId=${encodeURIComponent(environmentId)}&applicationId=${encodeURIComponent(applicationId)}`,
    ).then((response) => ({
      items: (response.items ?? [])
        .slice(0, 200)
        .map(safeAssignedMiddlewareProfile),
    })),
  middlewareProfileCatalog: (environmentId: string, applicationId: string) =>
    request<Collection<MiddlewareProfileEntry>>(
      `/v1/middlewares/catalog?environmentId=${encodeURIComponent(environmentId)}&applicationId=${encodeURIComponent(applicationId)}`,
    ),
  createMiddlewareProfile: (
    input: {
      name: string;
      spec: MiddlewareProfileSpec;
      assignments: MiddlewareProfileAssignment[];
    },
    idempotencyKey: string,
  ) =>
    request<MiddlewareProfileEntry>("/v1/middlewares", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: input,
    }),
  reviseMiddlewareProfile: (
    profileId: string,
    input: {
      baseRevision: number;
      spec: MiddlewareProfileSpec;
      assignments: MiddlewareProfileAssignment[];
    },
    idempotencyKey: string,
  ) =>
    request<MiddlewareProfileEntry>(
      `/v1/middlewares/${encodeURIComponent(profileId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  cloneMiddlewareProfile: (
    profileId: string,
    input: {
      name: string;
      sourceRevision: number;
      assignments: MiddlewareProfileAssignment[];
    },
    idempotencyKey: string,
  ) =>
    request<MiddlewareProfileEntry>(
      `/v1/middlewares/${encodeURIComponent(profileId)}/clone`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  deactivateMiddlewareProfile: (
    profileId: string,
    revision: number,
    idempotencyKey: string,
  ) =>
    request<MiddlewareProfileEntry>(
      `/v1/middlewares/${encodeURIComponent(profileId)}/deactivate`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { revision },
      },
    ),
  createEnvironment: (input: {
    projectId: string;
    name: string;
    slug?: string;
    protectionPolicy?: "development" | "protected";
  }) =>
    request<Environment>("/v1/environments", {
      method: "POST",
      headers: idempotencyHeaders(),
      body: input,
    }),
  applications: () =>
    request<Collection<Application> | Application[]>("/v1/applications").then(
      asCollection,
    ),
  application: (id: string) =>
    request<Application>(`/v1/applications/${encodeURIComponent(id)}`),
  helmApprovals: (
    applicationId: string,
    environmentId: string,
    requestedLimit = 50,
  ) => {
    const limit = boundedHelmLimit(requestedLimit);
    return request<Collection<HelmApproval>>(
      `${helmTargetPath(applicationId, environmentId)}/approvals?limit=${limit}`,
    ).then((response) => ({
      items: (response.items ?? []).slice(0, limit).map(safeHelmApproval),
    }));
  },
  previewHelmValues: (
    applicationId: string,
    environmentId: string,
    input: HelmValuesInput,
  ) =>
    request<HelmValuesPreview>(
      `${helmTargetPath(applicationId, environmentId)}/values-preview`,
      { method: "POST", body: safeHelmValuesInput(input) },
    ).then((value) => ({
      approval: {
        id: value.approval.id,
        revision: value.approval.revision,
      },
      normalizedValuesYaml: value.normalizedValuesYaml,
      valuesDigest: value.valuesDigest,
      currentValuesDigest: value.currentValuesDigest,
      effectiveValues: value.effectiveValues,
      changedPaths: (value.changedPaths ?? []).slice(0, 512),
    })),
  helmRelease: (applicationId: string, environmentId: string) =>
    request<HelmReleaseStatus>(
      `${helmTargetPath(applicationId, environmentId)}/release`,
    ).then(safeHelmReleaseStatus),
  helmRenderedPreview: (applicationId: string, environmentId: string) =>
    request<HelmRenderedPreview>(
      `${helmTargetPath(applicationId, environmentId)}/rendered-preview`,
    ).then(safeHelmRenderedPreview),
  helmReleaseHistory: (
    applicationId: string,
    environmentId: string,
    requestedLimit = 25,
  ) => {
    const limit = boundedHelmLimit(requestedLimit);
    return request<Collection<HelmReleaseStatus>>(
      `${helmTargetPath(applicationId, environmentId)}/releases?limit=${limit}`,
    ).then((response) => ({
      items: (response.items ?? []).slice(0, limit).map(safeHelmReleaseStatus),
    }));
  },
  upsertHelmRelease: (
    applicationId: string,
    environmentId: string,
    input: HelmValuesInput,
    idempotencyKey: string,
  ) =>
    helmMutation(
      `${helmTargetPath(applicationId, environmentId)}/release`,
      "PUT",
      safeHelmValuesInput(input),
      idempotencyKey,
    ),
  retryHelmRelease: (
    applicationId: string,
    environmentId: string,
    idempotencyKey: string,
  ) =>
    helmMutation(
      `${helmTargetPath(applicationId, environmentId)}/release/retry`,
      "POST",
      {},
      idempotencyKey,
    ),
  disableHelmRelease: (
    applicationId: string,
    environmentId: string,
    idempotencyKey: string,
  ) =>
    helmMutation(
      `${helmTargetPath(applicationId, environmentId)}/release/disable`,
      "POST",
      {},
      idempotencyKey,
    ),
  rollbackHelmRelease: (
    applicationId: string,
    environmentId: string,
    sourceRevisionId: string,
    idempotencyKey: string,
  ) =>
    helmMutation(
      `${helmTargetPath(applicationId, environmentId)}/release/rollback`,
      "POST",
      { sourceRevisionId },
      idempotencyKey,
    ),
  buildDefinitions: (applicationId: string) =>
    request<Collection<BuildDefinition> | BuildDefinition[]>(
      `/v1/applications/${encodeURIComponent(applicationId)}/build-definitions`,
    ).then((response) => {
      const collection = asCollection(response);
      return {
        items: collection.items
          .slice(0, 1_000)
          .map(safeBuildDefinition)
          .filter((definition) => definition.applicationId === applicationId),
        nextCursor: collection.nextCursor,
      };
    }),
  autoDeployPolicies: (applicationId: string) =>
    request<Collection<AutoDeployPolicy>>(
      `/v1/applications/${encodeURIComponent(applicationId)}/auto-deploy-policies`,
    ),
  createAutoDeployPolicy: (
    applicationId: string,
    input: CreateAutoDeployPolicy,
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<AutoDeployPolicy>(
      `/v1/applications/${encodeURIComponent(applicationId)}/auto-deploy-policies`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  reviseAutoDeployPolicy: (
    policyId: string,
    input: ReviseAutoDeployPolicy,
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<AutoDeployPolicy>(
      `/v1/auto-deploy-policies/${encodeURIComponent(policyId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  autoDeployPolicyRevisions: (policyId: string, limit = 50) =>
    request<Collection<AutoDeployPolicyRevision>>(
      `/v1/auto-deploy-policies/${encodeURIComponent(policyId)}/revisions?limit=${Math.max(1, Math.min(100, Math.trunc(limit)))}`,
    ),
  autoDeployPolicyRuns: (policyId: string, limit = 50) =>
    request<Collection<AutoDeployRun>>(
      `/v1/auto-deploy-policies/${encodeURIComponent(policyId)}/runs?limit=${Math.max(1, Math.min(100, Math.trunc(limit)))}`,
    ),
  createBuildDefinition: (
    applicationId: string,
    input: CreateBuildDefinition,
    idempotencyKey: string,
  ) =>
    request<BuildDefinition>(
      `/v1/applications/${encodeURIComponent(applicationId)}/build-definitions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: safeCreateBuildDefinition(input),
      },
    ).then(safeBuildDefinition),
  buildDefinition: (definitionId: string) =>
    request<BuildDefinition>(
      `/v1/build-definitions/${encodeURIComponent(definitionId)}`,
    ).then(safeBuildDefinition),
  buildAttempts: (applicationId: string, requestedLimit = 50) => {
    const limit = boundedRegistryLimit(requestedLimit);
    return request<Collection<BuildAttempt> | BuildAttempt[]>(
      `/v1/applications/${encodeURIComponent(applicationId)}/builds?limit=${encodeURIComponent(String(limit))}`,
    ).then((response) => {
      const collection = asCollection(response);
      return {
        items: collection.items
          .slice(0, limit)
          .map(safeBuildAttempt)
          .filter((attempt) => attempt.applicationId === applicationId),
        nextCursor: collection.nextCursor,
      };
    });
  },
  buildAttempt: (attemptId: string) =>
    request<BuildAttempt>(`/v1/builds/${encodeURIComponent(attemptId)}`).then(
      safeBuildAttempt,
    ),
  buildLogSnapshot: (attemptId: string, options: BuildLogOptions = {}) =>
    request<BuildLogSnapshot>(
      `/v1/builds/${encodeURIComponent(attemptId)}/logs?${buildLogQuery(options, false)}`,
    ).then(safeBuildLogSnapshot),
  buildLogStreamURL: (attemptId: string, options: BuildLogOptions = {}) =>
    `/v1/builds/${encodeURIComponent(attemptId)}/logs?${buildLogQuery(options, true)}`,
  cancelBuildAttempt: (attemptId: string, idempotencyKey: string) =>
    request<BuildAttempt>(
      `/v1/builds/${encodeURIComponent(attemptId)}/cancel`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      },
    ).then(safeBuildAttempt),
  retryBuildAttempt: (attemptId: string, idempotencyKey: string) =>
    request<BuildAttempt>(`/v1/builds/${encodeURIComponent(attemptId)}/retry`, {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
    }).then(safeBuildAttempt),
  promoteBuildAttempt: (
    attemptId: string,
    input: PromoteBuildAttempt,
    idempotencyKey: string = crypto.randomUUID(),
    gitETag?: string,
  ) =>
    request<OperationWire>(
      `/v1/builds/${encodeURIComponent(attemptId)}/promote`,
      {
        method: "POST",
        headers: {
          "Idempotency-Key": idempotencyKey,
          ...(gitETag ? { "If-Match": gitETag } : {}),
        },
        body: input,
      },
    ).then(normalizeOperation),
  externalDNSIntegrations: (requestedLimit = 50) => {
    const limit = boundedRegistryLimit(requestedLimit);
    return request<{ items: ExternalDNSIntegration[]; truncated: boolean }>(
      `/v1/external-dns/integrations?limit=${encodeURIComponent(String(limit))}`,
    ).then((response) => ({
      items: (response.items ?? [])
        .slice(0, limit)
        .map(safeExternalDNSIntegration),
      truncated:
        response.truncated === true || (response.items ?? []).length > limit,
    }));
  },
  createExternalDNSIntegration: (
    input: ExternalDNSIntegrationInput,
    idempotencyKey: string,
  ) =>
    request<ExternalDNSIntegration>("/v1/external-dns/integrations", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: input,
    }).then(safeExternalDNSIntegration),
  updateExternalDNSIntegration: (
    integrationId: string,
    input: ExternalDNSIntegrationInput,
    idempotencyKey: string,
  ) =>
    request<ExternalDNSIntegration>(
      `/v1/external-dns/integrations/${encodeURIComponent(integrationId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeExternalDNSIntegration),
  deactivateExternalDNSIntegration: (
    integrationId: string,
    idempotencyKey: string,
  ) =>
    request<ExternalDNSIntegration>(
      `/v1/external-dns/integrations/${encodeURIComponent(integrationId)}`,
      { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey } },
    ).then(safeExternalDNSIntegration),
  externalDNSStatus: () =>
    request<ExternalDNSStatus>("/v1/external-dns/status").then(
      safeExternalDNSStatus,
    ),
  environmentExternalDNSIntegrations: (
    environmentId: string,
    requestedLimit = 50,
  ) => {
    const limit = boundedRegistryLimit(requestedLimit);
    return request<ExternalDNSCatalog>(
      `/v1/environments/${encodeURIComponent(environmentId)}/external-dns-integrations?limit=${encodeURIComponent(String(limit))}`,
    ).then((response) => safeExternalDNSCatalog(response, limit));
  },
  applicationExternalDNSIntegrations: (
    applicationId: string,
    environmentId: string,
    requestedLimit = 50,
  ) => {
    const limit = boundedRegistryLimit(requestedLimit);
    const query = new URLSearchParams({
      environmentId,
      limit: String(limit),
    });
    return request<ExternalDNSCatalog>(
      `/v1/applications/${encodeURIComponent(applicationId)}/external-dns-integrations?${query.toString()}`,
    ).then((response) => safeExternalDNSCatalog(response, limit));
  },
  registryTargets: (requestedLimit = 50) => {
    const limit = boundedRegistryLimit(requestedLimit);
    return request<{ items: RegistryTarget[]; truncated: boolean }>(
      `/v1/registry-targets?limit=${encodeURIComponent(String(limit))}`,
    ).then((response) => ({
      items: (response.items ?? []).slice(0, limit).map(safeRegistryTarget),
      truncated:
        response.truncated === true || (response.items ?? []).length > limit,
    }));
  },
  projectRegistryPullCredentials: (projectId: string) =>
    request<ProjectRegistryPullCredentialCatalog>(
      `/v1/projects/${encodeURIComponent(projectId)}/registry-pull-credentials`,
    ).then((response) => ({
      items: (response.items ?? [])
        .slice(0, 64)
        .map(safeProjectRegistryPullCredential),
      availableTargets: (response.availableTargets ?? [])
        .slice(0, 64)
        .map((target) => ({
          id: target.id,
          name: target.name,
          server: target.server,
          repositoryPrefix: target.repositoryPrefix,
        })),
    })),
  createProjectRegistryPullCredential: (
    projectId: string,
    input: { name: string; registryTargetId: string },
    idempotencyKey: string,
  ) =>
    request<ProjectRegistryPullCredential>(
      `/v1/projects/${encodeURIComponent(projectId)}/registry-pull-credentials`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeProjectRegistryPullCredential),
  deleteProjectRegistryPullCredential: (
    projectId: string,
    credentialId: string,
  ) =>
    request<void>(
      `/v1/projects/${encodeURIComponent(projectId)}/registry-pull-credentials/${encodeURIComponent(credentialId)}`,
      { method: "DELETE" },
    ),
  applicationRegistryPullSelection: (applicationId: string) =>
    request<ApplicationRegistryPullSelection>(
      `/v1/applications/${encodeURIComponent(applicationId)}/registry-pull-selection`,
    ).then((value) => ({
      applicationId: value.applicationId,
      type: value.type,
      ...(value.type === "project-credential"
        ? { projectCredentialId: value.projectCredentialId }
        : {}),
    })),
  putApplicationRegistryPullSelection: (
    applicationId: string,
    input: Pick<
      ApplicationRegistryPullSelection,
      "type" | "projectCredentialId"
    >,
    idempotencyKey: string,
  ) =>
    request<ApplicationRegistryPullSelection>(
      `/v1/applications/${encodeURIComponent(applicationId)}/registry-pull-selection`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ),
  createRegistryTarget: (input: RegistryTargetInput, idempotencyKey: string) =>
    request<RegistryTarget>("/v1/registry-targets", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: input,
    }).then(safeRegistryTarget),
  updateRegistryTarget: (
    targetId: string,
    input: RegistryTargetInput,
    idempotencyKey: string,
  ) =>
    request<RegistryTarget>(
      `/v1/registry-targets/${encodeURIComponent(targetId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeRegistryTarget),
  applicationRegistry: (applicationId: string, requestedLimit = 50) => {
    const limit = boundedRegistryLimit(requestedLimit);
    return request<{
      items: ApplicationRegistryTarget[];
      truncated?: boolean;
    }>(
      `/v1/applications/${encodeURIComponent(applicationId)}/registry?limit=${encodeURIComponent(String(limit))}`,
    ).then((response) => ({
      items: (response.items ?? [])
        .slice(0, limit)
        .map((item) => safeApplicationRegistryTarget(item, limit))
        .filter((item) => item.policy.serviceId === applicationId),
      truncated:
        response.truncated === true || (response.items ?? []).length > limit,
    }));
  },
  putRegistryPolicy: (
    applicationId: string,
    targetId: string,
    input: RegistryPolicyInput,
    idempotencyKey: string,
  ) =>
    request<RegistryPolicy>(
      `/v1/applications/${encodeURIComponent(applicationId)}/registry/policies/${encodeURIComponent(targetId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeRegistryPolicy),
  previewRegistryCleanup: (
    applicationId: string,
    targetId: string,
    idempotencyKey: string,
  ) =>
    request<RegistryCleanupPlan>(
      `/v1/applications/${encodeURIComponent(applicationId)}/registry/cleanup-previews`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { targetId },
      },
    ).then(safeRegistryCleanupPlan),
  registryCleanupPlan: (planId: string) =>
    request<RegistryCleanupPlan>(
      `/v1/registry-cleanup-plans/${encodeURIComponent(planId)}`,
    ).then(safeRegistryCleanupPlan),
  executeRegistryCleanup: (
    planId: string,
    confirmation: string,
    idempotencyKey: string,
  ) =>
    request<RegistryCleanupPlan>(
      `/v1/registry-cleanup-plans/${encodeURIComponent(planId)}/executions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { confirmation },
      },
    ).then(safeRegistryCleanupPlan),
  runtimeSecretBindings: (applicationId: string, environmentId?: string) => {
    const query = new URLSearchParams();
    if (environmentId !== undefined) query.set("environmentId", environmentId);
    const search = query.toString();
    return request<
      Collection<RuntimeSecretBindingMetadata> | RuntimeSecretBindingMetadata[]
    >(
      `/v1/applications/${encodeURIComponent(applicationId)}/secret-bindings${search ? `?${search}` : ""}`,
    ).then((response): Collection<RuntimeSecretBindingMetadata> => {
      const collection = asCollection(response);
      return {
        items: collection.items
          .map(safeRuntimeSecretMetadata)
          .filter(
            (binding) =>
              binding.applicationId === applicationId &&
              (environmentId === undefined ||
                binding.environmentId === environmentId),
          ),
        nextCursor: collection.nextCursor,
      };
    });
  },
  runtimeSecretBinding: (bindingId: string) =>
    request<RuntimeSecretBindingDetail>(
      `/v1/secret-bindings/${encodeURIComponent(bindingId)}`,
    ).then(safeRuntimeSecretDetail),
  createRuntimeSecretBinding: (
    applicationId: string,
    input: CreateRuntimeSecretBinding,
    idempotencyKey: string,
  ) =>
    request<RuntimeSecretBindingDetail>(
      `/v1/applications/${encodeURIComponent(applicationId)}/secret-bindings`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeRuntimeSecretDetail),
  rotateRuntimeSecretBinding: (
    bindingId: string,
    input: RotateRuntimeSecretBinding,
    idempotencyKey: string,
  ) =>
    request<RuntimeSecretBindingDetail>(
      `/v1/secret-bindings/${encodeURIComponent(bindingId)}/versions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeRuntimeSecretDetail),
  deleteRuntimeSecretBinding: (bindingId: string, idempotencyKey: string) =>
    request<void>(`/v1/secret-bindings/${encodeURIComponent(bindingId)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": idempotencyKey },
    }),
  certificateBindings: (applicationId: string, environmentId?: string) => {
    const query = new URLSearchParams();
    if (environmentId !== undefined) query.set("environmentId", environmentId);
    const search = query.toString();
    return request<
      Collection<CertificateBindingMetadata> | CertificateBindingMetadata[]
    >(
      `/v1/applications/${encodeURIComponent(applicationId)}/certificate-bindings${search ? `?${search}` : ""}`,
    ).then((response): Collection<CertificateBindingMetadata> => {
      const collection = asCollection(response);
      return {
        items: collection.items
          .map(safeCertificateMetadata)
          .filter(
            (binding) =>
              binding.applicationId === applicationId &&
              (environmentId === undefined ||
                binding.environmentId === environmentId),
          ),
        nextCursor: collection.nextCursor,
      };
    });
  },
  certificateBinding: (bindingId: string) =>
    request<CertificateBindingDetail>(
      `/v1/certificate-bindings/${encodeURIComponent(bindingId)}`,
    ).then(safeCertificateDetail),
  createCertificateBinding: (
    applicationId: string,
    input: CreateCertificateBinding,
    idempotencyKey: string,
  ) =>
    request<CertificateBindingDetail>(
      `/v1/applications/${encodeURIComponent(applicationId)}/certificate-bindings`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeCertificateDetail),
  rotateCertificateBinding: (
    bindingId: string,
    input: RotateCertificateBinding,
    idempotencyKey: string,
  ) =>
    request<CertificateBindingDetail>(
      `/v1/certificate-bindings/${encodeURIComponent(bindingId)}/versions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: input,
      },
    ).then(safeCertificateDetail),
  deleteCertificateBinding: (bindingId: string, idempotencyKey: string) =>
    request<void>(`/v1/certificate-bindings/${encodeURIComponent(bindingId)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": idempotencyKey },
    }),
  applicationSSLIPHostname: (applicationId: string, environmentId: string) => {
    const query = new URLSearchParams({ environmentId });
    return request<SSLIPHostnamePreview>(
      `/v1/applications/${encodeURIComponent(applicationId)}/sslip-hostname?${query.toString()}`,
    ).then(safeSSLIPHostnamePreview);
  },
  applicationCertificateIssuers: (
    applicationId: string,
    environmentId: string,
    hostname: string,
  ) => {
    const query = new URLSearchParams({ environmentId, hostname });
    return request<unknown>(
      `/v1/applications/${encodeURIComponent(applicationId)}/certificate-issuers?${query.toString()}`,
    ).then(safeCertificateIssuerCatalog);
  },
  platformCertificateIssuers: () =>
    request<unknown>("/v1/platform/certificate-issuers").then((value) => {
      const collection = plainRecord(value);
      if (
        !collection ||
        !exactKeys(collection, ["items"]) ||
        !Array.isArray(collection.items) ||
        collection.items.length > 100
      ) {
        throw new Error(
          "The certificate issuer administration response was invalid.",
        );
      }
      return { items: collection.items.map(safeCertificateIssuerAdminEntry) };
    }),
  createPlatformCertificateIssuer: (
    name: string,
    input: CertificateIssuerMutation,
    idempotencyKey: string,
  ) =>
    request<unknown>("/v1/platform/certificate-issuers", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: { name, ...safeCertificateIssuerMutation(input) },
    }).then(safeCertificateIssuerAdminEntry),
  revisePlatformCertificateIssuer: (
    profileId: string,
    baseRevision: number,
    input: CertificateIssuerMutation,
    idempotencyKey: string,
  ) =>
    request<unknown>(
      `/v1/platform/certificate-issuers/${encodeURIComponent(profileId)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { baseRevision, ...safeCertificateIssuerMutation(input) },
      },
    ).then(safeCertificateIssuerAdminEntry),
  deactivatePlatformCertificateIssuer: (
    profileId: string,
    revision: number,
    idempotencyKey: string,
  ) =>
    request<unknown>(
      `/v1/platform/certificate-issuers/${encodeURIComponent(profileId)}/deactivate`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { revision },
      },
    ).then(safeCertificateIssuerAdminEntry),
  createApplication: (
    input: {
      projectId: string;
      name: string;
      slug?: string;
    },
    idempotencyKey?: string,
  ) =>
    request<Application>("/v1/applications", {
      method: "POST",
      headers: idempotencyKey
        ? { "Idempotency-Key": idempotencyKey }
        : idempotencyHeaders(),
      body: input,
    }),

  deployments: () =>
    request<Collection<Deployment> | Deployment[]>("/v1/deployments").then(
      (value) => {
        const collection = asCollection(value);
        return {
          ...collection,
          items: collection.items.map(normalizeDeployment),
        };
      },
    ),
  deployment: (id: string) =>
    request<Deployment>(`/v1/deployments/${encodeURIComponent(id)}`).then(
      normalizeDeployment,
    ),
  createDeployment: (input: CreateDeployment) =>
    request<OperationWire>("/v1/deployments", {
      method: "POST",
      headers: idempotencyHeaders(),
      body: safeCreateDeploymentInput(input),
    }).then(normalizeOperation),
  previewImageResolution: (
    environmentId: string,
    applicationId: string,
    image: string,
  ) =>
    request<ImageResolutionPreview>(
      "/v1/deployments/image-resolution-preview",
      {
        method: "POST",
        body: { environmentId, applicationId, image },
      },
    ).then(safeImageResolutionPreview),
  deploymentStatus: (id: string) =>
    request<DeploymentStatus>(
      `/v1/deployments/${encodeURIComponent(id)}/status`,
    ),
  deploymentRollbackSources: (id: string, requestedLimit = 25) => {
    const limit = boundedDeploymentRollbackLimit(requestedLimit);
    return request<Collection<DeploymentRollbackCandidate>>(
      `/v1/deployments/${encodeURIComponent(id)}/rollback-sources?limit=${limit}`,
    ).then((response) => ({
      items: (response.items ?? [])
        .slice(0, limit)
        .map(safeDeploymentRollbackCandidate)
        .filter(
          (candidate): candidate is DeploymentRollbackCandidate =>
            candidate !== undefined,
        ),
    }));
  },
  rollbackDeployment: (
    id: string,
    sourceOperationId: string,
    idempotencyKey: string,
  ) =>
    request<OperationWire>(
      `/v1/deployments/${encodeURIComponent(id)}/rollback`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: { sourceOperationId },
      },
    ).then(normalizeOperation),
  deploymentConfig: (id: string) =>
    request<ConfigBundle>(`/v1/deployments/${encodeURIComponent(id)}/config`),
  validateDeploymentConfig: (id: string, change: ConfigChange) =>
    request<ConfigValidation>(
      `/v1/deployments/${encodeURIComponent(id)}/config/validate`,
      { method: "POST", body: change },
    ),
  previewDeploymentConfig: (id: string, change: ConfigChange, etag: string) =>
    request<ConfigPreview>(
      `/v1/deployments/${encodeURIComponent(id)}/config/preview`,
      {
        method: "POST",
        headers: { "If-Match": etag },
        body: change,
      },
    ),
  saveDeploymentConfig: (
    id: string,
    change: ConfigChange,
    etag: string,
    previewToken: string,
    idempotencyKey: string = crypto.randomUUID(),
  ) =>
    request<OperationWire>(`/v1/deployments/${encodeURIComponent(id)}/config`, {
      method: "PUT",
      headers: {
        "Idempotency-Key": idempotencyKey,
        "If-Match": etag,
        "Preview-Token": previewToken,
      },
      body: change,
    }).then(normalizeOperation),

  operations: () =>
    request<Collection<OperationWire> | OperationWire[]>("/v1/operations").then(
      (value) => {
        const collection = asCollection(value);
        return {
          ...collection,
          items: collection.items.map(normalizeOperation),
        };
      },
    ),
  operation: (id: string) =>
    request<OperationWire>(`/v1/operations/${encodeURIComponent(id)}`).then(
      normalizeOperation,
    ),

  workloads: (applicationId: string) =>
    request<Collection<Workload> | Workload[]>(
      `/v1/applications/${encodeURIComponent(applicationId)}/workloads`,
    ).then(asCollection),
  workloadLogs: (workloadId: string, options: WorkloadLogOptions = {}) => {
    const query = new URLSearchParams();
    if (options.pod !== undefined) query.set("pod", options.pod);
    if (options.revision !== undefined) query.set("revision", options.revision);
    if (options.container !== undefined)
      query.set("container", options.container);
    if (options.tailLines !== undefined)
      query.set("tailLines", String(options.tailLines));
    if (options.since !== undefined) query.set("since", options.since);
    if (options.previous !== undefined)
      query.set("previous", String(options.previous));
    if (options.limitBytes !== undefined)
      query.set("limitBytes", String(options.limitBytes));
    const search = query.toString();
    return request<LogSnapshot>(
      `/v1/workloads/${encodeURIComponent(workloadId)}/logs${search ? `?${search}` : ""}`,
    );
  },
  workloadEvents: (workloadId: string, options: WorkloadEventOptions = {}) => {
    const query = new URLSearchParams();
    if (options.limit !== undefined) query.set("limit", String(options.limit));
    const search = query.toString();
    return request<EventSnapshot>(
      `/v1/workloads/${encodeURIComponent(workloadId)}/events${search ? `?${search}` : ""}`,
    );
  },
  monitoringStatus: () => request<MonitoringStatus>("/v1/monitoring/status"),
  metricRange: (input: {
    scopeType: "service" | "namespace" | "global";
    scopeId: string;
    metric: MetricKey;
    from: Date;
    to: Date;
    stepSeconds: number;
  }) => {
    const query = new URLSearchParams({
      scopeType: input.scopeType,
      scopeId: input.scopeId,
      metric: input.metric,
      from: input.from.toISOString(),
      to: input.to.toISOString(),
      step: `${input.stepSeconds}s`,
    });
    return request<MetricRangeResult>(`/v1/metrics/query-range?${query}`);
  },
  latestPlatformRelease: () =>
    request<LatestPlatformRelease>("/v1/platform/releases/latest"),
  platformHelmApprovals: () =>
    request<Collection<HelmApproval>>("/v1/platform/helm/approvals").then(
      (response) => ({
        items: (response.items ?? []).slice(0, 1_000).map(safeHelmApproval),
      }),
    ),
  createPlatformHelmApproval: (
    input: CreateHelmApproval,
    idempotencyKey: string,
  ) =>
    request<HelmApproval>("/v1/platform/helm/approvals", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: safeCreateHelmApproval(input),
    }).then(safeHelmApproval),
  platformUpgrades: () =>
    request<Collection<PlatformUpgrade>>("/v1/platform/upgrades"),
};

export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return error instanceof Error ? error.message : "Something went wrong.";
}
