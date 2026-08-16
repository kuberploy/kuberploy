// Package builds owns durable GitHub-delivery and source-build orchestration.
// It persists only identities, immutable build inputs, and opaque references;
// provider and registry credential bytes never cross the store boundary.
package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
)

var (
	ErrNotFound       = errors.New("build orchestration record not found")
	ErrConflict       = errors.New("build orchestration conflict")
	ErrUnauthorized   = errors.New("GitHub source is not authorized")
	ErrLeaseHeld      = errors.New("build orchestration lease is held")
	ErrLeaseLost      = errors.New("build orchestration lease was lost")
	ErrTerminal       = errors.New("build attempt is terminal")
	ErrInvalid        = errors.New("invalid build orchestration input")
	ErrProviderRetry  = errors.New("GitHub provider operation should be retried")
	ErrInfrastructure = errors.New("build infrastructure operation failed")

	uuidRE       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	kubeNameRE   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	loginRE      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)
	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	permissionRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	failureRE    = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	logRefRE     = regexp.MustCompile(`^k8s://[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?/pods/[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?/containers/agent$`)
)

type InstallationLifecycle string

const (
	InstallationPending   InstallationLifecycle = "pending-verification"
	InstallationActive    InstallationLifecycle = "active"
	InstallationSuspended InstallationLifecycle = "suspended"
	InstallationDeleted   InstallationLifecycle = "deleted"
)

type Installation struct {
	ID                   string
	AppID                int64
	GitHubInstallationID int64
	Account              githubapp.AccountIdentity
	RepositorySelection  string
	Permissions          githubapp.Permissions
	Lifecycle            InstallationLifecycle
	SuspendedAt          *time.Time
	DeletedAt            *time.Time
	LastVerifiedAt       time.Time
	UpdatedAt            time.Time
}

type RepositoryLifecycle string

const (
	RepositoryActive  RepositoryLifecycle = "active"
	RepositoryRemoved RepositoryLifecycle = "removed"
)

type Repository struct {
	ID             string
	InstallationID string
	Identity       githubapp.RepositoryIdentity
	Lifecycle      RepositoryLifecycle
	LastVerifiedAt time.Time
	RemovedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type RegistryMode string

const (
	RegistryManaged  RegistryMode = "managed"
	RegistryExternal RegistryMode = "external"
)

type RegistryBinding struct {
	TargetID              string       `json:"targetId"`
	Mode                  RegistryMode `json:"mode"`
	Server                string       `json:"server"`
	RepositoryPrefix      string       `json:"repositoryPrefix"`
	PushCredentialSecret  string       `json:"pushCredentialSecret"`
	CacheCredentialSecret string       `json:"cacheCredentialSecret"`
}

// ExecutionSettings are operator-owned settings copied into every immutable
// attempt. They contain Secret object names and file paths, never secret data.
type ExecutionSettings struct {
	Namespace               string                     `json:"namespace"`
	PodServiceAccount       string                     `json:"podServiceAccount"`
	BuilderAgentImage       string                     `json:"builderAgentImage"`
	BuildKitImage           string                     `json:"buildKitImage"`
	DinDImage               string                     `json:"dindImage,omitempty"`
	BuildSecret             string                     `json:"buildSecret,omitempty"`
	SSHSecret               string                     `json:"sshSecret,omitempty"`
	NodeSelector            map[string]string          `json:"nodeSelector"`
	Toleration              builder.TaintToleration    `json:"toleration"`
	CheckoutResources       builder.ContainerResources `json:"checkoutResources"`
	DinDResources           builder.ContainerResources `json:"dindResources"`
	AgentResources          builder.ContainerResources `json:"agentResources"`
	WorkspaceSizeLimit      string                     `json:"workspaceSizeLimit"`
	SocketSizeLimit         string                     `json:"socketSizeLimit"`
	ResultSizeLimit         string                     `json:"resultSizeLimit"`
	DockerDataSizeLimit     string                     `json:"dockerDataSizeLimit"`
	ActiveDeadlineSeconds   int64                      `json:"activeDeadlineSeconds"`
	TTLSecondsAfterFinished int64                      `json:"ttlSecondsAfterFinished"`
	Egress                  []builder.EgressEndpoint   `json:"egress"`
}

// DefinitionSpec is the complete closed build definition. MaxAttempts is for
// infrastructure retries of the same immutable operation; it does not change
// the build generation, destination, or cache candidate.
type DefinitionSpec struct {
	ContextPath    string                  `json:"contextPath"`
	DockerfilePath string                  `json:"dockerfilePath"`
	Platforms      []string                `json:"platforms"`
	Registry       RegistryBinding         `json:"registry"`
	BuildArgs      []builder.BuildArg      `json:"buildArgs,omitempty"`
	SecretFiles    []builder.FileReference `json:"secretFiles,omitempty"`
	SSHFiles       []builder.FileReference `json:"sshFiles,omitempty"`
	CacheTrustLane string                  `json:"cacheTrustLane"`
	CacheImports   int                     `json:"cacheImports"`
	Profile        builder.BuildProfile    `json:"profile"`
	Execution      ExecutionSettings       `json:"execution"`
	MaxAttempts    int                     `json:"maxAttempts"`
}

type BuildDefinition struct {
	ID                   string
	ProjectID            string
	ServiceID            string
	InstallationID       string
	RepositoryID         string
	TriggerRef           string
	Spec                 DefinitionSpec
	DefinitionDigest     string
	DefinitionGeneration int64
	Enabled              bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// AttemptDefinition keeps the authorized immutable user definition separate
// from the current operator-owned execution settings snapshotted by a new
// attempt. This prevents platform upgrades from replaying stale runtime images
// or placement policy while preserving historical attempts exactly.
type AttemptDefinition struct {
	Definition BuildDefinition
	Execution  ExecutionSettings
}

type DeliveryState string

const (
	DeliveryClaimed    DeliveryState = "claimed"
	DeliveryProcessing DeliveryState = "processing"
	DeliveryEnqueued   DeliveryState = "enqueued"
	DeliveryIgnored    DeliveryState = "ignored"
	DeliveryFailed     DeliveryState = "failed"
)

type DeliveryReceipt struct {
	ClaimKey             string
	AppID                int64
	GitHubInstallationID int64
	DeliveryID           string
	Event                string
	BodySHA256           string
	TypedEvent           []byte
	RepositoryID         int64
	GitRef               string
	State                DeliveryState
	FailureCode          string
	LeaseOwner           string
	LeaseUntil           time.Time
	AvailableAt          time.Time
	ReceivedAt           time.Time
	CompletedAt          *time.Time
	UpdatedAt            time.Time
}

type AttemptState string

const (
	AttemptQueued     AttemptState = "queued"
	AttemptPreparing  AttemptState = "preparing"
	AttemptRunning    AttemptState = "running"
	AttemptCancelling AttemptState = "cancelling"
	AttemptSucceeded  AttemptState = "succeeded"
	AttemptFailed     AttemptState = "failed"
	AttemptCancelled  AttemptState = "cancelled"
)

type BuildAttempt struct {
	ID                string
	DefinitionID      string
	DeliveryClaimKey  string
	ProjectID         string
	ServiceID         string
	CommitSHA         string
	GitRef            string
	Generation        int64
	DefinitionDigest  string
	PlanRequest       builder.JobPlanRequest
	CheckoutRequest   builder.CheckoutRequest
	InputDigest       string
	RegistryMode      RegistryMode
	State             AttemptState
	ExecutionAttempts int
	MaxAttempts       int
	AvailableAt       time.Time
	LeaseOwner        string
	LeaseUntil        time.Time
	JobNamespace      string
	JobName           string
	CacheCandidate    string
	CacheReference    string
	Result            *builder.BuildResult
	LogReference      string
	FailureCode       string
	CancelRequestedAt *time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type AuthorizedPush struct {
	Installation Installation
	Repository   Repository
	Definitions  []BuildDefinition
}

type EnqueuePush struct {
	ClaimKey   string
	CommitSHA  string
	GitRef     string
	ResolvedAt time.Time
}

type OutboxMessage struct {
	AttemptID   string
	Kind        string
	TraceID     string
	Attempts    int
	AvailableAt time.Time
	PublishedAt *time.Time
}

func (i Installation) validate() error {
	if !uuidRE.MatchString(i.ID) || i.AppID <= 0 || i.GitHubInstallationID <= 0 || !validAccount(i.Account) ||
		(i.RepositorySelection != "all" && i.RepositorySelection != "selected") || i.LastVerifiedAt.IsZero() || i.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if i.Lifecycle != InstallationActive && i.Lifecycle != InstallationSuspended && i.Lifecycle != InstallationDeleted {
		return ErrInvalid
	}
	if len(i.Permissions) == 0 || len(i.Permissions) > 64 {
		return ErrInvalid
	}
	for name, level := range i.Permissions {
		if name == "" || (level != githubapp.PermissionNone && level != githubapp.PermissionRead && level != githubapp.PermissionWrite) {
			return ErrInvalid
		}
	}
	if (i.Lifecycle == InstallationSuspended) != (i.SuspendedAt != nil) || (i.Lifecycle == InstallationDeleted) != (i.DeletedAt != nil) {
		return ErrInvalid
	}
	return nil
}

func (r Repository) validate() error {
	if !uuidRE.MatchString(r.ID) || !uuidRE.MatchString(r.InstallationID) || !validRepository(r.Identity) || r.LastVerifiedAt.IsZero() || r.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if r.Lifecycle != RepositoryActive && r.Lifecycle != RepositoryRemoved {
		return ErrInvalid
	}
	if (r.Lifecycle == RepositoryRemoved) != (r.RemovedAt != nil) {
		return ErrInvalid
	}
	return nil
}

func validAccount(account githubapp.AccountIdentity) bool {
	return account.ID > 0 && loginRE.MatchString(account.Login) && (account.Type == "User" || account.Type == "Organization")
}

func validRepository(repository githubapp.RepositoryIdentity) bool {
	return repository.ID > 0 && repository.OwnerID > 0 && loginRE.MatchString(repository.OwnerLogin) &&
		repositoryRE.MatchString(repository.Name) && repository.Name != "." && repository.Name != ".." &&
		!strings.HasSuffix(strings.ToLower(repository.Name), ".git")
}

func validPermissions(permissions githubapp.Permissions) bool {
	if len(permissions) == 0 || len(permissions) > 64 {
		return false
	}
	for name, level := range permissions {
		if !permissionRE.MatchString(name) || (level != githubapp.PermissionNone && level != githubapp.PermissionRead && level != githubapp.PermissionWrite) {
			return false
		}
	}
	return true
}

func validInstallationEvent(event githubapp.InstallationEvent) bool {
	if event.InstallationID <= 0 || !validAccount(event.Account) || (event.RepositorySelection != "all" && event.RepositorySelection != "selected") || !validPermissions(event.Permissions) {
		return false
	}
	switch event.Action {
	case "created", "deleted", "suspend", "unsuspend", "new_permissions_accepted":
		return true
	default:
		return false
	}
}

func validRepositoryEvent(event githubapp.InstallationRepositoriesEvent) bool {
	if event.InstallationID <= 0 || !validAccount(event.Account) || (event.RepositorySelection != "all" && event.RepositorySelection != "selected") {
		return false
	}
	identities := event.Added
	if event.Action == "removed" {
		identities = event.Removed
	} else if event.Action != "added" {
		return false
	}
	if len(identities) == 0 {
		return false
	}
	seen := map[int64]struct{}{}
	for _, identity := range identities {
		if !validRepository(identity) || identity.OwnerID != event.Account.ID || !strings.EqualFold(identity.OwnerLogin, event.Account.Login) {
			return false
		}
		if _, ok := seen[identity.ID]; ok {
			return false
		}
		seen[identity.ID] = struct{}{}
	}
	return (event.Action == "added" && len(event.Removed) == 0) || (event.Action == "removed" && len(event.Added) == 0)
}

func validPushEvent(event githubapp.PushEvent) bool {
	if event.InstallationID <= 0 || !validGitRef(event.Ref) || !validRepository(event.Repository) || (len(event.UntrustedAfter) != 40 && len(event.UntrustedAfter) != 64) || strings.Trim(event.UntrustedAfter, "0123456789abcdef") != "" {
		return false
	}
	allZero := strings.Trim(event.UntrustedAfter, "0") == ""
	return (event.Deleted && allZero) || (!event.Deleted && !allZero)
}

func (d BuildDefinition) validate() error {
	if !uuidRE.MatchString(d.ID) || !uuidRE.MatchString(d.ProjectID) || !uuidRE.MatchString(d.ServiceID) ||
		!uuidRE.MatchString(d.InstallationID) || !uuidRE.MatchString(d.RepositoryID) || !validGitRef(d.TriggerRef) ||
		d.DefinitionGeneration < 1 || d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if d.Spec.Execution.BuildKitImage == "" {
		digest, err := legacyDefinitionDigestWithoutBuildKit(d.Spec)
		if err != nil || d.DefinitionDigest != digest {
			return ErrInvalid
		}
		// Definitions accepted before BuildKit became an explicit execution
		// setting retain their original immutable digest. Every new attempt
		// already replaces the complete operator-owned Execution snapshot from
		// the current validated runtime, including BuildKitImage.
		upgraded := d.Spec
		upgraded.Execution.BuildKitImage = builder.DefaultBuildKitImage
		return validateDefinitionSpec(d.ProjectID, d.ServiceID, upgraded)
	}
	digest, err := definitionDigest(d.Spec)
	if err != nil || d.DefinitionDigest != digest {
		return ErrInvalid
	}
	return validateDefinitionSpec(d.ProjectID, d.ServiceID, d.Spec)
}

func validateDefinitionSpec(projectID, serviceID string, spec DefinitionSpec) error {
	if !uuidRE.MatchString(spec.Registry.TargetID) || (spec.Registry.Mode != RegistryManaged && spec.Registry.Mode != RegistryExternal) ||
		!kubeNameRE.MatchString(spec.Registry.PushCredentialSecret) ||
		!kubeNameRE.MatchString(spec.Registry.CacheCredentialSecret) ||
		spec.Registry.PushCredentialSecret == spec.Registry.CacheCredentialSecret ||
		spec.CacheImports < 1 || spec.CacheImports > 8 ||
		spec.MaxAttempts < 1 || spec.MaxAttempts > 5 {
		return ErrInvalid
	}
	if server, err := registryServer(spec.Registry.Server); err != nil || server != spec.Registry.Server {
		return ErrInvalid
	}
	if spec.Execution.BuildSecret == "" && len(spec.SecretFiles) != 0 || spec.Execution.SSHSecret == "" && len(spec.SSHFiles) != 0 {
		return ErrInvalid
	}
	// A generated request exercises the builder's closed validation contract.
	request := generatedBuildRequest("11111111-1111-4111-8111-111111111111", 1, projectID, serviceID, strings.Repeat("a", 40), spec, nil, "sha256:"+strings.Repeat("b", 64))
	if err := request.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	job := jobPlanRequest(request, spec, "11111111-1111-4111-8111-111111111111")
	if _, err := builder.PlanJob(job); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func definitionDigest(spec DefinitionSpec) (string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// legacyDefinitionDigestWithoutBuildKit reconstructs the exact JSON shape
// used before ExecutionSettings gained buildKitImage. PostgreSQL stores the
// specification as jsonb, so recomputing this from the closed typed model is
// safer than trusting jsonb's reordered textual representation.
func legacyDefinitionDigestWithoutBuildKit(spec DefinitionSpec) (string, error) {
	type legacyExecutionSettings struct {
		Namespace               string                     `json:"namespace"`
		PodServiceAccount       string                     `json:"podServiceAccount"`
		BuilderAgentImage       string                     `json:"builderAgentImage"`
		BuildSecret             string                     `json:"buildSecret,omitempty"`
		SSHSecret               string                     `json:"sshSecret,omitempty"`
		NodeSelector            map[string]string          `json:"nodeSelector"`
		Toleration              builder.TaintToleration    `json:"toleration"`
		CheckoutResources       builder.ContainerResources `json:"checkoutResources"`
		DinDResources           builder.ContainerResources `json:"dindResources"`
		AgentResources          builder.ContainerResources `json:"agentResources"`
		WorkspaceSizeLimit      string                     `json:"workspaceSizeLimit"`
		SocketSizeLimit         string                     `json:"socketSizeLimit"`
		ResultSizeLimit         string                     `json:"resultSizeLimit"`
		DockerDataSizeLimit     string                     `json:"dockerDataSizeLimit"`
		ActiveDeadlineSeconds   int64                      `json:"activeDeadlineSeconds"`
		TTLSecondsAfterFinished int64                      `json:"ttlSecondsAfterFinished"`
		Egress                  []builder.EgressEndpoint   `json:"egress"`
	}
	type legacyDefinitionSpec struct {
		ContextPath    string                  `json:"contextPath"`
		DockerfilePath string                  `json:"dockerfilePath"`
		Platforms      []string                `json:"platforms"`
		Registry       RegistryBinding         `json:"registry"`
		BuildArgs      []builder.BuildArg      `json:"buildArgs,omitempty"`
		SecretFiles    []builder.FileReference `json:"secretFiles,omitempty"`
		SSHFiles       []builder.FileReference `json:"sshFiles,omitempty"`
		CacheTrustLane string                  `json:"cacheTrustLane"`
		CacheImports   int                     `json:"cacheImports"`
		Profile        builder.BuildProfile    `json:"profile"`
		Execution      legacyExecutionSettings `json:"execution"`
		MaxAttempts    int                     `json:"maxAttempts"`
	}
	execution := spec.Execution
	encoded, err := json.Marshal(legacyDefinitionSpec{
		ContextPath: spec.ContextPath, DockerfilePath: spec.DockerfilePath, Platforms: spec.Platforms,
		Registry: spec.Registry, BuildArgs: spec.BuildArgs, SecretFiles: spec.SecretFiles, SSHFiles: spec.SSHFiles,
		CacheTrustLane: spec.CacheTrustLane, CacheImports: spec.CacheImports, Profile: spec.Profile,
		Execution: legacyExecutionSettings{
			Namespace: execution.Namespace, PodServiceAccount: execution.PodServiceAccount, BuilderAgentImage: execution.BuilderAgentImage,
			BuildSecret: execution.BuildSecret, SSHSecret: execution.SSHSecret, NodeSelector: execution.NodeSelector, Toleration: execution.Toleration,
			CheckoutResources: execution.CheckoutResources, DinDResources: execution.DinDResources, AgentResources: execution.AgentResources,
			WorkspaceSizeLimit: execution.WorkspaceSizeLimit, SocketSizeLimit: execution.SocketSizeLimit, ResultSizeLimit: execution.ResultSizeLimit,
			DockerDataSizeLimit: execution.DockerDataSizeLimit, ActiveDeadlineSeconds: execution.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: execution.TTLSecondsAfterFinished, Egress: execution.Egress,
		},
		MaxAttempts: spec.MaxAttempts,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func PrepareDefinition(definition BuildDefinition, now time.Time) (BuildDefinition, error) {
	definition.Spec.Platforms = slices.Clone(definition.Spec.Platforms)
	slices.Sort(definition.Spec.Platforms)
	definition.Spec.BuildArgs = slices.Clone(definition.Spec.BuildArgs)
	slices.SortFunc(definition.Spec.BuildArgs, func(a, b builder.BuildArg) int { return strings.Compare(a.Name, b.Name) })
	definition.Spec.SecretFiles = slices.Clone(definition.Spec.SecretFiles)
	slices.SortFunc(definition.Spec.SecretFiles, func(a, b builder.FileReference) int { return strings.Compare(a.ID, b.ID) })
	definition.Spec.SSHFiles = slices.Clone(definition.Spec.SSHFiles)
	slices.SortFunc(definition.Spec.SSHFiles, func(a, b builder.FileReference) int { return strings.Compare(a.ID, b.ID) })
	definition.Spec.Execution.Egress = slices.Clone(definition.Spec.Execution.Egress)
	for index := range definition.Spec.Execution.Egress {
		definition.Spec.Execution.Egress[index].Ports = slices.Clone(definition.Spec.Execution.Egress[index].Ports)
		slices.Sort(definition.Spec.Execution.Egress[index].Ports)
	}
	slices.SortFunc(definition.Spec.Execution.Egress, func(a, b builder.EgressEndpoint) int { return strings.Compare(a.CIDR, b.CIDR) })
	if definition.DefinitionGeneration == 0 {
		definition.DefinitionGeneration = 1
	}
	if definition.CreatedAt.IsZero() {
		definition.CreatedAt = now.UTC()
	}
	definition.UpdatedAt = now.UTC()
	digest, err := definitionDigest(definition.Spec)
	if err != nil {
		return BuildDefinition{}, err
	}
	definition.DefinitionDigest = digest
	if err := definition.validate(); err != nil {
		return BuildDefinition{}, err
	}
	return definition, nil
}

func registryServer(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\r\n") {
		return "", ErrInvalid
	}
	if !strings.Contains(raw, "://") {
		candidate, err := url.Parse("https://" + raw)
		if err != nil || candidate.Host != raw || candidate.Path != "" || candidate.User != nil {
			return "", ErrInvalid
		}
		return candidate.Host, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrInvalid
	}
	return u.Host, nil
}

// RegistryServer canonicalizes an operator-owned registry endpoint to the
// host[:port] form copied into immutable build definitions.
func RegistryServer(raw string) (string, error) { return registryServer(raw) }

func validGitRef(ref string) bool {
	var name string
	if strings.HasPrefix(ref, "refs/heads/") {
		name = strings.TrimPrefix(ref, "refs/heads/")
	} else if strings.HasPrefix(ref, "refs/tags/") {
		name = strings.TrimPrefix(ref, "refs/tags/")
	} else {
		return false
	}
	if name == "" || name == "@" || len(name) > 255 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "//") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") || strings.HasSuffix(strings.ToLower(name), ".lock") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return !strings.ContainsAny(name, " ~^:?*[\\\x00\r\n\t")
}

func validateFailureCode(code string) error {
	if !failureRE.MatchString(code) {
		return ErrInvalid
	}
	return nil
}
