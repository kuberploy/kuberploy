package edge

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

type MemoryStore struct {
	mu        sync.Mutex
	targets   map[string]Target
	readiness map[string]Readiness
	sslip     map[string]SSLIPIngressObservation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{targets: map[string]Target{}, readiness: map[string]Readiness{}, sslip: map[string]SSLIPIngressObservation{}}
}

func (s *MemoryStore) SynchronizeTargets(_ context.Context, configDigest string, desired []DesiredTarget, now time.Time) error {
	if s == nil || !validDigest(configDigest) || now.IsZero() || len(desired) < 1 || len(desired) > MaximumTargets {
		return ErrInvalid
	}
	for index, target := range desired {
		if target.Validate() != nil || target.RuntimeConfigDigest != configDigest || index > 0 && desired[index-1].Key >= target.Key {
			return ErrInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]DesiredTarget, len(desired))
	for _, value := range desired {
		wanted[targetMapKey(value.Key, value.Revision)] = value
	}
	for mapKey, target := range s.targets {
		if !target.Active {
			continue
		}
		if _, keep := wanted[mapKey]; keep {
			continue
		}
		if now.Before(target.UpdatedAt) {
			return ErrConflict
		}
		target.Active, target.LeaseOwner, target.LeaseUntil, target.WorkerContract, target.WorkerConfigDigest = false, "", nil, "", ""
		target.UpdatedAt = now.UTC()
		s.targets[mapKey] = target
	}
	for _, value := range desired {
		mapKey := targetMapKey(value.Key, value.Revision)
		if current, exists := s.targets[mapKey]; exists {
			identity := current.DesiredTarget
			identity.RuntimeConfigDigest = value.RuntimeConfigDigest
			if identity != value || now.Before(current.UpdatedAt) {
				return ErrConflict
			}
			configChanged := current.RuntimeConfigDigest != value.RuntimeConfigDigest
			if !current.Active || configChanged {
				current.DesiredTarget, current.Active = value, true
				current.State, current.NextObservationAt = StateAwaiting, now.UTC()
				current.ConsecutiveFailures, current.LastFailureCode = 0, ""
				current.LeaseOwner, current.LeaseUntil, current.WorkerContract, current.WorkerConfigDigest = "", nil, "", ""
				current.UpdatedAt = now.UTC()
				s.targets[mapKey] = current
			}
			continue
		}
		target := Target{DesiredTarget: value, Active: true, State: StateAwaiting, NextObservationAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
		if target.Validate() != nil {
			return ErrInvalid
		}
		s.targets[mapKey] = target
	}
	return nil
}

func (s *MemoryStore) Target(_ context.Context, key string, revision int64) (Target, error) {
	if s == nil || key == "" || revision <= 0 {
		return Target{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, exists := s.targets[targetMapKey(key, revision)]
	if !exists {
		return Target{}, ErrNotFound
	}
	return cloneTarget(target), nil
}

func (s *MemoryStore) ClaimTarget(_ context.Context, owner, contract, configDigest string, now time.Time, duration time.Duration) (Lease, bool, error) {
	if s == nil || !workerIDPattern.MatchString(owner) || contract != RuntimeContract || !validDigest(configDigest) || now.IsZero() ||
		duration < 30*time.Second || duration > 15*time.Minute {
		return Lease{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.targets))
	for key := range s.targets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	selected := ""
	for _, key := range keys {
		target := s.targets[key]
		if !target.Active || target.State == StateFailed || target.RuntimeConfigDigest != configDigest || target.NextObservationAt.After(now) ||
			target.LeaseUntil != nil && target.LeaseUntil.After(now) {
			continue
		}
		if selected == "" || target.NextObservationAt.Before(s.targets[selected].NextObservationAt) {
			selected = key
		}
	}
	if selected == "" {
		return Lease{}, false, nil
	}
	target := s.targets[selected]
	target.LeaseEpoch++
	until := now.UTC().Add(duration)
	target.LeaseOwner, target.LeaseUntil, target.WorkerContract, target.WorkerConfigDigest = owner, &until, contract, configDigest
	target.UpdatedAt = now.UTC()
	s.targets[selected] = target
	lease := Lease{Target: cloneTarget(target), Owner: owner, Epoch: target.LeaseEpoch, Until: until}
	if lease.Validate(now.UTC()) != nil {
		return Lease{}, false, ErrConflict
	}
	return lease, true, nil
}

func (s *MemoryStore) HeartbeatTarget(_ context.Context, lease Lease, now time.Time, duration time.Duration) (Lease, error) {
	if s == nil || now.IsZero() || duration < 30*time.Second || duration > 15*time.Minute || lease.Validate(now) != nil {
		return Lease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, exists := s.targets[targetMapKey(lease.Target.Key, lease.Target.Revision)]
	if !exists || !sameLease(target, lease, now) {
		return Lease{}, ErrLeaseLost
	}
	until := now.UTC().Add(duration)
	target.LeaseUntil, target.UpdatedAt = &until, now.UTC()
	s.targets[targetMapKey(target.Key, target.Revision)] = target
	return Lease{Target: cloneTarget(target), Owner: lease.Owner, Epoch: lease.Epoch, Until: until}, nil
}

func (s *MemoryStore) RecordTargetReady(_ context.Context, lease Lease, receipt ObservationReceipt, observedAt, next time.Time) (Target, error) {
	if s == nil || observedAt.IsZero() || next.Before(observedAt) || lease.Validate(observedAt) != nil ||
		receipt.Validate(lease.Target.DesiredTarget) != nil {
		return Target{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mapKey := targetMapKey(lease.Target.Key, lease.Target.Revision)
	target, exists := s.targets[mapKey]
	if !exists || !sameLease(target, lease, observedAt) {
		return Target{}, ErrLeaseLost
	}
	if target.ObservedIdentityDigest != "" && target.ObservedIdentityDigest != receipt.IdentityDigest {
		return Target{}, ErrIdentityChanged
	}
	observed := observedAt.UTC()
	if receipt.SSLIP != nil {
		observation := SSLIPIngressObservation{TargetKey: target.Key, ProfileRevision: target.Revision,
			DesiredDigest: target.DesiredDigest, RuntimeConfigDigest: target.RuntimeConfigDigest,
			Endpoint: *receipt.SSLIP, ObservedAt: observed}
		if observation.Validate(target.DesiredTarget) != nil {
			return Target{}, ErrInvalid
		}
		if current, present := s.sslip[mapKey]; present {
			if current.TargetKey != observation.TargetKey || current.ProfileRevision != observation.ProfileRevision ||
				current.DesiredDigest != observation.DesiredDigest || current.Endpoint.PublicIPv4 != observation.Endpoint.PublicIPv4 ||
				current.Endpoint.Source != observation.Endpoint.Source || current.Endpoint.ServiceUID != observation.Endpoint.ServiceUID ||
				observation.ObservedAt.Before(current.ObservedAt) {
				return Target{}, ErrIdentityChanged
			}
		}
		s.sslip[mapKey] = observation
	}
	target.State, target.NextObservationAt, target.LastObservedAt = StateReady, next.UTC(), &observed
	target.ObservedIdentityDigest, target.ObservedResourceVersions = receipt.IdentityDigest, receipt.ResourceVersionDigest
	target.ConsecutiveFailures, target.LastFailureCode = 0, ""
	target.LeaseOwner, target.LeaseUntil, target.WorkerContract, target.WorkerConfigDigest = "", nil, "", ""
	target.UpdatedAt = observed
	s.targets[mapKey] = target
	return cloneTarget(target), nil
}

func (s *MemoryStore) SSLIPIngressObservation(_ context.Context, key string, revision int64) (SSLIPIngressObservation, error) {
	if s == nil || key != "traefik" || revision <= 0 {
		return SSLIPIngressObservation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observation, present := s.sslip[targetMapKey(key, revision)]
	if !present {
		return SSLIPIngressObservation{}, ErrNotFound
	}
	return observation, nil
}

func (s *MemoryStore) RecordTargetRetry(_ context.Context, lease Lease, code string, permanent bool, next, now time.Time) (Target, error) {
	if s == nil || !failureCodePattern.MatchString(code) || now.IsZero() || next.Before(now) || lease.Validate(now) != nil {
		return Target{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mapKey := targetMapKey(lease.Target.Key, lease.Target.Revision)
	target, exists := s.targets[mapKey]
	if !exists || !sameLease(target, lease, now) {
		return Target{}, ErrLeaseLost
	}
	target.ConsecutiveFailures = min(30, target.ConsecutiveFailures+1)
	target.LastFailureCode, target.NextObservationAt = code, next.UTC()
	if permanent || target.ConsecutiveFailures == 30 {
		target.State = StateFailed
	} else {
		target.State = StateAwaiting
	}
	target.LeaseOwner, target.LeaseUntil, target.WorkerContract, target.WorkerConfigDigest = "", nil, "", ""
	target.UpdatedAt = now.UTC()
	s.targets[mapKey] = target
	return cloneTarget(target), nil
}

func (s *MemoryStore) RecordReadiness(_ context.Context, readiness Readiness) error {
	if s == nil || readiness.Validate() != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.readiness[readiness.WorkerID]; exists {
		if readiness.WorkerEpoch < current.WorkerEpoch || readiness.WorkerEpoch > current.WorkerEpoch+1 {
			return ErrConflict
		}
		if readiness.WorkerEpoch == current.WorkerEpoch && (readiness.Contract != current.Contract || readiness.ConfigDigest != current.ConfigDigest ||
			readiness.TargetCount != current.TargetCount || !readiness.StartedAt.Equal(current.StartedAt) || readiness.ObservedAt.Before(current.ObservedAt)) {
			return ErrConflict
		}
	}
	s.readiness[readiness.WorkerID] = readiness
	return nil
}

func (s *MemoryStore) RuntimeReady(_ context.Context, contract, configDigest string, targetCount int, now time.Time, maximumAge time.Duration) error {
	if s == nil || contract != RuntimeContract || !validDigest(configDigest) || targetCount < 1 || targetCount > MaximumTargets || now.IsZero() ||
		maximumAge < 30*time.Second || maximumAge > 15*time.Minute {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	workerReady := false
	for _, readiness := range s.readiness {
		if readiness.Contract == contract && readiness.ConfigDigest == configDigest && readiness.TargetCount == targetCount &&
			readiness.LeaseUntil.After(now) && !readiness.ObservedAt.After(now.Add(5*time.Second)) && !readiness.ObservedAt.Before(now.Add(-maximumAge)) {
			workerReady = true
			break
		}
	}
	if !workerReady {
		return ErrUnavailable
	}
	active := 0
	for _, target := range s.targets {
		if !target.Active {
			continue
		}
		if target.RuntimeConfigDigest != configDigest || target.State != StateReady || target.LastObservedAt == nil ||
			target.LastObservedAt.Before(now.Add(-maximumAge)) || target.LastObservedAt.After(now.Add(5*time.Second)) {
			return ErrUnavailable
		}
		active++
	}
	if active != targetCount {
		return ErrUnavailable
	}
	return nil
}

func sameLease(target Target, lease Lease, now time.Time) bool {
	return target.Active && target.Key == lease.Target.Key && target.Revision == lease.Target.Revision &&
		target.DesiredDigest == lease.Target.DesiredDigest && target.RuntimeConfigDigest == lease.Target.RuntimeConfigDigest &&
		target.LeaseOwner == lease.Owner && target.LeaseEpoch == lease.Epoch && target.LeaseUntil != nil &&
		target.LeaseUntil.Equal(lease.Until) && target.LeaseUntil.After(now) && target.WorkerContract == RuntimeContract &&
		target.WorkerConfigDigest == lease.Target.RuntimeConfigDigest
}

func compareDesired(left, right DesiredTarget) int { return strings.Compare(left.Key, right.Key) }

var _ Store = (*MemoryStore)(nil)
