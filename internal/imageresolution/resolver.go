package imageresolution

import (
	"context"
	"errors"
	"strings"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type Catalog interface {
	AuthorizedImageSourcesForActor(context.Context, string, string, string) ([]AuthorizedSource, error)
}

type Provider interface {
	ResolveTag(context.Context, AuthorizedSource, TagReference, *ProviderAuthority, Platform) (string, error)
}

type ProviderAuthority struct {
	Profile   *imagepull.Profile
	Anonymous bool
	Token     *TokenAuthority
}

type Resolution struct {
	RequestedImage string `json:"requestedImage"`
	ImmutableImage string `json:"immutableImage"`
	Resolved       bool   `json:"resolved"`
}

type Resolver struct {
	Catalog  Catalog
	Provider Provider
	Config   RuntimeConfig
}

func (r *Resolver) Available() bool {
	return r != nil && r.Catalog != nil && r.Provider != nil && r.Config.Validate() == nil &&
		(len(r.Config.Profiles) != 0 || len(r.Config.AnonymousTargetIDs) != 0)
}

func (r *Resolver) Resolve(ctx context.Context, actor, applicationID, environmentID, image string) (Resolution, error) {
	result := Resolution{RequestedImage: image}
	if !validActorIdentity(actor) || !uuidPattern.MatchString(applicationID) || !uuidPattern.MatchString(environmentID) {
		return result, ErrInvalid
	}
	if IsImmutableImage(image) {
		result.ImmutableImage = image
		return result, nil
	}
	if r == nil || r.Catalog == nil || r.Provider == nil || r.Config.Validate() != nil {
		return result, ErrUnavailable
	}
	reference, err := ParseTagReference(image)
	if err != nil {
		return result, err
	}
	sources, err := r.Catalog.AuthorizedImageSourcesForActor(ctx, actor, applicationID, environmentID)
	if err != nil {
		return result, err
	}
	if len(sources) > MaximumAuthorizedSources {
		return result, ErrConflict
	}
	matches := make([]AuthorizedSource, 0, 1)
	for _, source := range sources {
		if source.Validate(applicationID) != nil {
			return result, ErrConflict
		}
		server, _ := canonicalRegistryServer(source.Target.Endpoint)
		if server == reference.Server && source.Policy.Repository == reference.Repository {
			matches = append(matches, source)
		}
	}
	if len(matches) == 0 {
		return result, ErrNotFound
	}
	if len(matches) != 1 {
		return result, ErrConflict
	}
	source := matches[0]
	profile, anonymous, configured := r.Config.authority(source.Target.ID)
	if !configured || profile == nil && !anonymous {
		return result, ErrUnavailable
	}
	if anonymous && strings.Contains(reference.Server, ":") {
		return result, ErrConflict
	}
	authority := &ProviderAuthority{Anonymous: anonymous}
	authority.Token = r.Config.tokenAuthority(source.Target.ID)
	if profile != nil {
		server, _ := canonicalRegistryServer(source.Target.Endpoint)
		if profile.RegistryServer != server || source.Target.PullCredentialRef == "" || profile.CredentialRef != source.Target.PullCredentialRef {
			return result, ErrConflict
		}
		authority.Profile = profile
	}
	digest, err := r.Provider.ResolveTag(ctx, source, reference, authority, r.Config.Platform)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		return result, err
	}
	if !digestPattern.MatchString(digest) {
		return result, ErrConflict
	}
	result.ImmutableImage = reference.Server + "/" + reference.Repository + "@" + strings.ToLower(digest)
	result.Resolved = true
	return result, nil
}

func validActorIdentity(actor string) bool {
	return actor != "" && len(actor) <= 128 && strings.TrimSpace(actor) == actor && !strings.ContainsAny(actor, "\x00\r\n\t ")
}
