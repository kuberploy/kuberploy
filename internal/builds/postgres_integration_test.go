package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/buildpromotion"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/id"
	platformstore "github.com/kuberploy/kuberploy/internal/store"
	platformpostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgreSQLBuildOrchestrationParity(t *testing.T) {
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
	store, err := NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	runtimeIdentity := SourceBuildRuntimeIdentity{ConfigDigest: "sha256:" + strings.Repeat("9", 64), GitHubAppID: 987654,
		BuilderNamespace: "kuberploy-build-dind", BuilderAgentImage: "ghcr.io/kuberploy/builder@sha256:" + strings.Repeat("8", 64)}
	observation := SourceBuildWorkerObservation{WorkerID: "postgres-runtime-worker-01", SourceBuildRuntimeIdentity: runtimeIdentity,
		StartedAt: now.Add(-time.Minute), ObservedAt: now}
	if err = store.ObserveSourceBuildWorker(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err = store.SourceBuildRuntimeReady(ctx, runtimeIdentity, now, SourceBuildHeartbeatMaxAge); err != nil {
		t.Fatalf("fresh matching worker not ready: %v", err)
	}
	if err = store.SourceBuildRuntimeReady(ctx, runtimeIdentity, now.Add(SourceBuildHeartbeatMaxAge+time.Second), SourceBuildHeartbeatMaxAge); !errors.Is(err, ErrRuntimeNotReady) {
		t.Fatalf("stale worker remained ready: %v", err)
	}
	userID, projectID, serviceID, installationID, repositoryID, registryID, definitionID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	providerInstall := now.UnixNano() & 0x3fffffffffffffff
	providerRepo := providerInstall + 1
	accountID := providerInstall + 2
	appID := providerInstall + 3
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin',$3,$4,1,$5)`, userID, "Build Test "+suffix, "build-test-"+suffix, "subject-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at) VALUES($1,$2,$3,$4)`, projectID, "Build Test "+suffix, "build-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'Service','service',$3)`, serviceID, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO github_installations(id,github_installation_id,account_login,account_type,owner_user_id,visibility,repository_selection,repository_count,created_at,updated_at) VALUES($1,$2,'kuberploy','Organization',$3,'private','selected',1,$4,$4)`, installationID, providerInstall, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,push_credential_ref,cache_credential_ref,created_at,updated_at) VALUES($1,$2,'managed','registry.test','kuberploy','registry-push','registry-cache',$3,$3)`, registryID, "registry-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	installation := Installation{ID: installationID, AppID: appID, GitHubInstallationID: providerInstall, Account: githubapp.AccountIdentity{ID: accountID, Login: "kuberploy", Type: "Organization"}, RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}, Lifecycle: InstallationActive, LastVerifiedAt: now, UpdatedAt: now}
	if err = store.PutInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	repository := Repository{ID: repositoryID, InstallationID: installationID, Identity: githubapp.RepositoryIdentity{ID: providerRepo, Name: "demo", OwnerID: accountID, OwnerLogin: "kuberploy"}, Lifecycle: RepositoryActive, LastVerifiedAt: now, CreatedAt: now, UpdatedAt: now}
	if err = store.PutRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	definition := definitionWithIDs(t, now, RegistryManaged, definitionID, projectID, serviceID, installationID, repositoryID, registryID)
	if err = store.PutDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	githubUser := githubapp.AccountIdentity{ID: providerInstall + 100, Login: "build-user", Type: "User"}
	if err = store.BindGitHubUser(ctx, userID, githubUser, now); err != nil {
		t.Fatal(err)
	}
	githubUser.Login = "build-user-renamed"
	if err = store.BindGitHubUser(ctx, userID, githubUser, now.Add(time.Second)); err != nil {
		t.Fatalf("same immutable GitHub user could not refresh login: %v", err)
	}
	if err = store.BindGitHubUser(ctx, userID, githubapp.AccountIdentity{ID: githubUser.ID + 1, Login: "other-user", Type: "User"}, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("actor rebound to a different GitHub user: %v", err)
	}
	authorization := SetupAuthorization{ActorID: userID, IdempotencyKey: "postgres-setup-auth-01",
		RequestFingerprint: "sha256:" + strings.Repeat("1", 64), State: strings.Repeat("s", 64), ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now}
	if _, replay, putErr := store.PutSetupAuthorization(ctx, authorization); putErr != nil || replay {
		t.Fatalf("setup authorization replay=%v err=%v", replay, putErr)
	}
	if storedAuthorization, replay, putErr := store.PutSetupAuthorization(ctx, authorization); putErr != nil || !replay || storedAuthorization.State != authorization.State {
		t.Fatalf("setup authorization replay=%#v replay=%v err=%v", storedAuthorization, replay, putErr)
	}
	handoffDigest := sha256.Sum256([]byte("postgres-setup-handoff\x00" + definitionID))
	handoff := SetupHandoff{Digest: handoffDigest, ActorID: userID, GitHubUser: githubUser,
		Installation: githubapp.Installation{ID: providerInstall, AppID: appID, Account: installation.Account,
			RepositorySelection: installation.RepositorySelection, Permissions: installation.Permissions},
		Repositories: []githubapp.RepositoryIdentity{repository.Identity}, ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now}
	if err = store.PutSetupHandoff(ctx, handoff); err != nil {
		t.Fatal(err)
	}
	consumed, replay, err := store.ConsumeSetupHandoff(ctx, handoffDigest, userID, "postgres-setup-link-01",
		"sha256:"+strings.Repeat("2", 64), now.Add(time.Second))
	if err != nil || replay || consumed.Installation.ID != providerInstall {
		t.Fatalf("handoff consumed=%#v replay=%v err=%v", consumed, replay, err)
	}
	if consumed, replay, err = store.ConsumeSetupHandoff(ctx, handoffDigest, userID, "postgres-setup-link-01",
		"sha256:"+strings.Repeat("2", 64), now.Add(2*time.Second)); err != nil || !replay {
		t.Fatalf("handoff replay=%#v replay=%v err=%v", consumed, replay, err)
	}
	if err = store.CompleteSetupHandoff(ctx, handoffDigest, installationID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	apiResourceID := id.New()
	if got, replay, claimErr := store.ClaimAPICommand(ctx, userID, APICommandDefinitionCreate, serviceID, "postgres-api-command-01",
		"sha256:"+strings.Repeat("3", 64), apiResourceID, now); claimErr != nil || replay || got != apiResourceID {
		t.Fatalf("api command resource=%q replay=%v err=%v", got, replay, claimErr)
	}
	if got, replay, claimErr := store.ClaimAPICommand(ctx, userID, APICommandDefinitionCreate, serviceID, "postgres-api-command-01",
		"sha256:"+strings.Repeat("3", 64), id.New(), now); claimErr != nil || !replay || got != apiResourceID {
		t.Fatalf("api command replay resource=%q replay=%v err=%v", got, replay, claimErr)
	}
	event := githubapp.PushEvent{Ref: "refs/heads/main", UntrustedAfter: strings.Repeat("a", 40), Repository: repository.Identity, InstallationID: providerInstall, Sender: githubapp.AccountIdentity{ID: providerInstall + 4, Login: "sender", Type: "User"}}
	typed, _ := json.Marshal(event)
	claimDigest := sha256.Sum256([]byte("postgres-build-contract\x00" + definitionID))
	claimKey := hex.EncodeToString(claimDigest[:])
	receipt := DeliveryReceipt{AppID: appID, GitHubInstallationID: providerInstall, DeliveryID: id.New(), Event: "push", BodySHA256: "sha256:" + strings.Repeat("d", 64), TypedEvent: typed, RepositoryID: providerRepo, GitRef: event.Ref, State: DeliveryClaimed, AvailableAt: now, ReceivedAt: now, UpdatedAt: now}
	claim := githubapp.OneTimeClaim{Kind: "github-delivery", ClaimKey: claimKey, RetainUntil: now.Add(24 * time.Hour), Permanent: true}
	inserted, err := store.ClaimDelivery(ctx, claim, receipt)
	if err != nil || !inserted {
		t.Fatalf("claim inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.ClaimDelivery(ctx, claim, receipt)
	if err != nil || inserted {
		t.Fatalf("replay inserted=%v err=%v", inserted, err)
	}
	_, acquired, err := store.AcquireDelivery(ctx, claimKey, "postgres-contract", now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	authorized, err := store.AuthorizePush(ctx, appID, providerInstall, repository.Identity, event.Ref)
	if err != nil || len(authorized.Definitions) != 1 {
		t.Fatalf("authorized=%#v err=%v", authorized, err)
	}
	lifecycleEvent := githubapp.InstallationEvent{Action: "suspend", InstallationID: providerInstall, Account: installation.Account, RepositorySelection: "selected", Permissions: installation.Permissions}
	if err = store.ApplyInstallationEvent(ctx, appID, lifecycleEvent, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnqueuePushBuilds(ctx, EnqueuePush{ClaimKey: claimKey, CommitSHA: strings.Repeat("b", 40), GitRef: event.Ref, ResolvedAt: now}, "postgres-contract", storedAttemptDefinitions(authorized.Definitions), now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("suspended installation enqueued: %v", err)
	}
	lifecycleEvent.Action = "unsuspend"
	if err = store.ApplyInstallationEvent(ctx, appID, lifecycleEvent, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.EnqueuePushBuilds(ctx, EnqueuePush{ClaimKey: claimKey, CommitSHA: strings.Repeat("b", 40), GitRef: event.Ref, ResolvedAt: now}, "postgres-contract", storedAttemptDefinitions(authorized.Definitions), now)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%#v err=%v", attempts, err)
	}
	// Build-log access is authorized and audited by the central store, not by
	// the build catalog. Exercise the exact PostgreSQL transaction against this
	// freshly persisted attempt: viewer build visibility alone is insufficient,
	// logs.read is additive, and revocation cannot leave an audit success behind.
	viewerID, viewerGrantID := id.New(), id.New()
	// Login identities use the legacy developer/platform-admin column; the
	// effective viewer role is carried only by the scoped access grant below.
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'developer',$3,$4,1,$5)`, viewerID, "Build Log Viewer "+suffix, "build-log-test-"+suffix, "build-log-subject-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,permissions,source,created_by,created_at)
		VALUES($1,$2,'viewer','application',$3,ARRAY['logs.read']::text[],'explicit',$2,$4)`, viewerGrantID, viewerID, serviceID, now); err != nil {
		t.Fatal(err)
	}
	platformStore, err := platformpostgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer platformStore.Close()
	if err = platformStore.AuditBuildLogAccess(ctx, viewerID, attempts[0].ID, "build.logs.snapshot", "postgres-build-log-audit-01"); err != nil {
		t.Fatalf("authorized build log access failed: %v", err)
	}
	var buildLogAuditCount int
	var buildLogAuditSource string
	if err = pool.QueryRow(ctx, `SELECT count(*),COALESCE(max(detail->>'source'),'') FROM audit_events
		WHERE actor_id=$1 AND target_type='build-attempt' AND target_id=$2 AND action='build.logs.snapshot' AND request_id='postgres-build-log-audit-01'`, viewerID, attempts[0].ID).Scan(&buildLogAuditCount, &buildLogAuditSource); err != nil || buildLogAuditCount != 1 || buildLogAuditSource != "kubernetes-live" {
		t.Fatalf("audit count=%d source=%q err=%v", buildLogAuditCount, buildLogAuditSource, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE access_grants SET permissions=ARRAY[]::text[] WHERE id=$1`, viewerGrantID); err != nil {
		t.Fatal(err)
	}
	if err = platformStore.AuditBuildLogAccess(ctx, viewerID, attempts[0].ID, "build.logs.follow", "postgres-build-log-audit-02"); !errors.Is(err, platformstore.ErrForbidden) {
		t.Fatalf("revoked logs.read still reached build logs: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND target_type='build-attempt' AND target_id=$2`, viewerID, attempts[0].ID).Scan(&buildLogAuditCount); err != nil || buildLogAuditCount != 1 {
		t.Fatalf("failed access wrote an audit success: count=%d err=%v", buildLogAuditCount, err)
	}
	purged, err := store.PurgeExpiredDeliveryPayloads(ctx, claim.RetainUntil.Add(time.Second))
	if err != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
	storedReceipt, err := store.Delivery(ctx, claimKey)
	if err != nil || storedReceipt.State != DeliveryEnqueued || len(storedReceipt.TypedEvent) != 0 {
		t.Fatalf("stored receipt=%#v err=%v", storedReceipt, err)
	}
	inserted, err = store.ClaimDelivery(ctx, claim, receipt)
	if err != nil || inserted {
		t.Fatalf("purged replay inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.ClaimOnce(ctx, claim)
	if err != nil || inserted {
		t.Fatalf("permanent tombstone inserted=%v err=%v", inserted, err)
	}
	attempt, err := store.ClaimNextAttempt(ctx, "postgres-builder", now, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ID != attempts[0].ID || attempt.ExecutionAttempts != 1 {
		t.Fatalf("attempt=%#v", attempt)
	}
	if err = store.MarkAttemptRunning(ctx, attempt.ID, "postgres-builder", now); err != nil {
		t.Fatal(err)
	}
	result := builder.BuildResult{APIVersion: builder.ProtocolVersion, OperationID: attempt.ID, Generation: attempt.Generation, Status: "Succeeded", Image: builder.Image{Reference: attempt.PlanRequest.Build.Destination.Repository + "@sha256:" + strings.Repeat("e", 64), Digest: "sha256:" + strings.Repeat("e", 64), Platforms: attempt.PlanRequest.Build.Platforms}, Cache: &builder.Cache{Reference: cacheReference(attempt), Digest: "sha256:" + strings.Repeat("f", 64)}, StartedAt: now, CompletedAt: now.Add(time.Minute)}
	if err = store.CompleteAttempt(ctx, attempt.ID, "postgres-builder", BuildCompletion{Result: result, CacheReference: cacheReference(attempt), LogReference: "k8s://kuberploy-build-dind/pods/build-pod/containers/agent"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Attempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != AttemptSucceeded || stored.Result == nil || stored.CacheReference == "" {
		t.Fatalf("stored=%#v", stored)
	}
	if _, promotionErr := store.SuccessfulReleaseProjection(ctx, attempt.ID); !errors.Is(promotionErr, buildpromotion.ErrNotReady) {
		t.Fatalf("unprojected PostgreSQL attempt was promotable: %v", promotionErr)
	}
	projectionNow := now.Add(time.Minute)
	projection, err := store.ClaimNextReleaseProjection(ctx, "postgres-release-projector", projectionNow, time.Minute)
	if err != nil || projection.Attempt.ID != attempt.ID || projection.Definition.ID != definition.ID || projection.Attempts != 1 {
		t.Fatalf("release projection=%#v err=%v", projection, err)
	}
	staleLease := projection.Lease
	staleLease.Epoch++
	if _, err = store.HeartbeatReleaseProjection(ctx, staleLease, projectionNow.Add(time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale projection lease survived: %v", err)
	}
	projection.Lease, err = store.HeartbeatReleaseProjection(ctx, projection.Lease, projectionNow.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	projectionRetryAt := projectionNow.Add(10 * time.Second)
	if retry, retryErr := store.RetryReleaseProjection(ctx, projection.Lease, "registry-release-write-failed", projectionNow.Add(2*time.Second), projectionRetryAt); retryErr != nil || !retry {
		t.Fatalf("projection retry=%v err=%v", retry, retryErr)
	}
	projection, err = store.ClaimNextReleaseProjection(ctx, "postgres-release-projector", projectionRetryAt, time.Minute)
	if err != nil || projection.Attempts != 2 {
		t.Fatalf("release projection reclaim=%#v err=%v", projection, err)
	}
	if err = store.CompleteReleaseProjection(ctx, projection.Lease, attempt.ID, attempt.ID, projectionRetryAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var projectionState string
	var releaseID, cacheID *string
	if err = pool.QueryRow(ctx, `SELECT state,release_id::text,cache_generation_id::text FROM build_release_projections WHERE attempt_id=$1`, attempt.ID).Scan(&projectionState, &releaseID, &cacheID); err != nil || projectionState != "succeeded" || releaseID == nil || *releaseID != attempt.ID || cacheID == nil || *cacheID != attempt.ID {
		t.Fatalf("projection state=%q release=%v cache=%v err=%v", projectionState, releaseID, cacheID, err)
	}
	promotable, err := store.SuccessfulReleaseProjection(ctx, attempt.ID)
	if err != nil || promotable.AttemptID != attempt.ID || promotable.ReleaseID != attempt.ID ||
		promotable.ProjectID != projectID || promotable.ApplicationID != serviceID ||
		promotable.RegistryTargetID != registryID || promotable.ImageReference != result.Image.Reference ||
		promotable.ImageDigest != result.Image.Digest || promotable.Repository == "" ||
		!promotable.CompletedAt.Equal(result.CompletedAt) || !promotable.ProjectionCompletedAt.Equal(projectionRetryAt.Add(time.Second)) {
		t.Fatalf("PostgreSQL promotion projection=%#v err=%v", promotable, err)
	}
	outbox, err := store.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range outbox {
		if message.AttemptID == attempt.ID {
			found = true
			if message.TraceID != claimKey {
				t.Fatalf("trace=%s", message.TraceID)
			}
		}
	}
	if !found {
		t.Fatalf("outbox missing attempt %s", attempt.ID)
	}

	// Exercise the same immutable retry and cancellation transitions used by
	// the memory contract against row locks and PostgreSQL lease predicates.
	retryNow := now.Add(2 * time.Hour)
	retryDigest := sha256.Sum256([]byte("postgres-build-retry-contract\x00" + definitionID))
	retryClaim := githubapp.OneTimeClaim{Kind: "github-delivery", ClaimKey: hex.EncodeToString(retryDigest[:]), RetainUntil: retryNow.Add(24 * time.Hour), Permanent: true}
	retryReceipt := receipt
	retryReceipt.DeliveryID = id.New()
	retryReceipt.AvailableAt, retryReceipt.ReceivedAt, retryReceipt.UpdatedAt = retryNow, retryNow, retryNow
	inserted, err = store.ClaimDelivery(ctx, retryClaim, retryReceipt)
	if err != nil || !inserted {
		t.Fatalf("retry claim inserted=%v err=%v", inserted, err)
	}
	if _, acquired, err = store.AcquireDelivery(ctx, retryClaim.ClaimKey, "postgres-contract", retryNow, time.Minute); err != nil || !acquired {
		t.Fatalf("retry acquired=%v err=%v", acquired, err)
	}
	retryAuthorized, err := store.AuthorizePush(ctx, appID, providerInstall, repository.Identity, event.Ref)
	if err != nil || len(retryAuthorized.Definitions) != 1 {
		t.Fatalf("retry authorized=%#v err=%v", retryAuthorized, err)
	}
	retryAttempts, err := store.EnqueuePushBuilds(ctx, EnqueuePush{ClaimKey: retryClaim.ClaimKey, CommitSHA: strings.Repeat("c", 40), GitRef: event.Ref, ResolvedAt: retryNow}, "postgres-contract", storedAttemptDefinitions(retryAuthorized.Definitions), retryNow)
	if err != nil || len(retryAttempts) != 1 {
		t.Fatalf("retry attempts=%#v err=%v", retryAttempts, err)
	}
	retryAttempt, err := store.ClaimNextAttempt(ctx, "postgres-retry", retryNow, time.Minute)
	if err != nil || retryAttempt.ID != retryAttempts[0].ID || retryAttempt.ExecutionAttempts != 1 {
		t.Fatalf("retry attempt=%#v err=%v", retryAttempt, err)
	}
	providerRetryAt := retryNow.Add(10 * time.Second)
	if err = store.DeferAttempt(ctx, retryAttempt.ID, "postgres-retry", "github-provider-retry", retryNow, providerRetryAt); err != nil {
		t.Fatal(err)
	}
	retryAttempt, err = store.ClaimNextAttempt(ctx, "postgres-retry", providerRetryAt, time.Minute)
	if err != nil || retryAttempt.State != AttemptPreparing || retryAttempt.ExecutionAttempts != 1 || retryAttempt.FailureCode != "github-provider-retry" {
		t.Fatalf("provider-deferred attempt=%#v err=%v", retryAttempt, err)
	}
	retryAt := retryNow.Add(30 * time.Second)
	if scheduled, scheduleErr := store.ScheduleAttemptRetry(ctx, retryAttempt.ID, "postgres-retry", "kubernetes-ensure-failed", providerRetryAt, retryAt); scheduleErr != nil || !scheduled {
		t.Fatalf("retry scheduled=%v err=%v", scheduled, scheduleErr)
	}
	queued, err := store.Attempt(ctx, retryAttempt.ID)
	if err != nil || queued.State != AttemptQueued || queued.ID != retryAttempt.ID || queued.Generation != retryAttempt.Generation || queued.InputDigest != retryAttempt.InputDigest || queued.CacheCandidate != retryAttempt.CacheCandidate || queued.PlanRequest.Build.Destination != retryAttempt.PlanRequest.Build.Destination {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	retried, err := store.ClaimNextAttempt(ctx, "postgres-retry", retryAt, time.Minute)
	if err != nil || retried.ID != retryAttempt.ID || retried.ExecutionAttempts != 2 {
		t.Fatalf("retried=%#v err=%v", retried, err)
	}
	if _, err = store.RequestCancel(ctx, retried.ID, retryAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	cancelRetryAt := retryAt.Add(20 * time.Second)
	if scheduled, scheduleErr := store.ScheduleAttemptRetry(ctx, retried.ID, "postgres-retry", "kubernetes-cancel-failed", retryAt.Add(2*time.Second), cancelRetryAt); scheduleErr != nil || !scheduled {
		t.Fatalf("cancel scheduled=%v err=%v", scheduled, scheduleErr)
	}
	cancelling, err := store.ClaimNextAttempt(ctx, "postgres-cancel", cancelRetryAt, time.Minute)
	if err != nil || cancelling.ID != retried.ID || cancelling.State != AttemptCancelling {
		t.Fatalf("cancelling=%#v err=%v", cancelling, err)
	}
	if err = store.CompleteCancellation(ctx, cancelling.ID, "postgres-cancel", cancelRetryAt); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.Attempt(ctx, cancelling.ID)
	if err != nil || cancelled.State != AttemptCancelled || cancelled.FailureCode != "" {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	manualRetryKey := "postgres-manual-retry-01"
	manualClaimKey := APICommandClaimKey(userID, APICommandAttemptRetry, cancelled.ID, manualRetryKey)
	manualRetryID := RetryAttemptID(manualClaimKey, cancelled.DefinitionID)
	currentExecution := definition.Spec.Execution
	currentExecution.BuilderAgentImage = "registry.test/system/builder-agent@sha256:" + strings.Repeat("9", 64)
	if got, replay, claimErr := store.ClaimAPICommand(ctx, userID, APICommandAttemptRetry, cancelled.ID, manualRetryKey,
		"sha256:"+strings.Repeat("4", 64), manualRetryID, cancelRetryAt.Add(time.Minute)); claimErr != nil || replay || got != manualRetryID {
		t.Fatalf("manual retry claim resource=%q replay=%v err=%v", got, replay, claimErr)
	}
	manualRetry, replay, err := store.RetryAttempt(ctx, cancelled.ID, manualRetryID, manualClaimKey, currentExecution, cancelRetryAt.Add(time.Minute))
	if err != nil || replay || manualRetry.ID != manualRetryID || manualRetry.Generation <= cancelled.Generation || manualRetry.CommitSHA != cancelled.CommitSHA || manualRetry.GitRef != cancelled.GitRef {
		t.Fatalf("manual retry=%#v replay=%v err=%v", manualRetry, replay, err)
	}
	if manualRetry.PlanRequest.AgentImage != currentExecution.BuilderAgentImage || manualRetry.PlanRequest.AgentImage == cancelled.PlanRequest.AgentImage {
		t.Fatalf("manual retry agent image=%q, want refreshed operator runtime %q", manualRetry.PlanRequest.AgentImage, currentExecution.BuilderAgentImage)
	}
	if replayedRetry, replay, retryErr := store.RetryAttempt(ctx, cancelled.ID, manualRetryID, manualClaimKey, definition.Spec.Execution, cancelRetryAt.Add(2*time.Minute)); retryErr != nil || !replay || replayedRetry.ID != manualRetry.ID || replayedRetry.PlanRequest.AgentImage != currentExecution.BuilderAgentImage {
		t.Fatalf("manual retry replay=%#v replay=%v err=%v", replayedRetry, replay, retryErr)
	}

	// External targets persist and plan through the identical closed builder
	// contract; only the registry mode/lifecycle ownership differs.
	externalServiceID, externalRegistryID, externalDefinitionID := id.New(), id.New(), id.New()
	if _, err = pool.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'External Service',$3,$4)`, externalServiceID, projectID, "external-"+suffix, retryNow); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,push_credential_ref,cache_credential_ref,created_at,updated_at) VALUES($1,$2,'external','registry.test','kuberploy','registry-push','registry-cache',$3,$3)`, externalRegistryID, "external-registry-"+suffix, retryNow); err != nil {
		t.Fatal(err)
	}
	externalDefinition := definitionWithIDs(t, retryNow, RegistryExternal, externalDefinitionID, projectID, externalServiceID, installationID, repositoryID, externalRegistryID)
	if err = store.PutDefinition(ctx, externalDefinition); err != nil {
		t.Fatal(err)
	}
	externalNow := retryNow.Add(time.Hour)
	externalDigest := sha256.Sum256([]byte("postgres-external-registry-contract\x00" + externalDefinitionID))
	externalClaim := githubapp.OneTimeClaim{Kind: "github-delivery", ClaimKey: hex.EncodeToString(externalDigest[:]), RetainUntil: externalNow.Add(24 * time.Hour), Permanent: true}
	externalReceipt := receipt
	externalReceipt.DeliveryID = id.New()
	externalReceipt.AvailableAt, externalReceipt.ReceivedAt, externalReceipt.UpdatedAt = externalNow, externalNow, externalNow
	if inserted, err = store.ClaimDelivery(ctx, externalClaim, externalReceipt); err != nil || !inserted {
		t.Fatalf("external claim inserted=%v err=%v", inserted, err)
	}
	if _, acquired, err = store.AcquireDelivery(ctx, externalClaim.ClaimKey, "postgres-contract", externalNow, time.Minute); err != nil || !acquired {
		t.Fatalf("external acquired=%v err=%v", acquired, err)
	}
	externalAuthorized, err := store.AuthorizePush(ctx, appID, providerInstall, repository.Identity, event.Ref)
	if err != nil || len(externalAuthorized.Definitions) != 2 {
		t.Fatalf("external authorized=%#v err=%v", externalAuthorized, err)
	}
	externalAttempts, err := store.EnqueuePushBuilds(ctx, EnqueuePush{ClaimKey: externalClaim.ClaimKey, CommitSHA: strings.Repeat("d", 40), GitRef: event.Ref, ResolvedAt: externalNow}, "postgres-contract", storedAttemptDefinitions(externalAuthorized.Definitions), externalNow)
	if err != nil || len(externalAttempts) != 2 {
		t.Fatalf("external attempts=%#v err=%v", externalAttempts, err)
	}
	modes := map[RegistryMode]bool{}
	for _, candidate := range externalAttempts {
		modes[candidate.RegistryMode] = true
		if candidate.PlanRequest.Build.Cache.CandidateExport == "" || candidate.PlanRequest.Build.Cache.Schema != "v1" || candidate.PlanRequest.Build.Cache.BuildDefinition != candidate.DefinitionDigest {
			t.Fatalf("candidate cache=%#v", candidate.PlanRequest.Build.Cache)
		}
	}
	if !modes[RegistryManaged] || !modes[RegistryExternal] {
		t.Fatalf("registry modes=%v", modes)
	}
	externalOutbox, err := store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	externalMessages := 0
	for _, message := range externalOutbox {
		if message.TraceID == externalClaim.ClaimKey {
			externalMessages++
		}
	}
	if externalMessages != len(externalAttempts) {
		t.Fatalf("external outbox messages=%d attempts=%d", externalMessages, len(externalAttempts))
	}
	if _, err = pool.Exec(ctx, `DELETE FROM github_one_time_claims WHERE kind='github-delivery' AND claim_key=$1`, claimKey); err == nil {
		t.Fatal("database allowed permanent tombstone deletion")
	}
}
