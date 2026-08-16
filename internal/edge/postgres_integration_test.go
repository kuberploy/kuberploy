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

func TestAuthoritativePostgreSQLLeaseUsesStoredPrecision(t *testing.T) {
	now := time.Date(2026, time.August, 12, 1, 2, 3, 456789123, time.UTC)
	storedNow := now.Truncate(time.Microsecond)
	storedUntil := now.Add(2 * time.Minute).Truncate(time.Microsecond)
	config := testRuntimeConfig()
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := config.DesiredTargets()
	if err != nil {
		t.Fatal(err)
	}
	target := Target{DesiredTarget: targets[0], Active: true, State: StateAwaiting,
		NextObservationAt: storedNow, LeaseOwner: testWorkerID, LeaseEpoch: 1, LeaseUntil: &storedUntil,
		WorkerContract: RuntimeContract, WorkerConfigDigest: digest, CreatedAt: storedNow, UpdatedAt: storedNow}
	lease, err := authoritativePostgreSQLLease(target, testWorkerID, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Until.Equal(storedUntil) || lease.Until.Nanosecond()%1000 != 0 {
		t.Fatalf("lease did not preserve PostgreSQL precision: %s", lease.Until.Format(time.RFC3339Nano))
	}
}

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
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_readiness
			WHERE runtime_kind='edge' AND scope_key='global' AND worker_id IN ($1,$2)`, testWorkerID, "edge-worker-test-0002")
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE integration_id=$1 OR integration_id IS NULL`, testExternalDNSIntegrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, testExternalDNSIntegrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	// Production time.Now values normally carry nanoseconds while PostgreSQL
	// timestamptz stores microseconds. Keep that precision mismatch in this test
	// so returned claims and heartbeats must use the database-authoritative time.
	now := time.Now().UTC()
	if now.Nanosecond()%1000 == 0 {
		now = now.Add(time.Nanosecond)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
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
	heartbeat := readiness
	heartbeat.ObservedAt = readiness.ObservedAt.Add(time.Second)
	heartbeat.LeaseUntil = readiness.LeaseUntil.Add(time.Second)
	if err = store.RecordReadiness(ctx, heartbeat); err != nil {
		t.Fatalf("nanosecond readiness heartbeat did not preserve PostgreSQL identity: %v", err)
	}
	readiness = heartbeat
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
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_readiness WHERE runtime_kind='edge' AND scope_key='global' AND worker_id=$1`, testWorkerID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE integration_id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
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
		applicationID = "a4444444-4444-4444-8444-444444444444"
		userID        = "a5555555-5555-4555-8555-555555555555"
		integrationA  = "a6666666-6666-4666-8666-666666666666"
		integrationB  = "a7777777-7777-4777-8777-777777777777"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE target_key='traefik'`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id IN ($1,$2)`, integrationA, integrationB)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1,'edge-wake-admin','platform-admin','edge-wake-admin','edge-wake-admin',1,$2)`, userID, now); err != nil {
		t.Fatal(err)
	}
	for _, integration := range []struct{ id, slug, owner string }{
		{id: integrationA, slug: "edge-wake-a", owner: "kuberploy.wake-a"},
		{id: integrationB, slug: "edge-wake-b", owner: "kuberploy.wake-b"},
	} {
		if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integrations(
			id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,
			destructive_sync_confirmed,operator_profile_ref,created_by,created_at,updated_at
		) VALUES($1,$2,'Edge wake DNS','adopted','cloudflare',$3,'["example.test"]'::jsonb,
			'upsert-only',false,'external-dns-profile',$4,$5,$5)`, integration.id, integration.slug, integration.owner, userID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Edge wake project','edge-wake-project',NULL,$2)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production','edge-wake-runtime','edge-wake-runtime',$3)`, environmentID, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)
		VALUES($1,$2,'Edge wake application','edge-wake-application',$3)`, applicationID, projectID, now); err != nil {
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
	if _, err = pool.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at
	) VALUES($1,1,$2,'appconfig-v1alpha1','active',$3,$3)`, bindingID, head, now); err != nil {
		t.Fatal(err)
	}
	documentPath := "tenants/" + projectID + "/environments/" + environmentID + "/apps/" + applicationID + "/app.yaml"
	if _, err = pool.Exec(ctx, `INSERT INTO git_projected_documents(
		binding_id,generation,path,application_id,source_revision,config_revision,blob_id,content_sha256,
		raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at
	) VALUES($1,1,$2,$3,$4,$4,$5,$6,$7,'{}'::jsonb,false,
		'[{
			"code":"TraefikRuntimeUnobserved",
			"detail":"No fresh exact Traefik runtime observation is available for this route.",
			"pointer":"/spec/routes/0"
		}]'::jsonb,'config.kuberploy.io/v1alpha1','appconfig-v1alpha1',$8)`, bindingID, documentPath, applicationID,
		head, strings.Repeat("b", 40), "sha256:"+strings.Repeat("c", 64), []byte("kind: AppConfig\n"), now); err != nil {
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

	// An ordinary successful poll used to leave this exact persisted dynamic
	// diagnostic stranded forever. It now wakes only the matching binding so a
	// same-head policy activation can authoritatively clear or re-emit it.
	ordinaryLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("ordinary claim found=%v lease=%#v err=%v", found, ordinaryLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, ordinaryLease, receipt, now.Add(config.PollInterval+time.Second), now.Add(2*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("indexing")
	resetReady(now.Add(config.PollInterval + 2*time.Second))

	// Immutable parser/binding diagnostics are not runtime recovery signals.
	// Successful polls must neither clear them nor request a generation that
	// could make them readable without the exact schema/identity fences.
	if _, err = pool.Exec(ctx, `UPDATE git_projected_documents SET diagnostics=$2::jsonb
		WHERE binding_id=$1 AND generation=1 AND path=$3`, bindingID,
		`[{"code":"SchemaViolation","detail":"spec must be an object","pointer":"/spec"}]`, documentPath); err != nil {
		t.Fatal(err)
	}
	schemaLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(2*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("schema claim found=%v lease=%#v err=%v", found, schemaLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, schemaLease, receipt, now.Add(2*config.PollInterval+time.Second), now.Add(3*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("ready")
	if _, err = pool.Exec(ctx, `UPDATE git_projected_documents SET diagnostics=$2::jsonb
		WHERE binding_id=$1 AND generation=1 AND path=$3`, bindingID,
		`[{"code":"BindingMismatch","detail":"document identity does not match binding","pointer":"/metadata/id"}]`, documentPath); err != nil {
		t.Fatal(err)
	}
	bindingLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(3*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("binding claim found=%v lease=%#v err=%v", found, bindingLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, bindingLease, receipt, now.Add(3*config.PollInterval+time.Second), now.Add(4*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("ready")

	// A diagnostic row is authoritative only through the binding's exact active
	// generation at its indexed head. Staging, failed, and missing generations
	// cannot be used to request same-head recovery.
	if _, err = pool.Exec(ctx, `UPDATE git_projected_documents SET diagnostics=$2::jsonb
		WHERE binding_id=$1 AND generation=1 AND path=$3`, bindingID,
		`[{"code":"TraefikRuntimeUnobserved","detail":"runtime stale","pointer":"/spec/routes/0"}]`, documentPath); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_projection_generations SET state='staging',activated_at=NULL
		WHERE binding_id=$1 AND generation=1`, bindingID); err != nil {
		t.Fatal(err)
	}
	stagingLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(4*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("staging claim found=%v lease=%#v err=%v", found, stagingLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, stagingLease, receipt, now.Add(4*config.PollInterval+time.Second), now.Add(5*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("ready")
	if _, err = pool.Exec(ctx, `UPDATE git_projection_generations SET state='failed',activated_at=NULL
		WHERE binding_id=$1 AND generation=1`, bindingID); err != nil {
		t.Fatal(err)
	}
	failedLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(5*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("failed claim found=%v lease=%#v err=%v", found, failedLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, failedLease, receipt, now.Add(5*config.PollInterval+time.Second), now.Add(6*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("ready")
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET projection_generation=2 WHERE id=$1`, bindingID); err != nil {
		t.Fatal(err)
	}
	missingLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(6*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("missing-generation claim found=%v lease=%#v err=%v", found, missingLease, err)
	}
	if _, err = store.RecordTargetReady(ctx, missingLease, receipt, now.Add(6*config.PollInterval+time.Second), now.Add(7*config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	assertState("ready")
	if _, err = pool.Exec(ctx, `UPDATE git_repository_bindings SET projection_generation=1 WHERE id=$1`, bindingID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE git_projection_generations SET state='active',activated_at=$2
		WHERE binding_id=$1 AND generation=1`, bindingID, now.Add(7*config.PollInterval)); err != nil {
		t.Fatal(err)
	}

	// ExternalDNS runtime recovery is scoped to the exact target integration.
	// A healthy integration A cannot wake a document that references B.
	if _, err = pool.Exec(ctx, `UPDATE git_projected_documents SET parsed=$2::jsonb,diagnostics=$3::jsonb
		WHERE binding_id=$1 AND generation=1 AND path=$4`, bindingID,
		`{"spec":{"routes":[{"dns":{"mode":"externalDns","integrationRef":"edge-wake-b"}}]}}`,
		`[{"code":"ExternalDNSRuntimeUnobserved","detail":"runtime stale","pointer":"/spec/routes/0/dns/integrationRef"}]`, documentPath); err != nil {
		t.Fatal(err)
	}
	externalConfig := testRuntimeConfig()
	profileA := externalConfig.Profiles.ExternalDNS[0]
	profileA.IntegrationID = integrationA
	profileB := profileA
	profileB.IntegrationID = integrationB
	targetA, targetErr := (TargetProfile{Kind: KindExternalDNS, ExternalDNS: &profileA}).Desired(testDigest("edge-wake-config-a"))
	if targetErr != nil {
		t.Fatal(targetErr)
	}
	targetB, targetErr := (TargetProfile{Kind: KindExternalDNS, ExternalDNS: &profileB}).Desired(testDigest("edge-wake-config-b"))
	if targetErr != nil {
		t.Fatal(targetErr)
	}
	for _, candidate := range []struct {
		target    DesiredTarget
		at        time.Time
		wantState string
	}{
		{target: targetA, at: now.Add(7*config.PollInterval + time.Second), wantState: "ready"},
		{target: targetB, at: now.Add(7*config.PollInterval + 2*time.Second), wantState: "indexing"},
	} {
		tx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if wakeErr := invalidateMatchingEdgeRuntimeDiagnostic(ctx, tx, candidate.target, candidate.at); wakeErr != nil {
			tx.Rollback(ctx) //nolint:errcheck
			t.Fatal(wakeErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		assertState(candidate.wantState)
	}
	resetReady(now.Add(7*config.PollInterval + 3*time.Second))

	retryLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, digest, now.Add(7*config.PollInterval), config.WorkLease)
	if err != nil || !found {
		t.Fatalf("retry claim found=%v lease=%#v err=%v", found, retryLease, err)
	}
	if _, err = store.RecordTargetRetry(ctx, retryLease, "kubernetes-unavailable", false,
		now.Add(8*config.PollInterval), now.Add(7*config.PollInterval+time.Second)); err != nil {
		t.Fatal(err)
	}
	assertState("indexing")
}

func migrateEdgeTestDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	return testdb.ApplyMigrations(ctx, pool)
}
