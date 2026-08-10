package gitprojection

import (
	"context"
	"errors"
	"strings"
)

type VariableSetSnapshot struct {
	Scope           string         `json:"scope"`
	BindingID       string         `json:"bindingId"`
	ProjectID       string         `json:"projectId"`
	EnvironmentID   string         `json:"environmentId"`
	Path            string         `json:"path"`
	Present         bool           `json:"present"`
	ETag            string         `json:"etag,omitempty"`
	RawYAML         string         `json:"rawYaml,omitempty"`
	Document        map[string]any `json:"document,omitempty"`
	IndexedRevision string         `json:"indexedRevision"`
}

func (c *ControlPlane) variableBinding(ctx context.Context, actor, environmentID string) (Binding, error) {
	if err := c.validate(); err != nil || !uuidRE.MatchString(actor) || !uuidRE.MatchString(environmentID) {
		return Binding{}, ErrInvalid
	}
	environment, err := c.Catalog.GetEnvironmentForActor(ctx, actor, environmentID)
	if err != nil {
		return Binding{}, err
	}
	binding, err := c.Catalog.GetEnvironmentGitBindingForActor(ctx, actor, environmentID)
	if err != nil {
		return Binding{}, err
	}
	if binding.Validate() != nil || binding.Kind != BindingEnvironment || binding.ProjectID != environment.ProjectID || binding.EnvironmentID != environment.ID {
		return Binding{}, ErrProviderMismatch
	}
	return binding, nil
}

func (c *ControlPlane) VariableSets(ctx context.Context, actor, environmentID string) ([]VariableSetSnapshot, error) {
	binding, err := c.variableBinding(ctx, actor, environmentID)
	if err != nil {
		return nil, err
	}
	paths, err := DependencyPaths(binding)
	if err != nil {
		return nil, err
	}
	result := make([]VariableSetSnapshot, 0, 2)
	for index, variablePath := range paths {
		scope := "project"
		if index == 1 {
			scope = "environment"
		}
		snapshot := VariableSetSnapshot{Scope: scope, BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID, Path: variablePath, IndexedRevision: binding.IndexedRevision}
		document, documentErr := c.Store.Document(ctx, binding.ID, variablePath)
		if errors.Is(documentErr, ErrNotFound) {
			result = append(result, snapshot)
			continue
		}
		if documentErr != nil || document.Validate(binding) != nil || !document.Valid {
			if documentErr != nil {
				return nil, documentErr
			}
			return nil, ErrInvalid
		}
		snapshot.Present, snapshot.ETag, snapshot.RawYAML, snapshot.Document = true, `"`+document.ContentSHA256+`"`, string(document.Raw), cloneMap(document.Parsed)
		result = append(result, snapshot)
	}
	return result, nil
}

func (c *ControlPlane) PlanVariableMutation(ctx context.Context, actor, environmentID, scope, expectedETag string) (WritePlan, error) {
	binding, err := c.variableBinding(ctx, actor, environmentID)
	if err != nil {
		return WritePlan{}, err
	}
	paths, _ := DependencyPaths(binding)
	variablePath := ""
	if scope == "project" {
		variablePath = paths[0]
	} else if scope == "environment" {
		variablePath = paths[1]
	} else {
		return WritePlan{}, ErrInvalid
	}
	plan := WritePlan{BindingID: binding.ID, ProjectID: binding.ProjectID, EnvironmentID: binding.EnvironmentID, BaseRevision: binding.IndexedRevision, PolicyVersion: binding.ParserVersion, VariableScope: scope, VariablePath: variablePath}
	document, documentErr := c.Store.Document(ctx, binding.ID, variablePath)
	if errors.Is(documentErr, ErrNotFound) {
		if strings.TrimSpace(expectedETag) != "" {
			return WritePlan{}, ErrConflict
		}
		plan.Precondition = MutationCreateIfAbsent
	} else {
		if documentErr != nil || document.Validate(binding) != nil || !document.Valid {
			if documentErr != nil {
				return WritePlan{}, documentErr
			}
			return WritePlan{}, ErrInvalid
		}
		actual := `"` + document.ContentSHA256 + `"`
		if expectedETag == "" {
			return WritePlan{}, ErrPreconditionRequired
		}
		if !validStrongETag(expectedETag) || expectedETag != actual {
			return WritePlan{}, ErrConflict
		}
		plan.Precondition, plan.ExpectedETag = MutationMatchETag, actual
	}
	if plan.Validate(binding) != nil {
		return WritePlan{}, ErrStale
	}
	return plan, nil
}
