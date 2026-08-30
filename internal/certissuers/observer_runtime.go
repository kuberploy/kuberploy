package certissuers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

type ObserverStore interface {
	List(context.Context, int) ([]Entry, error)
	RecordObservation(context.Context, Observation) error
	Observation(context.Context, string, int64) (Observation, error)
}

type ObserverRuntime struct {
	Store          ObserverStore
	ReadinessStore ObserverReadinessStore
	Reader         ClusterIssuerReader
	Config         ObserverConfig
	Identity       ObserverRuntimeIdentity
	WorkerID       string
	StartedAt      time.Time
	Now            func() time.Time

	mu    sync.Mutex
	lease ObserverReadinessLease
}

func (r *ObserverRuntime) Validate() error {
	if r == nil || r.Store == nil || r.ReadinessStore == nil || r.Reader == nil || r.Config.Validate() != nil || !r.Config.Enabled ||
		r.Identity.Validate() != nil || !idemRE.MatchString(r.WorkerID) || r.StartedAt.IsZero() || r.StartedAt.Location() != time.UTC || r.Now == nil {
		return ErrObservationUnavailable
	}
	expected, err := ObserverIdentityForConfig(r.Config)
	if err != nil || !observerIdentityEqual(expected, r.Identity) {
		return ErrObservationUnavailable
	}
	return nil
}

func (r *ObserverRuntime) Run(ctx context.Context) error {
	if ctx == nil || r.Validate() != nil {
		return ErrObservationUnavailable
	}
	ticker := time.NewTicker(r.Config.PollInterval)
	defer ticker.Stop()
	for {
		_ = r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce enumerates only the bounded platform catalog. Every active catalog
// name must be in the immutable runtime allowlist; Kubernetes is then contacted
// with one exact named GET per active entry.
func (r *ObserverRuntime) RunOnce(ctx context.Context) error {
	return r.runOnce(ctx, false)
}

// RefreshPreviouslyReadyOnce keeps already-proven issuer observations fresh
// while protected Git publication retries an unrelated ref conflict. It never
// promotes a pending, revised, or previously degraded issuer: those revisions
// still require a successful publication cycle before normal observation.
func (r *ObserverRuntime) RefreshPreviouslyReadyOnce(ctx context.Context) error {
	return r.runOnce(ctx, true)
}

func (r *ObserverRuntime) runOnce(ctx context.Context, requirePreviouslyReady bool) error {
	if ctx == nil || r.Validate() != nil {
		return ErrObservationUnavailable
	}
	entries, targetDigest, err := observerTargets(ctx, r.Store, r.Config)
	if err != nil {
		return err
	}
	now := r.Now()
	if now.IsZero() || now.Location() != time.UTC || now.Before(r.StartedAt) {
		return ErrObservationUnavailable
	}
	if requirePreviouslyReady {
		for _, entry := range entries {
			observation, observationErr := r.Store.Observation(ctx, entry.Profile.ID, entry.Revision.Revision)
			if observationErr != nil || observation.State != Ready || observation.ObservedSpecDigest != entry.Revision.SpecDigest ||
				observation.ObservedGeneration < 1 || observation.ObservedAt == nil || observation.ObservedAt.Location() != time.UTC ||
				observation.ObservedAt.After(now) || observation.UpdatedAt.IsZero() || observation.UpdatedAt.Location() != time.UTC ||
				observation.UpdatedAt.Before(*observation.ObservedAt) || observation.UpdatedAt.After(now) {
				return ErrObservationUnavailable
			}
		}
	}
	var failures []error
	for _, entry := range entries {
		callContext, cancel := context.WithTimeout(ctx, r.Config.RequestTimeout)
		snapshot, observeErr := r.Reader.ClusterIssuer(callContext, entry.Profile.Name)
		cancel()
		if observeErr != nil {
			failures = append(failures, ErrObservationUnavailable)
			continue
		}
		observation, exact := observationFromSnapshot(entry, snapshot, now)
		if recordErr := r.Store.RecordObservation(ctx, observation); recordErr != nil {
			failures = append(failures, recordErr)
			continue
		}
		if !exact {
			failures = append(failures, ErrObservationUnavailable)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	return r.recordReadiness(ctx, ObserverWorkerObservation{WorkerID: r.WorkerID, Identity: r.Identity, TargetDigest: targetDigest,
		TargetCount: len(entries), StartedAt: r.StartedAt, ObservedAt: now})
}

func observationFromSnapshot(entry Entry, snapshot ClusterIssuerSnapshot, now time.Time) (Observation, bool) {
	reason := ""
	switch {
	case snapshot.Name != entry.Profile.Name:
		reason = "object-name-mismatch"
	case snapshot.AnnotatedRevision != entry.Revision.Revision:
		reason = "revision-annotation-mismatch"
	case snapshot.AnnotatedSpecDigest != entry.Revision.SpecDigest:
		reason = "digest-annotation-mismatch"
	case snapshot.Solver != entry.Revision.Solver || snapshot.SpecDigest != entry.Revision.SpecDigest:
		reason = "live-spec-mismatch"
	case snapshot.Generation < 1 || snapshot.ReadyObservedGeneration != snapshot.Generation:
		reason = "generation-not-observed"
	case !snapshot.Ready:
		reason = "clusterissuer-not-ready"
	}
	observedAt := now
	state := Ready
	if reason != "" {
		state = Degraded
	}
	return Observation{ProfileID: entry.Profile.ID, Revision: entry.Revision.Revision, State: state,
		ObservedSpecDigest: snapshot.SpecDigest, ObservedGeneration: snapshot.ReadyObservedGeneration,
		Reason: reason, ObservedAt: &observedAt, UpdatedAt: now}, reason == ""
}

func (r *ObserverRuntime) Probe(ctx context.Context) error {
	probe := &ObserverReadinessProbe{Store: r.Store, ReadinessStore: r.ReadinessStore, Config: r.Config, Identity: r.Identity, Now: r.Now}
	return probe.Probe(ctx)
}

func (r *ObserverRuntime) recordReadiness(ctx context.Context, observation ObserverWorkerObservation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease.Validate() == nil && r.lease.Until.After(observation.ObservedAt) {
		lease, err := r.ReadinessStore.HeartbeatObserverReadiness(ctx, r.lease, observation, r.Config.ReadinessLease)
		if err == nil {
			r.lease = lease
			return nil
		}
		if !errors.Is(err, ErrObserverLeaseLost) {
			return err
		}
	}
	lease, err := r.ReadinessStore.AcquireObserverReadiness(ctx, observation, r.Config.ReadinessLease)
	if err != nil {
		return err
	}
	r.lease = lease
	return nil
}

type ObserverReadinessProbe struct {
	Store          ObserverStore
	ReadinessStore ObserverReadinessStore
	Config         ObserverConfig
	Identity       ObserverRuntimeIdentity
	Now            func() time.Time
}

func (p *ObserverReadinessProbe) Probe(ctx context.Context) error {
	if ctx == nil || p == nil || p.Store == nil || p.ReadinessStore == nil || p.Config.Validate() != nil || !p.Config.Enabled ||
		p.Identity.Validate() != nil || p.Now == nil {
		return ErrObservationUnavailable
	}
	expected, err := ObserverIdentityForConfig(p.Config)
	if err != nil || !observerIdentityEqual(expected, p.Identity) {
		return ErrObservationUnavailable
	}
	entries, targetDigest, err := observerTargets(ctx, p.Store, p.Config)
	if err != nil {
		return ErrObservationUnavailable
	}
	now := p.Now()
	if now.IsZero() || now.Location() != time.UTC || p.ReadinessStore.ObserverRuntimeReady(ctx, p.Identity, targetDigest, len(entries), now, p.Config.MaximumAge) != nil {
		return ErrObservationUnavailable
	}
	for _, entry := range entries {
		observation, observationErr := p.Store.Observation(ctx, entry.Profile.ID, entry.Revision.Revision)
		if observationErr != nil || observation.State != Ready || observation.ObservedSpecDigest != entry.Revision.SpecDigest ||
			observation.ObservedGeneration < 1 || observation.ObservedAt == nil || observation.ObservedAt.Location() != time.UTC ||
			observation.ObservedAt.Before(now.Add(-p.Config.MaximumAge)) || observation.ObservedAt.After(now) ||
			observation.UpdatedAt.IsZero() || observation.UpdatedAt.Location() != time.UTC || observation.UpdatedAt.Before(*observation.ObservedAt) ||
			observation.UpdatedAt.After(now) {
			return ErrObservationUnavailable
		}
	}
	return nil
}

func observerTargets(ctx context.Context, store ObserverStore, config ObserverConfig) ([]Entry, string, error) {
	if ctx == nil || store == nil || config.Validate() != nil || !config.Enabled {
		return nil, "", ErrObservationUnavailable
	}
	all, err := store.List(ctx, 500)
	if err != nil || len(all) >= 500 {
		return nil, "", ErrObservationUnavailable
	}
	entries := make([]Entry, 0, len(all))
	seen := map[string]struct{}{}
	for _, entry := range all {
		if _, duplicate := seen[entry.Profile.Name]; duplicate {
			return nil, "", ErrObservationUnavailable
		}
		seen[entry.Profile.Name] = struct{}{}
		if entry.Profile.Lifecycle != Active {
			continue
		}
		if entry.Profile.ID != entry.Revision.ProfileID ||
			entry.Profile.CurrentRevision != entry.Revision.Revision || entry.Revision.Revision < 1 {
			return nil, "", ErrObservationUnavailable
		}
		clean, solver, digest, normalizeErr := normalizeSpec(entry.Revision.Spec)
		if normalizeErr != nil || solver != entry.Revision.Solver || digest != entry.Revision.SpecDigest {
			return nil, "", ErrObservationUnavailable
		}
		entry.Revision.Spec = clean
		entries = append(entries, entry)
	}
	if len(entries) > MaximumObservedIssuers {
		return nil, "", ErrObservationUnavailable
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Profile.Name < entries[right].Profile.Name })
	encoded, err := json.Marshal(struct {
		Contract string `json:"contract"`
		Targets  []struct {
			ProfileID, Name, SpecDigest string
			Revision                    int64
		} `json:"targets"`
	}{Contract: ObserverContract, Targets: func() []struct {
		ProfileID, Name, SpecDigest string
		Revision                    int64
	} {
		values := make([]struct {
			ProfileID, Name, SpecDigest string
			Revision                    int64
		}, len(entries))
		for index, entry := range entries {
			values[index].ProfileID, values[index].Name, values[index].SpecDigest, values[index].Revision =
				entry.Profile.ID, entry.Profile.Name, entry.Revision.SpecDigest, entry.Revision.Revision
		}
		return values
	}()})
	if err != nil {
		return nil, "", ErrObservationUnavailable
	}
	sum := sha256.Sum256(encoded)
	return entries, "sha256:" + hex.EncodeToString(sum[:]), nil
}
