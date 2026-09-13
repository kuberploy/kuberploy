package httpapi_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

// Only provider resolution is substituted: HTTP reads the real authorized
// PostgreSQL deployment, then the real build store accepts the derived command.
type postgresSourceAcceptance struct {
	store      *builds.PostgreSQLStore
	definition builds.BuildDefinition
	before     func(context.Context) error
}

func (b *postgresSourceAcceptance) AcceptSourceDeployment(ctx context.Context, command builds.SourceDeploymentCommand) (builds.SourceDeploymentAcceptance, error) {
	command.DefinitionID, command.ExpectedDefinitionDigest = b.definition.ID, b.definition.DefinitionDigest
	command.Execution, command.CommitSHA, command.AcceptedAt = b.definition.Spec.Execution, strings.Repeat("a", 40), time.Now().UTC()
	if b.before != nil {
		if err := b.before(ctx); err != nil {
			return builds.SourceDeploymentAcceptance{}, err
		}
	}
	return b.store.AcceptSourceDeployment(ctx, command)
}

func TestPostgreSQLSourceDeploymentHTTPUsesIndependentAuthorityTokens(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	buildStore, err := builds.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	acceptance := &postgresSourceAcceptance{store: buildStore}
	projection := &projectionHTTPBackend{}
	srv := httptest.NewServer(httpapi.New(httpapi.Options{Store: st, BootstrapToken: "one-time-secret",
		SourceDeployments: acceptance,
		HighRiskLimiter:   ratelimit.NewMemoryLimiter(10_000)}))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	f := &apiFixture{t: t, server: srv, client: &http.Client{Jar: jar}}
	user := f.bootstrap()
	project := decode[domain.Project](t, f.request(http.MethodPost, "/v1/projects", "pg-source-project", map[string]string{"name": "Source HTTP"}))
	environment := decode[domain.Environment](t, f.request(http.MethodPost, "/v1/environments", "pg-source-environment", map[string]string{"projectId": project.ID, "name": "Production"}))
	application := decode[domain.Application](t, f.request(http.MethodPost, "/v1/applications", "pg-source-app", map[string]string{"projectId": project.ID, "name": "App"}))
	deploymentResponse := f.request(http.MethodPost, "/v1/deployments", "pg-source-deployment", map[string]any{
		"environmentId": environment.ID, "applicationId": application.ID,
		"image": "registry.test/app@sha256:" + strings.Repeat("1", 64), "runtime": domain.DefaultWorkloadRuntime(8080, nil)})
	if deploymentResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment status=%d problem=%+v", deploymentResponse.StatusCode, decode[httpapi.Problem](t, deploymentResponse))
	}
	operation := decode[domain.Operation](t, deploymentResponse)
	config, err := st.GetDeploymentConfigForActor(ctx, user.ID, operation.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	binding := projectedHTTPBinding(t, project.ID, environment.ID, now.Add(-time.Minute))
	document, err := gitprojection.NewDocument(binding, 1, application.ID, binding.IndexedRevision, binding.IndexedRevision,
		strings.Repeat("e", 40), config.RawYAML, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	gitETag := `"sha256:` + strings.Repeat("b", 64) + `"`
	projection.bundle = gitprojection.Bundle{ETag: gitETag, Documents: []gitprojection.Document{document},
		Dependencies: []gitprojection.DependencyState{{Path: "tenants/" + project.ID + "/variables.yaml"}, {Path: binding.Prefix + "/variables.yaml"}}}
	installationID, repositoryID, registryID := id.New(), id.New(), id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at)
		VALUES($1,34,'kuberploy','Organization',$2,'private','selected',1,$3,$3)`, installationID, user.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,push_credential_ref,cache_credential_ref,created_at,updated_at)
		VALUES($1,'source-http-registry','managed','registry.test','kuberploy','registry-push','registry-cache',$2,$2)`, registryID, now); err != nil {
		t.Fatal(err)
	}
	if err = buildStore.PutInstallation(ctx, builds.Installation{ID: installationID, AppID: 12, GitHubInstallationID: 34,
		Account: githubapp.AccountIdentity{ID: 56, Login: "kuberploy", Type: "Organization"}, RepositorySelection: "selected",
		Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
		Lifecycle:   builds.InstallationActive, LastVerifiedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = buildStore.PutRepository(ctx, builds.Repository{ID: repositoryID, InstallationID: installationID,
		Identity:  githubapp.RepositoryIdentity{ID: 78, OwnerID: 56, OwnerLogin: "kuberploy", Name: "fixture"},
		Lifecycle: builds.RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	resources := builder.ContainerResources{CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "256Mi",
		CPULimit: "1", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi"}
	execution := builds.ExecutionSettings{Namespace: "kuberploy-build-dind", PodServiceAccount: "kuberploy-build-pod",
		BuilderAgentImage: "registry.test/builder@sha256:" + strings.Repeat("1", 64), BuildKitImage: builder.DefaultBuildKitImage,
		NodeSelector: map[string]string{}, CheckoutResources: resources, DinDResources: resources, AgentResources: resources,
		WorkspaceSizeLimit: "10Gi", SocketSizeLimit: "16Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "20Gi",
		ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 3600,
		Egress: []builder.EgressEndpoint{{CIDR: "192.0.2.10/32", Ports: []int{443}}}}
	acceptance.definition, err = builds.PrepareDefinition(builds.BuildDefinition{ID: id.New(), ProjectID: project.ID, ServiceID: application.ID,
		SourceKind: builds.SourceGitHub, InstallationID: installationID, RepositoryID: repositoryID, TriggerRef: "refs/heads/main", Enabled: true,
		Spec: builds.DefinitionSpec{ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"},
			Registry: builds.RegistryBinding{TargetID: registryID, Mode: builds.RegistryManaged, Server: "registry.test", RepositoryPrefix: "kuberploy",
				PushCredentialSecret: "registry-push", CacheCredentialSecret: "registry-cache"}, CacheTrustLane: "trusted", CacheImports: 1,
			Profile: builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"}, Execution: execution, MaxAttempts: 3}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = buildStore.PutDefinition(ctx, acceptance.definition); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	f.server = httptest.NewServer(httpapi.New(httpapi.Options{Store: st, SourceDeployments: acceptance,
		GitProjection: projection, GitProjectionReadiness: &projectionHTTPReadiness{}, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	defer f.server.Close()
	response := f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "pg-source-accept", map[string]string{"mode": "deploy"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("source acceptance status=%d problem=%+v", response.StatusCode, decode[httpapi.Problem](t, response))
	}
	accepted := decode[struct {
		IntentID string `json:"intentId"`
	}](t, response)
	var persistedGitETag string
	if err = pool.QueryRow(ctx, `SELECT source_config_etag FROM source_deployment_intents WHERE id=$1`, accepted.IntentID).Scan(&persistedGitETag); err != nil || persistedGitETag != gitETag {
		t.Fatalf("durable Git authority token=%s err=%v", persistedGitETag, err)
	}
	// Change the database projection after the HTTP read. Its separate token
	// must still reject a stale command, without replacing the Git token above.
	acceptance.before = func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `UPDATE deployments SET config_version=config_version+1,config_etag=$2 WHERE id=$1`, operation.TargetID,
			domain.DeploymentConfigETag(operation.TargetID, 2, config.RawYAML))
		return err
	}
	response = f.request(http.MethodPost, "/v1/deployments/"+operation.TargetID+"/source-build", "pg-source-stale-01", map[string]string{"mode": "deploy"})
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusConflict || problem.Code != "BuildConflict" {
		t.Fatalf("stale projection status=%d problem=%+v", response.StatusCode, problem)
	}
}
