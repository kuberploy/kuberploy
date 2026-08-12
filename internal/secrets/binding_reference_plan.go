package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type middlewareReferenceError struct{ err error }

func (e middlewareReferenceError) Error() string { return e.err.Error() }
func (e middlewareReferenceError) Unwrap() error { return e.err }

// IsMiddlewareReferenceError lets admission and projection preserve a precise
// diagnostic while all errors.Is checks still see the underlying authority
// failure.
func IsMiddlewareReferenceError(err error) bool {
	var target middlewareReferenceError
	return errors.As(err, &target)
}

// BindingReferencePlan is a metadata-only snapshot of every runtime-secret use
// in one exact AppConfig. It is safe to carry beside an immutable Git write
// command, but a durable store must still re-resolve every use inside the same
// transaction that persists the command and Git-current references.
type BindingReferencePlan struct {
	Scope Scope                      `json:"scope"`
	Uses  []ResolvedBindingReference `json:"uses"`
}

func (p BindingReferencePlan) Validate() error {
	if p.Scope.Validate() != nil || len(p.Uses) > 256 {
		return ErrInvalid
	}
	previous := ""
	bindingVersions := map[string]string{}
	for _, use := range p.Uses {
		if !uuidRE.MatchString(use.BindingID) || !uuidRE.MatchString(use.VersionID) || !dnsLabelRE.MatchString(use.Name) ||
			!secretKeyRE.MatchString(use.Key) || use.Version < 1 || use.Namespace != p.Scope.Namespace ||
			use.TargetSecretName != TargetSecretName(Binding{ID: use.BindingID, Name: use.Name}, use.Version) ||
			use.Delivery.Validate() != nil || use.Delivery.SourceKey != use.Key {
			return ErrInvalid
		}
		if versionID, exists := bindingVersions[use.BindingID]; exists && versionID != use.VersionID {
			return ErrInvalid
		}
		bindingVersions[use.BindingID] = use.VersionID
		key := bindingReferenceUseKey(use)
		if previous != "" && key <= previous {
			return ErrInvalid
		}
		previous = key
	}
	return nil
}

const MiddlewareBasicAuthUsersPath = "/var/run/secrets/kuberploy/traefik-basic-auth/users"

// ResolveMiddlewareBindingReferences resolves BasicAuth metadata through the
// same exact runtime-secret scope/version/delivery policy as workload values.
// Traefik consumes the derived namespaced Secret directly; the file delivery
// identity is only the write-time authorization recorded on the binding.
func ResolveMiddlewareBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, refs []domain.SecretBindingRef) (BindingReferencePlan, error) {
	return resolveMiddlewareBindingReferences(ctx, catalog, scope, refs, false)
}

func ResolveGitCurrentMiddlewareBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, refs []domain.SecretBindingRef) (BindingReferencePlan, error) {
	return resolveMiddlewareBindingReferences(ctx, catalog, scope, refs, true)
}

func resolveMiddlewareBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, refs []domain.SecretBindingRef, allowRetained bool) (BindingReferencePlan, error) {
	if catalog == nil || scope.Validate() != nil || len(refs) > 32 {
		return BindingReferencePlan{}, ErrInvalid
	}
	snapshot := &bindingReferenceSnapshot{catalog: catalog, bindings: map[string]Binding{}, versions: map[string][]Version{}}
	plan := BindingReferencePlan{Scope: scope, Uses: []ResolvedBindingReference{}}
	for _, ref := range refs {
		if ref.Key != "users" {
			return BindingReferencePlan{}, ErrInvalid
		}
		resolver := ResolveBindingReference
		if allowRetained {
			resolver = ResolveGitCurrentBindingReference
		}
		resolved, err := resolver(ctx, snapshot, scope, ref, Delivery{SourceKey: ref.Key, Kind: DeliveryFile, FilePath: MiddlewareBasicAuthUsersPath, FileMode: 0o400})
		if err != nil {
			return BindingReferencePlan{}, err
		}
		plan.Uses = append(plan.Uses, resolved)
	}
	slices.SortFunc(plan.Uses, func(left, right ResolvedBindingReference) int {
		return compareStrings(bindingReferenceUseKey(left), bindingReferenceUseKey(right))
	})
	if plan.Validate() != nil {
		return BindingReferencePlan{}, ErrInvalid
	}
	return plan, nil
}

// MergeBindingReferencePlans preserves every distinct exact delivery while
// rejecting cross-scope or duplicate entries.
func MergeBindingReferencePlans(plans ...BindingReferencePlan) (BindingReferencePlan, error) {
	if len(plans) == 0 {
		return BindingReferencePlan{}, ErrInvalid
	}
	merged := BindingReferencePlan{Scope: plans[0].Scope, Uses: []ResolvedBindingReference{}}
	for _, plan := range plans {
		if plan.Validate() != nil || plan.Scope != merged.Scope {
			return BindingReferencePlan{}, ErrInvalid
		}
		merged.Uses = append(merged.Uses, plan.Uses...)
	}
	slices.SortFunc(merged.Uses, func(left, right ResolvedBindingReference) int {
		return compareStrings(bindingReferenceUseKey(left), bindingReferenceUseKey(right))
	})
	if merged.Validate() != nil {
		return BindingReferencePlan{}, ErrInvalid
	}
	return merged, nil
}

// ResolveAppConfigBindingReferences resolves every secret-bearing surface of
// one AppConfig into a single transaction-bound plan. Callers must not resolve
// workload and middleware references independently because that could persist
// only one family of deletion guards.
func ResolveAppConfigBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, runtime domain.WorkloadRuntime, middlewareRefs []domain.SecretBindingRef) (BindingReferencePlan, error) {
	workload, err := ResolveWorkloadBindingReferences(ctx, catalog, scope, runtime)
	if err != nil {
		return BindingReferencePlan{}, err
	}
	if len(middlewareRefs) == 0 {
		return workload, nil
	}
	middleware, err := ResolveMiddlewareBindingReferences(ctx, catalog, scope, middlewareRefs)
	if err != nil {
		return BindingReferencePlan{}, middlewareReferenceError{err: err}
	}
	return MergeBindingReferencePlans(workload, middleware)
}

// ResolveGitCurrentAppConfigBindingReferences is the retained-version variant
// used only while validating an already committed Git document.
func ResolveGitCurrentAppConfigBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, runtime domain.WorkloadRuntime, middlewareRefs []domain.SecretBindingRef) (BindingReferencePlan, error) {
	workload, err := ResolveGitCurrentWorkloadBindingReferences(ctx, catalog, scope, runtime)
	if err != nil {
		return BindingReferencePlan{}, err
	}
	if len(middlewareRefs) == 0 {
		return workload, nil
	}
	middleware, err := ResolveGitCurrentMiddlewareBindingReferences(ctx, catalog, scope, middlewareRefs)
	if err != nil {
		return BindingReferencePlan{}, middlewareReferenceError{err: err}
	}
	return MergeBindingReferencePlans(workload, middleware)
}

func (p BindingReferencePlan) Digest() (string, error) {
	if p.Validate() != nil {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// BindingVersions returns the unique binding/version pairs that require one
// Git-current deletion guard. Every delivery remains in Uses for exact
// transaction-time authorization and drift detection.
func (p BindingReferencePlan) BindingVersions() []ReferenceIdentity {
	if p.Validate() != nil {
		return nil
	}
	result := make([]ReferenceIdentity, 0, len(p.Uses))
	for _, use := range p.Uses {
		identity := ReferenceIdentity{BindingID: use.BindingID, VersionID: use.VersionID}
		if len(result) == 0 || result[len(result)-1] != identity {
			result = append(result, identity)
		}
	}
	return result
}

type ReferenceIdentity struct {
	BindingID string `json:"bindingId"`
	VersionID string `json:"versionId"`
}

func (i ReferenceIdentity) Validate() error {
	if !uuidRE.MatchString(i.BindingID) || !uuidRE.MatchString(i.VersionID) {
		return ErrInvalid
	}
	return nil
}

// ResolveWorkloadBindingReferences resolves the exact outer environment
// destination for every SecretBindingRef. Ordinary values never enter the
// resolver. The result contains no material, ciphertext or provider artifact.
func ResolveWorkloadBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, runtime domain.WorkloadRuntime) (BindingReferencePlan, error) {
	return resolveWorkloadBindingReferences(ctx, catalog, scope, runtime, false)
}

func ResolveGitCurrentWorkloadBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, runtime domain.WorkloadRuntime) (BindingReferencePlan, error) {
	return resolveWorkloadBindingReferences(ctx, catalog, scope, runtime, true)
}

func resolveWorkloadBindingReferences(ctx context.Context, catalog BindingReferenceCatalog, scope Scope, runtime domain.WorkloadRuntime, allowRetained bool) (BindingReferencePlan, error) {
	if catalog == nil || scope.Validate() != nil || len(domain.ValidateWorkloadRuntime(runtime)) != 0 {
		return BindingReferencePlan{}, ErrInvalid
	}
	snapshot := &bindingReferenceSnapshot{catalog: catalog, bindings: map[string]Binding{}, versions: map[string][]Version{}}
	plan := BindingReferencePlan{Scope: scope, Uses: []ResolvedBindingReference{}}
	for _, variable := range runtime.Env {
		if variable.ValueFrom == nil {
			continue
		}
		ref := variable.ValueFrom.SecretBindingRef
		resolver := ResolveBindingReference
		if allowRetained {
			resolver = ResolveGitCurrentBindingReference
		}
		resolved, err := resolver(ctx, snapshot, scope, ref, Delivery{
			SourceKey: ref.Key, Kind: DeliveryEnvironment, EnvironmentName: variable.Name,
		})
		if err != nil {
			return BindingReferencePlan{}, err
		}
		plan.Uses = append(plan.Uses, resolved)
	}
	slices.SortFunc(plan.Uses, func(left, right ResolvedBindingReference) int {
		return compareStrings(bindingReferenceUseKey(left), bindingReferenceUseKey(right))
	})
	if plan.Validate() != nil {
		return BindingReferencePlan{}, ErrInvalid
	}
	return plan, nil
}

func bindingReferenceUseKey(use ResolvedBindingReference) string {
	return use.BindingID + "\x00" + use.VersionID + "\x00" + use.Key + "\x00" + use.Delivery.EnvironmentName
}

func compareStrings(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

type bindingReferenceSnapshot struct {
	catalog  BindingReferenceCatalog
	bindings map[string]Binding
	versions map[string][]Version
}

func (s *bindingReferenceSnapshot) Binding(ctx context.Context, bindingID string) (Binding, error) {
	if binding, ok := s.bindings[bindingID]; ok {
		return binding, nil
	}
	binding, err := s.catalog.Binding(ctx, bindingID)
	if err != nil {
		return Binding{}, err
	}
	s.bindings[bindingID] = binding
	return binding, nil
}

func (s *bindingReferenceSnapshot) Versions(ctx context.Context, bindingID string) ([]Version, error) {
	if versions, ok := s.versions[bindingID]; ok {
		return cloneVersions(versions), nil
	}
	versions, err := s.catalog.Versions(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	s.versions[bindingID] = cloneVersions(versions)
	return cloneVersions(versions), nil
}

func cloneVersions(input []Version) []Version {
	result := make([]Version, len(input))
	for index := range input {
		result[index] = cloneVersion(input[index])
	}
	return result
}
