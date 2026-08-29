package httpapi

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/automation"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

type capabilitiesResponse struct {
	Actions       []string          `json:"actions"`
	Capabilities  []capabilityView  `json:"capabilities"`
	Features      map[string]bool   `json:"features"`
	FeatureStates map[string]string `json:"featureStates"`
	Defaults      map[string]string `json:"defaults"`
	Limits        map[string]any    `json:"limits"`
}

type capabilityView struct {
	Resource  string                 `json:"resource"`
	Actions   []string               `json:"actions"`
	Scope     string                 `json:"scope"`
	Role      domain.AccessRole      `json:"role,omitempty"`
	ScopeType domain.AccessScopeType `json:"scopeType,omitempty"`
	ScopeID   string                 `json:"scopeId,omitempty"`
	Source    string                 `json:"source,omitempty"`
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	effective, err := s.store.EffectiveCapabilities(r.Context(), currentUser(r.Context()).ID)
	if err != nil {
		mappedError(w, r, err)
		return
	}
	authentication := currentAuthentication(r)
	views := make([]capabilityView, 0, len(effective)+1)
	if authentication.Kind != authenticationServiceAccount {
		views = append(views, capabilityView{Resource: "account", Actions: []string{"github-installations:read", "github-installations:share", "teams:create", "teams:read"}, Scope: "self", ScopeType: "", ScopeID: ""})
	}
	actionSet := map[string]struct{}{}
	if len(views) != 0 {
		for _, action := range views[0].Actions {
			actionSet[action] = struct{}{}
		}
	}
	for _, capability := range effective {
		actions := append([]string(nil), capability.Actions...)
		if authentication.Kind == authenticationServiceAccount {
			actions = filterAutomationActions(actions, authentication.Scopes)
		}
		views = append(views, capabilityView{Resource: string(capability.ScopeType), Actions: actions, Scope: capability.ScopeID, Role: capability.Role, ScopeType: capability.ScopeType, ScopeID: capability.ScopeID, Source: capability.Source})
		for _, action := range actions {
			actionSet[action] = struct{}{}
		}
	}
	actions := make([]string, 0, len(actionSet))
	for action := range actionSet {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	monitoringConfigured := false
	if s.metrics != nil && (s.monitoringMode == "managed" || s.monitoringMode == "existing") {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		monitoringConfigured = s.metrics.Probe(ctx) == nil
		cancel()
	}
	runtimeConfigured := false
	if s.runtime != nil && s.runtimeReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		runtimeConfigured = s.runtimeReadiness.Probe(ctx) == nil
		cancel()
	}
	buildsConfigured := false
	if s.builds != nil && s.githubWebhookBackend != nil && s.buildReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		buildsConfigured = s.buildReadiness.Probe(ctx) == nil
		cancel()
	}
	gitSSHBuildsConfigured := false
	if s.builds != nil && s.gitSSHKeys != nil && s.buildReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		gitSSHBuildsConfigured = s.buildReadiness.Probe(ctx) == nil
		cancel()
	}
	buildLogsConfigured := false
	if s.builds != nil && s.buildLogs != nil && s.buildLogReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		buildLogsConfigured = s.buildLogReadiness.Probe(ctx) == nil
		cancel()
	}
	gitConfigured := false
	if s.gitProjection != nil && s.gitReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		gitConfigured = s.gitReadiness.Probe(ctx) == nil
		cancel()
	}
	argoConfigured := false
	if gitConfigured && s.argoReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		argoConfigured = s.argoReadiness.Probe(ctx) == nil
		cancel()
	}
	registryConfigured := s.registry != nil && s.registry.service != nil
	managedRegistryConfigured := false
	if s.registry != nil && s.registry.service != nil && s.registryReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		managedRegistryConfigured = s.registryReadiness.Probe(ctx) == nil
		cancel()
	}
	secretBindingsConfigured := false
	if s.runtimeSecrets != nil && s.runtimeSecretReadiness != nil && gitConfigured && s.runtimeSecrets.ProviderAvailable(secrets.ProviderSealedSecrets) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		secretBindingsConfigured = s.runtimeSecretReadiness.Probe(ctx) == nil
		cancel()
	}
	registryPullsConfigured := false
	if argoConfigured && s.registryPulls != nil && s.registryPullConfig.Enabled && s.registryPullReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		registryPullsConfigured = s.registryPullReadiness.Probe(ctx) == nil
		cancel()
	}
	edgeConfigured := false
	edgeProfilesConfigured := s.edgeFeatures.Traefik || s.edgeFeatures.CertManager || s.edgeFeatures.ExternalDNS
	if s.edgeReadiness != nil && edgeProfilesConfigured {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		edgeConfigured = s.edgeReadiness.Probe(ctx) == nil
		cancel()
	}
	traefikConfigured := edgeConfigured && s.edgeFeatures.Traefik
	certManagerConfigured := edgeConfigured && s.edgeFeatures.CertManager
	certificateIssuerManagementConfigured := false
	if s.certificateIssuerAdmin != nil && s.certificateIssuerRuntimeReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		certificateIssuerManagementConfigured = s.certificateIssuerRuntimeReadiness.Probe(ctx) == nil
		cancel()
	}
	sslipConfigured := false
	if s.sslip != nil && s.edgeFeatures.Traefik {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		sslipConfigured = s.sslip.Probe(ctx) == nil
		cancel()
	}
	customCertificatesConfigured := false
	if s.certificates != nil && s.certificateReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		customCertificatesConfigured = s.certificateReadiness.Probe(ctx) == nil
		cancel()
	}
	helmDeploymentsConfigured, helmRollbacksConfigured := false, false
	if s.helmApplications != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		helmCapabilities, capabilityErr := s.helmApplications.Capabilities(ctx)
		cancel()
		if capabilityErr == nil {
			helmDeploymentsConfigured = helmCapabilities.HelmDeployments
			helmRollbacksConfigured = helmCapabilities.HelmRollbacks
		}
	}
	deploymentRollbacksConfigured := s.deploymentRollbacks != nil && gitConfigured && argoConfigured
	imageTagResolutionConfigured := s.imageResolution != nil && s.imageResolution.Available()
	_, variableSetsBackendConfigured := s.gitProjection.(VariableSetBackend)
	variableSetsConfigured := gitConfigured && variableSetsBackendConfigured
	middlewareProfilesConfigured := s.middleware != nil && argoConfigured && gitConfigured && traefikConfigured
	autoDeployConfigured := false
	if s.autoDeployService != nil && s.autoDeployPolicies != nil && s.autoDeployReadiness != nil && buildsConfigured && gitConfigured && argoConfigured {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		autoDeployConfigured = s.autoDeployReadiness.Probe(ctx) == nil
		cancel()
	}
	featureState := func(enabled, ready bool) string {
		if !enabled {
			return "disabled"
		}
		if ready {
			return "healthy"
		}
		return "unavailable"
	}
	gitEnabled := s.gitProjection != nil && s.gitReadiness != nil
	argoEnabled := gitEnabled && s.argoReadiness != nil
	edgeEnabled := s.edgeReadiness != nil && edgeProfilesConfigured
	buildEnabled := s.builds != nil && s.githubWebhookBackend != nil && s.buildReadiness != nil
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Actions:      actions,
		Capabilities: views,
		Features: map[string]bool{
			// A feature becomes true only after the API is backed by its real
			// controller and observed-health path. Chart templates or local
			// preview helpers alone are not an operational capability.
			"gitops": argoConfigured, "git": gitConfigured,
			"argoCD": argoConfigured, "argo": argoConfigured,
			"traefik": traefikConfigured, "edge": edgeConfigured,
			"monitoring": monitoringConfigured,
			"metrics":    monitoringConfigured,
			"logs":       runtimeConfigured,
			"builder":    buildsConfigured, "builds": buildsConfigured, "buildLogs": buildLogsConfigured,
			"autoDeploy": autoDeployConfigured,
			"registry":   registryConfigured, "managedRegistry": managedRegistryConfigured,
			"privateRegistryPulls": registryPullsConfigured,
			"gitSSH":               s.gitSSHKeys != nil,
			"gitSSHBuilds":         gitSSHBuildsConfigured,
			"deploymentRollbacks":  deploymentRollbacksConfigured,
			"imageTagResolution":   imageTagResolutionConfigured,
			"variableSets":         variableSetsConfigured,
			"helmDeployments":      helmDeploymentsConfigured, "helmRollbacks": helmRollbacksConfigured,
			"rollbacks":   aggregateRollbacksConfigured(deploymentRollbacksConfigured, helmRollbacksConfigured),
			"certManager": certManagerConfigured, "customCertificates": customCertificatesConfigured,
			"certificateIssuerCatalog":    certManagerConfigured && s.certificateIssuers != nil,
			"certificateIssuerManagement": certificateIssuerManagementConfigured,
			"sslip":                       sslipConfigured,
			"externalDNS":                 argoConfigured && gitConfigured && edgeConfigured && s.edgeFeatures.ExternalDNS && s.externalDNS != nil && s.externalDNS.service != nil,
			"traefikMiddlewares":          middlewareProfilesConfigured,
			"middlewareProfiles":          middlewareProfilesConfigured,
			"externalDNSConfiguration":    s.externalDNS != nil && s.externalDNS.service != nil,
			"secretBindings":              secretBindingsConfigured, "serviceAccounts": true,
			"teams": true, "githubAppSharing": true, "githubAppSetup": s.githubSetup != nil,
		},
		FeatureStates: map[string]string{
			"gitops": featureState(argoEnabled, argoConfigured), "git": featureState(gitEnabled, gitConfigured),
			"argoCD": featureState(argoEnabled, argoConfigured), "argo": featureState(argoEnabled, argoConfigured),
			"traefik": featureState(edgeEnabled && s.edgeFeatures.Traefik, traefikConfigured), "edge": featureState(edgeEnabled, edgeConfigured),
			"builder": featureState(buildEnabled, buildsConfigured), "builds": featureState(buildEnabled, buildsConfigured),
			"gitSSHBuilds": featureState(s.gitSSHKeys != nil && s.builds != nil && s.buildReadiness != nil, gitSSHBuildsConfigured),
		},
		Defaults: map[string]string{"buildPlatform": s.defaultBuildPlatform},
		Limits:   map[string]any{"deploymentReplicas": 100, "environmentVariables": 256, "workloadPorts": 32, "tolerations": 32, "idempotencyKeyBytes": 128, "serviceAccountTokenMaxTTLSeconds": int64(automation.MaxTokenTTL.Seconds())},
	})
}

func aggregateRollbacksConfigured(deployment, helm bool) bool { return deployment || helm }

func filterAutomationActions(actions []string, scopes []domain.AutomationScope) []string {
	result := make([]string, 0, len(actions))
	for _, action := range actions {
		allowed := false
		switch action {
		case "applications:read", "deployments:read", "environments:read", "projects:read", "deployment-config:read", "deployment-config:validate", "operations:read", "metrics:read", "secret-bindings:read", "app-sources:read", "builds:read", "external-dns-integrations:read", "helm-releases:read":
			allowed = automation.Allows(scopes, domain.AutomationScopeAppRead)
		case "applications:create", "applications:delete", "deployments:create", "deployments:update", "environments:create", "environments:delete", "projects:create", "projects:delete", "deployment-config:preview", "deployment-config:write", "helm-releases:deploy", "helm-releases:disable", "helm-releases:retry", "helm-releases:rollback":
			allowed = automation.Allows(scopes, domain.AutomationScopeAppEdit)
		case "logs:read":
			allowed = automation.Allows(scopes, domain.AutomationScopeLogsRead)
		case "app-sources:write", "builds:cancel", "builds:retry":
			allowed = automation.Allows(scopes, domain.AutomationScopeBuildCreate)
		}
		if allowed {
			result = append(result, action)
		}
	}
	return result
}

type monitoringStatusResponse struct {
	Mode       string    `json:"mode"`
	Status     string    `json:"status"`
	Available  bool      `json:"available"`
	Message    string    `json:"message"`
	ObservedAt time.Time `json:"observedAt"`
}

func (s *Server) monitoringStatus(w http.ResponseWriter, r *http.Request) {
	response := monitoringStatusResponse{
		Mode:       s.monitoringMode,
		Status:     "disabled",
		Available:  false,
		Message:    "Monitoring is disabled for this installation. Metrics are unavailable and are never reported as zero.",
		ObservedAt: time.Now().UTC(),
	}
	if s.monitoringMode == "managed" || s.monitoringMode == "existing" {
		response.Status = "unavailable"
		response.Message = "Monitoring is configured, but the Prometheus query boundary is unavailable."
		if s.metrics != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := s.metrics.Probe(ctx); err == nil {
				response.Status = "available"
				response.Available = true
				response.Message = "The scoped Prometheus query boundary is available."
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}
