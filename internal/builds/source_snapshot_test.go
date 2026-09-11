package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitssh"
)

type controlledAttemptAuthorizationStore struct {
	*MemoryStore
	installation Installation
	repository   Repository
	err          error
}

func (s *controlledAttemptAuthorizationStore) AttemptAuthorization(context.Context, string) (Installation, Repository, error) {
	return s.installation, s.repository, s.err
}

func TestGitHubDefinitionDigestIncludesSourceIdentityAndRef(t *testing.T) {
	original := validDefinition(t, testNow, RegistryManaged)
	tests := map[string]func(*BuildDefinition){
		"installation": func(definition *BuildDefinition) {
			definition.InstallationID = "77777777-7777-4777-8777-777777777777"
		},
		"repository": func(definition *BuildDefinition) {
			definition.RepositoryID = "88888888-8888-4888-8888-888888888888"
		},
		"ref": func(definition *BuildDefinition) {
			definition.TriggerRef = "refs/heads/release"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := cloneDefinition(original)
			mutate(&changed)
			prepared, err := PrepareDefinition(changed, testNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if prepared.DefinitionDigest == original.DefinitionDigest {
				t.Fatalf("GitHub %s change retained definition digest %q", name, original.DefinitionDigest)
			}
		})
	}
}

func TestControllerExecutesQueuedGitHubSourceSnapshotAfterAppSourceEdit(t *testing.T) {
	store, original := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)

	replacement := cloneDefinition(original)
	replacement.TriggerRef = "refs/heads/release"
	replacement, err := PrepareDefinition(replacement, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDefinition(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	current, err := store.Definition(t.Context(), original.ID)
	if err != nil || current.DefinitionDigest == attempt.DefinitionDigest {
		t.Fatalf("App source was not edited: current=%#v err=%v", current, err)
	}

	authorizedStore := &controlledAttemptAuthorizationStore{
		MemoryStore:  store,
		installation: validInstallation(clock),
		repository:   repositoryFixture(clock),
	}
	provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}
	kubernetes := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
	controller := &BuildController{Store: authorizedStore, Provider: provider, Kubernetes: kubernetes, Owner: "snapshot-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}

	result, err := controller.ReconcileNext(t.Context())
	if err != nil || result.State != AttemptSucceeded || len(kubernetes.workloads) != 1 {
		t.Fatalf("result=%#v workloads=%d err=%v", result, len(kubernetes.workloads), err)
	}
	executed := kubernetes.workloads[0].Attempt.SourceSnapshot
	if executed.DefinitionDigest != attempt.DefinitionDigest || executed.TriggerRef != original.TriggerRef || executed.TriggerRef == current.TriggerRef {
		t.Fatalf("executed source=%#v current ref=%q", executed, current.TriggerRef)
	}
	if provider.verifyCalls != 1 || provider.mintCalls != 1 {
		t.Fatalf("live GitHub authorization calls: verify=%d mint=%d", provider.verifyCalls, provider.mintCalls)
	}
}

func TestControllerRejectsQueuedGitHubSnapshotWhenLiveAuthorizationIsRevoked(t *testing.T) {
	store, original := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	replacement := cloneDefinition(original)
	replacement.TriggerRef = "refs/heads/release"
	replacement, err := PrepareDefinition(replacement, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDefinition(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}

	authorizedStore := &controlledAttemptAuthorizationStore{MemoryStore: store, err: ErrUnauthorized}
	provider := &fakeProvider{resolvedCommit: attempt.CommitSHA, now: clock}
	kubernetes := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
	controller := &BuildController{Store: authorizedStore, Provider: provider, Kubernetes: kubernetes, Owner: "snapshot-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}

	if _, err = controller.ReconcileNext(t.Context()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked GitHub authorization accepted: %v", err)
	}
	stored, getErr := store.Attempt(t.Context(), attempt.ID)
	if getErr != nil || stored.State != AttemptFailed || stored.FailureCode != "github-authorization-revoked" || len(kubernetes.workloads) != 0 {
		t.Fatalf("stored=%#v workloads=%d err=%v", stored, len(kubernetes.workloads), getErr)
	}
	if provider.verifyCalls != 0 || provider.mintCalls != 0 {
		t.Fatalf("provider called after durable authorization revocation: verify=%d mint=%d", provider.verifyCalls, provider.mintCalls)
	}
}

func TestControllerChecksSnapshotGitSSHKeyRevocationAfterAppSourceEdit(t *testing.T) {
	definition := validGitSSHDefinition(t)
	store := NewMemoryStore()
	if err := store.PutDefinition(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	attempt, err := newAttempt(definition, Repository{}, EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("b", 64), CommitSHA: strings.Repeat("a", 40), GitRef: definition.TriggerRef, ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.attempts[attempt.ID] = attempt
	store.mu.Unlock()

	replacement := cloneDefinition(definition)
	replacement.GitSSH.RepositoryURL = "ssh://git@git.example.test/team/replacement.git"
	replacement.GitSSH.KeyScope = "project"
	replacement.GitSSH.KeyOwnerID = definition.ProjectID
	replacement.GitSSH.KeyRevision = 2
	replacement, err = PrepareDefinition(replacement, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutDefinition(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeGitSSHCredentials{metadata: gitssh.KeyMetadata{
		Scope: gitssh.ScopeApp, OwnerID: definition.ServiceID, Revision: definition.GitSSH.KeyRevision, Status: gitssh.StatusRevoked,
	}}
	kubernetes := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
	controller := &BuildController{Store: store, GitSSH: credentials, Kubernetes: kubernetes, Owner: "snapshot-controller", LeaseDuration: time.Minute, Now: func() time.Time { return testNow }}

	if _, err = controller.ReconcileNext(t.Context()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked Git SSH key accepted: %v", err)
	}
	stored, getErr := store.Attempt(t.Context(), attempt.ID)
	if getErr != nil || stored.State != AttemptFailed || stored.FailureCode != "git-ssh-key-revoked" || len(kubernetes.workloads) != 0 {
		t.Fatalf("stored=%#v workloads=%d err=%v", stored, len(kubernetes.workloads), getErr)
	}
	if credentials.activeScope != gitssh.ScopeApp || credentials.activeOwner != definition.ServiceID {
		t.Fatalf("checked current App key instead of snapshot key: scope=%q owner=%q", credentials.activeScope, credentials.activeOwner)
	}
}
