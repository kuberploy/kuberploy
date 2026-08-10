package registry

import (
	"regexp"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store"
)

var pullCredentialName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 ._-]{0,62}[A-Za-z0-9])?$`)
var pullCredentialUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidateProjectPullCredential(value domain.ProjectRegistryPullCredential) error {
	if !pullCredentialUUID.MatchString(value.ID) || !pullCredentialUUID.MatchString(value.ProjectID) ||
		!pullCredentialUUID.MatchString(value.RegistryTargetID) || strings.TrimSpace(value.Name) != value.Name ||
		!pullCredentialName.MatchString(value.Name) {
		return store.ErrRegistryPolicyInvalid
	}
	return nil
}

func ValidateApplicationPullSelection(value domain.ApplicationRegistryPullSelection) error {
	if !pullCredentialUUID.MatchString(value.ApplicationID) {
		return store.ErrRegistryPolicyInvalid
	}
	switch value.Mode {
	case domain.ApplicationRegistryPullPublic:
		if value.ProjectCredentialID != "" {
			return store.ErrRegistryPolicyInvalid
		}
	case domain.ApplicationRegistryPullCredential:
		if !pullCredentialUUID.MatchString(value.ProjectCredentialID) {
			return store.ErrRegistryPolicyInvalid
		}
	default:
		return store.ErrRegistryPolicyInvalid
	}
	return nil
}
