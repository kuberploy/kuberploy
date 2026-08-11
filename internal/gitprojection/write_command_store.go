package gitprojection

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func cloneWriteCommand(value WriteCommand) WriteCommand {
	value.Content = append([]byte(nil), value.Content...)
	if value.CommittedAt != nil {
		copyTime := *value.CommittedAt
		value.CommittedAt = &copyTime
	}
	if value.IndexedAt != nil {
		copyTime := *value.IndexedAt
		value.IndexedAt = &copyTime
	}
	return value
}

func (s *MemoryStore) PutWriteCommand(_ context.Context, command WriteCommand) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.bindings[command.Plan.BindingID]
	if !exists {
		return ErrNotFound
	}
	if command.Validate(binding) != nil || command.State != WriteCommandPending || command.Plan.Validate(binding) != nil {
		return ErrInvalid
	}
	if current, exists := s.writeCommands[command.OperationID]; exists {
		if !equalWriteCommand(current, command) {
			return ErrConflict
		}
		return nil
	}
	s.writeCommands[command.OperationID] = cloneWriteCommand(command)
	return nil
}

func equalWriteCommand(left, right WriteCommand) bool {
	return reflect.DeepEqual(left, right)
}

func (s *MemoryStore) WriteCommand(_ context.Context, operationID string) (WriteCommand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.writeCommands[operationID]
	if !exists {
		return WriteCommand{}, ErrNotFound
	}
	binding, exists := s.bindings[command.Plan.BindingID]
	if !exists || command.Validate(binding) != nil {
		return WriteCommand{}, ErrInvalid
	}
	return cloneWriteCommand(command), nil
}

func (s *MemoryStore) MarkWriteCommandCommitted(_ context.Context, operationID, revision string, now time.Time) (WriteCommand, error) {
	if !commitRE.MatchString(revision) || now.IsZero() {
		return WriteCommand{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command, exists := s.writeCommands[operationID]
	if !exists {
		return WriteCommand{}, ErrNotFound
	}
	if command.State != WriteCommandPending {
		if command.CommittedRevision == revision {
			return cloneWriteCommand(command), nil
		}
		return WriteCommand{}, ErrConflict
	}
	if now.Before(command.UpdatedAt) {
		return WriteCommand{}, ErrConflict
	}
	committedAt := now.UTC()
	command.State, command.CommittedRevision, command.CommittedAt, command.UpdatedAt = WriteCommandGitCommitted, revision, &committedAt, committedAt
	s.writeCommands[operationID] = command
	return cloneWriteCommand(command), nil
}

const writeCommandColumns = `operation_id::text,deployment_id::text,actor_id::text,binding_id::text,project_id::text,
	environment_id::text,application_id::text,target_ref,path,base_revision,precondition,expected_etag,chart_identity,
	policy_version,content,content_sha256,message,publication_mode,state,committed_revision,committed_at,indexed_generation,indexed_at,created_at,updated_at`

const variableWriteCommandColumns = `operation_id::text,actor_id::text,binding_id::text,project_id::text,environment_id::text,
	variable_scope,target_ref,path,base_revision,precondition,expected_etag,policy_version,content,content_sha256,message,publication_mode,
	state,committed_revision,committed_at,indexed_generation,indexed_at,request_digest,created_at,updated_at`

func scanWriteCommand(row rowScanner) (WriteCommand, error) {
	var command WriteCommand
	err := row.Scan(&command.OperationID, &command.DeploymentID, &command.ActorID, &command.Plan.BindingID, &command.Plan.ProjectID,
		&command.Plan.EnvironmentID, &command.Plan.ApplicationID, &command.TargetRef, &command.Path, &command.Plan.BaseRevision,
		&command.Plan.Precondition, &command.Plan.ExpectedETag, &command.Plan.ChartDigest, &command.Plan.PolicyVersion,
		&command.Content, &command.ContentSHA256, &command.Message, &command.PublicationMode, &command.State, &command.CommittedRevision, &command.CommittedAt,
		&command.IndexedGeneration, &command.IndexedAt, &command.CreatedAt, &command.UpdatedAt)
	if err != nil {
		return WriteCommand{}, classifyPostgres(err)
	}
	return command, nil
}

func scanVariableWriteCommand(row rowScanner) (WriteCommand, error) {
	var command WriteCommand
	err := row.Scan(&command.OperationID, &command.ActorID, &command.Plan.BindingID, &command.Plan.ProjectID, &command.Plan.EnvironmentID,
		&command.Plan.VariableScope, &command.TargetRef, &command.Path, &command.Plan.BaseRevision, &command.Plan.Precondition,
		&command.Plan.ExpectedETag, &command.Plan.PolicyVersion, &command.Content, &command.ContentSHA256, &command.Message,
		&command.PublicationMode, &command.State, &command.CommittedRevision, &command.CommittedAt, &command.IndexedGeneration,
		&command.IndexedAt, &command.RequestDigest, &command.CreatedAt, &command.UpdatedAt)
	if err != nil {
		return WriteCommand{}, classifyPostgres(err)
	}
	command.Plan.VariablePath = command.Path
	return command, nil
}

func (s *PostgreSQLStore) PutWriteCommand(ctx context.Context, command WriteCommand) error {
	binding, err := s.Binding(ctx, command.Plan.BindingID)
	if err != nil {
		return err
	}
	if command.Validate(binding) != nil || command.State != WriteCommandPending || command.Plan.Validate(binding) != nil {
		return ErrInvalid
	}
	var result pgconn.CommandTag
	if command.Plan.VariableScope != "" {
		result, err = s.pool.Exec(ctx, `INSERT INTO git_write_commands(operation_id,command_kind,actor_id,binding_id,project_id,environment_id,
			variable_scope,target_ref,path,base_revision,precondition,expected_etag,policy_version,content,content_sha256,message,publication_mode,state,request_digest,created_at,updated_at)
			VALUES($1,'variable-set',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'pending',$17,$18,$18) ON CONFLICT DO NOTHING`,
			command.OperationID, command.ActorID, command.Plan.BindingID, command.Plan.ProjectID, command.Plan.EnvironmentID,
			command.Plan.VariableScope, command.TargetRef, command.Path, command.Plan.BaseRevision, command.Plan.Precondition,
			command.Plan.ExpectedETag, command.Plan.PolicyVersion, command.Content, command.ContentSHA256, command.Message,
			command.PublicationMode, command.RequestDigest, command.CreatedAt)
	} else {
		result, err = s.pool.Exec(ctx, `INSERT INTO git_write_commands(operation_id,command_kind,deployment_id,actor_id,binding_id,project_id,
		environment_id,application_id,target_ref,path,base_revision,precondition,expected_etag,chart_identity,policy_version,
		content,content_sha256,message,publication_mode,state,created_at,updated_at)
		VALUES($1,'deployment',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'pending',$19,$19) ON CONFLICT DO NOTHING`,
			command.OperationID, command.DeploymentID, command.ActorID, command.Plan.BindingID, command.Plan.ProjectID,
			command.Plan.EnvironmentID, command.Plan.ApplicationID, command.TargetRef, command.Path, command.Plan.BaseRevision,
			command.Plan.Precondition, command.Plan.ExpectedETag, command.Plan.ChartDigest, command.Plan.PolicyVersion,
			command.Content, command.ContentSHA256, command.Message, command.PublicationMode, command.CreatedAt)
	}
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	current, err := s.WriteCommand(ctx, command.OperationID)
	if err != nil || !equalWriteCommand(current, command) {
		return ErrConflict
	}
	return nil
}

func (s *PostgreSQLStore) WriteCommand(ctx context.Context, operationID string) (WriteCommand, error) {
	command, err := scanWriteCommand(s.pool.QueryRow(ctx, `SELECT `+writeCommandColumns+` FROM git_write_commands WHERE operation_id=$1 AND command_kind='deployment'`, operationID))
	if errors.Is(err, ErrNotFound) {
		command, err = scanVariableWriteCommand(s.pool.QueryRow(ctx, `SELECT `+variableWriteCommandColumns+` FROM git_write_commands WHERE operation_id=$1 AND command_kind='variable-set'`, operationID))
	}
	if err != nil {
		return WriteCommand{}, err
	}
	binding, err := s.Binding(ctx, command.Plan.BindingID)
	if err != nil || command.Validate(binding) != nil {
		return WriteCommand{}, ErrInvalid
	}
	return command, nil
}

func (s *PostgreSQLStore) MarkWriteCommandCommitted(ctx context.Context, operationID, revision string, now time.Time) (WriteCommand, error) {
	if !commitRE.MatchString(revision) || now.IsZero() {
		return WriteCommand{}, ErrInvalid
	}
	command, err := scanWriteCommand(s.pool.QueryRow(ctx, `UPDATE git_write_commands SET state='git-committed',
		committed_revision=$2,committed_at=$3,updated_at=$3 WHERE operation_id=$1 AND command_kind='deployment' AND state='pending' AND updated_at<=$3
		RETURNING `+writeCommandColumns, operationID, revision, now.UTC()))
	if err == nil {
		return command, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return WriteCommand{}, err
	}
	command, err = scanVariableWriteCommand(s.pool.QueryRow(ctx, `UPDATE git_write_commands SET state='git-committed',
		committed_revision=$2,committed_at=$3,updated_at=$3 WHERE operation_id=$1 AND command_kind='variable-set' AND state='pending' AND updated_at<=$3
		RETURNING `+variableWriteCommandColumns, operationID, revision, now.UTC()))
	if err == nil {
		return command, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return WriteCommand{}, err
	}
	command, err = s.WriteCommand(ctx, operationID)
	if err != nil {
		return WriteCommand{}, err
	}
	if command.CommittedRevision != revision || command.State == WriteCommandPending {
		return WriteCommand{}, ErrConflict
	}
	return command, nil
}

var _ WriteCommandStore = (*MemoryStore)(nil)
var _ WriteCommandStore = (*PostgreSQLStore)(nil)
