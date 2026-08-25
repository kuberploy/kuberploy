package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"time"

	contract "github.com/kuberploy/kuberploy/api"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/buildlogs"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/deploymentrollback"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/emailaddr"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/observability"
	"github.com/kuberploy/kuberploy/internal/operationcache"
	"github.com/kuberploy/kuberploy/internal/passwordauth"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/releases"
	"github.com/kuberploy/kuberploy/internal/runtimeview"
	"github.com/kuberploy/kuberploy/internal/store"
	appschema "github.com/kuberploy/kuberploy/schema"
)

const sessionCookie = "kuberploy_session"
const csrfCookie = "kuberploy_csrf"
const githubSetupSessionCookie = "kuberploy_github_setup_session"
const githubSetupHandoffCookie = "kuberploy_github_setup_handoff"
const githubInstallReturnCookiePath = "/v1/github/installations/setup"
const githubOAuthCallbackCookiePath = "/v1/github/installations/callback"
const githubSetupLinkCookiePath = "/v1/github/installations/link"
const githubSetupCompletePath = "/github/setup/complete"
const githubSetupCookieTTL = 15 * time.Minute

type contextKey int

const (
	requestIDKey contextKey = iota
	userKey
	sessionHashKey
	authenticationKey
)

type ReleaseService interface {
	Latest(context.Context) (releases.Snapshot, error)
}

type MetricsService interface {
	Probe(context.Context) error
	QueryRange(context.Context, observability.Scope, observability.MetricKey, observability.Range) (observability.Result, error)
}

type RuntimeViewService interface {
	Snapshot(context.Context, runtimeview.SnapshotRequest) (runtimeview.LogSnapshot, error)
	Events(context.Context, runtimeview.EventRequest) (runtimeview.EventSnapshot, error)
	Follow(context.Context, runtimeview.FollowRequest) (RuntimeLogStream, error)
	Rollout(context.Context, string) (runtimeview.RolloutStatus, error)
}

type ReadinessProbe interface {
	Probe(context.Context) error
}

// EdgeRuntimeFeatures describes only operator-configured profiles. A profile
// is advertised to clients only when EdgeReadiness also proves the same exact
// aggregate runtime configuration is fresh.
type EdgeRuntimeFeatures struct {
	Traefik     bool
	CertManager bool
	ExternalDNS bool
}

type RegistryPullResolver interface {
	ResolveRegistryPull(context.Context, imagepull.RuntimeConfig, string, string, string) (domain.RegistryPullReference, bool, error)
}

type RuntimeLogStream interface {
	Channel() <-chan runtimeview.StreamEvent
	Close()
}

type BuildLogService interface {
	Snapshot(context.Context, buildlogs.SnapshotRequest) (buildlogs.Snapshot, error)
	Follow(context.Context, buildlogs.FollowRequest) (BuildLogStream, error)
}

type BuildLogStream interface {
	Channel() <-chan buildlogs.StreamEvent
	Close()
}

type Options struct {
	Store                              store.Store
	BootstrapToken, Version, PublicURL string
	MonitoringMode                     string
	SessionTTL                         time.Duration
	SecureCookie                       bool
	Releases                           ReleaseService
	Metrics                            MetricsService
	Runtime                            RuntimeViewService
	RuntimeReadiness                   ReadinessProbe
	RuntimeSecrets                     RuntimeSecretBackend
	RuntimeSecretReadiness             ReadinessProbe
	Certificates                       CertificateManagementBackend
	CertificateReadiness               ReadinessProbe
	CertificateReferences              CertificateReferenceBackend
	CertificateIssuers                 CertificateIssuerCatalog
	CertificateIssuerAdmin             CertificateIssuerAdminBackend
	CertificateIssuerRuntimeReadiness  ReadinessProbe
	RegistryPullReadiness              ReadinessProbe
	RegistryPulls                      RegistryPullResolver
	RegistryPullConfig                 imagepull.RuntimeConfig
	ImageResolution                    *imageresolution.Resolver
	GitHubSetup                        GitHubSetupBackend
	GitHubWebhook                      GitHubWebhookBackend
	Builds                             BuildBackend
	BuildPromotions                    *buildpromotion.Resolver
	BuildLogs                          BuildLogService
	GitBindingRepositories             GitBindingRepositoryResolver
	PlatformGitBinding                 PlatformGitBindingConfig
	BuildReadiness                     ReadinessProbe
	DefaultBuildPlatform               string
	BuilderSettings                    BuilderPlatformSettingsBackend
	BuildLogReadiness                  ReadinessProbe
	ValkeyReadiness                    ReadinessProbe
	OperationCache                     operationcache.Cache
	AppConfigRenderedPreviews          AppConfigRenderedPreviewBackend
	GitProjection                      GitProjectionBackend
	GitProjectionReadiness             ReadinessProbe
	ArgoReadiness                      ReadinessProbe
	EdgeReadiness                      ReadinessProbe
	EdgeFeatures                       EdgeRuntimeFeatures
	SSLIP                              SSLIPHostnameResolver
	Registry                           RegistryManagementService
	RegistryReadiness                  ReadinessProbe
	ExternalDNS                        ExternalDNSManagementService
	HelmApplications                   HelmApplicationBackend
	GitSSHKeys                         GitSSHKeyBackend
	MiddlewareProfiles                 middlewareProfileBackend
	DeploymentRollbacks                *deploymentrollback.Resolver
	AutoDeployService                  *autodeploy.PolicyService
	AutoDeployPolicies                 autodeploy.PolicyReader
	AutoDeployReadiness                ReadinessProbe
	HighRiskLimiter                    ratelimit.Limiter
}
type Server struct {
	store                             store.Store
	bootstrapHash                     [32]byte
	bootstrapConfigured               bool
	version, publicURL                string
	monitoringMode                    string
	sessionTTL                        time.Duration
	secureCookie                      bool
	releases                          ReleaseService
	metrics                           MetricsService
	runtime                           RuntimeViewService
	runtimeReadiness                  ReadinessProbe
	runtimeSecrets                    RuntimeSecretBackend
	runtimeSecretReadiness            ReadinessProbe
	certificates                      CertificateManagementBackend
	certificateReadiness              ReadinessProbe
	certificateReferences             CertificateReferenceBackend
	certificateIssuers                CertificateIssuerCatalog
	certificateIssuerAdmin            CertificateIssuerAdminBackend
	certificateIssuerRuntimeReadiness ReadinessProbe
	registryPullReadiness             ReadinessProbe
	registryPulls                     RegistryPullResolver
	registryPullConfig                imagepull.RuntimeConfig
	imageResolution                   *imageresolution.Resolver
	imageResolutionCatalog            imageresolution.Catalog
	githubSetup                       GitHubSetupBackend
	githubWebhookBackend              GitHubWebhookBackend
	builds                            BuildBackend
	buildPromotions                   *buildpromotion.Resolver
	buildLogs                         BuildLogService
	gitBindingRepositories            GitBindingRepositoryResolver
	platformGitBinding                PlatformGitBindingConfig
	buildReadiness                    ReadinessProbe
	defaultBuildPlatform              string
	builderSettings                   BuilderPlatformSettingsBackend
	buildLogReadiness                 ReadinessProbe
	valkeyReadiness                   ReadinessProbe
	operationCache                    operationcache.Cache
	appConfigRenderedPreviews         AppConfigRenderedPreviewBackend
	gitProjection                     GitProjectionBackend
	gitReadiness                      ReadinessProbe
	argoReadiness                     ReadinessProbe
	edgeReadiness                     ReadinessProbe
	edgeFeatures                      EdgeRuntimeFeatures
	sslip                             SSLIPHostnameResolver
	registryReadiness                 ReadinessProbe
	registry                          *registryHTTP
	externalDNS                       *externalDNSHTTP
	helmApplications                  HelmApplicationBackend
	gitSSHKeys                        GitSSHKeyBackend
	middleware                        middlewareProfileBackend
	deploymentRollbacks               *deploymentrollback.Resolver
	autoDeployService                 *autodeploy.PolicyService
	autoDeployPolicies                autodeploy.PolicyReader
	autoDeployReadiness               ReadinessProbe
	highRiskLimiter                   ratelimit.Limiter
	handler                           http.Handler
}

func New(o Options) *Server {
	defaultBuildPlatform := strings.TrimSpace(o.DefaultBuildPlatform)
	if defaultBuildPlatform == "" {
		defaultBuildPlatform = "linux/" + runtime.GOARCH
	}
	s := &Server{store: o.Store, version: o.Version, publicURL: strings.TrimSuffix(o.PublicURL, "/"), monitoringMode: strings.TrimSpace(o.MonitoringMode), sessionTTL: o.SessionTTL, secureCookie: o.SecureCookie, releases: o.Releases, metrics: o.Metrics, runtime: o.Runtime, runtimeReadiness: o.RuntimeReadiness, runtimeSecrets: o.RuntimeSecrets, runtimeSecretReadiness: o.RuntimeSecretReadiness, certificates: o.Certificates, certificateReadiness: o.CertificateReadiness, certificateReferences: o.CertificateReferences, certificateIssuers: o.CertificateIssuers, certificateIssuerAdmin: o.CertificateIssuerAdmin, certificateIssuerRuntimeReadiness: o.CertificateIssuerRuntimeReadiness, registryPullReadiness: o.RegistryPullReadiness, registryPulls: o.RegistryPulls, registryPullConfig: o.RegistryPullConfig, imageResolution: o.ImageResolution, githubSetup: o.GitHubSetup, githubWebhookBackend: o.GitHubWebhook, builds: o.Builds, buildPromotions: o.BuildPromotions, buildLogs: o.BuildLogs, gitBindingRepositories: o.GitBindingRepositories, platformGitBinding: o.PlatformGitBinding, buildReadiness: o.BuildReadiness, defaultBuildPlatform: defaultBuildPlatform, builderSettings: o.BuilderSettings, buildLogReadiness: o.BuildLogReadiness, valkeyReadiness: o.ValkeyReadiness, operationCache: o.OperationCache, appConfigRenderedPreviews: o.AppConfigRenderedPreviews, gitProjection: o.GitProjection, gitReadiness: o.GitProjectionReadiness, argoReadiness: o.ArgoReadiness, edgeReadiness: o.EdgeReadiness, edgeFeatures: o.EdgeFeatures, sslip: o.SSLIP, registryReadiness: o.RegistryReadiness, registry: newRegistryHTTP(o.Registry, o.RegistryReadiness), externalDNS: newExternalDNSHTTP(o.ExternalDNS, o.EdgeReadiness, o.EdgeFeatures.ExternalDNS), helmApplications: o.HelmApplications, gitSSHKeys: o.GitSSHKeys, middleware: o.MiddlewareProfiles, deploymentRollbacks: o.DeploymentRollbacks, autoDeployService: o.AutoDeployService, autoDeployPolicies: o.AutoDeployPolicies, autoDeployReadiness: o.AutoDeployReadiness, highRiskLimiter: o.HighRiskLimiter}
	s.imageResolutionCatalog, _ = o.Store.(imageresolution.Catalog)
	if s.gitBindingRepositories == nil {
		if resolver, ok := o.Builds.(GitBindingRepositoryResolver); ok {
			s.gitBindingRepositories = resolver
		}
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.sessionTTL <= 0 {
		s.sessionTTL = 12 * time.Hour
	}
	if s.monitoringMode != "managed" && s.monitoringMode != "existing" {
		s.monitoringMode = "disabled"
	}
	if o.BootstrapToken != "" {
		s.bootstrapHash = sha256.Sum256([]byte(o.BootstrapToken))
		s.bootstrapConfigured = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /openapi.yaml", s.openapiYAML)
	mux.HandleFunc("GET /openapi.json", s.openapiJSON)
	mux.HandleFunc("GET /openapi-agent.json", s.openapiAgentJSON)
	mux.HandleFunc("GET /arazzo.yaml", s.arazzoYAML)
	mux.HandleFunc("GET /v1/meta", s.meta)
	mux.Handle("POST /v1/auth/bootstrap", s.highRiskRemote(bootstrapLimit, http.HandlerFunc(s.bootstrap)))
	mux.Handle("POST /v1/auth/login", s.highRiskRemote(loginLimit, http.HandlerFunc(s.login)))
	mux.Handle("POST /v1/auth/invitations/accept", s.highRiskRemote(invitationAcceptLimit, http.HandlerFunc(s.acceptInvitation)))
	mux.Handle("GET /v1/me", s.protect(http.HandlerFunc(s.me)))
	mux.Handle("GET /v1/capabilities", s.protect(http.HandlerFunc(s.capabilities)))
	mux.Handle("GET /v1/audit-events", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.listAuditEvents)))))
	mux.Handle("GET /v1/monitoring/status", s.protect(http.HandlerFunc(s.monitoringStatus)))
	mux.Handle("GET /v1/metrics/query-range", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.metricsQueryRange))))
	mux.Handle("POST /v1/auth/logout", s.protect(s.humanOnly(http.HandlerFunc(s.logout))))
	mux.Handle("GET /v1/users", s.protect(s.humanOnly(http.HandlerFunc(s.users))))
	mux.Handle("POST /v1/users/invitations", s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(invitationIssueLimit, http.HandlerFunc(s.createInvitation))))))
	mux.Handle("DELETE /v1/users/{id}", s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.deleteUser))))))
	mux.Handle("GET /v1/teams", s.protect(s.humanOnly(http.HandlerFunc(s.teams))))
	mux.Handle("POST /v1/teams", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.teams)))))
	mux.Handle("DELETE /v1/teams/{id}", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.deleteTeam)))))
	mux.Handle("GET /v1/teams/{id}/members", s.protect(s.humanOnly(http.HandlerFunc(s.teamMembers))))
	mux.Handle("POST /v1/teams/{id}/members", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.teamMembers)))))
	mux.Handle("DELETE /v1/teams/{teamId}/members/{userId}", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.removeTeamMember)))))
	mux.Handle("GET /v1/github/installations", s.protect(s.humanOnly(http.HandlerFunc(s.githubInstallations))))
	mux.Handle("POST /v1/github/installations", s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.githubInstallations))))))
	mux.Handle("PATCH /v1/github/installations/{id}/sharing", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.githubInstallationSharing)))))
	mux.Handle("POST /v1/github/installations/authorize", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(githubSetupLimit, http.HandlerFunc(s.beginGitHubSetup)))))))
	mux.Handle("GET /v1/github/installations/setup", s.secretNoStore(s.protectGitHubSetupReturn(s.adminOnly(http.HandlerFunc(s.continueGitHubSetup)))))
	mux.Handle("GET /v1/github/installations/callback", s.secretNoStore(s.protectGitHubSetupReturn(s.adminOnly(http.HandlerFunc(s.completeGitHubSetup)))))
	mux.Handle("POST /v1/github/installations/link", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(githubSetupLimit, http.HandlerFunc(s.linkGitHubSetup)))))))
	mux.Handle("GET /v1/github/installations/{id}/repositories", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.githubInstallationRepositories)))))
	mux.Handle("POST /v1/webhooks/github", s.secretNoStore(http.HandlerFunc(s.receiveGitHubWebhook)))
	mux.Handle("GET /v1/projects", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.projects))))
	mux.Handle("POST /v1/projects", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.projects))))
	mux.Handle("GET /v1/projects/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.project))))
	mux.Handle("GET /v1/projects/{id}/registry-pull-credentials", registryNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.projectRegistryPullCredentials)))))
	mux.Handle("GET /v1/projects/{id}/git-ssh-keys", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.projectGitSSHKeys)))))
	mux.Handle("POST /v1/projects/{id}/git-ssh-keys", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.projectGitSSHKeys))))))
	mux.Handle("POST /v1/projects/{id}/git-ssh-keys/rotate", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.rotateProjectGitSSHKey))))))
	mux.Handle("DELETE /v1/projects/{id}/git-ssh-keys/active", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.revokeProjectGitSSHKey))))))
	mux.Handle("POST /v1/projects/{id}/registry-pull-credentials", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryPolicyLimit, http.HandlerFunc(s.projectRegistryPullCredentials))))))
	mux.Handle("DELETE /v1/projects/{id}/registry-pull-credentials/{credentialId}", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryPolicyLimit, http.HandlerFunc(s.projectRegistryPullCredential))))))
	mux.Handle("GET /v1/projects/{id}/grants", s.protect(s.humanOnly(http.HandlerFunc(s.projectAccessGrants))))
	mux.Handle("POST /v1/projects/{id}/grants", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.projectAccessGrants)))))
	mux.Handle("DELETE /v1/projects/{projectId}/grants/{grantId}", s.protect(s.humanOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.deleteProjectAccessGrant)))))
	mux.Handle("GET /v1/projects/{id}/service-accounts", s.protect(s.humanOnly(http.HandlerFunc(s.projectServiceAccounts))))
	mux.Handle("POST /v1/projects/{id}/service-accounts", s.protect(s.humanOnly(s.highRiskActor(serviceAccountLimit, http.HandlerFunc(s.projectServiceAccounts)))))
	mux.Handle("DELETE /v1/service-accounts/{id}", s.protect(s.humanOnly(s.highRiskActor(serviceAccountLimit, http.HandlerFunc(s.disableServiceAccount)))))
	mux.Handle("GET /v1/service-accounts/{id}/tokens", s.protect(s.humanOnly(http.HandlerFunc(s.serviceAccountTokens))))
	mux.Handle("POST /v1/service-accounts/{id}/tokens", s.protect(s.humanOnly(s.highRiskActor(serviceAccountLimit, http.HandlerFunc(s.serviceAccountTokens)))))
	mux.Handle("DELETE /v1/service-accounts/{serviceAccountId}/tokens/{tokenId}", s.protect(s.humanOnly(s.highRiskActor(serviceAccountLimit, http.HandlerFunc(s.revokeServiceAccountToken)))))
	mux.Handle("GET /v1/environments", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.environments))))
	mux.Handle("POST /v1/environments", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.environments))))
	mux.Handle("GET /v1/environments/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.environment))))
	mux.Handle("DELETE /v1/environments/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.deleteEnvironment))))
	mux.Handle("GET /v1/environments/{id}/apps", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.environmentApps))))
	mux.Handle("POST /v1/environments/{id}/clone", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.cloneEnvironment))))
	mux.Handle("GET /v1/environments/{id}/git-binding", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.environmentGitBinding)))))
	mux.Handle("POST /v1/environments/{id}/git-binding", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitBindingLimit, http.HandlerFunc(s.environmentGitBinding))))))
	mux.Handle("GET /v1/environments/{id}/variable-sets", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.variableSets)))))
	mux.Handle("POST /v1/environments/{id}/variable-sets/{scope}/preview", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(variableSetMutationLimit, http.HandlerFunc(s.previewVariableSet))))))
	mux.Handle("PUT /v1/environments/{id}/variable-sets/{scope}", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(variableSetMutationLimit, http.HandlerFunc(s.saveVariableSet))))))
	mux.Handle("GET /v1/platform/argo/git-binding", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(http.HandlerFunc(s.platformArgoGitBinding))))))
	mux.Handle("POST /v1/platform/argo/git-binding", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(platformGitBindingLimit, http.HandlerFunc(s.platformArgoGitBinding)))))))
	mux.Handle("GET /v1/platform/certificate-issuers", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(http.HandlerFunc(s.platformCertificateIssuers))))))
	mux.Handle("POST /v1/platform/certificate-issuers", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.platformCertificateIssuers)))))))
	mux.Handle("PUT /v1/platform/certificate-issuers/{id}", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.platformCertificateIssuer)))))))
	mux.Handle("POST /v1/platform/certificate-issuers/{id}/deactivate", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.deactivatePlatformCertificateIssuer)))))))
	mux.Handle("GET /v1/platform/builder-settings", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(http.HandlerFunc(s.builderPlatformSettings))))))
	mux.Handle("PUT /v1/platform/builder-settings", s.secretNoStore(s.protect(s.humanOnly(s.adminOnly(s.highRiskActor(accessControlLimit, http.HandlerFunc(s.builderPlatformSettings)))))))
	mux.Handle("GET /v1/middlewares", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.middlewareProfiles)))))
	mux.Handle("GET /v1/middlewares/catalog", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.middlewareProfileCatalog)))))
	mux.Handle("POST /v1/middlewares/validate", s.secretNoStore(s.protect(s.humanOnly(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.validateMiddlewareProfile))))))
	mux.Handle("POST /v1/middlewares", s.secretNoStore(s.protect(s.humanOnly(s.requireAutomationScope(domain.AutomationScopeAppEdit, s.highRiskActor(accessControlLimit, http.HandlerFunc(s.middlewareProfiles)))))))
	mux.Handle("PUT /v1/middlewares/{id}", s.secretNoStore(s.protect(s.humanOnly(s.requireAutomationScope(domain.AutomationScopeAppEdit, s.highRiskActor(accessControlLimit, http.HandlerFunc(s.middlewareProfile)))))))
	mux.Handle("POST /v1/middlewares/{id}/clone", s.secretNoStore(s.protect(s.humanOnly(s.requireAutomationScope(domain.AutomationScopeAppEdit, s.highRiskActor(accessControlLimit, http.HandlerFunc(s.cloneMiddlewareProfile)))))))
	mux.Handle("POST /v1/middlewares/{id}/deactivate", s.secretNoStore(s.protect(s.humanOnly(s.requireAutomationScope(domain.AutomationScopeAppEdit, s.highRiskActor(accessControlLimit, http.HandlerFunc(s.deactivateMiddlewareProfile)))))))
	mux.Handle("GET /v1/environments/{id}/external-dns-integrations", externalDNSNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.externalDNS.environmentCatalog)))))
	mux.Handle("GET /v1/applications", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applications))))
	mux.Handle("POST /v1/applications", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.applications))))
	mux.Handle("GET /v1/applications/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.application))))
	mux.Handle("DELETE /v1/applications/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.deleteApplication))))
	mux.Handle("GET /v1/applications/{id}/git-ssh-keys", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.applicationGitSSHKeys)))))
	mux.Handle("POST /v1/applications/{id}/git-ssh-keys", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.applicationGitSSHKeys))))))
	mux.Handle("POST /v1/applications/{id}/git-ssh-keys/rotate", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.rotateApplicationGitSSHKey))))))
	mux.Handle("DELETE /v1/applications/{id}/git-ssh-keys/active", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(gitSSHKeyLimit, http.HandlerFunc(s.revokeApplicationGitSSHKey))))))
	mux.Handle("GET /v1/applications/{id}/registry-pull-selection", registryNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.applicationRegistryPullSelection)))))
	mux.Handle("PUT /v1/applications/{id}/registry-pull-selection", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryPolicyLimit, http.HandlerFunc(s.applicationRegistryPullSelection))))))
	mux.Handle("GET /v1/applications/{id}/workloads", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationWorkloads))))
	mux.Handle("GET /v1/applications/{id}/external-dns-integrations", externalDNSNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.externalDNS.applicationCatalog)))))
	mux.Handle("GET /v1/applications/{id}/sslip-hostname", s.sslipNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, s.sslipReady(http.HandlerFunc(s.sslipHostname))))))
	mux.Handle("GET /v1/applications/{id}/certificate-issuers", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationCertificateIssuerCatalog)))))
	mux.Handle("PUT /v1/applications/{id}/environments/{environmentId}/helm/release", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.helmUpsert)))))
	mux.Handle("GET /v1/applications/{id}/environments/{environmentId}/helm/release", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.helmHead)))))
	mux.Handle("GET /v1/applications/{id}/environments/{environmentId}/helm/releases", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.helmHistory)))))
	mux.Handle("POST /v1/applications/{id}/environments/{environmentId}/helm/release/retry", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.helmRetry)))))
	mux.Handle("POST /v1/applications/{id}/environments/{environmentId}/helm/release/disable", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.helmDisable)))))
	mux.Handle("POST /v1/applications/{id}/environments/{environmentId}/helm/release/rollback", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.helmRollback)))))
	mux.Handle("GET /v1/applications/{id}/build-definitions", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationBuildDefinitions)))))
	mux.Handle("GET /v1/applications/{id}/build-secret-profiles", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationBuildSecretProfiles)))))
	mux.Handle("POST /v1/applications/{id}/build-definitions", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeBuildCreate, s.highRiskActor(buildDefinitionLimit, http.HandlerFunc(s.applicationBuildDefinitions))))))
	mux.Handle("GET /v1/applications/{id}/auto-deploy-policies", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationAutoDeployPolicies)))))
	mux.Handle("POST /v1/applications/{id}/auto-deploy-policies", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(buildDefinitionLimit, http.HandlerFunc(s.applicationAutoDeployPolicies))))))
	mux.Handle("GET /v1/auto-deploy-policies/{id}", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.autoDeployPolicy)))))
	mux.Handle("PUT /v1/auto-deploy-policies/{id}", s.secretNoStore(s.protect(s.humanOnly(s.highRiskActor(buildDefinitionLimit, http.HandlerFunc(s.autoDeployPolicy))))))
	mux.Handle("GET /v1/auto-deploy-policies/{id}/revisions", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.autoDeployPolicyRevisions)))))
	mux.Handle("GET /v1/auto-deploy-policies/{id}/runs", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.autoDeployPolicyRuns)))))
	mux.Handle("GET /v1/applications/{id}/builds", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.applicationBuildHistory)))))
	mux.Handle("GET /v1/build-definitions/{id}", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.buildDefinition)))))
	mux.Handle("POST /v1/build-definitions/{id}/builds", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeBuildCreate, s.highRiskActor(buildCommandLimit, http.HandlerFunc(s.buildDefinitionBuild))))))
	mux.Handle("GET /v1/applications/{id}/registry", registryNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.registry.applicationInventory)))))
	mux.Handle("PUT /v1/applications/{id}/registry/policies/{targetId}", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryPolicyLimit, http.HandlerFunc(s.registry.putPolicy))))))
	mux.Handle("POST /v1/applications/{id}/registry/cleanup-previews", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryPreviewLimit, http.HandlerFunc(s.registry.previewCleanup))))))
	mux.Handle("GET /v1/applications/{id}/secret-bindings", s.secretNoStore(s.protect(s.runtimeSecretReady(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.listSecretBindings))))))
	mux.Handle("POST /v1/applications/{id}/secret-bindings", s.secretNoStore(s.protect(s.runtimeSecretReady(s.humanOnly(s.highRiskActor(secretCreateLimit, http.HandlerFunc(s.createSecretBinding)))))))
	mux.Handle("GET /v1/secret-bindings/{id}", s.secretNoStore(s.protect(s.runtimeSecretReady(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.getSecretBinding))))))
	mux.Handle("POST /v1/secret-bindings/{id}/versions", s.secretNoStore(s.protect(s.runtimeSecretReady(s.humanOnly(s.highRiskActor(secretRotateLimit, http.HandlerFunc(s.rotateSecretBinding)))))))
	mux.Handle("DELETE /v1/secret-bindings/{id}", s.secretNoStore(s.protect(s.runtimeSecretReady(s.humanOnly(s.highRiskActor(secretDeleteLimit, http.HandlerFunc(s.deleteSecretBinding)))))))
	mux.Handle("GET /v1/applications/{id}/certificate-bindings", s.secretNoStore(s.protect(s.humanOnly(s.certificateReady(http.HandlerFunc(s.listCertificateBindings))))))
	mux.Handle("POST /v1/applications/{id}/certificate-bindings", s.secretNoStore(s.protect(s.humanOnly(s.certificateReady(s.highRiskActor(certificateCreateLimit, http.HandlerFunc(s.createCertificateBinding)))))))
	mux.Handle("GET /v1/certificate-bindings/{id}", s.secretNoStore(s.protect(s.humanOnly(s.certificateReady(http.HandlerFunc(s.getCertificateBinding))))))
	mux.Handle("POST /v1/certificate-bindings/{id}/versions", s.secretNoStore(s.protect(s.humanOnly(s.certificateReady(s.highRiskActor(certificateRotateLimit, http.HandlerFunc(s.rotateCertificateBinding)))))))
	mux.Handle("DELETE /v1/certificate-bindings/{id}", s.secretNoStore(s.protect(s.humanOnly(s.certificateReady(s.highRiskActor(certificateDeleteLimit, http.HandlerFunc(s.deleteCertificateBinding)))))))
	mux.Handle("GET /v1/deployments", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.deployments))))
	mux.Handle("POST /v1/deployments", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.deployments))))
	mux.Handle("POST /v1/deployments/image-resolution-preview", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.previewImageResolution)))))
	mux.Handle("GET /v1/deployments/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.deployment))))
	mux.Handle("GET /v1/deployments/{id}/status", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.deploymentStatus)))))
	mux.Handle("GET /v1/deployments/{id}/rollback-sources", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.deploymentRollbackSources(s.deploymentRollbacks, w, r) })))))
	mux.Handle("POST /v1/deployments/{id}/rollback", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, s.highRiskActor(deploymentRollbackLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.rollbackDeployment(s.deploymentRollbacks, w, r) }))))))
	mux.Handle("GET /v1/workloads/{id}/logs", s.protect(s.requireAutomationScope(domain.AutomationScopeLogsRead, http.HandlerFunc(s.workloadLogs))))
	mux.Handle("GET /v1/workloads/{id}/logs/follow", s.protect(s.humanOnly(http.HandlerFunc(s.followWorkloadLogs))))
	mux.Handle("GET /v1/workloads/{id}/events", s.protect(s.requireAutomationScope(domain.AutomationScopeLogsRead, http.HandlerFunc(s.workloadEvents))))
	mux.Handle("GET /v1/deployments/{id}/config", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.deploymentConfig))))
	mux.Handle("POST /v1/deployments/{id}/config/validate", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.validateDeploymentConfig))))
	mux.Handle("POST /v1/deployments/{id}/config/preview", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.previewDeploymentConfig))))
	mux.Handle("PUT /v1/deployments/{id}/config", s.protect(s.requireAutomationScope(domain.AutomationScopeAppEdit, http.HandlerFunc(s.saveDeploymentConfig))))
	mux.Handle("GET /v1/operations", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.operations))))
	mux.Handle("GET /v1/operations/{id}", s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.operation))))
	mux.Handle("GET /v1/builds/{id}", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.buildAttempt)))))
	mux.Handle("GET /v1/builds/{id}/logs", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeLogsRead, http.HandlerFunc(s.buildAttemptLogs)))))
	mux.Handle("POST /v1/builds/{id}/cancel", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeBuildCreate, s.highRiskActor(buildCommandLimit, http.HandlerFunc(s.cancelBuildAttempt))))))
	mux.Handle("POST /v1/builds/{id}/retry", s.secretNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeBuildCreate, s.highRiskActor(buildCommandLimit, http.HandlerFunc(s.retryBuildAttempt))))))
	mux.Handle("POST /v1/builds/{id}/promote", s.secretNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.promoteBuildAttempt)))))
	mux.Handle("GET /v1/registry-targets", registryNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.registry.targets)))))
	mux.Handle("POST /v1/registry-targets", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryTargetLimit, http.HandlerFunc(s.registry.targets))))))
	mux.Handle("PUT /v1/registry-targets/{id}", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryTargetLimit, http.HandlerFunc(s.registry.updateTarget))))))
	mux.Handle("GET /v1/external-dns/integrations", externalDNSNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.externalDNS.integrations)))))
	mux.Handle("POST /v1/external-dns/integrations", externalDNSNoStore(s.protect(s.humanOnly(s.highRiskActor(externalDNSManageLimit, http.HandlerFunc(s.externalDNS.integrations))))))
	mux.Handle("PUT /v1/external-dns/integrations/{id}", externalDNSNoStore(s.protect(s.humanOnly(s.highRiskActor(externalDNSManageLimit, http.HandlerFunc(s.externalDNS.updateIntegration))))))
	mux.Handle("DELETE /v1/external-dns/integrations/{id}", externalDNSNoStore(s.protect(s.humanOnly(s.highRiskActor(externalDNSManageLimit, http.HandlerFunc(s.externalDNS.deactivateIntegration))))))
	mux.Handle("GET /v1/external-dns/status", externalDNSNoStore(s.protect(s.humanOnly(http.HandlerFunc(s.externalDNS.status)))))
	mux.Handle("GET /v1/registry-cleanup-plans/{id}", registryNoStore(s.protect(s.requireAutomationScope(domain.AutomationScopeAppRead, http.HandlerFunc(s.registry.cleanupPlan)))))
	mux.Handle("POST /v1/registry-cleanup-plans/{id}/executions", registryNoStore(s.protect(s.humanOnly(s.highRiskActor(registryExecuteLimit, http.HandlerFunc(s.registry.executeCleanup))))))
	mux.Handle("GET /v1/platform/releases/latest", s.protect(s.humanOnly(s.adminOnly(http.HandlerFunc(s.latestRelease)))))
	mux.Handle("GET /v1/config-schemas/config.kuberploy.io/v1alpha1", s.protect(http.HandlerFunc(s.configSchema)))
	s.handler = s.withRequestID(s.recover(mux))
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("X-Request-ID")
		if v == "" || len(v) > 128 {
			v = id.New()
		}
		w.Header().Set("X-Request-ID", v)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, v)))
	})
}
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeProblem(w, r, 500, "InternalError", "Internal error", "The server could not complete the request.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func requestID(ctx context.Context) string        { v, _ := ctx.Value(requestIDKey).(string); return v }
func currentUser(ctx context.Context) domain.User { v, _ := ctx.Value(userKey).(domain.User); return v }

func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, authentication, sessionHash, ok := s.authenticateRequest(w, r)
		if !ok {
			return
		}
		if authentication.Kind == authenticationSession && unsafe(r.Method) && !s.validCSRF(r) {
			writeProblem(w, r, 403, "CSRFRejected", "Request rejected", "The CSRF token or request origin is invalid.")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, authenticationKey, authentication)
		if len(sessionHash) != 0 {
			ctx = context.WithValue(ctx, sessionHashKey, sessionHash)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// protectGitHubSetupReturn authenticates only the short-lived, path-scoped
// SameSite=Lax copy of the initiating human session. The primary session stays
// SameSite=Strict and therefore is intentionally absent on GitHub's cross-site
// Setup URL and OAuth callback navigations.
func (s *Server) protectGitHubSetupReturn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, sessionHash, ok := s.authenticateGitHubSetupReturn(w, r)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, authenticationKey, requestAuthentication{Kind: authenticationSession})
		ctx = context.WithValue(ctx, sessionHashKey, sessionHash)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Authorize(r.Context(), currentUser(r.Context()).ID, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
			mappedError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func unsafe(m string) bool { return m != "GET" && m != "HEAD" && m != "OPTIONS" }
func (s *Server) validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	header := r.Header.Get("X-CSRF-Token")
	if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if s.publicURL != "" {
		expected, err := url.Parse(s.publicURL)
		return err == nil && strings.EqualFold(u.Scheme, expected.Scheme) && strings.EqualFold(u.Host, expected.Host)
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeProblem(w, r, 503, "DatabaseUnavailable", "Not ready", "PostgreSQL is unavailable.")
		return
	}
	if s.valkeyReadiness != nil {
		if err := s.valkeyReadiness.Probe(ctx); err != nil {
			writeProblem(w, r, 503, "ValkeyUnavailable", "Not ready", "Valkey is unavailable.")
			return
		}
	}
	// Optional control-plane runtimes are exposed through /v1/capabilities and
	// are checked again by each dependent operation. A transient feature outage
	// must not remove the entire API Pod from Service endpoints.
	writeJSON(w, 200, map[string]string{"status": "ready"})
}
func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	bootstrapRequired, err := s.store.BootstrapRequired(r.Context())
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "DatabaseUnavailable", "Metadata unavailable", "Bootstrap state could not be read.")
		return
	}
	sum := sha256.Sum256(contract.OpenAPIYAML)
	writeJSON(w, 200, map[string]any{"version": s.version, "apiVersion": "v1", "contractVersion": "1.0.0", "contractDigest": "sha256:" + hex.EncodeToString(sum[:]), "appConfigVersion": "config.kuberploy.io/v1alpha1", "bootstrapRequired": bootstrapRequired})
}
func (s *Server) openapiYAML(w http.ResponseWriter, r *http.Request) {
	writeContract(w, r, "application/yaml", contract.OpenAPIYAML)
}
func (s *Server) openapiJSON(w http.ResponseWriter, r *http.Request) {
	writeContract(w, r, "application/json", contract.OpenAPIJSON)
}
func (s *Server) openapiAgentJSON(w http.ResponseWriter, r *http.Request) {
	profile, err := contract.AgentProfileJSON()
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "ContractInvalid", "Contract unavailable", "The embedded agent profile could not be derived from OpenAPI.")
		return
	}
	writeContract(w, r, "application/json", profile)
}
func (s *Server) arazzoYAML(w http.ResponseWriter, r *http.Request) {
	writeContract(w, r, "application/vnd.oai.workflows+yaml;version=1.1.0", contract.ArazzoYAML)
}
func writeContract(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"sha256-` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
func (s *Server) configSchema(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/schema+json")
	w.Write(appschema.AppConfig)
}

type bootstrapRequest struct {
	Token       string `json:"token"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if !s.bootstrapConfigured {
		writeProblem(w, r, 503, "BootstrapUnavailable", "Bootstrap unavailable", "No bootstrap token is configured.")
		return
	}
	var in bootstrapRequest
	if !decode(w, r, &in) {
		return
	}
	got := sha256.Sum256([]byte(in.Token))
	if subtle.ConstantTimeCompare(got[:], s.bootstrapHash[:]) != 1 {
		writeProblem(w, r, 401, "InvalidBootstrapToken", "Bootstrap rejected", "The bootstrap token is invalid.")
		return
	}
	in.Email, _ = emailaddr.Normalize(in.Email)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.Email == "" {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "email is required and must be a valid email address.", FieldError{Pointer: "/email", Code: "Invalid", Detail: "Enter a valid email address."})
		return
	}
	if in.DisplayName == "" || len(in.DisplayName) > 100 {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "displayName is required.", FieldError{Pointer: "/displayName", Code: "Required", Detail: "Enter a display name of at most 100 characters."})
		return
	}
	passwordHash, err := passwordauth.Hash(in.Password)
	if err != nil {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "password must be 12 to 256 bytes.", FieldError{Pointer: "/password", Code: "Invalid", Detail: "Use a password between 12 and 256 bytes."})
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	hash := sha256.Sum256(raw)
	now := time.Now().UTC()
	u := domain.User{ID: id.New(), Email: in.Email, DisplayName: in.DisplayName, Role: "platform-admin", Issuer: "kuberploy:bootstrap", Subject: id.New(), GrantRevision: 1, CreatedAt: now}
	if err := s.store.BootstrapAdmin(r.Context(), u, passwordHash, hash[:], now.Add(s.sessionTTL)); err != nil {
		if errors.Is(err, store.ErrBootstrapConsumed) {
			writeProblem(w, r, 409, "BootstrapConsumed", "Bootstrap already completed", "The one-time administrator bootstrap has already been used.")
			return
		}
		writeProblem(w, r, 500, "PersistenceFailed", "Bootstrap failed", "The administrator session could not be persisted.")
		return
	}
	csrf, err := s.setSessionCookies(w, raw)
	if err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	w.Header().Set("X-CSRF-Token", csrf)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, u)
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in loginRequest
	if !decode(w, r, &in) {
		return
	}
	in.Email, _ = emailaddr.Normalize(in.Email)
	if in.Email == "" || len(in.Password) > 256 {
		passwordauth.DummyVerify(in.Password)
		s.loginRejected(w, r)
		return
	}
	u, encoded, err := s.store.LocalCredential(r.Context(), in.Email)
	if err != nil {
		passwordauth.DummyVerify(in.Password)
		s.loginRejected(w, r)
		return
	}
	ok, upgrade := passwordauth.Verify(encoded, in.Password)
	if !ok {
		s.loginRejected(w, r)
		return
	}
	upgraded := ""
	if upgrade {
		upgraded, err = passwordauth.Hash(in.Password)
		if err != nil {
			writeProblem(w, r, 500, "CredentialUnavailable", "Login unavailable", "Authentication could not be completed.")
			return
		}
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	hash := sha256.Sum256(raw)
	if old, cookieErr := r.Cookie(sessionCookie); cookieErr == nil {
		if oldRaw, decodeErr := base64.RawURLEncoding.DecodeString(old.Value); decodeErr == nil && len(oldRaw) == 32 {
			oldHash := sha256.Sum256(oldRaw)
			if err = s.store.RevokeSession(r.Context(), oldHash[:]); err != nil {
				writeProblem(w, r, 503, "SessionUnavailable", "Login unavailable", "Authentication could not be completed.")
				return
			}
		}
	}
	u, err = s.store.CreateLoginSession(r.Context(), u.ID, encoded, upgraded, hash[:], time.Now().UTC().Add(s.sessionTTL))
	if err != nil {
		s.loginRejected(w, r)
		return
	}
	csrf, err := s.setSessionCookies(w, raw)
	if err != nil {
		writeProblem(w, r, 500, "EntropyUnavailable", "Internal error", "Secure session creation failed.")
		return
	}
	w.Header().Set("X-CSRF-Token", csrf)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) loginRejected(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusUnauthorized, "InvalidCredentials", "Login rejected", "The login or password is invalid.")
}

func (s *Server) setSessionCookies(w http.ResponseWriter, raw []byte) (string, error) {
	csrfRaw := make([]byte, 24)
	if _, err := rand.Read(csrfRaw); err != nil {
		return "", err
	}
	session := base64.RawURLEncoding.EncodeToString(raw)
	csrf := base64.RawURLEncoding.EncodeToString(csrfRaw)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: int(s.sessionTTL.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", HttpOnly: false, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: int(s.sessionTTL.Seconds())})
	return csrf, nil
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, struct {
		domain.User
		Authentication requestAuthentication `json:"authentication"`
	}{User: currentUser(r.Context()), Authentication: currentAuthentication(r)})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	if err := s.store.RevokeSession(r.Context(), hash); err != nil {
		writeProblem(w, r, 500, "PersistenceFailed", "Logout failed", "The session could not be revoked.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: "/", MaxAge: -1, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode})
	s.clearGitHubSetupSessionCookie(w, githubInstallReturnCookiePath)
	s.clearGitHubSetupSessionCookie(w, githubOAuthCallbackCookiePath)
	s.clearGitHubSetupHandoffCookie(w)
	w.WriteHeader(204)
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		writeProblem(w, r, 400, "InvalidJSON", "Invalid JSON", "The request body is not valid JSON: "+err.Error())
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		writeProblem(w, r, 400, "InvalidJSON", "Invalid JSON", "The request must contain exactly one JSON object.")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func idemKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.Header.Get("Idempotency-Key")
	if len(v) < 1 || len(v) > 128 {
		writeProblem(w, r, 400, "IdempotencyKeyRequired", "Idempotency key required", "Provide an Idempotency-Key header of 1 to 128 characters.")
		return "", false
	}
	return v, true
}
func collection(w http.ResponseWriter, items any) {
	value := reflect.ValueOf(items)
	if !value.IsValid() || (value.Kind() == reflect.Slice && value.IsNil()) {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func mappedError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, 404, "NotFound", "Not found", "The requested resource was not found.")
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeProblem(w, r, 409, "IdempotencyConflict", "Idempotency conflict", "This idempotency key was already used with different input.")
	case errors.Is(err, store.ErrDeletionConfirmation):
		writeProblem(w, r, 409, "DeletionConfirmationMismatch", "Confirmation does not match", "Type the exact current name or email to confirm deletion.")
	case errors.Is(err, store.ErrSelfDeletion):
		writeProblem(w, r, 409, "SelfDeletionBlocked", "Current user cannot be deleted", "Sign in as another platform administrator before deleting this user.")
	case errors.Is(err, store.ErrUserDeletionBlocked):
		writeProblem(w, r, 409, "UserDeletionBlocked", "User still owns required access", "Transfer owned GitHub installations and final team or platform administrator roles before deleting this user.")
	case errors.Is(err, store.ErrTeamDeletionBlocked):
		writeProblem(w, r, 409, "TeamDeletionBlocked", "Team still owns resources", "Move or remove the team's projects, GitHub sharing, setup handoffs, and secret bindings before deleting it.")
	case errors.Is(err, store.ErrApplicationDeletionBlocked):
		writeProblem(w, r, 409, "ApplicationDeletionBlocked", "App still has resources", "Disable and remove the App's deployments, build configuration, releases, bindings, and policies before deleting it.")
	case errors.Is(err, store.ErrEnvironmentDeletionBlocked):
		writeProblem(w, r, 409, "EnvironmentDeletionBlocked", "Environment still has resources", "Remove the Environment's deployments, Git binding, releases, variables, certificates, and integrations before deleting it.")
	case errors.Is(err, store.ErrConflict):
		writeProblem(w, r, 409, "Conflict", "Conflict", "The request conflicts with existing state.")
	case errors.Is(err, store.ErrForbidden):
		writeProblem(w, r, 403, "Forbidden", "Forbidden", "You do not have access to perform this action.")
	case errors.Is(err, store.ErrPreconditionFailed):
		writeProblem(w, r, 412, "PreconditionFailed", "Precondition failed", "The AppConfig changed after this draft was loaded. Reload and preview again.")
	case errors.Is(err, store.ErrPreviewExpired):
		writeProblem(w, r, 409, "PreviewExpired", "Preview expired", "Preview the exact draft again before saving.")
	case errors.Is(err, store.ErrPreviewConsumed):
		writeProblem(w, r, 409, "PreviewConsumed", "Preview already used", "A preview token can be used for one successful save only.")
	case errors.Is(err, store.ErrPreviewInvalid):
		writeProblem(w, r, 409, "PreviewInvalid", "Preview does not match", "The preview token is not valid for this actor, deployment, ETag, or draft.")
	case errors.Is(err, store.ErrConfigProjectionMissing):
		writeProblem(w, r, 500, "ConfigProjectionMissing", "Configuration projection missing", "This deployment requires an explicit configuration projection repair before it can be edited.")
	default:
		writeProblem(w, r, 500, "PersistenceFailed", "Persistence failed", "The request could not be persisted.")
	}
}
