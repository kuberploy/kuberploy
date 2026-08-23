package projectionpolicy

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestEdgeRoutePolicyRequiresExactFreshObservedProfilesPostgreSQL(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	config := edgeRouteTestConfig(t)
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_readiness WHERE runtime_kind='edge' AND scope_key='global' AND worker_id='projection-edge-test-worker'`)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE target_key IN ('traefik','cert-manager')`)
	}
	cleanup(ctx)
	defer cleanup(context.Background())
	profiles, err := config.TargetProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		desired, desiredErr := profile.Desired(digest)
		if desiredErr != nil {
			t.Fatal(desiredErr)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO edge_runtime_targets(
			target_key,profile_revision,kind,integration_id,management_mode,namespace,profile_config_map,
			external_txt_owner_id,external_policy,external_domains,desired_digest,runtime_config_digest,
			active,runtime_state,next_observation_at,last_observed_at,observed_identity_digest,
			observed_resource_versions,created_at,updated_at
		) VALUES($1,$2,$3,NULL,$4,$5,$6,'','','',$7,$8,true,'ready',$9,$10,$11,$12,$13,$10)`,
			desired.Key, desired.Revision, desired.Kind, desired.Mode, desired.Namespace, desired.ProfileConfigMap,
			desired.DesiredDigest, digest, now.Add(time.Minute), now.Add(-time.Second),
			"sha256:"+strings.Repeat("d", 64), "sha256:"+strings.Repeat("e", 64), now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('edge','global','projection-edge-test-worker',1,$1,$2,jsonb_build_object('targetCount',$3::integer),
		'{}'::jsonb,$4,$5,$6,$5)`, edge.RuntimeContract, digest, config.TargetCount(),
		now.Add(-time.Minute), now.Add(-time.Second), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	document := edgeRouteTestDocument(t, "letsencrypt", "letsencrypt-production")
	policy := &EdgeRouteReferencePolicy{Config: config}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := policy.ValidateCurrentTx(ctx, tx, document, now)
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("ready exact route diagnostics=%#v err=%v", diagnostics, err)
	}

	sslipDocument := edgeRouteSSLIPTestDocument(t, "kp-02fe19f3fda07f85d2b8.8-8-8-8.sslip.io")
	policy.SSLIP = fakeSSLIPHostnameResolver{result: edge.SSLIPHostnameResolution{
		Hostname: "kp-02fe19f3fda07f85d2b8.8-8-8-8.sslip.io", Source: edge.SSLIPSourceServiceIP, ObservedAt: now,
	}}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, sslipDocument, now)
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("exact derived sslip route diagnostics=%#v err=%v", diagnostics, err)
	}
	policy.SSLIP = fakeSSLIPHostnameResolver{result: edge.SSLIPHostnameResolution{
		Hostname: "kp-02fe19f3fda07f85d2b8.1-1-1-1.sslip.io", Source: edge.SSLIPSourceServiceIP, ObservedAt: now,
	}}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, sslipDocument, now)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "SSLIPHostnameMismatch") {
		t.Fatalf("substituted sslip hostname diagnostics=%#v err=%v", diagnostics, err)
	}
	policy.SSLIP = fakeSSLIPHostnameResolver{err: edge.ErrUnavailable}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, sslipDocument, now)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "SSLIPHostnameUnavailable") {
		t.Fatalf("unobserved sslip endpoint diagnostics=%#v err=%v", diagnostics, err)
	}
	policy.SSLIP = nil

	unapproved := edgeRouteTestDocument(t, "letsencrypt", "tenant-issuer")
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, unapproved, now)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "CertificateIssuerNotApproved") {
		t.Fatalf("unapproved issuer diagnostics=%#v err=%v", diagnostics, err)
	}
	managed := &fakeCertificateIssuerReferencePolicy{ready: true}
	policy.ManagedIssuers = managed
	policy.ManagedIssuerMaxAge = 5 * time.Minute
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, unapproved, now)
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 || len(managed.selections) != 1 || managed.selections[0] != (certissuers.Selection{Hostname: "api.example.test", IssuerName: "tenant-issuer"}) || managed.path != unapproved.Scope().Path {
		t.Fatalf("managed issuer was not resolved transactionally diagnostics=%#v policy=%#v err=%v", diagnostics, managed, err)
	}
	managed.ready = false
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, unapproved, now)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "CertificateIssuerNotReady") {
		t.Fatalf("stale managed issuer diagnostics=%#v err=%v", diagnostics, err)
	}
	managed.ready = true

	custom := edgeRouteTestDocument(t, "customCertificate", "tenant-secret")
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, custom, now)
	if err == nil && len(diagnostics) == 0 {
		err = policy.ReconcileCurrentTx(ctx, tx, custom, now)
	}
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "CustomCertificateBindingUnavailable") {
		t.Fatalf("unwired custom certificate accepted diagnostics=%#v err=%v", diagnostics, err)
	}
	certificateResolver := &fakeCertificateReferenceResolver{result: certificates.ResolvedReference{
		BindingID: "77777777-7777-4777-8777-777777777777", SecretVersionID: "88888888-8888-4888-8888-888888888888",
		Name: "tenant-secret", Version: 7, Namespace: custom.Scope().Namespace, TargetSecretName: "kp-tenant-secret-v7-0123456789",
		LeafFingerprint: "sha256:" + strings.Repeat("1", 64), PublicKeyFingerprint: "sha256:" + strings.Repeat("2", 64),
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	}}
	policy.Certificates = certificateResolver
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, custom, now)
	if err == nil && len(diagnostics) == 0 {
		err = policy.ReconcileCurrentTx(ctx, tx, custom, now)
	}
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("ready typed certificate rejected diagnostics=%#v err=%v", diagnostics, err)
	}
	if certificateResolver.reconcileDigest != "" || certificateResolver.reconcileReference != custom.Scope().Path ||
		certificateResolver.reconcileRevision != custom.Scope().SourceRevision || len(certificateResolver.reconciled) != 1 {
		t.Fatalf("direct-Git certificate guard was not reconciled from authoritative bytes: %#v", certificateResolver)
	}
	personal := custom
	personal.scope.OrganizationID = ""
	certificateResolver.reconciled = nil
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, personal, now)
	if err == nil && len(diagnostics) == 0 {
		err = policy.ReconcileCurrentTx(ctx, tx, personal, now)
	}
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 || len(certificateResolver.reconciled) != 1 {
		t.Fatalf("personal-project certificate reference was rejected diagnostics=%#v reconciled=%#v err=%v", diagnostics, certificateResolver.reconciled, err)
	}
	sharedHost := edgeRouteSharedHostCertificateDocument(t)
	certificateResolver.reconciled = nil
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, sharedHost, now)
	if err == nil && len(diagnostics) == 0 {
		err = policy.ReconcileCurrentTx(ctx, tx, sharedHost, now)
	}
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 || len(certificateResolver.reconciled) != 2 {
		t.Fatalf("same hostname/certificate on two paths was rejected diagnostics=%#v reconciled=%#v err=%v", diagnostics, certificateResolver.reconciled, err)
	}
	noRoutes := edgeRouteNoRoutesDocument(t)
	certificateResolver.reconciled = []certificates.ReferenceSelection{{Host: "stale.example.test"}}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, noRoutes, now)
	if err == nil && len(diagnostics) == 0 {
		err = policy.ReconcileCurrentTx(ctx, tx, noRoutes, now)
	}
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 || len(certificateResolver.reconciled) != 0 {
		t.Fatalf("route removal did not prune certificate guards diagnostics=%#v reconciled=%#v err=%v", diagnostics, certificateResolver.reconciled, err)
	}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.ReconcileDeletedTx(ctx, tx, custom.Scope(), now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("direct-Git deletion did not prune certificate guards: %v", err)
	}
	_ = tx.Rollback(ctx)
	if len(certificateResolver.reconciled) != 0 || certificateResolver.reconcileReference != custom.Scope().Path {
		t.Fatalf("deletion reconciled unexpected certificate selections: %#v", certificateResolver)
	}
	policy.Certificates = &fakeCertificateReferenceResolver{err: certificates.ErrHostMismatch}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, custom, now)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "CustomCertificateHostMismatch") {
		t.Fatalf("certificate SAN mismatch diagnostics=%#v err=%v", diagnostics, err)
	}

	// The exact same durable rows cannot be reused after their bounded
	// observation window or worker lease expires.
	staleNow := now.Add(2 * config.ReadinessMaxAge)
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = policy.ValidateCurrentTx(ctx, tx, document, staleNow)
	_ = tx.Rollback(ctx)
	if err != nil || !diagnosticCodesContain(diagnostics, "TraefikRuntimeUnobserved") || !diagnosticCodesContain(diagnostics, "CertManagerRuntimeUnobserved") {
		t.Fatalf("stale observations accepted diagnostics=%#v err=%v", diagnostics, err)
	}
}

func TestEdgeRoutePolicyUsesActiveManagedExternalDNSProfilePostgreSQL(t *testing.T) {
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
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	identity := strings.ReplaceAll(id.New()[:8], "-", "")
	userID, teamID, projectID, environmentID, integrationID := id.New(), id.New(), id.New(), id.New(), id.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cleanup := func(cleanupContext context.Context) {
		workerID := "projection-dynamic-edge-" + identity
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_readiness WHERE runtime_kind='edge' AND scope_key='global' AND worker_id=$1`, workerID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM edge_runtime_targets WHERE target_key=$1`, "external-dns/"+integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integration_environments WHERE integration_id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM external_dns_integrations WHERE id=$1`, integrationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM teams WHERE id=$1`, teamID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	if _, err = pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin',$3, $3,1,$4)`, userID, "dynamic-edge-"+identity, "dynamic-edge-"+identity, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,$2,$3,$4,$5)`,
		teamID, "Dynamic edge "+identity, "dynamic-edge-"+identity, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,$2,$3,$4,$5)`,
		projectID, "Dynamic project "+identity, "dynamic-project-"+identity, teamID, now); err != nil {
		t.Fatal(err)
	}
	namespace := "dynamic-edge-" + identity
	if _, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
		VALUES($1,$2,'Production','production',$3,$3,$4)`, environmentID, projectID, namespace, now); err != nil {
		t.Fatal(err)
	}
	integration := domain.ExternalDNSIntegration{ID: integrationID, Slug: "dynamic-" + identity, Name: "Dynamic " + identity,
		Mode: externaldns.ModeManaged, ProviderKind: "cloudflare", TXTOwnerID: "dynamic." + identity,
		AllowedDomainSuffixes: []string{"example.test"}, SyncPolicy: externaldns.SyncPolicyUpsert,
		CredentialSecretRef: "dynamic-credential-" + identity, ProviderConfigRef: "dynamic-provider-" + identity,
		EgressConfigRef: "dynamic-egress-" + identity, EnvironmentIDs: []string{environmentID}, RuntimeRevision: 1, Lifecycle: "active"}
	suffixes, _ := json.Marshal(integration.AllowedDomainSuffixes)
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integrations(
		id,slug,name,mode,provider_kind,txt_owner_id,allowed_domain_suffixes,sync_policy,destructive_sync_confirmed,
		credential_secret_ref,provider_config_ref,egress_config_ref,created_by,created_at,updated_at,runtime_revision,lifecycle)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,false,$9,$10,$11,$12,$13,$13,$14,$15)`,
		integration.ID, integration.Slug, integration.Name, integration.Mode, integration.ProviderKind, integration.TXTOwnerID,
		suffixes, integration.SyncPolicy, integration.CredentialSecretRef, integration.ProviderConfigRef, integration.EgressConfigRef,
		userID, now, integration.RuntimeRevision, integration.Lifecycle); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO external_dns_integration_environments(integration_id,environment_id,created_at) VALUES($1,$2,$3)`, integrationID, environmentID, now); err != nil {
		t.Fatal(err)
	}

	base := edgeRouteTestConfig(t)
	operational := externaldns.OperationalConfig{Enabled: true, BindingID: id.New(), Template: externaldns.ManagedRuntimeTemplate{Namespace: "external-dns", Version: "v0.18.0",
		Image: "registry.k8s.io/external-dns/external-dns:v0.18.0", ServiceAccount: "external-dns"}, PollInterval: 30 * time.Second}
	profile, err := externaldns.ManagedProfile(integration, operational.Template)
	if err != nil {
		t.Fatal(err)
	}
	runtime := base
	runtime.Profiles.ExternalDNS = []edge.ExternalDNSProfile{profile}
	runtimeDigest, err := runtime.Digest()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := runtime.TargetProfiles()
	if err != nil {
		t.Fatal(err)
	}
	workerID := "projection-dynamic-edge-" + identity
	for _, targetProfile := range profiles {
		desired, desiredErr := targetProfile.Desired(runtimeDigest)
		if desiredErr != nil {
			t.Fatal(desiredErr)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO edge_runtime_targets(
			 target_key,profile_revision,kind,integration_id,management_mode,namespace,profile_config_map,
			external_txt_owner_id,external_policy,external_domains,external_provider_kind,external_credential_secret_ref,
			external_provider_config_ref,external_egress_config_ref,desired_digest,runtime_config_digest,
			active,runtime_state,next_observation_at,last_observed_at,observed_identity_digest,
			observed_resource_versions,created_at,updated_at)
			VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,true,'ready',$17,$18,$19,$20,$18,$18)`,
			desired.Key, desired.Revision, desired.Kind, desired.IntegrationID, desired.Mode, desired.Namespace, desired.ProfileConfigMap,
			desired.ExternalTXTOwnerID, desired.ExternalPolicy, desired.ExternalDomains, desired.ExternalProviderKind, desired.ExternalCredentialSecretRef,
			desired.ExternalProviderConfigRef, desired.ExternalEgressConfigRef, desired.DesiredDigest, desired.RuntimeConfigDigest,
			now.Add(time.Minute), now.Add(time.Second), "sha256:"+strings.Repeat("d", 64), "sha256:"+strings.Repeat("e", 64)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,
		contract_version,config_digest,identity,observation,started_at,observed_at,lease_until,updated_at)
		VALUES('edge','global',$1,1,$2,$3,jsonb_build_object('targetCount',$4::integer),'{}'::jsonb,$5,$6,$7,$6)`,
		workerID, edge.RuntimeContract, runtimeDigest, runtime.TargetCount(), now, now.Add(time.Second), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	policy := &EdgeRouteReferencePolicy{Config: base, ExternalDNSConfig: operational}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := policy.ReadyExternalDNSIntegrationTx(ctx, tx, integrationID, environmentID, now.Add(2*time.Second))
	_ = tx.Rollback(ctx)
	if err != nil || !ready {
		t.Fatalf("active managed integration was not matched to the dynamic runtime profile: ready=%v err=%v", ready, err)
	}
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := policy.ValidateCurrentTx(ctx, tx, edgeRouteExternalDNSTestDocument(t, integration.Slug), now.Add(2*time.Second))
	_ = tx.Rollback(ctx)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("dynamic edge digest rejected a valid route: diagnostics=%#v err=%v", diagnostics, err)
	}
}

func TestRuntimePolicyDigestFencesExactEdgeProfiles(t *testing.T) {
	disabled, err := RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, edge.RuntimeConfig{}, externaldns.OperationalConfig{})
	if err != nil || !strings.HasPrefix(disabled, "sha256:") {
		t.Fatalf("disabled digest=%q err=%v", disabled, err)
	}
	config := edgeRouteTestConfig(t)
	first, err := RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, config, externaldns.OperationalConfig{})
	if err != nil {
		t.Fatal(err)
	}
	config.Profiles.CertManager.Revision++
	second, err := RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, config, externaldns.OperationalConfig{})
	if err != nil || first == second || first == disabled {
		t.Fatalf("edge digest did not fence profile revision: disabled=%q first=%q second=%q err=%v", disabled, first, second, err)
	}
	dormant := edge.DefaultRuntimeConfig()
	if _, err = RuntimePolicyDigest(secrets.RuntimeConfig{}, certificates.ObservationConfig{}, certissuers.ObserverConfig{}, imagepull.RuntimeConfig{}, dormant, externaldns.OperationalConfig{}); err == nil {
		t.Fatal("disabled edge config with dormant timing fields was accepted")
	}
}

func edgeRouteTestDocument(t *testing.T, tlsMode, reference string) AppConfigPolicyDocument {
	t.Helper()
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	spec["middlewares"] = []any{map[string]any{"name": "secure", "spec": map[string]any{"compress": map[string]any{}}}}
	tls := map[string]any{"mode": tlsMode}
	if tlsMode == "letsencrypt" {
		tls["issuerRef"] = reference
	} else if tlsMode == "customCertificate" {
		tls["secretRef"] = map[string]any{
			"bindingId": "77777777-7777-4777-8777-777777777777",
			"name":      reference,
			"version":   json.Number("7"),
		}
	}
	spec["routes"] = []any{map[string]any{
		"id": "public", "host": "api.example.test", "path": "/", "port": "http", "ingressClassName": "traefik",
		"middlewareRefs": []any{"secure"}, "dns": map[string]any{"mode": "manual"}, "tls": tls,
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func edgeRouteExternalDNSTestDocument(t *testing.T, integrationRef string) AppConfigPolicyDocument {
	t.Helper()
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	spec["routes"] = []any{map[string]any{
		"id": "public", "host": "api.example.test", "path": "/", "port": "http", "ingressClassName": "traefik",
		"middlewareRefs": []any{}, "dns": map[string]any{"mode": "externalDns", "integrationRef": integrationRef}, "tls": map[string]any{"mode": "httpOnly"},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func edgeRouteSSLIPTestDocument(t *testing.T, hostname string) AppConfigPolicyDocument {
	t.Helper()
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	spec["routes"] = []any{map[string]any{
		"id": "temporary", "host": hostname, "path": "/", "port": "http", "ingressClassName": "traefik",
		"middlewareRefs": []any{}, "dns": map[string]any{"mode": "sslip"}, "tls": map[string]any{"mode": "httpOnly"},
	}}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func edgeRouteSharedHostCertificateDocument(t *testing.T) AppConfigPolicyDocument {
	t.Helper()
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	spec := parsed["spec"].(map[string]any)
	tls := map[string]any{"mode": "customCertificate", "secretRef": map[string]any{
		"bindingId": "77777777-7777-4777-8777-777777777777", "name": "tenant-secret", "version": json.Number("7"),
	}}
	spec["routes"] = []any{
		map[string]any{"id": "root", "host": "api.example.test", "path": "/", "port": "http", "ingressClassName": "traefik", "middlewareRefs": []any{}, "dns": map[string]any{"mode": "manual"}, "tls": tls},
		map[string]any{"id": "health", "host": "api.example.test", "path": "/health", "port": "http", "ingressClassName": "traefik", "middlewareRefs": []any{}, "dns": map[string]any{"mode": "manual"}, "tls": tls},
	}
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func edgeRouteNoRoutesDocument(t *testing.T) AppConfigPolicyDocument {
	t.Helper()
	scope := runtimeSecretDocumentScope(t, "apps-production")
	runtime := domain.NormalizeWorkloadRuntime(domain.DefaultWorkloadRuntime(8080, nil))
	parsed := privatePolicyParsed("66666666-6666-4666-8666-666666666666", "managed-main", 7)
	delete(parsed["spec"].(map[string]any), "routes")
	document, err := newAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func edgeRouteTestConfig(t *testing.T) edge.RuntimeConfig {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	objects := func(names []string) []edge.ObjectExpectation {
		values := make([]edge.ObjectExpectation, len(names))
		for index, name := range names {
			values[index] = edge.ObjectExpectation{Name: name, SpecDigest: digest}
		}
		return values
	}
	traefikCRDs := slices.Clone(edge.RequiredTraefikCRDs)
	certCRDs := slices.Clone(edge.RequiredCertManagerCRDs)
	slices.Sort(traefikCRDs)
	slices.Sort(certCRDs)
	config := edge.DefaultRuntimeConfig()
	config.Enabled = true
	config.Profiles.Traefik = &edge.TraefikProfile{
		Revision: 1, Mode: edge.ModeManaged, Namespace: "kuberploy-edge", Version: "v3.5.0",
		Deployment: edge.DeploymentExpectation{Name: "traefik", ContainerName: "traefik", Image: "docker.io/library/traefik@" + digest, SpecDigest: digest},
		Service:    edge.ObjectExpectation{Name: "traefik", SpecDigest: digest}, IngressClass: edge.ObjectExpectation{Name: "traefik", SpecDigest: digest},
		CRDs: objects(traefikCRDs), ProfileConfigMap: "kuberploy-edge-profile",
		SSLIP: &edge.SSLIPProfile{Mode: edge.SSLIPAutoFirstIP},
	}
	config.Profiles.CertManager = &edge.CertManagerProfile{
		Revision: 1, Mode: edge.ModeManaged, Namespace: "kuberploy-cert-manager", Version: "v1.21.1",
		Deployments: []edge.DeploymentExpectation{
			{Name: "cert-manager", ContainerName: "cert-manager-controller", Image: "quay.io/jetstack/cert-manager-controller@" + digest, SpecDigest: digest},
			{Name: "cert-manager-cainjector", ContainerName: "cert-manager-cainjector", Image: "quay.io/jetstack/cert-manager-cainjector@" + digest, SpecDigest: digest},
			{Name: "cert-manager-webhook", ContainerName: "cert-manager-webhook", Image: "quay.io/jetstack/cert-manager-webhook@" + digest, SpecDigest: digest},
		},
		CRDs: objects(certCRDs), ProfileConfigMap: "kuberploy-cert-manager-profile", IngressClassName: "traefik",
		ProductionIssuer: "letsencrypt-production", ProductionServerClass: "letsencrypt-production", ProductionSolverTypes: []string{"http01"},
		StagingIssuer: "letsencrypt-staging", StagingServerClass: "letsencrypt-staging", StagingSolverTypes: []string{"http01"},
	}
	// Assert helper data is canonical before a test can accidentally exercise a
	// weakened invalid-configuration branch.
	if config.Validate() != nil {
		encoded, _ := json.Marshal(config.Profiles)
		t.Fatalf("invalid edge test config: %s", encoded)
	}
	return config
}

func diagnosticCodesContain(diagnostics []gitprojection.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

type fakeCertificateReferenceResolver struct {
	result             certificates.ResolvedReference
	err                error
	reconciled         []certificates.ReferenceSelection
	reconcileDigest    string
	reconcileReference string
	reconcileRevision  string
}

type fakeCertificateIssuerReferencePolicy struct {
	ready       bool
	application string
	environment string
	path        string
	selections  []certissuers.Selection
	deletedPath string
}

func (f *fakeCertificateIssuerReferencePolicy) ReconcileReferencesTx(_ context.Context, _ pgx.Tx, applicationID, environmentID, path string,
	selections []certissuers.Selection, _ time.Time, _ time.Duration) (bool, error) {
	f.application, f.environment, f.path = applicationID, environmentID, path
	f.selections = append([]certissuers.Selection(nil), selections...)
	return f.ready, nil
}

func (f *fakeCertificateIssuerReferencePolicy) ReconcileDeletedTx(_ context.Context, _ pgx.Tx, path string) error {
	f.deletedPath = path
	return nil
}

type fakeSSLIPHostnameResolver struct {
	result edge.SSLIPHostnameResolution
	err    error
}

func (f fakeSSLIPHostnameResolver) ResolveHostnameTx(
	_ context.Context,
	_ pgx.Tx,
	_ edge.SSLIPHostnameRequest,
	_ time.Time,
) (edge.SSLIPHostnameResolution, error) {
	return f.result, f.err
}

func (f fakeCertificateReferenceResolver) ResolveCertificateReferenceTx(
	_ context.Context,
	_ pgx.Tx,
	_ secrets.Scope,
	_ certificates.Reference,
	_ string,
	_ time.Time,
) (certificates.ResolvedReference, error) {
	return f.result, f.err
}

func (f *fakeCertificateReferenceResolver) ReconcileGitCurrentCertificateReferencesTx(
	_ context.Context,
	_ pgx.Tx,
	_ secrets.Scope,
	selections []certificates.ReferenceSelection,
	expectedDigest, referenceID, revision string,
	_ time.Time,
) error {
	f.reconciled = append([]certificates.ReferenceSelection(nil), selections...)
	f.reconcileDigest, f.reconcileReference, f.reconcileRevision = expectedDigest, referenceID, revision
	return f.err
}
