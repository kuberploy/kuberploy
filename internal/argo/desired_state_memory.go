package argo

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"sync"
	"time"
)

type MemoryDesiredStateStore struct {
	mu       sync.Mutex
	commands map[string]DesiredStateCommand
	ready    map[string]DesiredStateRuntimeLease
}

func NewMemoryDesiredStateStore() *MemoryDesiredStateStore {
	return &MemoryDesiredStateStore{commands: map[string]DesiredStateCommand{}, ready: map[string]DesiredStateRuntimeLease{}}
}

func cloneDesiredStateCommand(value DesiredStateCommand) DesiredStateCommand {
	value.Content = append([]byte(nil), value.Content...)
	value.CommittedAt = cloneTimePointer(value.CommittedAt)
	value.VerifiedAt = cloneTimePointer(value.VerifiedAt)
	value.CompletedAt = cloneTimePointer(value.CompletedAt)
	value.WriteBaseObservedAt = cloneTimePointer(value.WriteBaseObservedAt)
	if value.Lease != nil {
		lease := *value.Lease
		value.Lease = &lease
	}
	return value
}

func equalDesiredStateCommand(left, right DesiredStateCommand) bool {
	leftContent, rightContent := left.Content, right.Content
	left.Content, right.Content = nil, nil
	return reflect.DeepEqual(left, right) && bytes.Equal(leftContent, rightContent)
}

func (s *MemoryDesiredStateStore) CreateDesiredState(_ context.Context, command DesiredStateCommand) (bool, error) {
	if command.Validate() != nil || command.Lease != nil || command.WriteBaseRevision != "" || command.WriteBaseObservedAt != nil ||
		command.State != DesiredStatePending && command.State != DesiredStateBlockedPrerequisite {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.commands[command.ID]; exists {
		if equalDesiredStateCommand(current, command) {
			return false, nil
		}
		return false, ErrConflict
	}
	latestGeneration := int64(0)
	for _, current := range s.commands {
		if current.EnvironmentID != command.EnvironmentID {
			continue
		}
		if current.Generation == command.Generation {
			return false, ErrConflict
		}
		if isLiveDesiredState(current.State) {
			return false, ErrConflict
		}
		if current.Generation > latestGeneration {
			latestGeneration = current.Generation
		}
	}
	if command.Generation != latestGeneration+1 {
		return false, ErrConflict
	}
	s.commands[command.ID] = cloneDesiredStateCommand(command)
	return true, nil
}

func (s *MemoryDesiredStateStore) DesiredStateCommand(_ context.Context, commandID string) (DesiredStateCommand, error) {
	if !uuidRE.MatchString(commandID) {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[commandID]
	if !exists {
		return DesiredStateCommand{}, ErrNotFound
	}
	return cloneDesiredStateCommand(command), nil
}

func (s *MemoryDesiredStateStore) LatestDesiredState(_ context.Context, projectID, environmentID string) (DesiredStateStatus, error) {
	if !uuidRE.MatchString(projectID) || !uuidRE.MatchString(environmentID) {
		return DesiredStateStatus{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest DesiredStateCommand
	found := false
	for _, command := range s.commands {
		if command.ProjectID == projectID && command.EnvironmentID == environmentID && (!found || command.Generation > latest.Generation) {
			latest, found = command, true
		}
	}
	if !found {
		return DesiredStateStatus{}, ErrNotFound
	}
	return latest.Status(), nil
}

func (s *MemoryDesiredStateStore) ClaimDesiredState(_ context.Context, owner string, identity DesiredStateWorkerIdentity, now time.Time, leaseDuration time.Duration) (DesiredStateWork, error) {
	if !desiredStateOwnerRE.MatchString(owner) || identity.Validate() != nil || now.IsZero() || !validDesiredStateLeaseDuration(leaseDuration) {
		return DesiredStateWork{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := make([]DesiredStateCommand, 0, len(s.commands))
	for _, command := range s.commands {
		if isClaimableDesiredState(command, now) {
			candidates = append(candidates, command)
		}
	}
	slices.SortFunc(candidates, func(left, right DesiredStateCommand) int {
		if compared := left.NextAttemptAt.Compare(right.NextAttemptAt); compared != 0 {
			return compared
		}
		if compared := left.CreatedAt.Compare(right.CreatedAt); compared != 0 {
			return compared
		}
		return stringsCompare(left.ID, right.ID)
	})
	for _, candidate := range candidates {
		if s.bindingLeaseHeld(candidate.PlatformBindingID, candidate.ID, now) {
			continue
		}
		command := s.commands[candidate.ID]
		lease := DesiredStateLease{CommandID: command.ID, Owner: owner, Epoch: command.LeaseEpoch + 1,
			Until: now.UTC().Add(leaseDuration), Contract: identity.ContractVersion, ConfigDigest: identity.ConfigDigest}
		command.Lease, command.LeaseEpoch, command.UpdatedAt = &lease, lease.Epoch, now.UTC()
		if command.State != DesiredStateGitCommitted {
			command.State = DesiredStateClaimed
		}
		s.commands[command.ID] = command
		return DesiredStateWork{Command: cloneDesiredStateCommand(command), Lease: lease}, nil
	}
	return DesiredStateWork{}, ErrNotFound
}

func (s *MemoryDesiredStateStore) HeartbeatDesiredState(_ context.Context, lease DesiredStateLease, now time.Time, leaseDuration time.Duration) (DesiredStateLease, error) {
	if lease.Validate() != nil || now.IsZero() || !validDesiredStateLeaseDuration(leaseDuration) {
		return DesiredStateLease{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateLease{}, ErrLeaseLost
	}
	updated := lease
	updated.Until = now.UTC().Add(leaseDuration)
	command.Lease, command.UpdatedAt = &updated, now.UTC()
	s.commands[command.ID] = command
	return updated, nil
}

func (s *MemoryDesiredStateStore) BindDesiredStateWriteBase(_ context.Context, lease DesiredStateLease, revision string, observedAt, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || observedAt.IsZero() || now.IsZero() || observedAt.After(now) {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	if command.WriteBaseRevision != "" {
		if command.WriteBaseRevision != revision || command.WriteBaseObservedAt == nil || !command.WriteBaseObservedAt.Equal(observedAt) {
			return DesiredStateCommand{}, ErrConflict
		}
		return cloneDesiredStateCommand(command), nil
	}
	if command.State != DesiredStateClaimed || observedAt.Before(command.CreatedAt) || now.Before(command.UpdatedAt) {
		return DesiredStateCommand{}, ErrConflict
	}
	observed := observedAt.UTC()
	command.WriteBaseRevision, command.WriteBaseObservedAt, command.UpdatedAt = revision, &observed, now.UTC()
	s.commands[command.ID] = command
	return cloneDesiredStateCommand(command), nil
}

func (s *MemoryDesiredStateStore) MarkDesiredStateGitCommitted(_ context.Context, lease DesiredStateLease, revision string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	if command.State == DesiredStateGitCommitted {
		if command.CommittedRevision != revision {
			return DesiredStateCommand{}, ErrConflict
		}
		return cloneDesiredStateCommand(command), nil
	}
	if command.State != DesiredStateClaimed || now.Before(command.UpdatedAt) {
		return DesiredStateCommand{}, ErrConflict
	}
	committedAt := now.UTC()
	command.State, command.CommittedRevision, command.CommittedAt, command.UpdatedAt = DesiredStateGitCommitted, revision, &committedAt, committedAt
	s.commands[command.ID] = command
	return cloneDesiredStateCommand(command), nil
}

func (s *MemoryDesiredStateStore) CompleteDesiredStateVerified(_ context.Context, lease DesiredStateLease, revision string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !commitRE.MatchString(revision) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	if command.State != DesiredStateGitCommitted || command.CommittedRevision != revision || command.CommittedAt == nil || now.Before(*command.CommittedAt) {
		return DesiredStateCommand{}, ErrConflict
	}
	verifiedAt := now.UTC()
	command.State, command.VerifiedAt, command.CompletedAt, command.UpdatedAt, command.Lease = DesiredStateVerified, &verifiedAt, &verifiedAt, verifiedAt, nil
	s.commands[command.ID] = command
	return cloneDesiredStateCommand(command), nil
}

func (s *MemoryDesiredStateStore) RetryDesiredState(_ context.Context, lease DesiredStateLease, retry DesiredStateRetry, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || retry.Validate(now) != nil {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	if command.State != DesiredStateClaimed && command.State != DesiredStateGitCommitted {
		return DesiredStateCommand{}, ErrConflict
	}
	if command.State == DesiredStateClaimed {
		command.State = DesiredStatePending
	}
	command.NextAttemptAt, command.ConsecutiveFailures, command.LastFailureCode = retry.NextAttemptAt.UTC(), saturatingDesiredStateFailures(command.ConsecutiveFailures), retry.FailureCode
	command.Lease, command.UpdatedAt = nil, now.UTC()
	s.commands[command.ID] = command
	return cloneDesiredStateCommand(command), nil
}

func (s *MemoryDesiredStateStore) FailDesiredState(_ context.Context, lease DesiredStateLease, failureCode string, now time.Time) (DesiredStateCommand, error) {
	if lease.Validate() != nil || !failureCodeRE.MatchString(failureCode) || now.IsZero() {
		return DesiredStateCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.commands[lease.CommandID]
	if !exists || !activeDesiredStateLease(command, lease, now) {
		return DesiredStateCommand{}, ErrLeaseLost
	}
	if command.State != DesiredStateClaimed {
		return DesiredStateCommand{}, ErrConflict
	}
	completedAt := now.UTC()
	command.State, command.ConsecutiveFailures, command.LastFailureCode = DesiredStateFailed, saturatingDesiredStateFailures(command.ConsecutiveFailures), failureCode
	command.Lease, command.UpdatedAt, command.CompletedAt = nil, completedAt, &completedAt
	s.commands[command.ID] = command
	return cloneDesiredStateCommand(command), nil
}

func validDesiredStateLeaseDuration(duration time.Duration) bool {
	return duration >= minimumDesiredStateLease && duration <= maximumDesiredStateLease
}

func isLiveDesiredState(state DesiredStateCommandState) bool {
	return state == DesiredStatePending || state == DesiredStateClaimed || state == DesiredStateGitCommitted
}

func isClaimableDesiredState(command DesiredStateCommand, now time.Time) bool {
	if !isLiveDesiredState(command.State) || command.NextAttemptAt.After(now) {
		return false
	}
	return command.Lease == nil || !command.Lease.Until.After(now)
}

func (s *MemoryDesiredStateStore) bindingLeaseHeld(bindingID, exceptID string, now time.Time) bool {
	for _, command := range s.commands {
		if command.ID != exceptID && command.PlatformBindingID == bindingID && command.Lease != nil && command.Lease.Until.After(now) {
			return true
		}
	}
	return false
}

func activeDesiredStateLease(command DesiredStateCommand, lease DesiredStateLease, now time.Time) bool {
	return command.Lease != nil && *command.Lease == lease && lease.Until.After(now)
}

func saturatingDesiredStateFailures(current int) int {
	if current >= 30 {
		return 30
	}
	return current + 1
}

func stringsCompare(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

var _ DesiredStateStore = (*MemoryDesiredStateStore)(nil)
