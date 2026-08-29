package builds

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemorySourceReplacementKeepsOnlyCurrentAppSource(t *testing.T) {
	ctx := context.Background()
	store, original := seedMemory(t, RegistryManaged)
	replacementID := "99999999-9999-4999-8999-999999999999"
	replacement := definitionWithIDs(t, testNow.Add(time.Minute), RegistryManaged, replacementID, original.ProjectID, original.ServiceID, original.InstallationID, original.RepositoryID, original.Spec.Registry.TargetID)
	if err := store.PutDefinition(ctx, replacement); err != nil {
		t.Fatalf("replace definition: %v", err)
	}
	gotOriginal, err := store.Definition(ctx, original.ID)
	if err != nil {
		t.Fatalf("current source disappeared: %v", err)
	}
	if _, err = store.Definition(ctx, replacement.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edit minted another source identity: %v", err)
	}
	if !gotOriginal.Enabled || gotOriginal.DefinitionGeneration != original.DefinitionGeneration+1 || !gotOriginal.CreatedAt.Equal(original.CreatedAt) || gotOriginal.Spec.Registry.TargetID != replacement.Spec.Registry.TargetID {
		t.Fatalf("edited definition=%#v, want current source identity", gotOriginal)
	}
	authorized, err := store.AuthorizePush(ctx, testAppID, testProviderInstall, repositoryFixture(testNow).Identity, "refs/heads/main")
	if err != nil || len(authorized.Definitions) != 1 || authorized.Definitions[0].ID != original.ID {
		t.Fatalf("authorized definitions=%#v err=%v", authorized.Definitions, err)
	}
}

func TestAttemptKeepsAppSourceSnapshotWhileRefreshingRuntime(t *testing.T) {
	_, source := seedMemory(t, RegistryManaged)
	refreshed := source.Spec.Execution
	refreshed.BuilderAgentImage = "registry.test/system/builder-agent@sha256:" + strings.Repeat("9", 64)
	attempt, err := newAttemptWithExecution(source, refreshed, repositoryFixture(testNow), EnqueuePush{
		ClaimKey: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), GitRef: source.TriggerRef, ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.SourceSnapshot.DefinitionDigest != source.DefinitionDigest ||
		attempt.SourceSnapshot.Spec.Execution.BuilderAgentImage != source.Spec.Execution.BuilderAgentImage ||
		attempt.PlanRequest.AgentImage != refreshed.BuilderAgentImage {
		t.Fatalf("source snapshot or refreshed runtime drifted: snapshot=%q plan=%q", attempt.SourceSnapshot.Spec.Execution.BuilderAgentImage, attempt.PlanRequest.AgentImage)
	}
	if err = validateStoredAttempt(attempt); err != nil {
		t.Fatalf("stored attempt rejected: %v", err)
	}
}

func TestMemoryAPICommandAndRetryAreConcurrentAndFailClosed(t *testing.T) {
	ctx := context.Background()
	store, definition := seedMemory(t, RegistryManaged)
	now := testNow.Add(time.Hour)
	actorID := "77777777-7777-4777-8777-777777777777"
	resourceID := "88888888-8888-4888-8888-888888888888"
	fingerprint := "sha256:" + strings.Repeat("1", 64)

	var inserted, replayed atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got, replay, err := store.ClaimAPICommand(ctx, actorID, APICommandDefinitionCreate, testServiceID,
				"definition-concurrency-01", fingerprint, resourceID, now)
			if err != nil || got != resourceID {
				t.Errorf("claim got=%q replay=%v err=%v", got, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				inserted.Add(1)
			}
		}()
	}
	wait.Wait()
	if inserted.Load() != 1 || replayed.Load() != 31 {
		t.Fatalf("inserted=%d replayed=%d", inserted.Load(), replayed.Load())
	}
	if _, _, err := store.ClaimAPICommand(ctx, actorID, APICommandDefinitionCreate, testServiceID,
		"definition-concurrency-01", "sha256:"+strings.Repeat("2", 64), resourceID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency fingerprint substitution accepted: %v", err)
	}

	sourceClaim := strings.Repeat("a", 64)
	source, err := newAttempt(definition, repositoryFixture(now), EnqueuePush{ClaimKey: sourceClaim,
		CommitSHA: strings.Repeat("b", 40), GitRef: "refs/heads/main", ResolvedAt: now}, 1, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	completed := now.Add(time.Minute)
	source.State, source.CompletedAt, source.UpdatedAt = AttemptSucceeded, &completed, completed
	store.mu.Lock()
	store.attempts[source.ID] = cloneAttempt(source)
	store.serviceGeneration[serviceKey(definition.ProjectID, definition.ServiceID)] = source.Generation
	store.mu.Unlock()

	retryClaim := strings.Repeat("c", 64)
	retryID := RetryAttemptID(retryClaim, definition.ID)
	currentExecution := definition.Spec.Execution
	currentExecution.BuilderAgentImage = "registry.test/system/builder-agent@sha256:" + strings.Repeat("9", 64)
	inserted.Store(0)
	replayed.Store(0)
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			attempt, replay, retryErr := store.RetryAttempt(ctx, source.ID, retryID, retryClaim, currentExecution, completed.Add(time.Minute))
			if retryErr != nil || attempt.ID != retryID || attempt.DeliveryClaimKey != "" || attempt.TriggerKind != "retry" {
				t.Errorf("retry attempt=%#v replay=%v err=%v", attempt, replay, retryErr)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				inserted.Add(1)
			}
		}()
	}
	wait.Wait()
	if inserted.Load() != 1 || replayed.Load() != 31 {
		t.Fatalf("retry inserted=%d replayed=%d", inserted.Load(), replayed.Load())
	}
	retry, err := store.Attempt(ctx, retryID)
	if err != nil || retry.State != AttemptQueued || retry.Generation != 2 || retry.CommitSHA != source.CommitSHA || retry.GitRef != source.GitRef || retry.DefinitionDigest != source.DefinitionDigest {
		t.Fatalf("retry changed recorded source: %#v err=%v", retry, err)
	}
	if retry.PlanRequest.AgentImage != currentExecution.BuilderAgentImage || retry.PlanRequest.AgentImage == source.PlanRequest.AgentImage {
		t.Fatalf("retry agent image=%q, want refreshed operator runtime %q", retry.PlanRequest.AgentImage, currentExecution.BuilderAgentImage)
	}
	store.mu.Lock()
	_, claimOK := store.claims[claimMapKey("github-delivery", retryClaim)]
	_, receiptOK := store.deliveries[retryClaim]
	store.mu.Unlock()
	if claimOK || receiptOK {
		t.Fatalf("manual rebuild created synthetic GitHub delivery: claim=%v receipt=%v", claimOK, receiptOK)
	}
	if _, _, err = store.RetryAttempt(ctx, source.ID, retryID, strings.Repeat("d", 64), currentExecution, completed.Add(2*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing retry rebound to a different durable claim: %v", err)
	}
}

func TestMemoryDeleteDefinitionBlocksActiveWorkAndCleansTerminalHistory(t *testing.T) {
	ctx := context.Background()
	store, definition := seedMemory(t, RegistryManaged)
	actorID := "77777777-7777-4777-8777-777777777777"
	fingerprint := "sha256:" + strings.Repeat("7", 64)
	now := testNow.Add(time.Hour)
	attempt, err := newAttempt(definition, repositoryFixture(now), EnqueuePush{ClaimKey: strings.Repeat("8", 64), CommitSHA: strings.Repeat("9", 40), GitRef: "refs/heads/main", ResolvedAt: now}, 1, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.attempts[attempt.ID] = cloneAttempt(attempt)
	store.mu.Unlock()
	if _, err = store.DeleteDefinition(ctx, actorID, definition.ServiceID, definition.ID, "disconnect-active", fingerprint, "request-active", now); !errors.Is(err, ErrDeletionBlocked) {
		t.Fatalf("active attempt did not block disconnect: %v", err)
	}
	completed := now.Add(time.Minute)
	store.mu.Lock()
	attempt.State, attempt.FailureCode, attempt.CompletedAt, attempt.UpdatedAt = AttemptFailed, "build-failed", &completed, completed
	store.attempts[attempt.ID] = cloneAttempt(attempt)
	store.mu.Unlock()
	replay, err := store.DeleteDefinition(ctx, actorID, definition.ServiceID, definition.ID, "disconnect-terminal", fingerprint, "request-delete", completed)
	if err != nil || replay {
		t.Fatalf("disconnect replay=%v err=%v", replay, err)
	}
	if _, err = store.Definition(ctx, definition.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("definition survived disconnect: %v", err)
	}
	if _, err = store.HistoricalAttempt(ctx, attempt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attempt survived disconnect: %v", err)
	}
	replay, err = store.DeleteDefinition(ctx, actorID, definition.ServiceID, definition.ID, "disconnect-terminal", fingerprint, "request-replay", completed.Add(time.Second))
	if err != nil || !replay {
		t.Fatalf("disconnect replay=%v err=%v", replay, err)
	}
	if _, err = store.DeleteDefinition(ctx, actorID, definition.ServiceID, definition.ID, "disconnect-terminal", "sha256:"+strings.Repeat("6", 64), "request-conflict", completed.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed disconnect replay accepted: %v", err)
	}
}
