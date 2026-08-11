package certificates

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestPostgreSQLCertificateAttestationContract(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	actorID := id.New()
	scope := secrets.Scope{
		OrganizationID: id.New(), ProjectID: id.New(), EnvironmentID: id.New(), ApplicationID: id.New(),
		Namespace: "certificate-" + actorID[:8],
	}
	suffix := actorID[:8]
	if _, err = pool.Exec(ctx, "INSERT INTO users(id,login,role,issuer,subject,created_at) VALUES($1,$2,'platform-admin','test',$3,$4)",
		actorID, "certificate-"+suffix, "certificate-"+actorID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,'Certificate team',$2,$3,$4)",
		scope.OrganizationID, "certificate-"+suffix, actorID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,'Certificate project',$2,$3,$4)",
		scope.ProjectID, "certificate-"+suffix, scope.OrganizationID, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at) VALUES($1,$2,'Certificate environment','certificate-environment',$3,$4,$5)",
		scope.EnvironmentID, scope.ProjectID, scope.Namespace, "certificate-"+suffix, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Certificate app','certificate-app',$3)",
		scope.ApplicationID, scope.ProjectID, testNow); err != nil {
		t.Fatal(err)
	}
	secretStore, err := secrets.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	certificateStore, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	provider := fakeSealedProvider{}
	secretService := secrets.Service{
		Store: secretStore, Keys: staticFingerprintKeys{value: []byte("0123456789abcdef0123456789abcdef")},
		SealedSecrets: provider, Now: func() time.Time { return testNow },
	}
	service := Service{Secrets: &secretService, Catalog: secretStore, Store: certificateStore, Now: func() time.Time { return testNow }}
	certificate, key := certificatePEM(t, []string{"api.example.test", "*.apps.example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	defer clear(certificate)
	defer clear(key)
	request := func() CreateRequest {
		material, materialErr := NewMaterial(certificate, key)
		if materialErr != nil {
			t.Fatal(materialErr)
		}
		return CreateRequest{
			ActorID: actorID, Scope: scope, Name: "public-edge", IdempotencyKey: "postgres-certificate-1",
			RequestID: "postgres-certificate", Material: material,
		}
	}
	created, err := service.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Create(ctx, request())
	if err != nil || !replayed.Replay || replayed.Certificate.SecretVersionID != created.Certificate.SecretVersionID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	stored, err := certificateStore.Version(ctx, created.Version.ID)
	if err != nil || !sameVersion(stored, created.Certificate) {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}

	if _, err = pool.Exec(ctx, "UPDATE secret_bindings SET purpose='runtime-secret' WHERE id=$1", created.Binding.ID); err == nil {
		t.Fatal("certificate binding purpose was mutable")
	}
	if _, err = pool.Exec(ctx, "UPDATE secret_binding_versions SET target_secret_type='Opaque' WHERE id=$1", created.Version.ID); err == nil {
		t.Fatal("certificate target Secret type was mutable")
	}
	if _, err = pool.Exec(ctx, "UPDATE tls_certificate_versions SET dns_names='[\"other.example.test\"]'::jsonb WHERE version_id=$1", created.Version.ID); err == nil {
		t.Fatal("certificate attestation was mutable")
	}
	if _, err = pool.Exec(ctx, "DELETE FROM tls_certificate_versions WHERE version_id=$1", created.Version.ID); err == nil {
		t.Fatal("certificate attestation was deletable")
	}
	if _, err = pool.Exec(ctx, "INSERT INTO tls_certificate_versions("+
		"version_id,binding_id,version_number,secret_content_fingerprint,leaf_fingerprint,public_key_fingerprint,dns_names,ip_addresses,not_before,not_after,created_by,created_at) "+
		"VALUES($1,$2,99,$3,$4,$5,'[\"api.example.test\"]'::jsonb,'[]'::jsonb,$6,$7,$8,$9)",
		created.Version.ID, created.Binding.ID, created.Version.ContentFingerprint[:], created.Certificate.LeafFingerprint,
		created.Certificate.PublicKeyFingerprint, created.Certificate.NotBefore, created.Certificate.NotAfter, actorID, testNow); err == nil {
		t.Fatal("rebound certificate version number was accepted")
	}
	if _, err = pool.Exec(ctx, "INSERT INTO secret_bindings("+
		"id,organization_id,project_id,environment_id,application_id,target_namespace,name,provider,purpose,state,active_version,created_by,created_at,updated_at) "+
		"VALUES('10000000-0000-4000-8000-000000000099',$1,$2,$3,$4,$5,'bad-certificate','external-secrets','tls-certificate','provisioning',0,$6,$7,$7)",
		scope.OrganizationID, scope.ProjectID, scope.EnvironmentID, scope.ApplicationID, scope.Namespace, actorID, testNow); err == nil {
		t.Fatal("TLS certificate purpose accepted an External Secrets provider")
	}

	active, err := secretService.ReconcileVersion(ctx, created.Version.ID, "postgres-certificate-ready")
	if err != nil || active.Version.State != secrets.VersionActive {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	ref := Reference{BindingID: created.Binding.ID, Name: created.Binding.Name, Version: created.Version.Number}
	resolve := func(resolveScope secrets.Scope, resolveRef Reference, host string, at time.Time) (referenceCandidate, error) {
		t.Helper()
		tx, beginErr := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		return resolveReferenceMetadataTx(ctx, tx, resolveScope, resolveRef, host, at)
	}
	resolved, err := resolve(scope, ref, "api.example.test", testNow.Add(2*time.Minute))
	if err != nil || resolved.Resolved.TargetSecretName != secrets.TargetSecretName(created.Binding, created.Version.Number) ||
		resolved.Certificate.SecretVersionID != created.Version.ID {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err = resolve(scope, ref, "one.apps.example.test", testNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("one-label wildcard was rejected: %v", err)
	}
	if _, err = resolve(scope, ref, "nested.one.apps.example.test", testNow.Add(2*time.Minute)); !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("multi-label wildcard error=%v", err)
	}
	wrongScope := scope
	wrongScope.ApplicationID = id.New()
	if _, err = resolve(wrongScope, ref, "api.example.test", testNow.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-application reference error=%v", err)
	}
	wrongName := ref
	wrongName.Name = "other-certificate"
	if _, err = resolve(scope, wrongName, "api.example.test", testNow.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binding-name substitution error=%v", err)
	}
	if _, err = resolve(scope, ref, "api.example.test", created.Certificate.NotAfter); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired certificate error=%v", err)
	}

	observationConfig := DefaultObservationConfig()
	observationConfig.Enabled = true
	observationConfig.Namespaces = []string{scope.Namespace}
	identity, err := ObservationIdentityForConfig(observationConfig)
	if err != nil {
		t.Fatal(err)
	}
	observationNow := testNow.Add(3 * time.Minute)
	workerID := "certificate-observer:" + suffix
	work, err := certificateStore.ClaimCertificateObservation(ctx, identity, workerID, observationConfig.Namespaces,
		observationNow, observationConfig.WorkLease)
	if err != nil || work.Validate() != nil || work.Binding.ID != created.Binding.ID || work.SecretVersion.ID != created.Version.ID {
		t.Fatalf("observation work=%#v err=%v", work, err)
	}
	heartbeatAt := observationNow.Add(observationConfig.HeartbeatInterval)
	lease, err := certificateStore.HeartbeatCertificateObservation(ctx, work.Lease, heartbeatAt, observationConfig.WorkLease)
	if err != nil || !lease.Until.After(work.Lease.Until) {
		t.Fatalf("heartbeat lease=%#v err=%v", lease, err)
	}
	readyAt := heartbeatAt.Add(time.Second)
	err = certificateStore.ApplyCertificateObservationReady(ctx, lease, ObservationReadyOutcome{
		ObservedAt: heartbeatAt, NextAt: readyAt.Add(observationConfig.PollInterval),
	}, readyAt)
	if err != nil {
		t.Fatal(err)
	}
	workerLease, err := certificateStore.AcquireCertificateObservationReadiness(ctx, ObservationWorkerObservation{
		WorkerID: workerID, Identity: identity, StartedAt: observationNow, ObservedAt: readyAt,
	}, CertificateObservationReadinessLease)
	if err != nil || workerLease.Validate() != nil {
		t.Fatalf("worker readiness=%#v err=%v", workerLease, err)
	}
	referenceResolver, err := NewPostgreSQLReferenceResolver(certificateStore, observationConfig)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := referenceResolver.ResolveCertificateReferenceTx(ctx, tx, scope, ref, "api.example.test", readyAt.Add(time.Second))
	_ = tx.Rollback(ctx)
	if err != nil || authorized.TargetSecretName != secrets.TargetSecretName(created.Binding, created.Version.Number) {
		t.Fatalf("authorized=%#v err=%v", authorized, err)
	}

	// Per-certificate readiness and worker liveness are independent fences.
	staleAt := readyAt.Add(observationConfig.MaximumObservationAge + time.Second)
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	_, err = referenceResolver.ResolveCertificateReferenceTx(ctx, tx, scope, ref, "api.example.test", staleAt)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale observation error=%v", err)
	}

	// Durable identities and terminal observation results cannot be rewritten
	// outside the fenced lease transition.
	if _, err = pool.Exec(ctx, `UPDATE tls_certificate_observations SET target_digest=$2 WHERE version_id=$1`,
		created.Version.ID, "sha256:"+strings.Repeat("f", 64)); err == nil {
		t.Fatal("certificate observation target digest was mutable")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM tls_certificate_observation_workers WHERE worker_id=$1`, workerLease.WorkerID); err == nil {
		t.Fatal("certificate observation worker receipt was deletable")
	}
}
