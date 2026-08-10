package argo

import (
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

// PostgreSQLProductionComponents keeps the desired-state planner, claim gate,
// catalog, readiness store, and materializer on one migrated PostgreSQL pool.
// The command package receives only the closed interfaces; it cannot bypass
// the serializable policy transactions with a second ad-hoc connection path.
type PostgreSQLProductionComponents struct {
	Catalog             *PostgreSQLRuntimeBindingCatalog
	RegistryEligibility *PostgreSQLRegistryEligibilityResolver
	ProjectionGate      *PostgreSQLDesiredStateProjectionGate
	Materializer        *PostgreSQLDesiredStateMaterializer
}

func NewPostgreSQLProductionComponents(
	store *PostgreSQLStore,
	bindings DesiredStateBindingStore,
	policy gitprojection.PostgreSQLAppConfigPolicyValidator,
	policyDigest string,
	maximumRegistryArtifactAge time.Duration,
	identity DesiredStateRuntimeIdentity,
) (*PostgreSQLProductionComponents, error) {
	if store == nil || store.pool == nil || bindings == nil || policy == nil || identity.Validate() != nil {
		return nil, ErrInvalid
	}
	catalog, err := NewPostgreSQLRuntimeBindingCatalog(store.pool)
	if err != nil {
		return nil, err
	}
	registryEligibility, err := NewPostgreSQLRegistryEligibilityResolver(store.pool, maximumRegistryArtifactAge)
	if err != nil {
		return nil, err
	}
	gate, err := NewPostgreSQLDesiredStateProjectionGate(store.pool, policy, registryEligibility, policyDigest)
	if err != nil {
		return nil, err
	}
	materializer, err := NewPostgreSQLDesiredStateMaterializer(store.pool, store, bindings, gate, registryEligibility, identity)
	if err != nil {
		return nil, err
	}
	return &PostgreSQLProductionComponents{
		Catalog: catalog, RegistryEligibility: registryEligibility, ProjectionGate: gate, Materializer: materializer,
	}, nil
}
