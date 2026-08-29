package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) BuildDefinitionIdentity(ctx context.Context, applicationID string) (autodeploy.BuildDefinitionIdentity, error) {
	var identity autodeploy.BuildDefinitionIdentity
	err := s.pool.QueryRow(ctx, `SELECT build_source_id::text,project_id::text,id::text FROM applications WHERE id=$1 AND build_source_id IS NOT NULL`, applicationID).
		Scan(&identity.ID, &identity.ProjectID, &identity.ApplicationID)
	return identity, classify(err)
}

func (s *Store) GetPinnedDeploymentConfig(ctx context.Context, actorID, deploymentID string) (domain.DeploymentConfig, error) {
	return s.GetDeploymentConfigForActor(ctx, actorID, deploymentID)
}

func (s *Store) GetServiceAccount(ctx context.Context, accountID string) (domain.ServiceAccount, error) {
	return getServiceAccount(ctx, s.pool, accountID, false)
}

func (s *Store) PolicyCommandReplay(ctx context.Context, actorID, key, action, requestDigest string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	return autoDeployPolicyReplay(ctx, s.pool, actorID, key, action, requestDigest)
}

func (s *Store) CreatePolicy(ctx context.Context, policy autodeploy.Policy, revision autodeploy.Revision, key, requestDigest, requestID string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	if policy.Validate() != nil || revision.ValidateFor(policy) != nil || revision.Revision != 1 || policy.CurrentRevision != 1 ||
		key == "" || requestDigest == "" || requestID == "" {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, autodeploy.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(policy.CreatedBy, "auto-deploy.policy", key)); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if replayPolicy, replayRevision, found, replayErr := autoDeployPolicyReplay(ctx, tx, policy.CreatedBy, key, "create", requestDigest); replayErr != nil || found {
		return replayPolicy, replayRevision, found, replayErr
	}
	if err = authorizeAutoDeployPolicyMutation(ctx, tx, policy.CreatedBy, policy.ProjectID, policy.EnvironmentID, policy.ApplicationID, revision.ServiceActorID); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO auto_deploy_policies(id,project_id,application_id,environment_id,current_revision,created_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, policy.ID, policy.ProjectID, policy.ApplicationID,
		policy.EnvironmentID, policy.CurrentRevision, policy.CreatedBy, policy.CreatedAt.UTC())
	if err == nil {
		err = insertAutoDeployRevision(ctx, tx, revision)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,action,request_digest,auto_deploy_policy_id,result_revision,request_id,created_at)
			VALUES($1,'auto-deploy-policy','auto-deploy-policy','global',$2,'create',$3,$4,$5,$6,$7)`, policy.CreatedBy, key, requestDigest, policy.ID, revision.Revision, requestID, revision.CreatedAt.UTC())
	}
	if err == nil {
		err = insertAutoDeployAudit(ctx, tx, policy.CreatedBy, "create", revision, requestID)
	}
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, classify(err)
	}
	return policy, revision, false, nil
}

func (s *Store) RevisePolicy(ctx context.Context, prior autodeploy.Policy, revision autodeploy.Revision, key, requestDigest, requestID string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	if prior.Validate() != nil || revision.PolicyID != prior.ID || revision.Revision != prior.CurrentRevision+1 ||
		key == "" || requestDigest == "" || requestID == "" {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, autodeploy.ErrInvalid
	}
	next := prior
	next.CurrentRevision = revision.Revision
	if revision.ValidateFor(next) != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, autodeploy.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(revision.CreatedBy, "auto-deploy.policy", key)); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if replayPolicy, replayRevision, found, replayErr := autoDeployPolicyReplay(ctx, tx, revision.CreatedBy, key, "revise", requestDigest); replayErr != nil || found {
		return replayPolicy, replayRevision, found, replayErr
	}
	current, currentRevision, err := autoDeployPolicyByID(ctx, tx, prior.ID, true)
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if current != prior || currentRevision.Revision != prior.CurrentRevision {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrConflict
	}
	if err = authorizeAutoDeployPolicyMutation(ctx, tx, revision.CreatedBy, prior.ProjectID, prior.EnvironmentID, prior.ApplicationID, revision.ServiceActorID); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if err = insertAutoDeployRevision(ctx, tx, revision); err == nil {
		_, err = tx.Exec(ctx, `UPDATE auto_deploy_policies SET current_revision=$2 WHERE id=$1`, prior.ID, revision.Revision)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,action,request_digest,auto_deploy_policy_id,result_revision,request_id,created_at)
			VALUES($1,'auto-deploy-policy','auto-deploy-policy','global',$2,'revise',$3,$4,$5,$6,$7)`, revision.CreatedBy, key, requestDigest, prior.ID, revision.Revision, requestID, revision.CreatedAt.UTC())
	}
	if err == nil {
		action := "revise"
		if currentRevision.Enabled != revision.Enabled {
			if revision.Enabled {
				action = "enable"
			} else {
				action = "disable"
			}
		}
		err = insertAutoDeployAudit(ctx, tx, revision.CreatedBy, action, revision, requestID)
	}
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, classify(err)
	}
	return next, revision, false, nil
}

func (s *Store) PolicyForActor(ctx context.Context, actorID, policyID string) (autodeploy.PolicyStatus, error) {
	policy, revision, err := autoDeployPolicyByID(ctx, s.pool, policyID, false)
	if err != nil {
		return autodeploy.PolicyStatus{}, err
	}
	if err = authorizeWith(ctx, s.pool, actorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: policy.ApplicationID}); err != nil {
		return autodeploy.PolicyStatus{}, err
	}
	return autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision}, nil
}

func (s *Store) PoliciesForApplication(ctx context.Context, actorID, applicationID string) ([]autodeploy.PolicyStatus, error) {
	if err := authorizeWith(ctx, s.pool, actorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, autoDeployPolicySelect+` WHERE p.application_id=$1 ORDER BY p.created_at,p.id`, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]autodeploy.PolicyStatus, 0)
	for rows.Next() {
		policy, revision, scanErr := scanAutoDeployPolicy(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, autodeploy.PolicyStatus{Policy: policy, CurrentRevision: revision})
	}
	return items, rows.Err()
}

func (s *Store) PolicyRevisionsForActor(ctx context.Context, actorID, policyID string, limit int) ([]autodeploy.Revision, error) {
	status, err := s.PolicyForActor(ctx, actorID, policyID)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, autodeploy.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT policy_id::text,revision,enabled,source_deployment_id::text,source_deployment_generation,
		source_config_etag,config_intent,template_digest,service_actor_id::text,created_by::text,created_at
		FROM auto_deploy_policy_revisions WHERE policy_id=$1 ORDER BY revision DESC LIMIT $2`, status.Policy.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]autodeploy.Revision, 0)
	for rows.Next() {
		var revision autodeploy.Revision
		if err = scanAutoDeployRevision(rows, &revision); err != nil {
			return nil, err
		}
		items = append(items, revision)
	}
	return items, rows.Err()
}

func (s *Store) PolicyRunsForActor(ctx context.Context, actorID, policyID string, limit int) ([]autodeploy.Run, error) {
	status, err := s.PolicyForActor(ctx, actorID, policyID)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, autodeploy.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT attempt_id::text,policy_id::text,policy_revision,definition_id::text,definition_digest,release_id::text,
		template_digest,source_deployment_id::text,source_deployment_generation,source_config_etag,idempotency_key,state,attempts,
		available_at,COALESCE(operation_id::text,''),COALESCE(deployment_id::text,''),failure_code,created_at,updated_at,completed_at
		FROM auto_deploy_runs WHERE policy_id=$1 ORDER BY created_at DESC,attempt_id DESC LIMIT $2`, status.Policy.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]autodeploy.Run, 0)
	for rows.Next() {
		var run autodeploy.Run
		if err = rows.Scan(&run.AttemptID, &run.PolicyID, &run.PolicyRevision, &run.DefinitionID, &run.DefinitionDigest, &run.ReleaseID,
			&run.TemplateDigest, &run.SourceDeploymentID, &run.SourceDeploymentGeneration, &run.SourceConfigETag, &run.IdempotencyKey,
			&run.State, &run.Attempts, &run.AvailableAt, &run.OperationID, &run.DeploymentID, &run.FailureCode, &run.CreatedAt, &run.UpdatedAt, &run.CompletedAt); err != nil {
			return nil, err
		}
		items = append(items, run)
	}
	return items, rows.Err()
}

func (s *Store) AuthorizeAutoDeploy(ctx context.Context, actorID string, scope domain.AutomationScope, projectID, environmentID, applicationID string) error {
	if scope != domain.AutomationScopeAppEdit {
		return autodeploy.ErrUnauthorized
	}
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM service_accounts sa JOIN users u ON u.id=sa.id
		JOIN environments e ON e.project_id=sa.project_id JOIN applications a ON a.project_id=sa.project_id
		WHERE sa.id=$1 AND sa.project_id=$2 AND sa.disabled_at IS NULL AND e.id=$3 AND a.id=$4)`, actorID, projectID, environmentID, applicationID).Scan(&enabled)
	if err != nil {
		return err
	}
	if !enabled {
		return autodeploy.ErrUnauthorized
	}
	if err = s.AuthorizePromotion(ctx, actorID, environmentID, applicationID); err != nil {
		return autodeploy.ErrUnauthorized
	}
	return nil
}

func authorizeAutoDeployPolicyMutation(ctx context.Context, q accessQuerier, actorID, projectID, environmentID, applicationID, serviceActorID string) error {
	if err := authorizeWith(ctx, q, actorID, domain.PermissionBuildsManage, domain.AccessTarget{Type: "application", ID: applicationID}); err != nil {
		return err
	}
	var target domain.AccessTarget
	target.Type, target.ID, target.EnvironmentID, target.ApplicationID = "deployment", environmentID+":"+applicationID, environmentID, applicationID
	err := q.QueryRow(ctx, `SELECT e.namespace,p.id::text,COALESCE(p.team_id::text,'')
		FROM environments e JOIN applications a ON a.project_id=e.project_id
		JOIN projects p ON p.id=e.project_id WHERE e.id=$1 AND a.id=$2 AND p.id=$3`, environmentID, applicationID, projectID).
		Scan(&target.Namespace, &target.ProjectID, &target.TeamID)
	if err != nil {
		return classify(err)
	}
	bindings, err := effectiveBindings(ctx, q, actorID)
	if err != nil {
		return err
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return base.ErrForbidden
	}
	account, err := getServiceAccount(ctx, q, serviceActorID, false)
	if err != nil {
		return err
	}
	if account.ProjectID != projectID || account.DisabledAt != nil {
		return base.ErrNotFound
	}
	projectTarget, err := resolveAccessTarget(ctx, q, domain.AccessTarget{Type: "project", ID: projectID})
	if err != nil {
		return err
	}
	if !accesspolicy.CanManageGrant(bindings, projectTarget, account.Role) {
		return base.ErrForbidden
	}
	return nil
}

func (s *Store) ResolveVerifiedRelease(ctx context.Context, attemptID string) (autodeploy.VerifiedRelease, error) {
	var release autodeploy.VerifiedRelease
	var imageDigest, registryDigest string
	err := s.pool.QueryRow(ctx, `SELECT a.id::text,a.definition_id::text,a.definition_digest,a.project_id::text,a.service_id::text,
		p.release_id::text,a.result->'image'->>'reference',a.result->'image'->>'digest',a.commit_sha,a.completed_at,rr.root_digest
		FROM build_attempts a
		JOIN build_release_projections p ON p.attempt_id=a.id AND p.state='succeeded' AND p.completed_at IS NOT NULL
		JOIN registry_releases rr ON rr.id=p.release_id AND rr.id=a.id
			AND rr.service_id=a.service_id::text AND rr.succeeded_at IS NOT NULL AND rr.availability='present'
		WHERE a.id=$1 AND a.state='succeeded' AND a.completed_at IS NOT NULL`, attemptID).Scan(&release.AttemptID,
		&release.DefinitionID, &release.DefinitionDigest, &release.ProjectID, &release.ApplicationID, &release.ReleaseID,
		&release.Image, &imageDigest, &release.CommitSHA, &release.CompletedAt, &registryDigest)
	if err != nil {
		return autodeploy.VerifiedRelease{}, classify(err)
	}
	if imageDigest != registryDigest || !strings.HasSuffix(release.Image, "@"+imageDigest) || release.Validate() != nil {
		return autodeploy.VerifiedRelease{}, autodeploy.ErrConflict
	}
	return release, nil
}

func insertAutoDeployRevision(ctx context.Context, tx pgx.Tx, revision autodeploy.Revision) error {
	_, err := tx.Exec(ctx, `INSERT INTO auto_deploy_policy_revisions(policy_id,revision,enabled,source_deployment_id,source_deployment_generation,
		source_config_etag,config_intent,template_digest,service_actor_id,created_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, revision.PolicyID, revision.Revision, revision.Enabled,
		revision.Template.SourceDeploymentID, revision.Template.SourceDeploymentGeneration, revision.Template.SourceConfigETag,
		revision.Template.ConfigIntent, revision.TemplateDigest, revision.ServiceActorID, revision.CreatedBy, revision.CreatedAt.UTC())
	return err
}

func insertAutoDeployAudit(ctx context.Context, tx pgx.Tx, actorID, action string, revision autodeploy.Revision, requestID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,action,target_type,target_id,request_id,detail,created_at)
		VALUES($1,$2,$3,'auto-deploy-policy',$4,$5,jsonb_build_object(
			'revision',$6::bigint,'serviceActorId',$7::text,'templateDigest',$8::text),$9)`,
		id.New(), actorID, "auto-deploy-policy."+action, revision.PolicyID, requestID, revision.Revision,
		revision.ServiceActorID, revision.TemplateDigest, revision.CreatedAt.UTC())
	return err
}

func autoDeployPolicyReplay(ctx context.Context, q accessQuerier, actorID, key, action, digest string) (autodeploy.Policy, autodeploy.Revision, bool, error) {
	var storedAction, storedDigest, policyID string
	var revision int64
	err := q.QueryRow(ctx, `SELECT action,request_digest,auto_deploy_policy_id::text,result_revision FROM mutation_receipts
		WHERE actor_id=$1 AND receipt_kind='auto-deploy-policy' AND namespace='auto-deploy-policy' AND scope_key='global' AND idempotency_key=$2`, actorID, key).Scan(&storedAction, &storedDigest, &policyID, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, nil
	}
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	if storedAction != action || storedDigest != digest {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, base.ErrIdempotencyConflict
	}
	policy, _, err := autoDeployPolicyByID(ctx, q, policyID, false)
	if err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	// Idempotent replay is still a read of the accepted policy. Re-check the
	// actor's current application visibility before returning its durable
	// result; otherwise a revoked actor could replay a prior mutation through
	// the revise endpoint without passing the normal policy authorization path.
	if err = authorizeWith(ctx, q, actorID, domain.PermissionBuildsRead, domain.AccessTarget{Type: "application", ID: policy.ApplicationID}); err != nil {
		return autodeploy.Policy{}, autodeploy.Revision{}, false, err
	}
	stored, err := autoDeployRevisionByID(ctx, q, policyID, revision)
	return policy, stored, err == nil, err
}

const autoDeployPolicySelect = `SELECT p.id::text,p.project_id::text,p.application_id::text,p.environment_id::text,
	p.current_revision,p.created_by::text,p.created_at,r.policy_id::text,r.revision,r.enabled,r.source_deployment_id::text,
	r.source_deployment_generation,r.source_config_etag,r.config_intent,r.template_digest,r.service_actor_id::text,r.created_by::text,r.created_at
	FROM auto_deploy_policies p JOIN auto_deploy_policy_revisions r ON r.policy_id=p.id AND r.revision=p.current_revision`

type autoDeployScanner interface{ Scan(...any) error }

func autoDeployPolicyByID(ctx context.Context, q rowQuerier, policyID string, lock bool) (autodeploy.Policy, autodeploy.Revision, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF p"
	}
	return scanAutoDeployPolicy(q.QueryRow(ctx, autoDeployPolicySelect+` WHERE p.id=$1`+suffix, policyID))
}

func scanAutoDeployPolicy(scanner autoDeployScanner) (autodeploy.Policy, autodeploy.Revision, error) {
	var policy autodeploy.Policy
	var revision autodeploy.Revision
	err := scanner.Scan(&policy.ID, &policy.ProjectID, &policy.ApplicationID, &policy.EnvironmentID,
		&policy.CurrentRevision, &policy.CreatedBy, &policy.CreatedAt, &revision.PolicyID, &revision.Revision, &revision.Enabled,
		&revision.Template.SourceDeploymentID, &revision.Template.SourceDeploymentGeneration, &revision.Template.SourceConfigETag,
		&revision.Template.ConfigIntent, &revision.TemplateDigest, &revision.ServiceActorID, &revision.CreatedBy, &revision.CreatedAt)
	return policy, revision, classify(err)
}

func autoDeployRevisionByID(ctx context.Context, q rowQuerier, policyID string, revisionNumber int64) (autodeploy.Revision, error) {
	var revision autodeploy.Revision
	err := scanAutoDeployRevision(q.QueryRow(ctx, `SELECT policy_id::text,revision,enabled,source_deployment_id::text,source_deployment_generation,
		source_config_etag,config_intent,template_digest,service_actor_id::text,created_by::text,created_at
		FROM auto_deploy_policy_revisions WHERE policy_id=$1 AND revision=$2`, policyID, revisionNumber), &revision)
	return revision, classify(err)
}

func scanAutoDeployRevision(scanner autoDeployScanner, revision *autodeploy.Revision) error {
	return scanner.Scan(&revision.PolicyID, &revision.Revision, &revision.Enabled, &revision.Template.SourceDeploymentID,
		&revision.Template.SourceDeploymentGeneration, &revision.Template.SourceConfigETag, &revision.Template.ConfigIntent,
		&revision.TemplateDigest, &revision.ServiceActorID, &revision.CreatedBy, &revision.CreatedAt)
}

var (
	_ autodeploy.PolicyCatalog     = (*Store)(nil)
	_ autodeploy.PolicyStore       = (*Store)(nil)
	_ autodeploy.PolicyReader      = (*Store)(nil)
	_ autodeploy.ServiceAuthorizer = (*Store)(nil)
	_ autodeploy.ReleaseVerifier   = (*Store)(nil)
)
