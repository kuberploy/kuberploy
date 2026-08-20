package store

import (
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// ExternalDNSStore keeps every profile mutation actor-bound, idempotent and
// audited. Implementations must authorize the exact catalog scope on reads.
type ExternalDNSStore interface {
	ListExternalDNSIntegrationsForActor(context.Context, string) ([]domain.ExternalDNSIntegration, error)
	CreateExternalDNSIntegrationForActor(context.Context, string, string, string, string, domain.ExternalDNSIntegration) (Result[domain.ExternalDNSIntegration], error)
	UpdateExternalDNSIntegrationForActor(context.Context, string, string, string, string, domain.ExternalDNSIntegration) (Result[domain.ExternalDNSIntegration], error)
	DeactivateExternalDNSIntegrationForActor(context.Context, string, string, string, string, string) (Result[domain.ExternalDNSIntegration], error)
	ExternalDNSIntegrationsForEnvironmentActor(context.Context, string, string) ([]domain.ExternalDNSIntegration, error)
	ExternalDNSIntegrationsForApplicationActor(context.Context, string, string, string) ([]domain.ExternalDNSIntegration, error)
	ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error)
	AdvanceExternalDNSRuntimeRevision(context.Context, string, int64, string, time.Time) error
	RecordExternalDNSPublication(context.Context, string, int64, bool, string, string, time.Time) error
}
