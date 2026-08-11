package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestExternalDNSManagementSQLPaths(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "external_dns_" + strings.ReplaceAll(id.New(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE") //nolint:errcheck
	scopedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := scopedURL.Query()
	query.Set("search_path", schema)
	scopedURL.RawQuery = query.Encode()
	migrationPool, err := pgxpool.New(ctx, scopedURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = testdb.ApplyMigrations(ctx, migrationPool); err != nil {
		migrationPool.Close()
		t.Fatal(err)
	}
	migrationPool.Close()
	st, err := Open(ctx, scopedURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	actorID := id.New()
	viewerID := id.New()
	identity := actorID[:8]
	if _, err = st.pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','external-dns-integration',$3,1,$4)`,
		actorID, "dns-admin-"+identity, "admin-"+identity, databaseTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO access_grants(
		id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at
	) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`,
		id.New(), actorID, databaseTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject(ctx, actorID, "dns-project-"+identity, "dns-project-fingerprint-"+identity,
		domain.CreateProject{Name: "DNS integration " + identity, Slug: "dns-" + identity})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, actorID, "dns-environment-"+identity, "dns-environment-fingerprint-"+identity,
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := st.CreateApplication(ctx, actorID, "dns-application-"+identity, "dns-application-fingerprint-"+identity,
		domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	projectionStore, err := gitprojection.NewPostgreSQLStore(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	projectionBinding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), project.Value.ID, environment.Value.ID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 8_100_000_000_000_001, RepositoryID: 8_100_000_000_000_002, Owner: "kuberploy", Name: "external-dns-policy"},
		"refs/heads/main", databaseTime(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	indexedAt := projectionBinding.CreatedAt.Add(time.Second)
	projectionBinding.TargetHeadRevision, projectionBinding.IndexedRevision = strings.Repeat("a", 40), strings.Repeat("a", 40)
	projectionBinding.TargetHeadObservedAt, projectionBinding.IndexedAt, projectionBinding.UpdatedAt = indexedAt, indexedAt, indexedAt
	projectionBinding.ProjectionGeneration, projectionBinding.State = 1, gitprojection.BindingReady
	if err = projectionStore.PutBinding(ctx, projectionBinding); err != nil {
		t.Fatal(err)
	}

	integration := domain.ExternalDNSIntegration{
		ID: id.New(), Slug: "public-" + identity, Name: "Public DNS " + identity,
		Mode: "managed", ProviderKind: "cloudflare", TXTOwnerID: "kuberploy." + identity,
		AllowedDomainSuffixes: []string{identity + ".example.com"}, SyncPolicy: "upsert-only",
		CredentialSecretRef: "dns-credentials-" + identity, ProviderConfigRef: "provider-" + identity,
		EgressConfigRef: "egress-" + identity, EnvironmentIDs: []string{environment.Value.ID}, CreatedBy: actorID,
	}
	created, err := st.CreateExternalDNSIntegrationForActor(ctx, actorID, "dns-create-"+identity, "dns-create-fingerprint-"+identity, "request-"+identity, integration)
	if err != nil || created.Replay || created.Value.ID != integration.ID {
		t.Fatalf("create=%#v err=%v", created, err)
	}
	if created.Value.RuntimeRevision != 1 || created.Value.Lifecycle != "active" || created.Value.ProtectedGitState != "pending" {
		t.Fatalf("create lifecycle receipt=%#v", created.Value)
	}
	publicationTime := databaseTime(time.Now())
	contentDigest := "sha256:" + strings.Repeat("c", 64)
	commit := strings.Repeat("d", 40)
	if err = st.RecordExternalDNSPublication(ctx, integration.ID, 1, false, contentDigest, commit, publicationTime); err != nil {
		t.Fatalf("record materialization: %v", err)
	}
	runtimeItems, err := st.ListExternalDNSIntegrationsForRuntime(ctx, 64)
	runtimeItem, found := findExternalDNSRuntimeItem(runtimeItems, integration.ID)
	if err != nil || !found || runtimeItem.ProtectedGitState != "materialized" {
		t.Fatalf("runtime materialization=%#v err=%v", runtimeItems, err)
	}
	template := externaldns.ManagedRuntimeTemplate{Namespace: "kuberploy-dns", Version: "v0.18.0", Image: "registry.k8s.io/external-dns/external-dns@sha256:" + strings.Repeat("a", 64), ServiceAccount: "external-dns-managed"}
	profile, err := externaldns.ManagedProfile(runtimeItem, template)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig := edge.DefaultRuntimeConfig()
	runtimeConfig.Enabled = true
	runtimeConfig.Profiles.ExternalDNS = []edge.ExternalDNSProfile{profile}
	runtimeDigest, err := runtimeConfig.Digest()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := runtimeConfig.DesiredTargets()
	if err != nil {
		t.Fatal(err)
	}
	edgeStore, err := edge.NewPostgreSQLStore(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = edgeStore.SynchronizeTargets(ctx, runtimeDigest, targets, databaseTime(time.Now())); err != nil {
		t.Fatalf("initial target sync: %v", err)
	}
	invalidatedBinding, err := projectionStore.Binding(ctx, projectionBinding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidatedBinding.State != gitprojection.BindingIndexing || !invalidatedBinding.UpdatedAt.After(projectionBinding.UpdatedAt) ||
		invalidatedBinding.TargetHeadRevision != projectionBinding.TargetHeadRevision || invalidatedBinding.IndexedRevision != projectionBinding.IndexedRevision {
		t.Fatalf("external DNS create did not invalidate the exact-head policy projection: %#v", invalidatedBinding)
	}
	observedBinding, _, err := projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{
		BindingID: projectionBinding.ID, Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef,
		Commit: projectionBinding.TargetHeadRevision, Source: gitprojection.ObservationPoll,
		ProviderRequest: "external-dns-policy-revalidation", ObservedAt: invalidatedBinding.UpdatedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if observedBinding.State != gitprojection.BindingIndexing {
		t.Fatalf("same-head provider observation erased the metadata revalidation request: %#v", observedBinding)
	}
	replayed, err := st.CreateExternalDNSIntegrationForActor(ctx, actorID, "dns-create-"+identity, "dns-create-fingerprint-"+identity, "request-replay-"+identity, integration)
	if err != nil || !replayed.Replay || replayed.Value.ID != integration.ID {
		t.Fatalf("replay=%#v err=%v", replayed, err)
	}
	if _, err = st.CreateExternalDNSIntegrationForActor(ctx, actorID, "dns-create-"+identity, "different-fingerprint-"+identity, "request-conflict-"+identity, integration); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("idempotency fingerprint mutation err=%v", err)
	}

	if _, err = st.ListExternalDNSIntegrationsForActor(ctx, actorID); err != nil {
		t.Fatalf("platform integration list err=%v", err)
	}
	environmentItems, err := st.ExternalDNSIntegrationsForEnvironmentActor(ctx, actorID, environment.Value.ID)
	if err != nil || len(environmentItems) != 1 || environmentItems[0].ID != integration.ID {
		t.Fatalf("environment catalog=%#v err=%v", environmentItems, err)
	}
	applicationItems, err := st.ExternalDNSIntegrationsForApplicationActor(ctx, actorID, application.Value.ID, environment.Value.ID)
	if err != nil || len(applicationItems) != 1 || applicationItems[0].ID != integration.ID {
		t.Fatalf("application catalog=%#v err=%v", applicationItems, err)
	}

	if _, err = st.pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'developer','external-dns-integration',$3,1,$4)`,
		viewerID, "dns-viewer-"+identity, "viewer-"+identity, databaseTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateProjectAccessGrant(ctx, actorID, "dns-grant-"+identity, "dns-grant-fingerprint-"+identity, "request-grant-"+identity,
		domain.CreateAccessGrant{ProjectID: project.Value.ID, SubjectUserID: viewerID, Role: domain.RoleViewer, ScopeType: domain.ScopeApplication, ScopeID: application.Value.ID}); err != nil {
		t.Fatal(err)
	}
	applicationItems, err = st.ExternalDNSIntegrationsForApplicationActor(ctx, viewerID, application.Value.ID, environment.Value.ID)
	if err != nil || len(applicationItems) != 1 || applicationItems[0].ID != integration.ID {
		t.Fatalf("scoped viewer application catalog=%#v err=%v", applicationItems, err)
	}
	if _, err = st.ExternalDNSIntegrationsForEnvironmentActor(ctx, viewerID, environment.Value.ID); !errors.Is(err, base.ErrForbidden) && !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("application grant crossed into environment catalog: %v", err)
	}
	if _, err = st.ListExternalDNSIntegrationsForActor(ctx, viewerID); !errors.Is(err, base.ErrForbidden) && !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("application grant crossed into platform metadata: %v", err)
	}

	updated := created.Value
	updated.Name = "Updated DNS " + identity
	updateResult, err := st.UpdateExternalDNSIntegrationForActor(ctx, actorID, "dns-update-"+identity, "dns-update-fingerprint-"+identity, "request-update-"+identity, updated)
	if err != nil || updateResult.Replay || updateResult.Value.Name != updated.Name {
		t.Fatalf("update=%#v err=%v", updateResult, err)
	}
	if updateResult.Value.RuntimeRevision != 2 || updateResult.Value.ProtectedGitState != "pending" {
		t.Fatalf("update did not advance exact runtime revision: %#v", updateResult.Value)
	}
	if err = st.RecordExternalDNSPublication(ctx, integration.ID, 1, false, contentDigest, commit, databaseTime(time.Now())); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("stale publication accepted: %v", err)
	}
	if err = edgeStore.SynchronizeTargets(ctx, runtimeDigest, targets, databaseTime(time.Now())); !errors.Is(err, edge.ErrConflict) {
		t.Fatalf("stale edge target revision accepted: %v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE external_dns_integrations SET name=name || ' invalid' WHERE id=$1`, integration.ID); err == nil {
		t.Fatal("desired state changed without exact revision increment")
	}
	if _, err = st.pool.Exec(ctx, `UPDATE external_dns_integrations SET runtime_revision=runtime_revision+1 WHERE id=$1`, integration.ID); err == nil {
		t.Fatal("runtime revision advanced without desired-state change")
	}
	if err = st.RecordExternalDNSPublication(ctx, integration.ID, 2, false, contentDigest, commit, databaseTime(time.Now())); err != nil {
		t.Fatalf("current publication rejected: %v", err)
	}
	reinvalidatedBinding, err := projectionStore.Binding(ctx, projectionBinding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reinvalidatedBinding.State != gitprojection.BindingIndexing || !reinvalidatedBinding.UpdatedAt.After(observedBinding.UpdatedAt) {
		t.Fatalf("external DNS update did not issue a new policy revalidation wakeup: %#v", reinvalidatedBinding)
	}
	updateReplay, err := st.UpdateExternalDNSIntegrationForActor(ctx, actorID, "dns-update-"+identity, "dns-update-fingerprint-"+identity, "request-update-replay-"+identity, updated)
	if err != nil || !updateReplay.Replay || updateReplay.Value.Name != updated.Name {
		t.Fatalf("update replay=%#v err=%v", updateReplay, err)
	}
	renamed := updateResult.Value
	renamed.Slug = "renamed-" + identity
	if _, err = st.UpdateExternalDNSIntegrationForActor(ctx, actorID, "dns-rename-"+identity, "dns-rename-fingerprint-"+identity, "request-rename-"+identity, renamed); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("immutable slug mutation err=%v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE external_dns_integrations SET txt_owner_id=$2 WHERE id=$1`, integration.ID, "renamed."+identity); err == nil {
		t.Fatal("database trigger accepted immutable TXT owner mutation")
	}

	var auditCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE target_type='external-dns-integration' AND target_id=$1
		AND action IN ('external-dns-integration.create','external-dns-integration.update')`, integration.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("mutation audit count=%d, want 2", auditCount)
	}
	deactivated, err := st.DeactivateExternalDNSIntegrationForActor(ctx, actorID, "dns-deactivate-"+identity, "dns-deactivate-fingerprint-"+identity, "request-deactivate-"+identity, integration.ID)
	if err != nil || deactivated.Value.Lifecycle != "deactivated" || deactivated.Value.ProtectedGitState != "pending" {
		t.Fatalf("deactivate=%#v err=%v", deactivated, err)
	}
	applicationItems, err = st.ExternalDNSIntegrationsForApplicationActor(ctx, actorID, application.Value.ID, environment.Value.ID)
	if err != nil || len(applicationItems) != 0 {
		t.Fatalf("deactivated tenant catalog=%#v err=%v", applicationItems, err)
	}
	runtimeItems, err = st.ListExternalDNSIntegrationsForRuntime(ctx, 64)
	_, found = findExternalDNSRuntimeItem(runtimeItems, integration.ID)
	if err != nil || !found {
		t.Fatalf("pending dematerialization missing: %#v %v", runtimeItems, err)
	}
	if err = st.RecordExternalDNSPublication(ctx, integration.ID, 2, true, "", strings.Repeat("e", 40), databaseTime(time.Now())); err != nil {
		t.Fatalf("dematerialization receipt: %v", err)
	}
	runtimeItems, err = st.ListExternalDNSIntegrationsForRuntime(ctx, 64)
	_, found = findExternalDNSRuntimeItem(runtimeItems, integration.ID)
	if err != nil || found {
		t.Fatalf("dematerialized target kept reconciling: %#v %v", runtimeItems, err)
	}
}

func findExternalDNSRuntimeItem(items []domain.ExternalDNSIntegration, id string) (domain.ExternalDNSIntegration, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return domain.ExternalDNSIntegration{}, false
}
