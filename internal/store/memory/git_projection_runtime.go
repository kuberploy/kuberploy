package memory

import (
	"context"
	"reflect"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

func memoryGitDocumentKey(bindingID, path string) string { return bindingID + "\x00" + path }

func sameMemoryGitBindingAuthority(left, right gitprojection.Binding) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.ScopeID == right.ScopeID &&
		left.ProjectID == right.ProjectID && left.EnvironmentID == right.EnvironmentID &&
		left.Repository == right.Repository && left.TargetRef == right.TargetRef && left.Prefix == right.Prefix &&
		left.CredentialMode == right.CredentialMode && left.CredentialSecretName == right.CredentialSecretName
}

// PutBinding and PutProjectionDocument provide the same narrow catalog seam as
// production for contract tests. They do not enable the runtime by themselves.
func (s *Store) PutBinding(_ context.Context, binding gitprojection.Binding) error {
	if binding.Validate() != nil {
		return gitprojection.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch binding.Kind {
	case gitprojection.BindingEnvironment:
		if current, exists := s.gitBindings[binding.EnvironmentID]; exists {
			if current.ID != binding.ID || !sameMemoryGitBindingAuthority(current, binding) {
				return base.ErrConflict
			}
		}
		s.gitBindings[binding.EnvironmentID] = binding
	case gitprojection.BindingPlatform:
		if current, exists := s.platformGitBindings["platform"]; exists {
			if current.ID != binding.ID || !sameMemoryGitBindingAuthority(current, binding) {
				return base.ErrConflict
			}
		}
		s.platformGitBindings["platform"] = binding
	default:
		return gitprojection.ErrInvalid
	}
	return nil
}

func (s *Store) Binding(_ context.Context, bindingID string) (gitprojection.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, binding := range s.gitBindings {
		if binding.ID == bindingID {
			return binding, nil
		}
	}
	for _, binding := range s.platformGitBindings {
		if binding.ID == bindingID {
			return binding, nil
		}
	}
	return gitprojection.Binding{}, base.ErrNotFound
}

func (s *Store) PutProjectionDocument(_ context.Context, document gitprojection.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var binding gitprojection.Binding
	for _, current := range s.gitBindings {
		if current.ID == document.BindingID {
			binding = current
			break
		}
	}
	if binding.ID == "" || document.Validate(binding) != nil || document.Generation != binding.ProjectionGeneration {
		return gitprojection.ErrInvalid
	}
	s.gitDocuments[memoryGitDocumentKey(binding.ID, document.Path)] = document
	return nil
}

func (s *Store) validateProjectionPlanLocked(plan *gitprojection.WritePlan) (gitprojection.Binding, error) {
	if plan == nil {
		return gitprojection.Binding{}, gitprojection.ErrInvalid
	}
	binding, exists := s.gitBindings[plan.EnvironmentID]
	if !exists || plan.Validate(binding) != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	path, err := gitprojection.ApplicationPath(binding, plan.ApplicationID)
	if err != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	document, exists := s.gitDocuments[memoryGitDocumentKey(binding.ID, path)]
	dependencyPaths, dependencyErr := gitprojection.DependencyPaths(binding)
	if dependencyErr != nil {
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	dependencies := make([]gitprojection.Document, 0, len(dependencyPaths))
	for _, dependencyPath := range dependencyPaths {
		dependency, present := s.gitDocuments[memoryGitDocumentKey(binding.ID, dependencyPath)]
		if !present {
			continue
		}
		if dependency.Validate(binding) != nil || !dependency.Valid {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
		dependencies = append(dependencies, dependency)
	}
	switch plan.Precondition {
	case gitprojection.MutationCreateIfAbsent:
		if exists {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	case gitprojection.MutationMatchETag:
		if !exists {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
		etag, etagErr := gitprojection.StrongETagWithDependencies(binding, []gitprojection.Document{document}, dependencyPaths, dependencies, plan.ChartDigest, plan.PolicyVersion)
		if etagErr != nil || etag != plan.ExpectedETag {
			return gitprojection.Binding{}, base.ErrPreconditionFailed
		}
	default:
		return gitprojection.Binding{}, base.ErrPreconditionFailed
	}
	return binding, nil
}

func (s *Store) resolveProjectedVariablesLocked(binding gitprojection.Binding, runtime domain.WorkloadRuntime) (variablecompiler.Resolution, error) {
	paths, err := gitprojection.DependencyPaths(binding)
	if err != nil {
		return variablecompiler.Resolution{}, base.ErrPreconditionFailed
	}
	documents := make([]gitprojection.Document, 0, len(paths))
	for _, dependencyPath := range paths {
		if document, present := s.gitDocuments[memoryGitDocumentKey(binding.ID, dependencyPath)]; present {
			documents = append(documents, document)
		}
	}
	states, err := variablecompiler.States(paths, documents)
	if err != nil {
		return variablecompiler.Resolution{}, base.ErrPreconditionFailed
	}
	resolution, err := variablecompiler.Resolve(states, documents, runtime)
	if err != nil {
		return variablecompiler.Resolution{}, base.ErrPreconditionFailed
	}
	return resolution, nil
}

func (s *Store) putGitWriteCommandLocked(actor, operationID, deploymentID string, plan *gitprojection.WritePlan, content []byte, message string, now time.Time) error {
	return s.putDeploymentGitCommandLocked(actor, operationID, deploymentID, plan, content, message, gitprojection.MutationUpsert, now)
}

func (s *Store) putGitDeleteCommandLocked(actor, operationID, deploymentID string, plan *gitprojection.WritePlan, content []byte, message string, now time.Time) error {
	return s.putDeploymentGitCommandLocked(actor, operationID, deploymentID, plan, content, message, gitprojection.MutationDelete, now)
}

func (s *Store) putDeploymentGitCommandLocked(actor, operationID, deploymentID string, plan *gitprojection.WritePlan, content []byte, message string, action gitprojection.MutationAction, now time.Time) error {
	if plan == nil {
		return nil
	}
	binding, err := s.validateProjectionPlanLocked(plan)
	if err != nil {
		return err
	}
	var command gitprojection.WriteCommand
	if action == gitprojection.MutationDelete {
		command, err = gitprojection.NewDeleteWriteCommand(operationID, deploymentID, actor, *plan, binding, content, message, now)
	} else {
		command, err = gitprojection.NewWriteCommand(operationID, deploymentID, actor, *plan, binding, content, message, now)
	}
	if err != nil {
		return err
	}
	environment, exists := s.environments[plan.EnvironmentID]
	if !exists {
		return base.ErrNotFound
	}
	mode := gitpublication.ModePullRequest
	if environment.ProtectionPolicy == domain.EnvironmentDevelopment {
		mode = gitpublication.ModeDirect
	} else if environment.ProtectionPolicy != domain.EnvironmentProtected {
		return base.ErrConflict
	}
	command.PublicationMode = gitprojection.PublicationMode(mode)
	if command.Validate(binding) != nil {
		return gitprojection.ErrInvalid
	}
	var publication gitpublication.Publication
	if mode == gitpublication.ModePullRequest {
		publication, err = gitpublication.NewPublication(operationID, binding.ID, gitpublication.Repository{
			InstallationID: binding.Repository.InstallationID, ID: binding.Repository.RepositoryID,
			Owner: binding.Repository.Owner, Name: binding.Repository.Name,
		}, binding.TargetRef, plan.BaseRevision, now)
		if err != nil {
			return err
		}
	}
	if current, exists := s.gitWriteCommands[operationID]; exists {
		if !reflect.DeepEqual(current, command) || s.gitPublicationModes[operationID] != mode ||
			(mode == gitpublication.ModePullRequest && !reflect.DeepEqual(s.gitPublications[operationID], publication)) {
			return base.ErrConflict
		}
		return nil
	}
	s.gitWriteCommands[operationID] = command
	s.gitPublicationModes[operationID] = mode
	if mode == gitpublication.ModePullRequest {
		s.gitPublications[operationID] = publication
	}
	return nil
}

func (s *Store) AcceptedGitWriteCommand(operationID string) (gitprojection.WriteCommand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.gitWriteCommands[operationID]
	if !exists {
		return gitprojection.WriteCommand{}, base.ErrNotFound
	}
	command.Content = append([]byte(nil), command.Content...)
	return command, nil
}

func (s *Store) AcceptedGitPublicationMode(operationID string) (gitpublication.Mode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mode, exists := s.gitPublicationModes[operationID]
	if !exists {
		return "", base.ErrNotFound
	}
	return mode, nil
}
