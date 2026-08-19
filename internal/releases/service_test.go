package releases

import (
	"context"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type blockingReleaseCache struct{}

func (blockingReleaseCache) Load(ctx context.Context) (Snapshot, bool, error) {
	<-ctx.Done()
	return Snapshot{}, false, ctx.Err()
}

func (blockingReleaseCache) Store(ctx context.Context, _ Snapshot, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

type releaseServiceChecker struct{}

func (releaseServiceChecker) Latest(context.Context, string) (FetchResult, error) {
	return FetchResult{Release: domain.ReleaseInfo{Version: "1.1.0"}}, nil
}

func TestServiceBoundsUnresponsiveReleaseCache(t *testing.T) {
	service := NewService(releaseServiceChecker{}, blockingReleaseCache{}, time.Minute)
	started := time.Now()
	if _, err := service.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*releaseCacheIOTimeout {
		t.Fatalf("release service exceeded cache outage budget: %s", elapsed)
	}
}
