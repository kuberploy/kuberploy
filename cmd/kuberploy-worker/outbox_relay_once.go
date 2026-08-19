package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/queue"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
	"github.com/kuberploy/kuberploy/internal/valkeystartup"
)

// runOutboxRelayOnce is a bounded recovery/qualification command. It uses only
// the worker's existing PostgreSQL and publisher identities, publishes the
// current bounded outbox batch, and never consumes or executes work.
func runOutboxRelayOnce(ctx context.Context, output io.Writer) error {
	databaseURL, err := config.Required("KUBERPLOY_DATABASE_URL")
	if err != nil {
		return err
	}
	database, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	publisher, err := openOutboxPublisher(ctx)
	if err != nil {
		return err
	}
	defer publisher.Close()
	published, replayed, err := (&queue.Relay{Store: database, Publisher: publisher, Batch: 100}).RunOnceWithReplay(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Published int   `json:"published"`
		Replayed  int64 `json:"replayed"`
	}{Published: published, Replayed: replayed})
}

type outboxPublisherOpen func(queue.ValkeyOptions) (*queue.ValkeyStream, error)
type outboxPublisherRetry func(context.Context, func() (*queue.ValkeyStream, error)) (*queue.ValkeyStream, error)

func outboxPublisherOptions() queue.ValkeyOptions {
	return queue.ValkeyOptions{
		Addresses:  config.List("KUBERPLOY_VALKEY_ADDRESSES", "127.0.0.1:6379"),
		Username:   config.Get("KUBERPLOY_VALKEY_PUBLISHER_USERNAME", os.Getenv("KUBERPLOY_VALKEY_USERNAME")),
		Password:   valkeyCredential("KUBERPLOY_VALKEY_PUBLISHER_PASSWORD", os.Getenv("KUBERPLOY_VALKEY_PASSWORD")),
		ClientName: "kuberploy-outbox-relay-once",
	}
}

func openOutboxPublisher(ctx context.Context) (*queue.ValkeyStream, error) {
	return openOutboxPublisherWith(ctx, outboxPublisherOptions(), queue.NewValkeyStream,
		func(retryContext context.Context, open func() (*queue.ValkeyStream, error)) (*queue.ValkeyStream, error) {
			return valkeystartup.Open(retryContext, open)
		})
}

func openOutboxPublisherWith(ctx context.Context, options queue.ValkeyOptions, open outboxPublisherOpen, retry outboxPublisherRetry) (*queue.ValkeyStream, error) {
	return retry(ctx, func() (*queue.ValkeyStream, error) {
		return open(options)
	})
}
