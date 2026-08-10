package projectionpolicy

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

// CertificateReferenceResolver proves one typed certificate reference against
// the exact tenant/application/namespace, active immutable version, fresh
// provider observation, public X.509 attestation, and route hostname. It never
// accepts or returns certificate/key material.
type CertificateReferenceResolver interface {
	ResolveCertificateReferenceTx(context.Context, pgx.Tx, secrets.Scope, certificates.Reference, string, time.Time) (certificates.ResolvedReference, error)
}

type SSLIPHostnameResolver interface {
	ResolveHostnameTx(context.Context, pgx.Tx, edge.SSLIPHostnameRequest, time.Time) (edge.SSLIPHostnameResolution, error)
}

// CertificateIssuerReferencePolicy is the dynamic, admin-managed issuer
// authority. It resolves and guards exact active/fresh profile revisions in
// the same generation-activation transaction. Bootstrap chart issuers remain
// the separate immutable edge-profile authority.
type CertificateIssuerReferencePolicy interface {
	ReconcileReferencesTx(context.Context, pgx.Tx, string, string, string, []certissuers.Selection, time.Time, time.Duration) (bool, error)
	ReconcileDeletedTx(context.Context, pgx.Tx, string) error
}

// EdgeRouteReferencePolicy binds route intent to the exact operator-approved
// and freshly observed Traefik/cert-manager/external-dns runtime. It never
// treats configured metadata as runtime readiness.
type EdgeRouteReferencePolicy struct {
	Config              edge.RuntimeConfig
	Certificates        CertificateReferenceResolver
	ManagedIssuers      CertificateIssuerReferencePolicy
	ManagedIssuerMaxAge time.Duration
	SSLIP               SSLIPHostnameResolver
}

func (p *EdgeRouteReferencePolicy) ValidateCurrentTx(
	ctx context.Context,
	tx pgx.Tx,
	document AppConfigPolicyDocument,
	now time.Time,
) ([]gitprojection.Diagnostic, error) {
	if p == nil || tx == nil || now.IsZero() || document.validate() != nil || p.Config.Validate() != nil {
		return nil, gitprojection.ErrInvalid
	}
	routes := document.Routes()
	if len(routes) == 0 {
		if p.ManagedIssuers != nil {
			scope := document.Scope()
			ok, err := p.ManagedIssuers.ReconcileReferencesTx(ctx, tx, scope.ApplicationID, scope.Binding.EnvironmentID,
				scope.Path, nil, now.UTC(), p.ManagedIssuerMaxAge)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, gitprojection.ErrConflict
			}
		}
		return nil, nil
	}
	if !p.Config.Enabled || p.Config.Profiles.Traefik == nil {
		return routeDiagnosticForAll(routes, "EdgeTraefikUnavailable", "No operator-approved Traefik route profile is enabled.", ""), nil
	}
	digest, err := p.Config.Digest()
	if err != nil {
		return nil, gitprojection.ErrInvalid
	}
	traefikProfile := edge.TargetProfile{Kind: edge.KindTraefik, Traefik: p.Config.Profiles.Traefik}
	traefikReady, err := p.targetReadyTx(ctx, tx, digest, traefikProfile, now)
	if err != nil {
		return nil, err
	}
	diagnostics := []gitprojection.Diagnostic{}
	managedIssuerSelections := []certissuers.Selection{}
	managedIssuerPointers := []string{}
	runtime := document.Runtime()
	scope := document.Scope()
	middlewareNames := document.MiddlewareNames()
	middlewareCounts := map[string]int{}
	for _, name := range middlewareNames {
		middlewareCounts[name]++
	}
	ids := map[string]int{}
	for index, route := range routes {
		pointer := "/spec/routes/" + strconv.Itoa(index)
		if !traefikReady {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "TraefikRuntimeUnobserved", Detail: "No fresh exact Traefik runtime observation is available for this route.", Pointer: pointer})
		}
		effectiveIngressClass := route.IngressClassName
		if effectiveIngressClass == "" {
			effectiveIngressClass = "traefik"
		}
		if effectiveIngressClass != p.Config.Profiles.Traefik.IngressClass.Name {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "IngressClassNotApproved", Detail: "The route ingress class does not match the exact operator-approved Traefik profile.", Pointer: pointer + "/ingressClassName"})
		}
		if route.ID != "" {
			if first, duplicate := ids[route.ID]; duplicate {
				diagnostics = append(diagnostics,
					gitprojection.Diagnostic{Code: "DuplicateRouteID", Detail: "Route IDs must be unique inside one AppConfig.", Pointer: "/spec/routes/" + strconv.Itoa(first) + "/id"},
					gitprojection.Diagnostic{Code: "DuplicateRouteID", Detail: "Route IDs must be unique inside one AppConfig.", Pointer: pointer + "/id"},
				)
			} else {
				ids[route.ID] = index
			}
		}
		portReady := false
		for _, port := range runtime.Ports {
			if port.Name == route.Port && port.Protocol == "TCP" {
				portReady = true
				break
			}
		}
		if !portReady {
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "RoutePortUnavailable", Detail: "The route must reference a declared TCP workload port.", Pointer: pointer + "/port"})
		}
		for refIndex, reference := range route.MiddlewareRefs {
			if middlewareCounts[reference] != 1 {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "MiddlewareReferenceUnavailable", Detail: "Each route middleware reference must resolve to exactly one definition in this AppConfig.", Pointer: pointer + "/middlewareRefs/" + strconv.Itoa(refIndex)})
			}
		}
		if route.DNS.Mode == "sslip" {
			if p.Config.Profiles.Traefik.SSLIP == nil || p.SSLIP == nil {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "SSLIPHostnameUnavailable", Detail: "No fresh operator-approved public Traefik ingress address is available for sslip.io.", Pointer: pointer + "/dns"})
			} else {
				resolved, resolveErr := p.SSLIP.ResolveHostnameTx(ctx, tx, edge.SSLIPHostnameRequest{
					ApplicationID: scope.ApplicationID,
					EnvironmentID: scope.Binding.EnvironmentID,
					ProjectID:     scope.Binding.ProjectID,
					Namespace:     scope.Namespace,
				}, now)
				switch {
				case resolveErr == nil && resolved.Validate() != nil:
					return nil, gitprojection.ErrConflict
				case resolveErr == nil && route.Host != resolved.Hostname:
					diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "SSLIPHostnameMismatch", Detail: "The sslip.io hostname must equal the exact server-derived hostname for this application, environment, and observed public ingress address.", Pointer: pointer + "/host"})
				case resolveErr == nil:
					// Exact fresh endpoint and derived host are proven.
				case errors.Is(resolveErr, edge.ErrNotFound), errors.Is(resolveErr, edge.ErrUnavailable):
					diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "SSLIPHostnameUnavailable", Detail: "No fresh operator-approved public Traefik ingress address is available for sslip.io.", Pointer: pointer + "/dns"})
				default:
					return nil, resolveErr
				}
			}
		}
		switch route.TLS.Mode {
		case "httpOnly":
			// Traefik readiness is the only serving dependency.
		case "letsencrypt":
			if p.Config.Profiles.CertManager == nil {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CertManagerProfileUnavailable", Detail: "Let's Encrypt routes require an operator-approved cert-manager profile.", Pointer: pointer + "/tls"})
				continue
			}
			certProfile := p.Config.Profiles.CertManager
			if certProfile.IngressClassName != p.Config.Profiles.Traefik.IngressClass.Name || effectiveIngressClass != certProfile.IngressClassName {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CertificateIngressClassMismatch", Detail: "The route, Traefik profile, and cert-manager solver ingress class must match exactly.", Pointer: pointer + "/ingressClassName"})
			}
			if !slices.Contains(certProfile.ApprovedIssuers(), route.TLS.IssuerRef) {
				if p.ManagedIssuers == nil {
					diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CertificateIssuerNotApproved", Detail: "The selected Let's Encrypt issuer is not present in the exact operator-approved issuer catalog.", Pointer: pointer + "/tls/issuerRef"})
				} else {
					managedIssuerSelections = append(managedIssuerSelections, certissuers.Selection{Hostname: route.Host, IssuerName: route.TLS.IssuerRef})
					managedIssuerPointers = append(managedIssuerPointers, pointer+"/tls/issuerRef")
				}
			}
			certReady, readyErr := p.targetReadyTx(ctx, tx, digest, edge.TargetProfile{Kind: edge.KindCertManager, CertManager: certProfile}, now)
			if readyErr != nil {
				return nil, readyErr
			}
			if !certReady {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CertManagerRuntimeUnobserved", Detail: "No fresh exact cert-manager and approved issuer observation is available for this route.", Pointer: pointer + "/tls"})
			}
		case "customCertificate":
			if route.TLS.SecretRef == nil || p.Certificates == nil || scope.OrganizationID == "" {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CustomCertificateBindingUnavailable", Detail: "Custom certificates require an exact ready certificate binding and fresh provider observation.", Pointer: pointer + "/tls/secretRef"})
				continue
			}
			resolved, resolveErr := p.Certificates.ResolveCertificateReferenceTx(ctx, tx, secrets.Scope{
				OrganizationID: scope.OrganizationID,
				ProjectID:      scope.Binding.ProjectID,
				EnvironmentID:  scope.Binding.EnvironmentID,
				ApplicationID:  scope.ApplicationID,
				Namespace:      scope.Namespace,
			}, *route.TLS.SecretRef, route.Host, now)
			switch {
			case resolveErr == nil && resolved.Validate() == nil:
				// Exact scope, active version, observation and SAN are proven.
			case resolveErr == nil:
				return nil, gitprojection.ErrConflict
			case errors.Is(resolveErr, certificates.ErrNotFound):
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CustomCertificateBindingNotFound", Detail: "The certificate binding does not exist in this application and environment.", Pointer: pointer + "/tls/secretRef"})
			case errors.Is(resolveErr, certificates.ErrNotReady):
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CustomCertificateNotReady", Detail: "The exact certificate version is not active with a fresh provider observation.", Pointer: pointer + "/tls/secretRef/version"})
			case errors.Is(resolveErr, certificates.ErrHostMismatch):
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CustomCertificateHostMismatch", Detail: "The certificate does not cover the exact route hostname.", Pointer: pointer + "/host"})
			default:
				return nil, resolveErr
			}
		}
	}
	if p.ManagedIssuers != nil {
		ok, reconcileErr := p.ManagedIssuers.ReconcileReferencesTx(ctx, tx, scope.ApplicationID, scope.Binding.EnvironmentID,
			scope.Path, managedIssuerSelections, now.UTC(), p.ManagedIssuerMaxAge)
		if reconcileErr != nil {
			return nil, reconcileErr
		}
		if !ok {
			for _, pointer := range managedIssuerPointers {
				diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "CertificateIssuerNotReady", Detail: "The exact managed issuer revision is unavailable, inactive, stale, or does not authorize this hostname.", Pointer: pointer})
			}
		}
	}
	emittedMiddleware := map[string]struct{}{}
	for _, name := range middlewareNames {
		if middlewareCounts[name] > 1 {
			if _, emitted := emittedMiddleware[name]; emitted {
				continue
			}
			emittedMiddleware[name] = struct{}{}
			diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: "DuplicateMiddlewareName", Detail: "Middleware definition names must be unique inside one AppConfig.", Pointer: middlewareNamePointer(middlewareNames, name)})
		}
	}
	return diagnostics, nil
}

func (p *EdgeRouteReferencePolicy) ReconcileDeletedTx(ctx context.Context, tx pgx.Tx, scope DocumentScope, now time.Time) error {
	if tx == nil || now.IsZero() || !validDocumentScope(scope) {
		return gitprojection.ErrInvalid
	}
	if p.ManagedIssuers != nil {
		return p.ManagedIssuers.ReconcileDeletedTx(ctx, tx, scope.Path)
	}
	return nil
}

// ReadyExternalDNSIntegrationTx implements the independent runtime readiness
// seam used after durable integration assignment and hostname policy succeed.
func (p *EdgeRouteReferencePolicy) ReadyExternalDNSIntegrationTx(ctx context.Context, tx pgx.Tx, integrationID, _ string, now time.Time) (bool, error) {
	if p == nil || tx == nil || integrationID == "" || now.IsZero() || p.Config.Validate() != nil || !p.Config.Enabled {
		return false, nil
	}
	digest, err := p.Config.Digest()
	if err != nil {
		return false, nil
	}
	for index := range p.Config.Profiles.ExternalDNS {
		profile := &p.Config.Profiles.ExternalDNS[index]
		if profile.IntegrationID == integrationID {
			return p.targetReadyTx(ctx, tx, digest, edge.TargetProfile{Kind: edge.KindExternalDNS, ExternalDNS: profile}, now)
		}
	}
	return false, nil
}

func (p *EdgeRouteReferencePolicy) targetReadyTx(ctx context.Context, tx pgx.Tx, configDigest string, profile edge.TargetProfile, now time.Time) (bool, error) {
	desired, err := profile.Desired(configDigest)
	if err != nil {
		return false, gitprojection.ErrInvalid
	}
	var kind, integrationID, mode, namespace, profileConfigMap, txtOwner, externalPolicy, externalDomains string
	var desiredDigest, runtimeDigest, state string
	var active bool
	var lastObserved *time.Time
	err = tx.QueryRow(ctx, `SELECT kind,COALESCE(integration_id::text,''),management_mode,namespace,profile_config_map,
		external_txt_owner_id,external_policy,external_domains,desired_digest,runtime_config_digest,
		active,runtime_state,last_observed_at
		FROM edge_runtime_targets WHERE target_key=$1 AND profile_revision=$2 FOR SHARE`, desired.Key, desired.Revision).
		Scan(&kind, &integrationID, &mode, &namespace, &profileConfigMap, &txtOwner, &externalPolicy, &externalDomains,
			&desiredDigest, &runtimeDigest, &active, &state, &lastObserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind != string(desired.Kind) || integrationID != desired.IntegrationID || mode != string(desired.Mode) || namespace != desired.Namespace ||
		profileConfigMap != desired.ProfileConfigMap || txtOwner != desired.ExternalTXTOwnerID || externalPolicy != desired.ExternalPolicy ||
		externalDomains != desired.ExternalDomains || desiredDigest != desired.DesiredDigest || runtimeDigest != configDigest || !active ||
		state != string(edge.StateReady) || lastObserved == nil || lastObserved.After(now.UTC()) || lastObserved.Before(now.UTC().Add(-p.Config.ReadinessMaxAge)) {
		return false, nil
	}
	var workerReady bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM edge_runtime_readiness
		WHERE contract_version=$1 AND config_digest=$2 AND target_count=$3
		  AND observed_at<=$4 AND observed_at>=$5 AND lease_until>$4
	)`, edge.RuntimeContract, configDigest, p.Config.TargetCount(), now.UTC(), now.UTC().Add(-p.Config.ReadinessMaxAge)).Scan(&workerReady)
	return workerReady, err
}

func routeDiagnosticForAll(routes []AppConfigRouteDocument, code, detail, suffix string) []gitprojection.Diagnostic {
	diagnostics := make([]gitprojection.Diagnostic, 0, len(routes))
	for index := range routes {
		diagnostics = append(diagnostics, gitprojection.Diagnostic{Code: code, Detail: detail, Pointer: "/spec/routes/" + strconv.Itoa(index) + suffix})
	}
	return diagnostics
}

func middlewareNamePointer(names []string, target string) string {
	for index, name := range names {
		if name == target {
			return "/spec/middlewares/" + strconv.Itoa(index) + "/name"
		}
	}
	return "/spec/middlewares"
}

var _ ReferencePolicy = (*EdgeRouteReferencePolicy)(nil)
var _ ExternalDNSRuntimePolicy = (*EdgeRouteReferencePolicy)(nil)
