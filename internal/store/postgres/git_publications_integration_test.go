package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/migrations"
)

func TestPostgreSQLProtectedEnvironmentAtomicallyCreatesFencedPullRequestPublication(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	st, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	actorID, installationID, repositoryID := id.New(), id.New(), id.New()
	now := databaseTime(time.Now())
	suffix := actorID[:8]
	cleanup := func(cleanupContext context.Context) {
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM audit_events WHERE actor_id=$1`, actorID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM mutation_receipts WHERE actor_id=$1`, actorID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_pull_request_publications WHERE binding_id IN (SELECT id FROM git_repository_bindings WHERE repository_id=$1)`, int64(189101))
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_path_reservations WHERE binding_id IN (SELECT id FROM git_repository_bindings WHERE repository_id=$1)`, int64(189101))
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_write_commands WHERE command_kind='deployment' AND binding_id IN (SELECT id FROM git_repository_bindings WHERE repository_id=$1)`, int64(189101))
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM outbox WHERE operation_id IN (SELECT id FROM operations WHERE request_id LIKE 'publication-deployment-%')`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM deployments WHERE environment_id IN (SELECT id FROM environments WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'publication-%'))`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM operations WHERE request_id LIKE 'publication-deployment-%'`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id IN (SELECT id FROM git_repository_bindings WHERE repository_id=$1)`, int64(189101))
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE repository_id=$1`, int64(189101))
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM environments WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'publication-%')`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM applications WHERE project_id IN (SELECT id FROM projects WHERE slug LIKE 'publication-%')`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM projects WHERE slug LIKE 'publication-%'`)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM github_repositories WHERE id=$1`, repositoryID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM github_installations WHERE id=$1`, installationID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM access_grants WHERE subject_user_id=$1 OR created_by=$1`, actorID)
		_, _ = st.pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, actorID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	if _, err = st.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','git-publication-test',$2,1,$3)`, actorID, "publication-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
		VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO github_installations(
		id,github_installation_id,account_login,account_type,owner_user_id,visibility,team_id,
		repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,
		last_verified_at,created_at,updated_at)
		VALUES($1,184342,'kuberploy','Organization',$2,'private',NULL,'selected',1,277,2888,
		'active','{"metadata":"read","contents":"write","pull_requests":"write"}'::jsonb,$3,$3,$3)`, installationID, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO github_repositories(
		id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,189101,2888,'kuberploy','publication-gitops','active',$3,$3,$3)`, repositoryID, installationID, now); err != nil {
		t.Fatal(err)
	}

	project, err := st.CreateProject(ctx, actorID, "publication-project-"+suffix, "publication-project-"+suffix,
		domain.CreateProject{Name: "Publication", Slug: "publication-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, actorID, "publication-environment-"+suffix, "publication-environment-"+suffix,
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Production", Slug: "production"})
	if err != nil || environment.Value.ProtectionPolicy != domain.EnvironmentProtected {
		t.Fatalf("environment=%#v err=%v", environment, err)
	}
	application, err := st.CreateApplication(ctx, actorID, "publication-application-"+suffix, "publication-application-"+suffix,
		domain.CreateApplication{ProjectID: project.Value.ID, Name: "API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}
	bindingResult, err := st.CreateEnvironmentGitBinding(ctx, actorID, "publication-binding-"+suffix, "publication-binding-"+suffix,
		"publication-binding-request-"+suffix, gitprojection.CreateEnvironmentBindingInput{
			EnvironmentID: environment.Value.ID, LinkedInstallationID: installationID, LinkedRepositoryID: repositoryID, GitHubAppID: 277,
			Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 184342, RepositoryID: 189101,
				Owner: "kuberploy", Name: "publication-gitops"}, TargetRef: "refs/heads/main",
		})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	readyAt := now.Add(time.Second)
	if _, err = st.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,1,$2,$3,'active',$4,$4)`, bindingResult.Value.ID, head, bindingResult.Value.ParserVersion, readyAt); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',target_head_revision=$2,indexed_revision=$2,
		projection_generation=1,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, bindingResult.Value.ID, head, readyAt); err != nil {
		t.Fatal(err)
	}
	plan := &gitprojection.WritePlan{BindingID: bindingResult.Value.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: head, Precondition: gitprojection.MutationCreateIfAbsent,
		ChartDigest: "sha256:" + strings.Repeat("b", 64), PolicyVersion: "publication-policy-v1"}
	_, operation, err := st.CreateDeployment(ctx, actorID, "publication-deployment-"+suffix, "publication-deployment-"+suffix,
		"publication-deployment-request-"+suffix, domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
			Image: "registry.example/api@sha256:" + strings.Repeat("c", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, plan)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := st.AcceptedGitPublicationMode(ctx, operation.ID)
	publication, publicationErr := st.Publication(ctx, operation.ID)
	if err != nil || publicationErr != nil || mode != gitpublication.ModePullRequest || publication.State != gitpublication.StatePendingCandidate ||
		publication.Repository.InstallationID != 184342 || publication.Repository.ID != 189101 || publication.BaseRevision != head {
		t.Fatalf("mode=%q publication=%#v modeErr=%v publicationErr=%v", mode, publication, err, publicationErr)
	}
	writeBase, err := publication.WithWriteBase(head, publication.UpdatedAt.Add(time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, publication, writeBase) != nil {
		t.Fatalf("write base next=%#v err=%v", writeBase, err)
	}
	next, err := writeBase.WithCandidate(strings.Repeat("d", 40), writeBase.UpdatedAt.Add(time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, writeBase, next) != nil {
		t.Fatalf("candidate next=%#v err=%v", next, err)
	}
	if err = st.CompareAndSwapPublication(ctx, writeBase, next); !errors.Is(err, gitpublication.ErrConflict) {
		t.Fatalf("stale publication CAS error=%v", err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='merge-verified',candidate_revision=$2,
		pull_request_number=7,pull_request_url='https://github.com/kuberploy/publication-gitops/pull/7',pull_request_state='closed',
		merge_revision=$3,target_revision=$4,provider_observed_at=$5,updated_at=$5,version=version+1 WHERE operation_id=$1`,
		operation.ID, strings.Repeat("d", 40), strings.Repeat("e", 40), strings.Repeat("f", 40), next.UpdatedAt.Add(time.Second)); err == nil {
		t.Fatal("database accepted candidate-ready to merge-verified state jump")
	}
	next, err = st.Publication(ctx, operation.ID)
	if err != nil || next.State != gitpublication.StateCandidateReady || next.Version != 3 {
		t.Fatalf("publication changed after rejected state jump: publication=%#v err=%v", next, err)
	}

	prURL := "https://github.com/kuberploy/publication-gitops/pull/7"
	openObservedAt := next.UpdatedAt.Add(time.Second)
	open, err := next.WithPullRequest(gitpublication.PullRequestObservation{
		Repository: next.Repository, Number: 7, URL: prURL, TargetRef: next.TargetRef,
		HeadRef: next.CandidateRef, HeadRevision: next.CandidateRevision,
		State: gitpublication.PullRequestOpen, ObservedAt: openObservedAt,
	}, openObservedAt)
	if err != nil {
		t.Fatalf("open publication=%#v err=%v", open, err)
	}
	if err = st.CompareAndSwapPublication(ctx, next, open); err != nil {
		t.Fatalf("store open publication=%#v err=%v", open, err)
	}
	mergeRevision := strings.Repeat("e", 40)
	mergedObservedAt := open.UpdatedAt.Add(time.Second)
	mergePending, err := open.WithPullRequest(gitpublication.PullRequestObservation{
		Repository: open.Repository, Number: 7, URL: prURL, TargetRef: open.TargetRef,
		HeadRef: open.CandidateRef, HeadRevision: open.CandidateRevision,
		State: gitpublication.PullRequestClosed, Merged: true, MergeRevision: mergeRevision, ObservedAt: mergedObservedAt,
	}, mergedObservedAt)
	if err != nil {
		t.Fatalf("merge-pending publication=%#v err=%v", mergePending, err)
	}
	if err = st.CompareAndSwapPublication(ctx, open, mergePending); err != nil {
		t.Fatalf("store merge-pending publication=%#v err=%v", mergePending, err)
	}
	firstIndexedRevision := strings.Repeat("f", 40)
	indexingAt := mergePending.UpdatedAt
	var state, desiredRevision string
	if err = st.pool.QueryRow(ctx, `SELECT state,desired_revision FROM deployments WHERE id=$1`, operation.TargetID).Scan(&state, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if state != "merge-pending-index" || desiredRevision != "" {
		t.Fatalf("merge verification advanced deployment before indexing: state=%q desired=%q", state, desiredRevision)
	}

	projectionStore, err := gitprojection.NewPostgreSQLStore(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	projectionBinding, err := projectionStore.Binding(ctx, bindingResult.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	command, err := projectionStore.WriteCommand(ctx, operation.ID)
	if err != nil || command.PublicationMode != gitprojection.PublicationPullRequest || command.State != gitprojection.WriteCommandPending {
		t.Fatalf("protected command=%#v err=%v", command, err)
	}
	projectionBinding, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{
		BindingID: projectionBinding.ID, Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef,
		Commit: firstIndexedRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "protected-merge-target", ObservedAt: indexingAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	work, err := projectionStore.ClaimReconciliation(ctx, "protected-index-proof", indexingAt.Add(2*time.Second), time.Minute)
	if err != nil || work.Binding.ID != projectionBinding.ID {
		t.Fatalf("claim binding=%q want=%q work=%#v err=%v", work.Binding.ID, projectionBinding.ID, work, err)
	}
	badGeneration, err := projectionStore.BeginGeneration(ctx, work.Lease, firstIndexedRevision, projectionBinding.ParserVersion, indexingAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	badDocument, err := gitprojection.NewDocument(projectionBinding, badGeneration.Number, application.Value.ID,
		firstIndexedRevision, firstIndexedRevision, strings.Repeat("1", 40), []byte("kind: AppConfig\nmetadata: {}\n"), nil, nil, indexingAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutDocuments(ctx, badGeneration, []gitprojection.Document{badDocument}); err != nil {
		t.Fatal(err)
	}
	policyInput := gitprojection.AppConfigPolicyInput{Binding: projectionBinding, Generation: badGeneration, Current: []gitprojection.Document{badDocument}}
	if err = policyInput.Validate(); err != nil {
		t.Fatalf("policy input invalid before activation: %#v: %v", policyInput, err)
	}
	if _, err = projectionStore.ActivateGeneration(ctx, work.Lease, badGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, indexingAt.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	command, err = projectionStore.WriteCommand(ctx, operation.ID)
	if err != nil || command.State != gitprojection.WriteCommandPending {
		t.Fatalf("mismatched indexed bytes advanced protected command: command=%#v err=%v", command, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT state,desired_revision FROM deployments WHERE id=$1`, operation.TargetID).Scan(&state, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if state != "merge-pending-index" || desiredRevision != "" {
		t.Fatalf("mismatched indexed bytes advanced deployment: state=%q desired=%q", state, desiredRevision)
	}
	if err = projectionStore.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{
		LastCommit: firstIndexedRevision, NextPollAt: indexingAt.Add(time.Hour),
	}, indexingAt.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	// A later authoritative descendant may contain the exact accepted bytes.
	// It remains pending until independent provider verification arrives.
	descendantRevision := strings.Repeat("9", 40)
	projectionBinding, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{
		BindingID: projectionBinding.ID, Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef,
		Commit: descendantRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "protected-merge-descendant", ObservedAt: indexingAt.Add(6 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	work, err = projectionStore.ClaimReconciliation(ctx, "protected-index-proof", indexingAt.Add(7*time.Second), time.Minute)
	if err != nil || work.Binding.ID != projectionBinding.ID {
		t.Fatalf("descendant claim=%#v err=%v", work, err)
	}
	exactGeneration, err := projectionStore.BeginGeneration(ctx, work.Lease, descendantRevision, projectionBinding.ParserVersion, indexingAt.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	exactDocument, err := gitprojection.NewDocument(projectionBinding, exactGeneration.Number, application.Value.ID,
		descendantRevision, descendantRevision, strings.Repeat("2", 40), command.Content, nil, nil, indexingAt.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutDocuments(ctx, exactGeneration, []gitprojection.Document{exactDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = projectionStore.ActivateGeneration(ctx, work.Lease, exactGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, indexingAt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	command, err = projectionStore.WriteCommand(ctx, operation.ID)
	if err != nil || command.State != gitprojection.WriteCommandPending {
		t.Fatalf("generation advanced before provider verification: command=%#v err=%v", command, err)
	}
	if err = projectionStore.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{
		LastCommit: descendantRevision, NextPollAt: indexingAt.Add(time.Hour),
	}, indexingAt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}

	// An unrelated later descendant preserves the effective config revision
	// that Argo renders, while the provider receipt records the newer target tip.
	laterDescendant := strings.Repeat("6", 40)
	projectionBinding, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{
		BindingID: projectionBinding.ID, Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef,
		Commit: laterDescendant, Source: gitprojection.ObservationPoll, ProviderRequest: "protected-merge-later-descendant", ObservedAt: indexingAt.Add(10 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	work, err = projectionStore.ClaimReconciliation(ctx, "protected-index-proof", indexingAt.Add(11*time.Second), time.Minute)
	if err != nil || work.Binding.ID != projectionBinding.ID {
		t.Fatalf("later descendant claim=%#v err=%v", work, err)
	}
	laterGeneration, err := projectionStore.BeginGeneration(ctx, work.Lease, laterDescendant, projectionBinding.ParserVersion, indexingAt.Add(12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	laterDocument, err := gitprojection.NewDocument(projectionBinding, laterGeneration.Number, application.Value.ID,
		laterDescendant, laterDescendant, strings.Repeat("2", 40), command.Content, nil, nil, indexingAt.Add(12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutDocuments(ctx, laterGeneration, []gitprojection.Document{laterDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = projectionStore.ActivateGeneration(ctx, work.Lease, laterGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, indexingAt.Add(13*time.Second)); err != nil {
		t.Fatal(err)
	}
	laterDocument, err = projectionStore.Document(ctx, projectionBinding.ID, laterDocument.Path)
	if err != nil || laterDocument.ConfigRevision != descendantRevision || laterDocument.SourceRevision != laterDescendant {
		t.Fatalf("later descendant did not preserve effective revision: document=%#v err=%v", laterDocument, err)
	}
	verifiedAt := indexingAt.Add(14 * time.Second)
	mergeVerified, err := mergePending.WithVerifiedMerge(laterDescendant, verifiedAt)
	if err != nil || st.CompareAndSwapPublication(ctx, mergePending, mergeVerified) != nil {
		t.Fatalf("late merge verification publication=%#v err=%v", mergeVerified, err)
	}
	command, err = projectionStore.WriteCommand(ctx, operation.ID)
	if err != nil || command.State != gitprojection.WriteCommandIndexed || command.CommittedRevision != laterDescendant ||
		command.IndexedGeneration != laterGeneration.Number {
		t.Fatalf("late provider convergence command=%#v err=%v", command, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT state,desired_revision FROM deployments WHERE id=$1`, operation.TargetID).Scan(&state, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if state != "git-committed" || desiredRevision != descendantRevision || desiredRevision == mergeVerified.TargetRevision || desiredRevision == mergeVerified.CandidateRevision {
		t.Fatalf("exact indexing did not advance verified target: state=%q desired=%q", state, desiredRevision)
	}
	if err = projectionStore.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{
		LastCommit: laterDescendant, NextPollAt: verifiedAt.Add(time.Hour),
	}, verifiedAt); err != nil {
		t.Fatal(err)
	}

	// Provider-before-activation is the complementary ordering: verification
	// leaves the command pending until an exact later generation activates.
	etag, err := gitprojection.StrongETag(projectionBinding, []gitprojection.Document{laterDocument}, nil, plan.ChartDigest, plan.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	updatePlan := &gitprojection.WritePlan{BindingID: projectionBinding.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		ApplicationID: application.Value.ID, BaseRevision: laterDescendant, Precondition: gitprojection.MutationMatchETag,
		ExpectedETag: etag, ChartDigest: plan.ChartDigest, PolicyVersion: plan.PolicyVersion}
	_, updateOperation, err := st.CreateDeployment(ctx, actorID, "publication-deployment-update-"+suffix, "publication-deployment-update-"+suffix,
		"publication-deployment-update-request-"+suffix, domain.CreateDeployment{EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
			Image: "registry.example/api@sha256:" + strings.Repeat("4", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}, updatePlan)
	if err != nil {
		t.Fatal(err)
	}
	updatePublication, err := st.Publication(ctx, updateOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	updateWriteBase, err := updatePublication.WithWriteBase(laterDescendant, verifiedAt.Add(time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, updatePublication, updateWriteBase) != nil {
		t.Fatalf("update write base=%#v err=%v", updateWriteBase, err)
	}
	updateCandidate, err := updateWriteBase.WithCandidate(strings.Repeat("3", 40), verifiedAt.Add(2*time.Second))
	if err != nil || st.CompareAndSwapPublication(ctx, updateWriteBase, updateCandidate) != nil {
		t.Fatalf("update candidate=%#v err=%v", updateCandidate, err)
	}
	updateOpenAt := verifiedAt.Add(3 * time.Second)
	updateOpen, err := updateCandidate.WithPullRequest(gitpublication.PullRequestObservation{Repository: updateCandidate.Repository, Number: 8,
		URL: "https://github.com/kuberploy/publication-gitops/pull/8", TargetRef: updateCandidate.TargetRef, HeadRef: updateCandidate.CandidateRef,
		HeadRevision: updateCandidate.CandidateRevision, State: gitpublication.PullRequestOpen, ObservedAt: updateOpenAt}, updateOpenAt)
	if err != nil || st.CompareAndSwapPublication(ctx, updateCandidate, updateOpen) != nil {
		t.Fatalf("update open=%#v err=%v", updateOpen, err)
	}
	updateMergedAt := verifiedAt.Add(4 * time.Second)
	updateMergePending, err := updateOpen.WithPullRequest(gitpublication.PullRequestObservation{Repository: updateOpen.Repository, Number: 8,
		URL: updateOpen.PullRequestURL, TargetRef: updateOpen.TargetRef, HeadRef: updateOpen.CandidateRef, HeadRevision: updateOpen.CandidateRevision,
		State: gitpublication.PullRequestClosed, Merged: true, MergeRevision: strings.Repeat("2", 40), ObservedAt: updateMergedAt}, updateMergedAt)
	if err != nil || st.CompareAndSwapPublication(ctx, updateOpen, updateMergePending) != nil {
		t.Fatalf("update merge pending=%#v err=%v", updateMergePending, err)
	}
	updateTarget := strings.Repeat("1", 40)
	updateCommand, err := projectionStore.WriteCommand(ctx, updateOperation.ID)
	if err != nil || updateCommand.State != gitprojection.WriteCommandPending {
		t.Fatalf("merge pending advanced before provider verification: command=%#v err=%v", updateCommand, err)
	}
	mergeContentRevision := strings.Repeat("0", 40)
	projectionBinding, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: projectionBinding.ID,
		Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef, Commit: mergeContentRevision,
		Source: gitprojection.ObservationPoll, ProviderRequest: "provider-before-merge-content", ObservedAt: verifiedAt.Add(6 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	work, err = projectionStore.ClaimReconciliation(ctx, "projection-merge-content", verifiedAt.Add(7*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mergeContentGeneration, err := projectionStore.BeginGeneration(ctx, work.Lease, mergeContentRevision, projectionBinding.ParserVersion, verifiedAt.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	mergeContentDocument, err := gitprojection.NewDocument(projectionBinding, mergeContentGeneration.Number, application.Value.ID, mergeContentRevision,
		mergeContentRevision, strings.Repeat("5", 40), updateCommand.Content, nil, nil, verifiedAt.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutDocuments(ctx, mergeContentGeneration, []gitprojection.Document{mergeContentDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = projectionStore.ActivateGeneration(ctx, work.Lease, mergeContentGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, verifiedAt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.FinishReconciliation(ctx, work.Lease, gitprojection.ReconciliationOutcome{
		LastCommit: mergeContentRevision, NextPollAt: verifiedAt.Add(time.Hour),
	}, verifiedAt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	projectionBinding, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: projectionBinding.ID,
		Repository: projectionBinding.Repository, TargetRef: projectionBinding.TargetRef, Commit: updateTarget,
		Source: gitprojection.ObservationPoll, ProviderRequest: "provider-before-activation", ObservedAt: verifiedAt.Add(10 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	updateVerifiedAt := verifiedAt.Add(11 * time.Second)
	updateVerified, err := updateMergePending.WithVerifiedMerge(updateTarget, updateVerifiedAt)
	if err != nil || st.CompareAndSwapPublication(ctx, updateMergePending, updateVerified) != nil {
		t.Fatalf("update merge verified=%#v err=%v", updateVerified, err)
	}
	updateCommand, err = projectionStore.WriteCommand(ctx, updateOperation.ID)
	if err != nil || updateCommand.State != gitprojection.WriteCommandPending {
		t.Fatalf("provider verification advanced before target activation: command=%#v err=%v", updateCommand, err)
	}
	work, err = projectionStore.ClaimReconciliation(ctx, "projection-after-provider", verifiedAt.Add(12*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	updateGeneration, err := projectionStore.BeginGeneration(ctx, work.Lease, updateTarget, projectionBinding.ParserVersion, verifiedAt.Add(13*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	updateDocument, err := gitprojection.NewDocument(projectionBinding, updateGeneration.Number, application.Value.ID, updateTarget, updateTarget,
		strings.Repeat("5", 40), updateCommand.Content, nil, nil, verifiedAt.Add(13*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutDocuments(ctx, updateGeneration, []gitprojection.Document{updateDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err = projectionStore.ActivateGeneration(ctx, work.Lease, updateGeneration, gitprojection.SchemaOnlyAppConfigPolicyValidator{}, verifiedAt.Add(14*time.Second)); err != nil {
		t.Fatal(err)
	}
	updateDocument, err = projectionStore.Document(ctx, projectionBinding.ID, updateDocument.Path)
	if err != nil || updateDocument.ConfigRevision != mergeContentRevision {
		t.Fatalf("provider-before descendant did not preserve effective revision: document=%#v err=%v", updateDocument, err)
	}
	updateCommand, err = projectionStore.WriteCommand(ctx, updateOperation.ID)
	if err != nil || updateCommand.State != gitprojection.WriteCommandIndexed || updateCommand.CommittedRevision != updateTarget ||
		updateCommand.IndexedGeneration != updateGeneration.Number {
		t.Fatalf("provider-before-activation did not converge: command=%#v err=%v", updateCommand, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT state,desired_revision FROM deployments WHERE id=$1`, updateOperation.TargetID).Scan(&state, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if state != "git-committed" || desiredRevision != mergeContentRevision || desiredRevision == updateTarget {
		t.Fatalf("provider-before deployment revision state=%q desired=%q", state, desiredRevision)
	}

	// The ordered repair migration must use the command's historical indexed
	// generation after the binding has advanced, and must be idempotent.
	historicalGeneration := updateCommand.IndexedGeneration
	advancedGeneration := historicalGeneration + 1
	advancedHead := strings.Repeat("a", 40)
	if _, err = st.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,$2,$3,$4,'active',$5,$5)`, projectionBinding.ID, advancedGeneration, advancedHead, projectionBinding.ParserVersion, verifiedAt.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2,projection_generation=$3,
		target_head_observed_at=$4,indexed_at=$4,updated_at=$4,state='ready' WHERE id=$1`, projectionBinding.ID, advancedHead,
		advancedGeneration, verifiedAt.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE deployments SET desired_revision=$2 WHERE id=$1`, updateOperation.TargetID, updateTarget); err != nil {
		t.Fatal(err)
	}
	repairSQL, err := migrations.FS.ReadFile("prisma/migrations/003_repair_protected_desired_revisions/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = st.pool.Exec(ctx, string(repairSQL)); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the last old-RC writer racing after the migration repair. The
	// persistent database authority must correct its descendant target tip.
	if _, err = st.pool.Exec(ctx, `UPDATE deployments SET desired_revision=$2 WHERE id=$1`, updateOperation.TargetID, updateTarget); err != nil {
		t.Fatal(err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT desired_revision FROM deployments WHERE id=$1`, updateOperation.TargetID).Scan(&desiredRevision); err != nil {
		t.Fatal(err)
	}
	if desiredRevision != mergeContentRevision || historicalGeneration == advancedGeneration {
		t.Fatalf("historical repair desired=%q indexedGeneration=%d activeGeneration=%d", desiredRevision, historicalGeneration, advancedGeneration)
	}
	observationResolver, err := argo.NewPostgreSQLObservationTargetResolver(st.pool)
	if err != nil {
		t.Fatal(err)
	}
	observationTarget, err := observationResolver.ResolveArgoObservationTarget(ctx, updateOperation.TargetID)
	if err != nil || observationTarget.DesiredRevision != mergeContentRevision {
		t.Fatalf("repaired observation target=%#v err=%v", observationTarget, err)
	}
	advancedBinding, err := projectionStore.Binding(ctx, projectionBinding.ID)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := argo.RenderApplication(argo.EnvironmentTarget{Project: project.Value, Environment: environment.Value, Binding: advancedBinding,
		ArgoNamespace: "argocd", Runtime: argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime",
			ChartVersion: "0.1.0-rc.297", ChartDigest: "sha256:" + strings.Repeat("7", 64), RendererImage: "ghcr.io/kuberploy/renderer@sha256:" + strings.Repeat("8", 64)}},
		application.Value, domain.Deployment{ID: updateOperation.TargetID, EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
			DesiredRevision: desiredRevision})
	if err != nil || !strings.Contains(string(rendered), `targetRevision: "`+mergeContentRevision+`"`) || strings.Contains(string(rendered), `targetRevision: "`+updateTarget+`"`) {
		t.Fatalf("repaired Argo values revision render err=%v\n%s", err, rendered)
	}
}
