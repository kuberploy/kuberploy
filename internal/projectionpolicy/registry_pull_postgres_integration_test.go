package projectionpolicy_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/projectionpolicy"
	storepostgres "github.com/kuberploy/kuberploy/internal/store/postgres"
)

func TestPostgreSQLRegistryPullPolicyIsExactAtomicAndNonDestructive(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = storepostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	userID, teamID, projectID := id.New(), id.New(), id.New()
	environmentID, applicationID, bindingID, targetID, ambiguousTargetID := id.New(), id.New(), id.New(), id.New(), id.New()
	pullCredentialID := id.New()
	suffix := strings.ReplaceAll(projectID, "-", "")[:12]
	namespace := "pull-policy-" + suffix
	repository := "tenant/" + projectID + "/" + applicationID
	server := "registry." + suffix + ".example.test:5000"
	pullRef := "runtime-pull/" + suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	cleanup := func(cleanupContext context.Context) {
		_, _ = pool.Exec(cleanupContext, `DELETE FROM runtime_registry_pull_artifacts WHERE environment_id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projected_documents WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_projection_generations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_safety_poll_cursors WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_verified_head_observations WHERE binding_id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM git_repository_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM application_registry_pull_selections WHERE application_id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM project_registry_pull_credentials WHERE id=$1`, pullCredentialID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM service_registry_policies WHERE registry_target_id=$1`, targetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM service_registry_policies WHERE registry_target_id=$1`, ambiguousTargetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM registry_targets WHERE id=$1`, targetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM registry_targets WHERE id=$1`, ambiguousTargetID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM applications WHERE id=$1`, applicationID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM environments WHERE id=$1`, environmentID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM projects WHERE id=$1`, projectID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM teams WHERE id=$1`, teamID)
		_, _ = pool.Exec(cleanupContext, `DELETE FROM users WHERE id=$1`, userID)
	}
	cleanup(ctx)
	defer cleanup(context.Background())

	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,login,role,issuer,subject,grant_revision,created_at)
			VALUES($1,$2,'platform-admin','registry-pull-policy',$2,1,$3)`, []any{userID, "pull-policy-" + suffix, now}},
		{`INSERT INTO teams(id,name,slug,created_by,created_at) VALUES($1,$2,$3,$4,$5)`, []any{teamID, "Pull policy " + suffix, "pull-policy-" + suffix, userID, now}},
		{`INSERT INTO projects(id,name,slug,team_id,created_at) VALUES($1,$2,$3,$4,$5)`, []any{projectID, "Pull project " + suffix, "pull-project-" + suffix, teamID, now}},
		{`INSERT INTO environments(id,project_id,name,slug,namespace,argo_project,created_at)
			VALUES($1,$2,'Production',$3,$4,$5,$6)`, []any{environmentID, projectID, "production-" + suffix, namespace, "argo-" + suffix, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_at) VALUES($1,$2,'API',$3,$4)`, []any{applicationID, projectID, "api-" + suffix, now}},
		{`INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,pull_credential_ref,created_at,updated_at)
			VALUES($1,$2,'external',$3,'tenant',$4,$5,$5)`, []any{targetID, "pull-target-" + suffix, "https://" + server, pullRef, now}},
		{`INSERT INTO service_registry_policies(registry_target_id,service_id,repository,created_at,updated_at)
			VALUES($1,$2,$3,$4,$4)`, []any{targetID, applicationID, repository, now}},
	} {
		if _, err = pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	binding, err := gitprojection.NewGitHubEnvironmentBinding(bindingID, projectID, environmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7_500_000_000_000_001, RepositoryID: 7_500_000_000_000_002, Owner: "kuberploy", Name: "pull-policy"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	projectionStore, err := gitprojection.NewPostgreSQLStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = projectionStore.PutBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	work, err := projectionStore.ClaimReconciliation(ctx, "registry-pull-policy-worker", now.Add(time.Second), 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	config := imagepull.DefaultRuntimeConfig()
	config.Enabled = true
	config.Namespaces = []string{namespace}
	config.Profiles = []imagepull.Profile{{Name: "external-main", TargetID: targetID, RegistryServer: server,
		CredentialRef: pullRef, Revision: 1, SourceSecretRef: "pull-source-" + suffix, SourceSecretKey: ".dockerconfigjson"}}
	if err = config.Validate(); err != nil {
		t.Fatal(err)
	}
	policyFor := func(runtimeConfig imagepull.RuntimeConfig) *projectionpolicy.Validator {
		return &projectionpolicy.Validator{Registry: &projectionpolicy.RegistryPullReferencePolicy{Config: runtimeConfig}}
	}
	resolverTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolved, present, resolveErr := projectionpolicy.ResolveRegistryPullTx(ctx, resolverTx, config, applicationID, environmentID, server+"/"+repository)
	if resolveErr != nil || !present || resolved != (projectionpolicy.RegistryPullReference{TargetID: targetID, ProfileName: "external-main", ProfileRevision: 1}) {
		resolverTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("resolved=%#v present=%t err=%v", resolved, present, resolveErr)
	}
	if resolved, present, resolveErr = projectionpolicy.ResolveRegistryPullTx(ctx, resolverTx, config, applicationID, environmentID, "docker.io/library/nginx"); resolveErr != nil || present || resolved != (projectionpolicy.RegistryPullReference{}) {
		resolverTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("public resolve=%#v present=%t err=%v", resolved, present, resolveErr)
	}
	if _, _, resolveErr = projectionpolicy.ResolveRegistryPullTx(ctx, resolverTx, imagepull.RuntimeConfig{}, applicationID, environmentID, server+"/"+repository); !errors.Is(resolveErr, imagepull.ErrUnavailable) {
		resolverTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("disabled private resolver error=%v", resolveErr)
	}
	if err = resolverTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO project_registry_pull_credentials(id,project_id,registry_target_id,name,created_by,created_at,updated_at)
		VALUES($1,$2,$3,'Production pull',$4,$5,$5)`, pullCredentialID, projectID, targetID, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO application_registry_pull_selections(application_id,mode,project_credential_id,updated_by,updated_at)
		VALUES($1,'public',NULL,$2,$3)`, applicationID, userID, now); err != nil {
		t.Fatal(err)
	}
	publicSelectionTx, beginErr := pool.Begin(ctx)
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if resolved, present, resolveErr = projectionpolicy.ResolveRegistryPullTx(ctx, publicSelectionTx, config, applicationID, environmentID, server+"/"+repository); resolveErr != nil || present || resolved != (projectionpolicy.RegistryPullReference{}) {
		publicSelectionTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("explicit public resolve=%#v present=%t err=%v", resolved, present, resolveErr)
	}
	if err = publicSelectionTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE application_registry_pull_selections
		SET mode='project-credential',project_credential_id=$2,updated_at=$3 WHERE application_id=$1`, applicationID, pullCredentialID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	selectedTx, beginErr := pool.Begin(ctx)
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if resolved, present, resolveErr = projectionpolicy.ResolveRegistryPullTx(ctx, selectedTx, config, applicationID, environmentID, server+"/"+repository); resolveErr != nil || !present || resolved != (projectionpolicy.RegistryPullReference{TargetID: targetID, ProfileName: "external-main", ProfileRevision: 1}) {
		selectedTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("selected resolve=%#v present=%t err=%v", resolved, present, resolveErr)
	}
	if err = selectedTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO registry_targets(id,name,mode,endpoint,repository_prefix,pull_credential_ref,created_at,updated_at)
		VALUES($1,$2,'external',$3,'tenant',$4,$5,$5)`, ambiguousTargetID, "pull-ambiguous-"+suffix, "https://"+server, "runtime-pull/ambiguous-"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO service_registry_policies(registry_target_id,service_id,repository,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)`, ambiguousTargetID, applicationID, repository, now); err != nil {
		t.Fatal(err)
	}
	ambiguousTx, beginErr := pool.Begin(ctx)
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if _, _, resolveErr = projectionpolicy.ResolveRegistryPullTx(ctx, ambiguousTx, config, applicationID, environmentID, server+"/"+repository); !errors.Is(resolveErr, imagepull.ErrConflict) {
		ambiguousTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("ambiguous resolver error=%v", resolveErr)
	}
	if err = ambiguousTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM service_registry_policies WHERE registry_target_id=$1`, ambiguousTargetID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM registry_targets WHERE id=$1`, ambiguousTargetID); err != nil {
		t.Fatal(err)
	}

	project := domain.Project{ID: projectID, Slug: "pull-project-" + suffix}
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Slug: "production-" + suffix}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api-" + suffix}
	deployment := domain.Deployment{Image: server + "/" + repository + "@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	publicRaw, err := gitops.RenderAppConfig(project, environment, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	privateRaw := withRegistryPull(t, publicRaw, targetID, "external-main", 1)

	sequence := 0
	stage := func(raw []byte, include bool, validator *projectionpolicy.Validator) (gitprojection.Binding, gitprojection.Document, error) {
		t.Helper()
		headCharacter := "123456789abcdef"[sequence]
		head := strings.Repeat(string(headCharacter), 40)
		observedAt := now.Add(time.Duration(2+sequence*6) * time.Second)
		sequence++
		var observed gitprojection.Binding
		observed, _, err = projectionStore.RecordVerifiedHead(ctx, gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
			TargetRef: binding.TargetRef, Commit: head, Source: gitprojection.ObservationPoll,
			ProviderRequest: "registry-pull-policy-" + string(headCharacter), ObservedAt: observedAt})
		if err != nil {
			return gitprojection.Binding{}, gitprojection.Document{}, err
		}
		binding = observed
		generation, beginErr := projectionStore.BeginGeneration(ctx, work.Lease, head, binding.ParserVersion, observedAt.Add(time.Second))
		if beginErr != nil {
			return gitprojection.Binding{}, gitprojection.Document{}, beginErr
		}
		var document gitprojection.Document
		if include {
			parsed, _, diagnostics := appconfig.ParseAndValidate(raw)
			if len(diagnostics) != 0 {
				t.Fatalf("invalid staged AppConfig: %#v\n%s", diagnostics, raw)
			}
			document, err = gitprojection.NewDocument(binding, generation.Number, applicationID, head, head,
				strings.Repeat(string("abcdef0123456789"[sequence]), 40), raw, parsed, nil, observedAt.Add(2*time.Second))
			if err != nil {
				return gitprojection.Binding{}, gitprojection.Document{}, err
			}
			if err = projectionStore.PutDocuments(ctx, generation, []gitprojection.Document{document}); err != nil {
				return gitprojection.Binding{}, gitprojection.Document{}, err
			}
		} else if err = projectionStore.PutDocuments(ctx, generation, nil); err != nil {
			return gitprojection.Binding{}, gitprojection.Document{}, err
		}
		activated, activateErr := projectionStore.ActivateGeneration(ctx, work.Lease, generation, validator, observedAt.Add(3*time.Second))
		if activateErr == nil {
			binding = activated
		}
		return activated, document, activateErr
	}

	// An AppConfig without the locked pull block remains credential-free and
	// does not create an artifact, even when its repository happens to be one
	// that also has a private registry policy.
	activated, publicDocument, err := stage(publicRaw, true, policyFor(config))
	if err != nil || activated.State != gitprojection.BindingReady {
		t.Fatalf("public activation binding=%#v err=%v", activated, err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, publicDocument.Path, true, "")
	assertArtifactCount(t, pool, environmentID, targetID, 0)
	publicPolicyDocument := typedPolicyDocument(t, binding, publicDocument, publicRaw, teamID, namespace)
	if eligible, eligibilityErr := registryPullEligible(t, pool, publicPolicyDocument, binding.UpdatedAt.Add(time.Second), time.Minute); eligibilityErr != nil || !eligible {
		t.Fatalf("public eligibility=%t err=%v", eligible, eligibilityErr)
	}
	_, unavailableRuntimeDocument, err := stage(privateRaw, true, &projectionpolicy.Validator{})
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, unavailableRuntimeDocument.Path, false, "RegistryPullReferencePolicyUnavailable")
	assertArtifactCount(t, pool, environmentID, targetID, 0)

	activated, privateDocument, err := stage(privateRaw, true, policyFor(config))
	if err != nil || activated.State != gitprojection.BindingReady {
		t.Fatalf("private activation binding=%#v err=%v", activated, err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, privateDocument.Path, true, "")
	privatePolicyDocument := typedPolicyDocument(t, binding, privateDocument, privateRaw, teamID, namespace)
	readyAt := binding.UpdatedAt.Add(time.Second)
	if eligible, eligibilityErr := registryPullEligible(t, pool, privatePolicyDocument, readyAt, time.Minute); eligibilityErr != nil || eligible {
		t.Fatalf("awaiting artifact eligibility=%t err=%v", eligible, eligibilityErr)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_observed_at=$4,observed_uid=$5,observed_resource_version='1',
		    next_observation_at=$6,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 1, readyAt, id.New(), readyAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if eligible, eligibilityErr := registryPullEligible(t, pool, privatePolicyDocument, readyAt.Add(time.Second), time.Minute); eligibilityErr != nil || !eligible {
		t.Fatalf("ready artifact eligibility=%t err=%v", eligible, eligibilityErr)
	}
	if eligible, eligibilityErr := registryPullEligible(t, pool, privatePolicyDocument, readyAt.Add(2*time.Minute), time.Minute); eligibilityErr != nil || eligible {
		t.Fatalf("stale artifact eligibility=%t err=%v", eligible, eligibilityErr)
	}
	firstArtifact := activeArtifact(t, pool, environmentID, targetID)
	if firstArtifact.revision != 1 || firstArtifact.profile != "external-main" || firstArtifact.pullRef != pullRef || firstArtifact.secretName == "" {
		t.Fatalf("unexpected first artifact: %#v", firstArtifact)
	}

	// Re-indexing the same exact profile at a later source commit is an artifact
	// no-op: identity and timestamps do not drift.
	_, _, err = stage(privateRaw, true, policyFor(config))
	if err != nil {
		t.Fatal(err)
	}
	replayArtifact := activeArtifact(t, pool, environmentID, targetID)
	if replayArtifact != firstArtifact {
		t.Fatalf("artifact replay mutated durable state: before=%#v after=%#v", firstArtifact, replayArtifact)
	}

	// Exact application-policy and profile metadata are semantic diagnostics;
	// neither can rotate or remove the previously deployable artifact.
	if _, err = pool.Exec(ctx, `DELETE FROM service_registry_policies WHERE registry_target_id=$1 AND service_id=$2`, targetID, applicationID); err != nil {
		t.Fatal(err)
	}
	_, unavailableDocument, err := stage(privateRaw, true, policyFor(config))
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, unavailableDocument.Path, false, "RegistryPullPolicyUnavailable")
	if activeArtifact(t, pool, environmentID, targetID) != firstArtifact {
		t.Fatal("missing application policy changed the active artifact")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO service_registry_policies(registry_target_id,service_id,repository,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)`, targetID, applicationID, repository, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_registry_policies SET repository=$3,updated_at=$4
		WHERE registry_target_id=$1 AND service_id=$2`, targetID, applicationID, repository+"-other", now.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, repositoryDocument, err := stage(privateRaw, true, policyFor(config))
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, repositoryDocument.Path, false, "RegistryPullRepositoryMismatch")
	if activeArtifact(t, pool, environmentID, targetID) != firstArtifact {
		t.Fatal("repository mismatch changed the active artifact")
	}
	if _, err = pool.Exec(ctx, `UPDATE service_registry_policies SET repository=$3,updated_at=$4
		WHERE registry_target_id=$1 AND service_id=$2`, targetID, applicationID, repository, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	tamperedProfile := strings.Replace(string(privateRaw), "profileRevision: 1", "profileRevision: 9", 1)
	_, mismatchDocument, err := stage([]byte(tamperedProfile), true, policyFor(config))
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, mismatchDocument.Path, false, "RegistryPullProfileMismatch")
	if activeArtifact(t, pool, environmentID, targetID) != firstArtifact {
		t.Fatal("profile mismatch changed the active artifact")
	}

	// A durable artifact conflict is an infrastructure/state failure, not a
	// diagnostic that could commit partial rotation. The prior indexed
	// generation remains active while the new head stays unindexed.
	failedAt := binding.UpdatedAt.Add(time.Second)
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='failed',consecutive_failures=1,last_failure_code='test-failure',updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`, environmentID, targetID, 1, failedAt); err != nil {
		t.Fatal(err)
	}
	priorIndexed := binding.IndexedRevision
	_, _, err = stage(privateRaw, true, policyFor(config))
	if !errors.Is(err, imagepull.ErrConflict) {
		t.Fatalf("artifact conflict error=%v", err)
	}
	currentBinding, getErr := projectionStore.Binding(ctx, binding.ID)
	if getErr != nil || currentBinding.IndexedRevision != priorIndexed || currentBinding.TargetHeadRevision == priorIndexed {
		t.Fatalf("failed ensure advanced indexed state: binding=%#v err=%v", currentBinding, getErr)
	}

	// Operator rotation creates revision 2 and retains revision 1 inactive.
	rotatedConfig := config
	rotatedConfig.Profiles = append([]imagepull.Profile(nil), config.Profiles...)
	rotatedConfig.Profiles[0].Revision = 2
	if err = rotatedConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	rotatedRaw := withRegistryPull(t, publicRaw, targetID, "external-main", 2)
	rotatedBinding, rotatedDocument, err := stage(rotatedRaw, true, policyFor(rotatedConfig))
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedDiagnostic(t, projectionStore, bindingID, rotatedDocument.Path, true, "")
	rotatedArtifact := activeArtifact(t, pool, environmentID, targetID)
	if rotatedArtifact.revision != 2 || rotatedArtifact.secretName == firstArtifact.secretName {
		t.Fatalf("profile rotation did not create a new identity: first=%#v rotated=%#v", firstArtifact, rotatedArtifact)
	}
	var oldActive bool
	if err = pool.QueryRow(ctx, `SELECT active FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=1`, environmentID, targetID).Scan(&oldActive); err != nil || oldActive {
		t.Fatalf("old artifact active=%t err=%v", oldActive, err)
	}
	rotatedPolicyDocument := typedPolicyDocument(t, rotatedBinding, rotatedDocument, rotatedRaw, teamID, namespace)
	rotatedReadyAt := rotatedBinding.UpdatedAt.Add(time.Second)
	if eligible, eligibilityErr := registryPullEligible(t, pool, rotatedPolicyDocument, rotatedReadyAt, time.Minute); eligibilityErr != nil || eligible {
		t.Fatalf("rotated awaiting eligibility=%t err=%v", eligible, eligibilityErr)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts
		SET runtime_state='ready',last_observed_at=$4,observed_uid=$5,observed_resource_version='2',
		    next_observation_at=$6,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`,
		environmentID, targetID, 2, rotatedReadyAt, id.New(), rotatedReadyAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rotatedArtifact = activeArtifact(t, pool, environmentID, targetID)
	if eligible, eligibilityErr := registryPullEligible(t, pool, rotatedPolicyDocument, rotatedReadyAt.Add(time.Second), time.Minute); eligibilityErr != nil || !eligible {
		t.Fatalf("rotated ready eligibility=%t err=%v", eligible, eligibilityErr)
	}

	// Deleting one AppConfig cannot prove a shared environment/target Secret is
	// unused. Schema 025 therefore retains the active artifact exactly.
	deletedBinding, _, err := stage(nil, false, policyFor(rotatedConfig))
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete := activeArtifact(t, pool, environmentID, targetID); afterDelete != rotatedArtifact {
		t.Fatalf("deletion destructively changed shared artifact: before=%#v after=%#v", rotatedArtifact, afterDelete)
	}
	deactivatedAt := deletedBinding.UpdatedAt.Add(time.Second)
	if _, err = pool.Exec(ctx, `UPDATE runtime_registry_pull_artifacts SET active=false,updated_at=$4
		WHERE environment_id=$1 AND registry_target_id=$2 AND profile_revision=$3`, environmentID, targetID, 2, deactivatedAt); err != nil {
		t.Fatal(err)
	}
	if eligible, eligibilityErr := registryPullEligible(t, pool, rotatedPolicyDocument, deactivatedAt.Add(time.Second), time.Minute); eligibilityErr != nil || eligible {
		t.Fatalf("inactive artifact eligibility=%t err=%v", eligible, eligibilityErr)
	}
}

func withRegistryPull(t testing.TB, raw []byte, targetID, profileName string, revision int64) []byte {
	t.Helper()
	needle := "    release:\n"
	replacement := "    registryPull:\n" +
		"      targetId: " + targetID + "\n" +
		"      profileName: " + profileName + "\n" +
		"      profileRevision: " + strconv.FormatInt(revision, 10) + "\n" +
		needle
	if revision < 1 || !strings.Contains(string(raw), needle) {
		t.Fatalf("invalid registry pull fixture revision=%d", revision)
	}
	return []byte(strings.Replace(string(raw), needle, replacement, 1))
}

func assertProjectedDiagnostic(t testing.TB, store *gitprojection.PostgreSQLStore, bindingID, path string, valid bool, code string) {
	t.Helper()
	document, err := store.Document(t.Context(), bindingID, path)
	if err != nil {
		t.Fatal(err)
	}
	if document.Valid != valid || code == "" && len(document.Diagnostics) != 0 || code != "" && (len(document.Diagnostics) != 1 || document.Diagnostics[0].Code != code) {
		t.Fatalf("projected diagnostic mismatch: %#v", document)
	}
}

type registryPullArtifactRow struct {
	revision   int64
	profile    string
	pullRef    string
	secretName string
	createdAt  time.Time
	updatedAt  time.Time
}

func activeArtifact(t testing.TB, pool *pgxpool.Pool, environmentID, targetID string) registryPullArtifactRow {
	t.Helper()
	var value registryPullArtifactRow
	err := pool.QueryRow(t.Context(), `SELECT profile_revision,profile_name,pull_credential_ref,secret_name,created_at,updated_at
		FROM runtime_registry_pull_artifacts WHERE environment_id=$1 AND registry_target_id=$2 AND active`, environmentID, targetID).
		Scan(&value.revision, &value.profile, &value.pullRef, &value.secretName, &value.createdAt, &value.updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	value.createdAt, value.updatedAt = value.createdAt.UTC(), value.updatedAt.UTC()
	return value
}

func assertArtifactCount(t testing.TB, pool *pgxpool.Pool, environmentID, targetID string, expected int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_registry_pull_artifacts
		WHERE environment_id=$1 AND registry_target_id=$2`, environmentID, targetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != expected {
		t.Fatalf("artifact count=%d want=%d", count, expected)
	}
}

func typedPolicyDocument(
	t testing.TB,
	binding gitprojection.Binding,
	document gitprojection.Document,
	raw []byte,
	organizationID, namespace string,
) projectionpolicy.AppConfigPolicyDocument {
	t.Helper()
	parsed, runtime, diagnostics := appconfig.ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("typed policy fixture diagnostics=%#v", diagnostics)
	}
	scope := projectionpolicy.DocumentScope{Binding: binding, OrganizationID: organizationID, Namespace: namespace,
		ApplicationID: document.ApplicationID, Path: document.Path, SourceRevision: document.SourceRevision,
		ConfigRevision: document.ConfigRevision}
	result, err := projectionpolicy.NewAppConfigPolicyDocument(scope, parsed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func registryPullEligible(
	t testing.TB,
	pool *pgxpool.Pool,
	document projectionpolicy.AppConfigPolicyDocument,
	now time.Time,
	maxAge time.Duration,
) (bool, error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		return false, err
	}
	defer tx.Rollback(t.Context()) //nolint:errcheck
	return projectionpolicy.RegistryPullArtifactEligibleTx(t.Context(), tx, document, now, maxAge)
}
