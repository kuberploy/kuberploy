package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/store"
)

const (
	ReasonCurrentGitIntent      = "current-git-intent"
	ReasonObservedRunning       = "observed-running"
	ReasonPin                   = "pin"
	ReasonActiveOperation       = "active-operation"
	ReasonRetainedSuccessWindow = "retained-success-window"
	ReasonMinimumSafetyAge      = "minimum-safety-age"
	ReasonCacheActive           = "cache-active"
	ReasonCacheRetained         = "cache-retained-generation"
	ReasonCacheRecentlyUsed     = "cache-recently-used"
	ReasonOutsideServiceScope   = "outside-service-scope"
	ReasonReachableManifest     = "reachable-from-protected-manifest"
	ReasonReachableBlob         = "reachable-from-protected-manifest"
	ReasonRetentionEligible     = "retention-eligible"
	ReasonCacheExpired          = "cache-unused-expired"
	ReasonCacheQuota            = "cache-byte-quota"
	ReasonUnreachableBlob       = "globally-unreachable"
)

type Service struct {
	store             store.RegistryStore
	protection        ProtectionRefresher
	now               func() time.Time
	newID             func() string
	maxObservationAge time.Duration
}

type Option func(*Service)

func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

func WithIDGenerator(newID func() string) Option {
	return func(s *Service) { s.newID = newID }
}

// WithMaxObservationAge sets the maximum age of inventory, catalog, Git,
// runtime, and operation checkpoints. Zero disables the age check while still
// requiring each checkpoint to exist and be complete.
func WithMaxObservationAge(max time.Duration) Option {
	return func(s *Service) { s.maxObservationAge = max }
}

func WithProtectionRefresher(refresher ProtectionRefresher) Option {
	return func(s *Service) { s.protection = refresher }
}

func NewService(repository store.RegistryStore, options ...Option) *Service {
	s := &Service{
		store:             repository,
		now:               func() time.Time { return time.Now().UTC() },
		newID:             id.New,
		maxObservationAge: 15 * time.Minute,
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Preview(ctx context.Context, targetID, serviceID string) (domain.RegistryCleanupPlan, error) {
	now := s.now().UTC()
	if s.protection != nil {
		if err := s.protection.RefreshRegistryProtection(ctx, targetID, serviceID, now, true); err != nil {
			return domain.RegistryCleanupPlan{}, err
		}
	}
	snapshot, err := s.store.RegistryLifecycleSnapshot(ctx, targetID, serviceID, now)
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	plan, err := BuildCleanupPlan(snapshot, now, s.maxObservationAge)
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	plan.ID = s.newID()
	returnPlan, _, err := s.store.SaveRegistryCleanupPlan(ctx, plan)
	return returnPlan, err
}

func (s *Service) Claim(ctx context.Context, planID, owner string, lease time.Duration) (domain.RegistryCleanupPlan, bool, error) {
	if err := s.refreshPlanProtection(ctx, planID); err != nil {
		return domain.RegistryCleanupPlan{}, false, err
	}
	return s.store.ClaimRegistryCleanupPlan(ctx, planID, owner, s.now().UTC(), lease)
}

func (s *Service) Renew(ctx context.Context, planID, owner string, lease time.Duration) error {
	return s.store.RenewRegistryCleanupPlanLeases(ctx, planID, owner, s.now().UTC(), lease)
}

func (s *Service) AuthorizeItem(ctx context.Context, planID string, ordinal int, owner string) (domain.RegistryCleanupItem, error) {
	if err := s.refreshPlanProtection(ctx, planID); err != nil {
		return domain.RegistryCleanupItem{}, err
	}
	return s.store.AuthorizeRegistryCleanupItem(ctx, planID, ordinal, owner, s.now().UTC())
}

func (s *Service) refreshPlanProtection(ctx context.Context, planID string) error {
	if s.protection == nil {
		return nil
	}
	plan, err := s.store.RegistryCleanupPlan(ctx, planID)
	if err != nil {
		return err
	}
	return s.protection.RefreshRegistryProtection(ctx, plan.RegistryTargetID, plan.ServiceID, s.now().UTC(), false)
}

func (s *Service) RecordItemResult(ctx context.Context, planID string, ordinal int, owner string, result domain.RegistryCleanupItemResult) error {
	if result.ObservedAt.IsZero() {
		result.ObservedAt = s.now().UTC()
	}
	return s.store.RecordRegistryCleanupItemResult(ctx, planID, ordinal, owner, result)
}

func (s *Service) Finish(ctx context.Context, planID, owner string, succeeded bool, failure string) error {
	return s.store.FinishRegistryCleanupPlan(ctx, planID, owner, succeeded, failure, s.now().UTC())
}

// DefaultPolicy explicitly materializes every managed default. Callers should
// persist the result rather than relying on UI or database implicit defaults.
func DefaultPolicy(targetID, serviceID, repository string, now time.Time) domain.ServiceRegistryPolicy {
	return domain.ServiceRegistryPolicy{
		RegistryTargetID:     targetID,
		ServiceID:            serviceID,
		Repository:           repository,
		KeepLastSuccessful:   domain.DefaultKeepLastSuccessful,
		MinimumSafetyAge:     domain.DefaultRegistrySafetyAge,
		CacheKeepGenerations: domain.DefaultCacheKeepGenerations,
		CacheUnusedExpiry:    domain.DefaultCacheUnusedExpiry,
		CacheByteQuota:       domain.DefaultCacheByteQuota,
		CreatedAt:            now.UTC(),
		UpdatedAt:            now.UTC(),
	}
}

func ValidateTarget(target domain.RegistryTarget) error {
	if strings.TrimSpace(target.ID) == "" || strings.TrimSpace(target.Name) == "" ||
		strings.TrimSpace(target.Endpoint) == "" || strings.TrimSpace(target.RepositoryPrefix) == "" {
		return fmt.Errorf("%w: registry target identity, name, endpoint, and repository prefix are required", store.ErrRegistryPolicyInvalid)
	}
	if target.Mode != domain.RegistryTargetManaged && target.Mode != domain.RegistryTargetExternal {
		return fmt.Errorf("%w: unsupported registry mode %q", store.ErrRegistryPolicyInvalid, target.Mode)
	}
	if !validSafeIdentity(target.ID) || len(target.Name) > 100 || !utf8.ValidString(target.Name) ||
		strings.TrimSpace(target.Name) != target.Name || strings.IndexFunc(target.Name, unicode.IsControl) >= 0 ||
		len(target.Endpoint) > 2048 || !validRepository(target.RepositoryPrefix) {
		return fmt.Errorf("%w: registry target metadata is not canonical", store.ErrRegistryPolicyInvalid)
	}
	// The independently trusted runtime profile decides whether an exact
	// private-network origin may use plain HTTP. Persisted metadata still has to
	// be a fixed canonical origin with no credentials, path, query, or fragment.
	if _, err := distributionBaseURL(target.Endpoint, true); err != nil {
		return fmt.Errorf("%w: registry endpoint is invalid", store.ErrRegistryPolicyInvalid)
	}
	for _, reference := range []string{target.PullCredentialRef, target.PushCredentialRef, target.CacheCredentialRef} {
		if reference != "" && !registryCredentialRefRE.MatchString(reference) {
			return fmt.Errorf("%w: registry credential reference is invalid", store.ErrRegistryPolicyInvalid)
		}
	}
	if target.PullCredentialRef != "" &&
		(target.PullCredentialRef == target.PushCredentialRef || target.PullCredentialRef == target.CacheCredentialRef) {
		return fmt.Errorf("%w: runtime pull credentials must be isolated from build credentials", store.ErrRegistryPolicyInvalid)
	}
	if target.PushCredentialRef != "" && target.PushCredentialRef == target.CacheCredentialRef {
		return fmt.Errorf("%w: release push and cache credentials must be isolated", store.ErrRegistryPolicyInvalid)
	}
	return nil
}

func NormalizePolicy(policy domain.ServiceRegistryPolicy, now time.Time) domain.ServiceRegistryPolicy {
	if policy.KeepLastSuccessful == 0 {
		policy.KeepLastSuccessful = domain.DefaultKeepLastSuccessful
	}
	if policy.MinimumSafetyAge == 0 {
		policy.MinimumSafetyAge = domain.DefaultRegistrySafetyAge
	}
	if policy.CacheKeepGenerations == 0 {
		policy.CacheKeepGenerations = domain.DefaultCacheKeepGenerations
	}
	if policy.CacheUnusedExpiry == 0 {
		policy.CacheUnusedExpiry = domain.DefaultCacheUnusedExpiry
	}
	if policy.CacheByteQuota == 0 {
		policy.CacheByteQuota = domain.DefaultCacheByteQuota
	}
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = now.UTC()
	}
	policy.UpdatedAt = now.UTC()
	return policy
}

func ValidatePolicy(policy domain.ServiceRegistryPolicy) error {
	if !validSafeIdentity(policy.RegistryTargetID) || !validSafeIdentity(policy.ServiceID) || !validRepository(policy.Repository) {
		return fmt.Errorf("%w: target, service, and repository are required", store.ErrRegistryPolicyInvalid)
	}
	if policy.KeepLastSuccessful < domain.MinimumKeepLastSuccessful || policy.KeepLastSuccessful > domain.MaximumKeepLastSuccessful {
		return fmt.Errorf("%w: keepLastSuccessful must be between %d and %d", store.ErrRegistryPolicyInvalid, domain.MinimumKeepLastSuccessful, domain.MaximumKeepLastSuccessful)
	}
	if policy.MinimumSafetyAge < time.Minute || policy.CacheUnusedExpiry < time.Minute {
		return fmt.Errorf("%w: lifecycle ages must be at least one minute", store.ErrRegistryPolicyInvalid)
	}
	if policy.CacheKeepGenerations < 1 || policy.CacheKeepGenerations > 20 || policy.CacheByteQuota <= 0 {
		return fmt.Errorf("%w: invalid cache generation or quota policy", store.ErrRegistryPolicyInvalid)
	}
	return nil
}

// ValidatePolicyForTarget closes the repository-ownership boundary used by
// builds, private pulls, release retention, and managed cleanup.
func ValidatePolicyForTarget(target domain.RegistryTarget, policy domain.ServiceRegistryPolicy) error {
	if ValidateTarget(target) != nil || ValidatePolicy(policy) != nil || policy.RegistryTargetID != target.ID ||
		!repositoryInTarget(target, policy.Repository) {
		return fmt.Errorf("%w: repository is outside the registry target", store.ErrRegistryPolicyInvalid)
	}
	return nil
}

func ValidateCatalog(snapshot domain.RegistryCatalogSnapshot) error {
	observation := snapshot.Observation
	if observation.RegistryTargetID == "" || observation.Repository == "" || observation.Revision <= 0 || observation.ObservedAt.IsZero() {
		return fmt.Errorf("%w: invalid catalog observation", store.ErrRegistryGraphInvalid)
	}
	if !observation.Complete {
		if len(snapshot.Manifests) != 0 || len(snapshot.Blobs) != 0 || len(snapshot.Children) != 0 || len(snapshot.BlobLinks) != 0 {
			return fmt.Errorf("%w: incomplete observations cannot replace catalog nodes", store.ErrRegistryGraphInvalid)
		}
		return nil
	}
	if observation.ManifestCount != len(snapshot.Manifests) || observation.BlobCount != len(snapshot.Blobs) {
		return fmt.Errorf("%w: catalog counts do not match nodes", store.ErrRegistryGraphInvalid)
	}
	manifests := make(map[string]domain.RegistryManifest, len(snapshot.Manifests))
	for _, manifest := range snapshot.Manifests {
		if manifest.RegistryTargetID != observation.RegistryTargetID || manifest.Repository != observation.Repository || !validDigest(manifest.Digest) || manifest.SizeBytes < 0 {
			return fmt.Errorf("%w: invalid manifest %q", store.ErrRegistryGraphInvalid, manifest.Digest)
		}
		if manifest.Kind != domain.RegistryManifestIndex && manifest.Kind != domain.RegistryManifestImage {
			return fmt.Errorf("%w: invalid manifest kind", store.ErrRegistryGraphInvalid)
		}
		if _, exists := manifests[manifest.Digest]; exists {
			return fmt.Errorf("%w: duplicate manifest %s", store.ErrRegistryGraphInvalid, manifest.Digest)
		}
		manifests[manifest.Digest] = manifest
	}
	blobs := make(map[string]struct{}, len(snapshot.Blobs))
	for _, blob := range snapshot.Blobs {
		if blob.RegistryTargetID != observation.RegistryTargetID || blob.Repository != observation.Repository || !validDigest(blob.Digest) || blob.SizeBytes < 0 {
			return fmt.Errorf("%w: invalid blob %q", store.ErrRegistryGraphInvalid, blob.Digest)
		}
		if _, exists := blobs[blob.Digest]; exists {
			return fmt.Errorf("%w: duplicate blob %s", store.ErrRegistryGraphInvalid, blob.Digest)
		}
		blobs[blob.Digest] = struct{}{}
	}
	children := make(map[string][]string)
	seenChildren := make(map[string]struct{})
	for _, link := range snapshot.Children {
		parent, parentOK := manifests[link.ParentDigest]
		_, childOK := manifests[link.ChildDigest]
		key := link.ParentDigest + "\x00" + link.ChildDigest
		if link.Repository != observation.Repository || !parentOK || !childOK || parent.Kind != domain.RegistryManifestIndex || link.ParentDigest == link.ChildDigest {
			return fmt.Errorf("%w: invalid manifest child edge", store.ErrRegistryGraphInvalid)
		}
		if _, exists := seenChildren[key]; exists {
			return fmt.Errorf("%w: duplicate manifest child edge", store.ErrRegistryGraphInvalid)
		}
		seenChildren[key] = struct{}{}
		children[link.ParentDigest] = append(children[link.ParentDigest], link.ChildDigest)
	}
	if cyclic(children) {
		return fmt.Errorf("%w: manifest graph contains a cycle", store.ErrRegistryGraphInvalid)
	}
	seenBlobs := make(map[string]struct{})
	for _, link := range snapshot.BlobLinks {
		_, manifestOK := manifests[link.ManifestDigest]
		_, blobOK := blobs[link.BlobDigest]
		key := link.ManifestDigest + "\x00" + link.BlobDigest
		if link.Repository != observation.Repository || !manifestOK || !blobOK {
			return fmt.Errorf("%w: invalid manifest blob edge", store.ErrRegistryGraphInvalid)
		}
		if _, exists := seenBlobs[key]; exists {
			return fmt.Errorf("%w: duplicate manifest blob edge", store.ErrRegistryGraphInvalid)
		}
		seenBlobs[key] = struct{}{}
	}
	return nil
}

// BuildCleanupPlan performs no writes. The same complete snapshot always
// yields the same PlanDigest and ordered items; only plan ID and persistence
// timestamps are assigned by Service.Preview and the store.
func BuildCleanupPlan(snapshot domain.RegistryLifecycleSnapshot, now time.Time, maxObservationAge time.Duration) (domain.RegistryCleanupPlan, error) {
	if snapshot.Target.Mode == domain.RegistryTargetExternal {
		return domain.RegistryCleanupPlan{}, store.ErrRegistryExternalLifecycle
	}
	if snapshot.Target.Mode != domain.RegistryTargetManaged {
		return domain.RegistryCleanupPlan{}, fmt.Errorf("%w: unknown target mode", store.ErrRegistryPolicyInvalid)
	}
	if err := ValidatePolicy(snapshot.Policy); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	if snapshot.Target.ID != snapshot.Policy.RegistryTargetID {
		return domain.RegistryCleanupPlan{}, fmt.Errorf("%w: target and policy mismatch", store.ErrRegistryPolicyInvalid)
	}
	if err := validateCompleteness(snapshot, now, maxObservationAge); err != nil {
		return domain.RegistryCleanupPlan{}, err
	}
	graph, err := newLifecycleGraph(snapshot)
	if err != nil {
		return domain.RegistryCleanupPlan{}, err
	}

	protected := make(map[string]map[string]struct{})
	deleteReasons := make(map[string]map[string]struct{})
	protect := func(key, reason string) { addReason(protected, key, reason) }
	markDelete := func(key, reason string) { addReason(deleteReasons, key, reason) }

	for _, reference := range snapshot.References {
		if reference.RegistryTargetID != snapshot.Target.ID || reference.ServiceID != snapshot.Policy.ServiceID {
			return domain.RegistryCleanupPlan{}, fmt.Errorf("%w: reference scope mismatch", store.ErrRegistryGraphInvalid)
		}
		reason := map[domain.RegistryArtifactReferenceKind]string{
			domain.RegistryReferenceCurrentGitIntent: ReasonCurrentGitIntent,
			domain.RegistryReferenceObservedRunning:  ReasonObservedRunning,
			domain.RegistryReferencePin:              ReasonPin,
			domain.RegistryReferenceActiveOperation:  ReasonActiveOperation,
		}[reference.Kind]
		key := nodeKey(reference.Repository, reference.Digest)
		if reason == "" || graph.manifests[key].Digest == "" {
			return domain.RegistryCleanupPlan{}, fmt.Errorf("%w: protected reference is absent from catalog", store.ErrRegistryObservationIncomplete)
		}
		protect(key, reason)
	}

	protectSuccessfulReleases(snapshot, graph, protect)
	cacheDecisions, cacheBefore, cacheAfter := decideCaches(snapshot, now)
	for key, decision := range cacheDecisions {
		if decision.protect {
			for _, reason := range decision.reasons {
				protect(key, reason)
			}
		} else {
			for _, reason := range decision.reasons {
				markDelete(key, reason)
			}
		}
	}

	cutoff := now.Add(-snapshot.Policy.MinimumSafetyAge)
	serviceRepositories := map[string]string{snapshot.Policy.Repository: "release-manifest"}
	for _, cache := range snapshot.CacheGenerations {
		if cache.ServiceID == snapshot.Policy.ServiceID {
			serviceRepositories[cache.Repository] = "cache-manifest"
		}
	}
	for key, manifest := range graph.manifests {
		kind, owned := serviceRepositories[manifest.Repository]
		_ = kind
		if !owned {
			protect(key, ReasonOutsideServiceScope)
			continue
		}
		if !manifest.FirstObservedAt.Before(cutoff) {
			protect(key, ReasonMinimumSafetyAge)
		}
	}

	// Reachability, rather than manifest age, protects platform manifests under
	// a retained multi-platform index.
	queue := sortedKeys(protected)
	seen := make(map[string]struct{}, len(queue))
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		for _, child := range graph.children[key] {
			if _, already := protected[child]; !already {
				protect(child, ReasonReachableManifest)
			}
			queue = append(queue, child)
		}
		sort.Strings(queue)
	}

	for key, manifest := range graph.manifests {
		if _, isProtected := protected[key]; isProtected {
			continue
		}
		if _, owned := serviceRepositories[manifest.Repository]; !owned {
			return domain.RegistryCleanupPlan{}, fmt.Errorf("%w: outside-scope manifest became eligible", store.ErrRegistryGraphInvalid)
		}
		if len(deleteReasons[key]) == 0 {
			markDelete(key, ReasonRetentionEligible)
		}
	}

	items := make([]domain.RegistryCleanupItem, 0, len(graph.manifests)+len(graph.blobByDigest))
	for key, manifest := range graph.manifests {
		resourceKind := serviceRepositories[manifest.Repository]
		if resourceKind == "" {
			resourceKind = "release-manifest"
		}
		if reasons, ok := protected[key]; ok {
			items = append(items, cleanupItem(manifest.Repository, resourceKind, manifest.Digest, domain.RegistryCleanupProtect, "none", manifest.SizeBytes, reasons))
		} else {
			items = append(items, cleanupItem(manifest.Repository, resourceKind, manifest.Digest, domain.RegistryCleanupDelete, "delete-manifest", manifest.SizeBytes, deleteReasons[key]))
		}
	}

	reachableBlobs := make(map[string]struct{})
	for key := range protected {
		for _, digest := range graph.manifestBlobs[key] {
			reachableBlobs[digest] = struct{}{}
		}
	}
	for digest, blobs := range graph.blobByDigest {
		reasons := make(map[string]struct{})
		if _, reachable := reachableBlobs[digest]; reachable {
			reasons[ReasonReachableBlob] = struct{}{}
		}
		for _, blob := range blobs {
			if !blob.FirstObservedAt.Before(cutoff) {
				reasons[ReasonMinimumSafetyAge] = struct{}{}
			}
		}
		size := maxBlobSize(blobs)
		if len(reasons) != 0 {
			items = append(items, cleanupItem("*", "blob", digest, domain.RegistryCleanupProtect, "none", size, reasons))
		} else {
			items = append(items, cleanupItem("*", "blob", digest, domain.RegistryCleanupDelete, "garbage-collect-blob", size, map[string]struct{}{ReasonUnreachableBlob: {}}))
		}
	}

	sortCleanupItems(items)
	summary := summarize(items, cacheBefore, cacheAfter, snapshot.Policy.CacheByteQuota)
	plan := domain.RegistryCleanupPlan{
		RegistryTargetID: snapshot.Target.ID,
		ServiceID:        snapshot.Policy.ServiceID,
		SnapshotToken:    store.RegistrySnapshotToken(snapshot),
		AuthorityToken:   store.RegistryAuthorityToken(snapshot),
		State:            "preview",
		Policy:           snapshot.Policy,
		Inventory:        snapshot.Inventory,
		Catalogs:         append([]domain.RegistryCatalogObservation(nil), snapshot.CatalogObservations...),
		Authorities:      append([]domain.RegistryAuthorityObservation(nil), snapshot.AuthorityObservations...),
		Summary:          summary,
		Items:            items,
		CreatedAt:        now.UTC(),
	}
	plan.PlanDigest = store.RegistryCleanupPlanDigest(plan)
	return plan, nil
}

func ArtifactProblemCode(target domain.RegistryTarget, release domain.RegistryRelease, manifestPresent bool) string {
	if manifestPresent && release.Availability == domain.RegistryArtifactPresent {
		return ""
	}
	if target.Mode == domain.RegistryTargetManaged {
		return domain.ProblemArtifactExpired
	}
	return domain.ProblemArtifactMissing
}

type lifecycleGraph struct {
	manifests     map[string]domain.RegistryManifest
	blobByDigest  map[string][]domain.RegistryBlob
	children      map[string][]string
	manifestBlobs map[string][]string
}

func newLifecycleGraph(snapshot domain.RegistryLifecycleSnapshot) (lifecycleGraph, error) {
	graph := lifecycleGraph{
		manifests:     make(map[string]domain.RegistryManifest),
		blobByDigest:  make(map[string][]domain.RegistryBlob),
		children:      make(map[string][]string),
		manifestBlobs: make(map[string][]string),
	}
	repositories := stringSet(snapshot.Inventory.Repositories)
	countsM, countsB := make(map[string]int), make(map[string]int)
	for _, manifest := range snapshot.Manifests {
		if !manifest.Present {
			continue
		}
		key := nodeKey(manifest.Repository, manifest.Digest)
		if manifest.RegistryTargetID != snapshot.Target.ID || !validDigest(manifest.Digest) ||
			!repositories[manifest.Repository] || graph.manifests[key].Digest != "" ||
			manifest.SizeBytes < 0 || manifest.FirstObservedAt.IsZero() ||
			manifest.LastObservedAt.IsZero() || manifest.LastObservationRevision <= 0 ||
			(manifest.Kind != domain.RegistryManifestIndex && manifest.Kind != domain.RegistryManifestImage) {
			return graph, fmt.Errorf("%w: invalid or duplicate manifest node", store.ErrRegistryGraphInvalid)
		}
		graph.manifests[key] = manifest
		countsM[manifest.Repository]++
	}
	blobNodes := make(map[string]struct{})
	for _, blob := range snapshot.Blobs {
		if !blob.Present {
			continue
		}
		key := nodeKey(blob.Repository, blob.Digest)
		if blob.RegistryTargetID != snapshot.Target.ID || !validDigest(blob.Digest) ||
			!repositories[blob.Repository] || blob.SizeBytes < 0 ||
			blob.FirstObservedAt.IsZero() || blob.LastObservedAt.IsZero() ||
			blob.LastObservationRevision <= 0 {
			return graph, fmt.Errorf("%w: invalid blob node", store.ErrRegistryGraphInvalid)
		}
		if _, duplicate := blobNodes[key]; duplicate {
			return graph, fmt.Errorf("%w: duplicate blob node", store.ErrRegistryGraphInvalid)
		}
		blobNodes[key] = struct{}{}
		graph.blobByDigest[blob.Digest] = append(graph.blobByDigest[blob.Digest], blob)
		countsB[blob.Repository]++
	}
	for _, link := range snapshot.Children {
		parent := nodeKey(link.Repository, link.ParentDigest)
		child := nodeKey(link.Repository, link.ChildDigest)
		if graph.manifests[parent].Digest == "" || graph.manifests[child].Digest == "" || graph.manifests[parent].Kind != domain.RegistryManifestIndex {
			return graph, fmt.Errorf("%w: dangling manifest child edge", store.ErrRegistryGraphInvalid)
		}
		graph.children[parent] = append(graph.children[parent], child)
	}
	if cyclic(graph.children) {
		return graph, fmt.Errorf("%w: cyclic manifest graph", store.ErrRegistryGraphInvalid)
	}
	for _, link := range snapshot.BlobLinks {
		manifest := nodeKey(link.Repository, link.ManifestDigest)
		blob := nodeKey(link.Repository, link.BlobDigest)
		if graph.manifests[manifest].Digest == "" {
			return graph, fmt.Errorf("%w: dangling manifest blob edge", store.ErrRegistryGraphInvalid)
		}
		if _, ok := blobNodes[blob]; !ok {
			return graph, fmt.Errorf("%w: dangling blob node", store.ErrRegistryGraphInvalid)
		}
		graph.manifestBlobs[manifest] = append(graph.manifestBlobs[manifest], link.BlobDigest)
	}
	for _, release := range snapshot.Releases {
		if release.RegistryTargetID != snapshot.Target.ID || release.ServiceID != snapshot.Policy.ServiceID || release.Repository != snapshot.Policy.Repository {
			return graph, fmt.Errorf("%w: release scope mismatch", store.ErrRegistryGraphInvalid)
		}
		if release.Availability == domain.RegistryArtifactPresent && graph.manifests[nodeKey(release.Repository, release.RootDigest)].Digest == "" {
			return graph, fmt.Errorf("%w: present release root is absent", store.ErrRegistryObservationIncomplete)
		}
	}
	for _, cache := range snapshot.CacheGenerations {
		if cache.State != "deleted" && cache.State != "missing" && graph.manifests[nodeKey(cache.Repository, cache.RootDigest)].Digest == "" {
			return graph, fmt.Errorf("%w: live cache root is absent", store.ErrRegistryObservationIncomplete)
		}
	}
	for repository := range repositories {
		observation, ok := latestCatalog(snapshot.CatalogObservations, repository)
		if !ok || !observation.Complete || observation.ManifestCount != countsM[repository] || observation.BlobCount != countsB[repository] {
			return graph, fmt.Errorf("%w: catalog counts for %s are not closed", store.ErrRegistryObservationIncomplete, repository)
		}
	}
	return graph, nil
}

func validateCompleteness(snapshot domain.RegistryLifecycleSnapshot, now time.Time, maxAge time.Duration) error {
	if !snapshot.Inventory.Complete || snapshot.Inventory.RegistryTargetID != snapshot.Target.ID || snapshot.Inventory.Revision == "" {
		return fmt.Errorf("%w: registry inventory", store.ErrRegistryObservationIncomplete)
	}
	if maxAge > 0 && snapshot.Inventory.ObservedAt.Before(now.Add(-maxAge)) {
		return fmt.Errorf("%w: registry inventory is stale", store.ErrRegistryObservationIncomplete)
	}
	repositories := stringSet(snapshot.Inventory.Repositories)
	if len(repositories) != len(snapshot.Inventory.Repositories) || !repositories[snapshot.Policy.Repository] {
		return fmt.Errorf("%w: repository inventory is incomplete", store.ErrRegistryObservationIncomplete)
	}
	for _, cache := range snapshot.CacheGenerations {
		if cache.RegistryTargetID != snapshot.Target.ID || cache.ServiceID != snapshot.Policy.ServiceID || !repositories[cache.Repository] {
			return fmt.Errorf("%w: cache scope is not inventoried", store.ErrRegistryObservationIncomplete)
		}
	}
	for repository := range repositories {
		observation, ok := latestCatalog(snapshot.CatalogObservations, repository)
		if !ok || !observation.Complete || observation.RegistryTargetID != snapshot.Target.ID {
			return fmt.Errorf("%w: catalog %s", store.ErrRegistryObservationIncomplete, repository)
		}
		if maxAge > 0 && observation.ObservedAt.Before(now.Add(-maxAge)) {
			return fmt.Errorf("%w: catalog %s is stale", store.ErrRegistryObservationIncomplete, repository)
		}
	}
	required := map[domain.RegistryAuthority]bool{
		domain.RegistryAuthorityGitIntent:  false,
		domain.RegistryAuthorityRuntime:    false,
		domain.RegistryAuthorityOperations: false,
	}
	for _, observation := range snapshot.AuthorityObservations {
		if observation.RegistryTargetID != snapshot.Target.ID || observation.ServiceID != snapshot.Policy.ServiceID {
			return fmt.Errorf("%w: authority scope mismatch", store.ErrRegistryObservationIncomplete)
		}
		if _, exists := required[observation.Authority]; !exists || required[observation.Authority] || !observation.Complete || observation.Revision == "" {
			return fmt.Errorf("%w: authority %s", store.ErrRegistryObservationIncomplete, observation.Authority)
		}
		if maxAge > 0 && observation.ObservedAt.Before(now.Add(-maxAge)) {
			return fmt.Errorf("%w: authority %s is stale", store.ErrRegistryObservationIncomplete, observation.Authority)
		}
		required[observation.Authority] = true
	}
	for authority, found := range required {
		if !found {
			return fmt.Errorf("%w: authority %s is absent", store.ErrRegistryObservationIncomplete, authority)
		}
	}
	return nil
}

func protectSuccessfulReleases(snapshot domain.RegistryLifecycleSnapshot, graph lifecycleGraph, protect func(string, string)) {
	releases := append([]domain.RegistryRelease(nil), snapshot.Releases...)
	sort.Slice(releases, func(i, j int) bool {
		if releases[i].SucceededAt == nil {
			return false
		}
		if releases[j].SucceededAt == nil {
			return true
		}
		if !releases[i].SucceededAt.Equal(*releases[j].SucceededAt) {
			return releases[i].SucceededAt.After(*releases[j].SucceededAt)
		}
		return releases[i].ID > releases[j].ID
	})
	seen := make(map[string]struct{})
	retained := 0
	for _, release := range releases {
		if release.RegistryTargetID != snapshot.Target.ID || release.ServiceID != snapshot.Policy.ServiceID || release.SucceededAt == nil || release.Availability != domain.RegistryArtifactPresent {
			continue
		}
		key := nodeKey(release.Repository, release.RootDigest)
		if graph.manifests[key].Digest == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if retained < snapshot.Policy.KeepLastSuccessful {
			protect(key, ReasonRetainedSuccessWindow)
			retained++
		}
	}
}

type cacheDecision struct {
	protect bool
	reasons []string
}

func decideCaches(snapshot domain.RegistryLifecycleSnapshot, now time.Time) (map[string]cacheDecision, int64, int64) {
	decisions := make(map[string]cacheDecision)
	groups := make(map[string][]domain.RegistryCacheGeneration)
	type rootInfo struct {
		key        string
		size       int64
		lastUsed   time.Time
		generation int64
		id         string
	}
	roots := make(map[string]rootInfo)
	for _, generation := range snapshot.CacheGenerations {
		if generation.State == "deleted" || generation.State == "missing" {
			continue
		}
		key := nodeKey(generation.Repository, generation.RootDigest)
		root := roots[key]
		root.key = key
		if generation.SizeBytes > root.size {
			root.size = generation.SizeBytes
		}
		if root.lastUsed.IsZero() || generation.LastUsedAt.After(root.lastUsed) {
			root.lastUsed = generation.LastUsedAt
		}
		if root.id == "" || generation.Generation < root.generation || (generation.Generation == root.generation && generation.ID < root.id) {
			root.generation = generation.Generation
			root.id = generation.ID
		}
		roots[key] = root
		group := strings.Join([]string{generation.PlatformSet, generation.TrustLane, generation.CacheSchema, generation.BuildDefinitionHash}, "\x00")
		groups[group] = append(groups[group], generation)
	}
	var before int64
	for _, root := range roots {
		before += root.size
	}
	fixed := make(map[string]map[string]struct{})
	for _, generations := range groups {
		sort.Slice(generations, func(i, j int) bool {
			if generations[i].Generation != generations[j].Generation {
				return generations[i].Generation > generations[j].Generation
			}
			return generations[i].ID > generations[j].ID
		})
		kept := 0
		for _, generation := range generations {
			key := nodeKey(generation.Repository, generation.RootDigest)
			if generation.State == "exporting" || generation.ActiveImports > 0 || generation.ActiveExports > 0 {
				addReason(fixed, key, ReasonCacheActive)
				continue
			}
			if generation.State == "succeeded" && kept < snapshot.Policy.CacheKeepGenerations {
				addReason(fixed, key, ReasonCacheRetained)
				kept++
				continue
			}
			if !generation.CreatedAt.Before(now.Add(-snapshot.Policy.MinimumSafetyAge)) {
				addReason(fixed, key, ReasonMinimumSafetyAge)
				continue
			}
		}
	}
	type candidate struct {
		root    rootInfo
		expired bool
	}
	var candidates []candidate
	for key, root := range roots {
		if len(fixed[key]) != 0 {
			continue
		}
		candidates = append(candidates, candidate{root: root, expired: !root.lastUsed.After(now.Add(-snapshot.Policy.CacheUnusedExpiry))})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].expired != candidates[j].expired {
			return candidates[i].expired
		}
		if !candidates[i].root.lastUsed.Equal(candidates[j].root.lastUsed) {
			return candidates[i].root.lastUsed.Before(candidates[j].root.lastUsed)
		}
		if candidates[i].root.generation != candidates[j].root.generation {
			return candidates[i].root.generation < candidates[j].root.generation
		}
		return candidates[i].root.id < candidates[j].root.id
	})
	remaining := before
	deletedRoots := make(map[string]map[string]struct{})
	for _, candidate := range candidates {
		key := candidate.root.key
		if candidate.expired {
			addReason(deletedRoots, key, ReasonCacheExpired)
			remaining -= candidate.root.size
			continue
		}
		if remaining > snapshot.Policy.CacheByteQuota {
			addReason(deletedRoots, key, ReasonCacheQuota)
			remaining -= candidate.root.size
			continue
		}
		addReason(fixed, key, ReasonCacheRecentlyUsed)
	}
	for key, reasons := range fixed {
		decisions[key] = cacheDecision{protect: true, reasons: reasonSlice(reasons)}
	}
	for key, reasons := range deletedRoots {
		if _, protected := fixed[key]; protected {
			continue
		}
		decisions[key] = cacheDecision{reasons: reasonSlice(reasons)}
	}
	if remaining < 0 {
		remaining = 0
	}
	return decisions, before, remaining
}

func cleanupItem(repository, resourceKind, digest string, disposition domain.RegistryCleanupDisposition, action string, size int64, reasons map[string]struct{}) domain.RegistryCleanupItem {
	state := "planned"
	if disposition == domain.RegistryCleanupProtect {
		state = "protected"
	}
	return domain.RegistryCleanupItem{
		Repository:     repository,
		ResourceKind:   resourceKind,
		Digest:         digest,
		Disposition:    disposition,
		Action:         action,
		EstimatedBytes: size,
		Reasons:        reasonSlice(reasons),
		State:          state,
	}
}

func summarize(items []domain.RegistryCleanupItem, cacheBefore, cacheAfter, quota int64) domain.RegistryCleanupSummary {
	summary := domain.RegistryCleanupSummary{CacheBytesBefore: cacheBefore, CacheBytesAfter: cacheAfter, CacheQuotaSatisfied: cacheAfter <= quota}
	for _, item := range items {
		switch {
		case item.ResourceKind == "blob" && item.Disposition == domain.RegistryCleanupDelete:
			summary.GarbageCollectBlobs++
			summary.EstimatedBytes += item.EstimatedBytes
		case item.ResourceKind != "blob" && item.Disposition == domain.RegistryCleanupDelete:
			summary.DeletedManifests++
		case item.ResourceKind != "blob":
			summary.ProtectedManifests++
		}
	}
	return summary
}

func sortCleanupItems(items []domain.RegistryCleanupItem) {
	sort.Slice(items, func(i, j int) bool {
		pi, pj := 0, 0
		if items[i].ResourceKind == "blob" {
			pi = 1
		}
		if items[j].ResourceKind == "blob" {
			pj = 1
		}
		if pi != pj {
			return pi < pj
		}
		if items[i].Repository != items[j].Repository {
			return items[i].Repository < items[j].Repository
		}
		if items[i].ResourceKind != items[j].ResourceKind {
			return items[i].ResourceKind < items[j].ResourceKind
		}
		return items[i].Digest < items[j].Digest
	})
	for ordinal := range items {
		items[ordinal].Ordinal = ordinal
	}
}

func latestCatalog(observations []domain.RegistryCatalogObservation, repository string) (domain.RegistryCatalogObservation, bool) {
	var latest domain.RegistryCatalogObservation
	found := false
	for _, observation := range observations {
		if observation.Repository == repository && (!found || observation.Revision > latest.Revision) {
			latest, found = observation, true
		}
	}
	return latest, found
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func nodeKey(repository, digest string) string { return repository + "\x00" + digest }

func addReason(target map[string]map[string]struct{}, key, reason string) {
	if target[key] == nil {
		target[key] = make(map[string]struct{})
	}
	target[key][reason] = struct{}{}
}

func reasonSlice(reasons map[string]struct{}) []string {
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(values map[string]map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func cyclic(children map[string][]string) bool {
	const (
		unseen = iota
		visiting
		done
	)
	state := make(map[string]int)
	var visit func(string) bool
	visit = func(node string) bool {
		switch state[node] {
		case visiting:
			return true
		case done:
			return false
		}
		state[node] = visiting
		for _, child := range children[node] {
			if visit(child) {
				return true
			}
		}
		state[node] = done
		return false
	}
	for node := range children {
		if visit(node) {
			return true
		}
	}
	return false
}

func maxBlobSize(blobs []domain.RegistryBlob) int64 {
	var size int64
	for _, blob := range blobs {
		if blob.SizeBytes > size {
			size = blob.SizeBytes
		}
	}
	return size
}

func IsLifecycleUnavailable(err error) bool {
	return errors.Is(err, store.ErrRegistryExternalLifecycle) || errors.Is(err, store.ErrRegistryObservationIncomplete) || errors.Is(err, store.ErrRegistrySnapshotStale)
}
