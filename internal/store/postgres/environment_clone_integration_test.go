package postgres

import (
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPostgreSQLEnvironmentCloneCopiesStoppedDraftConfigurationWithoutPublishing(t *testing.T) {
	if os.Getenv("KUBERPLOY_TEST_DATABASE_URL") == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := t.Context()
	store, err := Open(ctx, os.Getenv("KUBERPLOY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := strings.ReplaceAll(id.New(), "-", "")[:12]
	actorID := id.New()
	now := time.Now().UTC()
	if _, err = store.pool.Exec(ctx, `INSERT INTO users(id,display_name,role,issuer,subject,grant_revision,created_at) VALUES($1,$2,'platform-admin','environment-clone-test',$3,1,$4)`,
		actorID, "clone-admin-"+suffix, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at) VALUES($1,$2,'platform-admin','platform','platform','bootstrap',$2,$3)`,
		id.New(), actorID, now); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, actorID, "clone-project-"+suffix, "clone-project-"+suffix, domain.CreateProject{Name: "Clone project", Slug: "clone-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.CreateEnvironment(ctx, actorID, "clone-source-"+suffix, "clone-source-"+suffix, domain.CreateEnvironment{
		ProjectID: project.Value.ID, Name: "Development", Slug: "development", ProtectionPolicy: domain.EnvironmentDevelopment,
	})
	if err != nil {
		t.Fatal(err)
	}
	applications := make([]domain.Application, 0, 3)
	for index, name := range []string{"API", "Worker"} {
		slug := strings.ToLower(name)
		application, createErr := store.CreateApplication(ctx, actorID, "clone-app-"+slug+"-"+suffix, "clone-app-"+slug+"-"+suffix,
			domain.CreateApplication{ProjectID: project.Value.ID, Name: name, Slug: slug})
		if createErr != nil {
			t.Fatal(createErr)
		}
		applications = append(applications, application.Value)
		_, _, createErr = store.CreateDeployment(ctx, actorID, "clone-deployment-"+slug+"-"+suffix, "clone-deployment-"+slug+"-"+suffix, "request-"+suffix,
			domain.CreateDeployment{EnvironmentID: source.Value.ID, ApplicationID: application.Value.ID,
				Image: "registry.example.test/" + slug + "@sha256:" + strings.Repeat(string(rune('a'+index)), 64), Replicas: 1, Port: 8080}, nil)
		if createErr != nil {
			t.Fatal(createErr)
		}
	}
	draftApplication, err := store.CreateApplication(ctx, actorID, "clone-app-draft-"+suffix, "clone-app-draft-"+suffix,
		domain.CreateApplication{ProjectID: project.Value.ID, EnvironmentID: source.Value.ID, Name: "Draft", Slug: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	applications = append(applications, draftApplication.Value)
	draftPlacements, err := listEnvironmentAppPlacements(ctx, store.pool, source.Value.ID)
	if err != nil || len(draftPlacements) != 3 || draftPlacements[1].ApplicationID != draftApplication.Value.ID ||
		draftPlacements[1].State != domain.EnvironmentAppPlacementDraft || draftPlacements[1].DesiredState != domain.EnvironmentAppPlacementStopped {
		t.Fatalf("atomic draft placement=%#v err=%v", draftPlacements, err)
	}
	// One explicit placement plus one removed legacy placement exercises union
	// compatibility without changing source deployment authority.
	if _, err = store.pool.Exec(ctx, `DELETE FROM environment_app_placements WHERE environment_id=$1 AND application_id=$2`, source.Value.ID, applications[1].ID); err != nil {
		t.Fatal(err)
	}
	legacyCompatible, err := store.ListEnvironmentAppPlacementsForActor(ctx, actorID, source.Value.ID)
	if err != nil || len(legacyCompatible) != len(applications) {
		t.Fatalf("legacy-compatible environment App list=%#v err=%v", legacyCompatible, err)
	}
	legacySeen := false
	for _, placement := range legacyCompatible {
		if placement.ApplicationID == applications[1].ID {
			legacySeen = placement.State == domain.EnvironmentAppPlacementActive && placement.DesiredState == domain.EnvironmentAppPlacementRunning
		}
	}
	if !legacySeen {
		t.Fatalf("legacy deployment App missing from environment list: %#v", legacyCompatible)
	}

	sideEffectTables := []string{
		"outbox", "git_write_commands", "build_attempts",
		"registry_releases", "helm_app_revisions", "secret_bindings", "secret_binding_versions",
		"external_dns_integrations", "runtime_registry_pull_artifacts",
	}
	before := environmentCloneTableCounts(t, store, sideEffectTables)
	var deploymentsBefore, operationsBefore, inputsBefore int
	for table, destination := range map[string]*int{"deployments": &deploymentsBefore, "operations": &operationsBefore, "deployment_operation_inputs": &inputsBefore} {
		if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM public.`+table).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	input := domain.CloneEnvironment{Name: "Production", Slug: "production"}
	cloned, err := store.CloneEnvironment(ctx, actorID, source.Value.ID, "clone-environment-"+suffix, "clone-environment-fingerprint-"+suffix, input)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Replay || cloned.Value.Environment.ProjectID != project.Value.ID || cloned.Value.Environment.ProtectionPolicy != domain.EnvironmentDevelopment || len(cloned.Value.AppPlacements) != len(applications) {
		t.Fatalf("clone=%#v", cloned)
	}
	seen := map[string]bool{}
	for _, placement := range cloned.Value.AppPlacements {
		seen[placement.ApplicationID] = true
		if placement.EnvironmentID != cloned.Value.Environment.ID || placement.ProjectID != project.Value.ID ||
			placement.State != domain.EnvironmentAppPlacementDraft || placement.DesiredState != domain.EnvironmentAppPlacementStopped {
			t.Fatalf("placement=%#v", placement)
		}
	}
	for _, application := range applications {
		if !seen[application.ID] {
			t.Fatalf("shared application identity %s missing", application.ID)
		}
	}
	var clonedDrafts int
	var allStopped, allTargetBound bool
	if err = store.pool.QueryRow(ctx, `SELECT count(*),bool_and(state='stopped'),bool_and(position($2 in convert_from(config_raw,'UTF8'))>0)
		FROM deployments WHERE environment_id=$1`, cloned.Value.Environment.ID, cloned.Value.Environment.ID).Scan(&clonedDrafts, &allStopped, &allTargetBound); err != nil {
		t.Fatal(err)
	}
	if clonedDrafts != 2 || !allStopped || !allTargetBound {
		t.Fatalf("cloned drafts=%d stopped=%v targetBound=%v", clonedDrafts, allStopped, allTargetBound)
	}
	var deploymentsAfter, operationsAfter, inputsAfter int
	for table, destination := range map[string]*int{"deployments": &deploymentsAfter, "operations": &operationsAfter, "deployment_operation_inputs": &inputsAfter} {
		if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM public.`+table).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if deploymentsAfter != deploymentsBefore+2 || operationsAfter != operationsBefore+2 || inputsAfter != inputsBefore+2 {
		t.Fatalf("draft history counts deployments=%d operations=%d inputs=%d", deploymentsAfter-deploymentsBefore, operationsAfter-operationsBefore, inputsAfter-inputsBefore)
	}
	after := environmentCloneTableCounts(t, store, sideEffectTables)
	for table, count := range before {
		if after[table] != count {
			t.Fatalf("clone changed side-effect table %s: before=%d after=%d", table, count, after[table])
		}
	}
	var draftID, draftETag string
	var draftRaw []byte
	if err = store.pool.QueryRow(ctx, `SELECT id,config_etag,config_raw FROM deployments WHERE environment_id=$1 ORDER BY id LIMIT 1`,
		cloned.Value.Environment.ID).Scan(&draftID, &draftETag, &draftRaw); err != nil {
		t.Fatal(err)
	}
	candidate := appconfig.Apply(draftRaw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op: "replace", Path: "/spec/runtime/replicas", Value: 3,
	}}})
	if len(candidate.Diagnostics) != 0 {
		t.Fatalf("draft candidate diagnostics=%#v", candidate.Diagnostics)
	}
	tokenHash := sha256.Sum256([]byte("clone-draft-preview-" + suffix))
	if err = store.CreateDeploymentConfigPreview(ctx, actorID, domain.CreateConfigPreview{DeploymentID: draftID,
		BaseETag: draftETag, TokenHash: tokenHash[:], CandidateHash: candidate.Hash, CandidateRaw: candidate.Raw,
		Runtime: candidate.Runtime, ExpiresAt: time.Now().Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	saved, saveOperation, err := store.SaveDeploymentConfig(ctx, actorID, "clone-draft-save-"+suffix, "clone-draft-save-"+suffix,
		"request-"+suffix, domain.SaveDeploymentConfig{DeploymentID: draftID, BaseETag: draftETag, TokenHash: tokenHash[:],
			CandidateHash: candidate.Hash, RawYAML: candidate.Raw}, nil)
	if err != nil || saved.Value.State != "stopped" || saved.Value.Runtime.Replicas != 3 ||
		saveOperation.Kind != "deployment.config-draft-save" || saveOperation.Status != "succeeded" {
		t.Fatalf("saved draft=%#v operation=%#v err=%v", saved, saveOperation, err)
	}
	afterDraftSave := environmentCloneTableCounts(t, store, sideEffectTables)
	for table, count := range before {
		if afterDraftSave[table] != count {
			t.Fatalf("draft save changed side-effect table %s: before=%d after=%d", table, count, afterDraftSave[table])
		}
	}

	replay, err := store.CloneEnvironment(ctx, actorID, source.Value.ID, "clone-environment-"+suffix, "clone-environment-fingerprint-"+suffix, input)
	if err != nil || !replay.Replay || replay.Value.Environment.ID != cloned.Value.Environment.ID || len(replay.Value.AppPlacements) != len(applications) {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err = store.CloneEnvironment(ctx, actorID, source.Value.ID, "clone-environment-"+suffix, "changed-fingerprint-"+suffix,
		domain.CloneEnvironment{Name: "Different", Slug: "different"}); !errors.Is(err, base.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}

	second, err := store.CloneEnvironment(ctx, actorID, cloned.Value.Environment.ID, "clone-environment-second-"+suffix, "clone-environment-second-fingerprint-"+suffix,
		domain.CloneEnvironment{Name: "Staging", Slug: "staging"})
	if err != nil || len(second.Value.AppPlacements) != len(applications) {
		t.Fatalf("draft source clone=%#v err=%v", second, err)
	}
	var secondDrafts int
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE environment_id=$1 AND state='stopped'`, second.Value.Environment.ID).Scan(&secondDrafts); err != nil || secondDrafts != 2 {
		t.Fatalf("second draft configs=%d err=%v", secondDrafts, err)
	}
	deletedReplay, err := store.DeleteEnvironment(ctx, actorID, second.Value.Environment.ID, second.Value.Environment.Name,
		"delete-cloned-environment-"+suffix, "delete-cloned-environment-"+suffix, "request-"+suffix)
	if err != nil || deletedReplay {
		t.Fatalf("delete cloned Environment replay=%v err=%v", deletedReplay, err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE environment_id=$1`, second.Value.Environment.ID).Scan(&secondDrafts); err != nil || secondDrafts != 0 {
		t.Fatalf("deleted clone retained drafts=%d err=%v", secondDrafts, err)
	}
}

func environmentCloneTableCounts(t *testing.T, store *Store, tables []string) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		// Table names are fixed test constants, never caller input.
		if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.`+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}
