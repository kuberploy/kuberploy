package argo

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/domain"
)

// PostgreSQLObservationTargetResolver resolves one deployment-scoped Argo
// identity from central resources plus its one environment Git binding. It
// fails closed when an environment is unbound or ambiguously bound.
type PostgreSQLObservationTargetResolver struct{ pool *pgxpool.Pool }

func NewPostgreSQLObservationTargetResolver(pool *pgxpool.Pool) (*PostgreSQLObservationTargetResolver, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLObservationTargetResolver{pool: pool}, nil
}

func NewPostgreSQLObservationTargetResolverFromStore(store *PostgreSQLStore) (*PostgreSQLObservationTargetResolver, error) {
	if store == nil || store.pool == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLObservationTargetResolver{pool: store.pool}, nil
}

func (r *PostgreSQLObservationTargetResolver) ResolveArgoObservationTarget(ctx context.Context, deploymentID string) (ObservationTarget, error) {
	if r == nil || r.pool == nil || !uuidRE.MatchString(deploymentID) {
		return ObservationTarget{}, ErrInvalid
	}
	rows, err := r.pool.Query(ctx, `SELECT d.id::text,a.id::text,p.id::text,e.id::text,p.name,p.slug,e.slug,e.namespace,e.argo_project,d.desired_revision
		FROM deployments d
		JOIN applications a ON a.id=d.application_id
		JOIN environments e ON e.id=d.environment_id
		JOIN projects p ON p.id=a.project_id AND p.id=e.project_id
		JOIN git_repository_bindings b ON b.kind='environment' AND b.project_id=p.id AND b.environment_id=e.id
		WHERE d.id=$1 AND d.desired_revision<>'' AND b.target_head_revision IS NOT NULL AND b.state<>'missing-ref'
		ORDER BY b.id`, deploymentID)
	if err != nil {
		return ObservationTarget{}, classifyPostgres(err)
	}
	defer rows.Close()
	targets := make([]ObservationTarget, 0, 2)
	for rows.Next() {
		var target ObservationTarget
		var project domain.Project
		var environmentSlug string
		if err = rows.Scan(&target.DeploymentID, &target.ApplicationID, &target.ProjectID, &target.EnvironmentID, &project.Name, &project.Slug,
			&environmentSlug, &target.DestinationNamespace, &target.ArgoProject, &target.DesiredRevision); err != nil {
			return ObservationTarget{}, classifyPostgres(err)
		}
		project.ID = target.ProjectID
		namespace, argoProject := domain.DeriveEnvironmentDestination(project, environmentSlug)
		if namespace != target.DestinationNamespace || argoProject != target.ArgoProject || target.validate() != nil {
			return ObservationTarget{}, ErrInvalid
		}
		targets = append(targets, target)
		if len(targets) > 1 {
			return ObservationTarget{}, ErrConflict
		}
	}
	if err = rows.Err(); err != nil {
		return ObservationTarget{}, classifyPostgres(err)
	}
	if len(targets) == 0 {
		return ObservationTarget{}, ErrNotFound
	}
	return targets[0], nil
}

var _ ObservationTargetResolver = (*PostgreSQLObservationTargetResolver)(nil)
