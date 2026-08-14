package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/secrets"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func appConfigCertificateReferences(parsed map[string]any) ([]certificates.ReferenceSelection, error) {
	values, err := appconfig.CertificateReferences(parsed)
	if err != nil {
		return nil, base.ErrPreconditionFailed
	}
	result := make([]certificates.ReferenceSelection, 0, len(values))
	for _, value := range values {
		reference := certificates.Reference{BindingID: value.BindingID, Name: value.Name, Version: value.Version}
		if reference.Validate() != nil {
			return nil, base.ErrPreconditionFailed
		}
		result = append(result, certificates.ReferenceSelection{Host: value.Host, Reference: reference})
	}
	return result, nil
}

func (s *Store) validateCertificateReferencesTx(
	ctx context.Context, tx pgx.Tx, actor string, referencePlan *base.AppConfigReferencePlan,
	projectID, environmentID, applicationID string, selections []certificates.ReferenceSelection, now time.Time,
) (certificates.ReferencePlan, error) {
	if s == nil || s.certificateReferences == nil || tx == nil || referencePlan == nil ||
		referencePlan.CertificateDigest == "" || len(selections) == 0 || now.IsZero() {
		return certificates.ReferencePlan{}, base.ErrPreconditionFailed
	}
	scope, err := certificateReferenceScopeTx(ctx, tx, projectID, environmentID, applicationID)
	if err != nil {
		return certificates.ReferencePlan{}, err
	}
	plan, err := s.certificateReferences.ResolveCertificateReferencesTx(ctx, tx, scope, selections, now.UTC())
	if err != nil {
		return certificates.ReferencePlan{}, classifyCertificateReferenceError(err)
	}
	digest, err := plan.Digest()
	if err != nil || digest != referencePlan.CertificateDigest {
		return certificates.ReferencePlan{}, base.ErrPreconditionFailed
	}
	authorized := map[string]struct{}{}
	for _, use := range plan.Uses {
		if _, exists := authorized[use.Reference.BindingID]; exists {
			continue
		}
		target := domain.AccessTarget{Type: "secret-binding", ID: use.Reference.BindingID, TeamID: scope.OrganizationID,
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ApplicationID: scope.ApplicationID,
			Namespace: scope.Namespace}
		if err = authorizeWith(ctx, tx, actor, domain.PermissionSecretsBind, target); err != nil {
			return certificates.ReferencePlan{}, err
		}
		authorized[use.Reference.BindingID] = struct{}{}
	}
	return plan, nil
}

func certificateReferenceScopeTx(ctx context.Context, tx pgx.Tx, projectID, environmentID, applicationID string) (secrets.Scope, error) {
	if tx == nil {
		return secrets.Scope{}, base.ErrPreconditionFailed
	}
	var organizationID, namespace string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(p.team_id::text,''),e.namespace
		FROM projects p JOIN environments e ON e.project_id=p.id AND e.id=$2
		JOIN applications a ON a.project_id=p.id AND a.id=$3 WHERE p.id=$1
		FOR SHARE OF p,e,a`, projectID, environmentID, applicationID).Scan(&organizationID, &namespace); err != nil {
		return secrets.Scope{}, classify(err)
	}
	scope := secrets.Scope{OrganizationID: organizationID, ProjectID: projectID, EnvironmentID: environmentID,
		ApplicationID: applicationID, Namespace: namespace}
	if scope.Validate() != nil {
		return secrets.Scope{}, base.ErrPreconditionFailed
	}
	return scope, nil
}

func classifyCertificateReferenceError(err error) error {
	if errors.Is(err, certificates.ErrInvalid) || errors.Is(err, certificates.ErrNotFound) ||
		errors.Is(err, certificates.ErrNotReady) || errors.Is(err, certificates.ErrHostMismatch) ||
		errors.Is(err, certificates.ErrConflict) {
		return base.ErrPreconditionFailed
	}
	return err
}
