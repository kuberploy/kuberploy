package certificates

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

type ObservationState string

const (
	ObservationAwaiting ObservationState = "awaiting"
	ObservationReady    ObservationState = "ready"
	ObservationDegraded ObservationState = "degraded"
	ObservationRequeue  ObservationState = "requeue"
)

type ObservationSnapshot struct {
	BindingID           string
	VersionID           string
	TargetDigest        string
	Identity            ObservationIdentity
	State               ObservationState
	NextAt              time.Time
	ConsecutiveFailures int
	FailureCode         ObservationFailureCode
	LastObservedAt      time.Time
	LastReadyAt         time.Time
	LeaseEpoch          int64
	LeaseUntil          time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type memoryCertificateObservation struct {
	binding       secrets.Binding
	secretVersion secrets.Version
	attestation   Version
	snapshot      ObservationSnapshot
	lease         ObservationLease
}

// ObservationMemoryStore is an adversarial-test/local implementation of the
// same fenced contract expected from PostgreSQL. It is deliberately separate
// from the certificate attestation MemoryStore so production wiring cannot
// accidentally treat a one-time attestation as a current readiness result.
type ObservationMemoryStore struct {
	mu        sync.Mutex
	records   map[string]memoryCertificateObservation
	readiness map[string]ObservationReadinessLease
}

func NewObservationMemoryStore() *ObservationMemoryStore {
	return &ObservationMemoryStore{records: map[string]memoryCertificateObservation{}, readiness: map[string]ObservationReadinessLease{}}
}

// UpsertActiveCertificate mirrors the authoritative active-certificate join
// a PostgreSQL Claim transaction must perform. It accepts metadata only.
func (s *ObservationMemoryStore) UpsertActiveCertificate(binding secrets.Binding, secretVersion secrets.Version, attestation Version, availableAt time.Time) error {
	if s == nil || availableAt.IsZero() {
		return ErrInvalid
	}
	targetDigest, err := CertificateObservationTargetDigest(binding, secretVersion, attestation)
	if err != nil || availableAt.Before(binding.UpdatedAt) || availableAt.Before(secretVersion.UpdatedAt) || availableAt.Before(attestation.CreatedAt) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[secretVersion.ID]; ok {
		if existing.snapshot.BindingID != binding.ID || existing.snapshot.TargetDigest != targetDigest {
			return ErrConflict
		}
		existing.binding = binding
		existing.secretVersion = cloneObservedSecretVersion(secretVersion)
		existing.attestation = cloneVersion(attestation)
		s.records[secretVersion.ID] = existing
		return nil
	}
	s.records[secretVersion.ID] = memoryCertificateObservation{
		binding: binding, secretVersion: cloneObservedSecretVersion(secretVersion), attestation: cloneVersion(attestation),
		snapshot: ObservationSnapshot{BindingID: binding.ID, VersionID: secretVersion.ID, TargetDigest: targetDigest,
			State: ObservationAwaiting, NextAt: availableAt.UTC(), CreatedAt: availableAt.UTC(), UpdatedAt: availableAt.UTC()},
	}
	return nil
}

func (s *ObservationMemoryStore) Observation(versionID string) (ObservationSnapshot, error) {
	if s == nil || !uuidRE.MatchString(versionID) {
		return ObservationSnapshot{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[versionID]
	if !ok {
		return ObservationSnapshot{}, ErrNotFound
	}
	return record.snapshot, nil
}

func (s *ObservationMemoryStore) ClaimCertificateObservation(_ context.Context, identity ObservationIdentity, owner string, namespaces, namespacePrefixes []string, now time.Time, duration time.Duration) (ObservationWork, error) {
	normalized, err := NormalizeObservationNamespaces(namespaces)
	normalizedPrefixes, prefixErr := NormalizeObservationNamespacePrefixes(namespacePrefixes)
	if s == nil || identity.Validate() != nil || !observationWorkerIDRE.MatchString(owner) || err != nil || prefixErr != nil || len(namespaces)+len(namespacePrefixes) == 0 || !slices.Equal(normalized, namespaces) || !slices.Equal(normalizedPrefixes, namespacePrefixes) ||
		now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return ObservationWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *memoryCertificateObservation
	for versionID, candidate := range s.records {
		if candidate.snapshot.NextAt.After(now) || !candidate.lease.Until.IsZero() && candidate.lease.Until.After(now) {
			continue
		}
		if !observationNamespaceAllowed(namespaces, namespacePrefixes, candidate.binding.Scope.Namespace) {
			continue
		}
		digest, digestErr := CertificateObservationTargetDigest(candidate.binding, candidate.secretVersion, candidate.attestation)
		if digestErr != nil || digest != candidate.snapshot.TargetDigest {
			continue
		}
		copyCandidate := candidate
		if selected == nil || copyCandidate.snapshot.NextAt.Before(selected.snapshot.NextAt) ||
			copyCandidate.snapshot.NextAt.Equal(selected.snapshot.NextAt) && versionID < selected.snapshot.VersionID {
			selected = &copyCandidate
		}
	}
	if selected == nil {
		return ObservationWork{}, ErrNotFound
	}
	selected.snapshot.LeaseEpoch++
	selected.snapshot.UpdatedAt = now.UTC()
	selected.lease = ObservationLease{
		BindingID: selected.binding.ID, VersionID: selected.secretVersion.ID, Owner: owner,
		Epoch: selected.snapshot.LeaseEpoch, ClaimedAt: now.UTC(), Until: now.UTC().Add(duration),
		Identity: identity, TargetDigest: selected.snapshot.TargetDigest,
	}
	selected.snapshot.LeaseUntil = selected.lease.Until
	s.records[selected.secretVersion.ID] = *selected
	work := ObservationWork{Binding: selected.binding, SecretVersion: cloneObservedSecretVersion(selected.secretVersion),
		Attestation: cloneVersion(selected.attestation), Lease: selected.lease, ConsecutiveFailures: selected.snapshot.ConsecutiveFailures}
	if work.Validate() != nil {
		return ObservationWork{}, ErrConflict
	}
	return work, nil
}

func (s *ObservationMemoryStore) HeartbeatCertificateObservation(_ context.Context, lease ObservationLease, now time.Time, duration time.Duration) (ObservationLease, error) {
	if s == nil || lease.Validate() != nil || now.IsZero() || duration < 20*time.Second || duration > time.Hour {
		return ObservationLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[lease.VersionID]
	if !ok || !sameObservationLease(record.lease, lease) || !now.Before(record.lease.Until) || now.Before(record.snapshot.UpdatedAt) {
		return ObservationLease{}, ErrObservationLeaseLost
	}
	record.lease.Until = now.UTC().Add(duration)
	record.snapshot.LeaseUntil, record.snapshot.UpdatedAt = record.lease.Until, now.UTC()
	s.records[lease.VersionID] = record
	return record.lease, nil
}

func (s *ObservationMemoryStore) ApplyCertificateObservationReady(_ context.Context, lease ObservationLease, outcome ObservationReadyOutcome, now time.Time) error {
	if s == nil || lease.Validate() != nil || validateObservationTime(outcome.ObservedAt, outcome.NextAt, now, lease.ClaimedAt) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lockedObservation(lease, now)
	if err != nil {
		return err
	}
	record.snapshot.State, record.snapshot.Identity = ObservationReady, lease.Identity
	record.snapshot.NextAt, record.snapshot.ConsecutiveFailures, record.snapshot.FailureCode = outcome.NextAt.UTC(), 0, ""
	record.snapshot.LastObservedAt, record.snapshot.LastReadyAt = outcome.ObservedAt.UTC(), outcome.ObservedAt.UTC()
	record.snapshot.UpdatedAt, record.snapshot.LeaseUntil, record.lease = now.UTC(), time.Time{}, ObservationLease{}
	s.records[lease.VersionID] = record
	return nil
}

func (s *ObservationMemoryStore) ApplyCertificateObservationDegraded(_ context.Context, lease ObservationLease, outcome ObservationDegradedOutcome, now time.Time) error {
	if s == nil || lease.Validate() != nil || !outcome.FailureCode.valid() ||
		validateObservationTime(outcome.ObservedAt, outcome.NextAt, now, lease.ClaimedAt) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lockedObservation(lease, now)
	if err != nil {
		return err
	}
	record.snapshot.State, record.snapshot.Identity = ObservationDegraded, lease.Identity
	record.snapshot.NextAt, record.snapshot.FailureCode = outcome.NextAt.UTC(), outcome.FailureCode
	record.snapshot.ConsecutiveFailures = min(30, record.snapshot.ConsecutiveFailures+1)
	record.snapshot.LastObservedAt, record.snapshot.UpdatedAt = outcome.ObservedAt.UTC(), now.UTC()
	record.snapshot.LeaseUntil, record.lease = time.Time{}, ObservationLease{}
	s.records[lease.VersionID] = record
	return nil
}

func (s *ObservationMemoryStore) RequeueCertificateObservation(_ context.Context, lease ObservationLease, outcome ObservationRequeueOutcome, now time.Time) error {
	if s == nil || lease.Validate() != nil || !outcome.FailureCode.valid() || now.IsZero() || !outcome.NextAt.After(now) || outcome.NextAt.After(now.Add(24*time.Hour)) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lockedObservation(lease, now)
	if err != nil {
		return err
	}
	record.snapshot.State, record.snapshot.Identity = ObservationRequeue, lease.Identity
	record.snapshot.NextAt, record.snapshot.FailureCode = outcome.NextAt.UTC(), outcome.FailureCode
	record.snapshot.ConsecutiveFailures = min(30, record.snapshot.ConsecutiveFailures+1)
	record.snapshot.UpdatedAt, record.snapshot.LeaseUntil, record.lease = now.UTC(), time.Time{}, ObservationLease{}
	s.records[lease.VersionID] = record
	return nil
}

func (s *ObservationMemoryStore) ActiveCertificateReady(_ context.Context, bindingID, versionID string, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(versionID) || identity.Validate() != nil || now.IsZero() ||
		maximumAge < 10*time.Second || maximumAge > 30*time.Minute {
		return ErrObservationUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[versionID]
	if !ok || record.snapshot.BindingID != bindingID || record.snapshot.State != ObservationReady ||
		!observationIdentityEqual(record.snapshot.Identity, identity) || record.snapshot.LastReadyAt.IsZero() ||
		now.Before(record.attestation.NotBefore) || !now.Before(record.attestation.NotAfter) ||
		record.snapshot.LastReadyAt.Before(now.Add(-maximumAge)) || record.snapshot.LastReadyAt.After(now.Add(CertificateObservationReadinessSkew)) {
		return ErrObservationUnavailable
	}
	digest, err := CertificateObservationTargetDigest(record.binding, record.secretVersion, record.attestation)
	if err != nil || digest != record.snapshot.TargetDigest {
		return ErrObservationUnavailable
	}
	return nil
}

func (s *ObservationMemoryStore) lockedObservation(lease ObservationLease, now time.Time) (memoryCertificateObservation, error) {
	record, ok := s.records[lease.VersionID]
	if !ok || !sameObservationLease(record.lease, lease) || !now.Before(record.lease.Until) || now.Before(record.snapshot.UpdatedAt) {
		return memoryCertificateObservation{}, ErrObservationLeaseLost
	}
	digest, err := CertificateObservationTargetDigest(record.binding, record.secretVersion, record.attestation)
	if err != nil || digest != lease.TargetDigest || digest != record.snapshot.TargetDigest {
		return memoryCertificateObservation{}, ErrObservationLeaseLost
	}
	return record, nil
}

func sameObservationLease(left, right ObservationLease) bool {
	return left.BindingID == right.BindingID && left.VersionID == right.VersionID && left.Owner == right.Owner && left.Epoch == right.Epoch &&
		left.ClaimedAt.Equal(right.ClaimedAt) && left.Until.Equal(right.Until) && observationIdentityEqual(left.Identity, right.Identity) &&
		left.TargetDigest == right.TargetDigest
}

func (s *ObservationMemoryStore) AcquireCertificateObservationReadiness(_ context.Context, observation ObservationWorkerObservation, duration time.Duration) (ObservationReadinessLease, error) {
	if s == nil || observation.Validate() != nil || duration < 20*time.Second || duration > time.Hour {
		return ObservationReadinessLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if existing, ok := s.readiness[observation.WorkerID]; ok {
		epoch = existing.Epoch + 1
	}
	lease := ObservationReadinessLease{ObservationWorkerObservation: observation, Epoch: epoch, Until: observation.ObservedAt.UTC().Add(duration)}
	s.readiness[observation.WorkerID] = lease
	return lease, nil
}

func (s *ObservationMemoryStore) HeartbeatCertificateObservationReadiness(_ context.Context, lease ObservationReadinessLease, observedAt time.Time, duration time.Duration) (ObservationReadinessLease, error) {
	if s == nil || lease.Validate() != nil || observedAt.IsZero() || observedAt.Before(lease.ObservedAt) || duration < 20*time.Second || duration > time.Hour {
		return ObservationReadinessLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.readiness[lease.WorkerID]
	if !ok || current.Epoch != lease.Epoch || !current.StartedAt.Equal(lease.StartedAt) || !current.ObservedAt.Equal(lease.ObservedAt) ||
		!current.Until.Equal(lease.Until) || !observationIdentityEqual(current.Identity, lease.Identity) || !observedAt.Before(current.Until) {
		return ObservationReadinessLease{}, ErrObservationLeaseLost
	}
	current.ObservedAt, current.Until = observedAt.UTC(), observedAt.UTC().Add(duration)
	s.readiness[lease.WorkerID] = current
	return current, nil
}

func (s *ObservationMemoryStore) CertificateObservationRuntimeReady(_ context.Context, identity ObservationIdentity, now time.Time, maximumAge time.Duration) error {
	if s == nil || identity.Validate() != nil || now.IsZero() || maximumAge < 2*time.Second || maximumAge > 5*time.Minute {
		return ErrObservationUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range s.readiness {
		if observationIdentityEqual(lease.Identity, identity) && lease.Until.After(now) &&
			!lease.ObservedAt.Before(now.Add(-maximumAge)) && !lease.ObservedAt.After(now.Add(CertificateObservationReadinessSkew)) {
			return nil
		}
	}
	return ErrObservationUnavailable
}

func cloneObservedSecretVersion(version secrets.Version) secrets.Version {
	version.Deliveries = append([]secrets.Delivery(nil), version.Deliveries...)
	if version.Artifact != nil {
		artifact := *version.Artifact
		version.Artifact = &artifact
	}
	return version
}

var _ ObservationStore = (*ObservationMemoryStore)(nil)
