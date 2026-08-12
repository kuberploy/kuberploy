package registry

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

// ProtectionRefresher re-derives the three destructive-cleanup authorities
// from their current durable sources. A preview may refresh an unchanged stale
// observation; execution refreshes content only so its own plan token remains
// stable while any real authority change still invalidates the next delete.
type ProtectionRefresher interface {
	RefreshRegistryProtection(context.Context, string, string, time.Time, bool) error
}

type ProtectionInput struct {
	ReferenceKey   string
	Image          string
	SourceRevision string
	CreatedAt      time.Time
}

// BuildProtectionSnapshot converts trusted source rows into one closed
// registry authority snapshot. Images from another canonical registry are not
// roots for this target. A malformed image or an image under the target with a
// repository different from the service policy makes the observation
// incomplete and therefore blocks cleanup.
func BuildProtectionSnapshot(
	target domain.RegistryTarget,
	policy domain.ServiceRegistryPolicy,
	authority domain.RegistryAuthority,
	inputs []ProtectionInput,
	complete bool,
	observedAt time.Time,
) domain.RegistryProtectionSnapshot {
	kind := map[domain.RegistryAuthority]domain.RegistryArtifactReferenceKind{
		domain.RegistryAuthorityGitIntent:  domain.RegistryReferenceCurrentGitIntent,
		domain.RegistryAuthorityRuntime:    domain.RegistryReferenceObservedRunning,
		domain.RegistryAuthorityOperations: domain.RegistryReferenceActiveOperation,
	}[authority]
	snapshot := domain.RegistryProtectionSnapshot{Observation: domain.RegistryAuthorityObservation{
		RegistryTargetID: target.ID,
		ServiceID:        policy.ServiceID,
		Authority:        authority,
		Complete:         complete && kind != "" && !observedAt.IsZero(),
		ObservedAt:       observedAt.UTC(),
	}}
	server, validTarget := protectionTargetServer(target)
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if !snapshot.Observation.Complete || strings.TrimSpace(input.ReferenceKey) == "" {
			snapshot.Observation.Complete = false
			break
		}
		if _, duplicate := seen[input.ReferenceKey]; duplicate {
			snapshot.Observation.Complete = false
			break
		}
		seen[input.ReferenceKey] = struct{}{}
		serverName, repository, digest, validImage := protectionImage(input.Image)
		if !validTarget || !validImage || strings.TrimSpace(input.SourceRevision) == "" {
			snapshot.Observation.Complete = false
			break
		}
		if serverName != server {
			continue
		}
		if repository != policy.Repository {
			snapshot.Observation.Complete = false
			break
		}
		createdAt := input.CreatedAt.UTC()
		if createdAt.IsZero() {
			createdAt = observedAt.UTC()
		}
		snapshot.References = append(snapshot.References, domain.RegistryArtifactReference{
			RegistryTargetID: target.ID,
			ServiceID:        policy.ServiceID,
			Repository:       repository,
			Digest:           digest,
			Kind:             kind,
			ReferenceKey:     input.ReferenceKey,
			SourceRevision:   input.SourceRevision,
			CreatedAt:        createdAt,
			ObservedAt:       observedAt.UTC(),
		})
	}
	if !snapshot.Observation.Complete {
		snapshot.References = nil
	}
	sort.Slice(snapshot.References, func(i, j int) bool {
		return snapshot.References[i].ReferenceKey < snapshot.References[j].ReferenceKey
	})
	probe := snapshot
	probe.Observation.Revision = "registry-protection-content-v1"
	probe.Observation.ObservedAt = time.Time{}
	for index := range probe.References {
		probe.References[index].ObservedAt = time.Time{}
	}
	digest := store.RegistryProtectionSnapshotDigest(probe)
	snapshot.Observation.Revision = "registry-protection-v1:" + strings.TrimPrefix(digest, "sha256:")
	return snapshot
}

func protectionImage(image string) (string, string, string, bool) {
	if !registryImageDigestRE.MatchString(image) {
		return "", "", "", false
	}
	at := strings.LastIndexByte(image, '@')
	parsed, err := url.Parse("https://" + image[:at])
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", "", "", false
	}
	repository := strings.TrimPrefix(parsed.Path, "/")
	if parsed.Path != "/"+repository || !validRepository(repository) {
		return "", "", "", false
	}
	return strings.ToLower(parsed.Host), repository, image[at+1:], true
}

func protectionTargetServer(target domain.RegistryTarget) (string, bool) {
	parsed, err := url.Parse(target.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	return strings.ToLower(parsed.Host), true
}
