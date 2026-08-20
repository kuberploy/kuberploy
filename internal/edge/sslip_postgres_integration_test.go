package edge

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLSSLIPObservationAndResolverAreFenced(t *testing.T) {
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
		projectID     = "d1111111-1111-4111-8111-111111111111"
		environmentID = "d2222222-2222-4222-8222-222222222222"
		applicationID = "d3333333-3333-4333-8333-333333333333"
		namespace     = "sslip-runtime-test"
	)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `UPDATE edge_runtime_targets SET active=false,lease_owner=NULL,lease_until=NULL,
			worker_contract=NULL,worker_config_digest=NULL WHERE target_key IN ('traefik','cert-manager')`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_sslip_ingress_observations WHERE target_key='traefik'`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_readiness WHERE runtime_kind='edge' AND scope_key='global' AND worker_id=$1`, testWorkerID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE target_key IN ('traefik','cert-manager')`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at)
		VALUES($1,'sslip project','sslip-project',NULL,$2)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production',$3,$3,$4)`, environmentID, projectID, namespace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)
		VALUES($1,$2,'API','api',$3)`, applicationID, projectID, now); err != nil {
		t.Fatal(err)
	}
	config := testRuntimeConfig()
	dynamicCertManager := config.Profiles.CertManager
	config.Profiles.CertManager = nil
	config.Profiles.ExternalDNS = nil
	config.Profiles.Traefik.SSLIP = &SSLIPProfile{Mode: SSLIPAutoFirstIP}
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
	endpoint := &SSLIPIngressEndpoint{PublicIPv4: "8.8.8.8", Source: SSLIPSourceServiceIP,
		ServiceUID: "d4444444-4444-4444-8444-444444444444", ServiceResourceVersion: "rv-sslip-1"}
	receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: testDigest("pg-sslip-identity/8.8.8.8"), ResourceVersionDigest: testDigest("pg-sslip-version/1"), SSLIP: endpoint}
	observedAt := now.Add(time.Second)
	if _, err = store.RecordTargetReady(ctx, lease, receipt, observedAt, now.Add(config.PollInterval)); err != nil {
		t.Fatal(err)
	}
	readiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 1, Contract: RuntimeContract, ConfigDigest: digest,
		TargetCount: 1, StartedAt: now, ObservedAt: observedAt, LeaseUntil: now.Add(config.ReadinessMaxAge)}
	if err = store.RecordReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewPostgreSQLSSLIPResolver(pool, config)
	if err != nil {
		t.Fatal(err)
	}
	resolver.Now = func() time.Time { return now.Add(2 * time.Second) }
	result, err := resolver.ResolveHostname(ctx, SSLIPHostnameRequest{ApplicationID: applicationID, EnvironmentID: environmentID, ProjectID: projectID, Namespace: namespace})
	if err != nil || result.Hostname != "kp-f374a6c25092167f09ce.8-8-8-8.sslip.io" || result.Source != SSLIPSourceServiceIP || !result.ObservedAt.Equal(observedAt) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	// The API keeps its operator-authored Traefik profile, while managed
	// integrations can expand the worker's global runtime after API startup.
	// The exact current Traefik observation remains usable under that admitted
	// runtime digest; the unrelated target does not gate SSLIP readiness.
	dynamicConfig := config
	dynamicConfig.Profiles.CertManager = dynamicCertManager
	dynamicDigest, err := dynamicConfig.Digest()
	if err != nil {
		t.Fatal(err)
	}
	dynamicTargets, err := dynamicConfig.DesiredTargets()
	if err != nil || len(dynamicTargets) != 2 {
		t.Fatalf("dynamic targets=%#v err=%v", dynamicTargets, err)
	}
	dynamicStartedAt := now.Add(3 * time.Second)
	if err = store.SynchronizeTargets(ctx, dynamicDigest, dynamicTargets, dynamicStartedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE edge_runtime_targets
		SET next_observation_at=CASE WHEN target_key='traefik' THEN $1::timestamptz ELSE $2::timestamptz END
		WHERE target_key IN ('traefik','cert-manager')`, dynamicStartedAt, dynamicStartedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dynamicLease, found, err := store.ClaimTarget(ctx, testWorkerID, RuntimeContract, dynamicDigest, dynamicStartedAt, dynamicConfig.WorkLease)
	if err != nil || !found || dynamicLease.Target.Key != "traefik" {
		t.Fatalf("dynamic claim=%#v found=%v err=%v", dynamicLease, found, err)
	}
	dynamicObservedAt := dynamicStartedAt.Add(time.Second)
	dynamicReceipt := ObservationReceipt{TargetKey: dynamicLease.Target.Key, DesiredDigest: dynamicLease.Target.DesiredDigest,
		IdentityDigest: testDigest("pg-sslip-identity/8.8.8.8"), ResourceVersionDigest: testDigest("pg-sslip-version/2"), SSLIP: endpoint}
	if _, err = store.RecordTargetReady(ctx, dynamicLease, dynamicReceipt, dynamicObservedAt, dynamicStartedAt.Add(dynamicConfig.PollInterval)); err != nil {
		t.Fatal(err)
	}
	dynamicReadiness := Readiness{WorkerID: testWorkerID, WorkerEpoch: 2, Contract: RuntimeContract, ConfigDigest: dynamicDigest,
		TargetCount: 2, StartedAt: dynamicStartedAt, ObservedAt: dynamicObservedAt, LeaseUntil: dynamicStartedAt.Add(dynamicConfig.ReadinessMaxAge)}
	if err = store.RecordReadiness(ctx, dynamicReadiness); err != nil {
		t.Fatal(err)
	}
	resolver.Now = func() time.Time { return dynamicObservedAt.Add(time.Second) }
	result, err = resolver.ResolveHostname(ctx, SSLIPHostnameRequest{ApplicationID: applicationID, EnvironmentID: environmentID, ProjectID: projectID, Namespace: namespace})
	if err != nil || result.Hostname != "kp-f374a6c25092167f09ce.8-8-8-8.sslip.io" || !result.ObservedAt.Equal(dynamicObservedAt) {
		t.Fatalf("dynamic runtime result=%#v err=%v", result, err)
	}
	if err = resolver.Probe(ctx); err != nil {
		t.Fatalf("dynamic runtime probe: %v", err)
	}
	if _, err = resolver.ResolveHostname(ctx, SSLIPHostnameRequest{ApplicationID: applicationID, EnvironmentID: environmentID, ProjectID: projectID, Namespace: "wrong-namespace"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope result err=%v", err)
	}
	resolver.Now = func() time.Time { return dynamicObservedAt.Add(config.ReadinessMaxAge + time.Second) }
	if _, err = resolver.ResolveHostname(ctx, SSLIPHostnameRequest{ApplicationID: applicationID, EnvironmentID: environmentID, ProjectID: projectID, Namespace: namespace}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale observation err=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE edge_sslip_ingress_observations SET public_ipv4='1.1.1.1'::inet
		WHERE target_key='traefik' AND profile_revision=$1`, targets[0].Revision); err == nil {
		t.Fatal("direct endpoint substitution was accepted")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM edge_sslip_ingress_observations
		WHERE target_key='traefik' AND profile_revision=$1`, targets[0].Revision); err == nil {
		t.Fatal("observation deletion was accepted")
	}
}
