package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/queue"
)

func TestOutboxRelayOnceRequiresDatabaseAuthorityBeforeNetworkUse(t *testing.T) {
	t.Setenv("KUBERPLOY_DATABASE_URL", "")
	var output bytes.Buffer
	if err := runOutboxRelayOnce(t.Context(), &output); err == nil || output.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}

func TestOutboxPublisherOpeningUsesStartupRetryBoundary(t *testing.T) {
	options := queue.ValkeyOptions{Addresses: []string{"valkey.example.test:6379"}, ClientName: "test-relay"}
	wantErr := errors.New("temporary Valkey refusal")
	openCalls := 0
	retryCalled := false

	_, err := openOutboxPublisherWith(t.Context(), options,
		func(got queue.ValkeyOptions) (*queue.ValkeyStream, error) {
			openCalls++
			if len(got.Addresses) != 1 || got.Addresses[0] != options.Addresses[0] || got.ClientName != options.ClientName {
				t.Fatalf("options=%#v", got)
			}
			return nil, wantErr
		},
		func(ctx context.Context, open func() (*queue.ValkeyStream, error)) (*queue.ValkeyStream, error) {
			retryCalled = true
			_, openErr := open()
			if !errors.Is(openErr, wantErr) {
				t.Fatalf("open error=%v", openErr)
			}
			return nil, openErr
		})
	if !errors.Is(err, wantErr) || !retryCalled || openCalls != 1 {
		t.Fatalf("err=%v retryCalled=%t openCalls=%d", err, retryCalled, openCalls)
	}
}
