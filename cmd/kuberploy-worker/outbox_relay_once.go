package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/queue"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
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
	addresses := config.List("KUBERPLOY_VALKEY_ADDRESSES", "127.0.0.1:6379")
	defaultUsername := os.Getenv("KUBERPLOY_VALKEY_USERNAME")
	defaultPassword := os.Getenv("KUBERPLOY_VALKEY_PASSWORD")
	publisher, err := queue.NewValkeyStream(queue.ValkeyOptions{
		Addresses:  addresses,
		Username:   config.Get("KUBERPLOY_VALKEY_PUBLISHER_USERNAME", defaultUsername),
		Password:   valkeyCredential("KUBERPLOY_VALKEY_PUBLISHER_PASSWORD", defaultPassword),
		ClientName: "kuberploy-outbox-relay-once",
	})
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
