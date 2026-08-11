package helmapps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

type helmReleasePGFixture struct {
	userID, projectID, environmentID, applicationID                string
	approvalID, platformBindingID, environmentBindingID, clusterID string
	namespace, argoProject                                         string
	ociRepository                                                  string
	platformHead, environmentHead, catalogDigest, publisherDigest  string
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

	applicationOne, applicationOneDigest := id.New(), helmPGDigest([]byte("application-one"))
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
	f := newHelmReleasePGFixture()
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
	store, err := NewPostgresProtectedPublicationStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.NextPayloadCandidate(ctx)
	if err != nil || candidate.Kind != PublicationPayload || candidate.ReleaseRevisionID != release.ID || candidate.Target != target {
		t.Fatalf("payload candidate=%+v err=%v", candidate, err)
	}
	publisher := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: f.publisherDigest}
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
	if _, _, err = store.ClaimPayload(ctx, "helm-publisher-worker-0001", wrongPublisher,
		f.now.Add(5*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong publisher claimed work: %v", err)
	}
	payload, firstLease, err := store.ClaimPayload(ctx, "helm-publisher-worker-0001", publisher,
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
	payload, lease, err := store.ClaimPayload(ctx, "helm-publisher-worker-0002", publisher,
		retryAt, time.Minute)
	if err != nil || lease.Epoch != 2 || payload.Attempts != 2 {
		t.Fatalf("reclaimed payload=%+v lease=%+v err=%v", payload, lease, err)
	}
	observedAt := retryAt.Add(time.Second)
	payload, err = store.BindPayloadWriteBase(ctx, lease, f.platformHead, observedAt, observedAt)
	if err != nil || payload.WriteBaseRevision != f.platformHead {
		t.Fatalf("bound payload=%+v err=%v", payload, err)
	}
	payloadCommit := strings.Repeat("b", 40)
	payload, err = store.MarkPayloadCommitted(ctx, lease, payloadCommit, f.platformHead,
		observedAt.Add(time.Second))
	if err != nil || payload.State != ProtectedGitCommitted {
		t.Fatalf("committed payload=%+v err=%v", payload, err)
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
	candidate, err = store.NextApplicationCandidate(ctx)
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
	applicationID := id.New()
	application, replay, err := store.CreateApplicationForPayload(ctx, applicationID, payload.ID,
		ProtectedApplicationRuntime{ArgoNamespace: "argocd"}, publisher, observedAt.Add(4*time.Second))
	if err != nil || replay || application.State != ProtectedPending ||
		application.PayloadRevision != payloadCommit || application.Operation != "create" {
		t.Fatalf("application=%+v replay=%v err=%v", application, replay, err)
	}
	if candidate, candidateErr := store.NextApplicationCandidate(ctx); !errors.Is(candidateErr, ErrNotFound) {
		t.Fatalf("planned application remained a candidate: %+v err=%v", candidate, candidateErr)
	}
	if bytes := string(application.Content); strings.Contains(bytes, "targetRevision: refs/") ||
		!strings.Contains(bytes, "targetRevision: "+payloadCommit) || strings.Contains(bytes, "helm:") {
		t.Fatalf("unsafe Application source:\n%s", bytes)
	}
	application, applicationLease, err := store.ClaimApplication(ctx,
		"helm-publisher-worker-0003", publisher, observedAt.Add(5*time.Second), time.Minute)
	if err != nil || application.ID != applicationID {
		t.Fatalf("claimed application=%+v lease=%+v err=%v", application, applicationLease, err)
	}
	application, err = store.BindApplicationWriteBase(ctx, applicationLease, payloadCommit,
		observedAt.Add(6*time.Second), observedAt.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	applicationCommit := strings.Repeat("c", 40)
	application, err = store.MarkApplicationCommitted(ctx, applicationLease, applicationCommit,
		payloadCommit, observedAt.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	application, err = store.VerifyApplication(ctx, applicationLease, applicationCommit,
		application.ContentDigest, "provider-application-verified", observedAt.Add(8*time.Second))
	if err != nil || application.State != ProtectedVerified {
		t.Fatalf("verified application=%+v err=%v", application, err)
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
		platformHead: strings.Repeat("a", 40), environmentHead: strings.Repeat("1", 40),
		catalogDigest:   helmPGDigest([]byte("catalog")),
		publisherDigest: helmPGDigest([]byte("publisher")),
		values:          values, valuesDigest: helmPGDigest(values), schema: schema,
		schemaDigest: helmPGDigest(schema), manifest: manifest,
		manifestDigest: helmPGDigest(manifest), inventoryDigest: helmPGDigest([]byte("inventory")),
		now: time.Now().UTC().Truncate(time.Microsecond),
	}
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
		{`INSERT INTO users(id,login,role,issuer,subject,created_at)
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
			'refs/heads/main',$3,'','github-app','indexing',$4,0,'gitprojection.v1',$5,$5,$5)`,
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

func insertHelmApplication(ctx context.Context, tx pgx.Tx, f helmReleasePGFixture, application helmApplicationInsert, at time.Time) error {
	content := application.content
	if content == nil {
		content = []byte{}
	}
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

func claimHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID string, at time.Time) {
	t.Helper()
	requireHelmIntentTable(t, table)
	query := fmt.Sprintf(`UPDATE %s SET state='claimed',attempts=1,
		lease_owner='helm-publisher-worker-0001',lease_epoch=1,
			lease_until=$2::timestamptz+interval '5 minutes',updated_at=$2 WHERE id=$1`, table)
	if _, err := tx.Exec(ctx, query, intentID, at); err != nil {
		t.Fatal(err)
	}
}

func commitHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, parent, commit string, at time.Time) {
	t.Helper()
	requireHelmIntentTable(t, table)
	query := fmt.Sprintf(`UPDATE %s SET state='git-committed',write_base_revision=$2,
		write_base_observed_at=$4::timestamptz-interval '1 second',committed_revision=$3,
		committed_parent_revision=$2,committed_at=$4,updated_at=$4 WHERE id=$1`, table)
	if _, err := tx.Exec(ctx, query, intentID, parent, commit, at); err != nil {
		t.Fatal(err)
	}
}

func verifyHelmIntent(t *testing.T, ctx context.Context, tx pgx.Tx, table, intentID, pathDigest string, at time.Time) {
	t.Helper()
	requireHelmIntentTable(t, table)
	query := fmt.Sprintf(`UPDATE %s SET state='verified',lease_owner=NULL,lease_until=NULL,
		verified_at=$2,verified_path_digest=$3,provider_request=$4,
		completed_at=$2,updated_at=$2 WHERE id=$1`, table)
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

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
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
