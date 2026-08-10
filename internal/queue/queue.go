package queue

import (
	"context"
	"fmt"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

type Publisher interface {
	Publish(context.Context, domain.WorkMessage) error
}
type Consumer interface {
	Receive(context.Context, string, int) ([]domain.WorkMessage, error)
	Ack(context.Context, domain.WorkMessage) error
}

type datasetPublisher interface {
	DatasetIdentity(context.Context) (string, error)
}

type datasetReplayStore interface {
	ReconcileOutboxDataset(context.Context, string) (int64, error)
}

type Relay struct {
	Store     store.Store
	Publisher Publisher
	Batch     int
}

func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	published, _, err := r.RunOnceWithReplay(ctx)
	return published, err
}

// RunOnceWithReplay also reports how many PostgreSQL outbox rows were reopened
// because the durable Valkey dataset identity changed. This is safe operational
// evidence: it contains only a count, never work payloads or credentials.
func (r *Relay) RunOnceWithReplay(ctx context.Context) (int, int64, error) {
	limit := r.Batch
	if limit <= 0 {
		limit = 100
	}
	var replayed int64
	if publisher, ok := r.Publisher.(datasetPublisher); ok {
		replayStore, supported := r.Store.(datasetReplayStore)
		if !supported {
			return 0, 0, fmt.Errorf("outbox store does not support Valkey dataset replay")
		}
		datasetID, err := publisher.DatasetIdentity(ctx)
		if err != nil {
			return 0, 0, err
		}
		if replayed, err = replayStore.ReconcileOutboxDataset(ctx, datasetID); err != nil {
			return 0, 0, fmt.Errorf("reconcile Valkey dataset: %w", err)
		}
	}
	items, err := r.Store.PendingOutbox(ctx, limit)
	if err != nil {
		return 0, replayed, err
	}
	published := 0
	for _, item := range items {
		if err = r.Publisher.Publish(ctx, item); err != nil {
			_ = r.Store.MarkOutboxFailure(ctx, item.OperationID, err.Error())
			return published, replayed, fmt.Errorf("publish operation %s: %w", item.OperationID, err)
		}
		if err = r.Store.MarkOutboxPublished(ctx, item.OperationID); err != nil {
			return published, replayed, err
		}
		published++
	}
	return published, replayed, nil
}
