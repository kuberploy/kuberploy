package argo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	type candidate struct {
		bindingID         string
		projectID         string
		environmentID     string
		previousCommandID string
	}
	var selected candidate
	err = m.pool.QueryRow(ctx, `SELECT b.id::text,b.project_id::text,b.environment_id::text,COALESCE(latest.id::text,'')
	FROM git_repository_bindings b
	JOIN git_projection_generations generation
	  ON generation.binding_id=b.id AND generation.generation=b.projection_generation
	LEFT JOIN LATERAL (
		SELECT command.id,command.state,command.environment_revision,command.environment_generation
		FROM argo_desired_state_commands command
		WHERE command.environment_id=b.environment_id
		ORDER BY command.generation DESC LIMIT 1
	) latest ON true
	WHERE b.kind='environment' AND b.credential_mode='github-app' AND b.credential_secret_name=''
	  AND b.state='ready' AND b.target_head_revision=b.indexed_revision AND b.indexed_revision IS NOT NULL
	  AND b.projection_generation>0 AND generation.state='active'
	  AND generation.head_revision=b.indexed_revision AND generation.parser_version=b.parser_version
	  AND NOT EXISTS(SELECT 1 FROM argo_desired_state_commands live
	    WHERE live.environment_id=b.environment_id AND live.state IN ('pending','claimed','git-committed'))
	  AND (latest.id IS NULL OR (latest.state='verified' AND
	    (latest.environment_revision<>b.indexed_revision OR latest.environment_generation<>b.projection_generation)))
	ORDER BY b.indexed_at,b.id
	LIMIT 1`).Scan(&selected.bindingID, &selected.projectID, &selected.environmentID, &selected.previousCommandID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classifyPostgres(err)
	}
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
	if selected.previousCommandID != "" {
		value, readErr := m.store.DesiredStateCommand(ctx, selected.previousCommandID)
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
	if err != nil {
		return false, err
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
