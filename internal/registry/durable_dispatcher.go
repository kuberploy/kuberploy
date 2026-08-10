package registry

import (
	"context"
	"regexp"
)

var registryDispatchOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// DurableCleanupDispatcher acknowledges work only after Management has
// committed the execution idempotency record. RuntimeController polls that
// durable record, so the API never needs registry credentials or direct
// access to the destructive executor.
type DurableCleanupDispatcher struct{}

func (DurableCleanupDispatcher) Execute(ctx context.Context, planID, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !registryUUIDRE.MatchString(planID) || !registryDispatchOwnerRE.MatchString(owner) {
		return ErrRegistryCleanupUnavailable
	}
	return nil
}
