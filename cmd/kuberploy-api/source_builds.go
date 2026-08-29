package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

const githubAppSlugEnv = "KUBERPLOY_GITHUB_APP_SLUG"

var githubAppSlugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,98}[a-z0-9])?$`)

type sourceBuildCatalog interface {
	httpapi.BuildDefinitionCatalog
	builds.VerifiedInstallationCatalog
}

type sourceBuildAPI struct {
	store          *builds.PostgreSQLStore
	setup          httpapi.GitHubSetupBackend
	webhook        httpapi.GitHubWebhookBackend
	backend        httpapi.BuildBackend
	readiness      httpapi.ReadinessProbe
	settings       *builds.BuilderPlatformSettingsService
	webhookService *builds.WebhookService
}

func newSourceBuildAPI(ctx context.Context, databaseURL, publicURL, appSlug string, config builds.WorkerRuntimeConfig, catalog sourceBuildCatalog) (*sourceBuildAPI, error) {
	if !config.Enabled {
		return nil, nil
	}
	base, err := sourceBuildPublicURL(publicURL)
	if err != nil || !githubAppSlugRE.MatchString(appSlug) || catalog == nil {
		return nil, fmt.Errorf("invalid enabled GitHub build API configuration")
	}
	if err = githubapp.ProbeProjectedRuntime(ctx, config.GitHub); err != nil {
		return nil, err
	}
	client, err := githubapp.NewProjectedClient(config.GitHub)
	if err != nil {
		return nil, err
	}
	callbackURL := base + "/v1/github/installations/callback"
	exchanger, err := githubapp.NewProjectedOAuthCodeExchanger(config.GitHub, callbackURL)
	if err != nil {
		return nil, err
	}
	reader := githubapp.NewProjectedSecretReader()
	stateManager, err := githubapp.NewStateManager(config.GitHub, reader, nil, nil)
	if err != nil {
		return nil, err
	}
	verifier, err := githubapp.NewWebhookVerifier(config.GitHub, reader, nil)
	if err != nil {
		return nil, err
	}
	buildStore, err := builds.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	settings := &builds.BuilderPlatformSettingsService{Store: buildStore, Defaults: builds.DefaultBuilderPlatformSettings(config)}
	resolver := &httpapi.ServerBuildDefinitionResolver{Catalog: catalog, Runtime: config, Settings: settings}
	backend, err := httpapi.NewBuildBackendWithProvider(buildStore, resolver, client)
	if err != nil {
		buildStore.Close()
		return nil, err
	}
	identity, err := builds.RuntimeIdentity(config)
	if err != nil {
		buildStore.Close()
		return nil, err
	}
	setup := &builds.SetupService{StateManager: stateManager, Provider: &builds.GitHubSetupProvider{Exchanger: exchanger, Client: client},
		Store: buildStore, Catalog: catalog, InstallURL: "https://github.com/apps/" + appSlug + "/installations/new",
		OAuthClientID: config.GitHub.ClientID, OAuthCallbackURL: callbackURL, AppID: config.GitHub.AppID, HandoffTTL: config.GitHub.HandoffTTL}
	webhook := &builds.WebhookService{Verifier: verifier, Store: buildStore}
	readiness := &builds.RuntimeReadinessProbe{Store: buildStore, Identity: identity, MaxAge: builds.SourceBuildHeartbeatMaxAge}
	return &sourceBuildAPI{store: buildStore, setup: setup, webhook: webhook, webhookService: webhook, backend: backend, readiness: readiness, settings: settings}, nil
}

func (r *sourceBuildAPI) setGitProjectionWaker(store *gitprojection.PostgreSQLStore) {
	if r != nil && r.webhookService != nil && store != nil {
		r.webhookService.PushWaker = gitprojection.GitHubPushWaker{Store: store}
	}
}

func sourceBuildPublicURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("public URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", fmt.Errorf("GitHub builds require an HTTPS public origin")
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("GitHub builds require an HTTPS public origin")
		}
	}
	return "https://" + u.Host, nil
}

func (r *sourceBuildAPI) Close() {
	if r != nil && r.store != nil {
		r.store.Close()
	}
}
