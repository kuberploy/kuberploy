package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPostgreSQLVariableSetAuthorityTriggers(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	actorID, installationCatalogID, repositoryCatalogID := id.New(), id.New(), id.New()
	numericSeed, _ := strconv.ParseInt(strings.ReplaceAll(actorID[:13], "-", ""), 16, 64)
	installationNumber := 700000000 + numericSeed%100000000
	repositoryNumber := 800000000 + numericSeed%100000000
	now := databaseTime(time.Now().UTC())
	suffix := actorID[:8]
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
		VALUES($1,$2,'platform-admin','variable-trigger-test',$2,1,$3)`, actorID, "variable-trigger-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
		VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`, id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO github_installations(
		id,github_installation_id,account_login,account_type,owner_user_id,visibility,team_id,
		repository_selection,repository_count,github_app_id,github_account_id,lifecycle,permissions,last_verified_at,created_at,updated_at)
		VALUES($1,$2,'kuberploy','Organization',$3,'private',NULL,'selected',1,277,$5,
		'active','{"metadata":"read","contents":"write","pull_requests":"write"}'::jsonb,$4,$4,$4)`, installationCatalogID, installationNumber, actorID, now, repositoryNumber); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO github_repositories(
		id,installation_id,github_repository_id,github_owner_id,owner_login,name,lifecycle,last_verified_at,created_at,updated_at)
		VALUES($1,$2,$3,$3,'kuberploy',$4,'active',$5,$5,$5)`, repositoryCatalogID, installationCatalogID, repositoryNumber, "variable-trigger-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "variable-trigger-project-"+suffix, "sha256:"+strings.Repeat("1", 64),
		domain.CreateProject{Name: "Variable Trigger", Slug: "variable-trigger-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, actorID, "variable-trigger-environment-"+suffix, "sha256:"+strings.Repeat("2", 64),
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Protected", Slug: "protected", ProtectionPolicy: domain.EnvironmentProtected})
	if err != nil {
		t.Fatal(err)
	}
	bindingResult, err := store.CreateEnvironmentGitBinding(ctx, actorID, "variable-trigger-binding-"+suffix, "sha256:"+strings.Repeat("3", 64),
		"variable-trigger-binding-request-"+suffix, gitprojection.CreateEnvironmentBindingInput{
			EnvironmentID: environment.Value.ID, LinkedInstallationID: installationCatalogID, LinkedRepositoryID: repositoryCatalogID, GitHubAppID: 277,
			Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: installationNumber, RepositoryID: repositoryNumber,
				Owner: "kuberploy", Name: "variable-trigger-" + suffix}, TargetRef: "refs/heads/main",
		})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	readyAt := now.Add(time.Second)
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,1,$2,$3,'active',$4,$4)`, bindingResult.Value.ID, head, bindingResult.Value.ParserVersion, readyAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',target_head_revision=$2,indexed_revision=$2,
		projection_generation=1,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, bindingResult.Value.ID, head, readyAt); err != nil {
		t.Fatal(err)
	}
	paths, err := gitprojection.DependencyPaths(bindingResult.Value)
	if err != nil {
		t.Fatal(err)
	}
	plan := gitprojection.WritePlan{BindingID: bindingResult.Value.ID, ProjectID: project.Value.ID, EnvironmentID: environment.Value.ID,
		BaseRevision: head, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: bindingResult.Value.ParserVersion,
		VariableScope: "project", VariablePath: paths[0]}
	raw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  REGION: ap-southeast-1\n")
	tokenHash, candidateHash := sha256.Sum256([]byte("variable-trigger-preview-"+suffix)), sha256.Sum256(raw)
	if err = store.CreateVariableSetPreview(ctx, actorID, plan, tokenHash[:], candidateHash[:], time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fingerprint := "sha256:" + strings.Repeat("4", 64)
	accepted, err := store.SaveVariableSet(ctx, actorID, "variable-trigger-save-"+suffix, fingerprint, "variable-trigger-request-"+suffix,
		plan, tokenHash[:], candidateHash[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	started, execute, err := store.StartOperation(ctx, accepted.Value.ID, 1, "variable-worker-"+suffix, time.Minute)
	if err != nil || !execute || started.Kind != "variable-set.git-write" {
		t.Fatalf("variable operation was not leased: operation=%#v execute=%t err=%v", started, execute, err)
	}
	if err = store.RequeueOperation(ctx, accepted.Value.ID, 1, "variable-worker-"+suffix, "GitCommitResultPending", "retry exact VariableSet publication"); err != nil {
		t.Fatalf("variable operation could not be requeued after uncertain publication: %v", err)
	}
	requeued, err := store.GetOperation(ctx, accepted.Value.ID)
	if err != nil || requeued.Status != "queued" || len(requeued.Progress) != 1 || requeued.Progress[0].Status != "pending" {
		t.Fatalf("variable operation did not return to its durable queue: operation=%#v err=%v", requeued, err)
	}
	projectionStore, err := gitprojection.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer projectionStore.Close()
	command, err := projectionStore.WriteCommand(ctx, accepted.Value.ID)
	if err != nil || command.RequestDigest != fingerprint || command.ContentSHA256 == command.RequestDigest || command.PublicationMode != gitprojection.PublicationPullRequest {
		t.Fatalf("command=%#v err=%v", command, err)
	}
	publication, err := store.Publication(ctx, accepted.Value.ID)
	if err != nil || publication.State != gitpublication.StatePendingCandidate || publication.BindingID != plan.BindingID || publication.BaseRevision != head {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	exactReplay, err := store.SaveVariableSet(ctx, actorID, "variable-trigger-save-"+suffix, fingerprint, "replay", gitprojection.WritePlan{}, nil, nil, nil)
	if err != nil || !exactReplay.Replay || exactReplay.Value.ID != accepted.Value.ID {
		t.Fatalf("replay=%#v err=%v", exactReplay, err)
	}
	if _, err = store.SaveVariableSet(ctx, actorID, "variable-trigger-save-"+suffix, "sha256:"+strings.Repeat("5", 64), "replay", gitprojection.WritePlan{}, nil, nil, nil); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("substituted replay fingerprint=%v", err)
	}

	// Direct VariableSet recovery may discover its exact commit only after an
	// unrelated descendant was already indexed. PostgreSQL must preserve the
	// required pending -> git-committed -> indexed transitions while releasing
	// the durable path reservation in one transaction.
	directEnvironment, err := store.CreateEnvironment(ctx, actorID, "variable-trigger-direct-environment-"+suffix, "sha256:"+strings.Repeat("6", 64),
		domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Direct", Slug: "direct-" + suffix, ProtectionPolicy: domain.EnvironmentDevelopment})
	if err != nil {
		t.Fatal(err)
	}
	directBindingResult, err := store.CreateEnvironmentGitBinding(ctx, actorID, "variable-trigger-direct-binding-"+suffix, "sha256:"+strings.Repeat("7", 64),
		"variable-trigger-direct-binding-request-"+suffix, gitprojection.CreateEnvironmentBindingInput{
			EnvironmentID: directEnvironment.Value.ID, LinkedInstallationID: installationCatalogID, LinkedRepositoryID: repositoryCatalogID, GitHubAppID: 277,
			Repository: gitprojection.RepositoryIdentity{Provider: "github", InstallationID: installationNumber, RepositoryID: repositoryNumber,
				Owner: "kuberploy", Name: "variable-trigger-" + suffix}, TargetRef: "refs/heads/main",
		})
	if err != nil {
		t.Fatal(err)
	}
	directReadyAt := databaseTime(time.Now().UTC()).Add(time.Second)
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,1,$2,$3,'active',$4,$4)`, directBindingResult.Value.ID, head, directBindingResult.Value.ParserVersion, directReadyAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',target_head_revision=$2,indexed_revision=$2,
		projection_generation=1,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, directBindingResult.Value.ID, head, directReadyAt); err != nil {
		t.Fatal(err)
	}
	directPaths, err := gitprojection.DependencyPaths(directBindingResult.Value)
	if err != nil {
		t.Fatal(err)
	}
	directPlan := gitprojection.WritePlan{BindingID: directBindingResult.Value.ID, ProjectID: project.Value.ID, EnvironmentID: directEnvironment.Value.ID,
		BaseRevision: head, Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: directBindingResult.Value.ParserVersion,
		VariableScope: "environment", VariablePath: directPaths[1]}
	directRaw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  DIRECT: \"true\"\n")
	directTokenHash, directCandidateHash := sha256.Sum256([]byte("variable-trigger-direct-preview-"+suffix)), sha256.Sum256(directRaw)
	if err = store.CreateVariableSetPreview(ctx, actorID, directPlan, directTokenHash[:], directCandidateHash[:], time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	directFingerprint := "sha256:" + strings.Repeat("8", 64)
	directAccepted, err := store.SaveVariableSet(ctx, actorID, "variable-trigger-direct-save-"+suffix, directFingerprint,
		"variable-trigger-direct-request-"+suffix, directPlan, directTokenHash[:], directCandidateHash[:], directRaw)
	if err != nil {
		t.Fatal(err)
	}
	directCommand, err := projectionStore.WriteCommand(ctx, directAccepted.Value.ID)
	if err != nil || directCommand.PublicationMode != gitprojection.PublicationDirect {
		t.Fatalf("direct command=%#v err=%v", directCommand, err)
	}
	directBinding, err := projectionStore.Binding(ctx, directPlan.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	directReservationAt := directReadyAt.Add(time.Second)
	directLeaseUntil := directReservationAt.Add(time.Minute)
	directReservation := gitprojection.PathReservation{BindingID: directBinding.ID, TargetRef: directBinding.TargetRef, Path: directPlan.VariablePath,
		OperationID: directAccepted.Value.ID, Owner: "variable-trigger-direct-writer", BaseRevision: directPlan.BaseRevision,
		State: gitprojection.ReservationCandidate, LeaseUntil: &directLeaseUntil, CreatedAt: directReservationAt, UpdatedAt: directReservationAt}
	if _, replay, acquireErr := projectionStore.AcquirePath(ctx, directReservation, directReservationAt, time.Minute); acquireErr != nil || replay {
		t.Fatalf("direct variable reservation replay=%t err=%v", replay, acquireErr)
	}
	directOperationCommit, directDescendant := strings.Repeat("6", 40), strings.Repeat("7", 40)
	directIndexedAt := directReservationAt.Add(time.Second)
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,2,$2,$3,'active',$4,$4)`, directBinding.ID, directDescendant, directBinding.ParserVersion, directIndexedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',target_head_revision=$2,indexed_revision=$2,
		projection_generation=2,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, directBinding.ID, directDescendant, directIndexedAt); err != nil {
		t.Fatal(err)
	}
	directBinding, err = projectionStore.Binding(ctx, directBinding.ID)
	if err != nil {
		t.Fatal(err)
	}
	directWriteHead := gitprojection.VerifiedHead{BindingID: directBinding.ID, Repository: directBinding.Repository, TargetRef: directBinding.TargetRef,
		Commit: directDescendant, Source: gitprojection.ObservationWrite, ProviderRequest: "variable-trigger-direct-recovery-" + suffix,
		ObservedAt: directIndexedAt.Add(time.Second)}
	if _, err = projectionStore.FinalizeVerifiedPath(ctx, directBinding.ID, directBinding.TargetRef, directPlan.VariablePath,
		directAccepted.Value.ID, directOperationCommit, directWriteHead, directIndexedAt.Add(2*time.Second)); err != nil {
		t.Fatalf("direct variable recovery could not converge: %v", err)
	}
	directStoredCommand, err := projectionStore.WriteCommand(ctx, directAccepted.Value.ID)
	if err != nil || directStoredCommand.State != gitprojection.WriteCommandIndexed || directStoredCommand.CommittedRevision != directOperationCommit || directStoredCommand.IndexedGeneration != 2 {
		t.Fatalf("recovered direct variable command=%#v err=%v", directStoredCommand, err)
	}
	if _, err = projectionStore.PathReservation(ctx, directBinding.ID, directBinding.TargetRef, directPlan.VariablePath); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("direct variable recovery left path reservation: %v", err)
	}

	assertRejected := func(label, statement string, arguments ...any) {
		t.Helper()
		if _, execErr := store.pool.Exec(ctx, statement, arguments...); execErr == nil {
			t.Fatalf("%s was accepted", label)
		}
	}
	assertRejected("protected pending to git-committed", `UPDATE git_write_commands SET state='git-committed',committed_revision=$2,committed_at=now(),updated_at=now() WHERE operation_id=$1 AND command_kind='variable-set'`, accepted.Value.ID, strings.Repeat("b", 40))
	assertRejected("command authority mutation", `UPDATE git_write_commands SET request_digest=$2 WHERE operation_id=$1 AND command_kind='variable-set'`, accepted.Value.ID, "sha256:"+strings.Repeat("6", 64))
	assertRejected("command deletion", `DELETE FROM git_write_commands WHERE operation_id=$1 AND command_kind='variable-set'`, accepted.Value.ID)
	assertRejected("publication provider identity substitution", `UPDATE git_pull_request_publications SET repository_name=$2,updated_at=updated_at+interval '1 second',version=version+1 WHERE operation_id=$1`, accepted.Value.ID, "substituted-repository")
	assertRejected("publication binding substitution", `UPDATE git_pull_request_publications SET binding_id=$2,updated_at=updated_at+interval '1 second',version=version+1 WHERE operation_id=$1`, accepted.Value.ID, id.New())
	assertRejected("publication base substitution", `UPDATE git_pull_request_publications SET base_revision=$2,updated_at=updated_at+interval '1 second',version=version+1 WHERE operation_id=$1`, accepted.Value.ID, strings.Repeat("c", 40))

	orphanTx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	orphanOperation := id.New()
	orphanNow := time.Now().UTC()
	if _, err = orphanTx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at)
		VALUES($1,'variable-set.git-write','queued','project',$2,'orphan-publication',1,'[]'::jsonb,$3,$3)`, orphanOperation, project.Value.ID, orphanNow); err != nil {
		t.Fatal(err)
	}
	_, orphanErr := orphanTx.Exec(ctx, `INSERT INTO git_pull_request_publications(operation_id,binding_id,provider,installation_id,repository_id,
		repository_owner,repository_name,target_ref,base_revision,candidate_ref,state,created_at,updated_at)
		VALUES($1,$2,'github',$3,$4,'kuberploy',$5,'refs/heads/main',$6,'refs/heads/kuberploy/operations/'||$1::text,'pending-candidate',$7,$7)`,
		orphanOperation, plan.BindingID, installationNumber, repositoryNumber, "variable-trigger-"+suffix, head, orphanNow)
	_ = orphanTx.Rollback(context.Background())
	if orphanErr == nil {
		t.Fatal("publication without exactly one closed command family was accepted")
	}

	secondToken := sha256.Sum256([]byte("variable-trigger-preview-unconsumed-" + suffix))
	if err = store.CreateVariableSetPreview(ctx, actorID, plan, secondToken[:], candidateHash[:], time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertRejected("preview identity substitution", `UPDATE preview_authorities SET base_revision=$2 WHERE token_hash=$1 AND preview_kind='variable-set'`, secondToken[:], strings.Repeat("d", 40))
	assertRejected("preview parser substitution", `UPDATE preview_authorities SET policy_version=$2 WHERE token_hash=$1 AND preview_kind='variable-set'`, secondToken[:], "variables/v2")
	consumedAt := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `UPDATE preview_authorities SET consumed_at=$2 WHERE token_hash=$1 AND preview_kind='variable-set'`, secondToken[:], consumedAt); err != nil {
		t.Fatalf("exact preview consumption: %v", err)
	}
	assertRejected("preview double consumption", `UPDATE preview_authorities SET consumed_at=$2 WHERE token_hash=$1 AND preview_kind='variable-set'`, secondToken[:], consumedAt.Add(time.Second))
	assertRejected("preview deletion", `DELETE FROM preview_authorities WHERE token_hash=$1 AND preview_kind='variable-set'`, secondToken[:])

	driftToken := sha256.Sum256([]byte("variable-trigger-policy-drift-" + suffix))
	if err = store.CreateVariableSetPreview(ctx, actorID, plan, driftToken[:], candidateHash[:], time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE git_repository_bindings SET parser_version='variables/v2',updated_at=updated_at+interval '1 second' WHERE id=$1`, plan.BindingID); err != nil {
		t.Fatal(err)
	}
	pinnedPlan, _, err := store.VariableSetPreviewAuthority(ctx, actorID, driftToken[:])
	if err != nil || pinnedPlan.PolicyVersion != plan.PolicyVersion {
		t.Fatalf("preview parser authority drifted: plan=%#v err=%v", pinnedPlan, err)
	}
	if _, err = store.SaveVariableSet(ctx, actorID, "variable-trigger-policy-drift-save-"+suffix, "sha256:"+strings.Repeat("7", 64), "policy-drift", pinnedPlan, driftToken[:], candidateHash[:], raw); !errors.Is(err, base.ErrPreconditionFailed) {
		t.Fatalf("new save survived parser policy advancement: %v", err)
	}
	postDriftReplay, err := store.SaveVariableSet(ctx, actorID, "variable-trigger-save-"+suffix, fingerprint, "post-drift-replay", gitprojection.WritePlan{}, nil, nil, nil)
	if err != nil || !postDriftReplay.Replay || postDriftReplay.Value.ID != accepted.Value.ID {
		t.Fatalf("accepted replay depended on current parser policy: replay=%#v err=%v", postDriftReplay, err)
	}

	assertDeferredCommandRejected := func(label, kind, targetType, targetID, idemFingerprint string, initialState string) {
		t.Helper()
		tx, beginErr := store.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck
		operationID := id.New()
		createdAt := time.Now().UTC()
		if _, beginErr = tx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at)
			VALUES($1,$2,'queued',$3,$4,$5,1,'[{"name":"git-write","status":"pending"}]'::jsonb,$6,$6)`, operationID, kind, targetType, targetID, label, createdAt); beginErr != nil {
			t.Fatal(beginErr)
		}
		if _, beginErr = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,resource_type,resource_id,operation_id)
			VALUES($1,'resource','variable-sets.save','global',$2,$3,$4,$5,$6)`, actorID, label+operationID, idemFingerprint, targetType, targetID, operationID); beginErr != nil {
			t.Fatal(beginErr)
		}
		committedRevision, committedAt := "", any(nil)
		if initialState != "pending" {
			committedRevision, committedAt = strings.Repeat("e", 40), createdAt
		}
		_, insertErr := tx.Exec(ctx, `INSERT INTO git_write_commands(operation_id,command_kind,actor_id,binding_id,project_id,environment_id,variable_scope,
			target_ref,path,base_revision,precondition,expected_etag,policy_version,content,content_sha256,message,publication_mode,state,
			committed_revision,committed_at,request_digest,created_at,updated_at)
			VALUES($1,'variable-set',$2,$3,$4,$5,'project','refs/heads/main',$6,$7,'create-if-absent','',$8,$9,$10,'test command','pull-request',$11,$12,$13,$14,$15,$15)`,
			operationID, actorID, plan.BindingID, plan.ProjectID, plan.EnvironmentID, plan.VariablePath, plan.BaseRevision, plan.PolicyVersion,
			raw, "sha256:"+hex.EncodeToString(candidateHash[:]), initialState, committedRevision, committedAt, fingerprint, createdAt)
		if insertErr == nil {
			_, insertErr = tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)
		}
		if insertErr == nil {
			t.Fatalf("%s was accepted", label)
		}
	}
	assertDeferredCommandRejected("non-pristine-command", "variable-set.git-write", "project", project.Value.ID, fingerprint, "git-committed")
	assertDeferredCommandRejected("wrong-operation-kind", "deployment.git-write", "project", project.Value.ID, fingerprint, "pending")
	assertDeferredCommandRejected("wrong-operation-target", "variable-set.git-write", "environment", environment.Value.ID, fingerprint, "pending")
	assertDeferredCommandRejected("wrong-operation-fingerprint", "variable-set.git-write", "project", project.Value.ID, "sha256:"+strings.Repeat("9", 64), "pending")

	writeBase, candidateRevision, mergeRevision, targetRevision := strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	transitionAt := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='write-base-ready',write_base_revision=$2,updated_at=$3,version=version+1 WHERE operation_id=$1`, accepted.Value.ID, writeBase, transitionAt); err != nil {
		t.Fatal(err)
	}
	transitionAt = transitionAt.Add(time.Second)
	if _, err = store.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='candidate-ready',candidate_revision=$2,updated_at=$3,version=version+1 WHERE operation_id=$1`, accepted.Value.ID, candidateRevision, transitionAt); err != nil {
		t.Fatal(err)
	}
	transitionAt = transitionAt.Add(time.Second)
	pullRequestURL := "https://github.com/kuberploy/variable-trigger-" + suffix + "/pull/7"
	if _, err = store.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='pull-request-open',pull_request_number=7,
		pull_request_url=$2,pull_request_state='open',provider_observed_at=$3,updated_at=$3,version=version+1 WHERE operation_id=$1`, accepted.Value.ID, pullRequestURL, transitionAt); err != nil {
		t.Fatal(err)
	}
	transitionAt = transitionAt.Add(time.Second)
	if _, err = store.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='merge-pending',pull_request_state='closed',
		merge_revision=$2,provider_observed_at=$3,updated_at=$3,version=version+1 WHERE operation_id=$1`, accepted.Value.ID, mergeRevision, transitionAt); err != nil {
		t.Fatal(err)
	}
	transitionAt = transitionAt.Add(time.Second)
	if _, err = store.pool.Exec(ctx, `UPDATE git_pull_request_publications SET state='merge-verified',target_revision=$2,
		provider_observed_at=$3,updated_at=$3,version=version+1 WHERE operation_id=$1`, accepted.Value.ID, targetRevision, transitionAt); err != nil {
		t.Fatal(err)
	}
	indexedAt := transitionAt.Add(time.Second)
	currentBinding, err := projectionStore.Binding(ctx, plan.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	leaseUntil := transitionAt.Add(time.Minute)
	reservation := gitprojection.PathReservation{BindingID: plan.BindingID, TargetRef: currentBinding.TargetRef, Path: plan.VariablePath,
		OperationID: accepted.Value.ID, Owner: "variable-trigger-writer", BaseRevision: plan.BaseRevision,
		State: gitprojection.ReservationCandidate, LeaseUntil: &leaseUntil, CreatedAt: transitionAt, UpdatedAt: transitionAt}
	if _, replay, acquireErr := projectionStore.AcquirePath(ctx, reservation, transitionAt, time.Minute); acquireErr != nil || replay {
		t.Fatalf("variable reservation replay=%t err=%v", replay, acquireErr)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
		VALUES($1,2,$2,$3,'active',$4,$4)`, plan.BindingID, targetRevision, currentBinding.ParserVersion, indexedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE git_repository_bindings SET state='ready',target_head_revision=$2,indexed_revision=$2,
		projection_generation=2,target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`, plan.BindingID, targetRevision, indexedAt); err != nil {
		t.Fatal(err)
	}
	currentBinding, err = projectionStore.Binding(ctx, plan.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	writeHead := gitprojection.VerifiedHead{BindingID: currentBinding.ID, Repository: currentBinding.Repository, TargetRef: currentBinding.TargetRef,
		Commit: targetRevision, Source: gitprojection.ObservationWrite, ProviderRequest: "variable-trigger-recovery-" + suffix, ObservedAt: indexedAt.Add(time.Second)}
	if _, err = projectionStore.FinalizeVerifiedPath(ctx, currentBinding.ID, currentBinding.TargetRef, plan.VariablePath,
		accepted.Value.ID, targetRevision, writeHead, indexedAt.Add(2*time.Second)); err != nil {
		t.Fatalf("verified variable recovery could not become indexed: %v", err)
	}
	storedCommand, err := projectionStore.WriteCommand(ctx, accepted.Value.ID)
	if err != nil || storedCommand.State != gitprojection.WriteCommandIndexed || storedCommand.CommittedRevision != targetRevision || storedCommand.IndexedGeneration != 2 {
		t.Fatalf("recovered variable command=%#v err=%v", storedCommand, err)
	}
	if _, err = projectionStore.PathReservation(ctx, currentBinding.ID, currentBinding.TargetRef, plan.VariablePath); !errors.Is(err, gitprojection.ErrNotFound) {
		t.Fatalf("verified variable recovery left path reservation: %v", err)
	}
	assertRejected("terminal indexed command mutation", `UPDATE git_write_commands SET updated_at=$2 WHERE operation_id=$1 AND command_kind='variable-set'`, accepted.Value.ID, indexedAt.Add(time.Second))
}
