package environmentfoundation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

const RuntimeReadinessLease = 90 * time.Second

type EnvironmentCatalog interface {
	EnvironmentIDs(context.Context) ([]string, error)
}

type Runtime struct {
	Store       Store
	Catalog     EnvironmentCatalog
	Controller  *Controller
	Deletions   *DeletionController
	Config      RuntimeConfig
	WorkerEpoch int64
	StartedAt   time.Time
	Now         func() time.Time
	ReportError func(error)
}

func (r *Runtime) now() time.Time { return r.Now().UTC() }

func (r *Runtime) Validate() error {
	if r == nil || r.Store == nil || r.Catalog == nil || r.Controller == nil || r.Controller.Store != r.Store ||
		r.Config.Validate() != nil || !r.Config.Enabled || r.Controller.Profile != r.Config.Profile ||
		r.Controller.Publisher.Identity() != r.Config.Publisher || r.Controller.WorkerEpoch != r.WorkerEpoch ||
		r.WorkerEpoch < 1 || r.StartedAt.IsZero() || r.Now == nil {
		return ErrInvalid
	}
	return nil
}

func (r *Runtime) Run(ctx context.Context) error {
	if ctx == nil || r.Validate() != nil {
		return ErrInvalid
	}
	if r.StartedAt.After(r.now()) {
		return ErrInvalid
	}
	ticker := time.NewTicker(r.Config.PollInterval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil {
			if !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrConflict) {
				return err
			}
			if r.ReportError != nil {
				r.ReportError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runtime) RunOnce(ctx context.Context) error {
	if ctx == nil || r.Validate() != nil {
		return ErrInvalid
	}
	ids, err := exactEnvironmentIDs(ctx, r.Catalog)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	now := r.now()
	if r.Deletions != nil {
		if _, deletionErr := r.Deletions.Reconcile(ctx); deletionErr != nil && !errors.Is(deletionErr, ErrUnavailable) {
			return deletionErr
		}
	}
	profileDigest, err := r.Config.Profile.Digest()
	if err != nil {
		return err
	}
	for _, environmentID := range ids {
		intentID := deterministicIntentID(environmentID, profileDigest, r.Config.Profile.PublisherConfigDigest)
		if err = r.ensureCurrentIntent(ctx, intentID, environmentID, now); err != nil {
			// A newly enabled runtime can legitimately observe environments before
			// the exact platform Git binding has reached its first ready revision.
			// Keep the worker alive and retry; readiness remains unavailable.
			if errors.Is(err, ErrNotFound) {
				return ErrUnavailable
			}
			return err
		}
	}
	if _, err = r.Controller.Reconcile(ctx); err != nil && !errors.Is(err, ErrUnavailable) {
		return err
	}
	if err = r.Store.RecordReadiness(ctx, Readiness{WorkerID: r.Controller.WorkerID, WorkerEpoch: r.WorkerEpoch,
		Contract: Contract, ProfileDigest: profileDigest, PublisherConfigDigest: r.Config.Publisher.ConfigDigest,
		ActiveIntentCount: len(ids), StartedAt: r.StartedAt.UTC(), ObservedAt: now, LeaseUntil: now.Add(RuntimeReadinessLease)}); err != nil {
		return err
	}
	return r.Store.ExactReady(ctx, profileDigest, r.Config.Publisher.ConfigDigest, len(ids), now)
}

func (r *Runtime) ensureCurrentIntent(ctx context.Context, intentID, environmentID string, now time.Time) error {
	for depth := 0; depth < MaximumAttempts; depth++ {
		intent, err := r.Store.EnsureIntent(ctx, EnsureRequest{IntentID: intentID, EnvironmentID: environmentID, Profile: r.Config.Profile, Now: now})
		if err != nil {
			return err
		}
		if intent.State != StateFailed || (intent.LastFailureCode != "protected-git-rejected" &&
			intent.LastFailureCode != "protected-git-rebase" && intent.LastFailureCode != "protected-git-unavailable") {
			return nil
		}
		intentID = deterministicRecoveryIntentID(intent.ID)
	}
	return ErrConflict
}

type RuntimeReadinessProbe struct {
	Store   Store
	Catalog EnvironmentCatalog
	Config  RuntimeConfig
	Now     func() time.Time
}

func (p *RuntimeReadinessProbe) Probe(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Catalog == nil || p.Config.Validate() != nil || !p.Config.Enabled || ctx == nil {
		return ErrUnavailable
	}
	ids, err := exactEnvironmentIDs(ctx, p.Catalog)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	profileDigest, err := p.Config.Profile.Digest()
	if err != nil {
		return ErrUnavailable
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	return p.Store.ExactReady(ctx, profileDigest, p.Config.Publisher.ConfigDigest, len(ids), now)
}

func exactEnvironmentIDs(ctx context.Context, catalog EnvironmentCatalog) ([]string, error) {
	ids, err := catalog.EnvironmentIDs(ctx)
	if err != nil || len(ids) > 10_000 {
		return nil, ErrUnavailable
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	for index, id := range ids {
		if !uuidRE.MatchString(id) || index > 0 && id == ids[index-1] {
			return nil, ErrConflict
		}
	}
	return ids, nil
}

func deterministicIntentID(environmentID, profileDigest, publisherDigest string) string {
	sum := sha256.Sum256([]byte(Contract + "\x00" + environmentID + "\x00" + profileDigest + "\x00" + publisherDigest))
	return uuidFromDigest(sum)
}

func deterministicRecoveryIntentID(failedIntentID string) string {
	sum := sha256.Sum256([]byte(Contract + "\x00recovery\x00" + failedIntentID))
	return uuidFromDigest(sum)
}

func uuidFromDigest(sum [sha256.Size]byte) string {
	value := append([]byte(nil), sum[:16]...)
	value[6] = value[6]&0x0f | 0x50
	value[8] = value[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:]
}
