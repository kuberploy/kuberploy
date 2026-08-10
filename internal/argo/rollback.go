package argo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"go.yaml.in/yaml/v3"
)

type RollbackState string

const (
	RollbackPendingGit   RollbackState = "pending-git"
	RollbackGitCommitted RollbackState = "git-committed"
	RollbackFailed       RollbackState = "failed"
)

type RollbackCommand struct {
	ID                string        `json:"id"`
	ApplicationID     string        `json:"applicationId"`
	ProjectID         string        `json:"projectId"`
	EnvironmentID     string        `json:"environmentId"`
	BindingID         string        `json:"bindingId"`
	OperationID       string        `json:"operationId"`
	BaseRevision      string        `json:"baseRevision"`
	ExpectedETag      string        `json:"expectedETag"`
	ReleaseRepository string        `json:"releaseRepository"`
	ReleaseDigest     string        `json:"releaseDigest"`
	CandidateSHA256   string        `json:"candidateSha256"`
	State             RollbackState `json:"state"`
	GitRevision       string        `json:"gitRevision,omitempty"`
	FailureCode       string        `json:"failureCode,omitempty"`
	CreatedAt         time.Time     `json:"createdAt"`
	UpdatedAt         time.Time     `json:"updatedAt"`
}

func (r RollbackCommand) Validate() error {
	if !uuidRE.MatchString(r.ID) || !uuidRE.MatchString(r.ApplicationID) || !uuidRE.MatchString(r.ProjectID) || !uuidRE.MatchString(r.EnvironmentID) || !uuidRE.MatchString(r.BindingID) ||
		!uuidRE.MatchString(r.OperationID) || !commitRE.MatchString(r.BaseRevision) || !strongETag(r.ExpectedETag) || r.ReleaseRepository == "" ||
		!validReleaseRepository(r.ReleaseRepository) || !digestRE.MatchString(r.ReleaseDigest) ||
		!digestRE.MatchString(r.CandidateSHA256) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalid
	}
	switch r.State {
	case RollbackPendingGit:
		if r.GitRevision != "" || r.FailureCode != "" {
			return ErrInvalid
		}
	case RollbackGitCommitted:
		if !commitRE.MatchString(r.GitRevision) || r.FailureCode != "" {
			return ErrInvalid
		}
	case RollbackFailed:
		if r.GitRevision != "" || r.FailureCode == "" || len(r.FailureCode) > 64 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func strongETag(value string) bool {
	return len(value) == len(`"sha256:`)+64+1 && strings.HasPrefix(value, `"sha256:`) && strings.HasSuffix(value, `"`) && digestRE.MatchString(strings.Trim(value, `"`))
}

func validReleaseRepository(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsAny(value, "@\x00\r\n \t") && !strings.Contains(value, "..") && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/")
}

type RetainedReleaseVerifier interface {
	VerifyRetainedRelease(context.Context, string, string, string) error
}

// GitCommitter exposes only a desired-state Git mutation. There is
// intentionally no Argo Sync, Rollback, Patch, or Update method in this path.
type GitCommitter interface {
	CommitMutation(context.Context, gitprojection.Mutation) (string, error)
}

type RollbackStore interface {
	CreateRollback(context.Context, RollbackCommand, gitprojection.Mutation) (bool, error)
	Rollback(context.Context, string) (RollbackCommand, gitprojection.Mutation, error)
	CompleteRollback(context.Context, string, string, time.Time) error
	FailRollback(context.Context, string, string, time.Time) error
}

type RollbackService struct {
	Store     RollbackStore
	Artifacts RetainedReleaseVerifier
	Git       GitCommitter
}

type RollbackRequest struct {
	ID, OperationID, ApplicationID, EnvironmentID    string
	Binding                                          gitprojection.Binding
	BaseRevision, ExpectedETag                       string
	Current                                          []byte
	ReleaseRepository, ReleaseDigest, SourceRevision string
	Now                                              time.Time
}

func (s RollbackService) Plan(ctx context.Context, request RollbackRequest) (RollbackCommand, gitprojection.Mutation, error) {
	if s.Store == nil || s.Artifacts == nil || request.Binding.Validate() != nil || request.Binding.Kind != gitprojection.BindingEnvironment ||
		request.Binding.EnvironmentID != request.EnvironmentID || request.Binding.TargetHeadRevision != request.BaseRevision ||
		!uuidRE.MatchString(request.ID) || !uuidRE.MatchString(request.OperationID) || !uuidRE.MatchString(request.ApplicationID) || !uuidRE.MatchString(request.EnvironmentID) ||
		!commitRE.MatchString(request.BaseRevision) || !strongETag(request.ExpectedETag) || !digestRE.MatchString(request.ReleaseDigest) || !validReleaseRepository(request.ReleaseRepository) ||
		request.SourceRevision != "" && !commitRE.MatchString(request.SourceRevision) || request.Now.IsZero() {
		return RollbackCommand{}, gitprojection.Mutation{}, ErrInvalid
	}
	if err := s.Artifacts.VerifyRetainedRelease(ctx, request.ApplicationID, request.ReleaseRepository, request.ReleaseDigest); err != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, err
	}
	applicationPath, err := gitprojection.ApplicationPath(request.Binding, request.ApplicationID)
	if err != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, err
	}
	candidate, err := rollbackDocument(request.Current, request.ApplicationID, request.EnvironmentID, request.Binding.ProjectID, request.ReleaseRepository, request.ReleaseDigest, request.SourceRevision)
	if err != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, err
	}
	sum := sha256.Sum256(candidate)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	command := RollbackCommand{ID: request.ID, ApplicationID: request.ApplicationID, ProjectID: request.Binding.ProjectID, EnvironmentID: request.EnvironmentID, BindingID: request.Binding.ID,
		OperationID: request.OperationID, BaseRevision: request.BaseRevision, ExpectedETag: request.ExpectedETag, ReleaseRepository: request.ReleaseRepository,
		ReleaseDigest: request.ReleaseDigest, CandidateSHA256: digest, State: RollbackPendingGit, CreatedAt: request.Now.UTC(), UpdatedAt: request.Now.UTC()}
	mutation := gitprojection.Mutation{BindingID: request.Binding.ID, OperationID: request.OperationID, Path: applicationPath, BaseRevision: request.BaseRevision,
		ExpectedETag: request.ExpectedETag, Content: candidate, Message: fmt.Sprintf("rollback(%s): select retained release", request.ApplicationID)}
	if command.Validate() != nil || mutation.Validate(request.Binding) != nil {
		return RollbackCommand{}, gitprojection.Mutation{}, ErrInvalid
	}
	_, err = s.Store.CreateRollback(ctx, command, mutation)
	return command, mutation, err
}

func (s RollbackService) Execute(ctx context.Context, commandID string, now time.Time) (string, error) {
	if s.Store == nil || s.Git == nil || !uuidRE.MatchString(commandID) || now.IsZero() {
		return "", ErrInvalid
	}
	command, mutation, err := s.Store.Rollback(ctx, commandID)
	if err != nil {
		return "", err
	}
	if command.State == RollbackGitCommitted {
		return command.GitRevision, nil
	}
	if command.State != RollbackPendingGit {
		return "", ErrConflict
	}
	revision, err := s.Git.CommitMutation(ctx, mutation)
	if err != nil {
		// Provider/network/ref-lane failures are retryable against the same
		// immutable operation. The Git writer's operation trailer discovers a
		// push that succeeded before a lost database acknowledgement.
		return "", err
	}
	if !commitRE.MatchString(revision) {
		_ = s.Store.FailRollback(ctx, commandID, "invalid-git-revision", now)
		return "", errors.New("Git writer returned an invalid revision")
	}
	if err = s.Store.CompleteRollback(ctx, commandID, revision, now); err != nil {
		return "", err
	}
	return revision, nil
}

func rollbackDocument(current []byte, applicationID, environmentID, projectID, repository, digest, sourceRevision string) ([]byte, error) {
	parsed, _, diagnostics := appconfig.ParseAndValidate(current)
	if len(diagnostics) > 0 || boundString(parsed, "metadata", "id") != applicationID || boundString(parsed, "spec", "applicationId") != applicationID ||
		boundString(parsed, "spec", "environmentId") != environmentID || boundString(parsed, "spec", "projectId") != projectID {
		return nil, ErrInvalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(current))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, ErrInvalid
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrInvalid
	}
	release := mappingPath(&document, "spec", "delivery", "release")
	if release == nil || release.Kind != yaml.MappingNode || !setScalar(release, "repository", repository) || !setScalar(release, "digest", digest) {
		return nil, ErrInvalid
	}
	if sourceRevision != "" {
		if !commitRE.MatchString(sourceRevision) || !setScalar(release, "sourceRevision", sourceRevision) {
			return nil, ErrInvalid
		}
	} else {
		removeMappingKey(release, "sourceRevision")
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document.Content[0]); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	result := output.Bytes()
	after, _, afterDiagnostics := appconfig.ParseAndValidate(result)
	if len(afterDiagnostics) > 0 || boundString(after, "spec", "delivery", "release", "repository") != repository || boundString(after, "spec", "delivery", "release", "digest") != digest {
		return nil, ErrInvalid
	}
	return append([]byte(nil), result...), nil
}

func boundString(root map[string]any, keys ...string) string {
	var current any = root
	for _, key := range keys {
		mapping, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = mapping[key]
	}
	value, _ := current.(string)
	return value
}

func mappingPath(document *yaml.Node, keys ...string) *yaml.Node {
	if document == nil || len(document.Content) != 1 {
		return nil
	}
	current := document.Content[0]
	for _, wanted := range keys {
		if current.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for index := 0; index+1 < len(current.Content); index += 2 {
			if current.Content[index].Value == wanted {
				next = current.Content[index+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

func setScalar(mapping *yaml.Node, key, value string) bool {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return true
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	return true
}

func removeMappingKey(mapping *yaml.Node, key string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}

type memoryRollbackRecord struct {
	command  RollbackCommand
	mutation gitprojection.Mutation
}

type MemoryRollbackStore struct {
	mu         sync.Mutex
	values     map[string]memoryRollbackRecord
	operations map[string]string
}

func NewMemoryRollbackStore() *MemoryRollbackStore {
	return &MemoryRollbackStore{values: map[string]memoryRollbackRecord{}, operations: map[string]string{}}
}

func (s *MemoryRollbackStore) CreateRollback(_ context.Context, command RollbackCommand, mutation gitprojection.Mutation) (bool, error) {
	if !validRollbackCreate(command, mutation) {
		return false, ErrInvalid
	}
	sum := sha256.Sum256(mutation.Content)
	if command.CandidateSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.values[command.ID]; exists {
		if !sameRollbackCreate(current.command, current.mutation, command, mutation) {
			return false, ErrConflict
		}
		return false, nil
	}
	if id, exists := s.operations[command.OperationID]; exists {
		current := s.values[id]
		if !sameRollbackCreate(current.command, current.mutation, command, mutation) {
			return false, ErrConflict
		}
		return false, nil
	}
	s.values[command.ID] = memoryRollbackRecord{command: command, mutation: cloneMutation(mutation)}
	s.operations[command.OperationID] = command.ID
	return true, nil
}

func (s *MemoryRollbackStore) Rollback(_ context.Context, id string) (RollbackCommand, gitprojection.Mutation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[id]
	if !exists {
		return RollbackCommand{}, gitprojection.Mutation{}, ErrNotFound
	}
	return value.command, cloneMutation(value.mutation), nil
}

func (s *MemoryRollbackStore) CompleteRollback(_ context.Context, id, revision string, now time.Time) error {
	if !commitRE.MatchString(revision) || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[id]
	if !exists {
		return ErrNotFound
	}
	if value.command.State == RollbackGitCommitted && value.command.GitRevision == revision {
		return nil
	}
	if value.command.State != RollbackPendingGit {
		return ErrConflict
	}
	if now.Before(value.command.UpdatedAt) {
		return ErrConflict
	}
	value.command.State, value.command.GitRevision, value.command.UpdatedAt = RollbackGitCommitted, revision, now.UTC()
	s.values[id] = value
	return nil
}

func (s *MemoryRollbackStore) FailRollback(_ context.Context, id, code string, now time.Time) error {
	if code == "" || len(code) > 64 || now.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[id]
	if !exists {
		return ErrNotFound
	}
	if value.command.State != RollbackPendingGit {
		return ErrConflict
	}
	if now.Before(value.command.UpdatedAt) {
		return ErrConflict
	}
	value.command.State, value.command.FailureCode, value.command.UpdatedAt = RollbackFailed, code, now.UTC()
	s.values[id] = value
	return nil
}

func cloneMutation(value gitprojection.Mutation) gitprojection.Mutation {
	value.Content = append([]byte(nil), value.Content...)
	return value
}

func validRollbackCreate(command RollbackCommand, mutation gitprojection.Mutation) bool {
	expectedPath := path.Join(gitprojection.EnvironmentPrefix(command.ProjectID, command.EnvironmentID), "apps", command.ApplicationID, "app.yaml")
	return command.Validate() == nil && command.State == RollbackPendingGit && command.OperationID == mutation.OperationID && command.BindingID == mutation.BindingID &&
		command.BaseRevision == mutation.BaseRevision && command.ExpectedETag == mutation.ExpectedETag && mutation.Path == expectedPath &&
		len(mutation.Content) > 0 && len(mutation.Content) <= gitprojection.MaxDocumentBytes && len(mutation.Message) > 0 && len(mutation.Message) <= 512 &&
		utf8.ValidString(mutation.Message) && !strings.ContainsAny(mutation.Message, "\x00\r")
}

func sameRollbackCreate(left RollbackCommand, leftMutation gitprojection.Mutation, right RollbackCommand, rightMutation gitprojection.Mutation) bool {
	return left.ID == right.ID && left.ApplicationID == right.ApplicationID && left.ProjectID == right.ProjectID && left.EnvironmentID == right.EnvironmentID &&
		left.BindingID == right.BindingID && left.OperationID == right.OperationID && left.BaseRevision == right.BaseRevision && left.ExpectedETag == right.ExpectedETag &&
		left.ReleaseRepository == right.ReleaseRepository && left.ReleaseDigest == right.ReleaseDigest && left.CandidateSHA256 == right.CandidateSHA256 &&
		leftMutation.BindingID == rightMutation.BindingID && leftMutation.OperationID == rightMutation.OperationID && leftMutation.Path == rightMutation.Path &&
		leftMutation.BaseRevision == rightMutation.BaseRevision && leftMutation.ExpectedETag == rightMutation.ExpectedETag && leftMutation.Message == rightMutation.Message &&
		bytes.Equal(leftMutation.Content, rightMutation.Content)
}
