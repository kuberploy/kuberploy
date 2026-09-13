package gitprojection_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/testdb"
)

// This is the upgrade-recovery shape: the old command was accepted and its
// exact merge indexed, a newer command replaced the App, and provider merge
// verification completed only after the original document ceased to be current.
func TestPostgreSQLHistoricalProtectedUpsertRecovery(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{
		"recover", "late-verification", "unverified", "publication-binding", "publication-ref",
		"publication-installation", "publication-repository", "publication-owner", "publication-name",
		"historical-head", "historical-source", "historical-path", "historical-raw", "historical-hash", "historical-application",
		"historical-invalid", "historical-staging", "historical-failed", "historical-missing",
		"current-operation", "same-generation", "newer-operation-target", "newer-operation-generation",
		"old-operation-target", "old-operation-failed", "old-operation-cancelled", "old-operation-superseded",
		"deployment-environment", "deployment-application", "binding-not-ready", "current-generation-inactive",
		"delete", "direct",
	} {
		t.Run(mode, func(t *testing.T) {
			f := seedHistoricalPublication(t, pool, mode)
			before := f.preservedState(t)
			commandBefore, err := f.store.WriteCommand(t.Context(), f.oldOperation)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "late-verification" {
				pending, err := f.store.Publication(t.Context(), f.oldOperation)
				if err != nil {
					t.Fatal(err)
				}
				verified, err := pending.WithVerifiedMerge(f.currentHead, f.now.Add(10*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				if err = f.store.CompareAndSwapPublication(t.Context(), pending, verified); err != nil {
					t.Fatal(err)
				}
			} else if _, err = f.store.RecoverVerifiedPublications(t.Context(), 100); err != nil {
				t.Fatal(err)
			}
			command, err := f.store.WriteCommand(t.Context(), f.oldOperation)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.preservedState(t); got != before {
				t.Fatal("historical recovery changed the active deployment, placement, operations, or retained projection history")
			}
			if mode != "recover" && mode != "late-verification" {
				if !reflect.DeepEqual(command, commandBefore) {
					t.Fatal("historical recovery accepted incomplete or mismatched evidence")
				}
				return
			}
			if command.State != gitprojection.WriteCommandIndexed || command.IndexedGeneration != 1 || command.CommittedRevision != f.mergeHead {
				t.Fatalf("historically indexed command stayed unresolved: state=%s generation=%d revision=%s", command.State, command.IndexedGeneration, command.CommittedRevision)
			}
			if command.CommittedAt == nil || command.IndexedAt == nil || command.IndexedAt.Before(*command.CommittedAt) {
				t.Fatal("historical recovery produced invalid result timestamps")
			}
			intent := command
			intent.State, intent.CommittedRevision, intent.CommittedAt = commandBefore.State, commandBefore.CommittedRevision, commandBefore.CommittedAt
			intent.IndexedGeneration, intent.IndexedAt, intent.UpdatedAt = commandBefore.IndexedGeneration, commandBefore.IndexedAt, commandBefore.UpdatedAt
			if !reflect.DeepEqual(intent, commandBefore) {
				t.Fatal("historical recovery rewrote immutable command intent")
			}
			if count, err := f.store.RecoverVerifiedPublications(t.Context(), 100); err != nil || count != 0 {
				t.Fatalf("historical command remained in recovery: count=%d err=%v", count, err)
			}
		})
	}
}

type historicalPublicationFixture struct {
	pool                                             *pgxpool.Pool
	store                                            *gitprojection.PostgreSQLStore
	project, environment, application, binding       string
	otherEnvironment, otherApplication, otherBinding string
	user, deployment, oldOperation, newOperation     string
	mergeHead, currentHead                           string
	now                                              time.Time
}

func (f *historicalPublicationFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("fixture statement %.90s: %v", query, err)
	}
}

func (f *historicalPublicationFixture) preservedState(t *testing.T) string {
	t.Helper()
	var body string
	err := f.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'binding',(SELECT to_jsonb(b) FROM git_repository_bindings b WHERE id=$6),
		'deployment',(SELECT to_jsonb(d) FROM deployments d WHERE id=$1),
		'placement',(SELECT to_jsonb(p) FROM environment_app_placements p WHERE environment_id=$2 AND application_id=$3),
		'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM operations o WHERE id IN ($4,$5)),
		'generations',(SELECT jsonb_agg(to_jsonb(g) ORDER BY generation) FROM git_projection_generations g WHERE binding_id=$6),
		'documents',(SELECT jsonb_agg(to_jsonb(d) ORDER BY generation,path) FROM git_projected_documents d WHERE binding_id=$6))::text`,
		f.deployment, f.environment, f.application, f.oldOperation, f.newOperation, f.binding).Scan(&body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func seedHistoricalPublication(t *testing.T, pool *pgxpool.Pool, mode string) *historicalPublicationFixture {
	f := &historicalPublicationFixture{pool: pool, project: id.New(), environment: id.New(), application: id.New(), binding: id.New(),
		otherEnvironment: id.New(), otherApplication: id.New(), otherBinding: id.New(), user: id.New(), deployment: id.New(),
		oldOperation: id.New(), newOperation: id.New(), mergeHead: strings.Repeat("a", 40), currentHead: strings.Repeat("b", 40),
		now: time.Now().UTC().Truncate(time.Microsecond)}
	var err error
	f.store, err = gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Delete exact fixture identities only; every subtest has independent scope.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM git_write_commands WHERE operation_id=$1`, []any{f.oldOperation}},
			{`DELETE FROM git_pull_request_publications WHERE operation_id=$1`, []any{f.oldOperation}},
			{`DELETE FROM environment_app_placements WHERE environment_id=$1 AND application_id=$2`, []any{f.environment, f.application}},
			{`DELETE FROM deployments WHERE id=$1`, []any{f.deployment}},
			{`DELETE FROM operations WHERE id IN ($1,$2)`, []any{f.oldOperation, f.newOperation}},
			{`DELETE FROM git_repository_bindings WHERE id IN ($1,$2)`, []any{f.binding, f.otherBinding}},
			{`DELETE FROM applications WHERE id IN ($1,$2)`, []any{f.application, f.otherApplication}},
			{`DELETE FROM environments WHERE id IN ($1,$2)`, []any{f.environment, f.otherEnvironment}},
			{`DELETE FROM projects WHERE id=$1`, []any{f.project}},
			{`DELETE FROM users WHERE id=$1`, []any{f.user}},
		} {
			if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
				t.Errorf("exact historical fixture cleanup: %v", err)
			}
		}
	})
	f.exec(t, `INSERT INTO projects(id,name,slug,created_at) VALUES($1::uuid,'Historical publication',$1::text,$2)`, f.project, f.now)
	for _, environment := range []string{f.environment, f.otherEnvironment} {
		f.exec(t, `INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
			VALUES($1::uuid,$2,'History',$1::text,'kp-'||$1::text,'kp-'||$1::text,$3)`, environment, f.project, f.now)
	}
	for _, application := range []string{f.application, f.otherApplication} {
		f.exec(t, `INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1::uuid,$2,'History',$1::text,$3)`, application, f.project, f.now)
	}
	f.exec(t, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at)
		VALUES($1::uuid,'History','platform-admin','history',$1::text,1,$2)`, f.user, f.now)
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 930000001, RepositoryID: 930000002, Owner: "kuberploy", Name: "history-fixture"}
	binding, err := gitprojection.NewGitHubEnvironmentBinding(f.binding, f.project, f.environment, repository, "refs/heads/history", f.now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State, binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration = gitprojection.BindingReady, strings.Repeat("c", 40), strings.Repeat("c", 40), 2
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = f.now.Add(5*time.Second), f.now.Add(5*time.Second), f.now.Add(5*time.Second)
	if err = f.store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	other, err := gitprojection.NewGitHubEnvironmentBinding(f.otherBinding, f.project, f.otherEnvironment, repository, binding.TargetRef, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.PutBinding(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	for index, operation := range []string{f.oldOperation, f.newOperation} {
		f.exec(t, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,created_at,updated_at)
			VALUES($1::uuid,'deployment.git-write','succeeded','deployment',$2,$1::text,$3,$4,$4)`, operation, f.deployment, index+1, f.now)
	}
	f.exec(t, `INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,state,operation_id,generation,runtime,
		desired_revision,observed_revision,created_at,updated_at) VALUES($1,$2,$3,'registry.example.test/app@sha256:'||repeat('1',64),1,8080,
		'healthy',$4,2,'{}',$5,$5,$6,$6)`, f.deployment, f.environment, f.application, f.newOperation, f.currentHead, f.now)
	f.exec(t, `INSERT INTO environment_app_placements(project_id,environment_id,application_id,state,desired_state,created_at,updated_at)
		VALUES($1,$2,$3,'active','running',$4,$4)`, f.project, f.environment, f.application, f.now)
	plan := gitprojection.WritePlan{BindingID: f.binding, ProjectID: f.project, EnvironmentID: f.environment, ApplicationID: f.application,
		BaseRevision: strings.Repeat("c", 40), Precondition: gitprojection.MutationMatchETag, ExpectedETag: `"sha256:` + strings.Repeat("d", 64) + `"`,
		ChartDigest: "sha256:" + strings.Repeat("e", 64), PolicyVersion: "history-test-v1"}
	command, err := gitprojection.NewWriteCommand(f.oldOperation, f.deployment, f.user, plan, binding, []byte("replicas: 1\n"), "Review historical App", f.now)
	if err != nil {
		t.Fatal(err)
	}
	command.PublicationMode = gitprojection.PublicationPullRequest
	if mode == "delete" {
		command.Action = gitprojection.MutationDelete
	} else if mode == "direct" {
		command.PublicationMode = gitprojection.PublicationDirect
	}
	if err = f.store.PutWriteCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE git_repository_bindings SET target_head_revision=$2,indexed_revision=$2 WHERE id=$1`, f.binding, f.currentHead)
	for generation, head := range []string{f.mergeHead, f.currentHead} {
		f.exec(t, `INSERT INTO git_projection_generations(binding_id,generation,head_revision,parser_version,state,started_at,activated_at)
			VALUES($1,$2,$3,$4,'active',$5,$5)`, f.binding, generation+1, head, binding.ParserVersion, f.now.Add(time.Duration(generation+2)*time.Second))
		raw, digest := command.Content, command.ContentSHA256
		if generation == 1 {
			raw = []byte("replicas: 2\n")
			sum := sha256.Sum256(raw)
			digest = "sha256:" + hex.EncodeToString(sum[:])
		}
		f.exec(t, `INSERT INTO git_projected_documents(binding_id,generation,path,application_id,
			source_revision,config_revision,blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,parser_version,indexed_at)
			VALUES($1,$2,$3,$4,$5,$5,repeat('8',40),$6,$7,'{}',true,'[]','app-v1',$8,$9)`,
			f.binding, generation+1, command.Path, command.Plan.ApplicationID, head, digest, raw, binding.ParserVersion, f.now.Add(4*time.Second))
	}
	publicationBinding, targetRef := f.binding, binding.TargetRef
	installation, repositoryID, owner, name := repository.InstallationID, repository.RepositoryID, repository.Owner, repository.Name
	switch mode {
	case "publication-binding":
		publicationBinding = f.otherBinding
	case "publication-ref":
		targetRef = "refs/heads/other"
	case "publication-installation":
		installation++
	case "publication-repository":
		repositoryID++
	case "publication-owner":
		owner = "other"
	case "publication-name":
		name = "other"
	}
	publication, err := gitpublication.NewPublication(f.oldOperation, publicationBinding, gitpublication.Repository{
		InstallationID: installation, ID: repositoryID, Owner: owner, Name: name}, targetRef, command.Plan.BaseRevision, f.now)
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.CreatePublication(t.Context(), publication)
	if strings.HasPrefix(mode, "publication-") || mode == "direct" {
		// These invalid identities cannot enter retained history: the baseline
		// migration rejects them before recovery can ever select the command.
		if !errors.Is(err, gitpublication.ErrConflict) {
			t.Fatalf("invalid protected publication entered history: %v", err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		save := func(next gitpublication.Publication, transitionErr error) {
			t.Helper()
			if transitionErr != nil {
				t.Fatal(transitionErr)
			}
			if err := f.store.CompareAndSwapPublication(t.Context(), publication, next); err != nil {
				t.Fatal(err)
			}
			publication = next
		}
		save(publication.WithWriteBase(command.Plan.BaseRevision, f.now.Add(time.Second)))
		save(publication.WithCandidate(strings.Repeat("7", 40), f.now.Add(2*time.Second)))
		observation := gitpublication.PullRequestObservation{Repository: publication.Repository, Number: 1,
			URL: "https://github.com/" + owner + "/" + name + "/pull/1", TargetRef: targetRef,
			HeadRef: publication.CandidateRef, HeadRevision: publication.CandidateRevision,
			State: gitpublication.PullRequestOpen, ObservedAt: f.now.Add(3 * time.Second)}
		save(publication.WithPullRequest(observation, observation.ObservedAt))
		observation.State, observation.Merged, observation.MergeRevision = gitpublication.PullRequestClosed, true, f.mergeHead
		observation.ObservedAt = f.now.Add(4 * time.Second)
		save(publication.WithPullRequest(observation, observation.ObservedAt))
		if mode != "unverified" && mode != "late-verification" {
			// Simulate the older worker's durable verified receipt without its
			// missing historical-command convergence. Do not bypass DDL guards.
			verified, err := publication.WithVerifiedMerge(f.currentHead, f.now.Add(9*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE git_pull_request_publications SET state='merge-verified',target_revision=$2,
				updated_at=$3,version=$4 WHERE operation_id=$1`, f.oldOperation, verified.TargetRevision, verified.UpdatedAt, verified.Version)
		}
	}
	mutations := map[string]string{
		"historical-head":             `UPDATE git_projection_generations SET head_revision=repeat('6',40) WHERE binding_id=$1 AND generation=1`,
		"historical-source":           `UPDATE git_projected_documents SET source_revision=repeat('6',40) WHERE binding_id=$1 AND generation=1`,
		"historical-path":             `UPDATE git_projected_documents SET path=path||'.other' WHERE binding_id=$1 AND generation=1`,
		"historical-raw":              `UPDATE git_projected_documents SET raw='replicas: 3'::bytea WHERE binding_id=$1 AND generation=1`,
		"historical-hash":             `UPDATE git_projected_documents SET content_sha256='sha256:'||repeat('6',64) WHERE binding_id=$1 AND generation=1`,
		"historical-invalid":          `UPDATE git_projected_documents SET valid=false,diagnostics='[{"code":"invalid"}]' WHERE binding_id=$1 AND generation=1`,
		"historical-staging":          `UPDATE git_projection_generations SET state='staging',activated_at=NULL WHERE binding_id=$1 AND generation=1`,
		"historical-failed":           `UPDATE git_projection_generations SET state='failed',activated_at=NULL WHERE binding_id=$1 AND generation=1`,
		"historical-missing":          `DELETE FROM git_projection_generations WHERE binding_id=$1 AND generation=1`,
		"binding-not-ready":           `UPDATE git_repository_bindings SET state='waiting-for-git' WHERE id=$1`,
		"current-generation-inactive": `UPDATE git_projection_generations SET state='staging',activated_at=NULL WHERE binding_id=$1 AND generation=2`,
	}
	if query := mutations[mode]; query != "" {
		f.exec(t, query, f.binding)
	}
	switch mode {
	case "historical-application":
		f.exec(t, `UPDATE git_projected_documents SET application_id=$2 WHERE binding_id=$1 AND generation=1`, f.binding, f.otherApplication)
	case "current-operation":
		f.exec(t, `UPDATE deployments SET operation_id=$2 WHERE id=$1`, f.deployment, f.oldOperation)
	case "same-generation":
		f.exec(t, `UPDATE deployments SET generation=1 WHERE id=$1`, f.deployment)
	case "newer-operation-target":
		f.exec(t, `UPDATE operations SET target_id=$2 WHERE id=$1`, f.newOperation, f.application)
	case "newer-operation-generation":
		f.exec(t, `UPDATE operations SET generation=3 WHERE id=$1`, f.newOperation)
	case "old-operation-target":
		f.exec(t, `UPDATE operations SET target_id=$2 WHERE id=$1`, f.oldOperation, f.application)
	case "old-operation-failed", "old-operation-cancelled", "old-operation-superseded":
		f.exec(t, `UPDATE operations SET status=$2 WHERE id=$1`, f.oldOperation, strings.TrimPrefix(mode, "old-operation-"))
	case "deployment-environment":
		f.exec(t, `UPDATE deployments SET environment_id=$2 WHERE id=$1`, f.deployment, f.otherEnvironment)
	case "deployment-application":
		f.exec(t, `UPDATE deployments SET application_id=$2 WHERE id=$1`, f.deployment, f.otherApplication)
	}
	return f
}
