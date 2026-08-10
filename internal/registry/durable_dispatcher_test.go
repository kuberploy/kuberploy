package registry

import (
	"context"
	"errors"
	"testing"
)

func TestDurableCleanupDispatcherAcceptsOnlyCommittedWorkIdentity(t *testing.T) {
	dispatcher := DurableCleanupDispatcher{}
	if err := dispatcher.Execute(context.Background(), "11111111-1111-4111-8111-111111111111", "api-0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Execute(context.Background(), "../../other", "api-0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrRegistryCleanupUnavailable) {
		t.Fatalf("invalid plan dispatch err=%v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dispatcher.Execute(canceled, "11111111-1111-4111-8111-111111111111", "api-0123456789abcdef0123456789abcdef"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dispatch err=%v", err)
	}
}
