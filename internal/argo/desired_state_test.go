package argo_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	desiredStatePlatformBindingID = "12111111-1111-4111-8111-111111111111"
	desiredStateClusterID         = "13111111-1111-4111-8111-111111111111"
	desiredStateCommandID         = "14111111-1111-4111-8111-111111111111"
)

type staticDesiredStateProjectionGate struct {
	approval argo.DesiredStateProjectionApproval
	err      error
}

type staticDesiredStateRegistryEligibility struct {
	resolved bool
	err      error
	received *argo.DesiredStateProjectionApproval
}

func (r *staticDesiredStateRegistryEligibility) ResolveRegistryReferences(
	_ context.Context,
	_ argo.DesiredStateTarget,
	approval argo.DesiredStateProjectionApproval,
	_ time.Time,
) (bool, error) {
	if r.received != nil {
		*r.received = approval
	}
	return r.resolved, r.err
}

func (g staticDesiredStateProjectionGate) ApproveDesiredStateProjection(_ context.Context, _ argo.DesiredStateTarget) (argo.DesiredStateProjectionApproval, error) {
	return g.approval, g.err
}

func (g staticDesiredStateProjectionGate) ValidateDesiredStateClaim(_ context.Context, command argo.DesiredStateCommand, mode argo.DesiredStateClaimMode) error {
	if g.err != nil {
		return g.err
	}
	if mode != argo.DesiredStateClaimActive && mode != argo.DesiredStateClaimRecovery {
		return argo.ErrInvalid
	}
	approval := g.approval
	if approval.Contract != argo.DesiredStateProjectionApprovalContract || approval.BindingID != command.EnvironmentBindingID ||
		approval.IndexedRevision != command.EnvironmentRevision || approval.ProjectionGeneration != command.EnvironmentGeneration ||
		approval.PolicyDigest != command.PolicyDigest || approval.CatalogDigest != command.CatalogDigest ||
		!approval.AppConfigsValid || !approval.DependenciesValid ||
		!approval.SecretReferencesResolved || !approval.RegistryReferencesResolved {
		return argo.ErrInvalid
	}
	return nil
}

func desiredStateApproval(t *testing.T, target argo.DesiredStateTarget, applications []domain.Application, deployments []domain.Deployment) argo.DesiredStateProjectionApproval {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Revision     string
		Generation   int64
		Applications []domain.Application
		Deployments  []domain.Deployment
	}{target.Environment.Binding.IndexedRevision, target.Environment.Binding.ProjectionGeneration, applications, deployments})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return argo.DesiredStateProjectionApproval{
		Contract: argo.DesiredStateProjectionApprovalContract, BindingID: target.Environment.Binding.ID,
		IndexedRevision: target.Environment.Binding.IndexedRevision, ProjectionGeneration: target.Environment.Binding.ProjectionGeneration,
		PolicyDigest: "sha256:" + strings.Repeat("d", 64), CatalogDigest: "sha256:" + hex.EncodeToString(digest[:]),
		Applications: applications, Deployments: deployments,
		AppConfigsValid: true, DependenciesValid: true, SecretReferencesResolved: true, RegistryReferencesResolved: true,
	}
}

func planDesiredStateCommand(t *testing.T, id string, target argo.DesiredStateTarget, applications []domain.Application, deployments []domain.Deployment, previous *argo.DesiredStateCommand, now time.Time) (argo.DesiredStateCommand, error) {
	t.Helper()
	approval := desiredStateApproval(t, target, applications, deployments)
	return (argo.DesiredStatePlanner{
		Projection:          staticDesiredStateProjectionGate{approval: approval},
		RegistryEligibility: &staticDesiredStateRegistryEligibility{resolved: true},
	}).Plan(t.Context(), id, target, previous, now)
}

func desiredStateTargetFixture(t *testing.T, now time.Time) (argo.DesiredStateTarget, []domain.Application, []domain.Deployment) {
	t.Helper()
	environmentTarget, application := targetFixture(t)
	environmentTarget.Binding.CredentialMode = gitprojection.CredentialGitHubApp
	environmentTarget.Binding.CredentialSecretName = ""
	platform, err := gitprojection.NewGitHubPlatformBinding(desiredStatePlatformBindingID, desiredStateClusterID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 17, RepositoryID: 18, Owner: "kuberploy", Name: "platform"},
		"refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	platform.TargetHeadRevision = strings.Repeat("d", 40)
	platform.TargetHeadObservedAt, platform.UpdatedAt, platform.State = now, now, gitprojection.BindingIndexing
	deployment := deploymentFixture()
	deployment.DesiredRevision = environmentTarget.Binding.IndexedRevision
	return argo.DesiredStateTarget{Environment: environmentTarget, PlatformBinding: platform},
		[]domain.Application{application}, []domain.Deployment{deployment}
}

func desiredStateIdentity(t *testing.T, target argo.DesiredStateTarget) argo.DesiredStateRuntimeIdentity {
	t.Helper()
	repositorySecretName, err := argo.RepositoryCredentialName(target.PlatformBinding.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := argo.DesiredStateRuntimeIdentityForConfig(argo.DesiredStateRuntimeConfig{
		Enabled: true, GitHubAppID: target.PlatformBinding.Repository.InstallationID,
		PlatformBindingID: target.PlatformBinding.ID, ClusterID: target.PlatformBinding.ClusterID,
		ArgoNamespace: target.Environment.ArgoNamespace, RootApplicationName: argo.PlatformRootApplicationName,
		RepositorySecretName: repositorySecretName, Runtime: target.Environment.Runtime,
		DigestEnforcement: argo.ChartDigestNativeOCI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestDesiredStateCommandDerivesProtectedAuthorityAndDigestPin(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	target, applications, deployments := desiredStateTargetFixture(t, now)
	command, err := planDesiredStateCommand(t, desiredStateCommandID, target, applications, deployments, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "clusters/" + desiredStateClusterID + "/argocd/environments/" + environmentID + ".yaml"
	if command.Path != wantPath || command.BaseRevision != target.PlatformBinding.TargetHeadRevision ||
		command.Precondition != gitprojection.MutationCreateIfAbsent || command.ExpectedETag != "" ||
		command.DestinationNamespace != target.Environment.Environment.Namespace || command.ArgoProject != target.Environment.Environment.ArgoProject {
		t.Fatalf("server-derived command is wrong: %#v", command)
	}
	body := string(command.Content)
	if !strings.Contains(body, "repoURL: oci://ghcr.io/kuberploy/charts/kuberploy-runtime") ||
		!strings.Contains(body, "targetRevision: "+target.Environment.Runtime.ChartDigest) ||
		strings.Contains(body, "chart:") || strings.Contains(body, "targetRevision: "+target.Environment.Runtime.ChartVersion) {
		t.Fatalf("runtime source is not exact digest-pinned OCI:\n%s", body)
	}
	mutation := command.Mutation()
	if mutation.BindingID != target.PlatformBinding.ID || mutation.OperationID != command.ID || mutation.Path != wantPath || string(mutation.Content) != string(command.Content) {
		t.Fatalf("mutation authority changed: %#v", mutation)
	}

	for name, mutate := range map[string]func(*argo.DesiredStateCommand){
		"path": func(value *argo.DesiredStateCommand) {
			value.Path = "clusters/" + desiredStateClusterID + "/argocd/root.yaml"
		},
		"destination": func(value *argo.DesiredStateCommand) { value.DestinationNamespace = "attacker" },
		"project":     func(value *argo.DesiredStateCommand) { value.ArgoProject = "default" },
		"planned base": func(value *argo.DesiredStateCommand) {
			value.BaseRevision = strings.Repeat("f", 40)
		},
		"chart tag": func(value *argo.DesiredStateCommand) { value.Runtime.ChartDigest = "latest" },
		"content": func(value *argo.DesiredStateCommand) {
			value.Content = append(value.Content, []byte("# tampered\n")...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := command
			candidate.Content = append([]byte(nil), command.Content...)
			mutate(&candidate)
			if err := candidate.ValidateFor(target); !errors.Is(err, argo.ErrInvalid) {
				t.Fatalf("tampered command accepted: %v", err)
			}
		})
	}

	completed := command
	writeBaseObservedAt, committedAt, verifiedAt := now.Add(500*time.Millisecond), now.Add(time.Second), now.Add(2*time.Second)
	completed.WriteBaseRevision, completed.WriteBaseObservedAt = completed.BaseRevision, &writeBaseObservedAt
	completed.State, completed.CommittedRevision = argo.DesiredStateVerified, strings.Repeat("e", 40)
	completed.CommittedAt, completed.VerifiedAt, completed.CompletedAt = &committedAt, &verifiedAt, &verifiedAt
	completed.UpdatedAt = verifiedAt
	if completed.ValidateFor(target) != nil {
		t.Fatal("verified baseline fixture is invalid")
	}
	branchAdvanced := target
	branchAdvanced.Environment.Binding.TargetHeadRevision = strings.Repeat("9", 40)
	branchAdvanced.Environment.Binding.TargetHeadObservedAt = now.Add(3 * time.Second)
	branchAdvanced.Environment.Binding.UpdatedAt = now.Add(3 * time.Second)
	branchAdvanced.Environment.Binding.State = gitprojection.BindingIndexing
	if _, err = planDesiredStateCommand(t, "19111111-1111-4111-8111-111111111111", branchAdvanced, applications, deployments, &completed, now.Add(3*time.Second)); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("unindexed branch advance entered Argo desired state: %v", err)
	}
	if !strings.Contains(string(command.Content), "valuesRevision: "+deployments[0].DesiredRevision) || strings.Contains(string(command.Content), "valuesRevision: "+branchAdvanced.Environment.Binding.TargetHeadRevision) {
		t.Fatal("existing protected command followed mutable environment branch")
	}
	indexedAdvance := branchAdvanced
	indexedAdvance.Environment.Binding.IndexedRevision = indexedAdvance.Environment.Binding.TargetHeadRevision
	indexedAdvance.Environment.Binding.IndexedAt = now.Add(4 * time.Second)
	indexedAdvance.Environment.Binding.ProjectionGeneration++
	indexedAdvance.Environment.Binding.UpdatedAt = now.Add(4 * time.Second)
	indexedAdvance.Environment.Binding.State = gitprojection.BindingReady
	unchanged, err := planDesiredStateCommand(t, "19111111-1111-4111-8111-111111111111", indexedAdvance,
		applications, deployments, &completed, now.Add(5*time.Second))
	if !errors.Is(err, argo.ErrNoDesiredStateChange) {
		t.Fatalf("platform-only indexed advance changed tenant desired state: %v", err)
	}
	if unchanged.EnvironmentRevision != indexedAdvance.Environment.Binding.IndexedRevision ||
		unchanged.EnvironmentGeneration != indexedAdvance.Environment.Binding.ProjectionGeneration ||
		unchanged.ContentSHA256 != completed.ContentSHA256 || unchanged.CatalogDigest == completed.CatalogDigest {
		t.Fatalf("no-change receipt candidate lost current authority: %#v", unchanged)
	}
	if _, err = planDesiredStateCommand(t, "15111111-1111-4111-8111-111111111111", target, applications, deployments, &completed, now.Add(3*time.Second)); !errors.Is(err, argo.ErrNoDesiredStateChange) {
		t.Fatalf("unchanged catalog queued: %v", err)
	}
	secondApplication := domain.Application{ID: "16111111-1111-4111-8111-111111111111", ProjectID: projectID}
	secondDeployment := domain.Deployment{ID: "17111111-1111-4111-8111-111111111111", ApplicationID: secondApplication.ID, EnvironmentID: environmentID}
	next, err := planDesiredStateCommand(t, "15111111-1111-4111-8111-111111111111", target,
		append(applications, secondApplication), append(deployments, secondDeployment), &completed, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != 2 || next.Precondition != gitprojection.MutationMatchETag || next.ExpectedETag != `"`+completed.ContentSHA256+`"` {
		t.Fatalf("update baseline is not exact: %#v", next)
	}

	legacy := target
	legacy.PlatformBinding.CredentialMode = gitprojection.CredentialLegacySecret
	legacy.PlatformBinding.CredentialSecretName = "platform-git-secret"
	if _, err = planDesiredStateCommand(t, "18111111-1111-4111-8111-111111111111", legacy, applications, deployments, nil, now); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("legacy protected binding accepted: %v", err)
	}

	baseApproval := desiredStateApproval(t, target, applications, deployments)
	invalidApprovals := map[string]func(*argo.DesiredStateProjectionApproval){
		"invalid appconfig":     func(value *argo.DesiredStateProjectionApproval) { value.AppConfigsValid = false },
		"invalid dependency":    func(value *argo.DesiredStateProjectionApproval) { value.DependenciesValid = false },
		"unresolved secret":     func(value *argo.DesiredStateProjectionApproval) { value.SecretReferencesResolved = false },
		"stale revision":        func(value *argo.DesiredStateProjectionApproval) { value.IndexedRevision = strings.Repeat("8", 40) },
		"stale generation":      func(value *argo.DesiredStateProjectionApproval) { value.ProjectionGeneration++ },
		"forged policy digest":  func(value *argo.DesiredStateProjectionApproval) { value.PolicyDigest = "not-a-digest" },
		"forged catalog digest": func(value *argo.DesiredStateProjectionApproval) { value.CatalogDigest = "not-a-digest" },
	}
	for name, mutate := range invalidApprovals {
		t.Run(name, func(t *testing.T) {
			approval := baseApproval
			mutate(&approval)
			planner := argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: approval},
				RegistryEligibility: &staticDesiredStateRegistryEligibility{resolved: true}}
			if _, planErr := planner.Plan(t.Context(), "1a111111-1111-4111-8111-111111111111", target, nil, now); !errors.Is(planErr, argo.ErrInvalid) {
				t.Fatalf("unapproved projection entered desired state: %v", planErr)
			}
		})
	}
}

func TestDesiredStatePlannerDerivesRegistryResolutionAndRejectsCallerBypass(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	target, applications, deployments := desiredStateTargetFixture(t, now)
	approval := desiredStateApproval(t, target, applications, deployments)

	var received argo.DesiredStateProjectionApproval
	resolver := &staticDesiredStateRegistryEligibility{resolved: false, received: &received}
	planner := argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: approval}, RegistryEligibility: resolver}
	if _, err := planner.Plan(t.Context(), "1a111111-1111-4111-8111-111111111111", target, nil, now); !errors.Is(err, argo.ErrRegistryReferencesNotReady) {
		t.Fatalf("caller true bypassed exact registry eligibility: %v", err)
	}
	if received.RegistryReferencesResolved {
		t.Fatal("resolver received caller-controlled registry approval")
	}

	approval.RegistryReferencesResolved = false
	resolver.resolved = true
	planner = argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: approval}, RegistryEligibility: resolver}
	if _, err := planner.Plan(t.Context(), "1a111111-1111-4111-8111-111111111111", target, nil, now); err != nil {
		t.Fatalf("derived public/ready eligibility was not accepted: %v", err)
	}

	if _, err := (argo.DesiredStatePlanner{Projection: staticDesiredStateProjectionGate{approval: approval}}).
		Plan(t.Context(), "1a111111-1111-4111-8111-111111111111", target, nil, now); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("nil eligibility resolver accepted: %v", err)
	}
	sentinel := errors.New("eligibility infrastructure failed")
	resolver.err = sentinel
	if _, err := planner.Plan(t.Context(), "1a111111-1111-4111-8111-111111111111", target, nil, now); !errors.Is(err, sentinel) {
		t.Fatalf("eligibility error was hidden: %v", err)
	}
}

func TestMemoryDesiredStateEpochFencingAndSaturatingRecovery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	target, applications, deployments := desiredStateTargetFixture(t, now)
	command, err := planDesiredStateCommand(t, desiredStateCommandID, target, applications, deployments, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	store := argo.NewMemoryDesiredStateStore()
	prepopulatedReceipt := command
	writeBaseObservedAt := now
	prepopulatedReceipt.WriteBaseRevision, prepopulatedReceipt.WriteBaseObservedAt = command.BaseRevision, &writeBaseObservedAt
	if _, createErr := store.CreateDesiredState(t.Context(), prepopulatedReceipt); !errors.Is(createErr, argo.ErrInvalid) {
		t.Fatalf("caller-prepopulated write-base receipt was accepted: %v", createErr)
	}
	if created, createErr := store.CreateDesiredState(t.Context(), command); createErr != nil || !created {
		t.Fatalf("created=%v err=%v", created, createErr)
	}
	if created, replayErr := store.CreateDesiredState(t.Context(), command); replayErr != nil || created {
		t.Fatalf("exact replay created=%v err=%v", created, replayErr)
	}
	tampered := command
	tampered.Message = "different immutable command"
	if _, replayErr := store.CreateDesiredState(t.Context(), tampered); !errors.Is(replayErr, argo.ErrConflict) {
		t.Fatalf("tampered replay accepted: %v", replayErr)
	}
	identity := desiredStateIdentity(t, target)
	first, err := store.ClaimDesiredState(t.Context(), "argo-worker-owner-a", identity.DesiredStateWorkerIdentity, now, 30*time.Second)
	if err != nil || first.Lease.Epoch != 1 {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	second, err := store.ClaimDesiredState(t.Context(), "argo-worker-owner-b", identity.DesiredStateWorkerIdentity, now.Add(31*time.Second), 30*time.Second)
	if err != nil || second.Lease.Epoch != 2 {
		t.Fatalf("reclaim=%#v err=%v", second, err)
	}
	writeBaseObservedAt = now.Add(31 * time.Second)
	if _, err = store.BindDesiredStateWriteBase(t.Context(), first.Lease, command.BaseRevision, writeBaseObservedAt, writeBaseObservedAt); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale worker bound write base: %v", err)
	}
	if _, err = store.BindDesiredStateWriteBase(t.Context(), second.Lease, command.BaseRevision, writeBaseObservedAt, writeBaseObservedAt); err != nil {
		t.Fatalf("bind write base: %v", err)
	}
	if _, err = store.BindDesiredStateWriteBase(t.Context(), second.Lease, strings.Repeat("f", 40), writeBaseObservedAt, writeBaseObservedAt); !errors.Is(err, argo.ErrConflict) {
		t.Fatalf("write-base receipt was mutable: %v", err)
	}
	if _, err = store.MarkDesiredStateGitCommitted(t.Context(), first.Lease, strings.Repeat("e", 40), now.Add(32*time.Second)); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale worker wrote: %v", err)
	}
	committed, err := store.MarkDesiredStateGitCommitted(t.Context(), second.Lease, strings.Repeat("e", 40), now.Add(32*time.Second))
	if err != nil || committed.State != argo.DesiredStateGitCommitted {
		t.Fatalf("committed=%#v err=%v", committed, err)
	}

	lease := second.Lease
	clock := now.Add(32 * time.Second)
	for index := 0; index < 32; index++ {
		clock = clock.Add(time.Second)
		if _, err = store.RetryDesiredState(t.Context(), lease, argo.DesiredStateRetry{FailureCode: "provider-timeout", NextAttemptAt: clock}, clock); err != nil {
			t.Fatalf("retry %d: %v", index, err)
		}
		work, claimErr := store.ClaimDesiredState(t.Context(), "argo-worker-owner-b", identity.DesiredStateWorkerIdentity, clock, 30*time.Second)
		if claimErr != nil {
			t.Fatalf("reclaim %d: %v", index, claimErr)
		}
		lease = work.Lease
	}
	current, err := store.DesiredStateCommand(t.Context(), command.ID)
	if err != nil || current.ConsecutiveFailures != 30 || current.State != argo.DesiredStateGitCommitted || lease.Epoch != 34 {
		t.Fatalf("saturated command=%#v lease=%#v err=%v", current, lease, err)
	}
	clock = clock.Add(time.Second)
	verified, err := store.CompleteDesiredStateVerified(t.Context(), lease, strings.Repeat("e", 40), clock)
	if err != nil || verified.State != argo.DesiredStateVerified || verified.ConsecutiveFailures != 30 {
		t.Fatalf("saturated committed command did not recover: %#v err=%v", verified, err)
	}
	status, err := store.LatestDesiredState(t.Context(), projectID, environmentID)
	if err != nil || status.State != argo.DesiredStateVerified || status.CommittedRevision == "" {
		t.Fatalf("safe status=%#v err=%v", status, err)
	}
}

func TestDesiredStateReadinessIsExactFreshAndEpochFenced(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	target, _, _ := desiredStateTargetFixture(t, now)
	identity := desiredStateIdentity(t, target)
	store := argo.NewMemoryDesiredStateStore()
	observation := argo.DesiredStateRuntimeWorkerObservation{WorkerID: "argo-readiness-worker-a", DesiredStateRuntimeIdentity: identity, StartedAt: now, ObservedAt: now}
	first, err := store.AcquireDesiredStateReadiness(t.Context(), observation, argo.DesiredStateReadinessLease)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first readiness=%#v err=%v", first, err)
	}
	if err = store.DesiredStateRuntimeReady(t.Context(), identity, now.Add(20*time.Second), argo.DesiredStateHeartbeatMaxAge); err != nil {
		t.Fatalf("fresh exact readiness rejected: %v", err)
	}
	mismatch := identity
	mismatch.RepositorySecretName = "different-repository-secret"
	if err = store.DesiredStateRuntimeReady(t.Context(), mismatch, now.Add(20*time.Second), argo.DesiredStateHeartbeatMaxAge); !errors.Is(err, argo.ErrDesiredStateNotReady) {
		t.Fatalf("mismatched readiness accepted: %v", err)
	}
	second, err := store.AcquireDesiredStateReadiness(t.Context(), observation, argo.DesiredStateReadinessLease)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("second readiness=%#v err=%v", second, err)
	}
	if _, err = store.HeartbeatDesiredStateReadiness(t.Context(), first, now.Add(time.Second), argo.DesiredStateReadinessLease); !errors.Is(err, argo.ErrLeaseLost) {
		t.Fatalf("stale readiness heartbeat accepted: %v", err)
	}
	if err = store.DesiredStateRuntimeReady(t.Context(), identity, now.Add(time.Minute), argo.DesiredStateHeartbeatMaxAge); !errors.Is(err, argo.ErrDesiredStateNotReady) {
		t.Fatalf("expired readiness accepted: %v", err)
	}
}
