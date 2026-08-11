package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func (s *Store) CreateEnvironmentGitBinding(_ context.Context, actor, key, fingerprint, requestID string, in gitprojection.CreateEnvironmentBindingInput) (base.Result[gitprojection.Binding], error) {
	if err := in.Validate(); err != nil || actor == "" || key == "" || fingerprint == "" || requestID == "" {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, exists := s.environments[in.EnvironmentID]
	if !exists {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionConfigWrite, domain.AccessTarget{Type: "environment", ID: environment.ID}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	if err := s.authorizeLocked(actor, domain.PermissionBuildsManage, domain.AccessTarget{Type: "environment", ID: environment.ID}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	idemKey := ik(actor, "environment-git-bindings.create:"+environment.ID, key)
	if old, replay := s.idempotency[idemKey]; replay {
		if old.fingerprint != fingerprint {
			return base.Result[gitprojection.Binding]{}, base.ErrIdempotencyConflict
		}
		binding, found := s.gitBindings[environment.ID]
		if !found || binding.ID != old.resourceID {
			return base.Result[gitprojection.Binding]{}, base.ErrNotFound
		}
		return base.Result[gitprojection.Binding]{Value: binding, Replay: true}, nil
	}
	installation, exists := s.installations[in.LinkedInstallationID]
	project, projectExists := s.projects[environment.ProjectID]
	if !exists || !projectExists || installation.GitHubInstallationID != in.Repository.InstallationID ||
		!strings.EqualFold(installation.AccountLogin, in.Repository.Owner) ||
		in.LinkedRepositoryID != deterministicGitHubRepositoryID(installation.ID, in.Repository.RepositoryID) {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	if !s.isAdminLocked(actor) && installation.OwnerUserID != actor &&
		!(installation.Visibility == "team" && installation.TeamID != "" && installation.TeamID == project.TeamID) {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	if _, exists = s.gitBindings[environment.ID]; exists {
		return base.Result[gitprojection.Binding]{}, base.ErrConflict
	}
	now := time.Now().UTC()
	binding, err := gitprojection.NewGitHubEnvironmentBinding(id.New(), environment.ProjectID, environment.ID, in.Repository, in.TargetRef, now)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	s.gitBindings[environment.ID] = binding
	s.idempotency[idemKey] = idemRecord{fingerprint: fingerprint, typ: "git-binding", resourceID: binding.ID}
	s.audits++
	return base.Result[gitprojection.Binding]{Value: binding}, nil
}

func deterministicGitHubRepositoryID(installationID string, providerRepositoryID int64) string {
	hash := sha256.New()
	for _, part := range []string{"github-repository-v1", installationID, strconv.FormatInt(providerRepositoryID, 10)} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	value := hash.Sum(nil)[:16]
	value[6] = (value[6] & 0x0f) | 0x80
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func (s *Store) GetEnvironmentGitBindingForActor(_ context.Context, actor, environmentID string) (gitprojection.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionConfigRead, domain.AccessTarget{Type: "environment", ID: environmentID}); err != nil {
		return gitprojection.Binding{}, err
	}
	binding, exists := s.gitBindings[environmentID]
	if !exists {
		return gitprojection.Binding{}, base.ErrNotFound
	}
	return binding, nil
}

func (s *Store) CreatePlatformGitBinding(_ context.Context, actor, key, fingerprint, requestID string, in gitprojection.CreatePlatformBindingInput) (base.Result[gitprojection.Binding], error) {
	if err := in.Validate(); err != nil || actor == "" || key == "" || fingerprint == "" || requestID == "" {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	idemKey := ik(actor, "argo-platform-git-bindings.create:"+in.ClusterID, key)
	if old, replay := s.idempotency[idemKey]; replay {
		if old.fingerprint != fingerprint || old.typ != "argo-platform-git-binding" {
			return base.Result[gitprojection.Binding]{}, base.ErrIdempotencyConflict
		}
		binding, found := s.platformGitBindings[in.ClusterID]
		if !found || binding.ID != old.resourceID {
			return base.Result[gitprojection.Binding]{}, base.ErrNotFound
		}
		return base.Result[gitprojection.Binding]{Value: binding, Replay: true}, nil
	}
	installation, exists := s.installations[in.LinkedInstallationID]
	if !exists || installation.GitHubInstallationID != in.Repository.InstallationID ||
		!strings.EqualFold(installation.AccountLogin, in.Repository.Owner) ||
		in.LinkedRepositoryID != deterministicGitHubRepositoryID(installation.ID, in.Repository.RepositoryID) {
		return base.Result[gitprojection.Binding]{}, base.ErrNotFound
	}
	if _, exists = s.platformGitBindings[in.ClusterID]; exists {
		return base.Result[gitprojection.Binding]{}, base.ErrConflict
	}
	if _, err := in.Repository.CanonicalRemote(); err != nil {
		return base.Result[gitprojection.Binding]{}, gitprojection.ErrInvalid
	}
	now := time.Now().UTC()
	binding, err := gitprojection.NewGitHubPlatformBinding(in.BindingID, in.ClusterID, in.Repository, in.TargetRef, now)
	if err != nil {
		return base.Result[gitprojection.Binding]{}, err
	}
	s.platformGitBindings[in.ClusterID] = binding
	s.idempotency[idemKey] = idemRecord{fingerprint: fingerprint, typ: "argo-platform-git-binding", resourceID: binding.ID}
	s.audits++
	return base.Result[gitprojection.Binding]{Value: binding}, nil
}

func (s *Store) GetPlatformGitBindingForActor(_ context.Context, actor, clusterID string) (gitprojection.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(actor, domain.PermissionPlatformAdmin, domain.AccessTarget{Type: "platform", ID: "platform"}); err != nil {
		return gitprojection.Binding{}, err
	}
	binding, exists := s.platformGitBindings[clusterID]
	if !exists {
		return gitprojection.Binding{}, base.ErrNotFound
	}
	return binding, nil
}
