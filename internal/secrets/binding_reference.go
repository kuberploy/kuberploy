package secrets

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// BindingReferenceCatalog is deliberately read-only and metadata-only. A
// config validator never needs provider payloads or secret material.
type BindingReferenceCatalog interface {
	Binding(context.Context, string) (Binding, error)
	Versions(context.Context, string) ([]Version, error)
}

// ResolvedBindingReference is the complete safe identity consumed by a
// workload renderer. It intentionally omits provider revisions, manifests,
// ciphertext digests, fingerprints and all material.
type ResolvedBindingReference struct {
	BindingID        string   `json:"bindingId"`
	VersionID        string   `json:"versionId"`
	Name             string   `json:"name"`
	Key              string   `json:"key"`
	Version          int64    `json:"version"`
	Namespace        string   `json:"namespace"`
	TargetSecretName string   `json:"targetSecretName"`
	Delivery         Delivery `json:"delivery"`
}

// ResolveBindingReference pins an editable AppConfig reference to one exact,
// active, ready strict SealedSecret version in the same application and
// environment. expectedDelivery is the actual workload destination (for
// example the environment variable name), preventing a user from selecting a
// material key that was not authorized for that destination.
func ResolveBindingReference(ctx context.Context, catalog BindingReferenceCatalog, expectedScope Scope, ref domain.SecretBindingRef, expectedDelivery Delivery) (ResolvedBindingReference, error) {
	return resolveBindingReference(ctx, catalog, expectedScope, ref, expectedDelivery, false)
}

// ResolveGitCurrentBindingReference validates an AppConfig that is already the
// exact indexed Git state. A rotation retains the previous immutable Secret so
// the currently running workload can continue while the operator republishes
// the AppConfig with the new active version. This path accepts that one exact
// retained version; ordinary preview/save resolution remains active-only.
func ResolveGitCurrentBindingReference(ctx context.Context, catalog BindingReferenceCatalog, expectedScope Scope, ref domain.SecretBindingRef, expectedDelivery Delivery) (ResolvedBindingReference, error) {
	return resolveBindingReference(ctx, catalog, expectedScope, ref, expectedDelivery, true)
}

func resolveBindingReference(ctx context.Context, catalog BindingReferenceCatalog, expectedScope Scope, ref domain.SecretBindingRef, expectedDelivery Delivery, allowRetained bool) (ResolvedBindingReference, error) {
	if catalog == nil || expectedScope.Validate() != nil || !ref.Valid() || expectedDelivery.Validate() != nil ||
		expectedDelivery.SourceKey != ref.Key {
		return ResolvedBindingReference{}, ErrInvalid
	}
	binding, err := catalog.Binding(ctx, ref.BindingID)
	if err != nil {
		return ResolvedBindingReference{}, err
	}
	// Scope/name mismatches are indistinguishable from absence so this resolver
	// cannot be used as a cross-tenant binding oracle.
	if binding.Validate() != nil || binding.ID != ref.BindingID || binding.Scope != expectedScope || binding.Name != ref.Name ||
		binding.Provider != ProviderSealedSecrets || binding.Purpose != PurposeRuntimeSecret {
		return ResolvedBindingReference{}, ErrNotFound
	}
	if binding.State != BindingReady || binding.ActiveVersion < ref.Version {
		return ResolvedBindingReference{}, ErrNotReady
	}
	versions, err := catalog.Versions(ctx, binding.ID)
	if err != nil {
		return ResolvedBindingReference{}, err
	}
	var selected *Version
	for index := range versions {
		if versions[index].Number == ref.Version {
			if selected != nil {
				return ResolvedBindingReference{}, ErrConflict
			}
			copy := cloneVersion(versions[index])
			selected = &copy
		}
	}
	if selected == nil || selected.Validate() != nil || selected.BindingID != binding.ID ||
		selected.Provider != ProviderSealedSecrets || selected.TargetSecretType != TargetSecretOpaque ||
		(selected.State != VersionActive && (!allowRetained || selected.State != VersionRetained)) || selected.Artifact == nil {
		return ResolvedBindingReference{}, ErrNotReady
	}
	if selected.State == VersionActive && binding.ActiveVersion != ref.Version {
		return ResolvedBindingReference{}, ErrNotReady
	}
	target := TargetSecretName(binding, selected.Number)
	if selected.Artifact.ValidateFor(binding, selected.Number) != nil || selected.Artifact.TargetSecretName != target ||
		selected.Artifact.ObjectName != target {
		return ResolvedBindingReference{}, ErrProviderMismatch
	}
	matches := 0
	for _, delivery := range selected.Deliveries {
		if delivery == expectedDelivery {
			matches++
		}
	}
	if matches != 1 {
		return ResolvedBindingReference{}, ErrInvalid
	}
	return ResolvedBindingReference{BindingID: binding.ID, VersionID: selected.ID, Name: binding.Name,
		Key: ref.Key, Version: selected.Number, Namespace: binding.Scope.Namespace, TargetSecretName: target,
		Delivery: expectedDelivery}, nil
}
