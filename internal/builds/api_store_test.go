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

func TestMemoryDefinitionReplacementKeepsImmutableHistory(t *testing.T) {
	ctx := context.Background()
	store, original := seedMemory(t, RegistryManaged)
	replacementID := "99999999-9999-4999-8999-999999999999"
	replacement := definitionWithIDs(t, testNow.Add(time.Minute), RegistryManaged, replacementID, original.ProjectID, original.ServiceID, original.InstallationID, original.RepositoryID, original.Spec.Registry.TargetID)
	if err := store.PutDefinition(ctx, replacement); err != nil {
		t.Fatalf("replace definition: %v", err)
	}
	gotOriginal, err := store.Definition(ctx, original.ID)
	if err != nil || gotOriginal.Enabled {
		t.Fatalf("original definition=%#v err=%v, want disabled history", gotOriginal, err)
	}
	gotReplacement, err := store.Definition(ctx, replacement.ID)
	if err != nil || !gotReplacement.Enabled {
		t.Fatalf("replacement definition=%#v err=%v, want active", gotReplacement, err)
	}
	authorized, err := store.AuthorizePush(ctx, testAppID, testProviderInstall, repositoryFixture(testNow).Identity, "refs/heads/main")
	if err != nil || len(authorized.Definitions) != 1 || authorized.Definitions[0].ID != replacement.ID {
		t.Fatalf("authorized definitions=%#v err=%v", authorized.Definitions, err)
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
	source.State, source.FailureCode, source.CompletedAt, source.UpdatedAt = AttemptFailed, "build-failed", &completed, completed
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
			if retryErr != nil || attempt.ID != retryID || attempt.DeliveryClaimKey != retryClaim {
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
		t.Fatalf("retry changed immutable source: %#v err=%v", retry, err)
	}
	if retry.PlanRequest.AgentImage != currentExecution.BuilderAgentImage || retry.PlanRequest.AgentImage == source.PlanRequest.AgentImage {
		t.Fatalf("retry agent image=%q, want refreshed operator runtime %q", retry.PlanRequest.AgentImage, currentExecution.BuilderAgentImage)
	}
	store.mu.Lock()
	_, claimOK := store.claims[claimMapKey("github-delivery", retryClaim)]
	receipt, receiptOK := store.deliveries[retryClaim]
	store.mu.Unlock()
	if !claimOK || !receiptOK || receipt.State != DeliveryEnqueued {
		t.Fatalf("retry durability claim=%v receipt=%#v", claimOK, receipt)
	}
	if _, _, err = store.RetryAttempt(ctx, source.ID, retryID, strings.Repeat("d", 64), currentExecution, completed.Add(2*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing retry rebound to a different durable claim: %v", err)
	}
}
