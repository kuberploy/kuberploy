package builds

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestWebhookUsesAuthoritativeCommitAndExactlyOnceOutbox(t *testing.T) {
	for _, mode := range []RegistryMode{RegistryManaged, RegistryExternal} {
		t.Run(string(mode), func(t *testing.T) {
			store, definition := seedMemory(t, mode)
			clock := testNow
			provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
			envelope := testEnvelope(t, "11111111-2222-4333-8444-555555555555", strings.Repeat("a", 40), clock)
			service := webhookService(store, provider, envelope, &clock)
			first, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
			if err != nil {
				t.Fatal(err)
			}
			if first.State != DeliveryEnqueued || first.Replay || len(first.AttemptIDs) != 1 {
				t.Fatalf("first=%#v", first)
			}
			attempt, err := store.Attempt(context.Background(), first.AttemptIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			if attempt.CommitSHA != strings.Repeat("b", 40) || attempt.PlanRequest.Build.Commit != attempt.CommitSHA {
				t.Fatalf("untrusted commit reached build: %#v", attempt)
			}
			if attempt.RegistryMode != mode || attempt.PlanRequest.Build.Cache.CandidateExport == "" || !strings.Contains(attempt.PlanRequest.Build.Cache.CandidateExport, "/cache/v1/trusted/") {
				t.Fatalf("registry/cache input=%#v", attempt.PlanRequest.Build)
			}
			if attempt.PlanRequest.AgentImage != service.Runtime.BuilderAgentImage || attempt.PlanRequest.AgentImage == definition.Spec.Execution.BuilderAgentImage {
				t.Fatalf("new attempt did not snapshot the current operator runtime image: %q", attempt.PlanRequest.AgentImage)
			}
			second, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
			if err != nil {
				t.Fatal(err)
			}
			if second.State != DeliveryEnqueued || !second.Replay {
				t.Fatalf("replay=%#v", second)
			}
			outbox, err := store.PendingOutbox(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(outbox) != 1 || outbox[0].AttemptID != attempt.ID || outbox[0].TraceID != first.ClaimKey {
				t.Fatalf("outbox=%#v", outbox)
			}
		})
	}
}

func TestWebhookAcceptPersistsReceiptWithoutProviderWork(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	envelope := testEnvelope(t, "77777777-2222-4333-8444-555555555555", strings.Repeat("a", 40), clock)
	service := webhookService(store, provider, envelope, &clock)

	accepted, err := service.Accept(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil || accepted.State != DeliveryClaimed || accepted.Replay || accepted.ClaimKey == "" || len(accepted.AttemptIDs) != 0 {
		t.Fatalf("accepted=%#v err=%v", accepted, err)
	}
	provider.mu.Lock()
	providerCalls := provider.verifyCalls + provider.mintCalls + provider.resolveCalls
	provider.mu.Unlock()
	if providerCalls != 0 {
		t.Fatalf("receipt-only path contacted provider %d times", providerCalls)
	}
	stored, err := store.Delivery(context.Background(), accepted.ClaimKey)
	if err != nil || stored.State != DeliveryClaimed || len(stored.TypedEvent) == 0 {
		t.Fatalf("durable receipt=%#v err=%v", stored, err)
	}
	outbox, err := store.PendingOutbox(context.Background(), 10)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("HTTP receipt path created work directly: %#v err=%v", outbox, err)
	}
	replay, err := service.Accept(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil || !replay.Replay || replay.ClaimKey != accepted.ClaimKey || replay.State != DeliveryClaimed {
		t.Fatalf("receipt replay=%#v err=%v", replay, err)
	}
	resumed, err := service.ResumeDelivery(context.Background(), accepted.ClaimKey)
	if err != nil || resumed.State != DeliveryEnqueued || len(resumed.AttemptIDs) != 1 {
		t.Fatalf("worker resume=%#v err=%v", resumed, err)
	}
}

type recordingPushWaker struct {
	wakes []gitprojection.GitHubPushWake
	err   error
}

func (w *recordingPushWaker) Wake(_ context.Context, wake gitprojection.GitHubPushWake) (gitprojection.GitHubPushWakeResult, error) {
	w.wakes = append(w.wakes, wake)
	return gitprojection.GitHubPushWakeResult{Replay: len(w.wakes) > 1}, w.err
}

func TestAuthenticatedPushReceiptWakesProjectionWithoutTrustingAfterSHA(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	envelope := testEnvelope(t, "71717171-2222-4333-8444-555555555555", strings.Repeat("a", 40), clock)
	service := webhookService(store, &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}, envelope, &clock)
	waker := &recordingPushWaker{}
	service.PushWaker = waker
	accepted, err := service.Accept(t.Context(), http.Header{}, strings.NewReader("ignored"))
	if err != nil || accepted.ClaimKey == "" || len(waker.wakes) != 1 {
		t.Fatalf("accepted=%#v wakes=%#v err=%v", accepted, waker.wakes, err)
	}
	wake := waker.wakes[0]
	if wake.GitHubAppID != envelope.AppID || wake.InstallationID != testProviderInstall || wake.RepositoryID != testProviderRepo ||
		wake.TargetRef != "refs/heads/main" || wake.AfterCommit != strings.Repeat("a", 40) || wake.DeliveryHash != "sha256:"+accepted.ClaimKey {
		t.Fatalf("wake identity mismatch: %#v", wake)
	}
	if _, err = service.Accept(t.Context(), http.Header{}, strings.NewReader("ignored")); err != nil || len(waker.wakes) != 2 {
		t.Fatalf("durable receipt replay did not recover wake: calls=%d err=%v", len(waker.wakes), err)
	}
}

func TestTerminalDeliveryPayloadExpiresButPermanentTombstoneReplays(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	envelope := testEnvelope(t, "31313131-4242-4333-8444-515151515151", strings.Repeat("a", 40), clock)
	service := webhookService(store, provider, envelope, &clock)
	first, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil || first.State != DeliveryEnqueued || len(first.AttemptIDs) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}

	clock = envelope.ReplayUntil.Add(time.Second)
	purged, err := store.PurgeExpiredDeliveryPayloads(context.Background(), clock)
	if err != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
	receipt, err := store.Delivery(context.Background(), first.ClaimKey)
	if err != nil || receipt.State != DeliveryEnqueued || len(receipt.TypedEvent) != 0 {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	claim := githubapp.OneTimeClaim{Kind: "github-delivery", ClaimKey: first.ClaimKey, RetainUntil: envelope.ReplayUntil, Permanent: true}
	if inserted, claimErr := store.ClaimOnce(context.Background(), claim); claimErr != nil || inserted {
		t.Fatalf("permanent tombstone inserted=%v err=%v", inserted, claimErr)
	}
	resumed, err := service.ResumeDelivery(context.Background(), first.ClaimKey)
	if err != nil || resumed.State != DeliveryEnqueued || !resumed.Replay {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	replayed, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil || replayed.State != DeliveryEnqueued || !replayed.Replay {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	outbox, err := store.PendingOutbox(context.Background(), 10)
	if err != nil || len(outbox) != 1 || outbox[0].AttemptID != first.AttemptIDs[0] {
		t.Fatalf("outbox=%#v err=%v", outbox, err)
	}
}

func TestPendingDeliveryPayloadDoesNotExpire(t *testing.T) {
	store := NewMemoryStore()
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	envelope := testEnvelope(t, "61616161-7272-4333-8444-818181818181", strings.Repeat("a", 40), clock)
	service := webhookService(store, provider, envelope, &clock)
	outcome, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if !errors.Is(err, ErrProviderRetry) || outcome.State != DeliveryClaimed {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	purged, err := store.PurgeExpiredDeliveryPayloads(context.Background(), envelope.ReplayUntil.Add(time.Second))
	if err != nil || purged != 0 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
	receipt, err := store.Delivery(context.Background(), outcome.ClaimKey)
	if err != nil || len(receipt.TypedEvent) == 0 {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestWebhookReceiptResumesAfterProviderFailureWithoutRawPayload(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock, transient: 1}
	envelope := testEnvelope(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", strings.Repeat("a", 40), clock)
	service := webhookService(store, provider, envelope, &clock)
	first, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if !errors.Is(err, ErrProviderRetry) || first.State != DeliveryClaimed {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	receipt, err := store.Delivery(context.Background(), first.ClaimKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(receipt.TypedEvent), strings.Repeat("b", 40)) {
		t.Fatal("resolved source was fabricated in persisted event")
	}
	clock = clock.Add(31 * time.Second)
	provider.now = clock
	resumed, err := service.ResumeDelivery(context.Background(), first.ClaimKey)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != DeliveryEnqueued || !resumed.Replay || len(resumed.AttemptIDs) != 1 {
		t.Fatalf("resumed=%#v", resumed)
	}
}

func TestConcurrentDeliveryCreatesOneAttempt(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	envelope := testEnvelope(t, "99999999-8888-4777-8666-555555555555", strings.Repeat("a", 40), clock)
	service := webhookService(store, provider, envelope, &clock)
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent handle: %v", err)
		}
	}
	outbox, err := store.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 {
		t.Fatalf("outbox=%#v", outbox)
	}
}

func TestDeliveryIDCannotBeReboundToDifferentAuthenticatedBody(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	delivery := "12121212-3434-4567-8abc-909090909090"
	service := webhookService(store, provider, testEnvelope(t, delivery, strings.Repeat("a", 40), clock), &clock)
	if _, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored")); err != nil {
		t.Fatal(err)
	}
	changed := webhookService(store, provider, testEnvelope(t, delivery, strings.Repeat("d", 40), clock), &clock)
	if _, err := changed.Handle(context.Background(), http.Header{}, strings.NewReader("ignored")); !errors.Is(err, ErrConflict) {
		t.Fatalf("delivery rebound: %v", err)
	}
}

func TestAuthorizationRevokedDuringResolutionCannotReachOutbox(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	provider.resolveHook = func() {
		event := githubapp.InstallationEvent{
			Action: "suspend", InstallationID: testProviderInstall, Account: validInstallation(clock).Account,
			RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead},
		}
		if err := store.ApplyInstallationEvent(context.Background(), testAppID, event, clock.Add(time.Second)); err != nil {
			t.Errorf("suspend installation: %v", err)
		}
	}
	service := webhookService(store, provider, testEnvelope(t, "91919191-8282-4333-8444-717171717171", strings.Repeat("a", 40), clock), &clock)
	outcome, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if !errors.Is(err, ErrUnauthorized) || outcome.State != DeliveryFailed {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	outbox, outboxErr := store.PendingOutbox(context.Background(), 10)
	if outboxErr != nil || len(outbox) != 0 {
		t.Fatalf("outbox=%#v err=%v", outbox, outboxErr)
	}
}

func TestUnknownInstallationIsDurablyPendingAndKeepsTombstone(t *testing.T) {
	store := NewMemoryStore()
	clock := testNow
	provider := &fakeProvider{resolvedCommit: strings.Repeat("b", 40), now: clock}
	service := webhookService(store, provider, testEnvelope(t, "abababab-cdcd-4efe-8a8a-010101010101", strings.Repeat("a", 40), clock), &clock)
	outcome, err := service.Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if !errors.Is(err, ErrProviderRetry) || outcome.State != DeliveryClaimed {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	receipt, getErr := store.Delivery(context.Background(), outcome.ClaimKey)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if receipt.FailureCode != "github-installation-pending" || receipt.CompletedAt != nil {
		t.Fatalf("receipt=%#v", receipt)
	}
	clock = clock.Add(31 * time.Second)
	pending, pendingErr := store.PendingDeliveries(context.Background(), clock, 10)
	if pendingErr != nil || len(pending) != 1 || pending[0] != outcome.ClaimKey {
		t.Fatalf("pending=%v err=%v", pending, pendingErr)
	}
}

func TestClaimOnceHonorsPermanentAndEphemeralKinds(t *testing.T) {
	store := NewMemoryStore()
	claim := githubapp.OneTimeClaim{Kind: "github-state", ClaimKey: strings.Repeat("a", 64), RetainUntil: testNow.Add(time.Hour)}
	first, err := store.ClaimOnce(context.Background(), claim)
	if err != nil || !first {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := store.ClaimOnce(context.Background(), claim)
	if err != nil || second {
		t.Fatalf("second=%v err=%v", second, err)
	}
	claim.Permanent = true
	if _, err := store.ClaimOnce(context.Background(), claim); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid kind permanence accepted: %v", err)
	}
}

func TestSuccessfulCacheGenerationBecomesNextBuildImport(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	first := createAttempt(t, store, RegistryManaged, &clock)
	controller := &BuildController{Store: store, Provider: &fakeProvider{resolvedCommit: first.CommitSHA, now: clock}, Kubernetes: &fakeKubernetes{state: WorkloadSucceeded, promoted: true}, Owner: "build-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	if _, err := controller.ReconcileNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	provider := &fakeProvider{resolvedCommit: strings.Repeat("c", 40), now: clock}
	envelope := testEnvelope(t, "77777777-6666-4555-8444-333333333333", strings.Repeat("a", 40), clock)
	outcome, err := webhookService(store, provider, envelope, &clock).Handle(context.Background(), http.Header{}, strings.NewReader("ignored"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Attempt(context.Background(), outcome.AttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 || len(second.PlanRequest.Build.Cache.Imports) != 1 || second.PlanRequest.Build.Cache.Imports[0] != cacheReference(first) {
		t.Fatalf("second cache=%#v", second.PlanRequest.Build.Cache)
	}
}

func TestPermissionEventCannotUnsuspendInstallation(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	base := githubapp.InstallationEvent{InstallationID: testProviderInstall, Account: validInstallation(testNow).Account, RepositorySelection: "selected", Permissions: githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}}
	base.Action = "suspend"
	if err := store.ApplyInstallationEvent(context.Background(), testAppID, base, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	base.Action = "new_permissions_accepted"
	if err := store.ApplyInstallationEvent(context.Background(), testAppID, base, testNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthorizePush(context.Background(), testAppID, testProviderInstall, repositoryFixture(testNow).Identity, "refs/heads/main"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("permission event unsuspended installation: %v", err)
	}
}

func TestRepositoryEventCannotChangeAccountType(t *testing.T) {
	store, _ := seedMemory(t, RegistryManaged)
	event := githubapp.InstallationRepositoriesEvent{
		Action: "added", InstallationID: testProviderInstall,
		Account:             githubapp.AccountIdentity{ID: testAccountID, Login: "kuberploy", Type: "User"},
		RepositorySelection: "selected",
		Added:               []githubapp.RepositoryIdentity{{ID: 21, Name: "other", OwnerID: testAccountID, OwnerLogin: "kuberploy"}},
	}
	if err := store.ApplyRepositoryEvent(context.Background(), testAppID, event, testNow.Add(time.Minute)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("account type rebound: %v", err)
	}
}
