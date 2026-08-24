package domain

import "time"

type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email,omitempty"`
	DisplayName   string    `json:"displayName"`
	Role          string    `json:"role"`
	Issuer        string    `json:"issuer"`
	Subject       string    `json:"subject"`
	GrantRevision int64     `json:"grantRevision"`
	CreatedAt     time.Time `json:"createdAt"`
}

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	TeamID    string    `json:"teamId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type UserInvitation struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

type TeamMember struct {
	TeamID    string    `json:"teamId"`
	UserID    string    `json:"userId"`
	Role      string    `json:"role"`
	User      *User     `json:"user,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type GitHubInstallation struct {
	ID                   string    `json:"id"`
	GitHubInstallationID int64     `json:"githubInstallationId"`
	AccountLogin         string    `json:"accountLogin"`
	AccountType          string    `json:"accountType"`
	OwnerUserID          string    `json:"ownerUserId"`
	Visibility           string    `json:"visibility"`
	TeamID               string    `json:"teamId,omitempty"`
	RepositorySelection  string    `json:"repositorySelection"`
	RepositoryCount      int       `json:"repositoryCount"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type Environment struct {
	ID               string                      `json:"id"`
	ProjectID        string                      `json:"projectId"`
	Name             string                      `json:"name"`
	Slug             string                      `json:"slug"`
	Namespace        string                      `json:"namespace"`
	ArgoProject      string                      `json:"argoProject"`
	ProtectionPolicy EnvironmentProtectionPolicy `json:"protectionPolicy"`
	CreatedAt        time.Time                   `json:"createdAt"`
}

type EnvironmentProtectionPolicy string

const (
	EnvironmentDevelopment EnvironmentProtectionPolicy = "development"
	EnvironmentProtected   EnvironmentProtectionPolicy = "protected"
)

type Application struct {
	ID         string                `json:"id"`
	ProjectID  string                `json:"projectId"`
	Name       string                `json:"name"`
	Slug       string                `json:"slug"`
	SourceKind ApplicationSourceKind `json:"sourceKind"`
	CreatedAt  time.Time             `json:"createdAt"`
}

type ApplicationSourceKind string

const (
	ApplicationSourceOCI    ApplicationSourceKind = "oci"
	ApplicationSourceGitHub ApplicationSourceKind = "github"
	ApplicationSourceGitSSH ApplicationSourceKind = "git-ssh"
	ApplicationSourceHelm   ApplicationSourceKind = "helm"
)

func (kind ApplicationSourceKind) Valid() bool {
	return kind == ApplicationSourceOCI || kind == ApplicationSourceGitHub || kind == ApplicationSourceGitSSH || kind == ApplicationSourceHelm
}

type EnvironmentAppPlacementState string
type EnvironmentAppPlacementDesiredState string

const (
	EnvironmentAppPlacementDraft  EnvironmentAppPlacementState = "draft"
	EnvironmentAppPlacementActive EnvironmentAppPlacementState = "active"

	EnvironmentAppPlacementStopped EnvironmentAppPlacementDesiredState = "stopped"
	EnvironmentAppPlacementRunning EnvironmentAppPlacementDesiredState = "running"
)

// EnvironmentAppPlacement associates one project-owned App identity with an
// Environment. A clone creates only draft/stopped placements; no placement is
// deployment authority and no placement contains workload or secret values.
type EnvironmentAppPlacement struct {
	ProjectID       string                              `json:"projectId"`
	EnvironmentID   string                              `json:"environmentId"`
	ApplicationID   string                              `json:"applicationId"`
	ApplicationName string                              `json:"applicationName"`
	ApplicationSlug string                              `json:"applicationSlug"`
	State           EnvironmentAppPlacementState        `json:"state"`
	DesiredState    EnvironmentAppPlacementDesiredState `json:"desiredState"`
	CreatedAt       time.Time                           `json:"createdAt"`
	UpdatedAt       time.Time                           `json:"updatedAt"`
}

type EnvironmentCloneResult struct {
	Environment   Environment               `json:"environment"`
	AppPlacements []EnvironmentAppPlacement `json:"appPlacements"`
}

// ProjectRegistryPullCredential is a safe project-scoped catalog entry. It
// names an operator-owned registry target but never exposes Secret coordinates.
type ProjectRegistryPullCredential struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"projectId"`
	RegistryTargetID string    `json:"registryTargetId"`
	Name             string    `json:"name"`
	RegistryName     string    `json:"registryName"`
	RegistryServer   string    `json:"registryServer"`
	RepositoryPrefix string    `json:"repositoryPrefix"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type ApplicationRegistryPullMode string

const (
	ApplicationRegistryPullPublic     ApplicationRegistryPullMode = "public"
	ApplicationRegistryPullCredential ApplicationRegistryPullMode = "project-credential"
)

// ApplicationRegistryPullSelection is the service-level choice. An absent row
// means a legacy application has not explicitly selected a mode yet.
type ApplicationRegistryPullSelection struct {
	ApplicationID       string                      `json:"applicationId"`
	Mode                ApplicationRegistryPullMode `json:"mode"`
	ProjectCredentialID string                      `json:"projectCredentialId,omitempty"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
}

type Deployment struct {
	ID               string                 `json:"id"`
	EnvironmentID    string                 `json:"environmentId"`
	ApplicationID    string                 `json:"applicationId"`
	Image            string                 `json:"image"`
	Replicas         int                    `json:"replicas"`
	Port             int                    `json:"port"`
	Environment      map[string]string      `json:"environment,omitempty"`
	Route            *Route                 `json:"route,omitempty"`
	Runtime          WorkloadRuntime        `json:"runtime"`
	RegistryPull     *RegistryPullReference `json:"-"`
	State            string                 `json:"state"`
	OperationID      string                 `json:"operationId"`
	Generation       int64                  `json:"generation"`
	DesiredRevision  string                 `json:"desiredRevision,omitempty"`
	ObservedRevision string                 `json:"observedRevision,omitempty"`
	CreatedAt        time.Time              `json:"createdAt"`
	UpdatedAt        time.Time              `json:"updatedAt"`
	// ConfigRaw is an immutable operation snapshot and is never serialized in
	// deployment API responses. The config endpoints expose it with an ETag.
	ConfigRaw     []byte `json:"-"`
	ConfigVersion int64  `json:"-"`
}

type ManifestRelease struct {
	Tag             string    `json:"tag"`
	Version         string    `json:"version"`
	CreatedAt       time.Time `json:"createdAt"`
	NotesURL        string    `json:"notesUrl"`
	Summary         string    `json:"summary"`
	BreakingChanges bool      `json:"breakingChanges"`
}
type ManifestSource struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}
type ManifestVersions struct {
	Kuberploy    string `json:"kuberploy"`
	API          string `json:"api"`
	Worker       string `json:"worker"`
	Web          string `json:"web"`
	Migration    string `json:"migration"`
	Upgrader     string `json:"upgrader,omitempty"`
	BuilderAgent string `json:"builderAgent"`
	Chart        string `json:"chart"`
}
type KubernetesCompatibility struct {
	Constraint   string   `json:"constraint"`
	TestedMinors []string `json:"testedMinors"`
}
type DatabaseCompatibility struct {
	Engine                   string `json:"engine"`
	CurrentSchema            string `json:"currentSchema"`
	MinimumUpgradeableSchema string `json:"minimumUpgradeableSchema"`
	MigrationSetSHA256       string `json:"migrationSetSha256"`
	Strategy                 string `json:"strategy"`
	RollbackPolicy           string `json:"rollbackPolicy"`
}
type ReleaseCompatibility struct {
	SupportedUpgradeFrom string                  `json:"supportedUpgradeFrom"`
	Kubernetes           KubernetesCompatibility `json:"kubernetes"`
	Database             DatabaseCompatibility   `json:"database"`
}
type ManifestImage struct {
	Component string   `json:"component"`
	Reference string   `json:"reference"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
}
type ManifestChart struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	OCIReference  string `json:"ociReference"`
	OCIDigest     string `json:"ociDigest"`
	Package       string `json:"package"`
	PackageSHA256 string `json:"packageSha256"`
}
type ManifestArtifacts struct {
	Images          []ManifestImage `json:"images"`
	Chart           ManifestChart   `json:"chart"`
	ComponentCharts []ManifestChart `json:"componentCharts"`
}
type ManifestDependencyLock struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type ReleaseManifest struct {
	Schema         string                 `json:"$schema"`
	SchemaVersion  string                 `json:"schemaVersion"`
	Release        ManifestRelease        `json:"release"`
	Source         ManifestSource         `json:"source"`
	Versions       ManifestVersions       `json:"versions"`
	Compatibility  ReleaseCompatibility   `json:"compatibility"`
	Artifacts      ManifestArtifacts      `json:"artifacts"`
	DependencyLock ManifestDependencyLock `json:"dependencyLock"`
}

type ReleaseInfo struct {
	Tag            string          `json:"tag"`
	Version        string          `json:"version"`
	ManifestDigest string          `json:"manifestDigest"`
	Manifest       ReleaseManifest `json:"manifest"`
	ManifestBytes  []byte          `json:"-"`
	PublishedAt    time.Time       `json:"publishedAt"`
}

type Route struct {
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix"`
	TLSMode    string `json:"tlsMode"`
	DNSMode    string `json:"dnsMode,omitempty"`
}

type ProgressStep struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Detail     string     `json:"detail,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Operation struct {
	ID          string                  `json:"id"`
	Kind        string                  `json:"kind"`
	Status      string                  `json:"status"`
	TargetType  string                  `json:"targetType"`
	TargetID    string                  `json:"targetId"`
	RequestID   string                  `json:"requestId"`
	Generation  int64                   `json:"generation"`
	Progress    []ProgressStep          `json:"progress"`
	GitRevision string                  `json:"gitRevision,omitempty"`
	PullRequest *PullRequestPublication `json:"pullRequest,omitempty"`
	Problem     *ProblemData            `json:"problem,omitempty"`
	CreatedAt   time.Time               `json:"createdAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
	FinishedAt  *time.Time              `json:"finishedAt,omitempty"`
}

// GitPublicationResult is the closed worker-to-store completion contract. A
// direct publication carries only Revision. A protected publication carries
// only the exact provider pull-request receipt and never a desired revision.
type GitPublicationResult struct {
	Mode              string
	Revision          string
	CandidateRevision string
	PullRequestNumber int64
	PullRequestURL    string
	PullRequestState  string
}

type PullRequestPublication struct {
	Number            int64  `json:"number"`
	URL               string `json:"url"`
	State             string `json:"state"`
	CandidateRevision string `json:"candidateRevision"`
}

type ProblemData struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type WorkMessage struct {
	OperationID string
	Kind        string
	ScopeID     string
	Generation  int64
	TraceID     string
	DeliveryID  string
}

type CreateProject struct{ Name, Slug, TeamID string }
type CreateEnvironment struct {
	ProjectID, Name, Slug, Namespace, ArgoProject string
	ProtectionPolicy                              EnvironmentProtectionPolicy
}
type CloneEnvironment struct {
	Name, Slug       string
	ProtectionPolicy EnvironmentProtectionPolicy
}
type CreateApplication struct {
	ProjectID, EnvironmentID, Name, Slug string
	SourceKind                           ApplicationSourceKind
}
type CreateDeployment struct {
	EnvironmentID string
	ApplicationID string
	Image         string
	Replicas      int
	Port          int
	Environment   map[string]string
	Route         *Route
	Runtime       WorkloadRuntime
	RegistryPull  *RegistryPullReference
	// ConfigRaw is an internal, server-owned exact AppConfig snapshot. Public
	// deployment requests never populate it; rollback uses it so configuration
	// that is not representable by legacy runtime fields is not regenerated or
	// lost.
	ConfigRaw []byte
}

// RegistryPullReference is safe, locked AppConfig metadata. It never contains
// a Kubernetes Secret name, source Secret coordinate, or registry credential.
type RegistryPullReference struct {
	TargetID        string
	ProfileName     string
	ProfileRevision int64
}

func (r RegistryPullReference) Valid() bool {
	return uuidPattern.MatchString(r.TargetID) && validDNSLabel(r.ProfileName) && r.ProfileRevision > 0
}

type CreateTeam struct{ Name, Slug string }
type AddTeamMember struct{ UserID, Role string }
type CreateGitHubInstallation struct {
	GitHubInstallationID int64
	AccountLogin         string
	AccountType          string
	RepositorySelection  string
	RepositoryCount      int
}
type UpdateGitHubInstallationSharing struct{ Visibility, TeamID string }

type DeploymentStatus struct {
	DeploymentID         string             `json:"deploymentId"`
	State                string             `json:"state"`
	OperationID          string             `json:"operationId"`
	OperationStatus      string             `json:"operationStatus"`
	DesiredRevision      string             `json:"desiredRevision,omitempty"`
	ObservedRevision     string             `json:"observedRevision,omitempty"`
	ArgoSyncStatus       string             `json:"argoSyncStatus"`
	RolloutHealth        string             `json:"rolloutHealth"`
	ArgoObservedRevision string             `json:"argoObservedRevision,omitempty"`
	ArgoObservedAt       *time.Time         `json:"argoObservedAt,omitempty"`
	DesiredReplicas      *int32             `json:"desiredReplicas,omitempty"`
	ReadyReplicas        *int32             `json:"readyReplicas,omitempty"`
	RolloutConditions    []RolloutCondition `json:"rolloutConditions,omitempty"`
	RolloutObservedAt    *time.Time         `json:"rolloutObservedAt,omitempty"`
}

// RolloutCondition is the bounded, message-free Kubernetes Deployment
// condition projection. Provider messages are excluded because they can carry
// untrusted workload text; type, status, reason, and transition time are enough
// to report exact rollout state.
type RolloutCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

// ArgoRolloutObservation is the metadata-only subset of an Argo Application
// observation that may be joined into deployment status. It carries no
// Kubernetes object identity, resource payload, message, or command channel.
type ArgoRolloutObservation struct {
	DeploymentID, ApplicationID, ProjectID, EnvironmentID   string
	DestinationNamespace, DesiredRevision, ObservedRevision string
	SyncStatus, HealthStatus                                string
	ObservedAt                                              time.Time
}

// RegistryTargetMode is intentionally a closed set. External targets are data
// planes that Kuberploy may push to and pull from, but never lifecycle owners.
type RegistryTargetMode string

const (
	RegistryTargetManaged  RegistryTargetMode = "managed"
	RegistryTargetExternal RegistryTargetMode = "external"
)

const (
	DefaultKeepLastSuccessful   = 10
	MinimumKeepLastSuccessful   = 1
	MaximumKeepLastSuccessful   = 100
	DefaultRegistrySafetyAge    = 24 * time.Hour
	DefaultCacheKeepGenerations = 2
	DefaultCacheUnusedExpiry    = 7 * 24 * time.Hour
	DefaultCacheByteQuota       = int64(10 << 30)
)

type RegistryTarget struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Mode               RegistryTargetMode `json:"mode"`
	Endpoint           string             `json:"endpoint"`
	RepositoryPrefix   string             `json:"repositoryPrefix"`
	PullCredentialRef  string             `json:"pullCredentialRef,omitempty"`
	PushCredentialRef  string             `json:"pushCredentialRef,omitempty"`
	CacheCredentialRef string             `json:"cacheCredentialRef,omitempty"`
	CreatedAt          time.Time          `json:"createdAt"`
	UpdatedAt          time.Time          `json:"updatedAt"`
}

// ServiceRegistryPolicy applies to one immutable service identity. Cache
// retention fields are only enforced for managed targets; on external targets
// they are returned as operator-managed metadata and never drive cleanup.
type ServiceRegistryPolicy struct {
	RegistryTargetID     string        `json:"registryTargetId"`
	ServiceID            string        `json:"serviceId"`
	Repository           string        `json:"repository"`
	KeepLastSuccessful   int           `json:"keepLastSuccessful"`
	MinimumSafetyAge     time.Duration `json:"minimumSafetyAge"`
	CacheKeepGenerations int           `json:"cacheKeepGenerations"`
	CacheUnusedExpiry    time.Duration `json:"cacheUnusedExpiry"`
	CacheByteQuota       int64         `json:"cacheByteQuota"`
	CreatedAt            time.Time     `json:"createdAt"`
	UpdatedAt            time.Time     `json:"updatedAt"`
}

type RegistryManifestKind string

const (
	RegistryManifestIndex RegistryManifestKind = "index"
	RegistryManifestImage RegistryManifestKind = "manifest"
)

// RegistryManifest and RegistryBlob are repository-scoped catalog nodes. A
// managed registry may physically deduplicate blobs across repositories; the
// lifecycle planner therefore computes blob reachability across the complete
// registry inventory before producing any garbage-collection item.
type RegistryManifest struct {
	RegistryTargetID        string               `json:"registryTargetId"`
	Repository              string               `json:"repository"`
	Digest                  string               `json:"digest"`
	Kind                    RegistryManifestKind `json:"kind"`
	MediaType               string               `json:"mediaType"`
	SizeBytes               int64                `json:"sizeBytes"`
	PlatformOS              string               `json:"platformOs,omitempty"`
	PlatformArchitecture    string               `json:"platformArchitecture,omitempty"`
	PlatformVariant         string               `json:"platformVariant,omitempty"`
	Present                 bool                 `json:"present"`
	FirstObservedAt         time.Time            `json:"firstObservedAt"`
	LastObservedAt          time.Time            `json:"lastObservedAt"`
	LastObservationRevision int64                `json:"lastObservationRevision"`
	DeletedAt               *time.Time           `json:"deletedAt,omitempty"`
}

type RegistryBlob struct {
	RegistryTargetID        string     `json:"registryTargetId"`
	Repository              string     `json:"repository"`
	Digest                  string     `json:"digest"`
	MediaType               string     `json:"mediaType,omitempty"`
	SizeBytes               int64      `json:"sizeBytes"`
	Present                 bool       `json:"present"`
	FirstObservedAt         time.Time  `json:"firstObservedAt"`
	LastObservedAt          time.Time  `json:"lastObservedAt"`
	LastObservationRevision int64      `json:"lastObservationRevision"`
	DeletedAt               *time.Time `json:"deletedAt,omitempty"`
}

type RegistryManifestLink struct {
	Repository   string `json:"repository"`
	ParentDigest string `json:"parentDigest"`
	ChildDigest  string `json:"childDigest"`
}

type RegistryManifestBlobLink struct {
	Repository     string `json:"repository"`
	ManifestDigest string `json:"manifestDigest"`
	BlobDigest     string `json:"blobDigest"`
}

type RegistryCatalogObservation struct {
	ID               string    `json:"id"`
	RegistryTargetID string    `json:"registryTargetId"`
	Repository       string    `json:"repository"`
	Revision         int64     `json:"revision"`
	Complete         bool      `json:"complete"`
	SnapshotDigest   string    `json:"snapshotDigest"`
	ObservedAt       time.Time `json:"observedAt"`
	ManifestCount    int       `json:"manifestCount"`
	BlobCount        int       `json:"blobCount"`
}

type RegistryCatalogSnapshot struct {
	Observation RegistryCatalogObservation `json:"observation"`
	Manifests   []RegistryManifest         `json:"manifests"`
	Blobs       []RegistryBlob             `json:"blobs"`
	Children    []RegistryManifestLink     `json:"children"`
	BlobLinks   []RegistryManifestBlobLink `json:"blobLinks"`
}

type RegistryInventoryObservation struct {
	RegistryTargetID string    `json:"registryTargetId"`
	Revision         string    `json:"revision"`
	Complete         bool      `json:"complete"`
	Repositories     []string  `json:"repositories"`
	ObservedAt       time.Time `json:"observedAt"`
}

type RegistryAuthority string

const (
	RegistryAuthorityGitIntent  RegistryAuthority = "git-intent"
	RegistryAuthorityRuntime    RegistryAuthority = "runtime"
	RegistryAuthorityOperations RegistryAuthority = "operations"
)

type RegistryAuthorityObservation struct {
	RegistryTargetID string            `json:"registryTargetId"`
	ServiceID        string            `json:"serviceId"`
	Authority        RegistryAuthority `json:"authority"`
	Revision         string            `json:"revision"`
	Complete         bool              `json:"complete"`
	SnapshotDigest   string            `json:"snapshotDigest"`
	ObservedAt       time.Time         `json:"observedAt"`
}

type RegistryArtifactReferenceKind string

const (
	RegistryReferenceCurrentGitIntent RegistryArtifactReferenceKind = "current-git-intent"
	RegistryReferenceObservedRunning  RegistryArtifactReferenceKind = "observed-running"
	RegistryReferencePin              RegistryArtifactReferenceKind = "pin"
	RegistryReferenceActiveOperation  RegistryArtifactReferenceKind = "active-operation"
)

type RegistryArtifactReference struct {
	RegistryTargetID string                        `json:"registryTargetId"`
	ServiceID        string                        `json:"serviceId"`
	Repository       string                        `json:"repository"`
	Digest           string                        `json:"digest"`
	Kind             RegistryArtifactReferenceKind `json:"kind"`
	ReferenceKey     string                        `json:"referenceKey"`
	SourceRevision   string                        `json:"sourceRevision,omitempty"`
	CreatedAt        time.Time                     `json:"createdAt"`
	ObservedAt       time.Time                     `json:"observedAt"`
}

// RegistryProtectionSnapshot replaces all references owned by one asynchronous
// authority. Incomplete snapshots update only the authority checkpoint and
// leave the previous safe references in place.
type RegistryProtectionSnapshot struct {
	Observation RegistryAuthorityObservation `json:"observation"`
	References  []RegistryArtifactReference  `json:"references"`
}

type RegistryArtifactAvailability string

const (
	RegistryArtifactPresent RegistryArtifactAvailability = "present"
	RegistryArtifactExpired RegistryArtifactAvailability = "expired"
	RegistryArtifactMissing RegistryArtifactAvailability = "missing"
)

const (
	ProblemArtifactExpired = "ArtifactExpired"
	ProblemArtifactMissing = "ArtifactMissing"
)

type RegistryRelease struct {
	ID                     string                       `json:"id"`
	RegistryTargetID       string                       `json:"registryTargetId"`
	ServiceID              string                       `json:"serviceId"`
	Repository             string                       `json:"repository"`
	RootDigest             string                       `json:"rootDigest"`
	CreatedAt              time.Time                    `json:"createdAt"`
	SucceededAt            *time.Time                   `json:"succeededAt,omitempty"`
	Availability           RegistryArtifactAvailability `json:"availability"`
	AvailabilityObservedAt *time.Time                   `json:"availabilityObservedAt,omitempty"`
}

type RegistryCacheGeneration struct {
	ID                  string     `json:"id"`
	RegistryTargetID    string     `json:"registryTargetId"`
	ServiceID           string     `json:"serviceId"`
	Repository          string     `json:"repository"`
	PlatformSet         string     `json:"platformSet"`
	TrustLane           string     `json:"trustLane"`
	CacheSchema         string     `json:"cacheSchema"`
	BuildDefinitionHash string     `json:"buildDefinitionHash"`
	Generation          int64      `json:"generation"`
	RootDigest          string     `json:"rootDigest"`
	SizeBytes           int64      `json:"sizeBytes"`
	State               string     `json:"state"`
	ActiveImports       int        `json:"activeImports"`
	ActiveExports       int        `json:"activeExports"`
	CreatedAt           time.Time  `json:"createdAt"`
	CompletedAt         *time.Time `json:"completedAt,omitempty"`
	LastUsedAt          time.Time  `json:"lastUsedAt"`
}

// RegistryLifecycleSnapshot is a single-store consistent view used for both a
// dry-run plan and immediate pre-delete revalidation.
type RegistryLifecycleSnapshot struct {
	Target                RegistryTarget                 `json:"target"`
	Policy                ServiceRegistryPolicy          `json:"policy"`
	Inventory             RegistryInventoryObservation   `json:"inventory"`
	CatalogObservations   []RegistryCatalogObservation   `json:"catalogObservations"`
	AuthorityObservations []RegistryAuthorityObservation `json:"authorityObservations"`
	References            []RegistryArtifactReference    `json:"references"`
	Releases              []RegistryRelease              `json:"releases"`
	CacheGenerations      []RegistryCacheGeneration      `json:"cacheGenerations"`
	Manifests             []RegistryManifest             `json:"manifests"`
	Blobs                 []RegistryBlob                 `json:"blobs"`
	Children              []RegistryManifestLink         `json:"children"`
	BlobLinks             []RegistryManifestBlobLink     `json:"blobLinks"`
	AsOf                  time.Time                      `json:"asOf"`
}

type RegistryCleanupDisposition string

const (
	RegistryCleanupProtect RegistryCleanupDisposition = "protect"
	RegistryCleanupDelete  RegistryCleanupDisposition = "delete"
)

type RegistryCleanupItem struct {
	Ordinal         int                        `json:"ordinal"`
	Repository      string                     `json:"repository"`
	ResourceKind    string                     `json:"resourceKind"`
	Digest          string                     `json:"digest"`
	Disposition     RegistryCleanupDisposition `json:"disposition"`
	Action          string                     `json:"action"`
	EstimatedBytes  int64                      `json:"estimatedBytes"`
	Reasons         []string                   `json:"reasons"`
	State           string                     `json:"state"`
	ProviderMessage string                     `json:"providerMessage,omitempty"`
	UpdatedAt       time.Time                  `json:"updatedAt"`
}

type RegistryCleanupSummary struct {
	ProtectedManifests  int   `json:"protectedManifests"`
	DeletedManifests    int   `json:"deletedManifests"`
	GarbageCollectBlobs int   `json:"garbageCollectBlobs"`
	EstimatedBytes      int64 `json:"estimatedBytes"`
	CacheBytesBefore    int64 `json:"cacheBytesBefore"`
	CacheBytesAfter     int64 `json:"cacheBytesAfter"`
	CacheQuotaSatisfied bool  `json:"cacheQuotaSatisfied"`
}

type RegistryCleanupPlan struct {
	ID               string                         `json:"id"`
	RegistryTargetID string                         `json:"registryTargetId"`
	ServiceID        string                         `json:"serviceId"`
	SnapshotToken    string                         `json:"snapshotToken"`
	AuthorityToken   string                         `json:"authorityToken"`
	PlanDigest       string                         `json:"planDigest"`
	State            string                         `json:"state"`
	Policy           ServiceRegistryPolicy          `json:"policy"`
	Inventory        RegistryInventoryObservation   `json:"inventory"`
	Catalogs         []RegistryCatalogObservation   `json:"catalogs"`
	Authorities      []RegistryAuthorityObservation `json:"authorities"`
	Summary          RegistryCleanupSummary         `json:"summary"`
	Items            []RegistryCleanupItem          `json:"items"`
	CreatedAt        time.Time                      `json:"createdAt"`
	ClaimedAt        *time.Time                     `json:"claimedAt,omitempty"`
	CompletedAt      *time.Time                     `json:"completedAt,omitempty"`
	Failure          string                         `json:"failure,omitempty"`
}

type RegistryCleanupItemResult struct {
	State           string    `json:"state"`
	ProviderMessage string    `json:"providerMessage,omitempty"`
	ObservedAt      time.Time `json:"observedAt"`
}
