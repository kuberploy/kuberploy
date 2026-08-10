package memory

import (
	"context"
	"reflect"

	"github.com/kuberploy/kuberploy/internal/gitpublication"
)

func cloneGitPublication(value gitpublication.Publication) gitpublication.Publication {
	if value.ProviderObservedAt != nil {
		copy := *value.ProviderObservedAt
		value.ProviderObservedAt = &copy
	}
	return value
}

func (s *Store) CreatePublication(_ context.Context, publication gitpublication.Publication) error {
	if publication.Validate() != nil || publication.State != gitpublication.StatePendingCandidate || publication.Version != 1 {
		return gitpublication.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.gitPublications[publication.OperationID]; exists {
		if reflect.DeepEqual(current, publication) {
			return nil
		}
		return gitpublication.ErrConflict
	}
	command, exists := s.gitWriteCommands[publication.OperationID]
	if !exists || s.gitPublicationModes[publication.OperationID] != gitpublication.ModePullRequest ||
		command.Plan.BindingID != publication.BindingID || command.TargetRef != publication.TargetRef ||
		command.Plan.BaseRevision != publication.BaseRevision {
		return gitpublication.ErrInvalid
	}
	s.gitPublications[publication.OperationID] = cloneGitPublication(publication)
	return nil
}

func (s *Store) Publication(_ context.Context, operationID string) (gitpublication.Publication, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	publication, exists := s.gitPublications[operationID]
	if !exists {
		return gitpublication.Publication{}, gitpublication.ErrNotFound
	}
	if publication.Validate() != nil {
		return gitpublication.Publication{}, gitpublication.ErrInvalid
	}
	return cloneGitPublication(publication), nil
}

func (s *Store) CompareAndSwapPublication(_ context.Context, previous, next gitpublication.Publication) error {
	if gitpublication.ValidateTransition(previous, next) != nil {
		return gitpublication.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.gitPublications[previous.OperationID]
	if !exists {
		return gitpublication.ErrNotFound
	}
	if !reflect.DeepEqual(current, previous) {
		return gitpublication.ErrConflict
	}
	s.gitPublications[next.OperationID] = cloneGitPublication(next)
	if operation, ok := s.operations[next.OperationID]; ok {
		if deployment, exists := s.deployments[operation.TargetID]; exists && deployment.OperationID == operation.ID && deployment.Generation == operation.Generation {
			deployment.State = protectedDeploymentState(next)
			deployment.UpdatedAt = next.UpdatedAt
			s.deployments[deployment.ID] = deployment
		}
	}
	return nil
}

var _ gitpublication.Store = (*Store)(nil)
