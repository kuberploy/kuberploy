package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	base "github.com/kuberploy/kuberploy/internal/store"
)

var outboxDatasetIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (s *Store) CreateDeployment(ctx context.Context, actor, key, fingerprint, requestID string, in domain.CreateDeployment, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) (base.Result[domain.Deployment], domain.Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, advisoryIdentity(actor, "deployments.create", key)); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if old, ok, err := findIdem(ctx, tx, actor, "deployments.create", key); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	} else if ok {
		if old.fingerprint != fingerprint {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrIdempotencyConflict
		}
		d, err := getDeployment(ctx, tx, old.resourceID)
		if err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
		if err = authorizeWith(ctx, tx, actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
		opID := d.OperationID
		if old.operationID != nil {
			opID = *old.operationID
		}
		op, err := getOperation(ctx, tx, opID)
		return base.Result[domain.Deployment]{Value: d, Replay: true}, op, err
	}
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if base.AppConfigUsesRuntimeSecrets(in.Runtime) && referencePlan == nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	var sameProject bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN applications a ON a.project_id=e.project_id WHERE e.id=$1 AND a.id=$2)`, in.EnvironmentID, in.ApplicationID).Scan(&sameProject)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if !sameProject {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	var projectID string
	if err = tx.QueryRow(ctx, `SELECT project_id FROM environments WHERE id=$1`, in.EnvironmentID).Scan(&projectID); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	target, err := resolveAccessTarget(ctx, tx, domain.AccessTarget{Type: "environment", ID: in.EnvironmentID})
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	target.Type, target.ApplicationID = "deployment", in.ApplicationID
	bindings, err := effectiveBindings(ctx, tx, actor)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrForbidden
	}
	var projectionBinding gitprojection.Binding
	if projection != nil {
		if projection.ProjectID != projectID || projection.EnvironmentID != in.EnvironmentID || projection.ApplicationID != in.ApplicationID {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
		if projectionBinding, err = validateGitProjectionPlanTx(ctx, tx, projection); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.EnvironmentID+":"+in.ApplicationID); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	now := time.Now().UTC()
	dID, opID := id.New(), id.New()
	generation := int64(1)
	configVersion := int64(1)
	existing := false
	err = tx.QueryRow(ctx, `SELECT id,generation,config_version FROM deployments WHERE environment_id=$1 AND application_id=$2 FOR UPDATE`, in.EnvironmentID, in.ApplicationID).Scan(&dID, &generation, &configVersion)
	if err == nil {
		existing = true
		generation++
		configVersion++
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if existing {
		_, err = tx.Exec(ctx, `UPDATE operations SET status='superseded',updated_at=$2,finished_at=$2,problem=jsonb_build_object('code','Superseded','detail','A newer deployment release was accepted.') WHERE target_type='deployment' AND target_id=$1 AND status='queued'`, dID, now)
		if err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "pending"}})
	op := domain.Operation{ID: opID, Kind: "deployment.git-write", Status: "queued", TargetType: "deployment", TargetID: dID, RequestID: requestID, Generation: generation, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,kind,status,target_type,target_id,request_id,generation,progress,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, op.ID, op.Kind, op.Status, op.TargetType, op.TargetID, op.RequestID, op.Generation, progress, now)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	runtime := domain.RuntimeForCreateDeployment(in)
	replicas, port, ordinary := domain.LegacyWorkloadFields(runtime)
	envJSON, _ := json.Marshal(ordinary)
	runtimeJSON, _ := json.Marshal(runtime)
	var routeJSON []byte
	if in.Route != nil {
		routeJSON, _ = json.Marshal(in.Route)
	}
	d := domain.Deployment{ID: dID, EnvironmentID: in.EnvironmentID, ApplicationID: in.ApplicationID, Image: in.Image, Replicas: replicas, Port: port, Environment: cloneMap(ordinary), Route: in.Route, Runtime: runtime, RegistryPull: in.RegistryPull, State: "pending-git", OperationID: opID, Generation: generation, CreatedAt: now, UpdatedAt: now}
	project, err := getProject(ctx, tx, projectID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	environment, err := getEnvironment(ctx, tx, in.EnvironmentID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	application, err := getApplication(ctx, tx, in.ApplicationID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if err = validateSchedulingRuntimeTx(ctx, tx, project.ID, environment.ID, application.ID, runtime); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	d.ConfigRaw, err = gitops.RenderAppConfig(project, environment, application, d)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	rawHash := sha256.Sum256(d.ConfigRaw)
	exactParsed, exactRuntime, _, err := appConfigMaterialFromExactAppConfig(d.ConfigRaw, rawHash[:])
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	middlewareRefs, refsErr := middlewareprofiles.AppConfigSecretReferences(exactParsed)
	if refsErr != nil || (base.AppConfigUsesRuntimeSecrets(exactRuntime) || len(middlewareRefs) != 0) && referencePlan == nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	if projection != nil {
		resolution, resolutionErr := resolveProjectedVariablesTx(ctx, tx, projectionBinding, exactRuntime)
		if resolutionErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, resolutionErr
		}
		exactRuntime = resolution.Runtime
	}
	d.Runtime = exactRuntime
	d.Replicas, d.Port, d.Environment = domain.LegacyWorkloadFields(d.Runtime)
	envJSON, _ = json.Marshal(d.Environment)
	runtimeJSON, _ = json.Marshal(d.Runtime)
	d.ConfigVersion = configVersion
	configETag := domain.DeploymentConfigETag(d.ID, configVersion, d.ConfigRaw)
	if existing {
		_, err = tx.Exec(ctx, `UPDATE deployments SET image=$2,replicas=$3,port=$4,environment=$5,route=$6,runtime=$7,state='pending-git',operation_id=$8,generation=$9,config_raw=$10,config_etag=$11,config_version=$12,updated_at=$13 WHERE id=$1`, d.ID, d.Image, d.Replicas, d.Port, envJSON, routeJSON, runtimeJSON, d.OperationID, generation, d.ConfigRaw, configETag, configVersion, now)
		var created time.Time
		if scanErr := tx.QueryRow(ctx, `SELECT created_at FROM deployments WHERE id=$1`, d.ID).Scan(&created); scanErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, scanErr
		}
		d.CreatedAt = created
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO deployments(id,environment_id,application_id,image,replicas,port,environment,route,runtime,state,operation_id,generation,config_raw,config_etag,config_version,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)`, d.ID, d.EnvironmentID, d.ApplicationID, d.Image, d.Replicas, d.Port, envJSON, routeJSON, runtimeJSON, d.State, d.OperationID, generation, d.ConfigRaw, configETag, configVersion, now)
	}
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	if err = insertGitWriteCommandTx(ctx, tx, actor, op.ID, d.ID, projection, d.ConfigRaw, "deploy("+in.ApplicationID+"): accept immutable release", now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	// API acceptance validates the immutable candidate and its authorization,
	// but it is not proof that Git contains the candidate. Keep the currently
	// indexed AppConfig's deletion guards until exact projection activation
	// reconciles them in the same transaction as the new active generation.
	if projection != nil && (base.AppConfigUsesRuntimeSecrets(d.Runtime) || len(middlewareRefs) != 0) {
		if _, err = validateRuntimeSecretReferencesTx(ctx, tx, actor, referencePlan, projectID, in.EnvironmentID,
			in.ApplicationID, d.Runtime, middlewareRefs); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO deployment_operation_inputs(operation_id,deployment_id,image,replicas,port,environment,route,runtime,config_raw,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, op.ID, d.ID, d.Image, d.Replicas, d.Port, envJSON, routeJSON, runtimeJSON, d.ConfigRaw, now)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(operation_id,kind,scope_id,generation,trace_id) VALUES($1,$2,$3,$4,$5)`, op.ID, op.Kind, in.EnvironmentID, op.Generation, requestID)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	action := "deployment.create.accepted"
	if existing {
		action = "deployment.release.accepted"
	}
	if err = audit(ctx, tx, actor, action, "deployment", d.ID, requestID, map[string]any{"operationId": op.ID, "image": d.Image, "generation": generation}); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if err = putIdem(ctx, tx, actor, "deployments.create", key, fingerprint, "deployment", d.ID, &opID); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	return base.Result[domain.Deployment]{Value: d}, op, nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func getDeployment(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.Deployment, error) {
	var d domain.Deployment
	var envJSON, routeJSON, runtimeJSON []byte
	err := q.QueryRow(ctx, `SELECT id,environment_id,application_id,image,replicas,port,environment,route,runtime,state,operation_id,generation,desired_revision,observed_revision,created_at,updated_at FROM deployments WHERE id=$1`, id).Scan(&d.ID, &d.EnvironmentID, &d.ApplicationID, &d.Image, &d.Replicas, &d.Port, &envJSON, &routeJSON, &runtimeJSON, &d.State, &d.OperationID, &d.Generation, &d.DesiredRevision, &d.ObservedRevision, &d.CreatedAt, &d.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(envJSON, &d.Environment)
	}
	if err == nil && len(routeJSON) > 0 {
		d.Route = &domain.Route{}
		err = json.Unmarshal(routeJSON, d.Route)
	}
	if err == nil {
		err = json.Unmarshal(runtimeJSON, &d.Runtime)
	}
	return d, classify(err)
}

func (s *Store) GetDeploymentForOperation(ctx context.Context, operationID string) (domain.Deployment, error) {
	var d domain.Deployment
	var envJSON, routeJSON, runtimeJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT d.id,d.environment_id,d.application_id,i.image,i.replicas,i.port,i.environment,i.route,i.runtime,i.config_raw,d.state,i.operation_id,o.generation,d.desired_revision,d.observed_revision,d.created_at,o.created_at FROM deployment_operation_inputs i JOIN deployments d ON d.id=i.deployment_id JOIN operations o ON o.id=i.operation_id WHERE i.operation_id=$1`, operationID).Scan(&d.ID, &d.EnvironmentID, &d.ApplicationID, &d.Image, &d.Replicas, &d.Port, &envJSON, &routeJSON, &runtimeJSON, &d.ConfigRaw, &d.State, &d.OperationID, &d.Generation, &d.DesiredRevision, &d.ObservedRevision, &d.CreatedAt, &d.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(envJSON, &d.Environment)
	}
	if err == nil && len(routeJSON) > 0 {
		d.Route = &domain.Route{}
		err = json.Unmarshal(routeJSON, d.Route)
	}
	if err == nil {
		err = json.Unmarshal(runtimeJSON, &d.Runtime)
	}
	return d, classify(err)
}
func (s *Store) GetDeployment(ctx context.Context, id string) (domain.Deployment, error) {
	return getDeployment(ctx, s.pool, id)
}
func (s *Store) ListDeployments(ctx context.Context) ([]domain.Deployment, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM deployments ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.Deployment, 0, len(ids))
	for _, v := range ids {
		d, e := s.GetDeployment(ctx, v)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, nil
}

func scanOperation(row pgx.Row) (domain.Operation, error) {
	var op domain.Operation
	var progress, problem []byte
	err := row.Scan(&op.ID, &op.Kind, &op.Status, &op.TargetType, &op.TargetID, &op.RequestID, &op.Generation, &progress, &op.GitRevision, &problem, &op.CreatedAt, &op.UpdatedAt, &op.FinishedAt)
	if err != nil {
		return op, classify(err)
	}
	if len(progress) > 0 {
		if err = json.Unmarshal(progress, &op.Progress); err != nil {
			return op, err
		}
	}
	if len(problem) > 0 {
		op.Problem = &domain.ProblemData{}
		if err = json.Unmarshal(problem, op.Problem); err != nil {
			return op, err
		}
	}
	return op, nil
}

const operationSelect = `SELECT id,kind,status,target_type,target_id,request_id,generation,progress,git_revision,problem,created_at,updated_at,finished_at FROM operations WHERE id=$1`

func getOperation(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (domain.Operation, error) {
	op, err := scanOperation(q.QueryRow(ctx, operationSelect, id))
	if err != nil {
		return domain.Operation{}, err
	}
	publication, publicationErr := scanGitPublication(q.QueryRow(ctx, `SELECT `+gitPublicationColumns+` FROM git_pull_request_publications WHERE operation_id=$1 AND pull_request_number>0`, id))
	if publicationErr == nil {
		op.PullRequest = &domain.PullRequestPublication{Number: publication.PullRequestNumber, URL: publication.PullRequestURL,
			State: string(publication.PullRequestState), CandidateRevision: publication.CandidateRevision}
	} else if !errors.Is(publicationErr, gitpublication.ErrNotFound) {
		return domain.Operation{}, publicationErr
	}
	return op, nil
}
func (s *Store) GetOperation(ctx context.Context, id string) (domain.Operation, error) {
	return getOperation(ctx, s.pool, id)
}
func (s *Store) ListOperations(ctx context.Context) ([]domain.Operation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM operations ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	out := make([]domain.Operation, 0, len(ids))
	for _, v := range ids {
		op, e := s.GetOperation(ctx, v)
		if e != nil {
			return nil, e
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) DeploymentStatus(ctx context.Context, id string) (domain.DeploymentStatus, error) {
	return deploymentStatus(ctx, s.pool, id)
}

const deploymentStatusQuery = `SELECT d.id,d.state,d.operation_id,o.status,d.desired_revision,d.observed_revision,
	COALESCE(ao.sync_status,'unknown'),COALESCE(ao.health_status,'unknown'),COALESCE(ao.observed_revision,''),ao.observed_at
	FROM deployments d JOIN operations o ON o.id=d.operation_id
	JOIN environments e ON e.id=d.environment_id
	JOIN applications a ON a.id=d.application_id AND a.project_id=e.project_id
	LEFT JOIN argo_application_observations ao ON ao.deployment_id=d.id
		AND ao.application_id=d.application_id AND ao.environment_id=d.environment_id
		AND ao.project_id=e.project_id AND ao.destination_namespace=e.namespace
		AND d.desired_revision<>'' AND ao.desired_revision=d.desired_revision
		AND NOT (ao.sync_status='synced' AND ao.health_status='healthy' AND ao.observed_revision<>ao.desired_revision)
		AND ao.observed_at BETWEEN now()-interval '20 minutes' AND now()+interval '30 seconds'
	WHERE d.id=$1`

func deploymentStatus(ctx context.Context, query rowQuerier, id string) (domain.DeploymentStatus, error) {
	var out domain.DeploymentStatus
	err := query.QueryRow(ctx, deploymentStatusQuery, id).Scan(&out.DeploymentID, &out.State, &out.OperationID, &out.OperationStatus,
		&out.DesiredRevision, &out.ObservedRevision, &out.ArgoSyncStatus, &out.RolloutHealth, &out.ArgoObservedRevision, &out.ArgoObservedAt)
	return out, classify(err)
}

func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]domain.WorkMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT operation_id,kind,scope_id,generation,trace_id FROM outbox WHERE published_at IS NULL AND available_at<=now() ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorkMessage
	for rows.Next() {
		var w domain.WorkMessage
		if err = rows.Scan(&w.OperationID, &w.Kind, &w.ScopeID, &w.Generation, &w.TraceID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ReconcileOutboxDataset reconstructs only non-terminal durable work when the
// Valkey dataset identity changes. The transaction serializes all worker
// replicas, records the new dataset first-use, and makes previously published
// rows eligible for ordinary outbox delivery again. Terminal work is never
// replayed and PostgreSQL operation generation/lease checks remain the effect
// idempotency boundary.
func (s *Store) ReconcileOutboxDataset(ctx context.Context, datasetID string) (int64, error) {
	if !outboxDatasetIDPattern.MatchString(datasetID) {
		return 0, errors.New("Valkey dataset identity is invalid")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kuberploy-outbox-valkey-dataset'))`); err != nil {
		return 0, err
	}
	var observed string
	err = tx.QueryRow(ctx, `SELECT dataset_id::text FROM outbox_valkey_dataset WHERE singleton=true FOR UPDATE`).Scan(&observed)
	if err == nil && observed == datasetID {
		return 0, tx.Commit(ctx)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	command, err := tx.Exec(ctx, `UPDATE outbox AS event
		SET published_at=NULL,available_at=now(),last_error=NULL
		FROM operations AS operation
		WHERE event.operation_id=operation.id AND event.published_at IS NOT NULL
		AND operation.status IN ('queued','running')`)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_valkey_dataset(singleton,dataset_id,observed_at)
		VALUES(true,$1,now()) ON CONFLICT(singleton) DO UPDATE
		SET dataset_id=EXCLUDED.dataset_id,observed_at=EXCLUDED.observed_at`, datasetID); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}
func (s *Store) MarkOutboxPublished(ctx context.Context, operationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE outbox SET published_at=now(),last_error=NULL WHERE operation_id=$1`, operationID)
	return err
}
func (s *Store) MarkOutboxFailure(ctx context.Context, operationID, msg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE outbox SET attempts=attempts+1,last_error=$2,available_at=now()+make_interval(secs=>LEAST(60,1+attempts*2)) WHERE operation_id=$1`, operationID, msg)
	return err
}

func (s *Store) LeasePendingOperations(ctx context.Context, worker string, limit int, lease time.Duration) ([]domain.WorkMessage, error) {
	rows, err := s.pool.Query(ctx, `WITH candidates AS (SELECT o.id FROM operations o WHERE o.status='queued' OR (o.status='running' AND (o.lease_until IS NULL OR o.lease_until<now())) ORDER BY o.created_at FOR UPDATE SKIP LOCKED LIMIT $1), leased AS (UPDATE operations o SET lease_owner=$2,lease_until=now()+make_interval(secs=>$3),updated_at=now() FROM candidates c WHERE o.id=c.id RETURNING o.id,o.kind,o.target_id,o.generation,o.request_id) SELECT l.id,l.kind,COALESCE(ob.scope_id,l.target_id),l.generation,l.request_id FROM leased l LEFT JOIN outbox ob ON ob.operation_id=l.id`, limit, worker, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorkMessage
	for rows.Next() {
		var w domain.WorkMessage
		if err = rows.Scan(&w.OperationID, &w.Kind, &w.ScopeID, &w.Generation, &w.TraceID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) StartOperation(ctx context.Context, operationID string, generation int64, worker string, lease time.Duration) (domain.Operation, bool, error) {
	if worker == "" || lease <= 0 {
		return domain.Operation{}, false, base.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Operation{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status, owner, kind, targetID string
	var gen int64
	var leaseUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT status,generation,COALESCE(lease_owner,''),lease_until,kind,target_id FROM operations WHERE id=$1 FOR UPDATE`, operationID).Scan(&status, &gen, &owner, &leaseUntil, &kind, &targetID)
	if err != nil {
		return domain.Operation{}, false, classify(err)
	}
	if gen != generation || status == "succeeded" || status == "failed" || status == "cancelled" || status == "superseded" {
		op, e := getOperation(ctx, tx, operationID)
		return op, false, e
	}
	now := time.Now().UTC()
	if owner != "" && owner != worker && leaseUntil != nil && leaseUntil.After(now) {
		op, e := getOperation(ctx, tx, operationID)
		return op, false, e
	}
	if kind == "deployment.git-write" {
		var lowerRunning, currentGeneration bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE target_type='deployment' AND target_id=$1 AND generation<$2 AND status='running'), EXISTS(SELECT 1 FROM deployments WHERE id=$1 AND generation>$2)`, targetID, gen).Scan(&lowerRunning, &currentGeneration)
		if err != nil {
			return domain.Operation{}, false, err
		}
		if lowerRunning {
			_, err = tx.Exec(ctx, `UPDATE operations SET lease_owner=NULL,lease_until=NULL WHERE id=$1`, operationID)
			if err != nil {
				return domain.Operation{}, false, err
			}
			if err = tx.Commit(ctx); err != nil {
				return domain.Operation{}, false, err
			}
			op, e := s.GetOperation(ctx, operationID)
			return op, false, e
		}
		if currentGeneration {
			_, err = tx.Exec(ctx, `UPDATE operations SET status='superseded',lease_owner=NULL,lease_until=NULL,problem=jsonb_build_object('code','Superseded','detail','A newer deployment release is current.'),updated_at=$2,finished_at=$2 WHERE id=$1`, operationID, now)
			if err != nil {
				return domain.Operation{}, false, err
			}
			if err = tx.Commit(ctx); err != nil {
				return domain.Operation{}, false, err
			}
			op, e := s.GetOperation(ctx, operationID)
			return op, false, e
		}
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "running", StartedAt: &now}})
	_, err = tx.Exec(ctx, `UPDATE operations SET status='running',progress=$2,lease_owner=$3,lease_until=now()+make_interval(secs=>$4),updated_at=now() WHERE id=$1`, operationID, progress, worker, lease.Seconds())
	if err != nil {
		return domain.Operation{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Operation{}, false, err
	}
	op, err := s.GetOperation(ctx, operationID)
	return op, true, err
}

func (s *Store) HeartbeatOperation(ctx context.Context, operationID string, generation int64, worker string, lease time.Duration) error {
	if worker == "" || lease <= 0 {
		return base.ErrConflict
	}
	tag, err := s.pool.Exec(ctx, `UPDATE operations SET lease_until=now()+make_interval(secs=>$4),updated_at=now()
		WHERE id=$1 AND generation=$2 AND status='running' AND lease_owner=$3 AND lease_until>now()`, operationID, generation, worker, lease.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var status string
	var gen int64
	if err = s.pool.QueryRow(ctx, `SELECT status,generation FROM operations WHERE id=$1`, operationID).Scan(&status, &gen); err != nil {
		return classify(err)
	}
	if gen != generation {
		return fmt.Errorf("%w: operation generation changed", base.ErrConflict)
	}
	if terminalOperationStatus(status) {
		return nil
	}
	return base.ErrOperationLeaseLost
}

func (s *Store) RequeueOperation(ctx context.Context, operationID string, generation int64, worker, code, detail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now := time.Now().UTC()
	var kind, status, owner string
	var gen int64
	var leaseUntil *time.Time
	if err = tx.QueryRow(ctx, `SELECT kind,status,generation,COALESCE(lease_owner,''),lease_until FROM operations WHERE id=$1 FOR UPDATE`, operationID).Scan(&kind, &status, &gen, &owner, &leaseUntil); err != nil {
		return classify(err)
	}
	if gen != generation || (kind != "deployment.git-write" && kind != "variable-set.git-write") {
		return fmt.Errorf("%w: operation generation or kind changed", base.ErrConflict)
	}
	if status == "succeeded" || status == "queued" {
		return nil
	}
	if !validOperationLease(status, owner, leaseUntil, worker, now) {
		return base.ErrOperationLeaseLost
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "pending", Detail: detail}})
	if _, err = tx.Exec(ctx, `UPDATE operations SET status='queued',problem=NULL,progress=$2,lease_owner=NULL,lease_until=NULL,updated_at=$3,finished_at=NULL WHERE id=$1`, operationID, progress, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func terminalOperationStatus(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled" || status == "superseded"
}

func validOperationLease(status, owner string, leaseUntil *time.Time, worker string, now time.Time) bool {
	return status == "running" && worker != "" && owner == worker && leaseUntil != nil && leaseUntil.After(now)
}

func (s *Store) CompleteGitOperation(ctx context.Context, operationID string, generation int64, worker string, result domain.GitPublicationResult) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var targetID, targetType, actorID, requestID, owner, currentRevision, kind, publicationMode string
	var status string
	var gen int64
	var leaseUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT o.target_id,o.target_type,o.status,o.generation,o.request_id,o.kind,COALESCE(o.lease_owner,''),o.lease_until,o.git_revision,
		COALESCE(c.publication_mode,'direct') FROM operations o
		LEFT JOIN git_write_commands c ON c.operation_id=o.id
		WHERE o.id=$1 FOR UPDATE OF o`, operationID).Scan(&targetID, &targetType, &status, &gen, &requestID, &kind, &owner, &leaseUntil, &currentRevision, &publicationMode)
	if err != nil {
		return classify(err)
	}
	if status == "succeeded" {
		op, readErr := getOperation(ctx, tx, operationID)
		if readErr == nil && postgresPublicationResultMatchesOperation(result, op) {
			return nil
		}
		return fmt.Errorf("%w: operation succeeded with a different Git revision", base.ErrConflict)
	}
	if gen != generation || (kind != "deployment.git-write" && kind != "variable-set.git-write") {
		return fmt.Errorf("%w: operation generation changed", base.ErrConflict)
	}
	now := time.Now().UTC()
	if !validOperationLease(status, owner, leaseUntil, worker, now) {
		return base.ErrOperationLeaseLost
	}
	detail, deploymentState, auditAction := "committed as "+result.Revision, "git-committed", "deployment.git-committed"
	if kind == "variable-set.git-write" {
		auditAction = "variable-set.git-committed"
	}
	if publicationMode == string(gitpublication.ModeDirect) {
		if !validDirectPublicationResult(result) {
			return fmt.Errorf("%w: direct Git publication receipt does not match", base.ErrConflict)
		}
	} else if publicationMode == string(gitpublication.ModePullRequest) {
		publication, readErr := scanGitPublication(tx.QueryRow(ctx, `SELECT `+gitPublicationColumns+` FROM git_pull_request_publications WHERE operation_id=$1 FOR UPDATE`, operationID))
		if readErr != nil || !postgresPublicationResultMatchesReceipt(result, publication) {
			return fmt.Errorf("%w: protected Git publication receipt does not match", base.ErrConflict)
		}
		detail, deploymentState = "pull request created", postgresProtectedDeploymentState(publication)
		if kind == "deployment.git-write" {
			auditAction = "deployment.pull-request-created"
		} else {
			auditAction = "variable-set.pull-request-created"
		}
	} else {
		return fmt.Errorf("%w: unknown Git publication mode", base.ErrConflict)
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "succeeded", FinishedAt: &now, Detail: detail}})
	_, err = tx.Exec(ctx, `UPDATE operations SET status='succeeded',progress=$2,git_revision=$3,lease_owner=NULL,lease_until=NULL,updated_at=$4,finished_at=$4 WHERE id=$1`, operationID, progress, result.Revision, now)
	if err != nil {
		return err
	}
	if kind == "deployment.git-write" {
		if publicationMode == string(gitpublication.ModeDirect) {
			_, err = tx.Exec(ctx, `UPDATE deployments SET state=$2,desired_revision=$3,updated_at=$4 WHERE id=$1 AND generation=$5`, targetID, deploymentState, result.Revision, now, generation)
		} else {
			_, err = tx.Exec(ctx, `UPDATE deployments SET state=$2,updated_at=$3 WHERE id=$1 AND generation=$4`, targetID, deploymentState, now, generation)
		}
		if err != nil {
			return err
		}
	}
	err = tx.QueryRow(ctx, `SELECT actor_id FROM mutation_receipts WHERE receipt_kind='resource' AND operation_id=$1 LIMIT 1`, operationID).Scan(&actorID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if actorID != "" {
		metadata := map[string]string{"operationId": operationID}
		if result.Revision != "" {
			metadata["gitRevision"] = result.Revision
		}
		if err = audit(ctx, tx, actorID, auditAction, targetType, targetID, requestID, metadata); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func validDirectPublicationResult(result domain.GitPublicationResult) bool {
	return result.Mode == string(gitpublication.ModeDirect) && result.Revision != "" && result.CandidateRevision == "" &&
		result.PullRequestNumber == 0 && result.PullRequestURL == "" && result.PullRequestState == ""
}

func postgresPublicationResultMatchesReceipt(result domain.GitPublicationResult, publication gitpublication.Publication) bool {
	return result.Mode == string(gitpublication.ModePullRequest) && result.Revision == "" && publication.Validate() == nil &&
		publication.PullRequestNumber > 0 && result.CandidateRevision == publication.CandidateRevision &&
		result.PullRequestNumber == publication.PullRequestNumber && result.PullRequestURL == publication.PullRequestURL &&
		result.PullRequestState == string(publication.PullRequestState)
}

func postgresProtectedDeploymentState(publication gitpublication.Publication) string {
	if publication.State == gitpublication.StateMergePending || publication.State == gitpublication.StateMergeVerified {
		return "merge-pending-index"
	}
	if publication.PullRequestState == gitpublication.PullRequestClosed {
		return "review-closed"
	}
	return "review-pending"
}

func postgresPublicationResultMatchesOperation(result domain.GitPublicationResult, operation domain.Operation) bool {
	if operation.PullRequest == nil {
		return validDirectPublicationResult(result) && operation.GitRevision == result.Revision
	}
	return result.Mode == string(gitpublication.ModePullRequest) && result.Revision == "" && operation.GitRevision == "" &&
		result.CandidateRevision == operation.PullRequest.CandidateRevision && result.PullRequestNumber == operation.PullRequest.Number &&
		result.PullRequestURL == operation.PullRequest.URL && result.PullRequestState == operation.PullRequest.State
}

func (s *Store) FailOperation(ctx context.Context, operationID string, generation int64, worker, code, detail string) error {
	problem, _ := json.Marshal(domain.ProblemData{Code: code, Detail: detail})
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status, owner string
	var gen int64
	var leaseUntil *time.Time
	var currentProblem []byte
	if err = tx.QueryRow(ctx, `SELECT status,generation,COALESCE(lease_owner,''),lease_until,problem FROM operations WHERE id=$1 FOR UPDATE`, operationID).Scan(&status, &gen, &owner, &leaseUntil, &currentProblem); err != nil {
		return classify(err)
	}
	if gen != generation {
		return fmt.Errorf("%w: operation generation changed", base.ErrConflict)
	}
	if status == "succeeded" {
		return nil
	}
	if status == "failed" {
		var existing domain.ProblemData
		if json.Unmarshal(currentProblem, &existing) == nil && existing.Code == code && existing.Detail == detail {
			return nil
		}
		return fmt.Errorf("%w: operation already failed differently", base.ErrConflict)
	}
	if !validOperationLease(status, owner, leaseUntil, worker, now) {
		return base.ErrOperationLeaseLost
	}
	progress, _ := json.Marshal([]domain.ProgressStep{{Name: "git-write", Status: "failed", Detail: detail, FinishedAt: &now}})
	tag, err := tx.Exec(ctx, `UPDATE operations SET status='failed',problem=$3,progress=$4,lease_owner=NULL,lease_until=NULL,updated_at=$5,finished_at=$5 WHERE id=$1 AND generation=$2 AND status='running' AND lease_owner=$6`, operationID, generation, problem, progress, now, worker)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: operation generation changed", base.ErrConflict)
	}
	return tx.Commit(ctx)
}
