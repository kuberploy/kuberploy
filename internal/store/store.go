package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

var (
	ErrNotFound                      = errors.New("not found")
	ErrConflict                      = errors.New("conflict")
	ErrDeletionConfirmation          = fmt.Errorf("%w: deletion confirmation does not match", ErrConflict)
	ErrSelfDeletion                  = fmt.Errorf("%w: current user cannot delete itself", ErrConflict)
	ErrUserDeletionBlocked           = fmt.Errorf("%w: user owns a required resource or final role", ErrConflict)
	ErrTeamDeletionBlocked           = fmt.Errorf("%w: team still owns or binds resources", ErrConflict)
	ErrApplicationDeletionBlocked    = fmt.Errorf("%w: application still has dependent resources", ErrConflict)
	ErrEnvironmentDeletionBlocked    = fmt.Errorf("%w: environment still has dependent resources", ErrConflict)
	ErrBootstrapConsumed             = errors.New("bootstrap already consumed")
	ErrIdempotencyConflict           = errors.New("idempotency key reused with different input")
	ErrForbidden                     = errors.New("forbidden")
	ErrInvitationInvalid             = errors.New("invitation is invalid, expired, or already used")
	ErrRegistryExternalLifecycle     = errors.New("registry lifecycle is operator managed")
	ErrRegistryObservationIncomplete = errors.New("registry lifecycle observation is incomplete")
	ErrRegistrySnapshotStale         = errors.New("registry lifecycle snapshot is stale")
	ErrRegistryGraphInvalid          = errors.New("registry catalog graph is invalid")
	ErrRegistryPolicyInvalid         = errors.New("registry lifecycle policy is invalid")
	ErrRegistryLeaseLost             = errors.New("registry cleanup lease is not held")
	ErrPreconditionFailed            = errors.New("configuration precondition failed")
	ErrPreviewInvalid                = errors.New("configuration preview token is invalid")
	ErrPreviewExpired                = errors.New("configuration preview token expired")
	ErrPreviewConsumed               = errors.New("configuration preview token was already consumed")
	ErrConfigProjectionMissing       = errors.New("deployment configuration projection is missing")
	ErrOperationLeaseLost            = errors.New("operation lease is not held")
)

type Result[T any] struct {
	Value  T
	Replay bool
}

// BuildLogAttemptOwnership is the immutable authorization edge copied from a
// source-build attempt. It contains no Kubernetes identity, log reference, or
// build input. PostgreSQL resolves this edge directly in its audit transaction;
// the in-memory store consumes it through BuildLogAttemptCatalog.
type BuildLogAttemptOwnership struct {
	AttemptID     string
	ProjectID     string
	ApplicationID string
}

type BuildLogAttemptCatalog interface {
	BuildLogAttemptOwnership(context.Context, string) (BuildLogAttemptOwnership, error)
}

// AppConfigReferencePlan carries only immutable, non-secret identities resolved
// from one exact candidate. PostgreSQL repeats the resolution inside the same
// transaction as the Git write command before reconciling deletion guards.
type AppConfigReferencePlan struct {
	// RuntimeSecretDigest is the legacy field name for the digest of every
	// metadata-only SecretBindingRef in the exact AppConfig, including workload
	// environment values and reusable BasicAuth middleware. PostgreSQL
	// re-resolves the immutable candidate under row locks and requires the same
	// combined digest.
	RuntimeSecretDigest string
	// CertificateDigest binds every custom-certificate route to its exact
	// application/environment/namespace, active immutable version, hostname
	// coverage, and fresh observation identity.
	CertificateDigest string
}

func (p *AppConfigReferencePlan) Validate() error {
	if p == nil || p.RuntimeSecretDigest == "" && p.CertificateDigest == "" ||
		p.RuntimeSecretDigest != "" && !validSHA256Digest(p.RuntimeSecretDigest) ||
		p.CertificateDigest != "" && !validSHA256Digest(p.CertificateDigest) {
		return ErrPreconditionFailed
	}
	return nil
}

// NormalizeAppConfigReferencePlan closes the optional variadic compatibility
// seam to exactly zero or one plan. References are meaningful only beside an
// immutable Git write plan because Git is the desired-state authority.
func NormalizeAppConfigReferencePlan(projection *gitprojection.WritePlan, plans []*AppConfigReferencePlan) (*AppConfigReferencePlan, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	if len(plans) == 1 && plans[0] == nil {
		return nil, nil
	}
	if len(plans) != 1 || projection == nil || plans[0].Validate() != nil {
		return nil, ErrPreconditionFailed
	}
	return plans[0], nil
}

func AppConfigUsesRuntimeSecrets(runtime domain.WorkloadRuntime) bool {
	for _, variable := range runtime.Env {
		if variable.ValueFrom != nil {
			return true
		}
	}
	return false
}

func validSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size && value == "sha256:"+hex.EncodeToString(decoded)
}

// ExactSHA256Matches verifies identity against the original bytes. It is kept
// in the durable store layer so implementations cannot accidentally substitute
// a JSON reserialization for the immutable release asset.
func ExactSHA256Matches(body []byte, digest string) bool {
	if len(body) == 0 || len(body) > 256<<10 {
		return false
	}
	sum := sha256.Sum256(body)
	return digest == "sha256:"+hex.EncodeToString(sum[:])
}

type Store interface {
	RegistryStore
	ExternalDNSStore
	Ping(context.Context) error
	Close()
	BootstrapRequired(context.Context) (bool, error)
	BootstrapAdmin(context.Context, domain.User, string, []byte, time.Time) error
	UserBySession(context.Context, []byte, time.Time) (domain.User, error)
	RevokeSession(context.Context, []byte) error
	CreateUserInvitation(context.Context, string, string, []byte, time.Time, string) (domain.UserInvitation, error)
	AcceptUserInvitation(context.Context, []byte, string, string, []byte, []byte, time.Time) (domain.User, error)
	LocalCredential(context.Context, string) (domain.User, string, error)
	CreateLoginSession(context.Context, string, string, string, []byte, time.Time) (domain.User, error)
	ListUsersForActor(context.Context, string) ([]domain.User, error)
	DeleteUser(context.Context, string, string, string, string, string, string) (bool, error)

	CreateTeam(context.Context, string, string, string, string, domain.CreateTeam) (Result[domain.Team], error)
	ListTeamsForActor(context.Context, string) ([]domain.Team, error)
	DeleteTeam(context.Context, string, string, string, string, string, string) (bool, error)
	ListTeamMembersForActor(context.Context, string, string) ([]domain.TeamMember, error)
	AddTeamMember(context.Context, string, string, string, string, string, domain.AddTeamMember) (Result[domain.TeamMember], error)
	RemoveTeamMember(context.Context, string, string, string, string) error

	CreateGitHubInstallation(context.Context, string, string, string, string, domain.CreateGitHubInstallation) (Result[domain.GitHubInstallation], error)
	LinkVerifiedGitHubInstallation(context.Context, string, string, string, string, domain.CreateGitHubInstallation) (domain.GitHubInstallation, bool, error)
	ListGitHubInstallationsForActor(context.Context, string) ([]domain.GitHubInstallation, error)
	AuthorizeGitHubInstallationForProject(context.Context, string, string, string) error
	UpdateGitHubInstallationSharing(context.Context, string, string, string, string, string, domain.UpdateGitHubInstallationSharing) (Result[domain.GitHubInstallation], error)
	Authorize(context.Context, string, domain.Permission, domain.AccessTarget) error
	// AuthorizePromotion evaluates resources.write against the exact composite
	// environment/application target used by deployment creation. It preserves
	// application-scoped grants without accepting a caller-supplied project.
	AuthorizePromotion(context.Context, string, string, string) error
	EffectiveCapabilities(context.Context, string) ([]domain.AccessCapability, error)
	ListAuditEventsForActor(context.Context, string, domain.AuditEventQuery) ([]domain.AuditEvent, error)
	ListRegistryTargetsForActor(context.Context, string) ([]domain.RegistryTarget, error)
	CreateRegistryTargetForActor(context.Context, string, string, string, string, domain.RegistryTarget) (Result[domain.RegistryTarget], error)
	UpdateRegistryTargetForActor(context.Context, string, string, string, string, domain.RegistryTarget) (Result[domain.RegistryTarget], error)
	RegistryLifecycleSnapshotsForActor(context.Context, string, string, time.Time) ([]domain.RegistryLifecycleSnapshot, error)
	PutServiceRegistryPolicyForActor(context.Context, string, string, string, string, string, domain.ServiceRegistryPolicy) (Result[domain.ServiceRegistryPolicy], error)
	ListProjectRegistryPullCredentialsForActor(context.Context, string, string) ([]domain.ProjectRegistryPullCredential, error)
	ListRegistryPullTargetsForActor(context.Context, string, string) ([]domain.RegistryTarget, error)
	CreateProjectRegistryPullCredentialForActor(context.Context, string, string, string, string, domain.ProjectRegistryPullCredential) (Result[domain.ProjectRegistryPullCredential], error)
	DeleteProjectRegistryPullCredentialForActor(context.Context, string, string, string, string, string, string) (bool, error)
	ApplicationRegistryPullSelectionForActor(context.Context, string, string) (domain.ApplicationRegistryPullSelection, error)
	PutApplicationRegistryPullSelectionForActor(context.Context, string, string, string, string, domain.ApplicationRegistryPullSelection) (Result[domain.ApplicationRegistryPullSelection], error)
	SaveRegistryCleanupPreviewForActor(context.Context, string, string, string, string, string, domain.RegistryCleanupPlan) (Result[domain.RegistryCleanupPlan], error)
	RegistryCleanupPlanForActor(context.Context, string, string) (domain.RegistryCleanupPlan, error)
	PrepareRegistryCleanupExecutionForActor(context.Context, string, string, string, string, string, string) (Result[domain.RegistryCleanupPlan], error)
	ListProjectAccessGrants(context.Context, string, string) ([]domain.AccessGrant, error)
	CreateProjectAccessGrant(context.Context, string, string, string, string, domain.CreateAccessGrant) (Result[domain.AccessGrant], error)
	DeleteProjectAccessGrant(context.Context, string, string, string, string, string, string) (bool, error)
	CreateServiceAccount(context.Context, string, string, string, string, domain.CreateServiceAccount) (Result[domain.ServiceAccount], error)
	ListServiceAccounts(context.Context, string, string) ([]domain.ServiceAccount, error)
	CreateServiceAccountToken(context.Context, string, string, string, string, domain.CreateServiceAccountToken) (Result[domain.ServiceAccountToken], error)
	// ServiceAccountTokenReplay checks authorized idempotency before the API
	// allocates fresh credential entropy. It can recover metadata after a lost
	// first response, but never has access to or re-discloses the raw token.
	ServiceAccountTokenReplay(context.Context, string, string, string, string) (Result[domain.ServiceAccountToken], bool, error)
	ListServiceAccountTokens(context.Context, string, string) ([]domain.ServiceAccountToken, error)
	RevokeServiceAccountToken(context.Context, string, string, string, string, string, string) (bool, error)
	DisableServiceAccount(context.Context, string, string, string, string, string) (bool, error)
	ServiceAccountByToken(context.Context, []byte, time.Time) (domain.AutomationPrincipal, error)

	CreateProject(context.Context, string, string, string, domain.CreateProject) (Result[domain.Project], error)
	ListProjects(context.Context) ([]domain.Project, error)
	GetProject(context.Context, string) (domain.Project, error)
	ListProjectsForActor(context.Context, string) ([]domain.Project, error)
	GetProjectForActor(context.Context, string, string) (domain.Project, error)
	CreateEnvironment(context.Context, string, string, string, domain.CreateEnvironment) (Result[domain.Environment], error)
	CloneEnvironment(context.Context, string, string, string, string, domain.CloneEnvironment) (Result[domain.EnvironmentCloneResult], error)
	ListEnvironments(context.Context) ([]domain.Environment, error)
	GetEnvironment(context.Context, string) (domain.Environment, error)
	ListEnvironmentsForActor(context.Context, string) ([]domain.Environment, error)
	GetEnvironmentForActor(context.Context, string, string) (domain.Environment, error)
	DeleteEnvironment(context.Context, string, string, string, string, string, string) (bool, error)
	ListEnvironmentAppPlacementsForActor(context.Context, string, string) ([]domain.EnvironmentAppPlacement, error)
	CreateEnvironmentGitBinding(context.Context, string, string, string, string, gitprojection.CreateEnvironmentBindingInput) (Result[gitprojection.Binding], error)
	GetEnvironmentGitBindingForActor(context.Context, string, string) (gitprojection.Binding, error)
	CreatePlatformGitBinding(context.Context, string, string, string, string, gitprojection.CreatePlatformBindingInput) (Result[gitprojection.Binding], error)
	GetPlatformGitBindingForActor(context.Context, string) (gitprojection.Binding, error)
	CreateApplication(context.Context, string, string, string, domain.CreateApplication) (Result[domain.Application], error)
	ListApplications(context.Context) ([]domain.Application, error)
	GetApplication(context.Context, string) (domain.Application, error)
	ListApplicationsForActor(context.Context, string) ([]domain.Application, error)
	GetApplicationForActor(context.Context, string, string) (domain.Application, error)
	DeleteApplication(context.Context, string, string, string, string, string, string) (bool, error)

	CreateDeployment(context.Context, string, string, string, string, domain.CreateDeployment, *gitprojection.WritePlan, ...*AppConfigReferencePlan) (Result[domain.Deployment], domain.Operation, error)
	ListDeployments(context.Context) ([]domain.Deployment, error)
	GetDeployment(context.Context, string) (domain.Deployment, error)
	ListDeploymentsForActor(context.Context, string) ([]domain.Deployment, error)
	GetDeploymentForActor(context.Context, string, string) (domain.Deployment, error)
	GetDeploymentForOperation(context.Context, string) (domain.Deployment, error)
	DeploymentStatus(context.Context, string) (domain.DeploymentStatus, error)
	DeploymentStatusForActor(context.Context, string, string) (domain.DeploymentStatus, error)
	// AuditRuntimeAccess authorizes an exact deployment logs.read request and
	// durably records the bounded runtime read before Kubernetes is contacted.
	AuditRuntimeAccess(context.Context, string, string, string, string) error
	// AuditBuildLogAccess freshly resolves an exact build attempt and its
	// application/project, reauthorizes builds.read plus logs.read, and durably
	// records the bounded read before Kubernetes is contacted.
	AuditBuildLogAccess(context.Context, string, string, string, string) error
	GetDeploymentConfigForActor(context.Context, string, string) (domain.DeploymentConfig, error)
	CreateDeploymentConfigPreview(context.Context, string, domain.CreateConfigPreview, *gitprojection.WritePlan, ...*AppConfigReferencePlan) error
	SaveDeploymentConfig(context.Context, string, string, string, string, domain.SaveDeploymentConfig, *gitprojection.WritePlan, ...*AppConfigReferencePlan) (Result[domain.Deployment], domain.Operation, error)
	CreateVariableSetPreview(context.Context, string, gitprojection.WritePlan, []byte, []byte, time.Time) error
	VariableSetPreviewAuthority(context.Context, string, []byte) (gitprojection.WritePlan, []byte, error)
	SaveVariableSet(context.Context, string, string, string, string, gitprojection.WritePlan, []byte, []byte, []byte) (Result[domain.Operation], error)
	GetOperation(context.Context, string) (domain.Operation, error)
	ListOperations(context.Context) ([]domain.Operation, error)
	GetOperationForActor(context.Context, string, string) (domain.Operation, error)
	ListOperationsForActor(context.Context, string) ([]domain.Operation, error)
	PendingOutbox(context.Context, int) ([]domain.WorkMessage, error)
	MarkOutboxPublished(context.Context, string) error
	MarkOutboxFailure(context.Context, string, string) error
	LeasePendingOperations(context.Context, string, int, time.Duration) ([]domain.WorkMessage, error)
	StartOperation(context.Context, string, int64, string, time.Duration) (domain.Operation, bool, error)
	HeartbeatOperation(context.Context, string, int64, string, time.Duration) error
	CompleteGitOperation(context.Context, string, int64, string, domain.GitPublicationResult) error
	RequeueOperation(context.Context, string, int64, string, string, string) error
	FailOperation(context.Context, string, int64, string, string, string) error
}
