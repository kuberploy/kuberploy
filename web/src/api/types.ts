export type Collection<T> = {
  items: T[];
  nextCursor?: string | null;
};

export type ProblemDetail = {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  code?: string;
  requestId?: string;
  retryable?: boolean;
  errors?: Array<{
    pointer?: string;
    line?: number;
    column?: number;
    code?: string;
    detail?: string;
  }>;
};

export type ApiMeta = {
  platformVersion?: string;
  version?: string;
  apiVersion?: string;
  contractDigest?: string;
  bootstrapRequired?: boolean;
  features?: Record<string, boolean>;
};

export type User = {
  id: string;
  login?: string;
  displayName: string;
  email?: string;
  role: "platform-admin" | "project-admin" | "developer" | "viewer" | string;
  issuer?: string;
  subject?: string;
  grantRevision?: number;
  createdAt?: string;
};

export type AuthenticationContext =
  | { kind: "session" }
  | {
      kind: "service-account";
      serviceAccountId: string;
      tokenId: string;
      scopes: AutomationScope[];
      expiresAt: string;
    };

export type Principal = User & {
  authentication: AuthenticationContext;
};

export type UserInvitation = {
  id: string;
  token: string;
  expiresAt: string;
};

export type Team = {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
};

export type TeamMember = {
  teamId: string;
  userId: string;
  role: "owner" | "member";
  user?: User;
  createdAt: string;
};

export type GitHubInstallation = {
  id: string;
  githubInstallationId: number;
  accountLogin: string;
  accountType: string;
  ownerUserId: string;
  visibility: "private" | "team";
  teamId?: string;
  repositorySelection: string;
  repositoryCount: number;
  createdAt: string;
  updatedAt: string;
};

export type GitHubRepository = {
  id: string;
  githubRepositoryId: number;
  installationId: string;
  ownerId: number;
  ownerLogin: string;
  name: string;
  lifecycle: "active" | "removed";
};

export type PlatformArgoGitRepository = {
  provider: "github";
  installationId: number;
  repositoryId: number;
  owner: string;
  name: string;
};

export type PlatformArgoGitBinding = {
  id: string;
  clusterId: string;
  repository: PlatformArgoGitRepository;
  targetRef: string;
  pathPrefix: string;
  state: "ready" | "indexing" | "waiting-for-git" | "diverged" | "missing-ref";
  targetHeadRevision?: string;
  targetHeadObservedAt?: string;
  createdAt: string;
  updatedAt: string;
};

export type CreatePlatformArgoGitBinding = {
  installationId: string;
  repositoryId: string;
  targetRef: string;
};

export type EnvironmentGitBinding = {
  id: string;
  projectId: string;
  environmentId: string;
  repository: PlatformArgoGitRepository;
  targetRef: string;
  pathPrefix: string;
  credentialMode: "github-app";
  state: "ready" | "indexing" | "waiting-for-git" | "diverged" | "missing-ref";
  targetHeadRevision?: string;
  indexedRevision?: string;
  projectionGeneration: number;
  parserVersion: string;
  targetHeadObservedAt?: string;
  indexedAt?: string;
  createdAt: string;
  updatedAt: string;
};

export type CreateEnvironmentGitBinding = {
  installationId: string;
  repositoryId: string;
  targetRef: string;
};

export type LinkedGitHubSetup = {
  installation: GitHubInstallation;
  repositories: GitHubRepository[];
};

export type BuildArgument = {
  name: string;
  value: string;
};

export type BuildFileReference = {
  id: string;
  path: string;
};

export type BuildSecretProfile = {
  id: string;
  label: string;
};

export type BuildSecretProfileCatalog = {
  build: BuildSecretProfile[];
  ssh: BuildSecretProfile[];
};

export type BuildProfile = {
  resource: string;
  timeoutSeconds: number;
  egress: string;
};

export type CreateBuildDefinition = {
  installationId: string;
  repositoryId: string;
  registryTargetId: string;
  triggerRef: string;
  contextPath: string;
  dockerfilePath: string;
  platforms: Array<"linux/amd64" | "linux/arm64">;
  buildArgs?: BuildArgument[];
  secretProfileIds?: string[];
  sshProfileIds?: string[];
  cacheTrustLane: string;
  cacheImports: number;
  profile: BuildProfile;
  maxAttempts: number;
};

export type BuildRegistryBinding = {
  targetId: string;
  mode: "managed" | "external";
  server: string;
  repositoryPrefix: string;
};

export type BuildDefinition = {
  id: string;
  projectId: string;
  applicationId: string;
  installationId: string;
  repositoryId: string;
  triggerRef: string;
  contextPath: string;
  dockerfilePath: string;
  platforms: Array<"linux/amd64" | "linux/arm64">;
  registry: BuildRegistryBinding;
  buildArgs: BuildArgument[];
  secretFiles: BuildFileReference[];
  sshFiles: BuildFileReference[];
  cacheTrustLane: string;
  cacheImports: number;
  profile: BuildProfile;
  maxAttempts: number;
  definitionDigest: string;
  definitionGeneration: number;
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
};

export type BuildAttemptState =
  | "queued"
  | "preparing"
  | "running"
  | "cancelling"
  | "succeeded"
  | "failed"
  | "cancelled";

export type BuildImage = {
  reference: string;
  digest: string;
  platforms: Array<"linux/amd64" | "linux/arm64">;
};

export type BuildAttempt = {
  id: string;
  definitionId: string;
  projectId: string;
  applicationId: string;
  commitSha: string;
  gitRef: string;
  generation: number;
  state: BuildAttemptState;
  executionAttempts: number;
  maxAttempts: number;
  image?: BuildImage;
  cacheReuse?: "not-requested" | "unavailable" | "hit" | "miss" | "unknown";
  warnings?: Array<"ColdBuild" | "CacheDegraded" | "SensitiveBuildArg">;
  cacheReference?: string;
  failureCode?: string;
  cancelRequestedAt?: string;
  startedAt?: string;
  completedAt?: string;
  createdAt: string;
  updatedAt: string;
};

export type BuildLogSource = {
  id: string;
  ready: boolean;
  previous: boolean;
};

export type BuildLogCursor = {
  sourceId: string;
  timestamp: string;
  fingerprint: string;
};

export type BuildLogLine = {
  type: "line";
  timestamp?: string;
  source: BuildLogSource;
  message: string;
  truncated: boolean;
  cursor?: BuildLogCursor;
};

export type BuildLogSnapshot = {
  source: BuildLogSource;
  lines: BuildLogLine[];
  bytes: number;
  truncated: boolean;
  observedAt: string;
};

export type BuildLogStreamEvent = {
  type: "line" | "status" | "gap" | "heartbeat" | "terminal";
  line?: BuildLogLine;
  status?: {
    source: BuildLogSource;
    state: "active" | "reconnecting" | "ended";
    reason?: string;
  };
  gap?: { source: BuildLogSource; droppedLines: number };
  terminal?: { code: string; detail: string };
  at: string;
};

export type Capability = {
  resource?: string;
  actions?: string[];
  scope?: string;
  role?: AccessRole;
  scopeType?: AccessScopeType;
  scopeId?: string;
  source?: string;
};

export type Capabilities = {
  actions?: string[];
  capabilities?: Capability[];
  features?: Record<string, boolean>;
  featureStates?: Record<string, "disabled" | "unavailable" | "healthy">;
  limits?: Record<string, number | string | boolean>;
};

export type AuditEvent = {
  id: string;
  actorId: string;
  action: string;
  targetType: string;
  targetId: string;
  outcome: "accepted" | "succeeded" | "failed" | "recorded";
  requestId?: string;
  createdAt: string;
};

export type AuditEventList = { items: AuditEvent[] };

export type ResourceMetadata = {
  etag?: string;
  createdAt?: string;
  updatedAt?: string;
};

export type Project = ResourceMetadata & {
  id: string;
  name: string;
  slug?: string;
  teamId?: string;
  description?: string;
  status?: string;
};

export type AccessRole =
  | "viewer"
  | "developer"
  | "project-admin"
  | "organization-admin"
  | "platform-admin";

export type AccessScopeType =
  "platform" | "team" | "project" | "environment" | "namespace" | "application";

type AccessGrantFields = {
  id: string;
  role: AccessRole;
  scopeType: AccessScopeType;
  scopeId: string;
  permissions: Array<"logs.read">;
  source: "explicit" | "bootstrap" | "service-account";
  createdBy: string;
  createdAt: string;
};

export type AccessGrant = AccessGrantFields &
  (
    | { subjectUserId: string; subjectTeamId?: never }
    | { subjectTeamId: string; subjectUserId?: never }
  );

export type ServiceAccountRole = "viewer" | "developer" | "project-admin";

export type AutomationScope =
  "app.read" | "app.edit" | "build.create" | "logs.read";

export type ServiceAccount = {
  id: string;
  projectId: string;
  name: string;
  role: ServiceAccountRole;
  createdBy: string;
  createdAt: string;
  disabledAt?: string;
};

export type ServiceAccountToken = {
  id: string;
  serviceAccountId: string;
  name: string;
  prefix: string;
  scopes: AutomationScope[];
  expiresAt: string;
  lastUsedAt?: string;
  revokedAt?: string;
  createdBy: string;
  createdAt: string;
};

export type ServiceAccountTokenIssue = {
  tokenRecord: ServiceAccountToken;
  /** Returned only for the first successful execution, never on a replay. */
  token?: string;
};

export type Environment = ResourceMetadata & {
  id: string;
  projectId: string;
  name: string;
  slug?: string;
  namespace: string;
  argoProject?: string;
  protectionPolicy?: "development" | "protected";
  status?: string;
};

export type Application = ResourceMetadata & {
  id: string;
  projectId: string;
  name: string;
  slug?: string;
  description?: string;
  status?: string;
};

export type HelmApprovalKey = {
  id: string;
  revision: number;
};

export type HelmApproval = HelmApprovalKey & {
  repository: string;
  version: string;
  manifestDigest: string;
  packageDigest: string;
  valuesSchemaDigest: string;
  rendererImage: string;
  rendererVersion: "4.2.3";
  policyVersion: "external-helm-p0.v1";
  documentsDigest: string;
  valuesSchema: Record<string, unknown>;
  defaultValuesYaml: string;
  createdAt: string;
};

export type CreateHelmApproval = {
  repository: string;
  version: string;
  manifestDigest: string;
  packageDigest: string;
  valuesSchemaDigest: string;
};

export type HelmRenderedResource = {
  apiVersion: string;
  kind: string;
  namespace: string;
  name: string;
  sanitizedYaml?: string;
  previewOmitted: boolean;
};

export type HelmRenderedPreview = {
  releaseRevisionId: string;
  generation: number;
  manifestDigest: string;
  inventoryDigest: string;
  resourceCount: number;
  previewBytes: number;
  resources: HelmRenderedResource[];
};

export type HelmValuesInput = {
  approvalId: string;
  approvalRevision: number;
  valuesYaml: string;
};

export type HelmValuesPreview = {
  approval: HelmApprovalKey;
  normalizedValuesYaml: string;
  valuesDigest: string;
  currentValuesDigest?: string;
  effectiveValues: Record<string, unknown>;
  changedPaths: string[];
};

export type HelmReleaseAction =
  "initial" | "update" | "retry" | "disable" | "rollback";

export type HelmReleaseRevision = {
  id: string;
  generation: number;
  releaseName: string;
  action: HelmReleaseAction;
  desiredEnabled: boolean;
  parentRevisionId?: string;
  rollbackSourceRevisionId?: string;
  approval: HelmApprovalKey;
  renderCommandId?: string;
  valuesDigest: string;
  intentDigest: string;
  requestId: string;
  createdAt: string;
};

export type HelmReleasePhase =
  | "rendering"
  | "render-failed"
  | "payload-pending"
  | "payload-committed"
  | "payload-verified"
  | "application-pending"
  | "application-committed"
  | "published"
  | "failed";

export type HelmReleaseStatus = {
  revision: HelmReleaseRevision;
  phase: HelmReleasePhase;
  renderState?: "queued" | "processing" | "succeeded" | "failed";
  payloadIntentId?: string;
  payloadState?: string;
  payloadRevision?: string;
  cascadeState?: string;
  cascadeObservationState?: string;
  applicationIntentId?: string;
  applicationState?: string;
  applicationRevision?: string;
  failureCode?: string;
};

export type HelmMutationResult = {
  revision: HelmReleaseRevision;
  replayed: boolean;
};

export type ExistingImageSource = {
  type?: "image";
  image?: string;
  reference?: string;
  digest?: string;
};

export type DeploymentRoute = {
  hostname: string;
  pathPrefix: "/" | string;
  tlsMode: "httpOnly" | string;
  dnsMode?: "manual" | "sslip" | string;
};

export type ManualDeploymentRouteInput = {
  hostname: string;
  dnsMode?: "manual";
  pathPrefix: "/";
  tlsMode: "httpOnly";
};

export type SSLIPDeploymentRouteInput = {
  dnsMode: "sslip";
  pathPrefix: "/";
  tlsMode: "httpOnly";
  hostname?: never;
};

export type DeploymentRouteInput =
  ManualDeploymentRouteInput | SSLIPDeploymentRouteInput;

export type ResourceList = { cpu: string; memory: string };
export type ResourceLimits = Partial<ResourceList>;
export type WorkloadPort = {
  name: string;
  containerPort: number;
  servicePort?: number;
  protocol?: "TCP" | "UDP";
};
export type SecretBindingRef = {
  bindingId: string;
  name: string;
  key: string;
  version: number;
};
export type WorkloadEnv =
  | { name: string; value: string; valueFrom?: never }
  | {
      name: string;
      value?: never;
      valueFrom: { secretBindingRef: SecretBindingRef };
    };
export type LabelSelectorRequirement = {
  key: string;
  operator: "In" | "NotIn" | "Exists" | "DoesNotExist";
  values?: string[];
};
export type LabelSelector = {
  matchLabels?: Record<string, string>;
  matchExpressions?: LabelSelectorRequirement[];
};
export type NodeSelectorRequirement = {
  key: string;
  operator: "In" | "NotIn" | "Exists" | "DoesNotExist" | "Gt" | "Lt";
  values?: string[];
};
export type NodeSelectorTerm = {
  matchExpressions: NodeSelectorRequirement[];
};
export type PodAffinityTerm = {
  labelSelector: LabelSelector;
  topologyKey: string;
};
export type PodAffinity = {
  requiredDuringSchedulingIgnoredDuringExecution?: PodAffinityTerm[];
  preferredDuringSchedulingIgnoredDuringExecution?: Array<{
    weight: number;
    podAffinityTerm: PodAffinityTerm;
  }>;
};
export type WorkloadAffinity = {
  nodeAffinity?: {
    requiredDuringSchedulingIgnoredDuringExecution?: {
      nodeSelectorTerms: NodeSelectorTerm[];
    };
    preferredDuringSchedulingIgnoredDuringExecution?: Array<{
      weight: number;
      preference: NodeSelectorTerm;
    }>;
  };
  podAffinity?: PodAffinity;
  podAntiAffinity?: PodAffinity;
};
export type TopologySpreadConstraint = {
  maxSkew: number;
  topologyKey: string;
  whenUnsatisfiable: "DoNotSchedule" | "ScheduleAnyway";
  labelSelector: LabelSelector;
  minDomains?: number;
  nodeAffinityPolicy?: "Honor" | "Ignore";
  nodeTaintsPolicy?: "Honor" | "Ignore";
};
export type WorkloadToleration = {
  key: string;
  operator: "Equal" | "Exists";
  value?: string;
  effect: "NoSchedule" | "PreferNoSchedule" | "NoExecute";
  tolerationSeconds?: number;
};
export type MiddlewareProfileRef = {
  profileId: string;
  revision: number;
  specDigest: string;
  assignmentsDigest: string;
};
export type MiddlewareProfileAssignment = {
  scope: "project" | "environment" | "application";
  id: string;
};
export type MiddlewareProfileSpec = Record<string, Record<string, unknown>>;
export type AssignedMiddlewareProfile = MiddlewareProfileRef & {
  name: string;
  spec: MiddlewareProfileSpec;
};
export type MiddlewareProfileEntry = {
  profile: {
    id: string;
    name: string;
    lifecycle: "active" | "deactivated";
    currentRevision: number;
    createdBy: string;
    createdAt: string;
    deactivatedBy?: string;
    deactivatedAt?: string;
  };
  revision: {
    profileId: string;
    revision: number;
    spec: MiddlewareProfileSpec;
    specDigest: string;
    assignmentsDigest: string;
    createdBy: string;
    assignments: MiddlewareProfileAssignment[];
    createdAt: string;
    clonedFrom?: MiddlewareProfileRef;
  };
};
export type WorkloadProbePort = string | number;
export type WorkloadHTTPGetAction = {
  path: string;
  port: WorkloadProbePort;
  scheme?: "HTTP" | "HTTPS";
};
export type WorkloadTCPSocketAction = { port: WorkloadProbePort };
export type WorkloadExecAction = { command: string[] };
export type WorkloadProbe = {
  httpGet?: WorkloadHTTPGetAction;
  tcpSocket?: WorkloadTCPSocketAction;
  exec?: WorkloadExecAction;
  initialDelaySeconds?: number;
  periodSeconds?: number;
  timeoutSeconds?: number;
  successThreshold?: number;
  failureThreshold?: number;
};
export type WorkloadProbes = {
  startup?: WorkloadProbe;
  readiness?: WorkloadProbe;
  liveness?: WorkloadProbe;
};
export type WorkloadRuntime = {
  workloadType?: "Deployment" | "StatefulSet";
  replicas: number;
  strategy?: { type: "RollingUpdate" | "Recreate" | "OnDelete" };
  podManagementPolicy?: "OrderedReady" | "Parallel";
  command?: string[];
  args?: string[];
  workingDirectory?: string;
  terminationGracePeriodSeconds?: number;
  ports: WorkloadPort[];
  env?: WorkloadEnv[];
  resources: { requests: ResourceList; limits?: ResourceLimits };
  nodeSelector?: Record<string, string>;
  affinity?: WorkloadAffinity;
  topologySpreadConstraints?: TopologySpreadConstraint[];
  tolerations?: WorkloadToleration[];
  priorityClassName?: string;
  probes?: WorkloadProbes;
};

export type CreateDeployment = {
  environmentId: string;
  applicationId: string;
  image: string;
  /** Compare-only precondition required for a tag and forbidden for a digest.
   * The API re-resolves the tag and never persists this caller field. */
  expectedImmutableImage?: string;
  runtime: WorkloadRuntime;
  route?: DeploymentRouteInput;
};

/** Safe image-resolution projection. Registry targets, credential profiles,
 * token authorities, and authentication metadata are intentionally absent. */
export type ImageResolutionPreview = {
  requestedImage: string;
  immutableImage: string;
  resolved: boolean;
};

/** Caller-selectable intent for a verified build promotion. Build authority
 * coordinates are intentionally absent and are derived by the API. */
export type PromoteBuildAttempt = {
  environmentId: string;
  runtime: WorkloadRuntime;
  route?: DeploymentRouteInput;
};

export type Deployment = ResourceMetadata & {
  id: string;
  applicationId: string;
  environmentId: string;
  name?: string;
  image?: string;
  replicas?: number;
  port?: number;
  environment?: Record<string, string>;
  route?: DeploymentRoute;
  runtime: WorkloadRuntime;
  source?: ExistingImageSource;
  state?: string;
  operationId?: string;
  desiredRevision?: string;
  observedRevision?: string;
  /** Normalized compatibility alias for older UI components. */
  status?: string;
  /** Normalized compatibility alias for desiredRevision. */
  configRevision?: string;
};

export type DeploymentRollbackCandidate = {
  sourceOperationId: string;
  generation: number;
  image: string;
  artifactAssurance: "managed-release-verified" | "external-digest-unverified";
  managedReleaseVerified: boolean;
  createdAt: string;
};

export type OperationState =
  | "requested"
  | "queued"
  | "running"
  | "building"
  | "artifactReady"
  | "configPending"
  | "gitCommitted"
  | "reconciling"
  | "succeeded"
  | "healthy"
  | "failed"
  | "degraded"
  | "cancelled"
  | "superseded"
  | string;

export type OperationStep = {
  id?: string;
  name: string;
  state: OperationState;
  message?: string;
  startedAt?: string;
  completedAt?: string;
};

export type OperationProgress = {
  name: string;
  status: OperationState;
  detail?: string;
  startedAt?: string;
  finishedAt?: string;
};

export type OperationWire = {
  id: string;
  kind: string;
  status: OperationState;
  targetType: string;
  targetId: string;
  requestId: string;
  generation: number;
  progress: OperationProgress[];
  createdAt: string;
  updatedAt: string;
  finishedAt?: string;
  gitRevision?: string;
  pullRequest?: PullRequestPublication;
  problem?: ProblemDetail;
  /** Compatibility fields accepted only at the client boundary. */
  state?: OperationState;
  target?: { id?: string; type?: string; name?: string };
  targetRef?: { id?: string; type?: string; name?: string };
  steps?: OperationStep[];
  cancellable?: boolean;
  queuedAt?: string;
  startedAt?: string;
  completedAt?: string;
  candidateRevision?: string;
  result?: { deploymentId?: string; applicationId?: string; href?: string };
};

export type PullRequestPublication = {
  number: number;
  url: string;
  state: "open" | "closed";
  candidateRevision: string;
};

export type Operation = OperationWire & {
  state: OperationState;
};

export type DeploymentStatus = {
  deploymentId?: string;
  state?: string;
  operationId?: string;
  operationStatus?: string;
  desiredRevision?: string;
  observedRevision?: string;
  argoSyncStatus: "unknown" | "synced" | "out-of-sync";
  rolloutHealth:
    | "unknown"
    | "progressing"
    | "healthy"
    | "degraded"
    | "suspended"
    | "missing";
  argoObservedRevision?: string;
  argoObservedAt?: string;
  desiredReplicas?: number;
  readyReplicas?: number;
  rolloutConditions?: Array<{
    type: "Available" | "Progressing" | "ReplicaFailure" | "Ready" | "Failed";
    status: "True" | "False" | "Unknown";
    reason?: string;
    lastTransitionTime?: string;
  }>;
  rolloutObservedAt?: string;
  /** Other future observed-state fields remain optional. */
  gitChangeStatus?: string;
  buildStatus?: string;
  dnsStatus?: string;
  certificateStatus?: string;
  monitoringStatus?: string;
  observedAt?: string;
};

export type ReleaseCompatibility = {
  status: "compatible" | "incompatible" | "unknown";
  reasons: string[];
};

export type ReleaseChart = {
  name: string;
  version: string;
  ociReference: string;
  ociDigest: string;
  package: string;
  packageSha256: string;
};

export type PlatformReleaseManifest = {
  $schema: string;
  schemaVersion: "1.0.0";
  release: {
    tag: string;
    version: string;
    createdAt: string;
    notesUrl: string;
    summary: string;
    breakingChanges: boolean;
  };
  source: {
    repository: "kuberploy/kuberploy";
    commit: string;
  };
  versions: {
    kuberploy: string;
    api: string;
    worker: string;
    web: string;
    migration: string;
    upgrader: string;
    builderAgent: string;
    chart: string;
  };
  compatibility: {
    supportedUpgradeFrom: string;
    kubernetes: {
      constraint: string;
      testedMinors: string[];
    };
    database: {
      engine: "postgresql";
      currentSchema: string;
      minimumUpgradeableSchema: string;
      migrationSetSha256: string;
      strategy: string;
      rollbackPolicy: string;
    };
  };
  artifacts: {
    images: Array<{
      component:
        "api" | "worker" | "web" | "migration" | "upgrader" | "builder-agent";
      reference: string;
      digest: string;
      platforms: string[];
    }>;
    chart: ReleaseChart;
    componentCharts: ReleaseChart[];
  };
  dependencyLock: {
    file: "DEPENDENCIES.md";
    sha256: string;
  };
};

export type PlatformRelease = {
  tag: string;
  version: string;
  manifestDigest: string;
  publishedAt: string;
  notesUrl: string;
  breakingChanges: boolean;
  chart: ReleaseChart;
  manifest: PlatformReleaseManifest;
};

export type LatestPlatformRelease = {
  currentVersion: string;
  updateAvailable: boolean;
  compatibility: ReleaseCompatibility;
  release: PlatformRelease;
  lastCheckedAt: string;
};

export type ConfigDocument = {
  id: string;
  documentId?: string;
  gitPath?: string;
  path?: string;
  documentKind?: string;
  rawYaml: string;
  rawYAML?: string;
  document?: Record<string, unknown>;
  editablePointers?: string[];
  lockedPointers?: string[];
};

export type ConfigBundle = {
  kind: "ConfigBundle";
  etag: string;
  targetHeadRevision: string;
  indexedRevision: string;
  configRevision: string;
  freshness: "fresh" | "stale" | "projection-only";
  documents: ConfigDocument[];
  variableDependencies?: VariableDependencyState[];
  effectiveVariables?: EffectiveVariable[];
};

export type VariableSetScope = "project" | "environment";

export type VariableDependencyState = {
  path: string;
  present: boolean;
  blobId?: string;
};

export type VariableSetSnapshot = {
  scope: VariableSetScope;
  bindingId: string;
  projectId: string;
  environmentId: string;
  path: string;
  present: boolean;
  etag?: string;
  rawYaml?: string;
  document?: Record<string, unknown>;
  indexedRevision: string;
};

export type VariableSetPreview = {
  previewToken: string;
  scope: VariableSetScope;
  path: string;
  gitDiff: string;
  document: Record<string, unknown>;
  diagnostics: ConfigDiagnostic[];
  expiresAt: string;
};

export type EffectiveVariable = {
  name: string;
  value?: string;
  secretBindingRef?: {
    bindingId: string;
    name: string;
    key: string;
    version: number;
  };
  source: "project" | "environment" | "application";
  overrides?: Array<{
    scope: "project" | "environment" | "application";
    value?: string;
    secret?: boolean;
  }>;
};

export type ConfigChange =
  | {
      mode: "yaml";
      documents: Array<{ documentId: "app.yaml" | string; rawYaml: string }>;
    }
  | {
      mode: "jsonPatch";
      patch: Array<{
        op: "add" | "remove" | "replace";
        path: string;
        value?: unknown;
      }>;
    };

export type ConfigDiagnostic = {
  code: string;
  detail: string;
  pointer?: string;
  line?: number;
  column?: number;
};

export type ConfigValidation = {
  valid: boolean;
  diagnostics: ConfigDiagnostic[];
  effectiveVariables?: EffectiveVariable[];
};

export type ConfigPreview = {
  previewToken: string;
  gitDiff: string;
  renderedDiff: string;
  semanticChanges: Array<{
    pointer?: string;
    summary?: string;
    before?: unknown;
    after?: unknown;
  }>;
  warnings: string[];
  expiresAt: string;
  effectiveVariables?: EffectiveVariable[];
  renderIdentity: {
    contract: string;
    chartName: string;
    chartVersion: string;
    chartDigest: string;
    rendererImage: string;
    rendererVersion: string;
    policyVersion: string;
  };
  renderIdentityDigest: string;
};

export type Workload = {
  id: string;
  name: string;
  kind: "Deployment";
  namespace: string;
  replicas: number;
  revision?: string;
  state: string;
};

export type LogSource = {
  podId: string;
  podName: string;
  container: string;
  containerKind: "regular" | "init";
  restartCount: number;
  revision?: string;
  ready: boolean;
  terminating: boolean;
  previous: boolean;
};

export type LogLineCursor = {
  sourceId: string;
  timestamp: string;
  fingerprint: string;
};

export type LogLine = {
  type: "line";
  timestamp?: string;
  source: LogSource;
  message: string;
  truncated: boolean;
  cursor?: LogLineCursor;
};

export type LogSourceStatus = {
  source: LogSource;
  state: "active" | "reconnecting" | "ended" | "error" | "expired";
  reason?: string;
};

export type LogSnapshot = {
  lines: LogLine[];
  sources: LogSource[];
  sourceStatuses?: LogSourceStatus[];
  bytes: number;
  truncated: boolean;
  observedAt: string;
};

export type RuntimeEvent = {
  id: string;
  type: "Normal" | "Warning" | "Unknown";
  reason: string;
  message: string;
  messageTruncated: boolean;
  objectKind: "Deployment" | "ReplicaSet" | "Pod";
  objectName: string;
  count: number;
  firstSeen?: string;
  lastSeen?: string;
};

export type EventSnapshot = {
  items: RuntimeEvent[];
  truncated: boolean;
  observedAt: string;
};

export type ExternalDNSIntegrationMode = "managed" | "adopted";
export type ExternalDNSProviderKind =
  "aws" | "azure" | "cloudflare" | "google" | "rfc2136";
export type ExternalDNSSyncPolicy = "upsert-only" | "sync";

export type ExternalDNSIntegration = {
  id: string;
  slug: string;
  name: string;
  mode: ExternalDNSIntegrationMode;
  providerKind: ExternalDNSProviderKind;
  txtOwnerId: string;
  allowedDomainSuffixes: string[];
  syncPolicy: ExternalDNSSyncPolicy;
  destructiveSyncConfirmed: boolean;
  credentialSecretRef?: string;
  providerConfigRef?: string;
  egressConfigRef?: string;
  operatorProfileRef?: string;
  environmentIds: string[];
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  runtimeRevision?: number;
  lifecycle?: "active" | "deactivated";
  deactivatedAt?: string;
  protectedGitState?: "pending" | "materialized" | "dematerialized";
  protectedGitRevision?: number;
  protectedGitObservedAt?: string;
};

export type ExternalDNSIntegrationInput = {
  slug: string;
  name: string;
  mode: ExternalDNSIntegrationMode;
  providerKind: ExternalDNSProviderKind;
  txtOwnerId: string;
  allowedDomainSuffixes: string[];
  syncPolicy?: ExternalDNSSyncPolicy;
  destructiveSyncConfirmed?: boolean;
  credentialSecretRef?: string;
  providerConfigRef?: string;
  egressConfigRef?: string;
  operatorProfileRef?: string;
  environmentIds: string[];
};

export type ExternalDNSCatalogItem = {
  id: string;
  slug: string;
  name: string;
  mode: ExternalDNSIntegrationMode;
  providerKind: ExternalDNSProviderKind;
  allowedDomainSuffixes: string[];
  runtimeRevision?: number;
  runtimeAvailable: boolean;
};

export type ExternalDNSCatalog = {
  items: ExternalDNSCatalogItem[];
  truncated: boolean;
  configurationState: "empty" | "configured";
  controllerReadiness: "unobserved" | "ready";
  runtimeAvailable: boolean;
};

export type ExternalDNSStatus = {
  configurationState: "empty" | "configured";
  controllerReadiness: "unobserved" | "ready";
  runtimeAvailable: boolean;
  detail: string;
};

export type MonitoringStatus = {
  mode?: "managed" | "existing" | "disabled" | string;
  status?: string;
  available?: boolean;
  message?: string;
  observedAt?: string;
};

export type MetricKey =
  | "cpu-usage"
  | "memory-working-set"
  | "replicas-ready"
  | "container-restarts"
  | "http-request-rate"
  | "http-error-ratio"
  | "http-latency-p95";

export type MetricSample = {
  timestamp: string;
  value: number;
};

export type MetricRangeResult = {
  metric: MetricKey;
  scope: "service" | "namespace" | "global";
  series: Array<{
    labels: Partial<
      Record<
        | "__name__"
        | "cluster"
        | "namespace"
        | "kuberploy_project"
        | "kuberploy_environment"
        | "kuberploy_application"
        | "kuberploy_service",
        string
      >
    >;
    samples: MetricSample[];
  }>;
  observedAt: string;
};

export type RuntimeSecretProvider = "external-secrets" | "sealed-secrets";

export type RuntimeSecretDelivery =
  | {
      sourceKey: string;
      kind: "environment";
      environmentName: string;
    }
  | {
      sourceKey: string;
      kind: "file";
      filePath: string;
      fileMode: 256 | 288;
    };

export type RuntimeSecretBindingState =
  "provisioning" | "ready" | "deleting" | "deleted" | "failed";

export type RuntimeSecretVersionState =
  | "staging"
  | "awaiting-readiness"
  | "active"
  | "retained"
  | "failed"
  | "deleted";

export type RuntimeSecretBindingMetadata = {
  id: string;
  applicationId: string;
  environmentId: string;
  name: string;
  provider: RuntimeSecretProvider;
  state: RuntimeSecretBindingState;
  activeVersion?: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  deleteStartedAt?: string;
  deletedAt?: string;
};

export type RuntimeSecretVersionMetadata = {
  id: string;
  number: number;
  state: RuntimeSecretVersionState;
  deliveries: RuntimeSecretDelivery[];
  failureCode?: string;
  stagedAt?: string;
  readinessObservedAt?: string;
  activatedAt?: string;
  retainedAt?: string;
  createdAt: string;
  updatedAt: string;
};

export type RuntimeSecretBindingDetail = RuntimeSecretBindingMetadata & {
  versions: RuntimeSecretVersionMetadata[];
};

export type RuntimeSecretWriteValues = Record<string, string>;

export type CreateRuntimeSecretBinding = {
  environmentId: string;
  name: string;
  provider: RuntimeSecretProvider;
  deliveries: RuntimeSecretDelivery[];
  values: RuntimeSecretWriteValues;
};

export type RotateRuntimeSecretBinding = {
  expectedActiveVersion: number;
  deliveries: RuntimeSecretDelivery[];
  values: RuntimeSecretWriteValues;
};

export type CertificateBindingState =
  "provisioning" | "ready" | "deleting" | "deleted" | "failed";

export type CertificateBindingMetadata = {
  id: string;
  applicationId: string;
  environmentId: string;
  name: string;
  state: CertificateBindingState;
  activeVersion?: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  deleteStartedAt?: string;
  deletedAt?: string;
};

export type CertificateVersionMetadata = {
  number: number;
  leafFingerprint: string;
  publicKeyFingerprint: string;
  dnsNames: string[];
  ipAddresses: string[];
  notBefore: string;
  notAfter: string;
  createdBy: string;
  createdAt: string;
};

export type CertificateBindingDetail = CertificateBindingMetadata & {
  versions: CertificateVersionMetadata[];
};

export type CreateCertificateBinding = {
  environmentId: string;
  name: string;
  certificatePem: string;
  privateKeyPem: string;
};

export type RotateCertificateBinding = {
  expectedActiveVersion: number;
  certificatePem: string;
  privateKeyPem: string;
};

export type CertificateBindingReference = {
  bindingId: string;
  name: string;
  version: number;
};

export type SSLIPHostnamePreview = {
  mode: "sslip";
  hostname: string;
  source: "service-ip" | "verified-static-ip";
  observedAt: string;
};

export type CertificateIssuerCatalogItem = {
  name: string;
  environment: "production" | "staging";
  solverTypes: Array<"http01" | "dns01" | "dns01-cloudflare">;
  source: "bootstrap" | "managed";
  revision?: number;
};

export type CertificateIssuerCatalog = {
  items: CertificateIssuerCatalogItem[];
};

export type CertificateIssuerSolverType = "http01" | "dns01-cloudflare";

export type CertificateIssuerAdminEntry = {
  id: string;
  name: string;
  lifecycle: "active" | "deactivated";
  currentRevision: number;
  revision: {
    number: number;
    environment: "production" | "staging";
    email: string;
    accountPrivateKeySecretName: string;
    solver: CertificateIssuerSolverType;
    dnsZones?: string[];
    apiTokenSecretName?: string;
    apiTokenSecretKey?: string;
    specDigest: string;
    createdAt: string;
  };
  observation: {
    state: "pending" | "ready" | "degraded";
    observedGeneration?: number;
    reason?: string;
    observedAt?: string;
    updatedAt: string;
  };
  createdAt: string;
  deactivatedAt?: string;
};

export type CertificateIssuerMutation = {
  environment: "production" | "staging";
  email: string;
  accountPrivateKeySecretName: string;
  solver:
    | { type: "http01" }
    | {
        type: "dns01-cloudflare";
        dnsZones: string[];
        apiTokenSecretName: string;
        apiTokenSecretKey: string;
      };
};

export type RegistryTargetMode = "managed" | "external";

export type RegistryTarget = {
  id: string;
  name: string;
  mode: RegistryTargetMode;
  endpoint: string;
  repositoryPrefix: string;
  pullCredentialRef?: string;
  pushCredentialRef?: string;
  cacheCredentialRef?: string;
  createdAt: string;
  updatedAt: string;
};

export type RegistryTargetInput = {
  name: string;
  mode: RegistryTargetMode;
  endpoint: string;
  repositoryPrefix: string;
  pullCredentialRef?: string;
  pushCredentialRef?: string;
  cacheCredentialRef?: string;
};

export type RegistryPullTargetOption = {
  id: string;
  name: string;
  server: string;
  repositoryPrefix: string;
};

export type ProjectRegistryPullCredential = {
  id: string;
  projectId: string;
  registryTargetId: string;
  name: string;
  registryName: string;
  registryServer: string;
  repositoryPrefix: string;
  createdAt: string;
  updatedAt: string;
};

export type ProjectRegistryPullCredentialCatalog = {
  items: ProjectRegistryPullCredential[];
  availableTargets: RegistryPullTargetOption[];
};

export type ApplicationRegistryPullSelection = {
  applicationId: string;
  type: "public" | "project-credential";
  projectCredentialId?: string;
};

export type RegistryPolicy = {
  registryTargetId: string;
  serviceId: string;
  repository: string;
  keepLastSuccessful: number;
  minimumSafetyAgeSeconds: number;
  cacheKeepGenerations: number;
  cacheUnusedExpirySeconds: number;
  cacheByteQuota: number;
  createdAt: string;
  updatedAt: string;
};

export type RegistryPolicyInput = {
  repository: string;
  keepLastSuccessful?: number;
  minimumSafetyAgeSeconds?: number;
  cacheKeepGenerations?: number;
  cacheUnusedExpirySeconds?: number;
  cacheByteQuota?: number;
};

export type RegistryInventoryObservation = {
  revision: string;
  complete: boolean;
  repositories: string[];
  repositoriesTruncated: boolean;
  observedAt: string;
};

export type RegistryCatalogObservation = {
  repository: string;
  revision: number;
  complete: boolean;
  observedAt: string;
  manifestCount: number;
  blobCount: number;
};

export type RegistryRelease = {
  id: string;
  repository: string;
  rootDigest: string;
  createdAt: string;
  succeededAt?: string;
  availability: "present" | "expired" | "missing";
  availabilityObservedAt?: string;
};

export type RegistryCacheGeneration = {
  id: string;
  repository: string;
  platformSet: string;
  trustLane: string;
  cacheSchema: string;
  buildDefinitionHash: string;
  generation: number;
  rootDigest: string;
  sizeBytes: number;
  state: string;
  activeImports: number;
  activeExports: number;
  createdAt: string;
  completedAt?: string;
  lastUsedAt: string;
};

export type ApplicationRegistryTarget = {
  target: RegistryTarget;
  policy: RegistryPolicy;
  inventory?: RegistryInventoryObservation;
  catalogObservations: RegistryCatalogObservation[];
  catalogTruncated: boolean;
  releases: RegistryRelease[];
  releasesTruncated: boolean;
  cacheGenerations: RegistryCacheGeneration[];
  cacheGenerationsTruncated: boolean;
  observedAt: string;
};

export type RegistryCleanupSummary = {
  protectedManifests: number;
  deletedManifests: number;
  garbageCollectBlobs: number;
  estimatedBytes: number;
  cacheBytesBefore: number;
  cacheBytesAfter: number;
  cacheQuotaSatisfied: boolean;
};

export type RegistryCleanupItem = {
  ordinal: number;
  repository: string;
  resourceKind: string;
  digest: string;
  disposition: "protect" | "delete";
  action: string;
  estimatedBytes: number;
  reasons: string[];
  state: string;
  providerMessage?: string;
  updatedAt: string;
};

export type RegistryCleanupPlan = {
  id: string;
  registryTargetId: string;
  serviceId: string;
  planDigest: string;
  state: string;
  policy: RegistryPolicy;
  summary: RegistryCleanupSummary;
  items: RegistryCleanupItem[];
  itemsTruncated: boolean;
  createdAt: string;
  claimedAt?: string;
  completedAt?: string;
  failure?: string;
};

export type AutoDeployPolicyRevision = {
  revision: number;
  enabled: boolean;
  sourceDeploymentId: string;
  sourceDeploymentGeneration: number;
  sourceConfigETag: string;
  templateDigest: string;
  serviceActorId: string;
  createdBy: string;
  createdAt: string;
};

export type AutoDeployPolicy = {
  id: string;
  buildDefinitionId: string;
  projectId: string;
  applicationId: string;
  environmentId: string;
  currentRevision: number;
  current: AutoDeployPolicyRevision;
  createdBy: string;
  createdAt: string;
  updateSemantics: string;
};

export type AutoDeployRun = {
  attemptId: string;
  policyRevision: number;
  releaseId: string;
  state: "pending" | "processing" | "submitted" | "failed";
  attempts: number;
  operationId?: string;
  deploymentId?: string;
  failureCode?: string;
  availableAt: string;
  createdAt: string;
  updatedAt: string;
  completedAt?: string;
};

export type CreateAutoDeployPolicy = {
  buildDefinitionId: string;
  environmentId: string;
  templateDeploymentId: string;
  serviceActorId: string;
  enabled: boolean;
};

export type ReviseAutoDeployPolicy = {
  templateDeploymentId: string;
  serviceActorId: string;
  enabled: boolean;
  expectedRevision: number;
};
