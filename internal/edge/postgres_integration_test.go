package edge

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLEdgeRuntimeContract(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateEdgeTestDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const userID = "22222222-2222-4222-8222-222222222222"
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_readiness WHERE worker_id IN ($1,$2)`, testWorkerID, "edge-worker-test-0002")
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE integration_id=$1 OR integration_id IS NULL`, testExternalDNSIntegrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, testExternalDNSIntegrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,'edge-runtime-test','platform-admin','edge-runtime-test','edge-runtime-test',1,$2)`, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integrations(
		id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,
		destructive_sync_confirmed,operator_profile_ref,created_by,created_at,updated_at
	) VALUES($1,'edge-primary','Edge primary','adopted','cloudflare','kuberploy.primary',
		'["example.com","prod.example.com"]'::jsonb,'upsert-only',false,'external-dns-profile',$2,$3,$3)`,
		testExternalDNSIntegrationID, userID, now); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	config := testRuntimeConfig()
	digest, _ := config.Digest()
	targets, _ := config.DesiredTargets()
	if err = store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}

	first, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found {
		t.Fatalf("first claim: %#v %v", first, err)
	}
	second, found, err := store.ClaimTarget(ctx, "edge-worker-test-0002", RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found || second.Target.Key == first.Target.Key {
		t.Fatalf("multi-worker claim collided: first=%#v second=%#v found=%v err=%v", first, second, found, err)
	}
	// Idempotent synchronization cannot invalidate either worker.
	if err = store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	firstUpdated, err := store.HeartbeatTarget(ctx, first, now.Add(time.Second), config.WorkLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordTargetRetry(ctx, first, "stale-worker", false, now.Add(2*time.Second), now.Add(time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("superseded lease finalized: %v", err)
	}
	ready := func(lease Lease, label string) {
		t.Helper()
		receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
			IdentityDigest: testDigest("pg-identity/" + label), ResourceVersionDigest: testDigest("pg-version/" + label)}
		if _, readyErr := store.RecordTargetReady(ctx, lease, receipt, now.Add(2*time.Second), now.Add(config.PollInterval)); readyErr != nil {
			t.Fatalf("ready %s: %v", label, readyErr)
		}
	}
	ready(firstUpdated, firstUpdated.Target.Key)
	ready(second, second.Target.Key)
	third, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(2*time.Second), config.WorkLease)
	if err != nil || !found || third.Target.Key == first.Target.Key || third.Target.Key == second.Target.Key {
		t.Fatalf("third claim: %#v found=%v err=%v", third, found, err)
	}
	ready(third, third.Target.Key)
	readiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 1, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: len(targets), StartedAt: now, ObservedAt: now.Add(2 * time.Second), LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err = store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	regressed := readiness
	regressed.ObservedAt = now.Add(time.Second)
	if err = store.RecordReadiness(ctx, regressed); !errors.Is(err, ErrConflict) {
		t.Fatalf("regressed worker heartbeat accepted: %v", err)
	}
	jumped := readiness
	jumped.WorkerEpoch = 3
	if err = store.RecordReadiness(ctx, jumped); !errors.Is(err, ErrConflict) {
		t.Fatalf("worker epoch jump accepted: %v", err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(3*time.Second), config.ReadinessMaxAge); err != nil {
		t.Fatalf("fresh exact PG runtime is not ready: %v", err)
	}

	// Safe integration metadata is evaluated live. No credential or Secret
	// column is selected by the edge store.
	if _, err = pool.Exec(ctx, `UPDATE external_dns_integrations SET sync_policy='sync',
		destructive_sync_confirmed=true,runtime_revision=runtime_revision+1,updated_at=$2 WHERE id=$1`, testExternalDNSIntegrationID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(5*time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("changed integration metadata remained ready: %v", err)
	}
	if err = store.SynchronizeTargets(ctx, digest, targets, now.Add(5*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale external-dns profile synchronized: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE external_dns_integrations SET sync_policy='upsert-only',
		destructive_sync_confirmed=false,runtime_revision=runtime_revision+1,updated_at=$2 WHERE id=$1`, testExternalDNSIntegrationID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(7*time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("restored fields improperly reused a stale runtime revision: %v", err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, len(targets), now.Add(config.ReadinessMaxAge+time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale PG readiness accepted: %v", err)
	}

	// The exact observed UID/spec digest is pinned across every later poll.
	reobserve, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("re-observation claim: %#v %v", reobserve, err)
	}
	changed := ObservationReceipt{TargetKey: reobserve.Target.Key, DesiredDigest: reobserve.Target.DesiredDigest,
		IdentityDigest: testDigest("replacement-kubernetes-uid"), ResourceVersionDigest: testDigest("new-resource-version")}
	if _, err = store.RecordTargetReady(ctx, reobserve, changed, now.Add(config.PollInterval+time.Second), now.Add(2*config.PollInterval)); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("replacement Kubernetes identity accepted: %v", err)
	}
	if _, err = store.RecordTargetRetry(ctx, reobserve, "resource-identity-changed", true,
		now.Add(2*config.PollInterval), now.Add(config.PollInterval+time.Second)); err != nil {
		t.Fatalf("identity mismatch could not be durably finalized: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE edge_runtime_targets SET observed_identity_digest=$3,updated_at=$4
		WHERE target_key=$1 AND profile_revision=$2`, reobserve.Target.Key, reobserve.Target.Revision,
		testDigest("direct-identity-replacement"), now.Add(config.PollInterval+2*time.Second)); err == nil {
		t.Fatal("database trigger accepted direct Kubernetes identity replacement")
	}
	changedConfig := config
	changedConfig.MinimumBackoff = 6 * time.Second
	changedDigest, _ := changedConfig.Digest()
	changedTargets, _ := changedConfig.DesiredTargets()
	if err = store.SynchronizeTargets(ctx, changedDigest, changedTargets, now.Add(config.PollInterval+3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale profile revision re-armed after durable revision advanced: %v", err)
	}
}

func TestPostgreSQLExternalDNSManagedReferencesAreFenced(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateEdgeTestDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const (
		integrationID = "33333333-3333-4333-8333-333333333333"
		userID        = "44444444-4444-4444-8444-444444444444"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_readiness WHERE worker_id=$1`, testWorkerID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE integration_id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,'edge-managed-dns-test','platform-admin','edge-managed-dns-test','edge-managed-dns-test',1,$2)`, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integrations(
		id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,
		destructive_sync_confirmed,credential_secret_ref,provider_config_ref,egress_config_ref,
		created_by,created_at,updated_at
	) VALUES($1,'edge-managed','Edge managed','managed','cloudflare','kuberploy.managed',
		'["managed.example.com"]'::jsonb,'upsert-only',false,'dns-credentials','cloudflare-primary','cloudflare-egress',$2,$3,$3)`,
		integrationID, userID, now); err != nil {
		t.Fatal(err)
	}

	config := testRuntimeConfig()
	config.Profiles.Traefik = nil
	config.Profiles.CertManager = nil
	profile := config.Profiles.ExternalDNS[0]
	profile.IntegrationID = integrationID
	profile.Mode = ModeManaged
	profile.ProfileConfigMap = "edge-managed-profile"
	profile.ProviderKind = "cloudflare"
	profile.CredentialSecretRef = "dns-credentials"
	profile.ProviderConfigRef = "cloudflare-primary"
	profile.EgressConfigRef = "cloudflare-egress"
	profile.TXTOwnerID = "kuberploy.managed"
	profile.DomainFilters = []string{"managed.example.com"}
	config.Profiles.ExternalDNS = []ExternalDNSProfile{profile}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := config.DesiredTargets()
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%#v err=%v", targets, err)
	}
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SynchronizeTargets(ctx, digest, targets, now); err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now, config.WorkLease)
	if err != nil || !found {
		t.Fatalf("claim=%#v found=%v err=%v", lease, found, err)
	}
	receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: testDigest("managed-dns-identity"), ResourceVersionDigest: testDigest("managed-dns-version")}
	observedAt := now.Add(time.Second)
	if _, err = store.RecordTargetReady(ctx, lease, receipt, observedAt, now.Add(config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	readiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 1, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: 1, StartedAt: now, ObservedAt: observedAt, LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err = store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, 1, now.Add(2*time.Second), config.ReadinessMaxAge); err != nil {
		t.Fatalf("exact managed identity is not ready: %v", err)
	}

	if _, err = pool.Exec(ctx, `UPDATE edge_runtime_targets SET external_credential_secret_ref='substituted-secret',updated_at=$3
		WHERE target_key=$1 AND profile_revision=$2`, targets[0].Key, targets[0].Revision, now.Add(2*time.Second)); err == nil {
		t.Fatal("database trigger accepted a managed credential reference identity mutation")
	}
	if _, err = pool.Exec(ctx, `UPDATE external_dns_integrations SET credential_secret_ref='rotated-secret',runtime_revision=runtime_revision+1,updated_at=$2
		WHERE id=$1`, integrationID, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, 1, now.Add(4*time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("durable managed reference drift remained ready: %v", err)
	}
	if err = store.SynchronizeTargets(ctx, digest, targets, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale managed reference synchronized: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE external_dns_integrations SET credential_secret_ref='dns-credentials',runtime_revision=runtime_revision+1,updated_at=$2
		WHERE id=$1`, integrationID, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.RuntimeReady(ctx, RuntimeContract, digest, 1, now.Add(6*time.Second), config.ReadinessMaxAge); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("restored managed fields reused stale revision readiness: %v", err)
	}
}

func TestPostgreSQLEdgeSemanticTransitionsWakeGitPolicyRevalidation(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateEdgeTestDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const (
		projectID     = "a1111111-1111-4111-8111-111111111111"
		environmentID = "a2222222-2222-4222-8222-222222222222"
		bindingID     = "a3333333-3333-4333-8333-333333333333"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE target_key='traefik'`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Edge wake project','edge-wake-project',NULL,$2)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production','edge-wake-runtime','edge-wake-argo',$3)`, environmentID, projectID, now); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	if _, err = pool.Exec(ctx, `INSERT INTO git_repository_bindings(
		id,kind,scope_id,project_id,environment_id,cluster_id,provider,installation_id,repository_id,
		repository_owner,repository_name,target_ref,path_prefix,credential_mode,credential_secret_name,
		state,target_head_revision,indexed_revision,projection_generation,parser_version,
		target_head_observed_at,indexed_at,created_at,updated_at
	) VALUES($1,'environment',$2,$3,$2,NULL,'github',9000000000000001,9000000000000002,
		'kuberploy','edge-wake','refs/heads/main',$4,'github-app','','ready',$5,$5,1,'appconfig-v1alpha1',$6,$6,$6,$6)`,
		bindingID, environmentID, projectID, "tenants/"+projectID+"/environments/"+environmentID, head, now); err != nil {
		t.Fatal(err)
	}
	config := testRuntimeConfig()
	config.Profiles.CertManager = nil
	config.Profiles.ExternalDNS = nil
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := config.DesiredTargets()
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%#v err=%v", targets, err)
	}
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	assertState := func(want string) {
		t.Helper()
		var state string
		if scanErr := pool.QueryRow(ctx, `SELECT state FROM git_repository_bindings WHERE id=$1`, bindingID).Scan(&state); scanErr != nil || state != want {
			t.Fatalf("binding state=%q want=%q err=%v", state, want, scanErr)
		}
	}
	resetReady := func(at time.Time) {
		t.Helper()
		if _, resetErr := pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',updated_at=$2 WHERE id=$1`, bindingID, at); resetErr != nil {
			t.Fatal(resetErr)
		}
	}
	if err = store.SynchronizeTargets(ctx, digest, targets, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertState("indexing")
	resetReady(now.Add(2 * time.Second))
	lease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(3*time.Second), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("claim found=%v lease=%#v err=%v", found, lease, err)
	}
	receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: testDigest("wake-identity"), ResourceVersionDigest: testDigest("wake-version")}
	if _, err = store.RecordTargetReady(ctx, lease, receipt, now.Add(4*time.Second), now.Add(config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("indexing")
	resetReady(now.Add(5 * time.Second))
	retryLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("retry claim found=%v lease=%#v err=%v", found, retryLease, err)
	}
	if _, err = store.RecordTargetRetry(ctx, retryLease, "kubernetes-unavailable", false,
		now.Add(2*config.PollInterval), now.Add(config.PollInterval+time.Second)); err != nil {
		t.Fatal(err)
	}
	assertState("indexing")
}

func migrateEdgeTestDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	return testdb.ApplyMigrations(ctx, pool)
}
