package argo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

type PostgreSQLDesiredStateMaterializer struct {
	pool     *pgxpool.Pool
	store    DesiredStateStore
	bindings DesiredStateBindingStore
	planner  DesiredStatePlanner
	identity DesiredStateRuntimeIdentity
	newID    func() string
}

type desiredStateMaterializationCandidate struct {
	bindingID                 string
	projectID                 string
	environmentID             string
	latestCommandID           string
	previousVerifiedCommandID string
	preconditionCommandID     string
}

var errDesiredStateCandidateBlocked = errors.New("desired-state candidate is blocked by current tenant policy")

func NewPostgreSQLDesiredStateMaterializer(
	pool *pgxpool.Pool,
	store DesiredStateStore,
	bindings DesiredStateBindingStore,
	gate *PostgreSQLDesiredStateProjectionGate,
	registryEligibility DesiredStateRegistryEligibilityResolver,
	identity DesiredStateRuntimeIdentity,
) (*PostgreSQLDesiredStateMaterializer, error) {
	if pool == nil || store == nil || bindings == nil || gate == nil || registryEligibility == nil || identity.Validate() != nil ||
		gate.registryEligibility == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLDesiredStateMaterializer{pool: pool, store: store, bindings: bindings,
		planner: DesiredStatePlanner{Projection: gate, RegistryEligibility: registryEligibility}, identity: identity, newID: id.New}, nil
}

func (m *PostgreSQLDesiredStateMaterializer) MaterializeDesiredStateOnce(ctx context.Context, now time.Time) (bool, error) {
	if m == nil || m.pool == nil || m.store == nil || m.bindings == nil || m.planner.Projection == nil ||
		m.planner.RegistryEligibility == nil || m.identity.Validate() != nil || m.newID == nil || now.IsZero() {
		return false, ErrInvalid
	}
	platform, err := m.bindings.Binding(ctx, m.identity.PlatformBindingID)
	if err != nil {
		return false, err
	}
	if platform.Validate() != nil || platform.Kind != gitprojection.BindingPlatform || platform.ID != m.identity.PlatformBindingID ||
		platform.ClusterID != m.identity.ClusterID || platform.CredentialMode != gitprojection.CredentialGitHubApp ||
		platform.TargetHeadRevision == "" || (platform.State != gitprojection.BindingReady && platform.State != gitprojection.BindingIndexing) {
		return false, ErrArgoRuntimePrerequisiteNotReady
	}
	rows, err := m.pool.Query(ctx, `SELECT b.id::text,b.project_id::text,b.environment_id::text,
	COALESCE(latest.id::text,''),COALESCE(previous_verified.id::text,''),COALESCE(precondition_command.id::text,'')
	FROM git_repository_bindings b
	JOIN environments e ON e.id=b.environment_id AND e.project_id=b.project_id
	JOIN git_projection_generations generation
	  ON generation.binding_id=b.id AND generation.generation=b.projection_generation
	LEFT JOIN LATERAL (
		SELECT command.id,command.state,command.environment_revision,command.environment_generation,
		       command.chart_repository,command.chart_name,command.chart_version,command.chart_digest,
		       command.renderer_image,command.chart_digest_enforcement,command.argo_project
		FROM argo_desired_state_commands command
		WHERE command.environment_id=b.environment_id
		ORDER BY command.generation DESC LIMIT 1
	) latest ON true
	LEFT JOIN LATERAL (
		SELECT command.id
		FROM argo_desired_state_commands command
		WHERE command.environment_id=b.environment_id AND command.state='verified'
		ORDER BY command.generation DESC LIMIT 1
	) previous_verified ON true
	LEFT JOIN LATERAL (
		SELECT command.id
		FROM argo_desired_state_commands command
		WHERE command.environment_id=b.environment_id AND command.state='verified'
		ORDER BY command.generation DESC LIMIT 1
	) precondition_command ON true
	WHERE b.kind='environment' AND b.credential_mode='github-app' AND b.credential_secret_name=''
	  AND b.state='ready' AND b.target_head_revision=b.indexed_revision AND b.indexed_revision IS NOT NULL
	  AND b.projection_generation>0 AND generation.state='active'
	  AND generation.head_revision=b.indexed_revision AND generation.parser_version=b.parser_version
	  AND EXISTS(SELECT 1 FROM git_projected_documents document
	    WHERE document.binding_id=b.id AND document.generation=b.projection_generation
	      AND document.application_id IS NOT NULL)
	  AND NOT EXISTS(SELECT 1 FROM argo_desired_state_commands live
	    WHERE live.environment_id=b.environment_id AND live.state IN ('pending','claimed','git-committed'))
	  AND (latest.id IS NULL OR
	    (latest.state='verified' AND
	      (latest.environment_revision<>b.indexed_revision OR latest.environment_generation<>b.projection_generation OR
	       latest.chart_repository<>$1 OR latest.chart_name<>$2 OR latest.chart_version<>$3 OR
	       latest.chart_digest<>$4 OR latest.renderer_image<>$5 OR latest.chart_digest_enforcement<>$6 OR
	       latest.argo_project<>e.argo_project)) OR
	    (latest.state IN ('failed','superseded') AND
	      (latest.environment_revision<>b.indexed_revision OR latest.environment_generation<>b.projection_generation OR
	       latest.chart_repository<>$1 OR latest.chart_name<>$2 OR latest.chart_version<>$3 OR
	       latest.chart_digest<>$4 OR latest.renderer_image<>$5 OR latest.chart_digest_enforcement<>$6 OR
	       latest.argo_project<>e.argo_project)))
	ORDER BY b.indexed_at,b.id
	LIMIT 64`, m.identity.Runtime.ChartRepository, m.identity.Runtime.ChartName, m.identity.Runtime.ChartVersion,
		m.identity.Runtime.ChartDigest, m.identity.Runtime.RendererImage, m.identity.DigestEnforcement)
	if err != nil {
		return false, classifyPostgres(err)
	}
	defer rows.Close()
	candidates := make([]desiredStateMaterializationCandidate, 0, 64)
	for rows.Next() {
		var selected desiredStateMaterializationCandidate
		if err = rows.Scan(&selected.bindingID, &selected.projectID, &selected.environmentID, &selected.latestCommandID,
			&selected.previousVerifiedCommandID, &selected.preconditionCommandID); err != nil {
			return false, classifyPostgres(err)
		}
		candidates = append(candidates, selected)
	}
	if err = rows.Err(); err != nil {
		return false, classifyPostgres(err)
	}
	rows.Close()
	return materializeFirstEligibleCandidate(candidates, func(selected desiredStateMaterializationCandidate) (bool, error) {
		return m.materializeCandidate(ctx, platform, selected, now.UTC())
	})
}

func materializeFirstEligibleCandidate(
	candidates []desiredStateMaterializationCandidate,
	materialize func(desiredStateMaterializationCandidate) (bool, error),
) (bool, error) {
	if materialize == nil {
		return false, ErrInvalid
	}
	for _, selected := range candidates {
		created, err := materialize(selected)
		if errors.Is(err, errDesiredStateCandidateBlocked) {
			continue
		}
		if err != nil || created {
			return created, err
		}
	}
	return false, nil
}

func (m *PostgreSQLDesiredStateMaterializer) materializeCandidate(
	ctx context.Context,
	platform gitprojection.Binding,
	selected desiredStateMaterializationCandidate,
	now time.Time,
) (bool, error) {
	binding, err := m.bindings.Binding(ctx, selected.bindingID)
	if err != nil {
		return false, err
	}
	var project domain.Project
	var environment domain.Environment
	var teamID *string
	err = m.pool.QueryRow(ctx, `SELECT p.id::text,p.name,p.slug,p.team_id::text,p.created_at,
		e.id::text,e.project_id::text,e.name,e.slug,e.namespace,e.argo_project,e.created_at
	FROM projects p JOIN environments e ON e.project_id=p.id
	WHERE p.id=$1 AND e.id=$2`, selected.projectID, selected.environmentID).Scan(
		&project.ID, &project.Name, &project.Slug, &teamID, &project.CreatedAt,
		&environment.ID, &environment.ProjectID, &environment.Name, &environment.Slug,
		&environment.Namespace, &environment.ArgoProject, &environment.CreatedAt)
	if err != nil {
		return false, classifyPostgres(err)
	}
	if teamID != nil {
		project.TeamID = *teamID
	}
	target := DesiredStateTarget{Environment: EnvironmentTarget{Project: project, Environment: environment, Binding: binding,
		ArgoNamespace: m.identity.ArgoNamespace, Runtime: m.identity.Runtime}, PlatformBinding: platform}
	if target.Validate() != nil || binding.CredentialMode != gitprojection.CredentialGitHubApp {
		return false, ErrInvalid
	}
	var previous *DesiredStateCommand
	if selected.previousVerifiedCommandID != "" {
		value, readErr := m.store.DesiredStateCommand(ctx, selected.previousVerifiedCommandID)
		if readErr != nil {
			return false, readErr
		}
		if value.State != DesiredStateVerified {
			return false, ErrConflict
		}
		previous = &value
	}
	commandID := m.newID()
	if !uuidRE.MatchString(commandID) {
		return false, ErrInvalid
	}
	command, err := m.planner.Plan(ctx, commandID, target, previous, now.UTC())
	if errors.Is(err, ErrNoDesiredStateChange) {
		return false, nil
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrRegistryReferencesNotReady) {
		return false, errDesiredStateCandidateBlocked
	}
	if err != nil {
		return false, err
	}
	if selected.preconditionCommandID != "" && selected.preconditionCommandID != selected.previousVerifiedCommandID {
		precondition, readErr := m.store.DesiredStateCommand(ctx, selected.preconditionCommandID)
		if readErr != nil {
			return false, readErr
		}
		if precondition.ProjectID != command.ProjectID || precondition.EnvironmentID != command.EnvironmentID ||
			precondition.PlatformBindingID != command.PlatformBindingID || precondition.Path != command.Path ||
			(precondition.State != DesiredStateFailed && precondition.State != DesiredStateSuperseded) ||
			precondition.WriteBaseRevision == "" {
			return false, ErrInvalid
		}
		if precondition.ContentSHA256 == command.ContentSHA256 {
			return false, nil
		}
		command.Precondition = gitprojection.MutationMatchETag
		command.ExpectedETag = `"` + precondition.ContentSHA256 + `"`
		if command.ValidateFor(target) != nil {
			return false, ErrInvalid
		}
	}
	if selected.latestCommandID != "" {
		latest, readErr := m.store.DesiredStateCommand(ctx, selected.latestCommandID)
		if readErr != nil {
			return false, readErr
		}
		if latest.EnvironmentID != command.EnvironmentID || latest.ProjectID != command.ProjectID || latest.Generation < 1 ||
			latest.Generation == int64(^uint64(0)>>1) {
			return false, ErrInvalid
		}
		if command.Generation <= latest.Generation {
			command.Generation = latest.Generation + 1
			command.Message = fmt.Sprintf("Reconcile Argo desired state for environment %s generation %d", command.EnvironmentID, command.Generation)
			if command.ValidateFor(target) != nil {
				return false, ErrInvalid
			}
		}
	}
	created, err := m.store.CreateDesiredState(ctx, command)
	if errors.Is(err, ErrConflict) {
		// Another planner won the unique environment live-command boundary.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return created, nil
}

var _ ProductionDesiredStateMaterializer = (*PostgreSQLDesiredStateMaterializer)(nil)
