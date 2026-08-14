// Package projectionpolicy applies database-backed dynamic AppConfig policy
// inside the exact Git projection generation activation transaction.
package projectionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
)

var namespaceRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)

// DocumentScope is the server-resolved tenant, destination, Git and AppConfig
// identity supplied to reference policies. OrganizationID is empty exactly
// when the durable project is personal or platform-owned.
type DocumentScope struct {
	Binding        gitprojection.Binding
	OrganizationID string
	Namespace      string
	ApplicationID  string
	Path           string
	SourceRevision string
	ConfigRevision string
	ContentSHA256  string
}

// ReferencePolicy owns one dynamic reference family, such as runtime secrets
// or registry pull profiles. Semantic unresolved references return diagnostics
// and leave existing guards unchanged; infrastructure/store errors are
// returned so generation activation rolls back and retries.
type ReferencePolicy interface {
	ValidateCurrentTx(context.Context, pgx.Tx, AppConfigPolicyDocument, time.Time) ([]gitprojection.Diagnostic, error)
	ReconcileDeletedTx(context.Context, pgx.Tx, DocumentScope, time.Time) error
}

// CurrentReferenceReconciler mutates deletion guards only after every policy
// family and cross-document conflict check accepted the exact current
// AppConfig. It runs inside the document savepoint used to fence legacy policy
// seams that still combine validation with reconciliation.
type CurrentReferenceReconciler interface {
	ReconcileCurrentTx(context.Context, pgx.Tx, AppConfigPolicyDocument, time.Time) error
}

// ExternalDNSRuntimePolicy is intentionally separate from configured profile
// metadata. A configured/assigned profile never implies a ready controller.
type ExternalDNSRuntimePolicy interface {
	ReadyExternalDNSIntegrationTx(context.Context, pgx.Tx, string, string, time.Time) (bool, error)
}

type MiddlewareRuntimePolicy interface {
	ValidateMaterializedTx(context.Context, pgx.Tx, []middlewareprofiles.MaterializedDefinition, middlewareprofiles.Target, string, string, string, time.Time) (bool, error)
	ReconcileDeletedTx(context.Context, pgx.Tx, string) error
}

// Validator is the closed production composite. External DNS assignment and
// hostname policy are built in. Runtime-secret and registry reference families
// are injected explicitly and execute in the same serializable transaction.
type Validator struct {
	ExternalDNSRuntime ExternalDNSRuntimePolicy
	Middleware         MiddlewareRuntimePolicy
	Edge               ReferencePolicy
	Secrets            ReferencePolicy
	Registry           ReferencePolicy
}

func (v *Validator) ValidateAppConfigs(_ context.Context, input gitprojection.AppConfigPolicyInput) (gitprojection.AppConfigPolicyValidation, error) {
	if input.Validate() != nil {
		return gitprojection.AppConfigPolicyValidation{}, gitprojection.ErrInvalid
	}
	// The portable method cannot reconcile durable reference guards. Production
	// PostgreSQL activation always selects ValidateAppConfigsTx below.
	return gitprojection.AppConfigPolicyValidation{}, gitprojection.ErrPolicyUnavailable
}

func (v *Validator) ValidateAppConfigsTx(ctx context.Context, tx pgx.Tx, input gitprojection.AppConfigPolicyInput, now time.Time) (gitprojection.AppConfigPolicyValidation, error) {
	if v == nil || tx == nil || input.Validate() != nil || now.IsZero() {
		return gitprojection.AppConfigPolicyValidation{}, gitprojection.ErrInvalid
	}
	validation := gitprojection.AppConfigPolicyValidation{Diagnostics: map[string][]gitprojection.Diagnostic{}}
	currentPaths := make(map[string]struct{}, len(input.Current))
	currentDocuments := make([]AppConfigPolicyDocument, 0, len(input.Current))
	type routeOwner struct {
		path  string
		index int
	}
	routeOwners := map[string][]routeOwner{}
	for _, document := range input.Current {
		if document.ApplicationID == "" {
			continue
		}
		currentPaths[document.Path] = struct{}{}
		// Dynamic resolvers never reinterpret a schema-invalid or identity-
		// mismatched document, and they never mutate its previous references.
		if len(document.Diagnostics) != 0 {
			continue
		}
		parsed, runtime, diagnostics := appconfig.ParseAndValidate(document.Raw)
		if len(diagnostics) != 0 || parsed == nil {
			return gitprojection.AppConfigPolicyValidation{}, gitprojection.ErrConflict
		}
		scope, err := resolveDocumentScopeTx(ctx, tx, input.Binding, document.ApplicationID, document.Path, input.Generation.HeadRevision, document.ConfigRevision, document.ContentSHA256)
		if err != nil {
			return gitprojection.AppConfigPolicyValidation{}, err
		}
		policyDocument, err := newAppConfigPolicyDocument(scope, parsed, runtime)
		if err != nil {
			// The indexer and activation transaction must agree on the exact
			// schema-valid typed view. Any disagreement is an activation conflict,
			// never a partially interpreted policy document.
			return gitprojection.AppConfigPolicyValidation{}, gitprojection.ErrConflict
		}
		currentDocuments = append(currentDocuments, policyDocument)
		for index, route := range policyDocument.Routes() {
			ingressClass := route.IngressClassName
			if ingressClass == "" {
				ingressClass = "traefik"
			}
			key := ingressClass + "\x00" + route.Host + "\x00" + route.Path
			routeOwners[key] = append(routeOwners[key], routeOwner{path: document.Path, index: index})
		}
	}
	conflictKeys := make([]string, 0, len(routeOwners))
	for key, owners := range routeOwners {
		if len(owners) > 1 {
			conflictKeys = append(conflictKeys, key)
		}
	}
	sort.Strings(conflictKeys)
	for _, key := range conflictKeys {
		for _, owner := range routeOwners[key] {
			validation.Diagnostics[owner.path] = append(validation.Diagnostics[owner.path], gitprojection.Diagnostic{
				Code: "RouteConflict", Detail: "The exact ingress class, hostname, and path are already claimed by another AppConfig in this environment.",
				Pointer: "/spec/routes/" + strconv.Itoa(owner.index) + "/host",
			})
		}
	}
	// Some legacy policy seams both validate and reconcile their current
	// references. Fence each document behind a savepoint so any diagnostic from
	// any later family (or a cross-document route conflict computed above)
	// discards every current-reference/artifact mutation for that document.
	// Infrastructure errors still abort the outer serializable activation.
	for _, policyDocument := range currentDocuments {
		scope, runtime := policyDocument.Scope(), policyDocument.Runtime()
		if _, err := tx.Exec(ctx, "SAVEPOINT appconfig_policy_document"); err != nil {
			return gitprojection.AppConfigPolicyValidation{}, err
		}
		policyDiagnostics := append([]gitprojection.Diagnostic(nil), validation.Diagnostics[scope.Path]...)
		externalDNSDiagnostics, err := v.externalDNSDiagnosticsTx(ctx, tx, scope, policyDocument, now)
		if err != nil {
			return gitprojection.AppConfigPolicyValidation{}, err
		}
		policyDiagnostics = append(policyDiagnostics, externalDNSDiagnostics...)
		definitions := policyDocument.MiddlewareDefinitions()
		hasReusableMiddleware := false
		for _, definition := range definitions {
			hasReusableMiddleware = hasReusableMiddleware || definition.ProfileRef != nil
		}
		if hasReusableMiddleware {
			if v.Middleware == nil {
				policyDiagnostics = append(policyDiagnostics, gitprojection.Diagnostic{Code: "MiddlewareProfilePolicyUnavailable", Detail: "Reusable middleware profiles cannot be resolved by the active projection policy.", Pointer: "/spec/middlewares"})
			} else {
				matched, middlewareErr := v.Middleware.ValidateMaterializedTx(ctx, tx, definitions, middlewareprofiles.Target{ProjectID: scope.Binding.ProjectID, EnvironmentID: scope.Binding.EnvironmentID, ApplicationID: scope.ApplicationID}, scope.ApplicationID, scope.Binding.EnvironmentID, scope.Path, now)
				if middlewareErr != nil {
					return gitprojection.AppConfigPolicyValidation{}, middlewareErr
				}
				if !matched {
					policyDiagnostics = append(policyDiagnostics, gitprojection.Diagnostic{Code: "MiddlewareProfileMismatch", Detail: "A reusable middleware profile is stale, inactive, unassigned, or its materialized specification was changed.", Pointer: "/spec/middlewares"})
				}
			}
		} else if v.Middleware != nil {
			if _, middlewareErr := v.Middleware.ValidateMaterializedTx(ctx, tx, definitions, middlewareprofiles.Target{ProjectID: scope.Binding.ProjectID, EnvironmentID: scope.Binding.EnvironmentID, ApplicationID: scope.ApplicationID}, scope.ApplicationID, scope.Binding.EnvironmentID, scope.Path, now); middlewareErr != nil {
				return gitprojection.AppConfigPolicyValidation{}, middlewareErr
			}
		}
		if v.Edge == nil {
			if len(policyDocument.Routes()) != 0 {
				policyDiagnostics = append(policyDiagnostics, routeDiagnosticForAll(policyDocument.Routes(), "EdgeRoutePolicyUnavailable", "Route intent cannot be resolved by the active edge policy.", "")...)
			}
		} else {
			edgeDiagnostics, edgeErr := v.Edge.ValidateCurrentTx(ctx, tx, policyDocument, now)
			if edgeErr != nil {
				return gitprojection.AppConfigPolicyValidation{}, edgeErr
			}
			policyDiagnostics = append(policyDiagnostics, edgeDiagnostics...)
		}
		if v.Secrets == nil {
			if runtimeUsesSecrets(runtime) {
				policyDiagnostics = append(policyDiagnostics, gitprojection.Diagnostic{Code: "SecretReferencePolicyUnavailable", Detail: "Runtime-secret references cannot be resolved by the active projection policy.", Pointer: "/spec/runtime/env"})
			}
		} else {
			secretDiagnostics, secretErr := v.Secrets.ValidateCurrentTx(ctx, tx, policyDocument, now)
			if secretErr != nil {
				return gitprojection.AppConfigPolicyValidation{}, secretErr
			}
			policyDiagnostics = append(policyDiagnostics, secretDiagnostics...)
		}
		if v.Registry == nil {
			if policyDocument.Delivery().HasRegistryPull {
				policyDiagnostics = append(policyDiagnostics, gitprojection.Diagnostic{Code: "RegistryPullReferencePolicyUnavailable", Detail: "Private-image pull metadata cannot be resolved by the active projection policy.", Pointer: "/spec/delivery/registryPull"})
			}
		} else {
			registryDiagnostics, registryErr := v.Registry.ValidateCurrentTx(ctx, tx, policyDocument, now)
			if registryErr != nil {
				return gitprojection.AppConfigPolicyValidation{}, registryErr
			}
			policyDiagnostics = append(policyDiagnostics, registryDiagnostics...)
		}
		if len(policyDiagnostics) != 0 {
			validation.Diagnostics[scope.Path] = policyDiagnostics
			if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT appconfig_policy_document"); err != nil {
				return gitprojection.AppConfigPolicyValidation{}, err
			}
		} else {
			for _, family := range []ReferencePolicy{v.Edge, v.Secrets, v.Registry} {
				if reconciler, ok := family.(CurrentReferenceReconciler); ok {
					if err = reconciler.ReconcileCurrentTx(ctx, tx, policyDocument, now); err != nil {
						return gitprojection.AppConfigPolicyValidation{}, err
					}
				}
			}
		}
		if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT appconfig_policy_document"); err != nil {
			return gitprojection.AppConfigPolicyValidation{}, err
		}
	}

	// A direct Git deletion is an authoritative absence only when the entire
	// exact generation reaches this fenced activation transaction.
	for _, previous := range input.Previous {
		if previous.ApplicationID == "" {
			continue
		}
		if _, present := currentPaths[previous.Path]; present {
			continue
		}
		scope, err := resolveDocumentScopeTx(ctx, tx, input.Binding, previous.ApplicationID, previous.Path, input.Generation.HeadRevision, previous.ConfigRevision, previous.ContentSHA256)
		if err != nil {
			return gitprojection.AppConfigPolicyValidation{}, err
		}
		if v.Secrets != nil {
			if err = v.Secrets.ReconcileDeletedTx(ctx, tx, scope, now); err != nil {
				return gitprojection.AppConfigPolicyValidation{}, err
			}
		}
		if v.Registry != nil {
			if err = v.Registry.ReconcileDeletedTx(ctx, tx, scope, now); err != nil {
				return gitprojection.AppConfigPolicyValidation{}, err
			}
		}
		if v.Edge != nil {
			if err = v.Edge.ReconcileDeletedTx(ctx, tx, scope, now); err != nil {
				return gitprojection.AppConfigPolicyValidation{}, err
			}
		}
		if v.Middleware != nil {
			if err = v.Middleware.ReconcileDeletedTx(ctx, tx, scope.Path); err != nil {
				return gitprojection.AppConfigPolicyValidation{}, err
			}
		}
	}
	return validation, nil
}

func resolveDocumentScopeTx(ctx context.Context, tx pgx.Tx, binding gitprojection.Binding, applicationID, documentPath, sourceRevision, configRevision, contentSHA256 string) (DocumentScope, error) {
	var organizationID *string
	var namespace string
	err := tx.QueryRow(ctx, `SELECT p.team_id::text,e.namespace
		FROM git_repository_bindings b
		JOIN projects p ON p.id=b.project_id
		JOIN environments e ON e.id=b.environment_id AND e.project_id=p.id
		JOIN applications a ON a.id=$2 AND a.project_id=p.id
		WHERE b.id=$1 AND b.kind='environment' AND b.project_id=$3 AND b.environment_id=$4
		AND b.target_ref=$5 AND b.path_prefix=$6 AND b.target_head_revision=$7
		FOR SHARE OF b,p,e,a`, binding.ID, applicationID, binding.ProjectID, binding.EnvironmentID,
		binding.TargetRef, binding.Prefix, sourceRevision).Scan(&organizationID, &namespace)
	if err != nil {
		return DocumentScope{}, err
	}
	scope := DocumentScope{Binding: binding, Namespace: namespace, ApplicationID: applicationID, Path: documentPath,
		SourceRevision: sourceRevision, ConfigRevision: configRevision, ContentSHA256: contentSHA256}
	if organizationID != nil {
		scope.OrganizationID = *organizationID
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingEnvironment || !namespaceRE.MatchString(scope.Namespace) ||
		scope.ApplicationID == "" || scope.Path == "" || scope.SourceRevision == "" || scope.ConfigRevision == "" {
		return DocumentScope{}, gitprojection.ErrInvalid
	}
	return scope, nil
}

func (v *Validator) externalDNSDiagnosticsTx(ctx context.Context, tx pgx.Tx, scope DocumentScope, document AppConfigPolicyDocument, now time.Time) ([]gitprojection.Diagnostic, error) {
	routes := document.Routes()
	diagnostics := []gitprojection.Diagnostic{}
	for index, route := range routes {
		if route.DNS.Mode != "externalDns" {
			continue
		}
		pointer := "/spec/routes/" + strconv.Itoa(index)
		integrationRef := route.DNS.IntegrationRef
		hostname := route.Host
		var integrationID string
		var suffixesJSON []byte
		err := tx.QueryRow(ctx, `SELECT i.id::text,i.allowed_domain_suffixes
			FROM external_dns_integrations i
			JOIN external_dns_integration_environments x ON x.integration_id=i.id
			WHERE i.slug=$1 AND x.environment_id=$2
			FOR SHARE OF i,x`, integrationRef, scope.Binding.EnvironmentID).Scan(&integrationID, &suffixesJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "ExternalDNSIntegrationUnavailable", Detail: "The External DNS integration is not assigned to this exact environment.", Pointer: pointer + "/dns/integrationRef"})
			continue
		}
		if err != nil {
			return nil, err
		}
		var suffixes []string
		if err = json.Unmarshal(suffixesJSON, &suffixes); err != nil {
			return nil, err
		}
		if !externaldns.HostnameAllowed(hostname, suffixes) {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "ExternalDNSHostnameNotAllowed", Detail: "The route hostname is outside the integration's allowed domain suffixes.", Pointer: pointer + "/host"})
		}
		ready := false
		if v.ExternalDNSRuntime != nil {
			ready, err = v.ExternalDNSRuntime.ReadyExternalDNSIntegrationTx(ctx, tx, integrationID, scope.Binding.EnvironmentID, now)
			if err != nil {
				return nil, err
			}
		}
		if !ready {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "ExternalDNSRuntimeUnobserved", Detail: "No fresh exact External DNS controller readiness observation is available for this integration.", Pointer: pointer + "/dns/integrationRef"})
		}
	}
	return diagnostics, nil
}

func runtimeUsesSecrets(runtime domain.WorkloadRuntime) bool {
	for _, variable := range runtime.Env {
		if variable.ValueFrom != nil {
			return true
		}
	}
	return false
}

var _ gitprojection.PostgreSQLAppConfigPolicyValidator = (*Validator)(nil)
