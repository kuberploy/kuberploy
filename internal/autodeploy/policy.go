package autodeploy

import (
	"bytes"
	"context"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/variablecompiler"
)

type BuildSourceIdentity struct {
	ID, ProjectID, ApplicationID string
}

type BuildDefinitionIdentity = BuildSourceIdentity

type PolicyCatalog interface {
	BuildDefinitionIdentity(context.Context, string) (BuildDefinitionIdentity, error)
	GetApplication(context.Context, string) (domain.Application, error)
	GetEnvironment(context.Context, string) (domain.Environment, error)
	GetDeployment(context.Context, string) (domain.Deployment, error)
	GetServiceAccount(context.Context, string) (domain.ServiceAccount, error)
}

type PolicyProjection interface {
	Bundle(context.Context, string, domain.Deployment, string, time.Duration) (gitprojection.Bundle, error)
}

type PolicyStore interface {
	PolicyCommandReplay(context.Context, string, string, string, string) (Policy, Revision, bool, error)
	CreatePolicy(context.Context, Policy, Revision, string, string, string) (Policy, Revision, bool, error)
	RevisePolicy(context.Context, Policy, Revision, string, string, string) (Policy, Revision, bool, error)
}

func (s *PolicyService) CommandReplay(ctx context.Context, actor, key, action, requestDigest, expectedApplicationID, expectedPolicyID string) (Policy, Revision, bool, error) {
	if s == nil || s.Store == nil || !uuidRE.MatchString(actor) || key == "" || !digestRE.MatchString(requestDigest) ||
		(action != "create" && action != "revise") || (action == "create" && !uuidRE.MatchString(expectedApplicationID)) ||
		(action == "revise" && expectedApplicationID != "" && !uuidRE.MatchString(expectedApplicationID)) ||
		(action == "create" && expectedPolicyID != "") || (action == "revise" && !uuidRE.MatchString(expectedPolicyID)) {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	policy, revision, found, err := s.Store.PolicyCommandReplay(ctx, actor, key, action, requestDigest)
	if err != nil || !found {
		return policy, revision, found, err
	}
	if expectedApplicationID != "" && policy.ApplicationID != expectedApplicationID || action == "revise" && policy.ID != expectedPolicyID {
		return Policy{}, Revision{}, false, ErrConflict
	}
	return policy, revision, true, nil
}

type PolicyStatus struct {
	Policy          Policy   `json:"policy"`
	CurrentRevision Revision `json:"currentRevision"`
}

type PolicyReader interface {
	PolicyForActor(context.Context, string, string) (PolicyStatus, error)
	PoliciesForApplication(context.Context, string, string) ([]PolicyStatus, error)
	PolicyRevisionsForActor(context.Context, string, string, int) ([]Revision, error)
	PolicyRunsForActor(context.Context, string, string, int) ([]Run, error)
}

type IDSource func() (string, error)

type PolicyService struct {
	Catalog    PolicyCatalog
	Projection PolicyProjection
	Store      PolicyStore
	NewID      IDSource
	Now        func() time.Time
}

type CreatePolicyInput struct {
	ExpectedApplicationID string
	BuildDefinitionID     string
	EnvironmentID         string
	TemplateDeploymentID  string
	ServiceActorID        string
	Enabled               bool
	IdempotencyKey        string
	RequestDigest         string
	RequestID             string
}

type RevisePolicyInput struct {
	Policy               Policy
	CurrentRevision      Revision
	TemplateDeploymentID string
	ServiceActorID       string
	Enabled              bool
	IdempotencyKey       string
	RequestDigest        string
	RequestID            string
}

func (s *PolicyService) Create(ctx context.Context, actor string, input CreatePolicyInput) (Policy, Revision, bool, error) {
	if s == nil || s.Catalog == nil || s.Store == nil || s.NewID == nil || !uuidRE.MatchString(actor) ||
		!uuidRE.MatchString(input.ExpectedApplicationID) || !uuidRE.MatchString(input.EnvironmentID) ||
		!uuidRE.MatchString(input.TemplateDeploymentID) || !uuidRE.MatchString(input.ServiceActorID) ||
		input.IdempotencyKey == "" || input.RequestDigest == "" || input.RequestID == "" {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	if policy, revision, found, err := s.CommandReplay(ctx, actor, input.IdempotencyKey, "create", input.RequestDigest, input.ExpectedApplicationID, ""); err != nil || found {
		return policy, revision, found, err
	}
	definition, err := s.Catalog.BuildDefinitionIdentity(ctx, input.ExpectedApplicationID)
	if err != nil {
		return Policy{}, Revision{}, false, err
	}
	if definition.ApplicationID != input.ExpectedApplicationID {
		return Policy{}, Revision{}, false, ErrConflict
	}
	environment, application, template, err := s.resolveTemplate(ctx, definition, input.EnvironmentID, input.TemplateDeploymentID, input.ServiceActorID)
	if err != nil {
		return Policy{}, Revision{}, false, err
	}
	policyID, err := s.NewID()
	if err != nil || !uuidRE.MatchString(policyID) {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	now := s.now()
	policy := Policy{ID: policyID, ProjectID: definition.ProjectID,
		ApplicationID: application.ID, EnvironmentID: environment.ID, CurrentRevision: 1, CreatedBy: actor, CreatedAt: now}
	revision := Revision{PolicyID: policy.ID, Revision: 1, Enabled: input.Enabled, Template: template,
		TemplateDigest: TemplateDigest(template), ServiceActorID: input.ServiceActorID, CreatedBy: actor, CreatedAt: now}
	if policy.Validate() != nil || revision.ValidateFor(policy) != nil {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	return s.Store.CreatePolicy(ctx, policy, revision, input.IdempotencyKey, input.RequestDigest, input.RequestID)
}

func (s *PolicyService) Revise(ctx context.Context, actor string, input RevisePolicyInput) (Policy, Revision, bool, error) {
	if s == nil || s.Store == nil || !uuidRE.MatchString(actor) || input.Policy.Validate() != nil ||
		input.IdempotencyKey == "" || input.RequestDigest == "" || input.RequestID == "" {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	if policy, revision, found, err := s.CommandReplay(ctx, actor, input.IdempotencyKey, "revise", input.RequestDigest, input.Policy.ApplicationID, input.Policy.ID); err != nil || found {
		return policy, revision, found, err
	}
	next := input.Policy
	next.CurrentRevision++
	var revision Revision
	if !input.Enabled {
		current := input.CurrentRevision
		if current.ValidateFor(input.Policy) != nil || current.Revision != input.Policy.CurrentRevision ||
			input.TemplateDeploymentID != "" && input.TemplateDeploymentID != current.Template.SourceDeploymentID ||
			input.ServiceActorID != "" && input.ServiceActorID != current.ServiceActorID {
			return Policy{}, Revision{}, false, ErrConflict
		}
		revision = current
		revision.Revision = next.CurrentRevision
		revision.Enabled = false
		revision.CreatedBy = actor
		revision.CreatedAt = s.now()
	} else {
		if s.Catalog == nil || !uuidRE.MatchString(input.TemplateDeploymentID) || !uuidRE.MatchString(input.ServiceActorID) {
			return Policy{}, Revision{}, false, ErrInvalid
		}
		definition, err := s.Catalog.BuildDefinitionIdentity(ctx, input.Policy.ApplicationID)
		if err != nil || definition.ProjectID != input.Policy.ProjectID || definition.ApplicationID != input.Policy.ApplicationID {
			return Policy{}, Revision{}, false, ErrConflict
		}
		_, _, template, err := s.resolveTemplate(ctx, definition, input.Policy.EnvironmentID, input.TemplateDeploymentID, input.ServiceActorID)
		if err != nil {
			return Policy{}, Revision{}, false, err
		}
		revision = Revision{PolicyID: next.ID, Revision: next.CurrentRevision, Enabled: true, Template: template,
			TemplateDigest: TemplateDigest(template), ServiceActorID: input.ServiceActorID, CreatedBy: actor, CreatedAt: s.now()}
	}
	if revision.ValidateFor(next) != nil {
		return Policy{}, Revision{}, false, ErrInvalid
	}
	return s.Store.RevisePolicy(ctx, input.Policy, revision, input.IdempotencyKey, input.RequestDigest, input.RequestID)
}

func (s *PolicyService) resolveTemplate(ctx context.Context, definition BuildSourceIdentity, environmentID, deploymentID, serviceActorID string) (domain.Environment, domain.Application, Template, error) {
	if !uuidRE.MatchString(definition.ID) || !uuidRE.MatchString(definition.ProjectID) || !uuidRE.MatchString(definition.ApplicationID) {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	application, err := s.Catalog.GetApplication(ctx, definition.ApplicationID)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, err
	}
	environment, err := s.Catalog.GetEnvironment(ctx, environmentID)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, err
	}
	deployment, err := s.Catalog.GetDeployment(ctx, deploymentID)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, err
	}
	serviceActor, err := s.Catalog.GetServiceAccount(ctx, serviceActorID)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, err
	}
	if application.ID != definition.ApplicationID || application.ProjectID != definition.ProjectID ||
		environment.ID != environmentID || environment.ProjectID != definition.ProjectID ||
		deployment.ID != deploymentID || deployment.ApplicationID != application.ID || deployment.EnvironmentID != environment.ID ||
		serviceActor.ID != serviceActorID || serviceActor.ProjectID != definition.ProjectID || serviceActor.DisabledAt != nil {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	if s.Projection == nil {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	bundle, err := s.Projection.Bundle(ctx, serviceActorID, deployment, "", 0)
	if err != nil || bundle.ETag == "" {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	applicationDocument, found := autoDeployApplicationDocument(bundle, deployment.ApplicationID)
	// GetDeployment returns the exact latest accepted AppConfig snapshot. Do
	// not combine its generation with an older indexed Git document while the
	// projection worker is still converging after a save.
	if !found || !applicationDocument.Valid || len(deployment.ConfigRaw) == 0 ||
		!bytes.Equal(applicationDocument.Raw, deployment.ConfigRaw) {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	dependencies, _, err := variablecompiler.CanonicalDependencyIntent(bundle.Dependencies, bundle.Documents)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	dependencyIntent := make([]appconfig.AutoDeployDependencyIntent, len(dependencies))
	for index, dependency := range dependencies {
		dependencyIntent[index] = appconfig.AutoDeployDependencyIntent{Path: dependency.Path, Present: dependency.Present,
			BlobID: dependency.BlobID, ContentSHA256: dependency.ContentSHA256}
	}
	intent, _, diagnostics := appconfig.AutoDeployIntentTemplate(applicationDocument.Raw)
	if len(diagnostics) != 0 {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	intent, digest, err := appconfig.BindAutoDeployDependencies(intent, dependencyIntent)
	if err != nil {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	template := Template{SourceDeploymentID: deployment.ID, SourceDeploymentGeneration: deployment.Generation,
		SourceConfigETag: bundle.ETag, ConfigIntent: intent}
	if template.Validate(digest) != nil {
		return domain.Environment{}, domain.Application{}, Template{}, ErrConflict
	}
	return environment, application, template, nil
}

func autoDeployApplicationDocument(bundle gitprojection.Bundle, applicationID string) (gitprojection.Document, bool) {
	var result gitprojection.Document
	found := false
	for _, document := range bundle.Documents {
		if document.ApplicationID != applicationID {
			continue
		}
		if found {
			return gitprojection.Document{}, false
		}
		result, found = document, true
	}
	return result, found
}

func (s *PolicyService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
