package memory

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func registryScopeKey(parts ...string) string { return strings.Join(parts, "\x00") }

func (s *Store) PutRegistryTarget(_ context.Context, target domain.RegistryTarget) (domain.RegistryTarget, error) {
	if err := registry.ValidateTarget(target); err != nil {
		return domain.RegistryTarget{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if current, exists := s.registryTargets[target.ID]; exists {
		if current.Mode != target.Mode {
			return domain.RegistryTarget{}, base.ErrConflict
		}
		if current.RepositoryPrefix != target.RepositoryPrefix {
			for _, policy := range s.registryPolicies {
				if policy.RegistryTargetID == target.ID {
					return domain.RegistryTarget{}, base.ErrConflict
				}
			}
		}
		target.CreatedAt = current.CreatedAt
		if target.UpdatedAt.IsZero() {
			target.UpdatedAt = now
		}
	} else {
		if target.CreatedAt.IsZero() {
			target.CreatedAt = now
		}
		if target.UpdatedAt.IsZero() {
			target.UpdatedAt = target.CreatedAt
		}
	}
	for targetID, current := range s.registryTargets {
		if targetID != target.ID && current.Name == target.Name {
			return domain.RegistryTarget{}, base.ErrConflict
		}
	}
	s.registryTargets[target.ID] = target
	return target, nil
}

func (s *Store) RegistryTarget(_ context.Context, targetID string) (domain.RegistryTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.registryTargets[targetID]
	if !ok {
		return domain.RegistryTarget{}, base.ErrNotFound
	}
	return target, nil
}

func (s *Store) PutServiceRegistryPolicy(_ context.Context, policy domain.ServiceRegistryPolicy) (domain.ServiceRegistryPolicy, error) {
	now := time.Now().UTC()
	policy = registry.NormalizePolicy(policy, now)
	if err := registry.ValidatePolicy(policy); err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.registryTargets[policy.RegistryTargetID]
	if !ok {
		return domain.ServiceRegistryPolicy{}, base.ErrNotFound
	}
	if err := registry.ValidatePolicyForTarget(target, policy); err != nil {
		return domain.ServiceRegistryPolicy{}, err
	}
	key := registryScopeKey(policy.RegistryTargetID, policy.ServiceID)
	if current, ok := s.registryPolicies[key]; ok {
		policy.CreatedAt = current.CreatedAt
	}
	for otherKey, current := range s.registryPolicies {
		if otherKey != key && current.RegistryTargetID == policy.RegistryTargetID && current.Repository == policy.Repository {
			return domain.ServiceRegistryPolicy{}, base.ErrConflict
		}
	}
	s.registryPolicies[key] = policy
	return policy, nil
}

func (s *Store) ServiceRegistryPolicy(_ context.Context, targetID, serviceID string) (domain.ServiceRegistryPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, ok := s.registryPolicies[registryScopeKey(targetID, serviceID)]
	if !ok {
		return domain.ServiceRegistryPolicy{}, base.ErrNotFound
	}
	return policy, nil
}

func (s *Store) RecordRegistryInventory(_ context.Context, observation domain.RegistryInventoryObservation) error {
	if observation.RegistryTargetID == "" || observation.Revision == "" || observation.ObservedAt.IsZero() {
		return base.ErrRegistryObservationIncomplete
	}
	repositories := append([]string(nil), observation.Repositories...)
	sort.Strings(repositories)
	for i, repository := range repositories {
		if repository == "" || repository == "*" || (i > 0 && repository == repositories[i-1]) {
			return base.ErrRegistryObservationIncomplete
		}
	}
	observation.Repositories = repositories
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registryTargets[observation.RegistryTargetID]; !ok {
		return base.ErrNotFound
	}
	if current, ok := s.registryInventories[observation.RegistryTargetID]; ok && current.Revision == observation.Revision {
		if reflect.DeepEqual(current, observation) {
			return nil
		}
		return base.ErrConflict
	}
	s.registryInventories[observation.RegistryTargetID] = observation
	return nil
}

func (s *Store) ReplaceRegistryCatalog(_ context.Context, snapshot domain.RegistryCatalogSnapshot) error {
	snapshot.Observation.SnapshotDigest = base.RegistryCatalogSnapshotDigest(snapshot)
	if err := registry.ValidateCatalog(snapshot); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observation := snapshot.Observation
	if _, ok := s.registryTargets[observation.RegistryTargetID]; !ok {
		return base.ErrNotFound
	}
	key := registryScopeKey(observation.RegistryTargetID, observation.Repository)
	current, exists := s.registryCatalogs[key]
	if exists && observation.Revision < current.Observation.Revision {
		return base.ErrConflict
	}
	if exists && observation.Revision == current.Observation.Revision {
		if observation.SnapshotDigest == current.Observation.SnapshotDigest {
			return nil
		}
		return base.ErrConflict
	}
	if observation.ID == "" {
		observation.ID = id.New()
		snapshot.Observation.ID = observation.ID
	}
	if !observation.Complete {
		snapshot.Manifests = cloneManifests(current.Manifests)
		snapshot.Blobs = cloneBlobs(current.Blobs)
		snapshot.Children = append([]domain.RegistryManifestLink(nil), current.Children...)
		snapshot.BlobLinks = append([]domain.RegistryManifestBlobLink(nil), current.BlobLinks...)
		s.registryCatalogs[key] = snapshot
		return nil
	}
	snapshot = normalizeCatalog(snapshot, current)
	s.registryCatalogs[key] = snapshot
	s.reconcileAvailabilityLocked(snapshot)
	return nil
}

func normalizeCatalog(snapshot, current domain.RegistryCatalogSnapshot) domain.RegistryCatalogSnapshot {
	oldManifest := make(map[string]domain.RegistryManifest, len(current.Manifests))
	for _, manifest := range current.Manifests {
		oldManifest[manifest.Digest] = manifest
	}
	for index := range snapshot.Manifests {
		manifest := &snapshot.Manifests[index]
		manifest.Present = true
		manifest.LastObservedAt = snapshot.Observation.ObservedAt
		manifest.LastObservationRevision = snapshot.Observation.Revision
		manifest.DeletedAt = nil
		if old, ok := oldManifest[manifest.Digest]; ok {
			manifest.FirstObservedAt = old.FirstObservedAt
		} else if manifest.FirstObservedAt.IsZero() {
			manifest.FirstObservedAt = snapshot.Observation.ObservedAt
		}
	}
	oldBlob := make(map[string]domain.RegistryBlob, len(current.Blobs))
	for _, blob := range current.Blobs {
		oldBlob[blob.Digest] = blob
	}
	for index := range snapshot.Blobs {
		blob := &snapshot.Blobs[index]
		blob.Present = true
		blob.LastObservedAt = snapshot.Observation.ObservedAt
		blob.LastObservationRevision = snapshot.Observation.Revision
		blob.DeletedAt = nil
		if old, ok := oldBlob[blob.Digest]; ok {
			blob.FirstObservedAt = old.FirstObservedAt
		} else if blob.FirstObservedAt.IsZero() {
			blob.FirstObservedAt = snapshot.Observation.ObservedAt
		}
	}
	return cloneCatalog(snapshot)
}

func (s *Store) reconcileAvailabilityLocked(snapshot domain.RegistryCatalogSnapshot) {
	present := make(map[string]struct{}, len(snapshot.Manifests))
	for _, manifest := range snapshot.Manifests {
		present[manifest.Digest] = struct{}{}
	}
	target := s.registryTargets[snapshot.Observation.RegistryTargetID]
	for releaseID, release := range s.registryReleases {
		if release.RegistryTargetID != target.ID || release.Repository != snapshot.Observation.Repository {
			continue
		}
		if _, ok := present[release.RootDigest]; ok {
			release.Availability = domain.RegistryArtifactPresent
			release.AvailabilityObservedAt = nil
		} else {
			observed := snapshot.Observation.ObservedAt
			if target.Mode == domain.RegistryTargetManaged {
				release.Availability = domain.RegistryArtifactExpired
			} else {
				release.Availability = domain.RegistryArtifactMissing
			}
			release.AvailabilityObservedAt = &observed
		}
		s.registryReleases[releaseID] = release
	}
	for cacheID, cache := range s.registryCaches {
		if cache.RegistryTargetID == target.ID && cache.Repository == snapshot.Observation.Repository {
			if _, ok := present[cache.RootDigest]; !ok && cache.State != "deleted" {
				cache.State = "missing"
				s.registryCaches[cacheID] = cache
			}
		}
	}
}

func (s *Store) ReplaceRegistryProtectionSnapshot(_ context.Context, snapshot domain.RegistryProtectionSnapshot) error {
	snapshot.Observation.SnapshotDigest = base.RegistryProtectionSnapshotDigest(snapshot)
	observation := snapshot.Observation
	expectedKind := map[domain.RegistryAuthority]domain.RegistryArtifactReferenceKind{
		domain.RegistryAuthorityGitIntent:  domain.RegistryReferenceCurrentGitIntent,
		domain.RegistryAuthorityRuntime:    domain.RegistryReferenceObservedRunning,
		domain.RegistryAuthorityOperations: domain.RegistryReferenceActiveOperation,
	}[observation.Authority]
	if observation.RegistryTargetID == "" || observation.ServiceID == "" || observation.Revision == "" || observation.ObservedAt.IsZero() || expectedKind == "" {
		return base.ErrRegistryObservationIncomplete
	}
	if !observation.Complete && len(snapshot.References) != 0 {
		return base.ErrRegistryObservationIncomplete
	}
	seen := make(map[string]struct{})
	for index := range snapshot.References {
		reference := &snapshot.References[index]
		if reference.RegistryTargetID != observation.RegistryTargetID || reference.ServiceID != observation.ServiceID || reference.Kind != expectedKind || reference.ReferenceKey == "" || reference.Repository == "" {
			return base.ErrRegistryObservationIncomplete
		}
		if _, ok := seen[reference.ReferenceKey]; ok {
			return base.ErrConflict
		}
		seen[reference.ReferenceKey] = struct{}{}
		if reference.ObservedAt.IsZero() {
			reference.ObservedAt = observation.ObservedAt
		}
		if reference.CreatedAt.IsZero() {
			reference.CreatedAt = observation.ObservedAt
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registryTargets[observation.RegistryTargetID]; !ok {
		return base.ErrNotFound
	}
	key := registryScopeKey(observation.RegistryTargetID, observation.ServiceID, string(observation.Authority))
	if current, ok := s.registryAuthorities[key]; ok && current.Observation.Revision == observation.Revision {
		if current.Observation == observation && (!observation.Complete || reflect.DeepEqual(current.References, snapshot.References)) {
			return nil
		}
		return base.ErrConflict
	}
	if !observation.Complete {
		snapshot.References = cloneReferences(s.registryAuthorities[key].References)
	}
	s.registryAuthorities[key] = cloneProtectionSnapshot(snapshot)
	return nil
}

func (s *Store) PutRegistryPin(_ context.Context, reference domain.RegistryArtifactReference) error {
	if reference.Kind != domain.RegistryReferencePin || reference.RegistryTargetID == "" || reference.ServiceID == "" || reference.Repository == "" || reference.ReferenceKey == "" {
		return base.ErrRegistryPolicyInvalid
	}
	if reference.CreatedAt.IsZero() {
		reference.CreatedAt = time.Now().UTC()
	}
	if reference.ObservedAt.IsZero() {
		reference.ObservedAt = reference.CreatedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registryPolicies[registryScopeKey(reference.RegistryTargetID, reference.ServiceID)]; !ok {
		return base.ErrNotFound
	}
	s.registryPins[registryScopeKey(reference.RegistryTargetID, reference.ServiceID, reference.ReferenceKey)] = reference
	return nil
}

func (s *Store) DeleteRegistryPin(_ context.Context, targetID, serviceID, referenceKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registryPins, registryScopeKey(targetID, serviceID, referenceKey))
	return nil
}

func (s *Store) PutRegistryRelease(_ context.Context, release domain.RegistryRelease) (domain.RegistryRelease, bool, error) {
	if release.ID == "" || release.RegistryTargetID == "" || release.ServiceID == "" || release.Repository == "" || release.RootDigest == "" {
		return domain.RegistryRelease{}, false, base.ErrRegistryPolicyInvalid
	}
	if release.Availability == "" {
		release.Availability = domain.RegistryArtifactPresent
	}
	if release.Availability != domain.RegistryArtifactPresent || release.AvailabilityObservedAt != nil {
		return domain.RegistryRelease{}, false, base.ErrRegistryPolicyInvalid
	}
	if release.CreatedAt.IsZero() {
		release.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registryPolicies[registryScopeKey(release.RegistryTargetID, release.ServiceID)]; !ok {
		return domain.RegistryRelease{}, false, base.ErrNotFound
	}
	if current, ok := s.registryReleases[release.ID]; ok {
		if reflect.DeepEqual(current, release) {
			return cloneRelease(current), true, nil
		}
		return domain.RegistryRelease{}, false, base.ErrConflict
	}
	s.registryReleases[release.ID] = cloneRelease(release)
	return release, false, nil
}

func (s *Store) RegistryRelease(_ context.Context, releaseID string) (domain.RegistryRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok := s.registryReleases[releaseID]
	if !ok {
		return domain.RegistryRelease{}, base.ErrNotFound
	}
	return cloneRelease(release), nil
}

func (s *Store) PutRegistryCacheGeneration(_ context.Context, generation domain.RegistryCacheGeneration) (domain.RegistryCacheGeneration, bool, error) {
	if generation.ID == "" || generation.RegistryTargetID == "" || generation.ServiceID == "" || generation.Repository == "" || generation.PlatformSet == "" || generation.TrustLane == "" || generation.CacheSchema == "" || generation.BuildDefinitionHash == "" || generation.Generation <= 0 || generation.SizeBytes < 0 {
		return domain.RegistryCacheGeneration{}, false, base.ErrRegistryPolicyInvalid
	}
	if generation.CreatedAt.IsZero() {
		generation.CreatedAt = time.Now().UTC()
	}
	if generation.LastUsedAt.IsZero() {
		generation.LastUsedAt = generation.CreatedAt
	}
	if generation.State == "" {
		generation.State = "succeeded"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registryPolicies[registryScopeKey(generation.RegistryTargetID, generation.ServiceID)]; !ok {
		return domain.RegistryCacheGeneration{}, false, base.ErrNotFound
	}
	if current, ok := s.registryCaches[generation.ID]; ok {
		if reflect.DeepEqual(current, generation) {
			return cloneCache(current), true, nil
		}
		return domain.RegistryCacheGeneration{}, false, base.ErrConflict
	}
	for _, current := range s.registryCaches {
		if current.RegistryTargetID == generation.RegistryTargetID && current.ServiceID == generation.ServiceID && current.PlatformSet == generation.PlatformSet && current.TrustLane == generation.TrustLane && current.CacheSchema == generation.CacheSchema && current.BuildDefinitionHash == generation.BuildDefinitionHash && current.Generation == generation.Generation {
			return domain.RegistryCacheGeneration{}, false, base.ErrConflict
		}
	}
	s.registryCaches[generation.ID] = cloneCache(generation)
	return generation, false, nil
}

func (s *Store) RegistryLifecycleSnapshot(_ context.Context, targetID, serviceID string, now time.Time) (domain.RegistryLifecycleSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registryLifecycleSnapshotLocked(targetID, serviceID, now)
}

func (s *Store) registryLifecycleSnapshotLocked(targetID, serviceID string, now time.Time) (domain.RegistryLifecycleSnapshot, error) {
	target, ok := s.registryTargets[targetID]
	if !ok {
		return domain.RegistryLifecycleSnapshot{}, base.ErrNotFound
	}
	policy, ok := s.registryPolicies[registryScopeKey(targetID, serviceID)]
	if !ok {
		return domain.RegistryLifecycleSnapshot{}, base.ErrNotFound
	}
	snapshot := domain.RegistryLifecycleSnapshot{Target: target, Policy: policy, Inventory: cloneInventory(s.registryInventories[targetID]), AsOf: now.UTC()}
	for _, catalog := range s.registryCatalogs {
		if catalog.Observation.RegistryTargetID != targetID {
			continue
		}
		snapshot.CatalogObservations = append(snapshot.CatalogObservations, catalog.Observation)
		for _, manifest := range catalog.Manifests {
			if manifest.Present {
				snapshot.Manifests = append(snapshot.Manifests, manifest)
			}
		}
		for _, blob := range catalog.Blobs {
			if blob.Present {
				snapshot.Blobs = append(snapshot.Blobs, blob)
			}
		}
		for _, link := range catalog.Children {
			if manifestPresent(catalog.Manifests, link.ParentDigest) && manifestPresent(catalog.Manifests, link.ChildDigest) {
				snapshot.Children = append(snapshot.Children, link)
			}
		}
		for _, link := range catalog.BlobLinks {
			if manifestPresent(catalog.Manifests, link.ManifestDigest) && blobPresent(catalog.Blobs, link.BlobDigest) {
				snapshot.BlobLinks = append(snapshot.BlobLinks, link)
			}
		}
	}
	for _, authority := range s.registryAuthorities {
		if authority.Observation.RegistryTargetID == targetID && authority.Observation.ServiceID == serviceID {
			snapshot.AuthorityObservations = append(snapshot.AuthorityObservations, authority.Observation)
			snapshot.References = append(snapshot.References, cloneReferences(authority.References)...)
		}
	}
	for _, pin := range s.registryPins {
		if pin.RegistryTargetID == targetID && pin.ServiceID == serviceID {
			snapshot.References = append(snapshot.References, pin)
		}
	}
	for _, release := range s.registryReleases {
		if release.RegistryTargetID == targetID && release.ServiceID == serviceID {
			snapshot.Releases = append(snapshot.Releases, cloneRelease(release))
		}
	}
	for _, cache := range s.registryCaches {
		if cache.RegistryTargetID == targetID && cache.ServiceID == serviceID {
			snapshot.CacheGenerations = append(snapshot.CacheGenerations, cloneCache(cache))
		}
	}
	return snapshot, nil
}

func (s *Store) SaveRegistryCleanupPlan(_ context.Context, plan domain.RegistryCleanupPlan) (domain.RegistryCleanupPlan, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveRegistryCleanupPlanLocked(plan)
}

func (s *Store) saveRegistryCleanupPlanLocked(plan domain.RegistryCleanupPlan) (domain.RegistryCleanupPlan, bool, error) {
	target, ok := s.registryTargets[plan.RegistryTargetID]
	if !ok {
		return domain.RegistryCleanupPlan{}, false, base.ErrNotFound
	}
	if target.Mode != domain.RegistryTargetManaged {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryExternalLifecycle
	}
	if plan.ID == "" || plan.ServiceID == "" || plan.PlanDigest == "" || plan.PlanDigest != base.RegistryCleanupPlanDigest(plan) {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryPolicyInvalid
	}
	current, err := s.registryLifecycleSnapshotLocked(plan.RegistryTargetID, plan.ServiceID, plan.CreatedAt)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if base.RegistrySnapshotToken(current) != plan.SnapshotToken {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistrySnapshotStale
	}
	digestKey := registryScopeKey(plan.RegistryTargetID, plan.ServiceID, plan.PlanDigest)
	if existingID, exists := s.registryPlanDigests[digestKey]; exists {
		return clonePlan(s.registryPlans[existingID]), true, nil
	}
	if _, exists := s.registryPlans[plan.ID]; exists {
		return domain.RegistryCleanupPlan{}, false, base.ErrConflict
	}
	plan = clonePlan(plan)
	s.registryPlans[plan.ID] = plan
	s.registryPlanDigests[digestKey] = plan.ID
	return clonePlan(plan), false, nil
}

func (s *Store) RegistryCleanupPlan(_ context.Context, planID string) (domain.RegistryCleanupPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return domain.RegistryCleanupPlan{}, base.ErrNotFound
	}
	return clonePlan(plan), nil
}

func (s *Store) ClaimRegistryCleanupPlan(_ context.Context, planID, owner string, now time.Time, leaseDuration time.Duration) (domain.RegistryCleanupPlan, bool, error) {
	if owner == "" || leaseDuration <= 0 {
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistryPolicyInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return domain.RegistryCleanupPlan{}, false, base.ErrNotFound
	}
	if plan.State == "executing" {
		if s.planLeasesHeldLocked(plan, owner, now) {
			return clonePlan(plan), false, nil
		}
		// An executing plan is deliberately reclaimable after every repository
		// lease has expired. Manifest DELETE and the offline GC execution key are
		// idempotent, while per-item authorization revalidates the current
		// authority token before any resumed destructive step.
		repositories := cleanupLeaseRepositories(plan)
		for _, repository := range repositories {
			lease, exists := s.registryLeases[registryScopeKey(plan.RegistryTargetID, repository)]
			if exists && lease.until.After(now) && (lease.planID != plan.ID || lease.owner != owner) {
				return domain.RegistryCleanupPlan{}, false, base.ErrConflict
			}
		}
		until := now.Add(leaseDuration)
		for _, repository := range repositories {
			s.registryLeases[registryScopeKey(plan.RegistryTargetID, repository)] = registryCleanupLease{planID: plan.ID, owner: owner, until: until}
		}
		return clonePlan(plan), true, nil
	}
	if plan.State != "preview" {
		return clonePlan(plan), false, base.ErrConflict
	}
	snapshot, err := s.registryLifecycleSnapshotLocked(plan.RegistryTargetID, plan.ServiceID, now)
	if err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	if base.RegistrySnapshotToken(snapshot) != plan.SnapshotToken {
		plan.State = "superseded"
		s.registryPlans[planID] = plan
		return domain.RegistryCleanupPlan{}, false, base.ErrRegistrySnapshotStale
	}
	repositories := cleanupLeaseRepositories(plan)
	for _, repository := range repositories {
		key := registryScopeKey(plan.RegistryTargetID, repository)
		lease, exists := s.registryLeases[key]
		if exists && lease.until.After(now) && (lease.planID != plan.ID || lease.owner != owner) {
			return domain.RegistryCleanupPlan{}, false, base.ErrConflict
		}
	}
	until := now.Add(leaseDuration)
	for _, repository := range repositories {
		s.registryLeases[registryScopeKey(plan.RegistryTargetID, repository)] = registryCleanupLease{planID: plan.ID, owner: owner, until: until}
	}
	claimed := now.UTC()
	plan.State = "executing"
	plan.ClaimedAt = &claimed
	s.registryPlans[planID] = plan
	return clonePlan(plan), true, nil
}

func (s *Store) RenewRegistryCleanupPlanLeases(_ context.Context, planID, owner string, now time.Time, leaseDuration time.Duration) error {
	if owner == "" || leaseDuration <= 0 {
		return base.ErrRegistryPolicyInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return base.ErrNotFound
	}
	if plan.State != "executing" || !s.planLeasesHeldLocked(plan, owner, now) {
		return base.ErrRegistryLeaseLost
	}
	until := now.Add(leaseDuration)
	for _, repository := range cleanupLeaseRepositories(plan) {
		key := registryScopeKey(plan.RegistryTargetID, repository)
		lease := s.registryLeases[key]
		lease.until = until
		s.registryLeases[key] = lease
	}
	return nil
}

func (s *Store) AuthorizeRegistryCleanupItem(_ context.Context, planID string, ordinal int, owner string, now time.Time) (domain.RegistryCleanupItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return domain.RegistryCleanupItem{}, base.ErrNotFound
	}
	if plan.State != "executing" || !s.itemLeaseHeldLocked(plan, ordinal, owner, now) {
		return domain.RegistryCleanupItem{}, base.ErrRegistryLeaseLost
	}
	itemIndex := cleanupItemIndex(plan, ordinal)
	if itemIndex < 0 {
		return domain.RegistryCleanupItem{}, base.ErrNotFound
	}
	item := plan.Items[itemIndex]
	if item.Disposition != domain.RegistryCleanupDelete || item.State != "planned" {
		return domain.RegistryCleanupItem{}, base.ErrConflict
	}
	if item.ResourceKind == "blob" {
		for _, other := range plan.Items {
			if other.ResourceKind != "blob" && other.Disposition == domain.RegistryCleanupDelete && other.State != "deleted" {
				return domain.RegistryCleanupItem{}, base.ErrRegistrySnapshotStale
			}
		}
	}
	snapshot, err := s.registryLifecycleSnapshotLocked(plan.RegistryTargetID, plan.ServiceID, now)
	if err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	if base.RegistryAuthorityToken(snapshot) != plan.AuthorityToken {
		return domain.RegistryCleanupItem{}, base.ErrRegistrySnapshotStale
	}
	item.State = "deleting"
	plan.Items[itemIndex] = item
	s.registryPlans[planID] = plan
	return cloneCleanupItem(item), nil
}

func (s *Store) RecordRegistryCleanupItemResult(_ context.Context, planID string, ordinal int, owner string, result domain.RegistryCleanupItemResult) error {
	if result.State != "deleted" && result.State != "skipped" && result.State != "failed" {
		return base.ErrRegistryPolicyInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return base.ErrNotFound
	}
	if plan.State != "executing" || !s.itemLeaseHeldLocked(plan, ordinal, owner, result.ObservedAt) {
		return base.ErrRegistryLeaseLost
	}
	itemIndex := cleanupItemIndex(plan, ordinal)
	if itemIndex < 0 {
		return base.ErrNotFound
	}
	item := plan.Items[itemIndex]
	if item.State != "deleting" {
		if item.State == result.State && item.ProviderMessage == result.ProviderMessage {
			return nil
		}
		return base.ErrConflict
	}
	item.State = result.State
	item.ProviderMessage = result.ProviderMessage
	item.UpdatedAt = result.ObservedAt.UTC()
	plan.Items[itemIndex] = item
	if result.State == "deleted" {
		s.applyCleanupDeletionLocked(plan, item, result.ObservedAt.UTC())
	}
	snapshot, err := s.registryLifecycleSnapshotLocked(plan.RegistryTargetID, plan.ServiceID, result.ObservedAt)
	if err != nil {
		return err
	}
	plan.AuthorityToken = base.RegistryAuthorityToken(snapshot)
	s.registryPlans[planID] = plan
	return nil
}

func (s *Store) FinishRegistryCleanupPlan(_ context.Context, planID, owner string, succeeded bool, failure string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.registryPlans[planID]
	if !ok {
		return base.ErrNotFound
	}
	desired := "failed"
	if succeeded {
		desired = "succeeded"
	}
	if plan.State == desired {
		return nil
	}
	if plan.State != "executing" || !s.planLeasesHeldLocked(plan, owner, now) {
		return base.ErrRegistryLeaseLost
	}
	if succeeded {
		for _, item := range plan.Items {
			if item.Disposition == domain.RegistryCleanupDelete && item.State != "deleted" {
				return base.ErrConflict
			}
		}
		failure = ""
	} else if failure == "" {
		return base.ErrRegistryPolicyInvalid
	}
	completed := now.UTC()
	plan.State = desired
	plan.Failure = failure
	plan.CompletedAt = &completed
	s.registryPlans[planID] = plan
	for _, repository := range cleanupLeaseRepositories(plan) {
		key := registryScopeKey(plan.RegistryTargetID, repository)
		lease := s.registryLeases[key]
		if lease.planID == plan.ID && lease.owner == owner {
			delete(s.registryLeases, key)
		}
	}
	return nil
}

func (s *Store) applyCleanupDeletionLocked(plan domain.RegistryCleanupPlan, item domain.RegistryCleanupItem, observedAt time.Time) {
	if item.ResourceKind == "blob" {
		for key, catalog := range s.registryCatalogs {
			if catalog.Observation.RegistryTargetID != plan.RegistryTargetID {
				continue
			}
			for index := range catalog.Blobs {
				if catalog.Blobs[index].Digest == item.Digest && catalog.Blobs[index].Present {
					catalog.Blobs[index].Present = false
					catalog.Blobs[index].DeletedAt = &observedAt
				}
			}
			s.registryCatalogs[key] = catalog
		}
		return
	}
	catalogKey := registryScopeKey(plan.RegistryTargetID, item.Repository)
	catalog := s.registryCatalogs[catalogKey]
	for index := range catalog.Manifests {
		if catalog.Manifests[index].Digest == item.Digest && catalog.Manifests[index].Present {
			catalog.Manifests[index].Present = false
			catalog.Manifests[index].DeletedAt = &observedAt
		}
	}
	s.registryCatalogs[catalogKey] = catalog
	for releaseID, release := range s.registryReleases {
		if release.RegistryTargetID == plan.RegistryTargetID && release.Repository == item.Repository && release.RootDigest == item.Digest {
			release.Availability = domain.RegistryArtifactExpired
			release.AvailabilityObservedAt = &observedAt
			s.registryReleases[releaseID] = release
		}
	}
	for cacheID, cache := range s.registryCaches {
		if cache.RegistryTargetID == plan.RegistryTargetID && cache.Repository == item.Repository && cache.RootDigest == item.Digest {
			cache.State = "deleted"
			s.registryCaches[cacheID] = cache
		}
	}
}

func cleanupLeaseRepositories(plan domain.RegistryCleanupPlan) []string {
	set := map[string]struct{}{"*": {}}
	for _, item := range plan.Items {
		if item.Disposition == domain.RegistryCleanupDelete {
			set[item.Repository] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for repository := range set {
		out = append(out, repository)
	}
	sort.Strings(out)
	return out
}

func (s *Store) planLeasesHeldLocked(plan domain.RegistryCleanupPlan, owner string, now time.Time) bool {
	for _, repository := range cleanupLeaseRepositories(plan) {
		lease := s.registryLeases[registryScopeKey(plan.RegistryTargetID, repository)]
		if lease.planID != plan.ID || lease.owner != owner || !lease.until.After(now) {
			return false
		}
	}
	return true
}

func (s *Store) itemLeaseHeldLocked(plan domain.RegistryCleanupPlan, ordinal int, owner string, now time.Time) bool {
	index := cleanupItemIndex(plan, ordinal)
	if index < 0 {
		return false
	}
	repositories := []string{"*"}
	if plan.Items[index].Repository != "*" {
		repositories = append(repositories, plan.Items[index].Repository)
	}
	for _, repository := range repositories {
		lease := s.registryLeases[registryScopeKey(plan.RegistryTargetID, repository)]
		if lease.planID != plan.ID || lease.owner != owner || !lease.until.After(now) {
			return false
		}
	}
	return true
}

func cleanupItemIndex(plan domain.RegistryCleanupPlan, ordinal int) int {
	for index := range plan.Items {
		if plan.Items[index].Ordinal == ordinal {
			return index
		}
	}
	return -1
}

func manifestPresent(manifests []domain.RegistryManifest, digest string) bool {
	for _, manifest := range manifests {
		if manifest.Digest == digest && manifest.Present {
			return true
		}
	}
	return false
}

func blobPresent(blobs []domain.RegistryBlob, digest string) bool {
	for _, blob := range blobs {
		if blob.Digest == digest && blob.Present {
			return true
		}
	}
	return false
}

func cloneCatalog(in domain.RegistryCatalogSnapshot) domain.RegistryCatalogSnapshot {
	in.Manifests = cloneManifests(in.Manifests)
	in.Blobs = cloneBlobs(in.Blobs)
	in.Children = append([]domain.RegistryManifestLink(nil), in.Children...)
	in.BlobLinks = append([]domain.RegistryManifestBlobLink(nil), in.BlobLinks...)
	return in
}

func cloneManifests(in []domain.RegistryManifest) []domain.RegistryManifest {
	out := append([]domain.RegistryManifest(nil), in...)
	for index := range out {
		if out[index].DeletedAt != nil {
			value := *out[index].DeletedAt
			out[index].DeletedAt = &value
		}
	}
	return out
}

func cloneBlobs(in []domain.RegistryBlob) []domain.RegistryBlob {
	out := append([]domain.RegistryBlob(nil), in...)
	for index := range out {
		if out[index].DeletedAt != nil {
			value := *out[index].DeletedAt
			out[index].DeletedAt = &value
		}
	}
	return out
}

func cloneInventory(in domain.RegistryInventoryObservation) domain.RegistryInventoryObservation {
	in.Repositories = append([]string(nil), in.Repositories...)
	return in
}

func cloneReferences(in []domain.RegistryArtifactReference) []domain.RegistryArtifactReference {
	return append([]domain.RegistryArtifactReference(nil), in...)
}

func cloneProtectionSnapshot(in domain.RegistryProtectionSnapshot) domain.RegistryProtectionSnapshot {
	in.References = cloneReferences(in.References)
	return in
}

func cloneRelease(in domain.RegistryRelease) domain.RegistryRelease {
	if in.SucceededAt != nil {
		value := *in.SucceededAt
		in.SucceededAt = &value
	}
	if in.AvailabilityObservedAt != nil {
		value := *in.AvailabilityObservedAt
		in.AvailabilityObservedAt = &value
	}
	return in
}

func cloneCache(in domain.RegistryCacheGeneration) domain.RegistryCacheGeneration {
	if in.CompletedAt != nil {
		value := *in.CompletedAt
		in.CompletedAt = &value
	}
	return in
}

func cloneCleanupItem(in domain.RegistryCleanupItem) domain.RegistryCleanupItem {
	in.Reasons = append([]string(nil), in.Reasons...)
	return in
}

func clonePlan(in domain.RegistryCleanupPlan) domain.RegistryCleanupPlan {
	in.Inventory = cloneInventory(in.Inventory)
	in.Catalogs = append([]domain.RegistryCatalogObservation(nil), in.Catalogs...)
	in.Authorities = append([]domain.RegistryAuthorityObservation(nil), in.Authorities...)
	in.Items = append([]domain.RegistryCleanupItem(nil), in.Items...)
	for index := range in.Items {
		in.Items[index] = cloneCleanupItem(in.Items[index])
	}
	if in.ClaimedAt != nil {
		value := *in.ClaimedAt
		in.ClaimedAt = &value
	}
	if in.CompletedAt != nil {
		value := *in.CompletedAt
		in.CompletedAt = &value
	}
	return in
}

var _ base.RegistryStore = (*Store)(nil)
