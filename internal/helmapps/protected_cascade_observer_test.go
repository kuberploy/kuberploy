package helmapps

import (
	"errors"
	"testing"
	"time"
)

func TestCascadeObservationAcceptsProviderReceiptAfterLeaseClaim(t *testing.T) {
	claimAt := time.Date(2026, time.August, 21, 17, 40, 0, 0, time.UTC)
	providerAt := claimAt.Add(2 * time.Second)
	readAt := providerAt.Add(time.Second)

	got, err := cascadeObservationUpperBound(func() time.Time { return readAt }, providerAt)
	if err != nil || !got.Equal(readAt) {
		t.Fatalf("upper bound=%s err=%v", got, err)
	}
	if _, err = cascadeObservationUpperBound(func() time.Time { return claimAt }, providerAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale pre-provider bound was accepted: %v", err)
	}
}
