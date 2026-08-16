package helmapps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"
	"github.com/kuberploy/kuberploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"go.yaml.in/yaml/v3"
)

func helmPGArgoAuthority() ArgoMaterializationAuthority {
	return ArgoMaterializationAuthority{PolicyDigest: helmPGDigest([]byte("argo-materialization-policy")),
		Runtime: argo.RuntimeLock{ChartRepository: "oci://registry.example.com/kuberploy/runtime",
			ChartName: argo.RuntimeChartName, ChartVersion: "1.2.3",
			ChartDigest:   helmPGDigest([]byte("runtime-chart")),
			RendererImage: "registry.example.com/kuberploy/runtime@" + helmPGDigest([]byte("renderer"))},
		DigestEnforcement: argo.ChartDigestNativeOCI}
}

func helmPGArgoObservation(t *testing.T, f helmReleasePGFixture, workerID string,
	startedAt time.Time,
) (argo.DesiredStateRuntimeIdentity, argo.DesiredStateRuntimeWorkerObservation) {
	t.Helper()
	startedAt = startedAt.UTC().Truncate(time.Microsecond)
	repositorySecretName, err := argo.RepositoryCredentialName(f.platformBindingID)
	if err != nil {
		t.Fatal(err)
	}
	config := argo.DesiredStateRuntimeConfig{Enabled: true, GitHubAppID: 1,
		PlatformBindingID: f.platformBindingID, ClusterID: f.clusterID, ArgoNamespace: "argocd",
		RootApplicationName: "kuberploy-platform-root", RepositorySecretName: repositorySecretName,
		Runtime: helmPGArgoAuthority().Runtime, DigestEnforcement: argo.ChartDigestNativeOCI}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return identity, argo.DesiredStateRuntimeWorkerObservation{
		WorkerID: workerID, DesiredStateRuntimeIdentity: identity,
		StartedAt: startedAt, ObservedAt: startedAt,
	}
}

type helmReleasePGFixture struct {
	userID, projectID, environmentID, applicationID                string
	approvalID, platformBindingID, environmentBindingID, clusterID string
	foundationIntentID, desiredStateCommandID                      string
	namespace, argoProject                                         string
	ociRepository                                                  string
	platformHead, environmentHead, catalogDigest, publisherDigest  string
	foundationRevision, desiredStateRevision                       string
	values, schema, manifest                                       []byte
	valuesDigest, schemaDigest, documentsDigest                    string
	manifestDigest, inventoryDigest                                string
	now                                                            time.Time
}

func TestPostgresHelmReleaseTwoPhasePublicationContract(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool := openFreshHelmMigrationPool(t, ctx, databaseURL)
	var err error
	// This low-level legacy CRUD/CAS contract manually drives phase two. Keep
	// it on schema 010; schema 011's required live cascade observation is
	// exercised by TestPostgresProtectedPublicationStoreDisableLifecycle.
	if err = applyHelmMigrationsThrough(ctx, pool, "010_helm_application_materialization_bridge"); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	f := newHelmReleasePGFixture()
	setupHelmReleasePGFixture(t, ctx, tx, f)
	testPostgresReleaseServiceTransaction(t, ctx, tx, f.now.Add(time.Hour))

	releaseOne, commandOne := id.New(), id.New()
	insertHelmRenderCommand(t, ctx, tx, f, commandOne, f.values, f.valuesDigest, f.now)
	insertHelmRelease(t, ctx, tx, f, helmReleaseInsert{
		id: releaseOne, generation: 1, action: "initial", commandID: commandOne,
		values: f.values, valuesDigest: f.valuesDigest,
	}, f.now)
	if _, err = tx.Exec(ctx, `INSERT INTO helm_release_heads(
		project_id,environment_id,application_id,revision_id,generation,updated_at
	) VALUES($1,$2,$3,$4,1,$5)`, f.projectID, f.environmentID, f.applicationID, releaseOne, f.now); err != nil {
		t.Fatal(err)
	}

	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_release_revisions
			SET release_name='caller-controlled' WHERE id=$1`, releaseOne)
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `DELETE FROM helm_release_revisions WHERE id=$1`, releaseOne)
		return nestedErr
	})

	completeHelmRender(t, ctx, tx, f, commandOne, f.now.Add(time.Second))
	payloadOne := id.New()
	payloadOnePath := helmPayloadPath(f, releaseOne, false)
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmPayload(ctx, nested, f, helmPayloadInsert{
			id: payloadOne, releaseID: releaseOne, generation: 1, action: "publish",
			path: payloadOnePath + ".caller", content: f.manifest,
			contentDigest: f.manifestDigest, inventoryDigest: f.inventoryDigest,
			resourceCount: 1,
		}, f.now.Add(2*time.Second))
	})
	if err = insertHelmPayload(ctx, tx, f, helmPayloadInsert{
		id: payloadOne, releaseID: releaseOne, generation: 1, action: "publish",
		path: payloadOnePath, content: f.manifest, contentDigest: f.manifestDigest,
		inventoryDigest: f.inventoryDigest, resourceCount: 1,
	}, f.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_payload_intents
			SET content=$2 WHERE id=$1`, payloadOne, []byte("mutated\n"))
		return nestedErr
	})

	applicationOne, applicationOneDigest := id.New(), helmPGDigest([]byte("application-one\n"))
	payloadOneCommit := strings.Repeat("b", 40)
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmApplication(ctx, nested, f, helmApplicationInsert{
			id: applicationOne, releaseID: releaseOne, payloadID: payloadOne,
			generation: 1, action: "publish", payloadRevision: payloadOneCommit,
			payloadPath: payloadOnePath, sourceDirectory: helmSourceDirectory(f, releaseOne),
			applicationPath: helmApplicationPath(f), operation: "create",
			precondition: "create-if-absent", content: []byte("application-one\n"),
			contentDigest: applicationOneDigest,
		}, f.now.Add(3*time.Second))
	})

	claimHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadOne, f.now.Add(3*time.Second))
	commitHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadOne,
		f.platformHead, payloadOneCommit, f.now.Add(4*time.Second))
	verifyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadOne,
		f.manifestDigest, f.now.Add(5*time.Second))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_payload_intents
			SET updated_at=updated_at+interval '1 second' WHERE id=$1`, payloadOne)
		return nestedErr
	})

	advancePlatformHead(t, ctx, tx, f, payloadOneCommit, f.now.Add(6*time.Second))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmApplication(ctx, nested, f, helmApplicationInsert{
			id: applicationOne, releaseID: releaseOne, payloadID: payloadOne,
			generation: 1, action: "publish", payloadRevision: strings.Repeat("9", 40),
			payloadPath: payloadOnePath, sourceDirectory: helmSourceDirectory(f, releaseOne),
			applicationPath: helmApplicationPath(f), operation: "create",
			precondition: "create-if-absent", content: []byte("application-one\n"),
			contentDigest: applicationOneDigest,
		}, f.now.Add(7*time.Second))
	})
	if err = insertHelmApplication(ctx, tx, f, helmApplicationInsert{
		id: applicationOne, releaseID: releaseOne, payloadID: payloadOne,
		generation: 1, action: "publish", payloadRevision: payloadOneCommit,
		payloadPath: payloadOnePath, sourceDirectory: helmSourceDirectory(f, releaseOne),
		applicationPath: helmApplicationPath(f), operation: "create",
		precondition: "create-if-absent", content: []byte("application-one\n"),
		contentDigest: applicationOneDigest,
	}, f.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	// A new desired head cannot race a stable-path write that might become
	// visible under Argo's platform root.
	commandBlocked := id.New()
	insertHelmRenderCommand(t, ctx, tx, f, commandBlocked, f.values, f.valuesDigest, f.now.Add(8*time.Second))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmReleaseErr(ctx, nested, f, helmReleaseInsert{
			id: id.New(), generation: 2, action: "retry", parentID: releaseOne,
			commandID: commandBlocked, values: f.values, valuesDigest: f.valuesDigest,
		}, f.now.Add(8*time.Second))
	})

	applicationOneCommit := strings.Repeat("c", 40)
	claimHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationOne, f.now.Add(8*time.Second))
	commitHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationOne,
		payloadOneCommit, applicationOneCommit, f.now.Add(9*time.Second))
	verifyHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationOne,
		applicationOneDigest, f.now.Add(10*time.Second))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_application_intents
			SET provider_request='mutated' WHERE id=$1`, applicationOne)
		return nestedErr
	})

	advancePlatformHead(t, ctx, tx, f, applicationOneCommit, f.now.Add(11*time.Second))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmReleaseErr(ctx, nested, f, helmReleaseInsert{
			id: id.New(), generation: 2, action: "update", parentID: releaseOne,
			baseID: applicationOne, commandID: commandBlocked, releaseName: "wrong",
			values: f.values, valuesDigest: f.valuesDigest,
		}, f.now.Add(12*time.Second))
	})

	releaseTwo := id.New()
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmReleaseErr(ctx, nested, f, helmReleaseInsert{
			id: releaseTwo, generation: 2, action: "disable", parentID: releaseOne,
			values: f.values, valuesDigest: f.valuesDigest,
		}, f.now.Add(12*time.Second))
	})
	insertHelmRelease(t, ctx, tx, f, helmReleaseInsert{
		id: releaseTwo, generation: 2, action: "disable", parentID: releaseOne,
		baseID: applicationOne, values: f.values, valuesDigest: f.valuesDigest,
	}, f.now.Add(12*time.Second))
	if _, err = tx.Exec(ctx, `UPDATE helm_release_heads SET
		revision_id=$3,generation=2,updated_at=$4
		WHERE environment_id=$1 AND application_id=$2`,
		f.environmentID, f.applicationID, releaseTwo, f.now.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}

	payloadTwo := id.New()
	payloadTwoPath := helmPayloadPath(f, releaseTwo, true)
	disabledReceipt := []byte(fmt.Sprintf(
		`{"apiVersion":"kuberploy.io/v1alpha1","kind":"HelmReleaseDisabledReceipt","releaseRevisionId":%q,"generation":2,"projectId":%q,"environmentId":%q,"applicationId":%q}`,
		releaseTwo, f.projectID, f.environmentID, f.applicationID))
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmPayload(ctx, nested, f, helmPayloadInsert{
			id: payloadTwo, releaseID: releaseTwo, generation: 2,
			action: "disable-receipt", path: payloadTwoPath,
			content:       []byte(`{"kind":"caller-controlled"}`),
			contentDigest: helmPGDigest([]byte(`{"kind":"caller-controlled"}`)),
		}, f.now.Add(13*time.Second))
	})
	disabledDigest := helmPGDigest(disabledReceipt)
	if err = insertHelmPayload(ctx, tx, f, helmPayloadInsert{
		id: payloadTwo, releaseID: releaseTwo, generation: 2,
		action: "disable-receipt", path: payloadTwoPath,
		content: disabledReceipt, contentDigest: disabledDigest,
	}, f.now.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	payloadTwoCommit := strings.Repeat("d", 40)
	claimHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadTwo, f.now.Add(14*time.Second))
	commitHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadTwo,
		applicationOneCommit, payloadTwoCommit, f.now.Add(15*time.Second))
	verifyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", payloadTwo,
		disabledDigest, f.now.Add(16*time.Second))
	advancePlatformHead(t, ctx, tx, f, payloadTwoCommit, f.now.Add(17*time.Second))

	applicationTwo := id.New()
	if err = insertHelmApplication(ctx, tx, f, helmApplicationInsert{
		id: applicationTwo, releaseID: releaseTwo, payloadID: payloadTwo,
		generation: 2, action: "delete", payloadRevision: payloadTwoCommit,
		payloadPath: payloadTwoPath, applicationPath: helmApplicationPath(f),
		operation: "delete", precondition: "match-etag",
		expectedETag: `"` + applicationOneDigest + `"`,
	}, f.now.Add(18*time.Second)); err != nil {
		t.Fatal(err)
	}
	applicationTwoCommit := strings.Repeat("e", 40)
	claimHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationTwo, f.now.Add(19*time.Second))
	commitHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationTwo,
		payloadTwoCommit, applicationTwoCommit, f.now.Add(20*time.Second))
	verifyHelmIntent(t, ctx, tx, "helm_protected_application_intents", applicationTwo,
		"", f.now.Add(21*time.Second))
	advancePlatformHead(t, ctx, tx, f, applicationTwoCommit, f.now.Add(22*time.Second))

	// Rollback is a new desired/render/Git intent. After a verified delete its
	// stable-path before-image is correctly absent, so it must create again.
	releaseThree, commandThree := id.New(), id.New()
	insertHelmRenderCommand(t, ctx, tx, f, commandThree, f.values, f.valuesDigest, f.now.Add(23*time.Second))
	insertHelmRelease(t, ctx, tx, f, helmReleaseInsert{
		id: releaseThree, generation: 3, action: "rollback", parentID: releaseTwo,
		rollbackID: releaseOne, commandID: commandThree,
		values: f.values, valuesDigest: f.valuesDigest,
	}, f.now.Add(23*time.Second))
	if _, err = tx.Exec(ctx, `UPDATE helm_release_heads SET
		revision_id=$3,generation=3,updated_at=$4
		WHERE environment_id=$1 AND application_id=$2`,
		f.environmentID, f.applicationID, releaseThree, f.now.Add(23*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresProtectedPublicationStoreLifecycle(t *testing.T) {
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
	assertProtectedIntentValidatorsAreHardened(t, ctx, pool)
	f := newHelmReleasePGFixture()
	registerHelmAuthorityCleanup(t, pool, f.platformBindingID,
		"helm-lifecycle-worker-", "argo-desired-state-lifecycle-")
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, f)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	releases, err := NewPostgresReleaseService(pool, helmPGOperatorDigest())
	if err != nil {
		t.Fatal(err)
	}
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID}
	release, replay, err := releases.Upsert(ctx, UpsertReleaseRequest{
		Target: target, Approval: ApprovalKey{ID: f.approvalID, Revision: 1}, ValuesYAML: f.values,
		Actor: ReleaseActor{ID: f.userID, IdempotencyKey: "protected-release-" + id.New(), RequestID: "protected-release"},
	}, f.now.Add(time.Second))
	if err != nil || replay || release.Action != ReleaseInitial {
		t.Fatalf("release=%+v replay=%v err=%v", release, replay, err)
	}
	renderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, renderTx, f, release.RenderCommandID, f.now.Add(2*time.Second))
	if err = renderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	readinessAt := helmPGDatabaseNow(t, ctx, pool)
	_, argoObservation := helmPGArgoObservation(t, f, "argo-desired-state-lifecycle-0001",
		readinessAt.Add(-time.Second))
	store, err := NewPostgresProtectedPublicationStoreWithCascade(pool, helmPGArgoAuthority(), argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.NextPayloadCandidate(ctx)
	if err != nil || candidate.Kind != PublicationPayload || candidate.ReleaseRevisionID != release.ID || candidate.Target != target {
		t.Fatalf("payload candidate=%+v err=%v", candidate, err)
	}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest}
	publisherWorker := "helm-lifecycle-worker-0001"
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{WorkerID: publisherWorker,
		WorkerEpoch: 1, Publisher: publisher, StartedAt: readinessAt.Add(-time.Second),
		ObservedAt: readinessAt, LeaseUntil: readinessAt.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	argoObservation.ObservedAt = readinessAt
	argoLease, err := argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, publisherWorker, 1, publisher, readinessAt); err != nil {
		t.Fatal(err)
	}
	binding := ProtectedBindingSnapshot{
		PlatformBindingID: f.platformBindingID, EnvironmentBindingID: f.environmentBindingID,
		ClusterID: f.clusterID, PlatformTargetRef: "refs/heads/main",
		EnvironmentTargetRef: "refs/heads/main", EnvironmentRevision: f.environmentHead,
		EnvironmentGeneration: 1, CatalogDigest: f.catalogDigest,
		PlannedBaseRevision: f.platformHead,
	}
	payloadID := id.New()
	payload, replay, err := store.CreatePayloadForHead(ctx, payloadID, target, binding,
		publisher, f.now.Add(4*time.Second))
	if err != nil || replay || payload.State != ProtectedPending || payload.ContentDigest != f.manifestDigest {
		t.Fatalf("payload=%+v replay=%v err=%v", payload, replay, err)
	}
	if candidate, candidateErr := store.NextPayloadCandidate(ctx); !errors.Is(candidateErr, ErrNotFound) {
		t.Fatalf("planned payload remained a candidate: %+v err=%v", candidate, candidateErr)
	}
	replayed, replay, err := store.CreatePayloadForHead(ctx, payloadID, target, binding,
		publisher, f.now.Add(4*time.Second))
	if err != nil || !replay || replayed.ID != payload.ID {
		t.Fatalf("payload replay=%+v replay=%v err=%v", replayed, replay, err)
	}
	if _, _, err = store.CreatePayloadForHead(ctx, id.New(), target, binding, publisher,
		f.now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting payload identity was accepted: %v", err)
	}
	wrongPublisher := publisher
	wrongPublisher.ConfigDigest = helmPGDigest([]byte("wrong-publisher"))
	if _, _, err = store.ClaimPayload(ctx, publisherWorker, wrongPublisher,
		f.now.Add(5*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong publisher claimed work: %v", err)
	}
	payload, firstLease, err := store.ClaimPayload(ctx, publisherWorker, publisher,
		f.now.Add(5*time.Second), time.Minute)
	if err != nil || payload.State != ProtectedClaimed || firstLease.Epoch != 1 {
		t.Fatalf("claimed payload=%+v lease=%+v err=%v", payload, firstLease, err)
	}
	if _, _, err = store.ClaimPayload(ctx, "helm-publisher-worker-0002", publisher,
		f.now.Add(6*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active platform lane was double-claimed: %v", err)
	}
	retryAt := f.now.Add(20 * time.Second)
	payload, err = store.RetryPayload(ctx, firstLease, "provider-unavailable", retryAt,
		f.now.Add(7*time.Second))
	if err != nil || payload.State != ProtectedPending || payload.LeaseOwner != "" {
		t.Fatalf("retried payload=%+v err=%v", payload, err)
	}
	if _, _, err = store.ClaimPayload(ctx, "helm-publisher-worker-0002", publisher,
		retryAt.Add(-time.Microsecond), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("payload ignored retry deadline: %v", err)
	}
	payload, lease, err := store.ClaimPayload(ctx, publisherWorker, publisher,
		retryAt, time.Minute)
	if err != nil || lease.Epoch != 2 || payload.Attempts != 2 {
		t.Fatalf("reclaimed payload=%+v lease=%+v err=%v", payload, lease, err)
	}
	observedAt := retryAt.Add(time.Second)
	payload, err = store.BindPayloadWriteBase(ctx, lease, f.platformHead, observedAt, observedAt)
	if err != nil || payload.WriteBaseRevision != f.platformHead {
		t.Fatalf("bound payload=%+v err=%v", payload, err)
	}
	payloadRebind := strings.Repeat("d", 40)
	payload, err = store.RebindPayloadWriteBase(ctx, lease, f.platformHead, payloadRebind,
		observedAt.Add(time.Second), observedAt.Add(time.Second))
	if err != nil || payload.WriteBaseRevision != payloadRebind {
		t.Fatalf("rebound payload=%+v err=%v", payload, err)
	}
	stalePayloadLease := lease
	stalePayloadLease.Epoch--
	if _, err = store.RebindPayloadWriteBase(ctx, stalePayloadLease, payloadRebind,
		strings.Repeat("e", 40), observedAt.Add(2*time.Second),
		observedAt.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale payload lease rebound protected authority: %v", err)
	}
	if _, err = store.RebindPayloadWriteBase(ctx, lease, f.platformHead,
		strings.Repeat("e", 40), observedAt.Add(2*time.Second),
		observedAt.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong payload base rebound protected authority: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE helm_protected_payload_intents
		SET content=$2 WHERE id=$1`, payload.ID, []byte("identity mutation\n")); err == nil {
		t.Fatal("payload identity mutation bypassed the migration trigger")
	}
	payloadCommit := strings.Repeat("b", 40)
	payload, err = store.MarkPayloadCommitted(ctx, lease, payloadCommit, payloadRebind,
		observedAt.Add(2*time.Second))
	if err != nil || payload.State != ProtectedGitCommitted {
		t.Fatalf("committed payload=%+v err=%v", payload, err)
	}
	if _, err = store.RebindPayloadWriteBase(ctx, lease, payloadRebind,
		strings.Repeat("e", 40), observedAt.Add(3*time.Second),
		observedAt.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed payload write base was mutable: %v", err)
	}
	if _, err = store.VerifyPayload(ctx, lease, payloadCommit, helmPGDigest([]byte("wrong")),
		"provider-wrong-path", observedAt.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong provider path digest was accepted: %v", err)
	}
	payload, err = store.VerifyPayload(ctx, lease, payloadCommit, payload.ContentDigest,
		"provider-payload-verified", observedAt.Add(2*time.Second))
	if err != nil || payload.State != ProtectedVerified {
		t.Fatalf("verified payload=%+v err=%v", payload, err)
	}
	candidate, err = store.NextApplicationCandidate(ctx, publisher)
	if err != nil || candidate.Kind != PublicationApplication || candidate.ReleaseRevisionID != release.ID ||
		candidate.PayloadIntentID != payload.ID || candidate.Target != target {
		t.Fatalf("application candidate=%+v err=%v", candidate, err)
	}
	advanceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, payloadCommit, observedAt.Add(3*time.Second))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate an RC160 upgrade that already completed phase one before the
	// immutable prerequisite receipt existed. Phase two must reconstruct only
	// exact current authority, then proceed under the new insertion fence.
	legacyTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE helm_protected_payload_intents DISABLE TRIGGER helm_protected_payload_intents_validate`,
		`ALTER TABLE helm_protected_payload_intents DISABLE TRIGGER helm_protected_payload_prerequisite_receipt`,
	} {
		if err == nil {
			_, err = legacyTx.Exec(ctx, statement)
		}
	}
	if err == nil {
		_, err = legacyTx.Exec(ctx, `UPDATE helm_protected_payload_intents SET prerequisite_receipt_id=NULL,
			prerequisite_contract='',prerequisite_epoch=0 WHERE id=$1`, payload.ID)
	}
	for _, statement := range []string{
		`ALTER TABLE helm_protected_payload_intents ENABLE TRIGGER helm_protected_payload_prerequisite_receipt`,
		`ALTER TABLE helm_protected_payload_intents ENABLE TRIGGER helm_protected_payload_intents_validate`,
	} {
		if err == nil {
			_, err = legacyTx.Exec(ctx, statement)
		}
	}
	if err == nil {
		_, err = legacyTx.Exec(ctx, `ALTER TABLE helm_publication_prerequisite_receipts
			DISABLE TRIGGER helm_publication_prerequisite_receipts_validate`)
	}
	if err == nil {
		_, err = legacyTx.Exec(ctx, `DELETE FROM helm_publication_prerequisite_receipts
			WHERE release_revision_id=$1`, release.ID)
	}
	if enableResult, enableErr := legacyTx.Exec(ctx, `ALTER TABLE helm_publication_prerequisite_receipts
		ENABLE TRIGGER helm_publication_prerequisite_receipts_validate`); enableErr != nil && err == nil {
		_ = enableResult
		err = enableErr
	}
	if err != nil {
		_ = legacyTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = legacyTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Recreate the immutable phase-one receipt, then advance the environment to
	// a different verified current AppProject command and runtime. Phase two
	// must bind that current canonical authority without changing payload identity.
	sourceReceiptTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sourceBinding := payload.Binding
	sourceBinding.PlannedBaseRevision = payloadCommit
	if _, err = ensurePublicationPrerequisite(ctx, sourceReceiptTx, release, sourceBinding,
		helmPGArgoAuthority(), observedAt.Add(3*time.Second)); err != nil {
		_ = sourceReceiptTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = sourceReceiptTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	currentAuthority := helmPGArgoAuthority()
	currentAuthority.PolicyDigest = helmPGDigest([]byte("argo-materialization-policy-current"))
	currentAuthority.Runtime.ChartVersion = "1.2.4"
	currentAuthority.Runtime.ChartDigest = helmPGDigest([]byte("runtime-chart-current"))
	currentAuthority.Runtime.RendererImage = "registry.example.com/kuberploy/runtime@" +
		helmPGDigest([]byte("renderer-current"))
	currentEnvironmentRevision, currentDesiredRevision := strings.Repeat("2", 40), strings.Repeat("3", 40)
	currentCommandID := id.New()
	currentProject, renderErr := argo.RenderAppProjectAuthority(argo.AppProjectAuthority{
		ProjectID: f.projectID, EnvironmentID: f.environmentID,
		EnvironmentBindingID: f.environmentBindingID, Namespace: f.namespace,
		ArgoProject: f.argoProject, ArgoNamespace: "argocd",
		EnvironmentRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 102, Owner: "kuberploy", Name: "environment"},
		PlatformRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 101, Owner: "kuberploy", Name: "platform"}, Runtime: currentAuthority.Runtime,
	})
	if renderErr != nil {
		t.Fatal(renderErr)
	}
	currentBundle := append(append([]byte(nil), currentProject...),
		[]byte("---\napiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: current\n")...)
	currentTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE git_projection_generations SET state='failed',activated_at=NULL WHERE binding_id=$1 AND state='active'`, []any{f.environmentBindingID}},
		{`INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
			VALUES($1,2,$2,'gitprojection.v1','active',$3,$3)`, []any{f.environmentBindingID, currentEnvironmentRevision, observedAt.Add(3 * time.Second)}},
		{`UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,
			projection_generation=2,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
			[]any{f.environmentBindingID, currentEnvironmentRevision, observedAt.Add(3 * time.Second)}},
	} {
		if _, err = currentTx.Exec(ctx, statement.query, statement.args...); err != nil {
			_ = currentTx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if _, err = currentTx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		_ = currentTx.Rollback(ctx)
		t.Fatal(err)
	}
	_, err = currentTx.Exec(ctx, `INSERT INTO argo_desired_state_commands(
		id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,
		cluster_id,platform_target_ref,environment_target_ref,environment_revision,
		environment_generation,path,argo_namespace,destination_namespace,argo_project,
		base_revision,write_base_revision,write_base_observed_at,precondition,expected_etag,
		policy_digest,catalog_digest,chart_repository,chart_name,chart_version,chart_digest,
		renderer_image,chart_digest_enforcement,app_project_content,content,content_sha256,
		message,state,committed_revision,committed_at,verified_at,next_attempt_at,
		created_at,updated_at,completed_at
	) VALUES($1,2,$2,$3,$4,$5,$6,'refs/heads/main','refs/heads/main',$7,2,$8,
		'argocd',$9,$10,$11,$11,$12,'match-etag',$13,$14,$15,$16,'kuberploy-runtime',$17,$18,
		$19,'native-oci-digest-v1',$20,$21,$22,'current canonical AppProject','verified',$23,
		$12,$12,$12,$12,$12,$12)`, currentCommandID, f.projectID, f.environmentID,
		f.platformBindingID, f.environmentBindingID, f.clusterID, currentEnvironmentRevision,
		"clusters/"+f.clusterID+"/argocd/environments/"+f.environmentID+".yaml", f.namespace,
		f.argoProject, payloadCommit, observedAt.Add(3*time.Second), `"`+helmPGDigest([]byte("previous"))+`"`,
		currentAuthority.PolicyDigest, f.catalogDigest, currentAuthority.Runtime.ChartRepository,
		currentAuthority.Runtime.ChartVersion, currentAuthority.Runtime.ChartDigest,
		currentAuthority.Runtime.RendererImage, currentProject, currentBundle,
		helmPGDigest(currentBundle), currentDesiredRevision)
	if err != nil {
		_ = currentTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = currentTx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		_ = currentTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = currentTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A no-change materialization is stamped with the newly indexed projection,
	// but deliberately points at the older verified command whose exact bytes it
	// reused. Continuation authority must follow the current receipt tuple, not
	// require the referenced command's immutable origin tuple to match it.
	verifiedCurrentCommand, err := argoStore.DesiredStateCommand(ctx, currentCommandID)
	if err != nil {
		t.Fatal(err)
	}
	noChangeEnvironmentRevision := strings.Repeat("0", 40)
	noChangeProjectionAt := observedAt.Add(3500 * time.Millisecond)
	noChangeProjectionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE git_projection_generations SET state='failed',activated_at=NULL
			WHERE binding_id=$1 AND state='active'`, []any{f.environmentBindingID}},
		{`INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
			VALUES($1,3,$2,'gitprojection.v1','active',$3,$3)`,
			[]any{f.environmentBindingID, noChangeEnvironmentRevision, noChangeProjectionAt}},
		{`UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,
			projection_generation=3,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
			[]any{f.environmentBindingID, noChangeEnvironmentRevision, noChangeProjectionAt}},
	} {
		if _, err = noChangeProjectionTx.Exec(ctx, statement.query, statement.args...); err != nil {
			_ = noChangeProjectionTx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err = noChangeProjectionTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	currentMaterialization := verifiedCurrentCommand
	currentMaterialization.ID = id.New()
	currentMaterialization.Generation++
	currentMaterialization.EnvironmentRevision = noChangeEnvironmentRevision
	currentMaterialization.EnvironmentGeneration = 3
	currentMaterialization.WriteBaseRevision = ""
	currentMaterialization.WriteBaseObservedAt = nil
	currentMaterialization.State = argo.DesiredStatePending
	currentMaterialization.CommittedRevision = ""
	currentMaterialization.CommittedAt = nil
	currentMaterialization.VerifiedAt = nil
	currentMaterialization.CompletedAt = nil
	currentMaterialization.LeaseEpoch = 0
	currentMaterialization.NextAttemptAt = noChangeProjectionAt
	currentMaterialization.CreatedAt = noChangeProjectionAt
	currentMaterialization.UpdatedAt = noChangeProjectionAt
	createdMaterialization, materializationErr := argoStore.RecordDesiredStateMaterialization(ctx,
		currentMaterialization, verifiedCurrentCommand, noChangeProjectionAt)
	if materializationErr != nil || !createdMaterialization {
		t.Fatalf("current no-change materialization created=%v err=%v",
			createdMaterialization, materializationErr)
	}
	// Preserve the exact previous-release failure shape: phase two was planned against the
	// source projection, then retired pristine when the environment projection
	// advanced before its first claim. Migration 009 keeps that immutable
	// history and permits exactly one continuation-backed replacement.
	legacyApplicationID := id.New()
	legacyApplicationContent := []byte("legacy projection-superseded application\n")
	legacyApplicationTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacyApplicationTx.Exec(ctx, `ALTER TABLE helm_protected_application_intents
		DISABLE TRIGGER helm_protected_application_continuation_validate`); err == nil {
		err = insertLegacyHelmApplication(ctx, legacyApplicationTx, f, helmApplicationInsert{
			id: legacyApplicationID, releaseID: release.ID, payloadID: payload.ID,
			generation: release.Generation, action: "publish", payloadRevision: payloadCommit,
			payloadPath: payload.Path, sourceDirectory: helmSourceDirectory(f, release.ID),
			applicationPath: helmApplicationPath(f), operation: "create",
			precondition: "create-if-absent", content: legacyApplicationContent,
			contentDigest: helmPGDigest(legacyApplicationContent),
		}, observedAt.Add(3*time.Second))
	}
	if _, enableErr := legacyApplicationTx.Exec(ctx, `ALTER TABLE helm_protected_application_intents
		ENABLE TRIGGER helm_protected_application_continuation_validate`); err == nil {
		err = enableErr
	}
	if err != nil {
		_ = legacyApplicationTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = legacyApplicationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.ClaimApplication(ctx, publisherWorker, publisher,
		observedAt.Add(4*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale pristine Application was not retired before replacement: %v", err)
	}
	legacyApplication, err := store.Application(ctx, legacyApplicationID)
	if err != nil || legacyApplication.State != ProtectedSuperseded ||
		legacyApplication.LastFailureCode != "projection-superseded" ||
		legacyApplication.LeaseEpoch != 0 || legacyApplication.Attempts != 0 {
		t.Fatalf("legacy replacement predecessor=%+v err=%v", legacyApplication, err)
	}
	if candidate, candidateErr := store.NextApplicationCandidate(ctx, publisher); candidateErr != nil ||
		candidate.PayloadIntentID != payload.ID {
		t.Fatalf("pristine superseded predecessor did not reopen phase two: %+v err=%v",
			candidate, candidateErr)
	}
	currentStore, err := NewPostgresProtectedPublicationStoreWithCascade(pool, currentAuthority,
		argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	applicationID := id.New()
	application, replay, err := currentStore.CreateApplicationForPayload(ctx, applicationID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher, observedAt.Add(4*time.Second))
	if err != nil || replay || application.State != ProtectedPending ||
		application.PayloadRevision != payloadCommit || application.Operation != "create" {
		t.Fatalf("application=%+v replay=%v err=%v", application, replay, err)
	}
	continuation, continuationErr := currentStore.ApplicationContinuation(ctx, application.ID)
	if continuationErr != nil || continuation.SourceDesiredStateCommandID == continuation.CurrentDesiredStateCommandID ||
		continuation.SourceDesiredStateRevision == continuation.CurrentDesiredStateRevision ||
		continuation.CurrentEnvironmentRevision != noChangeEnvironmentRevision ||
		continuation.CurrentEnvironmentGeneration != 3 ||
		continuation.CurrentMaterializationReceiptID != currentMaterialization.ID ||
		continuation.CurrentDesiredStateCommandID != currentCommandID ||
		continuation.CurrentDesiredStateRevision != currentDesiredRevision ||
		continuation.CurrentRuntime != currentAuthority.Runtime ||
		!bytes.Equal(continuation.CurrentAppProjectContent, currentProject) {
		t.Fatalf("current continuation=%+v err=%v", continuation, continuationErr)
	}
	var continuationExact bool
	if err = pool.QueryRow(ctx, `SELECT public.helm_application_continuation_is_exact($1)`,
		application.ID).Scan(&continuationExact); err != nil || !continuationExact {
		t.Fatalf("no-change continuation exact=%v err=%v", continuationExact, err)
	}
	routeMutation, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = routeMutation.Exec(ctx, `UPDATE public.environments
		SET namespace='changed-current-route',argo_project='changed-current-route' WHERE id=$1`,
		f.environmentID); err != nil {
		_ = routeMutation.Rollback(ctx)
		t.Fatal(err)
	}
	if err = routeMutation.QueryRow(ctx, `SELECT public.helm_application_continuation_is_exact($1)`,
		application.ID).Scan(&continuationExact); err != nil {
		_ = routeMutation.Rollback(ctx)
		t.Fatal(err)
	}
	if continuationExact {
		_ = routeMutation.Rollback(ctx)
		t.Fatal("changed current route retained no-change continuation authority")
	}
	if err = routeMutation.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertMaterializationMutationDenied := func(name, statement string, value any) {
		t.Helper()
		mutation, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if _, mutationErr := mutation.Exec(ctx, `ALTER TABLE public.argo_desired_state_materialization_receipts
			DISABLE TRIGGER USER`); mutationErr == nil {
			_, mutationErr = mutation.Exec(ctx, statement, currentMaterialization.ID, value)
			if mutationErr == nil {
				mutationErr = mutation.QueryRow(ctx,
					`SELECT public.helm_application_continuation_is_exact($1)`,
					application.ID).Scan(&continuationExact)
			}
			if mutationErr != nil {
				_ = mutation.Rollback(ctx)
				t.Fatal(mutationErr)
			}
		} else {
			_ = mutation.Rollback(ctx)
			t.Fatal(mutationErr)
		}
		if continuationExact {
			_ = mutation.Rollback(ctx)
			t.Fatalf("changed current materialization %s retained continuation authority", name)
		}
		if rollbackErr := mutation.Rollback(ctx); rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
	}
	assertMaterializationMutationDenied("content", `UPDATE public.argo_desired_state_materialization_receipts
		SET desired_state_content_sha256=$2 WHERE id=$1`, helmPGDigest([]byte("changed-current-content")))
	assertMaterializationMutationDenied("runtime", `UPDATE public.argo_desired_state_materialization_receipts
		SET chart_version=$2 WHERE id=$1`, "99.0.0")
	if _, err = pool.Exec(ctx, `UPDATE helm_application_continuation_receipts
		SET current_app_project_content=$2 WHERE application_intent_id=$1`,
		application.ID, []byte("revoked AppProject authority\n")); err == nil {
		t.Fatal("continuation AppProject authority mutation bypassed immutable receipt trigger")
	}
	replayedApplication, replay, err := currentStore.CreateApplicationForPayload(ctx, applicationID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher, observedAt.Add(4*time.Second))
	if err != nil || !replay || replayedApplication.ID != application.ID {
		t.Fatalf("continuation replacement replay=%+v replay=%v err=%v", replayedApplication, replay, err)
	}
	if _, _, err = currentStore.CreateApplicationForPayload(ctx, id.New(), payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		observedAt.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate continuation replacement was accepted: %v", err)
	}
	// A continuation is current authority, not an everlasting capability. Move
	// the environment route to a new active projection before a matching
	// materialization exists. The untouched Application must retire and phase
	// two must remain blocked until the new AppProject is independently rendered.
	changedEnvironmentRevision := strings.Repeat("4", 40)
	changedProjectionAt := observedAt.Add(5 * time.Second)
	changedProjectionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE git_projection_generations SET state='failed',activated_at=NULL
			WHERE binding_id=$1 AND state='active'`, []any{f.environmentBindingID}},
		{`INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
			VALUES($1,4,$2,'gitprojection.v1','active',$3,$3)`,
			[]any{f.environmentBindingID, changedEnvironmentRevision, changedProjectionAt}},
		{`UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,
			projection_generation=4,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
			[]any{f.environmentBindingID, changedEnvironmentRevision, changedProjectionAt}},
	} {
		if _, err = changedProjectionTx.Exec(ctx, statement.query, statement.args...); err != nil {
			_ = changedProjectionTx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err = changedProjectionTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT public.helm_application_continuation_is_exact($1)`,
		application.ID).Scan(&continuationExact); err != nil {
		t.Fatal(err)
	}
	if continuationExact {
		t.Fatal("changed current projection retained stale continuation authority")
	}
	rotatedPublisher := publisher
	rotatedPublisher.ConfigDigest = helmPGDigest([]byte("rotated-continuation-publisher"))
	rotatedWorker := "helm-lifecycle-worker-0002"
	rotationAuthorityAt := helmPGDatabaseNow(t, ctx, pool)
	rotatedStartedAt := rotationAuthorityAt.Add(-time.Millisecond)
	if !rotatedStartedAt.After(readinessAt.Add(-time.Second)) {
		rotatedStartedAt = readinessAt.Add(-500 * time.Millisecond)
	}
	if err = currentStore.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: rotatedWorker, WorkerEpoch: 2, Publisher: rotatedPublisher,
		StartedAt: rotatedStartedAt, ObservedAt: rotationAuthorityAt,
		LeaseUntil: rotationAuthorityAt.Add(4 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	argoLease, err = argoStore.HeartbeatDesiredStateReadiness(ctx, argoLease,
		rotationAuthorityAt, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = currentStore.ActivateCascadeObserver(ctx, rotatedWorker, 2,
		rotatedPublisher, rotationAuthorityAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = currentStore.ClaimApplication(ctx, rotatedWorker, rotatedPublisher,
		changedProjectionAt, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale current continuation was not retired before claim: %v", err)
	}
	staleContinuationApplication, err := currentStore.Application(ctx, application.ID)
	if err != nil || staleContinuationApplication.State != ProtectedSuperseded ||
		staleContinuationApplication.LastFailureCode != "projection-superseded" ||
		staleContinuationApplication.LeaseEpoch != 0 || staleContinuationApplication.Attempts != 0 ||
		!staleContinuationApplication.ContinuationRequired {
		t.Fatalf("stale continuation predecessor=%+v err=%v", staleContinuationApplication, err)
	}
	if candidate, candidateErr := currentStore.NextApplicationCandidate(ctx, rotatedPublisher); candidateErr != nil ||
		candidate.PayloadIntentID != payload.ID {
		t.Fatalf("stale continuation did not reopen phase two: %+v err=%v", candidate, candidateErr)
	}
	publisher = rotatedPublisher
	blockedReplacementID := id.New()
	if _, _, err = currentStore.CreateApplicationForPayload(ctx, blockedReplacementID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		changedProjectionAt.Add(time.Second)); err == nil {
		t.Fatal("changed current projection was accepted without a fresh materialization")
	}
	var blockedReceipts int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM helm_application_continuation_receipts
		WHERE application_intent_id=$1`, blockedReplacementID).Scan(&blockedReceipts); err != nil {
		t.Fatal(err)
	}
	if blockedReceipts != 0 {
		t.Fatalf("failed changed-authority attempt persisted continuation receipts=%d", blockedReceipts)
	}

	changedAuthority := currentAuthority
	changedAuthority.PolicyDigest = helmPGDigest([]byte("argo-materialization-policy-changed"))
	changedAuthority.Runtime.ChartVersion = "1.2.5"
	changedAuthority.Runtime.ChartDigest = helmPGDigest([]byte("runtime-chart-changed"))
	changedAuthority.Runtime.RendererImage = "registry.example.com/kuberploy/runtime@" +
		helmPGDigest([]byte("renderer-changed"))
	changedProject, renderErr := argo.RenderAppProjectAuthority(argo.AppProjectAuthority{
		ProjectID: f.projectID, EnvironmentID: f.environmentID,
		EnvironmentBindingID: f.environmentBindingID, Namespace: f.namespace,
		ArgoProject: f.argoProject, ArgoNamespace: "argocd",
		EnvironmentRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 102, Owner: "kuberploy", Name: "environment"},
		PlatformRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 101, Owner: "kuberploy", Name: "platform"}, Runtime: changedAuthority.Runtime,
	})
	if renderErr != nil || bytes.Equal(changedProject, currentProject) {
		t.Fatalf("changed current AppProject render did not change authority: err=%v", renderErr)
	}
	changedBundle := append(append([]byte(nil), changedProject...),
		[]byte("---\napiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: changed\n")...)
	changedDesiredStateCommandID, changedDesiredStateRevision := id.New(), strings.Repeat("5", 40)
	changedCommandTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertVerifiedArgoDesiredStateCommand(t, ctx, changedCommandTx, f,
		changedDesiredStateCommandID, 3, changedEnvironmentRevision, 4,
		changedDesiredStateRevision, payloadCommit, changedAuthority,
		changedProject, changedBundle, changedProjectionAt.Add(2*time.Second))
	if err = changedCommandTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	currentStore, err = NewPostgresProtectedPublicationStore(pool, changedAuthority)
	if err != nil {
		t.Fatal(err)
	}
	applicationID = id.New()
	application, replay, err = currentStore.CreateApplicationForPayload(ctx, applicationID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		changedProjectionAt.Add(3*time.Second))
	if err != nil || replay || application.State != ProtectedPending {
		t.Fatalf("fresh changed-authority replacement=%+v replay=%v err=%v", application, replay, err)
	}
	continuation, continuationErr = currentStore.ApplicationContinuation(ctx, application.ID)
	if continuationErr != nil || continuation.CurrentEnvironmentRevision != changedEnvironmentRevision ||
		continuation.CurrentEnvironmentGeneration != 4 ||
		continuation.CurrentDesiredStateCommandID != changedDesiredStateCommandID ||
		continuation.CurrentDesiredStateRevision != changedDesiredStateRevision ||
		continuation.CurrentRuntime != changedAuthority.Runtime ||
		!bytes.Equal(continuation.CurrentAppProjectContent, changedProject) {
		t.Fatalf("fresh changed continuation=%+v err=%v", continuation, continuationErr)
	}
	// A newer verified materialization in the same active projection also
	// revokes an untouched continuation: its policy, runtime, and canonical
	// AppProject are no longer current even though the Git projection is equal.
	latestAuthority := changedAuthority
	latestAuthority.PolicyDigest = helmPGDigest([]byte("argo-materialization-policy-latest"))
	latestAuthority.Runtime.ChartVersion = "1.2.6"
	latestAuthority.Runtime.ChartDigest = helmPGDigest([]byte("runtime-chart-latest"))
	latestAuthority.Runtime.RendererImage = "registry.example.com/kuberploy/runtime@" +
		helmPGDigest([]byte("renderer-latest"))
	latestProject, renderErr := argo.RenderAppProjectAuthority(argo.AppProjectAuthority{
		ProjectID: f.projectID, EnvironmentID: f.environmentID,
		EnvironmentBindingID: f.environmentBindingID, Namespace: f.namespace,
		ArgoProject: f.argoProject, ArgoNamespace: "argocd",
		EnvironmentRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 102, Owner: "kuberploy", Name: "environment"},
		PlatformRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 101, Owner: "kuberploy", Name: "platform"}, Runtime: latestAuthority.Runtime,
	})
	if renderErr != nil || bytes.Equal(latestProject, changedProject) {
		t.Fatalf("latest AppProject render did not change authority: err=%v", renderErr)
	}
	latestBundle := append(append([]byte(nil), latestProject...),
		[]byte("---\napiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: latest\n")...)
	latestDesiredStateCommandID, latestDesiredStateRevision := id.New(), strings.Repeat("7", 40)
	latestCommandAt := changedProjectionAt.Add(3500 * time.Millisecond)
	latestCommandTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertVerifiedArgoDesiredStateCommand(t, ctx, latestCommandTx, f,
		latestDesiredStateCommandID, 4, changedEnvironmentRevision, 4,
		latestDesiredStateRevision, payloadCommit, latestAuthority,
		latestProject, latestBundle, latestCommandAt)
	if err = latestCommandTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT public.helm_application_continuation_is_exact($1)`,
		application.ID).Scan(&continuationExact); err != nil {
		t.Fatal(err)
	}
	if continuationExact {
		t.Fatal("newer materialization retained stale policy/runtime/AppProject authority")
	}
	latestRetirementAt := changedProjectionAt.Add(4 * time.Second)
	if _, _, err = currentStore.ClaimApplication(ctx, rotatedWorker, publisher,
		latestRetirementAt, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale materialization continuation was not retired: %v", err)
	}
	if candidate, candidateErr := currentStore.NextApplicationCandidate(ctx, publisher); candidateErr != nil ||
		candidate.PayloadIntentID != payload.ID {
		t.Fatalf("newer materialization did not reopen phase two: %+v err=%v", candidate, candidateErr)
	}
	staleMaterializationReplacementID := id.New()
	if _, _, err = currentStore.CreateApplicationForPayload(ctx, staleMaterializationReplacementID,
		payload.ID, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		latestRetirementAt.Add(100*time.Millisecond)); err == nil {
		t.Fatal("stale runtime authority created a continuation after newer materialization")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM helm_application_continuation_receipts
		WHERE application_intent_id=$1`, staleMaterializationReplacementID).Scan(&blockedReceipts); err != nil {
		t.Fatal(err)
	}
	if blockedReceipts != 0 {
		t.Fatalf("stale materialization attempt persisted continuation receipts=%d", blockedReceipts)
	}
	currentStore, err = NewPostgresProtectedPublicationStoreWithCascade(pool, latestAuthority,
		argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	applicationID = id.New()
	application, replay, err = currentStore.CreateApplicationForPayload(ctx, applicationID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		latestRetirementAt.Add(500*time.Millisecond))
	if err != nil || replay || application.State != ProtectedPending {
		t.Fatalf("latest materialization replacement=%+v replay=%v err=%v", application, replay, err)
	}
	continuation, continuationErr = currentStore.ApplicationContinuation(ctx, application.ID)
	if continuationErr != nil || continuation.CurrentDesiredStateCommandID != latestDesiredStateCommandID ||
		continuation.CurrentDesiredStateRevision != latestDesiredStateRevision ||
		continuation.CurrentRuntime != latestAuthority.Runtime ||
		!bytes.Equal(continuation.CurrentAppProjectContent, latestProject) {
		t.Fatalf("latest continuation=%+v err=%v", continuation, continuationErr)
	}
	firstApplicationClaimAt := latestRetirementAt.Add(time.Second)
	var firstApplicationLease ProtectedIntentLease
	application, firstApplicationLease, err = currentStore.ClaimApplication(ctx,
		rotatedWorker, publisher, firstApplicationClaimAt, time.Minute)
	if err != nil || application.ID != applicationID || firstApplicationLease.Epoch != 1 {
		t.Fatalf("initial exact continuation claim=%+v lease=%+v err=%v",
			application, firstApplicationLease, err)
	}
	recoveryAt := firstApplicationClaimAt.Add(time.Second)
	application, err = currentStore.RetryApplication(ctx, firstApplicationLease,
		"provider-unavailable", recoveryAt, firstApplicationClaimAt)
	if err != nil || application.State != ProtectedPending || application.LeaseEpoch != 1 {
		t.Fatalf("continuation recovery setup=%+v err=%v", application, err)
	}
	recoveryEnvironmentRevision := strings.Repeat("6", 40)
	recoveryProjectionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE git_projection_generations SET state='failed',activated_at=NULL
			WHERE binding_id=$1 AND state='active'`, []any{f.environmentBindingID}},
		{`INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
			VALUES($1,5,$2,'gitprojection.v1','active',$3,$3)`,
			[]any{f.environmentBindingID, recoveryEnvironmentRevision, recoveryAt}},
		{`UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,
			projection_generation=5,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
			[]any{f.environmentBindingID, recoveryEnvironmentRevision, recoveryAt}},
	} {
		if _, err = recoveryProjectionTx.Exec(ctx, statement.query, statement.args...); err != nil {
			_ = recoveryProjectionTx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err = recoveryProjectionTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT public.helm_application_continuation_is_exact($1)`,
		application.ID).Scan(&continuationExact); err != nil {
		t.Fatal(err)
	}
	if continuationExact {
		t.Fatal("post-attempt projection change retained current continuation authority")
	}
	var applicationLease ProtectedIntentLease
	application, applicationLease, err = currentStore.ClaimApplication(ctx,
		rotatedWorker, publisher, recoveryAt, time.Minute)
	if err != nil || application.ID != applicationID || applicationLease.Epoch != 2 {
		t.Fatalf("attempted continuation was not recoverable after authority advance: application=%+v lease=%+v err=%v",
			application, applicationLease, err)
	}
	var applicationRows, supersededRows, liveRows int
	if err = pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE state='superseded'),
		count(*) FILTER (WHERE state<>'superseded')
		FROM helm_protected_application_intents WHERE payload_intent_id=$1`, payload.ID).
		Scan(&applicationRows, &supersededRows, &liveRows); err != nil {
		t.Fatal(err)
	}
	if applicationRows != 4 || supersededRows != 3 || liveRows != 1 {
		t.Fatalf("continuation replacement history rows=%d superseded=%d live=%d",
			applicationRows, supersededRows, liveRows)
	}
	if receipt, receiptErr := store.PublicationPrerequisite(ctx, release.ID); receiptErr != nil ||
		receipt.PlannedBaseRevision != payloadCommit {
		t.Fatalf("phase-two legacy receipt=%+v err=%v", receipt, receiptErr)
	}
	if candidate, candidateErr := store.NextApplicationCandidate(ctx, publisher); !errors.Is(candidateErr, ErrNotFound) {
		t.Fatalf("planned application remained a candidate: %+v err=%v", candidate, candidateErr)
	}
	if bytes := string(application.Content); strings.Contains(bytes, "targetRevision: refs/") ||
		!strings.Contains(bytes, "targetRevision: "+payloadCommit) || strings.Contains(bytes, "helm:") {
		t.Fatalf("unsafe Application source:\n%s", bytes)
	}
	applicationWorkAt := recoveryAt
	application, err = store.BindApplicationWriteBase(ctx, applicationLease, payloadCommit,
		applicationWorkAt.Add(time.Second), applicationWorkAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	applicationRebind := strings.Repeat("e", 40)
	application, err = store.RebindApplicationWriteBase(ctx, applicationLease, payloadCommit,
		applicationRebind, applicationWorkAt.Add(2*time.Second), applicationWorkAt.Add(2*time.Second))
	if err != nil || application.WriteBaseRevision != applicationRebind {
		t.Fatalf("rebound application=%+v err=%v", application, err)
	}
	staleApplicationLease := applicationLease
	staleApplicationLease.Owner = "helm-publisher-worker-0999"
	if _, err = store.RebindApplicationWriteBase(ctx, staleApplicationLease, applicationRebind,
		strings.Repeat("f", 40), applicationWorkAt.Add(3*time.Second),
		applicationWorkAt.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale application lease rebound protected authority: %v", err)
	}
	if _, err = store.RebindApplicationWriteBase(ctx, applicationLease, payloadCommit,
		strings.Repeat("f", 40), applicationWorkAt.Add(3*time.Second),
		applicationWorkAt.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong application base rebound protected authority: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE helm_protected_application_intents
		SET content=$2 WHERE id=$1`, application.ID, []byte("identity mutation\n")); err == nil {
		t.Fatal("application identity mutation bypassed the migration trigger")
	}
	applicationCommit := strings.Repeat("c", 40)
	application, err = store.MarkApplicationCommitted(ctx, applicationLease, applicationCommit,
		applicationRebind, applicationWorkAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RebindApplicationWriteBase(ctx, applicationLease, applicationRebind,
		strings.Repeat("f", 40), applicationWorkAt.Add(4*time.Second),
		applicationWorkAt.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed application write base was mutable: %v", err)
	}
	application, err = store.VerifyApplication(ctx, applicationLease, applicationCommit,
		application.ContentDigest, "provider-application-verified", applicationWorkAt.Add(3*time.Second))
	if err != nil || application.State != ProtectedVerified {
		t.Fatalf("verified application=%+v err=%v", application, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE argo_desired_state_commands
		SET policy_digest=NULL WHERE id=$1`, f.desiredStateCommandID); err == nil {
		t.Fatal("policy digest mutation bypassed the migration 005 fence")
	}
	readiness := ProtectedPublisherReadiness{WorkerID: "helm-publisher-worker-0003",
		WorkerEpoch: 1, Publisher: publisher, StartedAt: observedAt,
		ObservedAt: observedAt.Add(time.Second), LeaseUntil: observedAt.Add(time.Minute)}
	if err = store.PutPublisherReadiness(ctx, readiness); err != nil {
		t.Fatal(err)
	}
	ready, err := store.PublisherReady(ctx, publisher, observedAt.Add(2*time.Second))
	if err != nil || !ready {
		t.Fatalf("publisher readiness=%v err=%v", ready, err)
	}
	ready, err = store.PublisherReady(ctx, wrongPublisher, observedAt.Add(2*time.Second))
	if err != nil || ready {
		t.Fatalf("wrong publisher readiness=%v err=%v", ready, err)
	}
}

func TestPostgresProtectedPublicationStoreDisableLifecycle(t *testing.T) {
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
	f := newHelmReleasePGFixture()
	f.now = time.Now().UTC().Add(-30 * time.Second).Truncate(time.Microsecond)
	registerHelmAuthorityCleanup(t, pool, f.platformBindingID,
		"helm-disable-worker-", "helm-publisher-worker-", "helm-cascade-observer-",
		"argo-desired-state-cascade-")
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, f)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	releases, err := NewPostgresReleaseService(pool, helmPGOperatorDigest())
	if err != nil {
		t.Fatal(err)
	}
	argoIdentity, argoObservation := helmPGArgoObservation(t, f,
		"argo-desired-state-cascade-0001", f.now)
	store, err := NewPostgresProtectedPublicationStoreWithCascade(pool, helmPGArgoAuthority(), argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID,
		ApplicationID: f.applicationID}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest}
	publisherWorker := "helm-disable-worker-0001"
	readinessAt := f.now.Add(3 * time.Second)
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{WorkerID: publisherWorker,
		WorkerEpoch: 1, Publisher: publisher, StartedAt: f.now,
		ObservedAt: readinessAt, LeaseUntil: readinessAt.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	argoObservation.ObservedAt = readinessAt
	argoLease, err := argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, publisherWorker, 1, publisher, readinessAt); err != nil {
		t.Fatal(err)
	}
	binding := ProtectedBindingSnapshot{PlatformBindingID: f.platformBindingID,
		EnvironmentBindingID: f.environmentBindingID, ClusterID: f.clusterID,
		PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
		EnvironmentRevision: f.environmentHead, EnvironmentGeneration: 1,
		CatalogDigest: f.catalogDigest, PlannedBaseRevision: f.platformHead}

	release, replay, err := releases.Upsert(ctx, UpsertReleaseRequest{
		Target: target, Approval: ApprovalKey{ID: f.approvalID, Revision: 1}, ValuesYAML: f.values,
		Actor: ReleaseActor{ID: f.userID, IdempotencyKey: "disable-predecessor-" + id.New(),
			RequestID: "disable-predecessor"},
	}, f.now.Add(time.Second))
	if err != nil || replay || !release.DesiredEnabled {
		t.Fatalf("predecessor release=%+v replay=%v err=%v", release, replay, err)
	}
	renderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, renderTx, f, release.RenderCommandID, f.now.Add(2*time.Second))
	if err = renderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	payload, replay, err := store.CreatePayloadForHead(ctx, id.New(), target, binding,
		publisher, f.now.Add(4*time.Second))
	if err != nil || replay || payload.Action != ProtectedPayloadPublish {
		t.Fatalf("predecessor payload=%+v replay=%v err=%v", payload, replay, err)
	}
	payload, payloadLease, err := store.ClaimPayload(ctx, publisherWorker, publisher,
		f.now.Add(5*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = store.BindPayloadWriteBase(ctx, payloadLease, f.platformHead,
		f.now.Add(6*time.Second), f.now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	payloadCommit := strings.Repeat("b", 40)
	payload, err = store.MarkPayloadCommitted(ctx, payloadLease, payloadCommit, f.platformHead,
		f.now.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	payload, err = store.VerifyPayload(ctx, payloadLease, payloadCommit, payload.ContentDigest,
		"disable-predecessor-payload", f.now.Add(8*time.Second))
	if err != nil || payload.State != ProtectedVerified {
		t.Fatalf("verified predecessor payload=%+v err=%v", payload, err)
	}
	advanceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, payloadCommit, f.now.Add(9*time.Second))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	application, replay, err := store.CreateApplicationForPayload(ctx, id.New(), payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher, f.now.Add(10*time.Second))
	if err != nil || replay || application.Action != ProtectedApplicationPublish {
		t.Fatalf("predecessor application=%+v replay=%v err=%v", application, replay, err)
	}
	requireProtectedForegroundResourcesFinalizer(t, application.Content)
	application, applicationLease, err := store.ClaimApplication(ctx, publisherWorker,
		publisher, f.now.Add(11*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	application, err = store.BindApplicationWriteBase(ctx, applicationLease, payloadCommit,
		f.now.Add(12*time.Second), f.now.Add(12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	applicationCommit := strings.Repeat("c", 40)
	application, err = store.MarkApplicationCommitted(ctx, applicationLease, applicationCommit,
		payloadCommit, f.now.Add(13*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	application, err = store.VerifyApplication(ctx, applicationLease, applicationCommit,
		application.ContentDigest, "disable-predecessor-application", f.now.Add(14*time.Second))
	if err != nil || application.State != ProtectedVerified {
		t.Fatalf("verified predecessor application=%+v err=%v", application, err)
	}
	// Simulate the exact legacy pre-finalizer Application produced before the
	// cascade contract existed. The test-only rewrite preserves every other
	// immutable field so the new preflight must adopt only the foreground
	// resources finalizer before it may delete.
	var legacyApplication protectedArgoApplication
	if err = yaml.Unmarshal(application.Content, &legacyApplication); err != nil {
		t.Fatal(err)
	}
	legacyApplication.Metadata.Finalizers = nil
	legacyContent, err := yaml.Marshal(legacyApplication)
	if err != nil {
		t.Fatal(err)
	}
	legacyContent = bytes.Replace(legacyContent, []byte("    finalizers: []\n"), nil, 1)
	if bytes.Contains(legacyContent, []byte("    finalizers:\n")) ||
		bytes.Contains(legacyContent, []byte("    finalizers: []\n")) {
		t.Fatal("legacy Application fixture retained a finalizer field")
	}
	legacyDigest := digestBytes(legacyContent)
	if _, changed, adoptErr := adoptProtectedArgoResourcesFinalizer(legacyContent); adoptErr != nil || !changed {
		t.Fatalf("legacy Application did not require exact finalizer adoption: changed=%v err=%v",
			changed, adoptErr)
	}
	legacyTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacyTx.Exec(ctx, `ALTER TABLE public.helm_protected_application_intents DISABLE TRIGGER USER`); err == nil {
		_, err = legacyTx.Exec(ctx, `UPDATE public.helm_protected_application_intents
			SET content=$2,content_digest=$3,verified_path_digest=$3 WHERE id=$1`,
			application.ID, legacyContent, legacyDigest)
	}
	if err == nil && application.ContinuationRequired {
		_, err = legacyTx.Exec(ctx, `ALTER TABLE public.helm_application_continuation_receipts DISABLE TRIGGER USER`)
	}
	if err == nil && application.ContinuationRequired {
		_, err = legacyTx.Exec(ctx, `UPDATE public.helm_application_continuation_receipts
			SET application_content_digest=$2 WHERE application_intent_id=$1`,
			application.ID, legacyDigest)
	}
	if err == nil && application.ContinuationRequired {
		_, err = legacyTx.Exec(ctx, `ALTER TABLE public.helm_application_continuation_receipts ENABLE TRIGGER USER`)
	}
	if err == nil {
		_, err = legacyTx.Exec(ctx, `ALTER TABLE public.helm_protected_application_intents ENABLE TRIGGER USER`)
	}
	if err != nil {
		_ = legacyTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = legacyTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	advanceTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, applicationCommit, f.now.Add(15*time.Second))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	disableRequest := DisableReleaseRequest{Target: target, Actor: ReleaseActor{ID: f.userID,
		IdempotencyKey: "disable-lifecycle-" + id.New(), RequestID: "disable-lifecycle"}}
	disable, replay, err := releases.Disable(ctx, disableRequest, f.now.Add(16*time.Second))
	if err != nil || replay || disable.DesiredEnabled || disable.Action != ReleaseDisable ||
		disable.BaseApplicationIntentID != application.ID {
		t.Fatalf("disable release=%+v replay=%v err=%v", disable, replay, err)
	}
	replayedDisable, replay, err := releases.Disable(ctx, disableRequest, f.now.Add(17*time.Second))
	if err != nil || !replay || replayedDisable.ID != disable.ID {
		t.Fatalf("disable replay=%+v replay=%v err=%v", replayedDisable, replay, err)
	}

	binding.PlannedBaseRevision = applicationCommit
	disablePayloadID := id.New()
	disablePayload, replay, err := store.CreatePayloadForHead(ctx, disablePayloadID, target, binding,
		publisher, f.now.Add(18*time.Second))
	if err != nil || replay || disablePayload.Action != ProtectedPayloadDisable {
		t.Fatalf("disable payload=%+v replay=%v err=%v", disablePayload, replay, err)
	}
	disablePayload, disablePayloadLease, err := store.ClaimPayload(ctx, publisherWorker,
		publisher, f.now.Add(19*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	disablePayload, err = store.BindPayloadWriteBase(ctx, disablePayloadLease, applicationCommit,
		f.now.Add(20*time.Second), f.now.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	disablePayloadCommit := strings.Repeat("d", 40)
	disablePayload, err = store.MarkPayloadCommitted(ctx, disablePayloadLease, disablePayloadCommit,
		applicationCommit, f.now.Add(21*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	disablePayload, err = store.VerifyPayload(ctx, disablePayloadLease, disablePayloadCommit,
		disablePayload.ContentDigest, "disable-payload", f.now.Add(22*time.Second))
	if err != nil || disablePayload.State != ProtectedVerified {
		t.Fatalf("verified disable payload=%+v err=%v", disablePayload, err)
	}
	advanceTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, disablePayloadCommit, f.now.Add(23*time.Second))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Current routing can change after the verified base was published. Cascade
	// observation must still bind the old live child's immutable foundation route.
	if _, err = pool.Exec(ctx, `UPDATE environments SET namespace='new-disable-route',
		argo_project='new-disable-route' WHERE id=$1`, f.environmentID); err != nil {
		t.Fatal(err)
	}
	preflightID, deleteID := id.New(), id.New()
	preflight, replay, err := store.CreateCascadePreflightForPayload(ctx, preflightID, deleteID,
		disablePayload.ID, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		f.now.Add(24*time.Second))
	if err != nil || replay || preflight.DeleteIntentID != deleteID || preflight.Operation != "update" {
		t.Fatalf("cascade preflight=%+v replay=%v err=%v", preflight, replay, err)
	}
	preflightWorker := publisherWorker
	preflight, preflightLease, err := store.ClaimCascadePreflight(ctx, preflightWorker, publisher,
		f.now.Add(25*time.Second), time.Minute)
	if err != nil || preflight.ID != preflightID {
		t.Fatalf("claimed cascade preflight=%+v lease=%+v err=%v", preflight, preflightLease, err)
	}
	// Reproduce the prior-release live recovery shape exactly: a prior release worker
	// exhausted the diagnostic counter after many unknown-side-effect retries,
	// while the durable lease epoch proves that Git work had already been
	// admitted. A live lease must still block authority rotation.
	capPublisher := publisher
	capPublisher.ConfigDigest = "sha256:" + strings.Repeat("7", 64)
	capWorker := "helm-disable-worker-0002"
	capRotationAt := helmPGDatabaseNow(t, ctx, pool)
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: capWorker, WorkerEpoch: 2, Publisher: capPublisher,
		StartedAt: capRotationAt.Add(-time.Second), ObservedAt: capRotationAt,
		LeaseUntil: capRotationAt.Add(4 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	argoLease, err = argoStore.HeartbeatDesiredStateReadiness(ctx, argoLease,
		capRotationAt, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, capWorker, 2,
		capPublisher, capRotationAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("live cascade lease did not fence authority rotation: %v", err)
	}
	// The test-only transition seeds only the already-observed live runtime
	// counters. Migration 015 must recover this row through its normal adopter;
	// no receipt or publisher identity is preplanted.
	seedCap, seedErr := pool.Begin(ctx)
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	if _, seedErr = seedCap.Exec(ctx,
		`ALTER TABLE public.helm_application_cascade_preflights DISABLE TRIGGER USER`); seedErr == nil {
		_, seedErr = seedCap.Exec(ctx, `UPDATE public.helm_application_cascade_preflights SET
			attempts=30,lease_epoch=91,updated_at=clock_timestamp()-interval '2 seconds',
			lease_until=clock_timestamp()-interval '1 second'
			WHERE id=$1 AND state='claimed' AND lease_owner=$2`, preflightID, preflightWorker)
	}
	if seedErr == nil {
		_, seedErr = seedCap.Exec(ctx,
			`ALTER TABLE public.helm_application_cascade_preflights ENABLE TRIGGER USER`)
	}
	if seedErr != nil {
		_ = seedCap.Rollback(ctx)
		t.Fatal(seedErr)
	}
	if seedErr = seedCap.Commit(ctx); seedErr != nil {
		t.Fatal(seedErr)
	}
	capRotationAt = helmPGDatabaseNow(t, ctx, pool)
	if _, err = store.ActivateCascadeObserver(ctx, capWorker, 2,
		capPublisher, capRotationAt); err != nil {
		t.Fatal(err)
	}
	preflight, preflightLease, err = store.AdoptCascadePreflight(ctx, capWorker, 2,
		capPublisher, minimumProtectedLease)
	if err != nil || preflight.ID != preflightID || preflight.Attempts != 30 ||
		preflight.LeaseEpoch != 92 || preflight.PublisherAdoptionEpoch != 1 {
		t.Fatalf("adopted saturated cascade preflight=%+v lease=%+v err=%v",
			preflight, preflightLease, err)
	}
	var saturatedReceipts int
	if err = pool.QueryRow(ctx, `SELECT count(*)
		FROM public.helm_application_cascade_adoption_receipts
		WHERE cascade_preflight_id=$1 AND previous_lease_epoch=91
		  AND adopted_lease_epoch=92 AND adopted_config_digest=$2`,
		preflightID, capPublisher.ConfigDigest).Scan(&saturatedReceipts); err != nil || saturatedReceipts != 1 {
		t.Fatalf("saturated cascade adoption receipts=%d err=%v", saturatedReceipts, err)
	}
	publisher = capPublisher
	publisherWorker = capWorker
	preflightWorker = capWorker
	// Prove the additive path-absence authority against a real PG18 state. The
	// transaction is rolled back after forcing deferred checks so the same
	// fixture can continue through successful finalizer adoption and pruning.
	absenceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	providerObservedAt := f.now.Add(25 * time.Second)
	assertAbsenceRejected := func(name string, mutate func(pgx.Tx) error) {
		t.Helper()
		nested, beginErr := absenceTx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if mutateErr := mutate(nested); mutateErr == nil {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s path-absence proof was accepted", name)
		}
		_ = nested.Rollback(ctx)
	}
	assertAbsenceRejected("stale provider head", func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `INSERT INTO public.helm_application_cascade_absence_receipts(
			cascade_preflight_id,provider_head,provider_request,provider_observed_at,
			operation_commit_absent) VALUES($1,$2,'absence-stale-head',$3,true)`,
			preflightID, strings.Repeat("8", 40), providerObservedAt)
		return nestedErr
	})
	assertAbsenceRejected("unproven operation", func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `INSERT INTO public.helm_application_cascade_absence_receipts(
			cascade_preflight_id,provider_head,provider_request,provider_observed_at,
			operation_commit_absent) VALUES($1,$2,'absence-operation-present',$3,false)`,
			preflightID, disablePayloadCommit, providerObservedAt)
		return nestedErr
	})
	assertAbsencePostimageRejected := func(name string, mutate func(pgx.Tx, time.Time) error) {
		t.Helper()
		nested, beginErr := absenceTx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		var recordedAt time.Time
		if pairErr := nested.QueryRow(ctx, `INSERT INTO public.helm_application_cascade_absence_receipts(
			cascade_preflight_id,provider_head,provider_request,provider_observed_at,
			operation_commit_absent) VALUES($1,$2,$3,$4,true) RETURNING recorded_at`,
			preflightID, disablePayloadCommit, "absence-"+name, providerObservedAt).
			Scan(&recordedAt); pairErr != nil {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s receipt setup failed: %v", name, pairErr)
		}
		if pairErr := mutate(nested, recordedAt); pairErr != nil {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s authority mutation setup failed: %v", name, pairErr)
		}
		if _, pairErr := nested.Exec(ctx, `UPDATE public.helm_application_cascade_preflights SET
				state='failed',consecutive_failures=consecutive_failures+1,
				last_failure_code='cascade-path-absent-recovery-required',
				lease_owner=NULL,lease_until=NULL,completed_at=$2,updated_at=$2,
				prerequisite_epoch=prerequisite_epoch+1
				WHERE id=$1 AND state='claimed' AND lease_owner=$3 AND lease_epoch=$4
				  AND committed_revision='' AND committed_at IS NULL`, preflightID,
			recordedAt, preflightLease.Owner, preflightLease.Epoch); pairErr != nil {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s terminal transition setup failed: %v", name, pairErr)
		}
		var exactAfterMutation bool
		if pairErr := nested.QueryRow(ctx,
			`SELECT public.helm_application_cascade_absence_receipt_is_exact($1)`, preflightID).
			Scan(&exactAfterMutation); pairErr != nil {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s exactness probe failed: %v", name, pairErr)
		} else if exactAfterMutation {
			_ = nested.Rollback(ctx)
			t.Fatalf("%s authority mutation retained exactness", name)
		}
		_, pairErr := nested.Exec(ctx, `SET CONSTRAINTS
				helm_application_cascade_absence_receipt_postimage,
				helm_application_cascade_absence_failure_postimage IMMEDIATE`)
		_ = nested.Rollback(ctx)
		if pairErr == nil {
			t.Fatalf("%s post-receipt authority change was accepted", name)
		}
	}
	assertAbsencePostimageRejected("git-head-advance", func(nested pgx.Tx, changedAt time.Time) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE public.git_repository_bindings SET
			target_head_revision=$2,state='indexing',target_head_observed_at=$3,updated_at=$3
			WHERE id=$1`, f.platformBindingID, strings.Repeat("8", 40), changedAt)
		return nestedErr
	})
	assertAbsencePostimageRejected("release-head-advance", func(nested pgx.Tx, changedAt time.Time) error {
		competingReleaseID, competingCommandID := id.New(), id.New()
		competingFixture := f
		competingFixture.namespace = "new-disable-route"
		insertHelmRenderCommand(t, ctx, nested, competingFixture, competingCommandID,
			f.values, f.valuesDigest, changedAt)
		insertHelmRelease(t, ctx, nested, competingFixture, helmReleaseInsert{id: competingReleaseID,
			generation: disable.Generation + 1, action: "rollback", parentID: disable.ID,
			rollbackID: release.ID, baseID: application.ID, commandID: competingCommandID,
			values: f.values, valuesDigest: f.valuesDigest}, changedAt)
		_, nestedErr := nested.Exec(ctx, `UPDATE public.helm_release_heads SET
			revision_id=$3,generation=$4,updated_at=$5
			WHERE environment_id=$1 AND application_id=$2`, f.environmentID,
			f.applicationID, competingReleaseID, disable.Generation+1, changedAt)
		return nestedErr
	})
	var absenceRecordedAt time.Time
	if err = absenceTx.QueryRow(ctx, `INSERT INTO public.helm_application_cascade_absence_receipts(
		cascade_preflight_id,provider_head,provider_request,provider_observed_at,
		operation_commit_absent) VALUES($1,$2,'absence-provider-proof',$3,true)
		RETURNING recorded_at`, preflightID, disablePayloadCommit, providerObservedAt).
		Scan(&absenceRecordedAt); err != nil {
		_ = absenceTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = absenceTx.Exec(ctx, `UPDATE public.helm_application_cascade_preflights SET
		state='failed',consecutive_failures=consecutive_failures+1,
		last_failure_code='cascade-path-absent-recovery-required',
		lease_owner=NULL,lease_until=NULL,completed_at=$2,updated_at=$2,
		prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND state='claimed' AND lease_owner=$3 AND lease_epoch=$4
		  AND committed_revision='' AND committed_at IS NULL`, preflightID,
		absenceRecordedAt, preflightLease.Owner, preflightLease.Epoch); err != nil {
		_ = absenceTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = absenceTx.Exec(ctx, `SET CONSTRAINTS
		helm_application_cascade_absence_receipt_postimage,
		helm_application_cascade_absence_failure_postimage IMMEDIATE`); err != nil {
		_ = absenceTx.Rollback(ctx)
		t.Fatal(err)
	}
	var absenceExact bool
	var terminalCandidates int
	if err = absenceTx.QueryRow(ctx, `SELECT
		public.helm_application_cascade_absence_receipt_is_exact($1),
		(SELECT count(*) FROM public.helm_application_cascade_preflights
		 WHERE id=$1 AND state IN ('pending','claimed','git-committed'))`, preflightID).
		Scan(&absenceExact, &terminalCandidates); err != nil || !absenceExact || terminalCandidates != 0 {
		_ = absenceTx.Rollback(ctx)
		t.Fatalf("absence exact=%v reclaimable=%d err=%v", absenceExact, terminalCandidates, err)
	}
	assertAbsenceRejected("receipt mutation", func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE public.helm_application_cascade_absence_receipts
			SET provider_request='forged' WHERE cascade_preflight_id=$1`, preflightID)
		return nestedErr
	})
	assertAbsenceRejected("replacement", func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `INSERT INTO public.helm_application_cascade_preflights
			SELECT (pg_catalog.jsonb_populate_record(NULL::public.helm_application_cascade_preflights,
				pg_catalog.to_jsonb(candidate)||pg_catalog.jsonb_build_object(
				'id',$2::text,'delete_intent_id',$3::text,'state','pending','attempts',0,
				'consecutive_failures',0,'last_failure_code','','lease_owner',NULL,
				'lease_epoch',0,'lease_until',NULL,'completed_at',NULL,
				'created_at',$4::timestamptz,'updated_at',$4::timestamptz,
				'next_attempt_at',$4::timestamptz))).*
			FROM public.helm_application_cascade_preflights AS candidate WHERE candidate.id=$1`,
			preflightID, id.New(), id.New(), absenceRecordedAt)
		return nestedErr
	})
	if err = absenceTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	nextAt := func(after time.Time) time.Time {
		t.Helper()
		return helmPGNotBefore(helmPGDatabaseNow(t, ctx, pool), after).Add(time.Microsecond)
	}
	failureAt := nextAt(preflight.UpdatedAt)
	type absenceRaceResult struct {
		preflight ProtectedApplicationCascadePreflight
		err       error
	}
	activationResult := make(chan error, 1)
	absenceResult := make(chan absenceRaceResult, 1)
	startRace := make(chan struct{})
	concurrentActivationAt := helmPGDatabaseNow(t, ctx, pool)
	go func() {
		<-startRace
		_, activateErr := store.ActivateCascadeObserver(ctx, capWorker, 2,
			capPublisher, concurrentActivationAt)
		activationResult <- activateErr
	}()
	go func() {
		<-startRace
		failed, failErr := store.FailCascadePreflightPathAbsent(ctx, preflightLease,
			ProtectedCascadePathAbsenceProof{ProviderHead: disablePayloadCommit,
				ProviderRequest: "absence-provider-proof", ProviderObservedAt: providerObservedAt,
				OperationCommitAbsent: true}, failureAt)
		absenceResult <- absenceRaceResult{preflight: failed, err: failErr}
	}()
	close(startRace)
	if activationErr := <-activationResult; activationErr != nil {
		t.Fatalf("activation/absence lock-order race did not converge: %v", activationErr)
	}
	result := <-absenceResult
	preflight, err = result.preflight, result.err
	if err != nil || preflight.State != ProtectedFailed ||
		preflight.LastFailureCode != "cascade-path-absent-recovery-required" {
		t.Fatalf("terminal absent cascade preflight=%+v err=%v", preflight, err)
	}
	failedStatus, err := releases.Head(ctx, target)
	if err != nil || failedStatus.Phase != ReleasePhaseFailed ||
		failedStatus.FailureCode != "cascade-path-absent-recovery-required" {
		t.Fatalf("absent cascade release status=%+v err=%v", failedStatus, err)
	}
	if _, _, err = store.ClaimCascadePreflight(ctx, preflightWorker, publisher,
		nextAt(preflight.UpdatedAt), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal absent cascade preflight was reclaimable: %v", err)
	}

	// Recovery is explicit: create a new enabled rollback release, publish its
	// Application with create-if-absent, then create a second disable release.
	// The failed preflight and receipt remain immutable history throughout.
	if _, err = pool.Exec(ctx, `UPDATE public.environments SET namespace=$2,argo_project=$3
		WHERE id=$1`, f.environmentID, f.namespace, f.argoProject); err != nil {
		t.Fatal(err)
	}
	rollbackAt := nextAt(preflight.UpdatedAt)
	rollback, replay, err := releases.Rollback(ctx, RollbackReleaseRequest{Target: target,
		SourceRevisionID: release.ID, Actor: ReleaseActor{ID: f.userID,
			IdempotencyKey: "absence-recovery-rollback-" + id.New(),
			RequestID:      "absence-recovery-rollback"}}, rollbackAt)
	if err != nil || replay || !rollback.DesiredEnabled || rollback.Action != ReleaseRollback {
		t.Fatalf("absence recovery rollback=%+v replay=%v err=%v", rollback, replay, err)
	}
	var recoveryAuthorized, wrongBaseAuthorized, wrongPathAuthorized, unrelatedEnabledAuthorized bool
	if err = pool.QueryRow(ctx, `SELECT
		public.helm_application_cascade_recovery_create_is_authorized($1,$2,$3),
		public.helm_application_cascade_recovery_create_is_authorized($1,$4,$3),
		public.helm_application_cascade_recovery_create_is_authorized($1,$2,$5),
		public.helm_application_cascade_recovery_create_is_authorized($6,$2,$3)`,
		rollback.ID, application.ID, application.ApplicationPath, id.New(),
		application.ApplicationPath+".wrong", release.ID).Scan(&recoveryAuthorized,
		&wrongBaseAuthorized, &wrongPathAuthorized, &unrelatedEnabledAuthorized); err != nil {
		t.Fatal(err)
	}
	if !recoveryAuthorized || wrongBaseAuthorized || wrongPathAuthorized || unrelatedEnabledAuthorized {
		t.Fatalf("absence recovery authority exact=%t wrong-base=%t wrong-path=%t unrelated-enabled=%t",
			recoveryAuthorized, wrongBaseAuthorized, wrongPathAuthorized, unrelatedEnabledAuthorized)
	}
	renderTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, renderTx, f, rollback.RenderCommandID, nextAt(rollbackAt))
	if err = renderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	binding.PlannedBaseRevision = disablePayloadCommit
	rollbackPayload, replay, err := store.CreatePayloadForHead(ctx, id.New(), target, binding,
		publisher, nextAt(rollbackAt))
	if err != nil || replay || rollbackPayload.Action != ProtectedPayloadPublish {
		t.Fatalf("rollback payload=%+v replay=%v err=%v", rollbackPayload, replay, err)
	}
	rollbackPayload, rollbackPayloadLease, err := store.ClaimPayload(ctx, publisherWorker,
		publisher, nextAt(rollbackPayload.UpdatedAt), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rollbackPayload, err = store.BindPayloadWriteBase(ctx, rollbackPayloadLease,
		disablePayloadCommit, nextAt(rollbackPayload.UpdatedAt), nextAt(rollbackPayload.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	rollbackPayloadCommit := strings.Repeat("1", 40)
	rollbackPayload, err = store.MarkPayloadCommitted(ctx, rollbackPayloadLease,
		rollbackPayloadCommit, disablePayloadCommit, nextAt(rollbackPayload.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	rollbackPayload, err = store.VerifyPayload(ctx, rollbackPayloadLease, rollbackPayloadCommit,
		rollbackPayload.ContentDigest, "absence-rollback-payload", nextAt(rollbackPayload.UpdatedAt))
	if err != nil || rollbackPayload.State != ProtectedVerified {
		t.Fatalf("verified rollback payload=%+v err=%v", rollbackPayload, err)
	}
	advanceTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, rollbackPayloadCommit, nextAt(rollbackPayload.UpdatedAt))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rollbackApplication, replay, err := store.CreateApplicationForPayload(ctx, id.New(),
		rollbackPayload.ID, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		nextAt(rollbackPayload.UpdatedAt))
	if err != nil || replay || rollbackApplication.Operation != "create" ||
		rollbackApplication.Precondition != "create-if-absent" {
		t.Fatalf("rollback recovery Application=%+v replay=%v err=%v",
			rollbackApplication, replay, err)
	}
	requireProtectedForegroundResourcesFinalizer(t, rollbackApplication.Content)
	rollbackApplication, rollbackApplicationLease, err := store.ClaimApplication(ctx,
		publisherWorker, publisher, nextAt(rollbackApplication.UpdatedAt), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rollbackApplication, err = store.BindApplicationWriteBase(ctx, rollbackApplicationLease,
		rollbackPayloadCommit, nextAt(rollbackApplication.UpdatedAt), nextAt(rollbackApplication.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	rollbackApplicationCommit := strings.Repeat("2", 40)
	rollbackApplication, err = store.MarkApplicationCommitted(ctx, rollbackApplicationLease,
		rollbackApplicationCommit, rollbackPayloadCommit, nextAt(rollbackApplication.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	rollbackApplication, err = store.VerifyApplication(ctx, rollbackApplicationLease,
		rollbackApplicationCommit, rollbackApplication.ContentDigest, "absence-rollback-application",
		nextAt(rollbackApplication.UpdatedAt))
	if err != nil || rollbackApplication.State != ProtectedVerified {
		t.Fatalf("verified rollback Application=%+v err=%v", rollbackApplication, err)
	}
	advanceTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, rollbackApplicationCommit,
		nextAt(rollbackApplication.UpdatedAt))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	secondDisable, replay, err := releases.Disable(ctx, DisableReleaseRequest{Target: target,
		Actor: ReleaseActor{ID: f.userID, IdempotencyKey: "absence-second-disable-" + id.New(),
			RequestID: "absence-second-disable"}}, nextAt(rollbackApplication.UpdatedAt))
	if err != nil || replay || secondDisable.DesiredEnabled || secondDisable.Action != ReleaseDisable ||
		secondDisable.BaseApplicationIntentID != rollbackApplication.ID {
		t.Fatalf("second disable=%+v replay=%v err=%v", secondDisable, replay, err)
	}
	binding.PlannedBaseRevision = rollbackApplicationCommit
	disablePayload, replay, err = store.CreatePayloadForHead(ctx, id.New(), target, binding,
		publisher, nextAt(secondDisable.CreatedAt))
	if err != nil || replay || disablePayload.Action != ProtectedPayloadDisable {
		t.Fatalf("second disable payload=%+v replay=%v err=%v", disablePayload, replay, err)
	}
	disablePayload, disablePayloadLease, err = store.ClaimPayload(ctx, publisherWorker,
		publisher, nextAt(disablePayload.UpdatedAt), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	disablePayload, err = store.BindPayloadWriteBase(ctx, disablePayloadLease,
		rollbackApplicationCommit, nextAt(disablePayload.UpdatedAt), nextAt(disablePayload.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	disablePayloadCommit = strings.Repeat("3", 40)
	disablePayload, err = store.MarkPayloadCommitted(ctx, disablePayloadLease,
		disablePayloadCommit, rollbackApplicationCommit, nextAt(disablePayload.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	disablePayload, err = store.VerifyPayload(ctx, disablePayloadLease, disablePayloadCommit,
		disablePayload.ContentDigest, "absence-second-disable-payload", nextAt(disablePayload.UpdatedAt))
	if err != nil || disablePayload.State != ProtectedVerified {
		t.Fatalf("verified second disable payload=%+v err=%v", disablePayload, err)
	}
	advanceTx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, disablePayloadCommit, nextAt(disablePayload.UpdatedAt))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	preflightID, deleteID = id.New(), id.New()
	preflight, replay, err = store.CreateCascadePreflightForPayload(ctx, preflightID, deleteID,
		disablePayload.ID, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher,
		nextAt(disablePayload.UpdatedAt))
	if err != nil || replay || preflight.Operation != "observe" ||
		preflight.SourceContentDigest != rollbackApplication.ContentDigest {
		t.Fatalf("second cascade preflight=%+v replay=%v err=%v", preflight, replay, err)
	}
	preflight, preflightLease, err = store.ClaimCascadePreflight(ctx, preflightWorker, publisher,
		nextAt(preflight.UpdatedAt), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err = store.BindCascadePreflightWriteBase(ctx, preflightLease, disablePayloadCommit,
		nextAt(preflight.UpdatedAt), nextAt(preflight.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	cascadeCommit := disablePayloadCommit
	preflight, err = store.VerifyCascadePreflight(ctx, preflightLease, cascadeCommit,
		preflight.AdoptedContentDigest, "cascade-finalizer-observed", nextAt(preflight.UpdatedAt))
	if err != nil || preflight.State != ProtectedVerified || preflight.CommittedRevision != "" {
		t.Fatalf("verified second cascade observation=%+v err=%v", preflight, err)
	}

	observerWorker := publisherWorker
	observationAt := nextAt(preflight.UpdatedAt)
	observedPreflight, observationLease, err := store.ClaimCascadeObservation(ctx, observerWorker, 2,
		publisher, observationAt, time.Minute)
	if err != nil || observedPreflight.ID != preflightID {
		t.Fatalf("cascade observation claim=%+v lease=%+v err=%v",
			observedPreflight, observationLease, err)
	}
	platformBinding, err := scanCascadePlatformBinding(pool.QueryRow(ctx, `SELECT id,kind,scope_id::text,
		COALESCE(project_id::text,''),COALESCE(environment_id::text,''),COALESCE(cluster_id::text,''),
		provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
		credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,
		projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at
		FROM public.git_repository_bindings WHERE id=$1`, f.platformBindingID))
	if err != nil {
		t.Fatal(err)
	}
	head := gitprojection.VerifiedHead{BindingID: platformBinding.ID, Repository: platformBinding.Repository,
		TargetRef: platformBinding.TargetRef, Commit: cascadeCommit,
		Source: gitprojection.ObservationWrite, ProviderRequest: "cascade-test-head", ObservedAt: observationAt}
	rootExpectation, err := argo.NewPlatformRootApplicationExpectation(argoIdentity, platformBinding, head)
	if err != nil {
		t.Fatal(err)
	}
	childExpectation, err := preflight.ApplicationExpectation()
	if err != nil {
		t.Fatal(err)
	}
	adoptionRevision, adoptionParentRevision := preflight.CommittedRevision, preflight.CommittedParentRevision
	if preflight.Operation == "observe" {
		adoptionRevision, adoptionParentRevision = preflight.WriteBaseRevision, preflight.WriteBaseRevision
	}
	receipt := ProtectedApplicationCascadeReceipt{ID: id.New(), DeleteIntentID: deleteID,
		CascadePreflightID: preflightID, ObservationEpoch: 1,
		ObservationLeaseEpoch:   observationLease.Epoch,
		ObserverActivationEpoch: observationLease.ObserverActivationEpoch,
		ReleaseRevisionID:       preflight.ReleaseRevisionID,
		PayloadIntentID:         preflight.PayloadIntentID, BaseApplicationIntentID: preflight.BaseApplicationIntentID,
		ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID,
		ClusterID: f.clusterID, ApplicationPath: preflight.ApplicationPath,
		SourceContentDigest: preflight.SourceContentDigest, AdoptedContentDigest: preflight.AdoptedContentDigest,
		AdoptionRevision: adoptionRevision, AdoptionParentRevision: adoptionParentRevision,
		ProviderHead: cascadeCommit, RootObservedRevision: cascadeCommit,
		RootUID: id.New(), RootResourceVersion: "root-rv-1", RootSpecDigest: rootExpectation.SpecDigest,
		RootSyncStatus: "Synced", ChildUID: id.New(), ChildResourceVersion: "child-rv-1",
		ChildSpecDigest: childExpectation.SpecDigest, FinalizerDigest: childExpectation.FinalizerDigest,
		ChildReleaseRevisionID: childExpectation.ReleaseRevisionID,
		ChildPayloadRevision:   childExpectation.TargetRevision, ChildPayloadPath: childExpectation.PayloadPath,
		ChildPayloadDigest: childExpectation.PayloadDigest, Publisher: publisher,
		WorkerID: observerWorker, WorkerEpoch: 2,
		ArgoContract: argoIdentity.ContractVersion, ArgoConfigDigest: argoIdentity.ConfigDigest,
		ObservedAt: observationAt}
	var expectedRootDigest, expectedChildDigest string
	if err = pool.QueryRow(ctx, `SELECT
		public.helm_application_cascade_expected_root_spec_digest($1),
		public.helm_application_cascade_expected_child_spec_digest($1)`, preflightID).
		Scan(&expectedRootDigest, &expectedChildDigest); err != nil {
		t.Fatal(err)
	}
	if receipt.RootSpecDigest != expectedRootDigest || receipt.ChildSpecDigest != expectedChildDigest {
		t.Fatalf("cascade expectations drifted: root=%s/%s child=%s/%s",
			receipt.RootSpecDigest, expectedRootDigest, receipt.ChildSpecDigest, expectedChildDigest)
	}
	receipt, err = store.RecordCascadeObservation(ctx, observationLease, receipt, observationAt)
	if err != nil || receipt.ObservationEpoch != 1 {
		t.Fatalf("cascade observation receipt=%+v err=%v", receipt, err)
	}
	if receipt.ObserverActivationEpoch != observationLease.ObserverActivationEpoch ||
		receipt.ArgoContract != argoLease.ContractVersion ||
		receipt.ArgoConfigDigest != argoLease.ConfigDigest ||
		receipt.ArgoWorkerID != argoLease.WorkerID ||
		receipt.ArgoWorkerEpoch != argoLease.Epoch ||
		!receipt.ArgoStartedAt.Equal(argoLease.StartedAt) ||
		!receipt.ArgoReadinessObservedAt.Equal(argoLease.ObservedAt) ||
		!receipt.ArgoReadinessLeaseUntil.Equal(argoLease.Until) {
		t.Fatalf("cascade receipt did not preserve DB-owned Argo activation tuple: receipt=%+v lease=%+v",
			receipt, argoLease)
	}
	if _, err = pool.Exec(ctx, `UPDATE public.helm_application_cascade_receipts
		SET argo_config_digest=$2,argo_worker_id=$3 WHERE id=$1`, receipt.ID,
		helmPGDigest([]byte("forged-argo-config")), "argo-desired-state-forged-0001"); err == nil {
		t.Fatal("cascade receipt accepted forged Argo worker/config tuple")
	}
	// A verified preflight survives a platform release. The current publisher
	// must append its own observation epoch; the older publisher receipt cannot
	// authorize a newly planned delete after that refresh.
	rotatedPublisher := publisher
	rotatedPublisher.ConfigDigest = helmPGDigest([]byte("rotated-cascade-publisher"))
	rotatedObserver := "helm-cascade-observer-0002"
	var rotationAt time.Time
	if err = pool.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&rotationAt); err != nil {
		t.Fatal(err)
	}
	argoLease, err = argoStore.HeartbeatDesiredStateReadiness(ctx, argoLease, rotationAt,
		4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var receiptExact bool
	if err = pool.QueryRow(ctx, `SELECT public.helm_application_cascade_observation_is_exact(
		$1,$2,$3)`, preflightID, publisher.ConfigDigest, rotationAt).Scan(&receiptExact); err != nil || !receiptExact {
		t.Fatalf("Argo readiness heartbeat invalidated exact cascade receipt: exact=%v err=%v",
			receiptExact, err)
	}
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{WorkerID: rotatedObserver,
		WorkerEpoch: 2, Publisher: rotatedPublisher, StartedAt: rotationAt.Add(-time.Second),
		ObservedAt: rotationAt, LeaseUntil: rotationAt.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, rotatedObserver, 2, rotatedPublisher, rotationAt); err != nil {
		t.Fatal(err)
	}
	rotatedPreflight, rotatedLease, err := store.ClaimCascadeObservation(ctx, rotatedObserver, 2,
		rotatedPublisher, rotationAt, time.Minute)
	if err != nil || rotatedPreflight.ID != preflightID {
		t.Fatalf("rotated cascade observation claim=%+v lease=%+v err=%v",
			rotatedPreflight, rotatedLease, err)
	}
	rotatedReceipt := receipt
	rotatedReceipt.ID = id.New()
	rotatedReceipt.ObservationEpoch = 1 // PostgreSQL assigns the next immutable epoch.
	rotatedReceipt.ObservationLeaseEpoch = rotatedLease.Epoch
	rotatedReceipt.ObserverActivationEpoch = rotatedLease.ObserverActivationEpoch
	rotatedReceipt.Publisher = rotatedPublisher
	rotatedReceipt.WorkerID = rotatedObserver
	rotatedReceipt.WorkerEpoch = 2
	rotatedReceipt.ObservedAt = rotationAt
	rotatedReceipt, err = store.RecordCascadeObservation(ctx, rotatedLease, rotatedReceipt, rotationAt)
	if err != nil || rotatedReceipt.ObservationEpoch != 2 {
		t.Fatalf("rotated cascade observation receipt=%+v err=%v", rotatedReceipt, err)
	}
	if staleCandidate, staleErr := store.NextApplicationCandidate(ctx, publisher); !errors.Is(staleErr, ErrNotFound) {
		t.Fatalf("old publisher retained delete authority: candidate=%+v err=%v", staleCandidate, staleErr)
	}
	if currentCandidate, currentErr := store.NextApplicationCandidate(ctx, rotatedPublisher); currentErr != nil ||
		currentCandidate.ReservedIntentID != deleteID || currentCandidate.PayloadIntentID != disablePayload.ID {
		t.Fatalf("current publisher delete candidate=%+v err=%v", currentCandidate, currentErr)
	}
	if _, err = pool.Exec(ctx, `UPDATE environments SET namespace=$2,argo_project=$3 WHERE id=$1`,
		f.environmentID, f.namespace, f.argoProject); err != nil {
		t.Fatal(err)
	}
	deleteAt := rotatedReceipt.ObservedAt.Add(time.Millisecond)

	deleted, replay, err := store.CreateApplicationForPayload(ctx, deleteID, disablePayload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, rotatedPublisher, deleteAt)
	if err != nil || replay || deleted.Action != ProtectedApplicationDelete ||
		deleted.Operation != "delete" || len(deleted.Content) != 0 || deleted.ContentDigest != "" {
		t.Fatalf("delete application=%+v replay=%v err=%v", deleted, replay, err)
	}
	var contentNull bool
	var contentLength int
	if err = pool.QueryRow(ctx, `SELECT content IS NULL,octet_length(content)
		FROM public.helm_protected_application_intents WHERE id=$1`, deleteID).
		Scan(&contentNull, &contentLength); err != nil || contentNull || contentLength != 0 {
		t.Fatalf("delete content null=%v length=%d err=%v", contentNull, contentLength, err)
	}
	replayedDelete, replay, err := store.CreateApplicationForPayload(ctx, deleteID, disablePayload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, rotatedPublisher, deleteAt.Add(time.Second))
	if err != nil || !replay || replayedDelete.ID != deleted.ID {
		t.Fatalf("delete replay=%+v replay=%v err=%v", replayedDelete, replay, err)
	}
	if _, _, err = store.CreateApplicationForPayload(ctx, id.New(), disablePayload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, rotatedPublisher,
		deleteAt.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate delete application was accepted: %v", err)
	}
	deleted, deleteLease, err := store.ClaimApplication(ctx, rotatedObserver, rotatedPublisher,
		deleteAt.Add(2*time.Second), time.Minute)
	if err != nil || deleted.ID != deleteID {
		t.Fatalf("claimed delete=%+v lease=%+v err=%v", deleted, deleteLease, err)
	}
	deleted, err = store.BindApplicationWriteBase(ctx, deleteLease, cascadeCommit,
		deleteAt.Add(3*time.Second), deleteAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deleteCommit := strings.Repeat("e", 40)
	deleted, err = store.MarkApplicationCommitted(ctx, deleteLease, deleteCommit,
		cascadeCommit, deleteAt.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deleted, err = store.VerifyApplication(ctx, deleteLease, deleteCommit, "",
		"disable-application", deleteAt.Add(5*time.Second))
	if err != nil || deleted.State != ProtectedVerified || deleted.VerifiedPathDigest != "" {
		t.Fatalf("verified delete=%+v err=%v", deleted, err)
	}
}

func TestPostgresProtectedPublisherActivationSerializesWithClaim(t *testing.T) {
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
	f := newHelmReleasePGFixture()
	f.now = helmPGDatabaseNow(t, ctx, pool).Add(-30 * time.Second)
	registerHelmAuthorityCleanup(t, pool, f.platformBindingID,
		"helm-publisher-race-", "argo-desired-state-race-")
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, f)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	releases, err := NewPostgresReleaseService(pool, helmPGOperatorDigest())
	if err != nil {
		t.Fatal(err)
	}
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID,
		ApplicationID: f.applicationID}
	release, _, err := releases.Upsert(ctx, UpsertReleaseRequest{
		Target: target, Approval: ApprovalKey{ID: f.approvalID, Revision: 1}, ValuesYAML: f.values,
		Actor: ReleaseActor{ID: f.userID, IdempotencyKey: "publisher-race-" + id.New(),
			RequestID: "publisher-race"},
	}, f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, renderTx, f, release.RenderCommandID, f.now.Add(2*time.Second))
	if err = renderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	authorityAt := helmPGDatabaseNow(t, ctx, pool)
	_, argoObservation := helmPGArgoObservation(t, f,
		"argo-desired-state-race-0001", authorityAt.Add(-5*time.Minute))
	store, err := NewPostgresProtectedPublicationStoreWithCascade(pool, helmPGArgoAuthority(),
		argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	argoObservation.ObservedAt = authorityAt
	if _, err = argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	oldPublisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest}
	oldWorker := "helm-publisher-race-old-0001"
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: oldWorker, WorkerEpoch: 1, Publisher: oldPublisher,
		StartedAt: authorityAt.Add(-4 * time.Minute), ObservedAt: authorityAt,
		LeaseUntil: authorityAt.Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, oldWorker, 1, oldPublisher, authorityAt); err != nil {
		t.Fatal(err)
	}
	binding := ProtectedBindingSnapshot{PlatformBindingID: f.platformBindingID,
		EnvironmentBindingID: f.environmentBindingID, ClusterID: f.clusterID,
		PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
		EnvironmentRevision: f.environmentHead, EnvironmentGeneration: 1,
		CatalogDigest: f.catalogDigest, PlannedBaseRevision: f.platformHead}
	payload, _, err := store.CreatePayloadForHead(ctx, id.New(), target, binding, oldPublisher,
		authorityAt)
	if err != nil {
		t.Fatal(err)
	}
	newPublisher := oldPublisher
	newPublisher.ConfigDigest = helmPGDigest([]byte("publisher-race-new"))
	newWorker := "helm-publisher-race-new-0002"
	newAuthorityAt := helmPGDatabaseNow(t, ctx, pool)
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: newWorker, WorkerEpoch: 2, Publisher: newPublisher,
		StartedAt: newAuthorityAt.Add(-time.Second), ObservedAt: newAuthorityAt,
		LeaseUntil: newAuthorityAt.Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(
		pg_catalog.hashtextextended($1,704215997))`, f.platformBindingID); err != nil {
		_ = blocker.Rollback(ctx)
		t.Fatal(err)
	}
	type claimResult struct {
		intent ProtectedPayloadIntent
		lease  ProtectedIntentLease
		err    error
	}
	claimDone := make(chan claimResult, 1)
	activateDone := make(chan error, 1)
	tracingAt := helmPGDatabaseNow(t, ctx, pool)
	go func() {
		intent, lease, claimErr := store.ClaimPayload(ctx, oldWorker, oldPublisher,
			tracingAt, minimumProtectedLease)
		claimDone <- claimResult{intent: intent, lease: lease, err: claimErr}
	}()
	waitForAdvisory := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			queryErr := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
				WHERE wait_event='advisory' AND pid<>pg_catalog.pg_backend_pid()`).Scan(&waiting)
			if queryErr != nil {
				t.Fatal(queryErr)
			}
			if waiting >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d advisory-lock waiter(s)", want)
	}
	waitForAdvisory(1)
	go func() {
		_, activateErr := store.ActivateCascadeObserver(ctx, newWorker, 2, newPublisher,
			tracingAt)
		activateDone <- activateErr
	}()
	waitForAdvisory(2)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	claimed := <-claimDone
	activatedErr := <-activateDone
	claimSucceeded := claimed.err == nil
	activationSucceeded := activatedErr == nil
	if claimSucceeded == activationSucceeded {
		t.Fatalf("claim/activation serialization allowed ambiguous outcome: claim=%+v activationErr=%v",
			claimed, activatedErr)
	}
	if claimSucceeded {
		if !errors.Is(activatedErr, ErrConflict) {
			t.Fatalf("activation did not reject admitted old lease: %v", activatedErr)
		}
		retryAt := helmPGNotBefore(helmPGDatabaseNow(t, ctx, pool), claimed.intent.UpdatedAt)
		if _, err = store.RetryPayload(ctx, claimed.lease, "provider-unavailable", retryAt,
			retryAt); err != nil {
			t.Fatal(err)
		}
		newAuthorityAt = helmPGDatabaseNow(t, ctx, pool)
		if _, err = store.ActivateCascadeObserver(ctx, newWorker, 2, newPublisher,
			newAuthorityAt); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(claimed.err, ErrConflict) && !errors.Is(claimed.err, ErrNotFound) {
		t.Fatalf("old claim failed unexpectedly after newer activation: %v", claimed.err)
	}
	if _, _, err = store.ClaimPayload(ctx, oldWorker, oldPublisher,
		helmPGDatabaseNow(t, ctx, pool), minimumProtectedLease); !errors.Is(err, ErrNotFound) &&
		!errors.Is(err, ErrConflict) {
		t.Fatalf("old publisher claimed after newer activation: payload=%s err=%v", payload.ID, err)
	}
}

func TestPostgresProtectedPublisherCrossReleaseAdoption(t *testing.T) {
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
	assertProtectedIntentValidatorsAreHardened(t, ctx, pool)
	f := newHelmReleasePGFixture()
	f.now = time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	registerHelmAuthorityCleanup(t, pool, f.platformBindingID,
		"helm-publisher-old-", "helm-publisher-current-", "argo-desired-state-adoption-")
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, setup, f)
	if err = setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	releases, err := NewPostgresReleaseService(pool, helmPGOperatorDigest())
	if err != nil {
		t.Fatal(err)
	}
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID}
	release, _, err := releases.Upsert(ctx, UpsertReleaseRequest{
		Target: target, Approval: ApprovalKey{ID: f.approvalID, Revision: 1}, ValuesYAML: f.values,
		Actor: ReleaseActor{ID: f.userID, IdempotencyKey: "publisher-adoption-" + id.New(), RequestID: "publisher-adoption"},
	}, f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, renderTx, f, release.RenderCommandID, f.now.Add(2*time.Second))
	if err = renderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	activationNow := helmPGDatabaseNow(t, ctx, pool)
	_, argoObservation := helmPGArgoObservation(t, f,
		"argo-desired-state-adoption-0001", activationNow.Add(-5*time.Minute))
	store, err := NewPostgresProtectedPublicationStoreWithCascade(pool, helmPGArgoAuthority(), argoObservation)
	if err != nil {
		t.Fatal(err)
	}
	original := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest}
	current := original
	current.ConfigDigest = helmPGDigest([]byte("publisher-next-release"))
	argoStore, err := argo.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	initialReadinessAt := activationNow.Add(-30 * time.Second)
	argoObservation.ObservedAt = initialReadinessAt
	argoLease, err := argoStore.AcquireDesiredStateReadiness(ctx, argoObservation, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	originalWorker := "helm-publisher-old-0001"
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: originalWorker, WorkerEpoch: 1, Publisher: original,
		StartedAt: activationNow.Add(-4 * time.Minute), ObservedAt: initialReadinessAt,
		LeaseUntil: initialReadinessAt.Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, originalWorker, 1, original, activationNow); err != nil {
		t.Fatal(err)
	}
	binding := ProtectedBindingSnapshot{
		PlatformBindingID: f.platformBindingID, EnvironmentBindingID: f.environmentBindingID,
		ClusterID: f.clusterID, PlatformTargetRef: "refs/heads/main",
		EnvironmentTargetRef: "refs/heads/main", EnvironmentRevision: f.environmentHead,
		EnvironmentGeneration: 1, CatalogDigest: f.catalogDigest, PlannedBaseRevision: f.platformHead,
	}
	payload, _, err := store.CreatePayloadForHead(ctx, id.New(), target, binding, original, f.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	payload, oldLease, err := store.ClaimPayload(ctx, originalWorker, original,
		activationNow.Add(-minimumProtectedLease), minimumProtectedLease)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = store.BindPayloadWriteBase(ctx, oldLease, f.platformHead,
		activationNow.Add(-14*time.Second), activationNow.Add(-14*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	// Readiness is bounded by database time. Use that same clock so a host clock
	// a few milliseconds ahead cannot create an otherwise valid future row.
	now := helmPGDatabaseNow(t, ctx, pool)
	currentWorker := "helm-publisher-current-0001"
	farFuture := now.Add(10 * time.Minute)
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: currentWorker, WorkerEpoch: 7, Publisher: current,
		StartedAt: now.Add(-time.Second), ObservedAt: farFuture,
		LeaseUntil: farFuture.Add(time.Minute),
	}); err == nil {
		t.Fatal("far-future protected publisher readiness was accepted")
	}
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: currentWorker, WorkerEpoch: 7, Publisher: current,
		StartedAt: now.Add(-2 * time.Minute), ObservedAt: now, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	argoLease, err = argoStore.HeartbeatDesiredStateReadiness(ctx, argoLease, now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, currentWorker, 7, current, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, originalWorker, 1, original,
		helmPGDatabaseNow(t, ctx, pool)); !errors.Is(err, ErrConflict) {
		t.Fatalf("older publisher process reactivated after monotonic advance: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE public.runtime_readiness
		SET updated_at=observed_at+interval '1 second'
		WHERE runtime_kind='helm-protected-publisher' AND scope_key='global' AND worker_id=$1`,
		currentWorker); err == nil {
		t.Fatal("publisher readiness accepted updated/observed timestamp divergence")
	}
	shadowTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = shadowTx.Exec(ctx, `
		CREATE TEMP TABLE runtime_readiness(dummy text);
		CREATE FUNCTION pg_temp.helm_protected_adoption_projection_is_fresh(
			uuid,uuid,uuid,uuid,uuid,text,text,text,bigint
		) RETURNS boolean LANGUAGE plpgsql AS $shadow$
		BEGIN RAISE EXCEPTION 'temporary projection function was invoked'; END
		$shadow$;`); err != nil {
		_ = shadowTx.Rollback(ctx)
		t.Fatal(err)
	}
	var shadowAdoptedID string
	// This call enters the payload validator through the adoption function's
	// pg_catalog,pg_temp search path. Migration 008 must keep every public
	// dependency exact even while matching temp objects exist.
	err = shadowTx.QueryRow(ctx, `SELECT public.adopt_helm_protected_payload_intent(
		$1,$2,$3,$4,$5,$6,$7)::text`, id.New(), currentWorker, 7, current.Contract,
		current.PolicyVersion, current.ConfigDigest, minimumProtectedLease.Milliseconds()).Scan(&shadowAdoptedID)
	if err != nil || shadowAdoptedID != payload.ID {
		_ = shadowTx.Rollback(ctx)
		t.Fatalf("schema-qualified shadow adoption id=%s err=%v", shadowAdoptedID, err)
	}
	if _, err = shadowTx.Exec(ctx, `DROP TABLE pg_temp.runtime_readiness;
		DROP FUNCTION pg_temp.helm_protected_adoption_projection_is_fresh(
			uuid,uuid,uuid,uuid,uuid,text,text,text,bigint
		)`); err != nil {
		_ = shadowTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = shadowTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	payload, err = store.Payload(ctx, payload.ID)
	adoptedLease := payloadLease(payload)
	if err != nil || payload.Publisher != current || payload.OriginalPublisherConfigDigest != original.ConfigDigest ||
		payload.PublisherAdoptionEpoch != 1 || adoptedLease.Epoch != oldLease.Epoch+1 ||
		payload.WriteBaseRevision != f.platformHead || payload.State != ProtectedClaimed {
		t.Fatalf("adopted payload=%+v lease=%+v err=%v", payload, adoptedLease, err)
	}
	replayedPayload, replay, err := store.CreatePayloadForHead(ctx, payload.ID, target, binding,
		original, helmPGNotBefore(time.Now().UTC(), payload.UpdatedAt))
	if err != nil || !replay || replayedPayload.ID != payload.ID || replayedPayload.Publisher != current ||
		replayedPayload.PublisherAdoptionEpoch != 1 {
		t.Fatalf("adopted payload replay=%+v replay=%v err=%v", replayedPayload, replay, err)
	}
	var payloadReceipts int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM helm_protected_publisher_adoption_receipts
		WHERE intent_kind='payload' AND payload_intent_id=$1 AND adoption_epoch=1
		AND original_config_digest=$2 AND previous_config_digest=$2 AND adopted_config_digest=$3
		AND intent_digest=$4 AND content_digest=$5 AND protected_path=$6
		AND precondition='create-if-absent' AND expected_etag=''
		AND write_base_revision=$7 AND committed_revision=''`, payload.ID, original.ConfigDigest,
		current.ConfigDigest, payload.IntentDigest, payload.ContentDigest, payload.Path,
		f.platformHead).Scan(&payloadReceipts); err != nil || payloadReceipts != 1 {
		t.Fatalf("payload adoption receipts=%d err=%v", payloadReceipts, err)
	}
	if _, _, err = store.ClaimPayload(ctx, "helm-publisher-old-0002", original,
		time.Now().UTC(), minimumProtectedLease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old publisher reclaimed adopted payload: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE helm_protected_payload_intents
		SET original_publisher_config_digest=$2,prerequisite_epoch=prerequisite_epoch+1,updated_at=clock_timestamp()
		WHERE id=$1`, payload.ID, current.ConfigDigest); err == nil {
		t.Fatal("original payload publisher identity was mutable")
	}

	payloadCommit := strings.Repeat("6", 40)
	payloadOperationAt := helmPGNotBefore(time.Now().UTC(), payload.UpdatedAt)
	payload, err = store.MarkPayloadCommitted(ctx, adoptedLease, payloadCommit, f.platformHead,
		payloadOperationAt)
	if err != nil {
		t.Fatal(err)
	}
	payloadOperationAt = helmPGNotBefore(time.Now().UTC(), payload.UpdatedAt)
	payload, err = store.VerifyPayload(ctx, adoptedLease, payloadCommit, payload.ContentDigest,
		"publisher-adoption-payload", payloadOperationAt)
	if err != nil {
		t.Fatal(err)
	}
	advanceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advancePlatformHead(t, ctx, advanceTx, f, payloadCommit,
		helmPGNotBefore(time.Now().UTC(), payload.UpdatedAt))
	if err = advanceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	application, _, err := store.CreateApplicationForPayload(ctx, id.New(), payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, original,
		helmPGNotBefore(time.Now().UTC(), payload.UpdatedAt))
	if err != nil {
		t.Fatal(err)
	}
	competing := newHelmReleasePGFixture()
	competing.platformBindingID = f.platformBindingID
	competing.clusterID = f.clusterID
	competing.platformHead = payloadCommit
	competingSetup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setupHelmReleasePGFixture(t, ctx, competingSetup, competing)
	if err = competingSetup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	competingTarget := ReleaseTarget{ProjectID: competing.projectID,
		EnvironmentID: competing.environmentID, ApplicationID: competing.applicationID}
	competingRelease, _, err := releases.Upsert(ctx, UpsertReleaseRequest{
		Target: competingTarget, Approval: ApprovalKey{ID: competing.approvalID, Revision: 1},
		ValuesYAML: competing.values, Actor: ReleaseActor{ID: competing.userID,
			IdempotencyKey: "publisher-competing-" + id.New(), RequestID: "publisher-competing"},
	}, competing.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	competingRender, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeHelmRender(t, ctx, competingRender, competing, competingRelease.RenderCommandID,
		competing.now.Add(2*time.Second))
	if err = competingRender.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	competingBinding := ProtectedBindingSnapshot{
		PlatformBindingID:    competing.platformBindingID,
		EnvironmentBindingID: competing.environmentBindingID, ClusterID: competing.clusterID,
		PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
		EnvironmentRevision: competing.environmentHead, EnvironmentGeneration: 1,
		CatalogDigest: competing.catalogDigest, PlannedBaseRevision: payloadCommit,
	}
	competingPayload, _, err := store.CreatePayloadForHead(ctx, id.New(), competingTarget,
		competingBinding, current, competing.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Even the shared schema owner cannot use a paired receipt+intent write to
	// bypass a route-authority change after the continuation receipt was
	// admitted: the authority UPDATE independently rechecks terminal authority.
	directBypass, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directReceiptID := id.New()
	_, err = directBypass.Exec(ctx, `INSERT INTO public.helm_protected_publisher_adoption_receipts(
		id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
		publisher_contract,original_config_digest,previous_config_digest,adopted_config_digest,
		policy_version,intent_digest,content_digest,protected_path,precondition,expected_etag,
		commit_trailer,prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
		recovery_state,write_base_revision,committed_revision,committed_parent_revision,
		previous_lease_epoch,adopted_lease_epoch,adopted_by_worker,adopted_worker_epoch,created_at)
		SELECT $1,'application',NULL,intent.id,intent.publisher_adoption_epoch+1,
		intent.publisher_contract,intent.original_publisher_config_digest,intent.publisher_config_digest,$2,
		$3,intent.intent_digest,intent.content_digest,intent.application_path,intent.precondition,
		intent.expected_etag,intent.commit_trailer,intent.prerequisite_receipt_id,
		intent.prerequisite_contract,intent.prerequisite_epoch,intent.state,intent.write_base_revision,
		intent.committed_revision,intent.committed_parent_revision,intent.lease_epoch,intent.lease_epoch+1,
		$4,$5,clock_timestamp() FROM public.helm_protected_application_intents intent WHERE intent.id=$6`,
		directReceiptID, current.ConfigDigest, current.PolicyVersion, currentWorker, 7, application.ID)
	if err != nil {
		_ = directBypass.Rollback(ctx)
		t.Fatalf("receipt-first projection setup: %v", err)
	}
	if _, err = directBypass.Exec(ctx, `UPDATE public.environments
		SET namespace='receipt-first-invalid',argo_project='receipt-first-invalid' WHERE id=$1`, f.environmentID); err != nil {
		_ = directBypass.Rollback(ctx)
		t.Fatal(err)
	}
	_, err = directBypass.Exec(ctx, `UPDATE public.helm_protected_application_intents intent SET
		publisher_config_digest=$2,publisher_adoption_epoch=intent.publisher_adoption_epoch+1,
		state='claimed',lease_owner=$3,lease_epoch=intent.lease_epoch+1,
		lease_until=receipt.created_at+interval '15 seconds',attempts=LEAST(intent.attempts+1,30),
		updated_at=receipt.created_at,prerequisite_epoch=intent.prerequisite_epoch+1
		FROM public.helm_protected_publisher_adoption_receipts receipt
		WHERE intent.id=$1 AND receipt.id=$4`, application.ID, current.ConfigDigest,
		currentWorker, directReceiptID)
	_ = directBypass.Rollback(ctx)
	if err == nil {
		t.Fatal("receipt-first paired DML bypassed the authority projection recheck")
	}

	// The authority UPDATE also owns the final cross-table lane check. A receipt
	// admitted first cannot be consumed after another payload takes that lane.
	laneBypass, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	laneReceiptID := id.New()
	_, err = laneBypass.Exec(ctx, `INSERT INTO public.helm_protected_publisher_adoption_receipts(
		id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
		publisher_contract,original_config_digest,previous_config_digest,adopted_config_digest,
		policy_version,intent_digest,content_digest,protected_path,precondition,expected_etag,
		commit_trailer,prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
		recovery_state,write_base_revision,committed_revision,committed_parent_revision,
		previous_lease_epoch,adopted_lease_epoch,adopted_by_worker,adopted_worker_epoch,created_at)
		SELECT $1,'application',NULL,intent.id,intent.publisher_adoption_epoch+1,
		intent.publisher_contract,intent.original_publisher_config_digest,intent.publisher_config_digest,$2,
		$3,intent.intent_digest,intent.content_digest,intent.application_path,intent.precondition,
		intent.expected_etag,intent.commit_trailer,intent.prerequisite_receipt_id,
		intent.prerequisite_contract,intent.prerequisite_epoch,intent.state,intent.write_base_revision,
		intent.committed_revision,intent.committed_parent_revision,intent.lease_epoch,intent.lease_epoch+1,
		$4,$5,clock_timestamp() FROM public.helm_protected_application_intents intent WHERE intent.id=$6`,
		laneReceiptID, current.ConfigDigest, current.PolicyVersion, currentWorker, 7, application.ID)
	if err != nil {
		_ = laneBypass.Rollback(ctx)
		t.Fatalf("receipt-first lane setup: %v", err)
	}
	claimAt := helmPGNotBefore(helmPGDatabaseNow(t, ctx, pool), competingPayload.UpdatedAt)
	if _, err = laneBypass.Exec(ctx, `UPDATE public.helm_protected_payload_intents SET
		state='claimed',lease_owner=$3,lease_epoch=lease_epoch+1,
		lease_until=$2::timestamptz+interval '1 minute',attempts=LEAST(attempts+1,30),
		updated_at=$2,prerequisite_epoch=prerequisite_epoch+1 WHERE id=$1`,
		competingPayload.ID, claimAt, currentWorker); err != nil {
		_ = laneBypass.Rollback(ctx)
		t.Fatalf("competing lane setup: %v", err)
	}
	_, err = laneBypass.Exec(ctx, `UPDATE public.helm_protected_application_intents intent SET
		publisher_config_digest=$2,publisher_adoption_epoch=intent.publisher_adoption_epoch+1,
		state='claimed',lease_owner=$3,lease_epoch=intent.lease_epoch+1,
		lease_until=receipt.created_at+interval '15 seconds',attempts=LEAST(intent.attempts+1,30),
		updated_at=receipt.created_at,prerequisite_epoch=intent.prerequisite_epoch+1
		FROM public.helm_protected_publisher_adoption_receipts receipt
		WHERE intent.id=$1 AND receipt.id=$4`, application.ID, current.ConfigDigest,
		currentWorker, laneReceiptID)
	_ = laneBypass.Rollback(ctx)
	if err == nil {
		t.Fatal("receipt-first paired DML bypassed the authority cross-table lane recheck")
	}
	// A receipt inserted without the same transaction's exact authority+lease
	// postimage must fail at the deferred constraint boundary.
	preplant, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = preplant.Exec(ctx, `INSERT INTO helm_protected_publisher_adoption_receipts(
		id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
		publisher_contract,original_config_digest,previous_config_digest,adopted_config_digest,
		policy_version,intent_digest,content_digest,protected_path,precondition,expected_etag,
		commit_trailer,prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
		recovery_state,write_base_revision,committed_revision,committed_parent_revision,
		previous_lease_epoch,adopted_lease_epoch,adopted_by_worker,adopted_worker_epoch,created_at)
		SELECT $1,'application',NULL,intent.id,intent.publisher_adoption_epoch+1,
		intent.publisher_contract,intent.original_publisher_config_digest,intent.publisher_config_digest,$2,
		$3,intent.intent_digest,intent.content_digest,intent.application_path,intent.precondition,
		intent.expected_etag,intent.commit_trailer,intent.prerequisite_receipt_id,
		intent.prerequisite_contract,intent.prerequisite_epoch,intent.state,intent.write_base_revision,
		intent.committed_revision,intent.committed_parent_revision,intent.lease_epoch,intent.lease_epoch+1,
		$4,$5,clock_timestamp() FROM helm_protected_application_intents intent WHERE intent.id=$6`,
		id.New(), current.ConfigDigest, current.PolicyVersion, currentWorker, 7, application.ID)
	if err != nil {
		_ = preplant.Rollback(ctx)
		t.Fatalf("preplant setup should reach deferred postimage fence: %v", err)
	}
	if err = preplant.Commit(ctx); err == nil {
		t.Fatal("preplanted adoption receipt committed without atomic intent postimage")
	}

	// The Application adoption function has the same hardened nested-trigger
	// path and must succeed without ambient public resolution.
	application, appLease, err := store.AdoptApplication(ctx, currentWorker, 7, current, minimumProtectedLease)
	if err != nil || application.Publisher != current ||
		application.OriginalPublisherConfigDigest != original.ConfigDigest ||
		application.PublisherAdoptionEpoch != 1 || application.State != ProtectedClaimed {
		t.Fatalf("adopted application=%+v lease=%+v err=%v", application, appLease, err)
	}
	replayedApplication, replay, err := store.CreateApplicationForPayload(ctx, application.ID,
		payload.ID, ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, original,
		helmPGNotBefore(time.Now().UTC(), application.UpdatedAt))
	if err != nil || !replay || replayedApplication.ID != application.ID ||
		replayedApplication.Publisher != current || replayedApplication.PublisherAdoptionEpoch != 1 {
		t.Fatalf("adopted application replay=%+v replay=%v err=%v", replayedApplication, replay, err)
	}
	third := current
	// A genuinely newer process may deliberately roll back to the original
	// release config. The old original process remains fenced by its start time.
	third.ConfigDigest = original.ConfigDigest
	thirdWorker := "helm-publisher-current-0002"
	now = helmPGNotBefore(helmPGDatabaseNow(t, ctx, pool), application.UpdatedAt)
	application, err = store.RetryApplication(ctx, appLease, "provider-unavailable",
		now, now)
	if err != nil || application.State != ProtectedPending {
		t.Fatalf("application retry=%+v err=%v", application, err)
	}
	if err = store.PutPublisherReadiness(ctx, ProtectedPublisherReadiness{
		WorkerID: thirdWorker, WorkerEpoch: 11, Publisher: third,
		StartedAt: now.Add(-time.Second), ObservedAt: now, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	argoLease, err = argoStore.HeartbeatDesiredStateReadiness(ctx, argoLease, now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivateCascadeObserver(ctx, thirdWorker, 11, third, now); err != nil {
		t.Fatal(err)
	}
	application, chainedLease, err := store.AdoptApplication(ctx, thirdWorker, 11, third, minimumProtectedLease)
	if err != nil || application.Publisher != third ||
		application.OriginalPublisherConfigDigest != original.ConfigDigest ||
		application.PublisherAdoptionEpoch != 2 || chainedLease.Epoch != appLease.Epoch+1 {
		t.Fatalf("chained application=%+v lease=%+v err=%v", application, chainedLease, err)
	}
	var chainReceipts int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM helm_protected_publisher_adoption_receipts
		WHERE application_intent_id=$1 AND
		((adoption_epoch=1 AND previous_config_digest=$2 AND adopted_config_digest=$3) OR
		 (adoption_epoch=2 AND previous_config_digest=$3 AND adopted_config_digest=$4))`,
		application.ID, original.ConfigDigest, current.ConfigDigest, third.ConfigDigest).Scan(&chainReceipts); err != nil || chainReceipts != 2 {
		t.Fatalf("application adoption chain receipts=%d err=%v", chainReceipts, err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM helm_protected_publisher_adoption_receipts
		WHERE application_intent_id=$1 AND adoption_epoch=1`, application.ID); err == nil {
		t.Fatal("immutable publisher adoption receipt was deleted")
	}
}

func TestPostgresHelmPublicationPrerequisiteAdmission(t *testing.T) {
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	f := newHelmReleasePGFixture()
	setupHelmReleasePGFixture(t, ctx, tx, f)
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID}
	requestDigest, err := releaseRequestDigest("upsert", target,
		ApprovalKey{ID: f.approvalID, Revision: 1}, f.valuesDigest, "")
	if err != nil {
		t.Fatal(err)
	}
	release, replay, err := (&PostgresReleaseService{operatorConfigDigest: helmPGOperatorDigest()}).mutateTx(ctx, tx,
		releaseMutation{kind: "upsert", target: target,
			actor:    ReleaseActor{ID: f.userID, IdempotencyKey: "prerequisite-" + id.New(), RequestID: "prerequisite"},
			approval: ApprovalKey{ID: f.approvalID, Revision: 1}, values: f.values, requestDigest: requestDigest},
		f.now.Add(time.Second))
	if err != nil || replay {
		t.Fatalf("release=%+v replay=%v err=%v", release, replay, err)
	}
	completeHelmRender(t, ctx, tx, f, release.RenderCommandID, f.now.Add(2*time.Second))
	binding := ProtectedBindingSnapshot{
		PlatformBindingID: f.platformBindingID, EnvironmentBindingID: f.environmentBindingID,
		ClusterID: f.clusterID, PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
		EnvironmentRevision: f.environmentHead, EnvironmentGeneration: 1,
		CatalogDigest: f.catalogDigest, PlannedBaseRevision: f.platformHead,
	}
	payloadFixture := helmPayloadInsert{id: id.New(), releaseID: release.ID,
		generation: release.Generation, action: "publish", path: helmPayloadPath(f, release.ID, false),
		content: f.manifest, contentDigest: f.manifestDigest,
		inventoryDigest: f.inventoryDigest, resourceCount: 1}

	t.Run("no dummy Deployment", func(t *testing.T) {
		var dummy bool
		if err := tx.QueryRow(ctx, `SELECT position('kind: Deployment' in convert_from(manifest,'UTF8'))>0
			FROM environment_foundation_intents WHERE id=$1`, f.foundationIntentID).Scan(&dummy); err != nil {
			t.Fatal(err)
		}
		if dummy {
			t.Fatal("foundation prerequisite used a dummy Deployment")
		}
		var deployments int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deployments
			WHERE environment_id=$1 AND application_id=$2`, f.environmentID, f.applicationID).Scan(&deployments); err != nil {
			t.Fatal(err)
		}
		if deployments != 0 {
			t.Fatalf("prerequisite admission created %d dummy deployments", deployments)
		}
	})

	t.Run("missing foundation", func(t *testing.T) {
		nested, beginErr := tx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = nested.Rollback(ctx) }()
		if _, updateErr := nested.Exec(ctx, `UPDATE environment_foundation_intents
			SET state='superseded',active=false,updated_at=$2 WHERE id=$1`,
			f.foundationIntentID, f.now.Add(3*time.Second)); updateErr != nil {
			t.Fatal(updateErr)
		}
		if _, gateErr := ensurePublicationPrerequisite(ctx, nested, release, binding, helmPGArgoAuthority(), f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrFoundationNotReady) {
			t.Fatalf("missing foundation error=%v", gateErr)
		}
	})

	t.Run("unverified Argo project", func(t *testing.T) {
		nested, beginErr := tx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = nested.Rollback(ctx) }()
		if _, updateErr := nested.Exec(ctx, `ALTER TABLE argo_desired_state_commands
			DISABLE TRIGGER argo_desired_state_commands_validate`); updateErr != nil {
			t.Fatal(updateErr)
		}
		if _, updateErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands SET
			state='pending',write_base_revision='',write_base_observed_at=NULL,
			committed_revision='',committed_at=NULL,verified_at=NULL,completed_at=NULL,
			updated_at=$2 WHERE id=$1`, f.desiredStateCommandID, f.now.Add(3*time.Second)); updateErr != nil {
			t.Fatal(updateErr)
		}
		if _, gateErr := ensurePublicationPrerequisite(ctx, nested, release, binding, helmPGArgoAuthority(), f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("unverified Argo project error=%v", gateErr)
		}
	})

	t.Run("missing Argo project", func(t *testing.T) {
		nested, beginErr := tx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = nested.Rollback(ctx) }()
		if _, deleteErr := nested.Exec(ctx, `ALTER TABLE argo_desired_state_materialization_receipts
			DISABLE TRIGGER argo_desired_state_materialization_receipts_validate`); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		if _, deleteErr := nested.Exec(ctx, `ALTER TABLE argo_desired_state_materialization_receipts
			DISABLE TRIGGER argo_materialization_app_project_content_validate`); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		if _, deleteErr := nested.Exec(ctx, `DELETE FROM argo_desired_state_materialization_receipts
			WHERE desired_state_command_id=$1`, f.desiredStateCommandID); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		if _, deleteErr := nested.Exec(ctx, `DELETE FROM argo_desired_state_commands WHERE id=$1`,
			f.desiredStateCommandID); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		if _, gateErr := ensurePublicationPrerequisite(ctx, nested, release, binding, helmPGArgoAuthority(), f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("missing Argo project error=%v", gateErr)
		}
	})

	t.Run("stale Argo project", func(t *testing.T) {
		nested, beginErr := tx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = nested.Rollback(ctx) }()
		stale := binding
		stale.EnvironmentRevision, stale.EnvironmentGeneration = strings.Repeat("2", 40), 2
		if _, gateErr := ensurePublicationPrerequisite(ctx, nested, release, stale, helmPGArgoAuthority(), f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("stale Argo project error=%v", gateErr)
		}
	})

	t.Run("rotated Argo policy requires fresh materialization", func(t *testing.T) {
		rotated := helmPGArgoAuthority()
		rotated.PolicyDigest = helmPGDigest([]byte("rotated-argo-materialization-policy"))
		if _, gateErr := ensurePublicationPrerequisite(ctx, tx, release, binding, rotated,
			f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("stale policy materialization error=%v", gateErr)
		}
	})

	t.Run("rotated Argo runtime requires fresh materialization", func(t *testing.T) {
		rotated := helmPGArgoAuthority()
		rotated.Runtime.ChartDigest = helmPGDigest([]byte("rotated-runtime-chart"))
		if _, gateErr := ensurePublicationPrerequisite(ctx, tx, release, binding, rotated,
			f.now.Add(4*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("stale runtime materialization error=%v", gateErr)
		}
	})

	t.Run("verified Argo policy digest is immutable", func(t *testing.T) {
		expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
			_, nestedErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands
				SET policy_digest=$2 WHERE id=$1`, f.desiredStateCommandID,
				helmPGDigest([]byte("mutated-verified-policy")))
			return nestedErr
		})
	})

	t.Run("unchanged AppProject survives unrelated branch advance", func(t *testing.T) {
		nested, beginErr := tx.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = nested.Rollback(ctx) }()
		advanced := binding
		advanced.EnvironmentRevision, advanced.EnvironmentGeneration = strings.Repeat("2", 40), 2
		if _, updateErr := nested.Exec(ctx, `INSERT INTO git_projection_generations(
			binding_id,generation,head_revision,parser_version,state,started_at,activated_at
		) VALUES($1,$2,$3,'gitprojection.v1','active',$4,$4)`, f.environmentBindingID,
			advanced.EnvironmentGeneration, advanced.EnvironmentRevision, f.now.Add(4*time.Second)); updateErr != nil {
			t.Fatal(updateErr)
		}
		if _, updateErr := nested.Exec(ctx, `UPDATE git_repository_bindings SET
			target_head_revision=$2,indexed_revision=$2,projection_generation=$3,
			target_head_observed_at=$4,indexed_at=$4,updated_at=$4 WHERE id=$1`,
			f.environmentBindingID, advanced.EnvironmentRevision, advanced.EnvironmentGeneration,
			f.now.Add(4*time.Second)); updateErr != nil {
			t.Fatal(updateErr)
		}
		if _, gateErr := ensurePublicationPrerequisite(ctx, nested, release, advanced, helmPGArgoAuthority(),
			f.now.Add(5*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
			t.Fatalf("pre-materializer branch advance error=%v", gateErr)
		}
		insertArgoMaterializationReceipt(t, ctx, nested, f, id.New(),
			advanced.EnvironmentRevision, advanced.EnvironmentGeneration,
			f.desiredStateCommandID, f.now.Add(5*time.Second))
		receipt, gateErr := ensurePublicationPrerequisite(ctx, nested, release, advanced, helmPGArgoAuthority(), f.now.Add(5*time.Second))
		if gateErr != nil || receipt.EnvironmentRevision != advanced.EnvironmentRevision ||
			receipt.EnvironmentGeneration != advanced.EnvironmentGeneration ||
			receipt.DesiredStateCommandID != f.desiredStateCommandID ||
			receipt.DesiredStateRevision != f.desiredStateRevision {
			t.Fatalf("unchanged AppProject receipt=%+v err=%v", receipt, gateErr)
		}
		replayed, replayErr := ensurePublicationPrerequisite(ctx, nested, release, advanced, helmPGArgoAuthority(),
			f.now.Add(6*time.Second))
		if replayErr != nil || replayed != receipt {
			t.Fatalf("unchanged AppProject replay=%+v want=%+v err=%v", replayed, receipt, replayErr)
		}
		if readback, readErr := publicationPrerequisite(ctx, nested, release.ID); readErr != nil || readback != receipt {
			t.Fatalf("unchanged AppProject readback=%+v want=%+v err=%v", readback, receipt, readErr)
		}
	})

	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	pendingDesiredStateID := id.New()
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
				'id',$2::text,'generation',2,'state','pending',
				'write_base_revision','','write_base_observed_at',NULL,
				'committed_revision','','committed_at',NULL,'verified_at',NULL,
				'completed_at',NULL,'next_attempt_at',$3::timestamptz,
				'created_at',$3::timestamptz,'updated_at',$3::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		f.desiredStateCommandID, pendingDesiredStateID, f.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands
			SET policy_digest=$2 WHERE id=$1`, pendingDesiredStateID,
			helmPGDigest([]byte("mutated-live-policy")))
		return nestedErr
	})
	if _, gateErr := ensurePublicationPrerequisite(ctx, tx, release, binding, helmPGArgoAuthority(),
		f.now.Add(5*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
		t.Fatalf("newer live Argo authority error=%v", gateErr)
	}
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET
		state='failed',consecutive_failures=1,last_failure_code='materialization-test',
		completed_at=$2,updated_at=$2 WHERE id=$1`, pendingDesiredStateID,
		f.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, gateErr := ensurePublicationPrerequisite(ctx, tx, release, binding, helmPGArgoAuthority(),
		f.now.Add(6*time.Second)); !errors.Is(gateErr, ErrArgoProjectNotReady) {
		t.Fatalf("newer failed Argo authority without fresh proof error=%v", gateErr)
	}
	insertArgoMaterializationReceipt(t, ctx, tx, f, id.New(), binding.EnvironmentRevision,
		binding.EnvironmentGeneration, f.desiredStateCommandID,
		f.now.Add(7*time.Second))

	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertHelmPayloadRow(ctx, nested, f, payloadFixture, f.now.Add(7*time.Second))
	})

	receipt, err := ensurePublicationPrerequisite(ctx, tx, release, binding, helmPGArgoAuthority(), f.now.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ensurePublicationPrerequisite(ctx, tx, release, binding, helmPGArgoAuthority(), f.now.Add(9*time.Second))
	if err != nil || replayed != receipt {
		t.Fatalf("receipt replay=%+v want=%+v err=%v", replayed, receipt, err)
	}
	if _, err = publicationPrerequisite(ctx, tx, release.ID); err != nil {
		t.Fatalf("exact receipt was not readable: %v", err)
	}
	if err = insertHelmPayloadRow(ctx, tx, f, payloadFixture, f.now.Add(9*time.Second)); err != nil {
		t.Fatalf("new writer receipt plus intent was rejected: %v", err)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_publication_prerequisite_receipts
			SET desired_state_revision=$2 WHERE release_revision_id=$1`, release.ID, strings.Repeat("7", 40))
		return nestedErr
	})

	if _, err = tx.Exec(ctx, `UPDATE environment_foundation_intents
		SET state='superseded',active=false,updated_at=$2 WHERE id=$1`,
		f.foundationIntentID, f.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	advancedEnvironmentRevision := strings.Repeat("2", 40)
	if _, err = tx.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at
	) VALUES($1,2,$2,'gitprojection.v1','active',$3,$3)`, f.environmentBindingID,
		advancedEnvironmentRevision, f.now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE git_repository_bindings SET
		target_head_revision=$2,indexed_revision=$2,projection_generation=2,
		target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
		f.environmentBindingID, advancedEnvironmentRevision, f.now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	laterDesiredStateID, laterDesiredStateRevision := id.New(), strings.Repeat("3", 40)
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
				'id',$2::text,'generation',3,'environment_revision',$3::text,
				'environment_generation',2,'base_revision',$4::text,
				'write_base_revision',$4::text,'committed_revision',$5::text,
				'write_base_observed_at',$6::timestamptz,'committed_at',$6::timestamptz,
				'verified_at',$6::timestamptz,'next_attempt_at',$6::timestamptz,
				'created_at',$6::timestamptz,'updated_at',$6::timestamptz,
				'completed_at',$6::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		f.desiredStateCommandID, laterDesiredStateID, advancedEnvironmentRevision,
		f.desiredStateRevision, laterDesiredStateRevision, f.now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	if got, readErr := publicationPrerequisite(ctx, tx, release.ID); readErr != nil || got != receipt {
		t.Fatalf("ordinary foundation/projection/AppProject advance stranded receipt: got=%+v err=%v", got, readErr)
	}
	phaseTwoBinding := binding
	phaseTwoBinding.PlannedBaseRevision = strings.Repeat("4", 40)
	if got, replayErr := ensurePublicationPrerequisite(ctx, tx, release, phaseTwoBinding, helmPGArgoAuthority(),
		f.now.Add(10*time.Second)); replayErr != nil || got != receipt {
		t.Fatalf("phase-local base advance stranded receipt replay: got=%+v err=%v", got, replayErr)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET argo_project='identity-mismatch'
		WHERE id=$1`, f.desiredStateCommandID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	if _, err = publicationPrerequisite(ctx, tx, release.ID); !errors.Is(err, ErrArgoProjectNotReady) {
		t.Fatalf("Argo project identity mismatch was accepted: %v", err)
	}
}

func TestPostgresHelmPublicationPrerequisiteUpgrade(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool := openFreshHelmMigrationPool(t, ctx, databaseURL)
	var err error
	if err = applyHelmMigrationsThrough(ctx, pool, "003_repair_protected_desired_revisions"); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	createRelease := func(f helmReleasePGFixture, suffix string) ReleaseRevision {
		t.Helper()
		setupHelmReleasePGFixture(t, ctx, tx, f)
		target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID}
		digest, digestErr := releaseRequestDigest("upsert", target,
			ApprovalKey{ID: f.approvalID, Revision: 1}, f.valuesDigest, "")
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		release, replay, mutateErr := (&PostgresReleaseService{operatorConfigDigest: helmPGOperatorDigest()}).mutateTx(
			ctx, tx, releaseMutation{kind: "upsert", target: target,
				actor:    ReleaseActor{ID: f.userID, IdempotencyKey: "upgrade-" + suffix + "-" + id.New(), RequestID: "upgrade-" + suffix},
				approval: ApprovalKey{ID: f.approvalID, Revision: 1}, values: f.values, requestDigest: digest},
			f.now.Add(time.Second))
		if mutateErr != nil || replay {
			t.Fatalf("upgrade release=%+v replay=%v err=%v", release, replay, mutateErr)
		}
		completeHelmRender(t, ctx, tx, f, release.RenderCommandID, f.now.Add(2*time.Second))
		return release
	}

	exact := newHelmReleasePGFixture()
	exactRelease := createRelease(exact, "exact")
	exactPayload := helmPayloadInsert{id: id.New(), releaseID: exactRelease.ID,
		generation: exactRelease.Generation, action: "publish", path: helmPayloadPath(exact, exactRelease.ID, false),
		content: exact.manifest, contentDigest: exact.manifestDigest,
		inventoryDigest: exact.inventoryDigest, resourceCount: 1}
	if err = insertLegacyHelmPayloadRow(ctx, tx, exact, exactPayload, exact.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", exactPayload.id, exact.now.Add(4*time.Second))
	exactCommit := strings.Repeat("6", 40)
	commitLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", exactPayload.id,
		exact.platformHead, exactCommit, exact.now.Add(5*time.Second))
	verifyLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", exactPayload.id,
		exact.manifestDigest, exact.now.Add(6*time.Second))
	legacyApplication := helmApplicationInsert{id: id.New(), releaseID: exactRelease.ID,
		payloadID: exactPayload.id, generation: exactRelease.Generation, action: "publish",
		payloadRevision: exactCommit, payloadPath: exactPayload.path,
		sourceDirectory: helmSourceDirectory(exact, exactRelease.ID), applicationPath: helmApplicationPath(exact),
		operation: "create", precondition: "create-if-absent", content: []byte("legacy application\n"),
		contentDigest: helmPGDigest([]byte("legacy application\n"))}
	if err = insertLegacyHelmApplication(ctx, tx, exact, legacyApplication, exact.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimLegacyHelmIntent(t, ctx, tx, "helm_protected_application_intents", legacyApplication.id,
		exact.now.Add(8*time.Second))
	// Simulate the old publisher pushing the stable Application path and losing
	// its database acknowledgement. The repository observer can advance the
	// platform head while the legacy intent remains claimed.
	claimedApplicationRevision := strings.Repeat("d", 40)
	advancePlatformHead(t, ctx, tx, exact, claimedApplicationRevision, exact.now.Add(9*time.Second))

	committed := newHelmReleasePGFixture()
	committedRelease := createRelease(committed, "committed-application")
	committedPayload := helmPayloadInsert{id: id.New(), releaseID: committedRelease.ID,
		generation: committedRelease.Generation, action: "publish", path: helmPayloadPath(committed, committedRelease.ID, false),
		content: committed.manifest, contentDigest: committed.manifestDigest,
		inventoryDigest: committed.inventoryDigest, resourceCount: 1}
	if err = insertLegacyHelmPayloadRow(ctx, tx, committed, committedPayload, committed.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", committedPayload.id, committed.now.Add(4*time.Second))
	committedPayloadRevision := strings.Repeat("c", 40)
	commitLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", committedPayload.id,
		committed.platformHead, committedPayloadRevision, committed.now.Add(5*time.Second))
	verifyLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", committedPayload.id,
		committed.manifestDigest, committed.now.Add(6*time.Second))
	committedApplication := helmApplicationInsert{id: id.New(), releaseID: committedRelease.ID,
		payloadID: committedPayload.id, generation: committedRelease.Generation, action: "publish",
		payloadRevision: committedPayloadRevision, payloadPath: committedPayload.path,
		sourceDirectory: helmSourceDirectory(committed, committedRelease.ID), applicationPath: helmApplicationPath(committed),
		operation: "create", precondition: "create-if-absent", content: []byte("committed legacy application\n"),
		contentDigest: helmPGDigest([]byte("committed legacy application\n"))}
	if err = insertLegacyHelmApplication(ctx, tx, committed, committedApplication, committed.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimLegacyHelmIntent(t, ctx, tx, "helm_protected_application_intents", committedApplication.id,
		committed.now.Add(8*time.Second))
	committedApplicationRevision := strings.Repeat("b", 40)
	commitLegacyHelmIntent(t, ctx, tx, "helm_protected_application_intents", committedApplication.id,
		committed.platformHead, committedApplicationRevision, committed.now.Add(9*time.Second))
	advancePlatformHead(t, ctx, tx, committed, committedApplicationRevision, committed.now.Add(10*time.Second))

	diverged := newHelmReleasePGFixture()
	divergedRelease := createRelease(diverged, "diverged")
	divergedPayload := helmPayloadInsert{id: id.New(), releaseID: divergedRelease.ID,
		generation: divergedRelease.Generation, action: "publish", path: helmPayloadPath(diverged, divergedRelease.ID, false),
		content: diverged.manifest, contentDigest: diverged.manifestDigest,
		inventoryDigest: diverged.inventoryDigest, resourceCount: 1}
	if err = insertLegacyHelmPayloadRow(ctx, tx, diverged, divergedPayload, diverged.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", divergedPayload.id, diverged.now.Add(4*time.Second))
	divergedCommit := strings.Repeat("7", 40)
	commitLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", divergedPayload.id,
		diverged.platformHead, divergedCommit, diverged.now.Add(5*time.Second))
	verifyLegacyHelmIntent(t, ctx, tx, "helm_protected_payload_intents", divergedPayload.id,
		diverged.manifestDigest, diverged.now.Add(6*time.Second))
	if _, err = tx.Exec(ctx, `UPDATE git_repository_bindings SET state='diverged',updated_at=$2
		WHERE id=$1`, diverged.platformBindingID, diverged.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	ambiguous := newHelmReleasePGFixture()
	ambiguousRelease := createRelease(ambiguous, "ambiguous")
	ambiguousPayload := helmPayloadInsert{id: id.New(), releaseID: ambiguousRelease.ID,
		generation: ambiguousRelease.Generation, action: "publish", path: helmPayloadPath(ambiguous, ambiguousRelease.ID, false),
		content: ambiguous.manifest, contentDigest: ambiguous.manifestDigest,
		inventoryDigest: ambiguous.inventoryDigest, resourceCount: 1}
	if err = insertLegacyHelmPayloadRow(ctx, tx, ambiguous, ambiguousPayload, ambiguous.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM argo_desired_state_commands WHERE id=$1`, ambiguous.desiredStateCommandID); err != nil {
		t.Fatal(err)
	}

	oldWriter := newHelmReleasePGFixture()
	oldWriterRelease := createRelease(oldWriter, "old-writer")
	if _, err = tx.Exec(ctx, `DELETE FROM argo_desired_state_commands WHERE id=$1`, oldWriter.desiredStateCommandID); err != nil {
		t.Fatal(err)
	}
	oldWriterPayload := helmPayloadInsert{id: id.New(), releaseID: oldWriterRelease.ID,
		generation: oldWriterRelease.Generation, action: "publish", path: helmPayloadPath(oldWriter, oldWriterRelease.ID, false),
		content: oldWriter.manifest, contentDigest: oldWriter.manifestDigest,
		inventoryDigest: oldWriter.inventoryDigest, resourceCount: 1}
	legacyPending, legacyEmptyClaim, legacyRecovery, legacyPendingRecovery := newHelmReleasePGFixture(),
		newHelmReleasePGFixture(), newHelmReleasePGFixture(), newHelmReleasePGFixture()
	setupHelmReleasePGFixture(t, ctx, tx, legacyPending)
	setupHelmReleasePGFixture(t, ctx, tx, legacyEmptyClaim)
	setupHelmReleasePGFixture(t, ctx, tx, legacyRecovery)
	setupHelmReleasePGFixture(t, ctx, tx, legacyPendingRecovery)

	body, err := migrations.FS.ReadFile("prisma/migrations/004_helm_publication_prerequisites/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var exactState, legacyApplicationState, legacyApplicationFailure string
	var legacyApplicationReceipt, legacyApplicationContract string
	var legacyApplicationEpoch int64
	var committedApplicationState, committedApplicationReceipt, committedApplicationContract string
	var committedApplicationEpoch int64
	var divergedState, ambiguousState, ambiguousFailure string
	var exactReceipts, divergedReceipts, ambiguousReceipts int
	var exactReceiptBase, committedReceiptBase string
	if err = tx.QueryRow(ctx, `SELECT state FROM helm_protected_payload_intents WHERE id=$1`, exactPayload.id).Scan(&exactState); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state,last_failure_code,prerequisite_receipt_id::text,
		prerequisite_contract,prerequisite_epoch FROM helm_protected_application_intents WHERE id=$1`,
		legacyApplication.id).Scan(&legacyApplicationState, &legacyApplicationFailure,
		&legacyApplicationReceipt, &legacyApplicationContract, &legacyApplicationEpoch); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state,prerequisite_receipt_id::text,
		prerequisite_contract,prerequisite_epoch FROM helm_protected_application_intents WHERE id=$1`,
		committedApplication.id).Scan(&committedApplicationState, &committedApplicationReceipt,
		&committedApplicationContract, &committedApplicationEpoch); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state,last_failure_code FROM helm_protected_payload_intents WHERE id=$1`,
		ambiguousPayload.id).Scan(&ambiguousState, &ambiguousFailure); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state FROM helm_protected_payload_intents WHERE id=$1`,
		divergedPayload.id).Scan(&divergedState); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, exactRelease.ID).Scan(&exactReceipts); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT planned_base_revision FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, exactRelease.ID).Scan(&exactReceiptBase); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT planned_base_revision FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, committedRelease.ID).Scan(&committedReceiptBase); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, ambiguousRelease.ID).Scan(&ambiguousReceipts); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM helm_publication_prerequisite_receipts
		WHERE release_revision_id=$1`, divergedRelease.ID).Scan(&divergedReceipts); err != nil {
		t.Fatal(err)
	}
	if exactState != string(ProtectedVerified) || exactReceipts != 1 ||
		exactReceiptBase != claimedApplicationRevision || committedReceiptBase != committedApplicationRevision {
		t.Fatalf("exact legacy terminal state=%s receipts=%d claimed_base=%s committed_base=%s",
			exactState, exactReceipts, exactReceiptBase, committedReceiptBase)
	}
	if legacyApplicationState != string(ProtectedClaimed) || legacyApplicationFailure != "" ||
		legacyApplicationReceipt != exactRelease.ID ||
		legacyApplicationContract != protectedPrerequisiteContract || legacyApplicationEpoch != 0 {
		t.Fatalf("legacy live application state=%s failure=%s receipt=%s contract=%s epoch=%d",
			legacyApplicationState, legacyApplicationFailure, legacyApplicationReceipt,
			legacyApplicationContract, legacyApplicationEpoch)
	}
	if committedApplicationState != string(ProtectedGitCommitted) ||
		committedApplicationReceipt != committedRelease.ID ||
		committedApplicationContract != protectedPrerequisiteContract || committedApplicationEpoch != 0 {
		t.Fatalf("committed legacy application state=%s receipt=%s contract=%s epoch=%d",
			committedApplicationState, committedApplicationReceipt,
			committedApplicationContract, committedApplicationEpoch)
	}
	if ambiguousState != string(ProtectedSuperseded) ||
		ambiguousFailure != "publication-prerequisite-missing" || ambiguousReceipts != 0 {
		t.Fatalf("ambiguous legacy live state=%s failure=%s receipts=%d",
			ambiguousState, ambiguousFailure, ambiguousReceipts)
	}
	if divergedState != string(ProtectedVerified) || divergedReceipts != 0 {
		t.Fatalf("diverged legacy terminal state=%s receipts=%d", divergedState, divergedReceipts)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_payload_intents SET
			state='claimed',attempts=attempts+1,lease_owner='old-rc-worker',lease_epoch=lease_epoch+1,
			lease_until=$2::timestamptz+interval '5 minutes',updated_at=$2 WHERE id=$1`,
			ambiguousPayload.id, ambiguous.now.Add(8*time.Second))
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_application_intents SET
			state='verified',lease_owner=NULL,lease_until=NULL,verified_at=$2,
			verified_path_digest=content_digest,provider_request='old-worker-verify',
			completed_at=$2,updated_at=$2 WHERE id=$1`, committedApplication.id,
			committed.now.Add(10*time.Second))
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_application_intents SET
			state='claimed',attempts=attempts+1,lease_owner='old-rc-worker',lease_epoch=lease_epoch+1,
			lease_until=$2::timestamptz+interval '5 minutes',updated_at=$2 WHERE id=$1`,
			legacyApplication.id, exact.now.Add(10*time.Second))
		return nestedErr
	})
	if _, err = tx.Exec(ctx, `UPDATE helm_protected_application_intents SET
		state='claimed',attempts=attempts+1,lease_owner='helm-publisher-worker-0004',lease_epoch=lease_epoch+1,
		lease_until=$2::timestamptz+interval '5 minutes',updated_at=$2,
		prerequisite_epoch=prerequisite_epoch+1 WHERE id=$1`,
		legacyApplication.id, exact.now.Add(10*time.Minute)); err != nil {
		t.Fatalf("v004 worker could not adopt exact legacy application: %v", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE helm_protected_application_intents SET
		lease_owner='helm-publisher-worker-0005',lease_epoch=lease_epoch+1,
		lease_until=$2::timestamptz+interval '5 minutes',attempts=attempts+1,updated_at=$2,
		prerequisite_epoch=prerequisite_epoch+1 WHERE id=$1`,
		committedApplication.id, committed.now.Add(10*time.Minute)); err != nil {
		t.Fatalf("v004 worker could not adopt committed legacy application: %v", err)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_payload_intents SET
			state='git-committed',committed_revision=$2,committed_parent_revision=$3,
			committed_at=$4,updated_at=$4 WHERE id=$1`, ambiguousPayload.id,
			strings.Repeat("8", 40), ambiguous.platformHead, ambiguous.now.Add(9*time.Second))
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		return insertLegacyHelmPayloadRow(ctx, nested, oldWriter, oldWriterPayload, oldWriter.now.Add(3*time.Second))
	})

	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	legacyPendingID, legacyEmptyClaimID, legacyRecoveryID, legacyPendingRecoveryID :=
		id.New(), id.New(), id.New(), id.New()
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
			'id',$2::text,'generation',2,'state','pending','write_base_revision','',
			'write_base_observed_at',NULL,'committed_revision','','committed_at',NULL,
			'verified_at',NULL,'completed_at',NULL,'lease_owner',NULL,'lease_epoch',0,
			'lease_until',NULL,'worker_contract',NULL,'worker_config_digest',NULL,
			'next_attempt_at',$3::timestamptz,'created_at',$3::timestamptz,
			'updated_at',$3::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		legacyPending.desiredStateCommandID, legacyPendingID, legacyPending.now); err != nil {
		t.Fatal(err)
	}
	legacyWorkerDigest := helmPGDigest([]byte("legacy-argo-worker"))
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
			'id',$2::text,'generation',2,'state','claimed','write_base_revision','',
			'write_base_observed_at',NULL,'committed_revision','','committed_at',NULL,
			'verified_at',NULL,'completed_at',NULL,'lease_owner','legacy-argo-worker-empty',
			'lease_epoch',1,'lease_until',$3::timestamptz + interval '5 minutes',
			'worker_contract','argo-desired-state.v1','worker_config_digest',$4::text,
			'next_attempt_at',$3::timestamptz,'created_at',$3::timestamptz,
			'updated_at',$3::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		legacyEmptyClaim.desiredStateCommandID, legacyEmptyClaimID,
		legacyEmptyClaim.now, legacyWorkerDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
			'id',$2::text,'generation',2,'state','claimed','write_base_revision',$4::text,
			'write_base_observed_at',$3::timestamptz,'committed_revision','','committed_at',NULL,
			'verified_at',NULL,'completed_at',NULL,'lease_owner','legacy-argo-worker-recovery',
			'lease_epoch',1,'lease_until',$3::timestamptz + interval '1 second',
			'worker_contract','argo-desired-state.v1','worker_config_digest',$5::text,
			'next_attempt_at',$3::timestamptz,'created_at',$3::timestamptz,
			'updated_at',$3::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		legacyRecovery.desiredStateCommandID, legacyRecoveryID,
		legacyRecovery.now, legacyRecovery.platformHead, legacyWorkerDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
			'id',$2::text,'generation',2,'state','pending','write_base_revision',$4::text,
			'write_base_observed_at',$3::timestamptz,'committed_revision','','committed_at',NULL,
			'verified_at',NULL,'completed_at',NULL,'lease_owner',NULL,'lease_epoch',1,
			'lease_until',NULL,'worker_contract',NULL,'worker_config_digest',NULL,
			'next_attempt_at',$3::timestamptz,'created_at',$3::timestamptz,
			'updated_at',$3::timestamptz))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		legacyPendingRecovery.desiredStateCommandID, legacyPendingRecoveryID,
		legacyPendingRecovery.now, legacyPendingRecovery.platformHead); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/005_helm_unchanged_project_receipt/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var pendingState, emptyClaimState, recoveryState, pendingRecoveryState string
	var recoveryPolicy *string
	if err = tx.QueryRow(ctx, `SELECT state FROM argo_desired_state_commands WHERE id=$1`,
		legacyPendingID).Scan(&pendingState); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state FROM argo_desired_state_commands WHERE id=$1`,
		legacyEmptyClaimID).Scan(&emptyClaimState); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state,policy_digest FROM argo_desired_state_commands WHERE id=$1`,
		legacyRecoveryID).Scan(&recoveryState, &recoveryPolicy); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT state FROM argo_desired_state_commands WHERE id=$1`,
		legacyPendingRecoveryID).Scan(&pendingRecoveryState); err != nil {
		t.Fatal(err)
	}
	if pendingState != "superseded" || emptyClaimState != "superseded" ||
		recoveryState != "claimed" || pendingRecoveryState != "pending" || recoveryPolicy != nil {
		t.Fatalf("legacy policy upgrade pending=%s empty_claim=%s recovery=%s pending_recovery=%s policy=%v",
			pendingState, emptyClaimState, recoveryState, pendingRecoveryState, recoveryPolicy)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands
			SET policy_digest=$2 WHERE id=$1`, legacyRecoveryID,
			helmPGDigest([]byte("mutated-legacy-live-policy")))
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands
			SET policy_digest=$2 WHERE id=$1`, legacyRecovery.desiredStateCommandID,
			helmPGDigest([]byte("mutated-legacy-terminal-policy")))
		return nestedErr
	})
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE argo_desired_state_commands SET
			state='pending',lease_owner=NULL,lease_until=NULL,worker_contract=NULL,
			worker_config_digest=NULL,write_base_revision='',write_base_observed_at=NULL,
			updated_at=$2 WHERE id=$1`, legacyPendingRecoveryID, legacyPendingRecovery.now.Add(2*time.Second))
		return nestedErr
	})
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET
		state='claimed',lease_owner='argo-v005-recovery-worker',lease_epoch=lease_epoch+1,
		lease_until=$2::timestamptz+interval '5 minutes',worker_contract='argo-desired-state.v1',
		worker_config_digest=$3,updated_at=$2 WHERE id=$1`, legacyPendingRecoveryID,
		legacyPendingRecovery.now.Add(2*time.Second), helmPGDigest([]byte("v005-argo-worker"))); err != nil {
		t.Fatalf("v005 worker could not adopt legacy claimed recovery: %v", err)
	}
	legacyRecoveredRevision := strings.Repeat("e", 40)
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET
		state='git-committed',committed_revision=$2,committed_at=$3,updated_at=$3
		WHERE id=$1`, legacyPendingRecoveryID, legacyRecoveredRevision,
		legacyPendingRecovery.now.Add(3*time.Second)); err != nil {
		t.Fatalf("legacy claimed recovery could not acknowledge Git commit: %v", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE argo_desired_state_commands SET
		state='verified',lease_owner=NULL,lease_until=NULL,worker_contract=NULL,
		worker_config_digest=NULL,verified_at=$2,completed_at=$2,updated_at=$2
		WHERE id=$1`, legacyPendingRecoveryID, legacyPendingRecovery.now.Add(4*time.Second)); err != nil {
		t.Fatalf("legacy committed recovery could not verify: %v", err)
	}
	insertArgoMaterializationReceipt(t, ctx, tx, legacyPendingRecovery, id.New(),
		legacyPendingRecovery.environmentHead, 1, legacyPendingRecoveryID,
		legacyPendingRecovery.now.Add(5*time.Second))
	var recoveredReceipts int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM argo_desired_state_materialization_receipts
		WHERE desired_state_command_id=$1 AND policy_digest=$2`, legacyPendingRecoveryID,
		helmPGArgoAuthority().PolicyDigest).Scan(&recoveredReceipts); err != nil {
		t.Fatal(err)
	}
	if recoveredReceipts != 1 {
		t.Fatalf("fresh current-policy materialization receipts=%d", recoveredReceipts)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/006_remove_platform_self_upgrade/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/007_helm_publisher_adoption/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/008_qualify_helm_intent_validators/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	malformedLegacyCommand := id.New()
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands
		SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
			to_jsonb(command) || jsonb_build_object(
				'id',$2::text,'generation',99,'content',to_jsonb(convert_to('malformed legacy bundle','UTF8')),
				'content_sha256',$3::text,'policy_digest',$5::text,
				'state','failed','write_base_revision','',
			'write_base_observed_at',NULL,'committed_revision','','committed_at',NULL,
			'verified_at',NULL,'completed_at',$4::timestamptz,'lease_owner',NULL,
			'lease_until',NULL,'worker_contract',NULL,'worker_config_digest',NULL,
			'created_at',$4::timestamptz,'updated_at',$4::timestamptz,
			'next_attempt_at',$4::timestamptz,'consecutive_failures',1,
			'last_failure_code','malformed-legacy'))).*
		FROM argo_desired_state_commands command WHERE command.id=$1`,
		legacyPendingRecovery.desiredStateCommandID, malformedLegacyCommand,
		helmPGDigest([]byte("malformed legacy bundle")), legacyPendingRecovery.now.Add(6*time.Second),
		helmPGArgoAuthority().PolicyDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/009_helm_application_continuation/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/010_helm_application_materialization_bridge/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	body, err = migrations.FS.ReadFile("prisma/migrations/011_helm_application_cascade_preflight/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	for _, function := range []string{
		"public.validate_helm_application_continuation_receipt()",
		"public.helm_application_continuation_is_exact(uuid)",
	} {
		var definition string
		if err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_functiondef($1::pg_catalog.regprocedure)`,
			function).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(definition, "current_command.environment_revision=NEW.current_environment_revision") ||
			strings.Contains(definition, "current_command.environment_generation=NEW.current_environment_generation") ||
			strings.Contains(definition, "current_command.environment_revision=receipt.current_environment_revision") ||
			strings.Contains(definition, "current_command.environment_generation=receipt.current_environment_generation") {
			t.Fatalf("migration 010 retained command-origin projection equality in %s", function)
		}
		if !strings.Contains(definition, "materialization.environment_revision=") ||
			!strings.Contains(definition, "materialization.environment_generation=") ||
			!strings.Contains(definition, "current_command.content_sha256=") ||
			!strings.Contains(definition, "current_command.policy_digest=") ||
			!strings.Contains(definition, "current_command.app_project_content=") {
			t.Fatalf("migration 010 weakened current materialization authority in %s", function)
		}
	}
	// Cascade observation matches the immutable route that produced the base
	// Application. A later environment namespace/AppProject edit must not make
	// the old live child impossible to observe and delete safely. Pre-009 bases
	// resolve that route through their publication prerequisite receipt.
	cascadePreflightID, originalDeleteID := id.New(), id.New()
	if _, err = tx.Exec(ctx, `ALTER TABLE helm_application_cascade_preflights DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO helm_application_cascade_preflights(
		id,delete_intent_id,release_revision_id,payload_intent_id,base_application_intent_id,
		release_generation,payload_revision,project_id,environment_id,application_id,
		platform_binding_id,environment_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,environment_revision,environment_generation,catalog_digest,
		planned_base_revision,argo_namespace,application_path,source_content,
		source_content_digest,adopted_content,adopted_content_digest,content_digest,
		operation,precondition,expected_etag,intent_digest,commit_trailer,contract,
		publisher_contract,publisher_policy_version,publisher_config_digest,
		original_publisher_config_digest,state,next_attempt_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,2,$6,$7,$8,$9,$10,$11,$12,'refs/heads/main',
		'refs/heads/main',$13,1,$14,$15,'argocd',$16,$17,$18,$17,$18,$18,
		'observe','match-etag',$19,$20,$21,'helm-application-cascade-preflight.v1',
		'helm-protected-publisher.v1','helm-protected-git.v1',$22,$22,'pending',$23,$23,$23)`,
		cascadePreflightID, originalDeleteID, committedRelease.ID, committedPayload.id,
		committedApplication.id, committedPayloadRevision, committed.projectID,
		committed.environmentID, committed.applicationID, committed.platformBindingID,
		committed.environmentBindingID, committed.clusterID, committed.environmentHead,
		committed.catalogDigest, committedApplicationRevision, committedApplication.applicationPath,
		committedApplication.content, committedApplication.contentDigest,
		`"`+committedApplication.contentDigest+`"`, helmPGDigest([]byte("cascade-route-regression")),
		"Kuberploy-Helm-Application-Cascade-Preflight: "+cascadePreflightID,
		committed.publisherDigest, committed.now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE helm_application_cascade_preflights ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	expectedChild, err := argo.NewProtectedApplicationExpectation("argocd", committed.argoProject,
		"https://github.com/kuberploy/platform.git", committedPayloadRevision, committed.namespace,
		committed.clusterID, committed.projectID, committed.environmentID, committed.applicationID,
		committedRelease.ID, committedPayload.path, committed.manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	var childDigestBefore, childDigestAfter string
	if err = tx.QueryRow(ctx, `SELECT public.helm_application_cascade_expected_child_spec_digest($1)`,
		cascadePreflightID).Scan(&childDigestBefore); err != nil {
		t.Fatal(err)
	}
	if childDigestBefore != expectedChild.SpecDigest {
		t.Fatalf("cascade old-route child digest=%s want=%s", childDigestBefore, expectedChild.SpecDigest)
	}
	if _, err = tx.Exec(ctx, `UPDATE environments SET namespace='new-current-route',
		argo_project='new-current-route' WHERE id=$1`, committed.environmentID); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT public.helm_application_cascade_expected_child_spec_digest($1)`,
		cascadePreflightID).Scan(&childDigestAfter); err != nil {
		t.Fatal(err)
	}
	if childDigestAfter != childDigestBefore {
		t.Fatalf("current route changed immutable cascade child digest: before=%s after=%s",
			childDigestBefore, childDigestAfter)
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_application_cascade_preflights
			SET operation='caller-defined' WHERE id=$1`, cascadePreflightID)
		return nestedErr
	})
	for _, function := range []string{
		"public.validate_helm_application_cascade_gate()",
		"public.helm_application_cascade_is_exact(uuid,text,timestamp with time zone)",
	} {
		var definition string
		if err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_functiondef($1::pg_catalog.regprocedure)`,
			function).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(definition, "preflight.delete_intent_id=NEW.id") ||
			strings.Contains(definition, "preflight.delete_intent_id=intent.id") {
			t.Fatalf("replacement delete identity remained coupled in %s", function)
		}
	}
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE old_cascade_writer_probe(
		action text NOT NULL,cascade_required boolean NOT NULL DEFAULT false,
		cascade_receipt_id uuid,cascade_contract text NOT NULL DEFAULT '',
		release_revision_id uuid,payload_intent_id uuid,release_generation bigint,
		project_id uuid,environment_id uuid,application_id uuid,platform_binding_id uuid,
		environment_binding_id uuid,cluster_id uuid,platform_target_ref text,
		application_path text,expected_etag text
	);
	CREATE TRIGGER old_cascade_writer_probe_guard BEFORE INSERT ON old_cascade_writer_probe
	FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_gate();
	INSERT INTO old_cascade_writer_probe(action) VALUES('publish');
	DO $probe$
	BEGIN
		BEGIN
			INSERT INTO old_cascade_writer_probe(action) VALUES('delete');
			RAISE EXCEPTION 'old delete writer bypassed cascade authority';
		EXCEPTION WHEN check_violation THEN NULL;
		END;
	END;
	$probe$;`); err != nil {
		t.Fatal(err)
	}
	var oldPublishRows int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM old_cascade_writer_probe`).Scan(&oldPublishRows); err != nil {
		t.Fatal(err)
	}
	if oldPublishRows != 1 {
		t.Fatalf("old publish/delete cascade compatibility rows=%d", oldPublishRows)
	}
	var malformedBackfill []byte
	if err = tx.QueryRow(ctx, `SELECT COALESCE(app_project_content,''::bytea)
		FROM argo_desired_state_commands WHERE id=$1`, malformedLegacyCommand).Scan(&malformedBackfill); err != nil {
		t.Fatal(err)
	}
	if len(malformedBackfill) != 0 {
		t.Fatal("malformed legacy desired-state bundle received AppProject authority")
	}
	var validCommandProject, validMaterializationProject []byte
	if err = tx.QueryRow(ctx, `SELECT command.app_project_content,receipt.app_project_content
		FROM argo_desired_state_commands command
		JOIN argo_desired_state_materialization_receipts receipt
		  ON receipt.desired_state_command_id=command.id
		WHERE command.id=$1`, legacyPendingRecoveryID).
		Scan(&validCommandProject, &validMaterializationProject); err != nil {
		t.Fatal(err)
	}
	if len(validCommandProject) == 0 || !bytes.Equal(validCommandProject, validMaterializationProject) {
		t.Fatal("valid legacy desired-state authority was not backfilled exactly through materialization")
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `INSERT INTO argo_desired_state_commands
			SELECT (jsonb_populate_record(NULL::argo_desired_state_commands,
				to_jsonb(command) || jsonb_build_object('id',$2::text,'generation',100))).*
			FROM argo_desired_state_commands command WHERE command.id=$1`,
			malformedLegacyCommand, id.New())
		return nestedErr
	})
	assertProtectedIntentValidatorsAreHardened(t, ctx, tx)
	for _, intent := range []struct {
		table string
		id    string
	}{
		{table: "helm_protected_application_intents", id: legacyApplication.id},
		{table: "helm_protected_application_intents", id: committedApplication.id},
	} {
		var active, original string
		var adoptionEpoch int64
		query := `SELECT publisher_config_digest,original_publisher_config_digest,
			publisher_adoption_epoch FROM ` + intent.table + ` WHERE id=$1`
		if err = tx.QueryRow(ctx, query, intent.id).Scan(&active, &original, &adoptionEpoch); err != nil {
			t.Fatal(err)
		}
		if active != original || adoptionEpoch != 0 {
			t.Fatalf("migration 007 backfill table=%s active=%s original=%s epoch=%d",
				intent.table, active, original, adoptionEpoch)
		}
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_protected_application_intents SET
			publisher_config_digest=$2,prerequisite_epoch=prerequisite_epoch+1,
			updated_at=updated_at WHERE id=$1`, legacyApplication.id,
			helmPGDigest([]byte("unreceipted-publisher-adoption")))
		return nestedErr
	})
}

func testPostgresReleaseServiceTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, at time.Time) {
	t.Helper()
	f := newHelmReleasePGFixture()
	f.now = at
	setupHelmReleasePGFixture(t, ctx, tx, f)
	target := ReleaseTarget{ProjectID: f.projectID, EnvironmentID: f.environmentID, ApplicationID: f.applicationID}
	actor := ReleaseActor{ID: f.userID, IdempotencyKey: "release-service-" + id.New(), RequestID: "release-service-create"}
	requestDigest, err := releaseRequestDigest("upsert", target,
		ApprovalKey{ID: f.approvalID, Revision: 1}, f.valuesDigest, "")
	if err != nil {
		t.Fatal(err)
	}
	service := &PostgresReleaseService{operatorConfigDigest: helmPGOperatorDigest()}
	mutation := releaseMutation{kind: "upsert", target: target, actor: actor,
		approval: ApprovalKey{ID: f.approvalID, Revision: 1}, values: f.values,
		requestDigest: requestDigest}
	created, replay, err := service.mutateTx(ctx, tx, mutation, at)
	if err != nil || replay || created.Action != ReleaseInitial || created.Generation != 1 || created.RenderCommandID == "" {
		t.Fatalf("transactional release create: %+v replay=%v err=%v", created, replay, err)
	}
	replayed, replay, err := service.mutateTx(ctx, tx, mutation, at)
	if err != nil || !replay || replayed.ID != created.ID {
		t.Fatalf("transactional release replay: %+v replay=%v err=%v", replayed, replay, err)
	}
	conflicting := mutation
	conflicting.requestDigest = helmPGDigest([]byte("conflicting-release-request"))
	if _, _, err = service.mutateTx(ctx, tx, conflicting, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting release idempotency replay accepted: %v", err)
	}
	retryActor := ReleaseActor{ID: f.userID, IdempotencyKey: "release-service-" + id.New(), RequestID: "release-service-retry"}
	retryDigest, err := releaseRequestDigest("retry", target, ApprovalKey{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	retried, replay, err := service.mutateTx(ctx, tx, releaseMutation{
		kind: "retry", target: target, actor: retryActor, requestDigest: retryDigest,
	}, at.Add(time.Second))
	if err != nil || replay || retried.Action != ReleaseRetry || retried.Generation != 2 ||
		retried.ParentRevisionID != created.ID || retried.ValuesDigest != created.ValuesDigest {
		t.Fatalf("transactional release retry: %+v replay=%v err=%v", retried, replay, err)
	}
	var statusRevisionID, renderState string
	err = tx.QueryRow(ctx, `SELECT release.id::text,command.state
		FROM helm_release_heads head
		JOIN helm_release_revisions release ON release.id=head.revision_id
		JOIN helm_render_commands command ON command.id=release.render_command_id
		WHERE head.environment_id=$1 AND head.application_id=$2 AND release.project_id=$3`,
		target.EnvironmentID, target.ApplicationID, target.ProjectID).Scan(&statusRevisionID, &renderState)
	if err != nil || statusRevisionID != retried.ID || renderState != "queued" {
		t.Fatalf("transactional release status revision=%s render=%s err=%v",
			statusRevisionID, renderState, err)
	}
}

type helmReleaseInsert struct {
	id, action, parentID, rollbackID, baseID, commandID, releaseName string
	generation                                                       int64
	values                                                           []byte
	valuesDigest                                                     string
}

type helmPayloadInsert struct {
	id, releaseID, action, path    string
	generation                     int64
	content                        []byte
	contentDigest, inventoryDigest string
	resourceCount                  int
}

type helmApplicationInsert struct {
	id, releaseID, payloadID, action, payloadRevision, payloadPath          string
	sourceDirectory, applicationPath, operation, precondition, expectedETag string
	content                                                                 []byte
	contentDigest                                                           string
	generation                                                              int64
}

func newHelmReleasePGFixture() helmReleasePGFixture {
	values := []byte("{}\n")
	schema := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sample\n")
	fixture := helmReleasePGFixture{
		userID: id.New(), projectID: id.New(), environmentID: id.New(), applicationID: id.New(),
		approvalID: id.New(), platformBindingID: id.New(), environmentBindingID: id.New(), clusterID: id.New(),
		foundationIntentID: id.New(), desiredStateCommandID: id.New(),
		platformHead: strings.Repeat("a", 40), environmentHead: strings.Repeat("1", 40),
		foundationRevision: strings.Repeat("8", 40), desiredStateRevision: strings.Repeat("9", 40),
		catalogDigest: helmPGDigest([]byte("catalog")),
		values:        values, valuesDigest: helmPGDigest(values), schema: schema,
		schemaDigest: helmPGDigest(schema), manifest: manifest,
		manifestDigest: helmPGDigest(manifest), inventoryDigest: helmPGDigest([]byte("inventory")),
		now: time.Now().UTC().Truncate(time.Microsecond),
	}
	fixture.publisherDigest = helmPGDigest([]byte("publisher-" + fixture.userID))
	shortEnvironment := strings.ReplaceAll(fixture.environmentID, "-", "")[:20]
	fixture.namespace, fixture.argoProject = "helm-"+shortEnvironment, "helm-"+shortEnvironment
	fixture.ociRepository = "oci://registry.example.com/platform/sample-" + strings.ReplaceAll(fixture.approvalID, "-", "")
	fixture.documentsDigest, _ = approvalDocumentsDigest(
		ApprovalKey{ID: fixture.approvalID, Revision: 1}, fixture.schema, fixture.values)
	return fixture
}

func setupHelmReleasePGFixture(t *testing.T, ctx context.Context, tx pgx.Tx, f helmReleasePGFixture) {
	t.Helper()
	approval := Approval{ApprovalKey: ApprovalKey{ID: f.approvalID, Revision: 1},
		OCIRepository: f.ociRepository, ChartVersion: "1.2.3",
		ManifestDigest:     helmPGDigest([]byte("chart-manifest")),
		PackageDigest:      helmPGDigest([]byte("chart-package")),
		ValuesSchemaDigest: f.schemaDigest, RendererImage: RendererImage,
		RendererVersion: HelmVersion, PolicyVersion: PolicyVersion, CreatedBy: f.userID,
		IdempotencyKey: "approval-" + f.approvalID, CreatedAt: f.now}
	approvalIdentity, err := approval.IdentityDigest()
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,display_name,role,issuer,subject,created_at)
			VALUES($1,$2,'platform-admin','helm-release-test',$3,$4)`, []any{f.userID, "helm-release-" + f.userID, f.userID, f.now}},
		{`INSERT INTO projects(id,name,slug,created_at) VALUES($1,'Helm Release',$2,$3)`, []any{f.projectID, "helm-" + f.projectID, f.now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
			VALUES($1,$2,'Helm Release','helm',$3,$4,$5)`, []any{f.environmentID, f.projectID, f.namespace, f.argoProject, f.now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at)
			VALUES($1,$2,'Sample','sample',$3)`, []any{f.applicationID, f.projectID, f.now}},
		{`INSERT INTO helm_chart_approvals(
			approval_id,revision,oci_repository,chart_version,manifest_digest,
			package_digest,values_schema_digest,renderer_image,renderer_version,
			policy_version,identity_digest,created_by,idempotency_key,created_at
		) VALUES($1,1,$2,'1.2.3',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			[]any{f.approvalID, f.ociRepository, helmPGDigest([]byte("chart-manifest")), helmPGDigest([]byte("chart-package")),
				f.schemaDigest, RendererImage, HelmVersion, PolicyVersion,
				approvalIdentity, f.userID, "approval-" + f.approvalID, f.now}},
		{`INSERT INTO helm_chart_approval_documents(
			approval_id,approval_revision,values_schema_json,default_values_yaml,
			values_schema_digest,documents_digest,created_at
		) VALUES($1,1,$2,$3,$4,$5,$6)`, []any{f.approvalID, f.schema, f.values,
			f.schemaDigest, f.documentsDigest, f.now}},
		{`INSERT INTO git_repository_bindings(
			id,kind,scope_id,cluster_id,provider,installation_id,repository_id,
			repository_owner,repository_name,target_ref,path_prefix,credential_secret_name,
			credential_mode,state,target_head_revision,projection_generation,parser_version,
			target_head_observed_at,created_at,updated_at
		) VALUES($1,'platform',$2,$2,'github',1,101,'kuberploy','platform',
			'refs/heads/main',$3,'','github-app','indexing',$4,0,'gitprojection.v1',$5,$5,$5)
			ON CONFLICT (id) DO NOTHING`,
			[]any{f.platformBindingID, f.clusterID, "clusters/" + f.clusterID, f.platformHead, f.now}},
		{`INSERT INTO git_repository_bindings(
			id,kind,scope_id,project_id,environment_id,provider,installation_id,repository_id,
			repository_owner,repository_name,target_ref,path_prefix,credential_secret_name,
			credential_mode,state,target_head_revision,indexed_revision,projection_generation,
			parser_version,target_head_observed_at,indexed_at,created_at,updated_at
		) VALUES($1,'environment',$2,$3,$2,'github',1,102,'kuberploy','environment',
			'refs/heads/main',$4,'','github-app','ready',$5,$5,1,'gitprojection.v1',$6,$6,$6,$6)`,
			[]any{f.environmentBindingID, f.environmentID, f.projectID,
				"tenants/" + f.projectID + "/environments/" + f.environmentID, f.environmentHead, f.now}},
		{`INSERT INTO git_projection_generations(
			binding_id,generation,head_revision,parser_version,state,started_at,activated_at
		) VALUES($1,1,$2,'gitprojection.v1','active',$3,$3)`, []any{f.environmentBindingID, f.environmentHead, f.now}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	foundationManifest := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + f.namespace + "\n")
	if _, err := tx.Exec(ctx, `INSERT INTO environment_foundation_intents(
		id,environment_id,project_id,namespace,argo_project,platform_binding_id,cluster_id,
		target_ref,planned_head_revision,binding_generation,profile_digest,publisher_config_digest,
		publisher_contract,publisher_policy,manifest_path,manifest,manifest_digest,intent_digest,
		commit_trailer,state,active,next_attempt_at,attempts,write_base_revision,
		write_base_observed_at,committed_revision,committed_parent_revision,provider_request,
		published_at,completed_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,'refs/heads/main',$8,1,$9,$10,
		'environment-foundation-protected-git.v1','platform-protected-git.v1',$11,$12,$13,$14,$15,
		'ready',true,$16,1,$8,$16,$17,$8,'foundation-fixture',$16,$16,$16,$16)`,
		f.foundationIntentID, f.environmentID, f.projectID, f.namespace, f.argoProject,
		f.platformBindingID, f.clusterID, f.platformHead, helmPGDigest([]byte("foundation-profile")),
		helmPGDigest([]byte("foundation-publisher")),
		"clusters/"+f.clusterID+"/argocd/foundations/"+f.environmentID+".yaml",
		foundationManifest, helmPGDigest(foundationManifest), helmPGDigest([]byte("foundation-intent-"+f.foundationIntentID)),
		"Kuberploy-Environment-Foundation-Intent: "+f.foundationIntentID, f.now, f.foundationRevision); err != nil {
		t.Fatal(err)
	}
	appProjectContent, err := argo.RenderAppProjectAuthority(argo.AppProjectAuthority{
		ProjectID: f.projectID, EnvironmentID: f.environmentID,
		EnvironmentBindingID: f.environmentBindingID, Namespace: f.namespace,
		ArgoProject: f.argoProject, ArgoNamespace: "argocd",
		EnvironmentRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 102, Owner: "kuberploy", Name: "environment"},
		PlatformRepository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1,
			RepositoryID: 101, Owner: "kuberploy", Name: "platform"},
		Runtime: helmPGArgoAuthority().Runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	argoContent := append(append([]byte(nil), appProjectContent...), []byte("---\napiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: fixture\n")...)
	if _, err := tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	var hasAppProjectContent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='argo_desired_state_commands'
		  AND column_name='app_project_content')`).Scan(&hasAppProjectContent); err != nil {
		t.Fatal(err)
	}
	var argoErr error
	if hasAppProjectContent {
		_, argoErr = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands(
		id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,
		cluster_id,platform_target_ref,environment_target_ref,environment_revision,
		environment_generation,path,argo_namespace,destination_namespace,argo_project,
		base_revision,write_base_revision,write_base_observed_at,precondition,expected_etag,
		catalog_digest,chart_repository,chart_name,chart_version,chart_digest,renderer_image,
		chart_digest_enforcement,app_project_content,content,content_sha256,message,state,committed_revision,
		committed_at,verified_at,next_attempt_at,created_at,updated_at,completed_at
	) VALUES($1,1,$2,$3,$4,$5,$6,'refs/heads/main','refs/heads/main',$7,1,$8,'argocd',$9,$10,
		$11,$11,$12,'create-if-absent','',$13,'oci://registry.example.com/kuberploy/runtime',
		'kuberploy-runtime','1.2.3',$14,$15,'native-oci-digest-v1',$16,$17,$18,
		'reconcile fixture AppProject','verified',$19,$12,$12,$12,$12,$12,$12)`,
			f.desiredStateCommandID, f.projectID, f.environmentID, f.platformBindingID,
			f.environmentBindingID, f.clusterID, f.environmentHead,
			"clusters/"+f.clusterID+"/argocd/environments/"+f.environmentID+".yaml",
			f.namespace, f.argoProject, f.foundationRevision, f.now,
			helmPGDigest([]byte("argo-desired-state-catalog")),
			helmPGDigest([]byte("runtime-chart")),
			"registry.example.com/kuberploy/runtime@"+helmPGDigest([]byte("renderer")),
			appProjectContent, argoContent, helmPGDigest(argoContent), f.desiredStateRevision)
	} else {
		_, argoErr = tx.Exec(ctx, `INSERT INTO argo_desired_state_commands(
			id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,
			cluster_id,platform_target_ref,environment_target_ref,environment_revision,
			environment_generation,path,argo_namespace,destination_namespace,argo_project,
			base_revision,write_base_revision,write_base_observed_at,precondition,expected_etag,
			catalog_digest,chart_repository,chart_name,chart_version,chart_digest,renderer_image,
			chart_digest_enforcement,content,content_sha256,message,state,committed_revision,
			committed_at,verified_at,next_attempt_at,created_at,updated_at,completed_at
		) VALUES($1,1,$2,$3,$4,$5,$6,'refs/heads/main','refs/heads/main',$7,1,$8,'argocd',$9,$10,
			$11,$11,$12,'create-if-absent','',$13,'oci://registry.example.com/kuberploy/runtime',
			'kuberploy-runtime','1.2.3',$14,$15,'native-oci-digest-v1',$16,$17,
			'reconcile fixture AppProject','verified',$18,$12,$12,$12,$12,$12,$12)`,
			f.desiredStateCommandID, f.projectID, f.environmentID, f.platformBindingID,
			f.environmentBindingID, f.clusterID, f.environmentHead,
			"clusters/"+f.clusterID+"/argocd/environments/"+f.environmentID+".yaml",
			f.namespace, f.argoProject, f.foundationRevision, f.now,
			helmPGDigest([]byte("argo-desired-state-catalog")),
			helmPGDigest([]byte("runtime-chart")),
			"registry.example.com/kuberploy/runtime@"+helmPGDigest([]byte("renderer")),
			argoContent, helmPGDigest(argoContent), f.desiredStateRevision)
	}
	var hasPolicyDigest, hasMaterializationReceipts bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='argo_desired_state_commands'
		  AND column_name='policy_digest'
	)`).Scan(&hasPolicyDigest); err != nil {
		t.Fatal(err)
	}
	if hasPolicyDigest {
		if _, err := tx.Exec(ctx, `UPDATE argo_desired_state_commands SET policy_digest=$2 WHERE id=$1`,
			f.desiredStateCommandID, helmPGDigest([]byte("argo-materialization-policy"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if argoErr != nil {
		t.Fatal(argoErr)
	}
	if err := tx.QueryRow(ctx, `SELECT to_regclass(
		'public.argo_desired_state_materialization_receipts') IS NOT NULL`).Scan(&hasMaterializationReceipts); err != nil {
		t.Fatal(err)
	}
	if hasMaterializationReceipts {
		insertArgoMaterializationReceipt(t, ctx, tx, f, id.New(), f.environmentHead, 1,
			f.desiredStateCommandID, f.now.Add(time.Microsecond))
	}
	expectPGCheck(t, ctx, tx, func(nested pgx.Tx) error {
		_, nestedErr := nested.Exec(ctx, `UPDATE helm_chart_approval_documents
			SET default_values_yaml='x: y' WHERE approval_id=$1`, f.approvalID)
		return nestedErr
	})
}

func insertHelmRenderCommand(t *testing.T, ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, commandID string, values []byte, valuesDigest string, at time.Time) {
	t.Helper()
	descriptor := []byte("apiVersion: kuberploy.io/v1alpha1\nkind: ApprovedHelmApplication\n")
	_, err := tx.Exec(ctx, `INSERT INTO helm_render_commands(
		id,idempotency_scope,idempotency_key,approval_id,approval_revision,project_id,
		environment_id,application_id,namespace,release_name,descriptor_yaml,values_yaml,
		descriptor_digest,values_digest,input_digest,operator_config_digest,state,available_at,created_at,updated_at
	) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,'sample',$9,$10,$11,$12,$13,$14,
		'queued',$15,$15,$15)`, commandID, f.userID, "render-"+commandID, f.approvalID,
		f.projectID, f.environmentID, f.applicationID, f.namespace, descriptor, values,
		helmPGDigest(descriptor), valuesDigest, helmPGDigest(append(append([]byte{}, descriptor...), values...)),
		helmPGOperatorDigest(), at)
	if err != nil {
		t.Fatal(err)
	}
}

func insertArgoMaterializationReceipt(t *testing.T, ctx context.Context, tx pgx.Tx,
	f helmReleasePGFixture, receiptID, environmentRevision string, environmentGeneration int64,
	commandID string, at time.Time) {
	t.Helper()
	var hasAppProjectContent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='argo_desired_state_materialization_receipts'
		  AND column_name='app_project_content')`).Scan(&hasAppProjectContent); err != nil {
		t.Fatal(err)
	}
	query := `INSERT INTO argo_desired_state_materialization_receipts(
		id,environment_binding_id,environment_revision,environment_generation,
		project_id,environment_id,platform_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,desired_state_command_id,desired_state_generation,
		desired_state_revision,desired_state_content_sha256,catalog_digest,policy_digest,
		chart_repository,chart_name,chart_version,chart_digest,renderer_image,
		chart_digest_enforcement,app_project_content,created_at
	) SELECT $2,command.environment_binding_id,$3,$4,command.project_id,
		command.environment_id,command.platform_binding_id,command.cluster_id,
		command.platform_target_ref,command.environment_target_ref,command.id,
		command.generation,command.committed_revision,command.content_sha256,command.catalog_digest,$5,
		command.chart_repository,command.chart_name,command.chart_version,
		command.chart_digest,command.renderer_image,command.chart_digest_enforcement,
		command.app_project_content,$6
	FROM argo_desired_state_commands command WHERE command.id=$1`
	if !hasAppProjectContent {
		query = `INSERT INTO argo_desired_state_materialization_receipts(
			id,environment_binding_id,environment_revision,environment_generation,
			project_id,environment_id,platform_binding_id,cluster_id,platform_target_ref,
			environment_target_ref,desired_state_command_id,desired_state_generation,
			desired_state_revision,desired_state_content_sha256,catalog_digest,policy_digest,
			chart_repository,chart_name,chart_version,chart_digest,renderer_image,
			chart_digest_enforcement,created_at
		) SELECT $2,command.environment_binding_id,$3,$4,command.project_id,
			command.environment_id,command.platform_binding_id,command.cluster_id,
			command.platform_target_ref,command.environment_target_ref,command.id,
			command.generation,command.committed_revision,command.content_sha256,command.catalog_digest,$5,
			command.chart_repository,command.chart_name,command.chart_version,
			command.chart_digest,command.renderer_image,command.chart_digest_enforcement,$6
		FROM argo_desired_state_commands command WHERE command.id=$1`
	}
	_, err := tx.Exec(ctx, query, commandID, receiptID,
		environmentRevision, environmentGeneration,
		helmPGDigest([]byte("argo-materialization-policy")), at.UTC())
	if err != nil {
		t.Fatal(err)
	}
}

func completeHelmRender(t *testing.T, ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, commandID string, at time.Time) {
	t.Helper()
	limitsDigest := helmPGDigest([]byte("limits"))
	if _, err := tx.Exec(ctx, `UPDATE helm_render_commands SET
		state='processing',attempts=1,lease_owner='helm-render-worker-0001',lease_epoch=1,
		lease_until=$2::timestamptz+interval '5 minutes',worker_contract=$3,worker_renderer_image=$4,
		worker_renderer_version=$5,worker_policy_version=$6,worker_limits_digest=$7,
		worker_operator_config_digest=$8,updated_at=$2 WHERE id=$1`, commandID, at, RendererContract, RendererImage,
		HelmVersion, PolicyVersion, limitsDigest, helmPGOperatorDigest()); err != nil {
		t.Fatal(err)
	}
	var inputDigest string
	if err := tx.QueryRow(ctx, `SELECT input_digest FROM helm_render_commands WHERE id=$1`, commandID).Scan(&inputDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO helm_render_results(
		command_id,input_digest,operator_config_digest,manifest_digest,inventory_digest,rendered_manifests,
		resource_count,output_bytes,renderer_image,renderer_version,policy_version,
		limits_digest,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12)`, commandID, inputDigest,
		helmPGOperatorDigest(),
		f.manifestDigest, f.inventoryDigest, f.manifest, len(f.manifest), RendererImage,
		HelmVersion, PolicyVersion, limitsDigest, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE helm_render_commands SET
		state='succeeded',lease_owner=NULL,lease_until=NULL,worker_contract=NULL,
		worker_renderer_image=NULL,worker_renderer_version=NULL,worker_policy_version=NULL,
		worker_limits_digest=NULL,worker_operator_config_digest=NULL,completed_at=$2,updated_at=$2 WHERE id=$1`,
		commandID, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func helmPGOperatorDigest() string {
	return helmPGDigest([]byte("helm-release-integration-operator.v1"))
}

func insertHelmRelease(t *testing.T, ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, release helmReleaseInsert, at time.Time) {
	t.Helper()
	if err := insertHelmReleaseErr(ctx, tx, f, release, at); err != nil {
		t.Fatal(err)
	}
}

func insertHelmReleaseErr(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, release helmReleaseInsert, at time.Time) error {
	releaseName := release.releaseName
	if releaseName == "" {
		releaseName = "sample"
	}
	_, err := tx.Exec(ctx, `INSERT INTO helm_release_revisions(
		id,generation,project_id,environment_id,application_id,release_name,action,
		desired_enabled,parent_revision_id,rollback_source_revision_id,base_intent_id,
		approval_id,approval_revision,render_command_id,values_yaml,values_digest,
		intent_digest,actor_id,idempotency_key,request_id,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1,$13,$14,$15,$16,$17,$18,$19,$20)`,
		release.id, release.generation, f.projectID, f.environmentID, f.applicationID,
		releaseName, release.action, release.action != "disable", nullableUUID(release.parentID),
		nullableUUID(release.rollbackID), nullableUUID(release.baseID), f.approvalID,
		nullableUUID(release.commandID), release.values, release.valuesDigest,
		helmPGDigest([]byte("release-"+release.id)), f.userID, "release-"+release.id,
		"request-"+release.id, at)
	return err
}

func insertHelmPayload(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, payload helmPayloadInsert, at time.Time) error {
	release, err := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+`
		WHERE id=$1 FOR KEY SHARE`, payload.releaseID))
	if err != nil {
		return err
	}
	binding := ProtectedBindingSnapshot{
		PlatformBindingID: f.platformBindingID, EnvironmentBindingID: f.environmentBindingID,
		ClusterID: f.clusterID, PlatformTargetRef: "refs/heads/main", EnvironmentTargetRef: "refs/heads/main",
		EnvironmentRevision: f.environmentHead, EnvironmentGeneration: 1,
		CatalogDigest: f.catalogDigest, PlannedBaseRevision: currentPlatformHead(ctx, tx, f.platformBindingID),
	}
	if _, err = ensurePublicationPrerequisite(ctx, tx, release, binding, helmPGArgoAuthority(), at); err != nil {
		return err
	}
	return insertHelmPayloadRow(ctx, tx, f, payload, at)
}

func insertLegacyHelmPayloadRow(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, payload helmPayloadInsert, at time.Time) error {
	var inventory any
	var count any
	if len(payload.inventoryDigest) > 0 {
		inventory, count = payload.inventoryDigest, payload.resourceCount
	}
	_, err := tx.Exec(ctx, `INSERT INTO helm_protected_payload_intents(
		id,release_revision_id,release_generation,project_id,environment_id,application_id,
		action,platform_binding_id,environment_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,environment_revision,environment_generation,catalog_digest,
		planned_base_revision,path,precondition,expected_etag,content,content_digest,
		manifest_inventory_digest,manifest_resource_count,intent_digest,commit_trailer,
		publisher_contract,publisher_config_digest,message,state,next_attempt_at,
		created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'refs/heads/main','refs/heads/main',
		$11,1,$12,$13,$14,'create-if-absent','',$15,$16,$17,$18,$19,$20,
		'helm-protected-publisher.v1',$21,$22,'pending',$23,$23,$23)`,
		payload.id, payload.releaseID, payload.generation, f.projectID, f.environmentID,
		f.applicationID, payload.action, f.platformBindingID, f.environmentBindingID,
		f.clusterID, f.environmentHead, f.catalogDigest,
		currentPlatformHead(ctx, tx, f.platformBindingID), payload.path,
		payload.content, payload.contentDigest, inventory, count,
		helmPGDigest([]byte("payload-"+payload.id)),
		"Kuberploy-Helm-Payload-Intent: "+payload.id, f.publisherDigest,
		"publish protected Helm payload "+payload.id, at)
	return err
}

func insertHelmPayloadRow(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, payload helmPayloadInsert, at time.Time) error {
	var inventory any
	var count any
	if len(payload.inventoryDigest) > 0 {
		inventory, count = payload.inventoryDigest, payload.resourceCount
	}
	_, err := tx.Exec(ctx, `INSERT INTO helm_protected_payload_intents(
		id,release_revision_id,release_generation,project_id,environment_id,application_id,
		action,platform_binding_id,environment_binding_id,cluster_id,platform_target_ref,
		environment_target_ref,environment_revision,environment_generation,catalog_digest,
		planned_base_revision,path,precondition,expected_etag,content,content_digest,
		manifest_inventory_digest,manifest_resource_count,intent_digest,commit_trailer,
		publisher_contract,publisher_config_digest,original_publisher_config_digest,
		publisher_adoption_epoch,message,state,next_attempt_at,
		prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'refs/heads/main','refs/heads/main',
		$11,1,$12,$13,$14,'create-if-absent','',$15,$16,$17,$18,$19,$20,
		'helm-protected-publisher.v1',$21,$21,0,$22,'pending',$23,$2,$24,0,$23,$23)`,
		payload.id, payload.releaseID, payload.generation, f.projectID, f.environmentID,
		f.applicationID, payload.action, f.platformBindingID, f.environmentBindingID,
		f.clusterID, f.environmentHead, f.catalogDigest,
		currentPlatformHead(ctx, tx, f.platformBindingID), payload.path,
		payload.content, payload.contentDigest, inventory, count,
		helmPGDigest([]byte("payload-"+payload.id)),
		"Kuberploy-Helm-Payload-Intent: "+payload.id, f.publisherDigest,
		"publish protected Helm payload "+payload.id, at, protectedPrerequisiteContract)
	return err
}

func insertLegacyHelmApplication(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, application helmApplicationInsert, at time.Time) error {
	content := application.content
	if content == nil {
		content = []byte{}
	}
	var continuationColumns bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='helm_protected_application_intents'
		AND column_name='continuation_required')`).Scan(&continuationColumns); err != nil {
		return err
	}
	if !continuationColumns {
		_, err := tx.Exec(ctx, `INSERT INTO helm_protected_application_intents(
			id,release_revision_id,payload_intent_id,release_generation,project_id,
			environment_id,application_id,action,platform_binding_id,environment_binding_id,
			cluster_id,platform_target_ref,environment_target_ref,environment_revision,
			environment_generation,catalog_digest,planned_base_revision,payload_revision,
			payload_path,source_directory,application_path,operation,precondition,expected_etag,
			content,content_digest,intent_digest,commit_trailer,publisher_contract,
			publisher_config_digest,message,state,next_attempt_at,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'refs/heads/main','refs/heads/main',
			$12,1,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
			'helm-protected-publisher.v1',$26,$27,'pending',$28,$28,$28)`,
			application.id, application.releaseID, application.payloadID, application.generation,
			f.projectID, f.environmentID, f.applicationID, application.action,
			f.platformBindingID, f.environmentBindingID, f.clusterID, f.environmentHead,
			f.catalogDigest, currentPlatformHead(ctx, tx, f.platformBindingID),
			application.payloadRevision, application.payloadPath, application.sourceDirectory,
			application.applicationPath, application.operation, application.precondition,
			application.expectedETag, content, application.contentDigest,
			helmPGDigest([]byte("application-"+application.id)),
			"Kuberploy-Helm-Application-Intent: "+application.id, f.publisherDigest,
			"publish protected Helm Application "+application.id, at)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO helm_protected_application_intents(
		id,release_revision_id,payload_intent_id,release_generation,project_id,
		environment_id,application_id,action,platform_binding_id,environment_binding_id,
		cluster_id,platform_target_ref,environment_target_ref,environment_revision,
		environment_generation,catalog_digest,planned_base_revision,payload_revision,
		payload_path,source_directory,application_path,operation,precondition,expected_etag,
		content,content_digest,intent_digest,commit_trailer,publisher_contract,
		publisher_config_digest,original_publisher_config_digest,publisher_adoption_epoch,
		continuation_required,continuation_contract,
		message,state,next_attempt_at,prerequisite_receipt_id,
		prerequisite_contract,prerequisite_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'refs/heads/main','refs/heads/main',
		$12,1,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
		'helm-protected-publisher.v1',$26,$26,0,false,'',$27,'pending',$28,$2,$29,0,$28,$28)`,
		application.id, application.releaseID, application.payloadID, application.generation,
		f.projectID, f.environmentID, f.applicationID, application.action,
		f.platformBindingID, f.environmentBindingID, f.clusterID, f.environmentHead,
		f.catalogDigest, currentPlatformHead(ctx, tx, f.platformBindingID),
		application.payloadRevision, application.payloadPath, application.sourceDirectory,
		application.applicationPath, application.operation, application.precondition,
		application.expectedETag, content, application.contentDigest,
		helmPGDigest([]byte("application-"+application.id)),
		"Kuberploy-Helm-Application-Intent: "+application.id, f.publisherDigest,
		"publish protected Helm Application "+application.id, at, protectedPrerequisiteContract)
	return err
}

func insertHelmApplication(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, application helmApplicationInsert, at time.Time) error {
	content := application.content
	if content == nil {
		content = []byte{}
	}
	payload, err := scanProtectedPayload(tx.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
		FROM public.helm_protected_payload_intents WHERE id=$1 FOR KEY SHARE`, application.payloadID))
	if err != nil {
		return err
	}
	release, err := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+`
		WHERE id=$1 FOR KEY SHARE`, application.releaseID))
	if err != nil {
		return err
	}
	binding := payload.Binding
	binding.PlannedBaseRevision = currentPlatformHead(ctx, tx, f.platformBindingID)
	intentDigest := helmPGDigest([]byte("application-" + application.id))
	value := ProtectedApplicationIntent{
		ID: application.id, ReleaseRevisionID: application.releaseID,
		PayloadIntentID: application.payloadID, ReleaseGeneration: application.generation,
		Target: release.Target, Binding: binding, Action: ProtectedApplicationAction(application.action),
		PayloadRevision: application.payloadRevision, PayloadPath: application.payloadPath,
		SourceDirectory: application.sourceDirectory, ApplicationPath: application.applicationPath,
		Operation: application.operation, Precondition: application.precondition,
		ExpectedETag: application.expectedETag, Content: content,
		ContentDigest: application.contentDigest, IntentDigest: intentDigest,
		CommitTrailer: "Kuberploy-Helm-Application-Intent: " + application.id,
		Publisher: ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
			PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest},
		OriginalPublisherConfigDigest: f.publisherDigest,
		ContinuationRequired:          true, ContinuationReceiptID: application.id,
		ContinuationContract: protectedContinuationContract,
		Message:              "publish protected Helm Application " + application.id,
		State:                ProtectedPending, NextAttemptAt: at, CreatedAt: at, UpdatedAt: at,
	}
	// Schema 010 predates the durable cascade columns, while the current Go
	// model correctly requires them for an actionable delete. Supply a
	// validation-only tuple when this legacy fixture builds its continuation;
	// schema 011 delete behavior is exercised through the real cascade store.
	if value.Action == ProtectedApplicationDelete {
		value.CascadeRequired = true
		value.CascadeReceiptID = id.New()
		value.CascadeContract = protectedCascadeContract
	}
	// Invalid rows intentionally continue to the database below so this
	// migration integration fixture proves the trigger rejects them. Valid
	// rows receive the exact continuation receipt before the cyclic intent FK.
	_, _ = ensureApplicationContinuation(ctx, tx, release, payload, value, helmPGArgoAuthority(), at)
	_, err = tx.Exec(ctx, `INSERT INTO helm_protected_application_intents(
		id,release_revision_id,payload_intent_id,release_generation,project_id,
		environment_id,application_id,action,platform_binding_id,environment_binding_id,
		cluster_id,platform_target_ref,environment_target_ref,environment_revision,
		environment_generation,catalog_digest,planned_base_revision,payload_revision,
		payload_path,source_directory,application_path,operation,precondition,expected_etag,
		content,content_digest,intent_digest,commit_trailer,publisher_contract,
		publisher_config_digest,original_publisher_config_digest,publisher_adoption_epoch,
		continuation_required,continuation_receipt_id,continuation_contract,
		message,state,next_attempt_at,prerequisite_receipt_id,
		prerequisite_contract,prerequisite_epoch,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'refs/heads/main','refs/heads/main',
		$12,1,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
		'helm-protected-publisher.v1',$26,$26,0,true,$1,$27,$28,'pending',$29,$2,$30,0,$29,$29)`,
		application.id, application.releaseID, application.payloadID, application.generation,
		f.projectID, f.environmentID, f.applicationID, application.action,
		f.platformBindingID, f.environmentBindingID, f.clusterID, f.environmentHead,
		f.catalogDigest, currentPlatformHead(ctx, tx, f.platformBindingID),
		application.payloadRevision, application.payloadPath, application.sourceDirectory,
		application.applicationPath, application.operation, application.precondition,
		application.expectedETag, content, application.contentDigest,
		intentDigest,
		"Kuberploy-Helm-Application-Intent: "+application.id, f.publisherDigest,
		protectedContinuationContract, "publish protected Helm Application "+application.id,
		at, protectedPrerequisiteContract)
	return err
}

func claimHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID string, at time.Time) {
	claimHelmIntentVersion(t, ctx, tx, table, intentID, at, true)
}

func claimLegacyHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID string, at time.Time) {
	claimHelmIntentVersion(t, ctx, tx, table, intentID, at, false)
}

func claimHelmIntentVersion(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID string, at time.Time, fenced bool) {
	t.Helper()
	requireHelmIntentTable(t, table)
	fence := ""
	if fenced {
		fence = ",prerequisite_epoch=prerequisite_epoch+1"
	}
	query := fmt.Sprintf(`UPDATE %s SET state='claimed',attempts=1,
		lease_owner='helm-publisher-worker-0001',lease_epoch=1,
			lease_until=$2::timestamptz+interval '5 minutes',updated_at=$2%s WHERE id=$1`, table, fence)
	if _, err := tx.Exec(ctx, query, intentID, at); err != nil {
		t.Fatal(err)
	}
}

func commitHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, parent, commit string, at time.Time) {
	commitHelmIntentVersion(t, ctx, tx, table, intentID, parent, commit, at, true)
}

func commitLegacyHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, parent, commit string, at time.Time) {
	commitHelmIntentVersion(t, ctx, tx, table, intentID, parent, commit, at, false)
}

func commitHelmIntentVersion(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, parent, commit string, at time.Time, fenced bool) {
	t.Helper()
	requireHelmIntentTable(t, table)
	fence := ""
	if fenced {
		fence = ",prerequisite_epoch=prerequisite_epoch+1"
	}
	query := fmt.Sprintf(`UPDATE %s SET state='git-committed',write_base_revision=$2,
		write_base_observed_at=$4::timestamptz-interval '1 second',committed_revision=$3,
		committed_parent_revision=$2,committed_at=$4,updated_at=$4%s WHERE id=$1`, table, fence)
	if _, err := tx.Exec(ctx, query, intentID, parent, commit, at); err != nil {
		t.Fatal(err)
	}
}

func verifyHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, pathDigest string, at time.Time) {
	verifyHelmIntentVersion(t, ctx, tx, table, intentID, pathDigest, at, true)
}

func verifyLegacyHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, pathDigest string, at time.Time) {
	verifyHelmIntentVersion(t, ctx, tx, table, intentID, pathDigest, at, false)
}

func verifyHelmIntentVersion(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, pathDigest string, at time.Time, fenced bool) {
	t.Helper()
	requireHelmIntentTable(t, table)
	fence := ""
	if fenced {
		fence = ",prerequisite_epoch=prerequisite_epoch+1"
	}
	query := fmt.Sprintf(`UPDATE %s SET state='verified',lease_owner=NULL,lease_until=NULL,
		verified_at=$2,verified_path_digest=$3,provider_request=$4,
		completed_at=$2,updated_at=$2%s WHERE id=$1`, table, fence)
	if _, err := tx.Exec(ctx, query, intentID, at, pathDigest, "provider-"+intentID); err != nil {
		t.Fatal(err)
	}
}

func requireHelmIntentTable(t *testing.T, table string) {
	t.Helper()
	if table != "helm_protected_payload_intents" && table != "helm_protected_application_intents" {
		t.Fatalf("unexpected Helm intent table %q", table)
	}
}

func advancePlatformHead(t *testing.T, ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, revision string, at time.Time) {
	t.Helper()
	if _, err := tx.Exec(ctx, `UPDATE git_repository_bindings SET
		target_head_revision=$2,target_head_observed_at=$3,updated_at=$3 WHERE id=$1`,
		f.platformBindingID, revision, at); err != nil {
		t.Fatal(err)
	}
}

func currentPlatformHead(ctx context.Context, tx pgx.Tx, bindingID string) string {
	var revision string
	_ = tx.QueryRow(ctx, `SELECT target_head_revision FROM git_repository_bindings WHERE id=$1`, bindingID).Scan(&revision)
	return revision
}

func insertVerifiedArgoDesiredStateCommand(t *testing.T, ctx context.Context, tx pgx.Tx,
	f helmReleasePGFixture, commandID string, commandGeneration int64,
	environmentRevision string, environmentGeneration int64,
	desiredStateRevision, baseRevision string, authority ArgoMaterializationAuthority,
	appProjectContent, content []byte, at time.Time,
) {
	t.Helper()
	if _, err := tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		DISABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
	_, insertErr := tx.Exec(ctx, `INSERT INTO argo_desired_state_commands(
		id,generation,project_id,environment_id,platform_binding_id,environment_binding_id,
		cluster_id,platform_target_ref,environment_target_ref,environment_revision,
		environment_generation,path,argo_namespace,destination_namespace,argo_project,
		base_revision,write_base_revision,write_base_observed_at,precondition,expected_etag,
		policy_digest,catalog_digest,chart_repository,chart_name,chart_version,chart_digest,
		renderer_image,chart_digest_enforcement,app_project_content,content,content_sha256,
		message,state,committed_revision,committed_at,verified_at,next_attempt_at,
		created_at,updated_at,completed_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,'refs/heads/main','refs/heads/main',$8,$25,$9,
		'argocd',$10,$11,$12,$12,$13,'match-etag',$14,$15,$16,$17,'kuberploy-runtime',$18,$19,
		$20,'native-oci-digest-v1',$21,$22,$23,'current canonical AppProject','verified',$24,
		$13,$13,$13,$13,$13,$13)`, commandID, commandGeneration, f.projectID,
		f.environmentID, f.platformBindingID, f.environmentBindingID, f.clusterID,
		environmentRevision,
		"clusters/"+f.clusterID+"/argocd/environments/"+f.environmentID+".yaml",
		f.namespace, f.argoProject, baseRevision, at,
		`"`+helmPGDigest([]byte("previous"))+`"`, authority.PolicyDigest, f.catalogDigest,
		authority.Runtime.ChartRepository, authority.Runtime.ChartVersion,
		authority.Runtime.ChartDigest, authority.Runtime.RendererImage,
		appProjectContent, content, helmPGDigest(content), desiredStateRevision,
		environmentGeneration)
	if insertErr != nil {
		t.Fatal(insertErr)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE argo_desired_state_commands
		ENABLE TRIGGER argo_desired_state_commands_validate`); err != nil {
		t.Fatal(err)
	}
}

func helmPayloadPath(f helmReleasePGFixture, releaseID string, disabled bool) string {
	name := "release.yaml"
	if disabled {
		name = "disabled.json"
	}
	return "clusters/" + f.clusterID + "/helm-manifests/environments/" + f.environmentID +
		"/applications/" + f.applicationID + "/revisions/" + releaseID + "/" + name
}

func helmSourceDirectory(f helmReleasePGFixture, releaseID string) string {
	return "clusters/" + f.clusterID + "/helm-manifests/environments/" + f.environmentID +
		"/applications/" + f.applicationID + "/revisions/" + releaseID
}

func helmApplicationPath(f helmReleasePGFixture) string {
	return "clusters/" + f.clusterID + "/argocd/helm-applications/" +
		f.environmentID + "/" + f.applicationID + ".yaml"
}

func helmPGDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func helmPGNotBefore(value time.Time, floors ...time.Time) time.Time {
	result := value.UTC()
	for _, floor := range floors {
		if result.Before(floor) {
			result = floor.UTC()
		}
	}
	return result
}

func applyHelmMigrationsThrough(ctx context.Context, pool *pgxpool.Pool, target string) error {
	history, err := migrations.History()
	if err != nil {
		return err
	}
	found := false
	for _, migration := range history {
		body, readErr := migrations.FS.ReadFile("prisma/migrations/" + migration.Name + "/migration.sql")
		if readErr != nil {
			return readErr
		}
		tx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		if migration.Name == target {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("migration %s not found", target)
	}
	return nil
}

func openFreshHelmMigrationPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "kuberploy_helm_" + strings.ReplaceAll(id.New(), "-", "")
	adminConfig.ConnConfig.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adminPool.Exec(ctx, "CREATE DATABASE "+(pgx.Identifier{databaseName}).Sanitize()); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	adminPool.Close()

	targetConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	targetConfig.ConnConfig.Database = databaseName
	targetPool, err := pgxpool.NewWithConfig(ctx, targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		targetPool.Close()
		dropConfig, dropErr := pgxpool.ParseConfig(databaseURL)
		if dropErr != nil {
			t.Errorf("parse Helm migration cleanup connection: %v", dropErr)
			return
		}
		dropConfig.ConnConfig.Database = "postgres"
		dropPool, dropErr := pgxpool.NewWithConfig(context.Background(), dropConfig)
		if dropErr != nil {
			t.Errorf("open Helm migration cleanup connection: %v", dropErr)
			return
		}
		defer dropPool.Close()
		if _, dropErr = dropPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+(pgx.Identifier{databaseName}).Sanitize()); dropErr != nil {
			t.Errorf("drop Helm migration database: %v", dropErr)
		}
	})
	return targetPool
}

func registerHelmAuthorityCleanup(t *testing.T, pool *pgxpool.Pool, platformBindingID string, workerPrefixes ...string) {
	t.Helper()
	patterns := make([]string, len(workerPrefixes))
	for index, prefix := range workerPrefixes {
		patterns[index] = prefix + "%"
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM public.runtime_readiness WHERE worker_id LIKE ANY($1::text[])`, patterns); err != nil {
			t.Errorf("clean up Helm authority readiness: %v", err)
		}
		cascadeTables := []string{
			"helm_application_cascade_absence_receipts",
			"helm_application_cascade_adoption_receipts",
			"helm_application_cascade_receipts",
			"helm_application_cascade_observation_jobs",
			"helm_application_cascade_preflights",
			"helm_application_cascade_observer_activations",
			"helm_protected_application_intents",
		}
		for _, table := range cascadeTables {
			if _, err := pool.Exec(cleanupCtx, "ALTER TABLE public."+table+" DISABLE TRIGGER USER"); err != nil {
				t.Errorf("disable %s cleanup trigger: %v", table, err)
				return
			}
		}
		preflightIDs := `SELECT id FROM public.helm_application_cascade_preflights WHERE platform_binding_id=$1`
		for _, statement := range []string{
			`UPDATE public.helm_protected_application_intents
				SET cascade_required=false,cascade_receipt_id=NULL,cascade_contract=''
				WHERE cascade_receipt_id IN (` + preflightIDs + `)`,
			`DELETE FROM public.helm_application_cascade_absence_receipts WHERE cascade_preflight_id IN (` + preflightIDs + `)`,
			`DELETE FROM public.helm_application_cascade_adoption_receipts WHERE cascade_preflight_id IN (` + preflightIDs + `)`,
			`DELETE FROM public.helm_application_cascade_receipts WHERE cascade_preflight_id IN (` + preflightIDs + `)`,
			`DELETE FROM public.helm_application_cascade_observation_jobs WHERE platform_binding_id=$1`,
			`DELETE FROM public.helm_application_cascade_preflights WHERE platform_binding_id=$1`,
		} {
			if _, err := pool.Exec(cleanupCtx, statement, platformBindingID); err != nil {
				t.Errorf("clean up Helm cascade state: %v", err)
				break
			}
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM public.helm_application_cascade_observer_activations WHERE platform_binding_id=$1`, platformBindingID); err != nil {
			t.Errorf("clean up Helm authority activation: %v", err)
		}
		for _, table := range cascadeTables {
			if _, err := pool.Exec(cleanupCtx, "ALTER TABLE public."+table+" ENABLE TRIGGER USER"); err != nil {
				t.Errorf("restore %s cleanup trigger: %v", table, err)
			}
		}
	})
}

func helmPGDatabaseNow(t *testing.T, ctx context.Context, q helmPGQuerier) time.Time {
	t.Helper()
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type helmPGQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func assertProtectedIntentValidatorsAreHardened(t *testing.T, ctx context.Context, query helmPGQuerier) {
	t.Helper()
	for _, function := range []string{
		"public.validate_helm_protected_payload_intent()",
		"public.validate_helm_protected_application_intent()",
	} {
		var settings []string
		var definition string
		if err := query.QueryRow(ctx, `SELECT function.proconfig,pg_catalog.pg_get_functiondef(function.oid)
			FROM pg_catalog.pg_proc function WHERE function.oid=$1::pg_catalog.regprocedure`, function).
			Scan(&settings, &definition); err != nil {
			t.Fatal(err)
		}
		if len(settings) != 1 || settings[0] != "search_path=pg_catalog, pg_temp" {
			t.Fatalf("%s search path=%v", function, settings)
		}
		for _, ambient := range []string{
			" helm_release_revisions%ROWTYPE", " helm_protected_payload_intents%ROWTYPE",
			" helm_protected_application_intents%ROWTYPE", " git_repository_bindings%ROWTYPE",
			"FROM helm_release_revisions", "FROM helm_protected_payload_intents",
			"FROM helm_release_heads", "FROM git_repository_bindings",
			"FROM helm_protected_application_intents", "FROM git_projection_generations",
			"FROM git_projected_documents", "FROM helm_render_results", "JOIN helm_render_commands",
		} {
			if strings.Contains(definition, ambient) {
				t.Fatalf("%s retains ambient dependency %q", function, ambient)
			}
		}
	}
}

func expectPGCheck(t *testing.T, ctx context.Context, tx pgx.Tx, call func(pgx.Tx) error) {
	t.Helper()
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = call(nested)
	_ = nested.Rollback(ctx)
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatalf("expected PostgreSQL check violation, got %v", err)
	}
}
