package projectionpolicy

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
)

const maximumPolicyRoutes = 32

var policyHostnameRE = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// AppConfigDNSDocument is the closed route DNS view. An omitted DNS object is
// normalized to manual, matching the locked runtime chart default.
type AppConfigDNSDocument struct {
	Mode           string `json:"mode"`
	IntegrationRef string `json:"integrationRef,omitempty"`
	TTL            int64  `json:"ttl,omitempty"`
	HasTTL         bool   `json:"hasTtl,omitempty"`
}

// AppConfigTLSDocument contains only safe route intent. Custom certificates
// carry an immutable certificate binding identity; Kubernetes Secret names
// and provider identities are never accepted from AppConfig.
type AppConfigTLSDocument struct {
	Mode            string                  `json:"mode"`
	IssuerRef       string                  `json:"issuerRef,omitempty"`
	SecretRef       *certificates.Reference `json:"secretRef,omitempty"`
	RedirectHTTP    bool                    `json:"redirectHttp,omitempty"`
	HasRedirectHTTP bool                    `json:"hasRedirectHttp,omitempty"`
}

// AppConfigRouteDocument is the immutable typed route view shared by dynamic
// policies. Slice fields are detached by AppConfigPolicyDocument accessors.
type AppConfigRouteDocument struct {
	ID               string               `json:"id,omitempty"`
	Host             string               `json:"host"`
	Path             string               `json:"path"`
	Port             string               `json:"port"`
	IngressClassName string               `json:"ingressClassName,omitempty"`
	MiddlewareRefs   []string             `json:"middlewareRefs"`
	DNS              AppConfigDNSDocument `json:"dns"`
	TLS              AppConfigTLSDocument `json:"tls"`
}

func decodeRoutePolicyDocument(spec map[string]any) ([]AppConfigRouteDocument, []string, []domain.SecretBindingRef, []middlewareprofiles.MaterializedDefinition, error) {
	routes := []AppConfigRouteDocument{}
	if raw, present := spec["routes"]; present {
		values, ok := raw.([]any)
		if !ok || len(values) > maximumPolicyRoutes {
			return nil, nil, nil, nil, gitprojection.ErrInvalid
		}
		for _, value := range values {
			route, ok := value.(map[string]any)
			if !ok || !onlyKeys(route, "id", "host", "path", "port", "ingressClassName", "middlewareRefs", "dns", "tls") {
				return nil, nil, nil, nil, gitprojection.ErrInvalid
			}
			decoded, err := decodeRoute(route)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			routes = append(routes, decoded)
		}
	}
	middlewareNames := []string{}
	secretReferences := []domain.SecretBindingRef{}
	definitions := []middlewareprofiles.MaterializedDefinition{}
	if raw, present := spec["middlewares"]; present {
		values, ok := raw.([]any)
		if !ok || len(values) > maximumPolicyRoutes {
			return nil, nil, nil, nil, gitprojection.ErrInvalid
		}
		for _, value := range values {
			middleware, ok := value.(map[string]any)
			if !ok || !onlyKeys(middleware, "id", "name", "spec", "profileRef") {
				return nil, nil, nil, nil, gitprojection.ErrInvalid
			}
			name, ok := middleware["name"].(string)
			spec, specOK := middleware["spec"].(map[string]any)
			if !ok || !specOK || !policyLabelRE.MatchString(name) || middlewareprofiles.ValidateDefinition(name, middlewareprofiles.Spec(spec)) != nil {
				return nil, nil, nil, nil, gitprojection.ErrInvalid
			}
			middlewareNames = append(middlewareNames, name)
			secretReferences = append(secretReferences, middlewareprofiles.SecretReferences(middlewareprofiles.Spec(spec))...)
			definition := middlewareprofiles.MaterializedDefinition{Name: name, Spec: middlewareprofiles.Spec(spec)}
			if rawRef, present := middleware["profileRef"]; present {
				encoded, _ := json.Marshal(rawRef)
				var ref middlewareprofiles.Ref
				if json.Unmarshal(encoded, &ref) != nil {
					return nil, nil, nil, nil, gitprojection.ErrInvalid
				}
				definition.ProfileRef = &ref
			}
			definitions = append(definitions, definition)
		}
	}
	if validateRoutePolicyDocument(routes, middlewareNames, domain.WorkloadRuntime{}) != nil {
		return nil, nil, nil, nil, gitprojection.ErrInvalid
	}
	return routes, middlewareNames, secretReferences, definitions, nil
}

// AppConfigCertificateReferences returns every typed custom-certificate use
// from one already parsed AppConfig. The same closed route decoder used by the
// projection worker prevents API admission from accepting a wider shape.
func AppConfigCertificateReferences(parsed map[string]any) ([]certificates.ReferenceSelection, error) {
	selections, err := appconfig.CertificateReferences(parsed)
	if err != nil {
		return nil, gitprojection.ErrInvalid
	}
	result := make([]certificates.ReferenceSelection, 0, len(selections))
	for _, selection := range selections {
		reference := certificates.Reference{BindingID: selection.BindingID, Name: selection.Name, Version: selection.Version}
		if reference.Validate() != nil {
			return nil, gitprojection.ErrInvalid
		}
		result = append(result, certificates.ReferenceSelection{Host: selection.Host, Reference: reference})
	}
	return result, nil
}

func decodeRoute(value map[string]any) (AppConfigRouteDocument, error) {
	host, hostOK := value["host"].(string)
	path, pathOK := value["path"].(string)
	port, portOK := value["port"].(string)
	tlsValue, tlsOK := value["tls"].(map[string]any)
	if !hostOK || !pathOK || !portOK || !tlsOK {
		return AppConfigRouteDocument{}, gitprojection.ErrInvalid
	}
	route := AppConfigRouteDocument{Host: host, Path: path, Port: port, MiddlewareRefs: []string{}, DNS: AppConfigDNSDocument{Mode: "manual"}}
	if raw, present := value["id"]; present {
		var ok bool
		route.ID, ok = raw.(string)
		if !ok {
			return AppConfigRouteDocument{}, gitprojection.ErrInvalid
		}
	}
	if raw, present := value["ingressClassName"]; present {
		var ok bool
		route.IngressClassName, ok = raw.(string)
		if !ok {
			return AppConfigRouteDocument{}, gitprojection.ErrInvalid
		}
	}
	if raw, present := value["middlewareRefs"]; present {
		values, ok := raw.([]any)
		if !ok || len(values) > 16 {
			return AppConfigRouteDocument{}, gitprojection.ErrInvalid
		}
		for _, item := range values {
			name, ok := item.(string)
			if !ok {
				return AppConfigRouteDocument{}, gitprojection.ErrInvalid
			}
			route.MiddlewareRefs = append(route.MiddlewareRefs, name)
		}
	}
	if raw, present := value["dns"]; present {
		dns, ok := raw.(map[string]any)
		if !ok {
			return AppConfigRouteDocument{}, gitprojection.ErrInvalid
		}
		decoded, err := decodeDNS(dns)
		if err != nil {
			return AppConfigRouteDocument{}, err
		}
		route.DNS = decoded
	}
	tls, err := decodeTLS(tlsValue)
	if err != nil {
		return AppConfigRouteDocument{}, err
	}
	route.TLS = tls
	return route, nil
}

func decodeDNS(value map[string]any) (AppConfigDNSDocument, error) {
	mode, ok := value["mode"].(string)
	if !ok {
		return AppConfigDNSDocument{}, gitprojection.ErrInvalid
	}
	switch mode {
	case "manual":
		if !onlyKeys(value, "mode") {
			return AppConfigDNSDocument{}, gitprojection.ErrInvalid
		}
		return AppConfigDNSDocument{Mode: mode}, nil
	case "sslip":
		if !onlyKeys(value, "mode") {
			return AppConfigDNSDocument{}, gitprojection.ErrInvalid
		}
		return AppConfigDNSDocument{Mode: mode}, nil
	case "externalDns":
		if !onlyKeys(value, "mode", "integrationRef", "ttl") {
			return AppConfigDNSDocument{}, gitprojection.ErrInvalid
		}
		integration, ok := value["integrationRef"].(string)
		if !ok {
			return AppConfigDNSDocument{}, gitprojection.ErrInvalid
		}
		dns := AppConfigDNSDocument{Mode: mode, IntegrationRef: integration}
		if raw, present := value["ttl"]; present {
			number, ok := raw.(json.Number)
			if !ok {
				return AppConfigDNSDocument{}, gitprojection.ErrInvalid
			}
			ttl, err := strconv.ParseInt(number.String(), 10, 64)
			if err != nil || strconv.FormatInt(ttl, 10) != number.String() {
				return AppConfigDNSDocument{}, gitprojection.ErrInvalid
			}
			dns.TTL, dns.HasTTL = ttl, true
		}
		return dns, nil
	default:
		return AppConfigDNSDocument{}, gitprojection.ErrInvalid
	}
}

func decodeTLS(value map[string]any) (AppConfigTLSDocument, error) {
	mode, ok := value["mode"].(string)
	if !ok {
		return AppConfigTLSDocument{}, gitprojection.ErrInvalid
	}
	tls := AppConfigTLSDocument{Mode: mode}
	switch mode {
	case "httpOnly":
		if !onlyKeys(value, "mode") {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
	case "letsencrypt":
		if !onlyKeys(value, "mode", "issuerRef", "redirectHttp") {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		var issuerOK bool
		tls.IssuerRef, issuerOK = value["issuerRef"].(string)
		if !issuerOK {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
	case "customCertificate":
		if !onlyKeys(value, "mode", "secretRef", "redirectHttp") {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		secretValue, secretOK := value["secretRef"].(map[string]any)
		if !secretOK || !onlyKeys(secretValue, "bindingId", "name", "version") {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		bindingID, bindingOK := secretValue["bindingId"].(string)
		name, nameOK := secretValue["name"].(string)
		number, numberOK := secretValue["version"].(json.Number)
		if !bindingOK || !nameOK || !numberOK {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		version, parseErr := strconv.ParseInt(number.String(), 10, 64)
		if parseErr != nil || version <= 0 || strconv.FormatInt(version, 10) != number.String() {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		reference := certificates.Reference{BindingID: bindingID, Name: name, Version: version}
		if reference.Validate() != nil {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		tls.SecretRef = &reference
	default:
		return AppConfigTLSDocument{}, gitprojection.ErrInvalid
	}
	if raw, present := value["redirectHttp"]; present {
		var redirectOK bool
		tls.RedirectHTTP, redirectOK = raw.(bool)
		if !redirectOK {
			return AppConfigTLSDocument{}, gitprojection.ErrInvalid
		}
		tls.HasRedirectHTTP = true
	}
	return tls, nil
}

func validateRoutePolicyDocument(routes []AppConfigRouteDocument, middlewareNames []string, _ domain.WorkloadRuntime) error {
	if len(routes) > maximumPolicyRoutes || len(middlewareNames) > maximumPolicyRoutes {
		return gitprojection.ErrInvalid
	}
	names := append([]string(nil), middlewareNames...)
	slices.Sort(names)
	for _, name := range names {
		if !policyLabelRE.MatchString(name) {
			return gitprojection.ErrInvalid
		}
	}
	for _, route := range routes {
		if !policyHostnameRE.MatchString(route.Host) || len(route.Host) > 253 || route.Path == "" || len(route.Path) > 2048 || !strings.HasPrefix(route.Path, "/") ||
			!policyLabelRE.MatchString(route.Port) || route.IngressClassName != "" && !policyLabelRE.MatchString(route.IngressClassName) || len(route.MiddlewareRefs) > 16 {
			return gitprojection.ErrInvalid
		}
		if route.ID != "" {
			if !policyLabelRE.MatchString(route.ID) {
				return gitprojection.ErrInvalid
			}
		}
		seenRefs := map[string]struct{}{}
		for _, reference := range route.MiddlewareRefs {
			if !policyLabelRE.MatchString(reference) {
				return gitprojection.ErrInvalid
			}
			if _, duplicate := seenRefs[reference]; duplicate {
				return gitprojection.ErrInvalid
			}
			seenRefs[reference] = struct{}{}
		}
		if route.DNS.Mode != "manual" && route.DNS.Mode != "sslip" && route.DNS.Mode != "externalDns" ||
			(route.DNS.Mode == "manual" || route.DNS.Mode == "sslip") && (route.DNS.IntegrationRef != "" || route.DNS.HasTTL || route.DNS.TTL != 0) ||
			route.DNS.Mode == "externalDns" && (!policyLabelRE.MatchString(route.DNS.IntegrationRef) || route.DNS.HasTTL && (route.DNS.TTL < 30 || route.DNS.TTL > 86400)) {
			return gitprojection.ErrInvalid
		}
		switch route.TLS.Mode {
		case "httpOnly":
			if route.TLS.IssuerRef != "" || route.TLS.SecretRef != nil || route.TLS.HasRedirectHTTP {
				return gitprojection.ErrInvalid
			}
		case "letsencrypt":
			if !policyLabelRE.MatchString(route.TLS.IssuerRef) || route.TLS.SecretRef != nil {
				return gitprojection.ErrInvalid
			}
		case "customCertificate":
			if route.TLS.SecretRef == nil || route.TLS.SecretRef.Validate() != nil || route.TLS.IssuerRef != "" {
				return gitprojection.ErrInvalid
			}
		default:
			return gitprojection.ErrInvalid
		}
	}
	return nil
}
