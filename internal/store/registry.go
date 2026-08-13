package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// RegistryStore is deliberately separate from Store so lifecycle workers and
// tests can depend on the narrow artifact contract without gaining access to
// users, deployments, or platform upgrades.
type RegistryStore interface {
	PutRegistryTarget(context.Context, domain.RegistryTarget) (domain.RegistryTarget, error)
	RegistryTarget(context.Context, string) (domain.RegistryTarget, error)
	PutServiceRegistryPolicy(context.Context, domain.ServiceRegistryPolicy) (domain.ServiceRegistryPolicy, error)
	ServiceRegistryPolicy(context.Context, string, string) (domain.ServiceRegistryPolicy, error)

	RecordRegistryInventory(context.Context, domain.RegistryInventoryObservation) error
	ReplaceRegistryCatalog(context.Context, domain.RegistryCatalogSnapshot) error
	ReplaceRegistryProtectionSnapshot(context.Context, domain.RegistryProtectionSnapshot) error
	PutRegistryPin(context.Context, domain.RegistryArtifactReference) error
	DeleteRegistryPin(context.Context, string, string, string) error
	PutRegistryRelease(context.Context, domain.RegistryRelease) (domain.RegistryRelease, bool, error)
	RegistryRelease(context.Context, string) (domain.RegistryRelease, error)
	PutRegistryCacheGeneration(context.Context, domain.RegistryCacheGeneration) (domain.RegistryCacheGeneration, bool, error)

	RegistryLifecycleSnapshot(context.Context, string, string, time.Time) (domain.RegistryLifecycleSnapshot, error)
	SaveRegistryCleanupPlan(context.Context, domain.RegistryCleanupPlan) (domain.RegistryCleanupPlan, bool, error)
	RegistryCleanupPlan(context.Context, string) (domain.RegistryCleanupPlan, error)
	ClaimRegistryCleanupPlan(context.Context, string, string, time.Time, time.Duration) (domain.RegistryCleanupPlan, bool, error)
	RenewRegistryCleanupPlanLeases(context.Context, string, string, time.Time, time.Duration) error
	AuthorizeRegistryCleanupItem(context.Context, string, int, string, time.Time) (domain.RegistryCleanupItem, error)
	RecordRegistryCleanupItemResult(context.Context, string, int, string, domain.RegistryCleanupItemResult) error
	FinishRegistryCleanupPlan(context.Context, string, string, bool, string, time.Time) error
}

type registrySnapshotTokenView struct {
	Target      domain.RegistryTarget
	Policy      domain.ServiceRegistryPolicy
	Inventory   domain.RegistryInventoryObservation
	Catalogs    []domain.RegistryCatalogObservation
	Authorities []domain.RegistryAuthorityObservation
	References  []domain.RegistryArtifactReference
	Releases    []domain.RegistryRelease
	Caches      []domain.RegistryCacheGeneration
	Manifests   []domain.RegistryManifest
	Blobs       []domain.RegistryBlob
	Children    []domain.RegistryManifestLink
	BlobLinks   []domain.RegistryManifestBlobLink
}

// RegistrySnapshotToken fingerprints all lifecycle inputs while deliberately
// excluding Snapshot.AsOf. Loading an unchanged database view at a later time
// must produce the same token for immediate pre-delete revalidation.
func RegistrySnapshotToken(snapshot domain.RegistryLifecycleSnapshot) string {
	view := canonicalRegistrySnapshot(snapshot)
	return digestJSON(view)
}

// RegistryAuthorityToken fingerprints semantic protection authorities and root
// records while excluding the observed catalog graph and liveness-only refresh
// metadata. A cleanup execution updates this token after each of its own
// successful catalog mutation; any concurrent pin, release, build-cache, Git,
// runtime, operation, policy, or authority-completeness change still makes the
// next item fail closed.
func RegistryAuthorityToken(snapshot domain.RegistryLifecycleSnapshot) string {
	type authorityState struct {
		Authority domain.RegistryAuthority
		Complete  bool
	}
	type authorityTokenView struct {
		Target      domain.RegistryTarget
		Policy      domain.ServiceRegistryPolicy
		Authorities []authorityState
		References  []domain.RegistryArtifactReference
		Releases    []domain.RegistryRelease
		Caches      []domain.RegistryCacheGeneration
	}
	view := canonicalRegistrySnapshot(snapshot)
	authorities := make([]authorityState, 0, len(view.Authorities))
	for _, observation := range view.Authorities {
		authorities = append(authorities, authorityState{Authority: observation.Authority, Complete: observation.Complete})
	}
	// Observation timestamps and revision counters are liveness evidence, not
	// protection content. Preview and claim validate their freshness separately.
	// Excluding them here lets an unchanged observer refresh run concurrently
	// without invalidating an offline sweep. Repository bytes cannot change while
	// the managed registry is stopped, and the physical checkpoint independently
	// scans the complete on-disk graph before GC. References retain their exact
	// source revisions and creation times so any real Git/runtime/operation root
	// change still invalidates the next destructive step.
	for index := range view.References {
		view.References[index].ObservedAt = time.Time{}
	}
	return digestJSON(authorityTokenView{
		Target: view.Target, Policy: view.Policy, Authorities: authorities,
		References: view.References, Releases: view.Releases, Caches: view.Caches,
	})
}

func RegistryCleanupPlanDigest(plan domain.RegistryCleanupPlan) string {
	type planView struct {
		RegistryTargetID string
		ServiceID        string
		SnapshotToken    string
		AuthorityToken   string
		Policy           domain.ServiceRegistryPolicy
		Inventory        domain.RegistryInventoryObservation
		Catalogs         []domain.RegistryCatalogObservation
		Authorities      []domain.RegistryAuthorityObservation
		Summary          domain.RegistryCleanupSummary
		Items            []domain.RegistryCleanupItem
	}
	items := append([]domain.RegistryCleanupItem(nil), plan.Items...)
	inventory := plan.Inventory
	inventory.Repositories = append([]string(nil), inventory.Repositories...)
	sort.Strings(inventory.Repositories)
	for i := range items {
		items[i].State = ""
		items[i].ProviderMessage = ""
		items[i].UpdatedAt = time.Time{}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repository != items[j].Repository {
			return items[i].Repository < items[j].Repository
		}
		if items[i].ResourceKind != items[j].ResourceKind {
			return items[i].ResourceKind < items[j].ResourceKind
		}
		return items[i].Digest < items[j].Digest
	})
	return digestJSON(planView{
		RegistryTargetID: plan.RegistryTargetID,
		ServiceID:        plan.ServiceID,
		SnapshotToken:    plan.SnapshotToken,
		AuthorityToken:   plan.AuthorityToken,
		Policy:           plan.Policy,
		Inventory:        inventory,
		Catalogs:         sortedCatalogs(plan.Catalogs),
		Authorities:      sortedAuthorities(plan.Authorities),
		Summary:          plan.Summary,
		Items:            items,
	})
}

// RegistryCleanupPlanCanResumeOfflineSweep identifies the only terminal
// cleanup shape that is safe to retry. Every manifest deletion must already be
// durably complete and every unfinished item must be an authorized blob from
// the exact prior offline sweep. The executor reuses the immutable plan and
// candidate-set digests, then performs a fresh physical reachability
// checkpoint while the registry is stopped before garbage collection.
func RegistryCleanupPlanCanResumeOfflineSweep(plan domain.RegistryCleanupPlan) bool {
	if plan.State != "failed" || plan.Failure == "" {
		return false
	}
	pendingBlobs := 0
	for _, item := range plan.Items {
		if item.Disposition != domain.RegistryCleanupDelete {
			continue
		}
		switch item.State {
		case "deleted":
		case "deleting", "failed":
			if item.ResourceKind != "blob" || item.Action != "garbage-collect-blob" {
				return false
			}
			pendingBlobs++
		default:
			return false
		}
	}
	return pendingBlobs > 0
}

// RegistryCatalogSnapshotDigest binds a catalog revision to its exact graph.
// Observation IDs and the digest field itself are excluded so a retried writer
// can prove idempotency even when the store assigned the observation UUID.
func RegistryCatalogSnapshotDigest(snapshot domain.RegistryCatalogSnapshot) string {
	observation := snapshot.Observation
	observation.ID = ""
	observation.SnapshotDigest = ""
	manifests := append([]domain.RegistryManifest(nil), snapshot.Manifests...)
	for index := range manifests {
		manifests[index].Present = true
		manifests[index].LastObservationRevision = observation.Revision
		manifests[index].LastObservedAt = observation.ObservedAt
		manifests[index].DeletedAt = nil
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Digest < manifests[j].Digest })
	blobs := append([]domain.RegistryBlob(nil), snapshot.Blobs...)
	for index := range blobs {
		blobs[index].Present = true
		blobs[index].LastObservationRevision = observation.Revision
		blobs[index].LastObservedAt = observation.ObservedAt
		blobs[index].DeletedAt = nil
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Digest < blobs[j].Digest })
	children := append([]domain.RegistryManifestLink(nil), snapshot.Children...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].ParentDigest != children[j].ParentDigest {
			return children[i].ParentDigest < children[j].ParentDigest
		}
		return children[i].ChildDigest < children[j].ChildDigest
	})
	blobLinks := append([]domain.RegistryManifestBlobLink(nil), snapshot.BlobLinks...)
	sort.Slice(blobLinks, func(i, j int) bool {
		if blobLinks[i].ManifestDigest != blobLinks[j].ManifestDigest {
			return blobLinks[i].ManifestDigest < blobLinks[j].ManifestDigest
		}
		return blobLinks[i].BlobDigest < blobLinks[j].BlobDigest
	})
	return digestJSON(struct {
		Observation domain.RegistryCatalogObservation
		Manifests   []domain.RegistryManifest
		Blobs       []domain.RegistryBlob
		Children    []domain.RegistryManifestLink
		BlobLinks   []domain.RegistryManifestBlobLink
	}{observation, manifests, blobs, children, blobLinks})
}

func RegistryProtectionSnapshotDigest(snapshot domain.RegistryProtectionSnapshot) string {
	observation := snapshot.Observation
	observation.SnapshotDigest = ""
	references := append([]domain.RegistryArtifactReference(nil), snapshot.References...)
	sort.Slice(references, func(i, j int) bool {
		if references[i].ReferenceKey != references[j].ReferenceKey {
			return references[i].ReferenceKey < references[j].ReferenceKey
		}
		if references[i].Repository != references[j].Repository {
			return references[i].Repository < references[j].Repository
		}
		return references[i].Digest < references[j].Digest
	})
	return digestJSON(struct {
		Observation domain.RegistryAuthorityObservation
		References  []domain.RegistryArtifactReference
	}{observation, references})
}

func canonicalRegistrySnapshot(snapshot domain.RegistryLifecycleSnapshot) registrySnapshotTokenView {
	inventory := snapshot.Inventory
	inventory.Repositories = append([]string(nil), inventory.Repositories...)
	sort.Strings(inventory.Repositories)

	refs := append([]domain.RegistryArtifactReference(nil), snapshot.References...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		if refs[i].ReferenceKey != refs[j].ReferenceKey {
			return refs[i].ReferenceKey < refs[j].ReferenceKey
		}
		if refs[i].Repository != refs[j].Repository {
			return refs[i].Repository < refs[j].Repository
		}
		return refs[i].Digest < refs[j].Digest
	})
	releases := append([]domain.RegistryRelease(nil), snapshot.Releases...)
	sort.Slice(releases, func(i, j int) bool { return releases[i].ID < releases[j].ID })
	caches := append([]domain.RegistryCacheGeneration(nil), snapshot.CacheGenerations...)
	sort.Slice(caches, func(i, j int) bool { return caches[i].ID < caches[j].ID })
	manifests := append([]domain.RegistryManifest(nil), snapshot.Manifests...)
	sort.Slice(manifests, func(i, j int) bool {
		if manifests[i].Repository != manifests[j].Repository {
			return manifests[i].Repository < manifests[j].Repository
		}
		return manifests[i].Digest < manifests[j].Digest
	})
	blobs := append([]domain.RegistryBlob(nil), snapshot.Blobs...)
	sort.Slice(blobs, func(i, j int) bool {
		if blobs[i].Repository != blobs[j].Repository {
			return blobs[i].Repository < blobs[j].Repository
		}
		return blobs[i].Digest < blobs[j].Digest
	})
	children := append([]domain.RegistryManifestLink(nil), snapshot.Children...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Repository != children[j].Repository {
			return children[i].Repository < children[j].Repository
		}
		if children[i].ParentDigest != children[j].ParentDigest {
			return children[i].ParentDigest < children[j].ParentDigest
		}
		return children[i].ChildDigest < children[j].ChildDigest
	})
	blobLinks := append([]domain.RegistryManifestBlobLink(nil), snapshot.BlobLinks...)
	sort.Slice(blobLinks, func(i, j int) bool {
		if blobLinks[i].Repository != blobLinks[j].Repository {
			return blobLinks[i].Repository < blobLinks[j].Repository
		}
		if blobLinks[i].ManifestDigest != blobLinks[j].ManifestDigest {
			return blobLinks[i].ManifestDigest < blobLinks[j].ManifestDigest
		}
		return blobLinks[i].BlobDigest < blobLinks[j].BlobDigest
	})
	return registrySnapshotTokenView{
		Target:      snapshot.Target,
		Policy:      snapshot.Policy,
		Inventory:   inventory,
		Catalogs:    sortedCatalogs(snapshot.CatalogObservations),
		Authorities: sortedAuthorities(snapshot.AuthorityObservations),
		References:  refs,
		Releases:    releases,
		Caches:      caches,
		Manifests:   manifests,
		Blobs:       blobs,
		Children:    children,
		BlobLinks:   blobLinks,
	}
}

func sortedCatalogs(in []domain.RegistryCatalogObservation) []domain.RegistryCatalogObservation {
	out := append([]domain.RegistryCatalogObservation(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		return out[i].Revision < out[j].Revision
	})
	return out
}

func sortedAuthorities(in []domain.RegistryAuthorityObservation) []domain.RegistryAuthorityObservation {
	out := append([]domain.RegistryAuthorityObservation(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Authority < out[j].Authority })
	return out
}

func digestJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err) // all registry token views contain only JSON-safe value types
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
