package registry

import (
	"context"
	"errors"
	"testing"
)

func TestProbeDistributionCredentialSourceUsesExactTargetAndErasesAuthorization(t *testing.T) {
	secret := []byte("probe-secret-marker")
	called := ""
	source := distributionCredentialSourceFunc(func(_ context.Context, targetID string) (DistributionAuthorization, error) {
		called = targetID
		return DistributionAuthorization{scheme: "Bearer", value: secret}, nil
	})
	if err := ProbeDistributionCredentialSource(context.Background(), "managed-target", source); err != nil {
		t.Fatal(err)
	}
	if called != "managed-target" {
		t.Fatalf("credential target=%q", called)
	}
	for _, value := range secret {
		if value != 0 {
			t.Fatal("probed authorization was not erased")
		}
	}
	if err := ProbeDistributionCredentialSource(context.Background(), "managed-target", distributionCredentialSourceFunc(func(context.Context, string) (DistributionAuthorization, error) {
		return DistributionAuthorization{}, errors.New("unavailable")
	})); !errors.Is(err, ErrDistributionCredentialUnavailable) {
		t.Fatalf("unavailable credential probe err=%v", err)
	}
}
