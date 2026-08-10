// Package middlewareprofiles owns the closed, reusable Traefik HTTP
// middleware policy. It deliberately accepts JSON-shaped values so the same
// validator can be used at HTTP admission and at the direct-Git projection
// boundary.
package middlewareprofiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var (
	ErrInvalid        = errors.New("invalid middleware profile value")
	ErrNotFound       = errors.New("middleware profile not found")
	ErrConflict       = errors.New("middleware profile conflict")
	ErrInactive       = errors.New("middleware profile is inactive")
	ErrUnassigned     = errors.New("middleware profile is not assigned to target")
	ErrReferenced     = errors.New("middleware profile is referenced")
	dnsLabelRE        = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	uuidRE            = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestRE          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	headerNameRE      = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	headerSensitiveRE = regexp.MustCompile(`(?i)(?:api[-_]?key|password|secret|token)`)
	durationRE        = regexp.MustCompile(`^(?:[0-9]+(?:ns|us|µs|ms|s|m|h))+$`)
	methodRE          = regexp.MustCompile(`^(?:\*|[A-Z][A-Z0-9!#$%&'*+.^_` + "`" + `|~-]{0,31})$`)
	mediaTypeRE       = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+*-]+$`)
	encodingRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	hostnameRE        = regexp.MustCompile(`^(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
)

var forbiddenHeaders = map[string]struct{}{
	"authorization": {}, "connection": {}, "content-length": {}, "cookie": {}, "host": {},
	"proxy-authorization": {}, "proxy-connection": {}, "set-cookie": {}, "te": {}, "trailer": {},
	"transfer-encoding": {}, "upgrade": {}, "x-forwarded-for": {}, "x-forwarded-host": {},
	"x-forwarded-port": {}, "x-forwarded-proto": {},
}

var familyKeys = map[string][]string{
	"redirectScheme":   {"scheme", "port", "permanent"},
	"redirectRegex":    {"regex", "replacement", "permanent"},
	"addPrefix":        {"prefix"},
	"stripPrefix":      {"prefixes", "forceSlash"},
	"stripPrefixRegex": {"regex"},
	"replacePath":      {"path"},
	"replacePathRegex": {"regex", "replacement"},
	"headers":          {"customRequestHeaders", "customResponseHeaders", "accessControlAllowCredentials", "accessControlAllowHeaders", "accessControlAllowMethods", "accessControlAllowOriginList", "accessControlAllowOriginListRegex", "accessControlExposeHeaders", "accessControlMaxAge", "addVaryHeader", "allowedHosts", "stsSeconds", "stsIncludeSubdomains", "stsPreload", "forceSTSHeader", "frameDeny", "customFrameOptionsValue", "contentTypeNosniff", "browserXssFilter", "customBrowserXSSValue", "contentSecurityPolicy", "contentSecurityPolicyReportOnly", "referrerPolicy", "permissionsPolicy", "isDevelopment"},
	"rateLimit":        {"average", "period", "burst"},
	"inFlightReq":      {"amount"},
	"ipAllowList":      {"sourceRange", "ipStrategy"},
	"compress":         {"excludedContentTypes", "includedContentTypes", "minResponseBodyBytes", "defaultEncoding", "encodings"},
	"buffering":        {"maxRequestBodyBytes", "memRequestBodyBytes", "maxResponseBodyBytes", "memResponseBodyBytes", "retryExpression"},
	"retry":            {"attempts", "initialInterval"},
	"basicAuth":        {"secretBindingRef", "removeHeader", "headerField"},
}

// Spec is exactly one supported Traefik HTTP middleware family.
type Spec map[string]any

func DecodeSpec(raw []byte) (Spec, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil || ValidateSpec(spec) != nil {
		return nil, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalid
	}
	return cloneSpec(spec), nil
}

func ValidateDefinition(name string, spec Spec) error {
	if !dnsLabelRE.MatchString(name) {
		return ErrInvalid
	}
	return ValidateSpec(spec)
}

// ValidateDefinitions validates the complete local middleware graph. Profiles
// are materialized as ordinary definitions before this boundary, so route
// references never need cross-namespace or external Traefik references.
func ValidateDefinitions(spec map[string]any) error {
	rawDefinitions, present := spec["middlewares"]
	if !present {
		rawDefinitions = []any{}
	}
	definitions, ok := rawDefinitions.([]any)
	if !ok || len(definitions) > 32 {
		return ErrInvalid
	}
	names := make(map[string]struct{}, len(definitions))
	for _, raw := range definitions {
		definition, ok := raw.(map[string]any)
		if !ok || !onlyKeys(definition, "id", "name", "spec", "profileRef") {
			return ErrInvalid
		}
		name, ok := definition["name"].(string)
		middlewareSpec, specOK := definition["spec"].(map[string]any)
		if !ok || !specOK || ValidateDefinition(name, Spec(middlewareSpec)) != nil {
			return ErrInvalid
		}
		if _, duplicate := names[name]; duplicate {
			return ErrInvalid
		}
		names[name] = struct{}{}
		if rawRef, hasRef := definition["profileRef"]; hasRef && !validateProfileRef(rawRef) {
			return ErrInvalid
		}
	}
	rawRoutes, present := spec["routes"]
	if !present {
		return nil
	}
	routes, ok := rawRoutes.([]any)
	if !ok || len(routes) > 32 {
		return ErrInvalid
	}
	for _, raw := range routes {
		route, ok := raw.(map[string]any)
		if !ok {
			return ErrInvalid
		}
		rawRefs, present := route["middlewareRefs"]
		if !present {
			continue
		}
		refs, ok := rawRefs.([]any)
		if !ok || len(refs) > 16 {
			return ErrInvalid
		}
		seen := map[string]struct{}{}
		for _, rawRef := range refs {
			ref, ok := rawRef.(string)
			_, resolved := names[ref]
			if !ok || !resolved {
				return ErrInvalid
			}
			if _, duplicate := seen[ref]; duplicate {
				return ErrInvalid
			}
			seen[ref] = struct{}{}
		}
	}
	return nil
}

func validateProfileRef(raw any) bool {
	value, ok := raw.(map[string]any)
	if !ok || !onlyKeys(value, "profileId", "revision", "specDigest", "assignmentsDigest") {
		return false
	}
	profileID, profileOK := value["profileId"].(string)
	revision, revisionOK := intValue(value["revision"])
	specDigest, specOK := value["specDigest"].(string)
	assignmentsDigest, assignmentsOK := value["assignmentsDigest"].(string)
	return profileOK && uuidRE.MatchString(profileID) && revisionOK && revision > 0 && specOK && digestRE.MatchString(specDigest) && assignmentsOK && digestRE.MatchString(assignmentsDigest)
}

func ValidateSpec(spec Spec) error {
	if len(spec) != 1 {
		return ErrInvalid
	}
	for family, raw := range spec {
		allowed, ok := familyKeys[family]
		value, object := raw.(map[string]any)
		if !ok || !object || !onlyKeys(value, allowed...) {
			return ErrInvalid
		}
		return validateFamily(family, value)
	}
	return ErrInvalid
}

func validateFamily(family string, v map[string]any) error { //nolint:gocyclo
	switch family {
	case "redirectScheme":
		scheme, ok := reqString(v, "scheme", 32)
		if !ok || scheme != "http" && scheme != "https" || !optionalBool(v, "permanent") {
			return ErrInvalid
		}
		if port, present, ok := optString(v, "port", 5); !ok || present && !validPort(port) {
			return ErrInvalid
		}
	case "redirectRegex":
		regex, ok := reqString(v, "regex", 2048)
		_, replacementOK := reqString(v, "replacement", 2048)
		if !ok || !replacementOK || !validRE2(regex) || !optionalBool(v, "permanent") {
			return ErrInvalid
		}
	case "addPrefix":
		path, ok := reqString(v, "prefix", 2048)
		if !ok || !validPath(path) {
			return ErrInvalid
		}
	case "stripPrefix":
		items, ok := stringList(v["prefixes"], true)
		if !ok || !all(items, validPath) || !optionalBool(v, "forceSlash") {
			return ErrInvalid
		}
	case "stripPrefixRegex":
		items, ok := stringList(v["regex"], true)
		if !ok || !all(items, validRE2) {
			return ErrInvalid
		}
	case "replacePath":
		path, ok := reqString(v, "path", 2048)
		if !ok || !validPath(path) {
			return ErrInvalid
		}
	case "replacePathRegex":
		regex, ok := reqString(v, "regex", 2048)
		_, replacementOK := reqString(v, "replacement", 2048)
		if !ok || !replacementOK || !validRE2(regex) {
			return ErrInvalid
		}
	case "headers":
		if !validateHeaders(v) {
			return ErrInvalid
		}
	case "rateLimit":
		if !integer(v, "average", 0, 1_000_000, true) || !integer(v, "burst", 0, 1_000_000, false) || !optionalDuration(v, "period") {
			return ErrInvalid
		}
	case "inFlightReq":
		if !integer(v, "amount", 1, 1_000_000, true) {
			return ErrInvalid
		}
	case "ipAllowList":
		items, ok := stringList(v["sourceRange"], true)
		if !ok || !all(items, validCIDR) || !validateIPStrategy(v["ipStrategy"]) {
			return ErrInvalid
		}
	case "compress":
		if !optionalList(v, "excludedContentTypes", validMediaType) || !optionalList(v, "includedContentTypes", validMediaType) || !integer(v, "minResponseBodyBytes", 0, 1_073_741_824, false) || !optionalToken(v, "defaultEncoding", encodingRE) || !optionalList(v, "encodings", func(s string) bool { return encodingRE.MatchString(s) }) {
			return ErrInvalid
		}
	case "buffering":
		for _, key := range []string{"maxRequestBodyBytes", "memRequestBodyBytes", "maxResponseBodyBytes", "memResponseBodyBytes"} {
			if !integer(v, key, 0, 1_073_741_824, false) {
				return ErrInvalid
			}
		}
		if _, _, ok := optString(v, "retryExpression", 2048); !ok {
			return ErrInvalid
		}
	case "retry":
		if !integer(v, "attempts", 1, 100, true) || !optionalDuration(v, "initialInterval") {
			return ErrInvalid
		}
	case "basicAuth":
		ref, ok := v["secretBindingRef"].(map[string]any)
		if !ok || !onlyKeys(ref, "bindingId", "name", "key", "version") || !optionalBool(v, "removeHeader") || !optionalToken(v, "headerField", headerNameRE) {
			return ErrInvalid
		}
		bindingID, bindingOK := ref["bindingId"].(string)
		name, nameOK := ref["name"].(string)
		key, keyOK := ref["key"].(string)
		version, versionOK := intValue(ref["version"])
		if !bindingOK || !nameOK || !keyOK || !versionOK || !(domain.SecretBindingRef{BindingID: bindingID, Name: name, Key: key, Version: version}).Valid() || key != "users" {
			return ErrInvalid
		}
		if field, present, _ := optString(v, "headerField", 128); present {
			if _, forbidden := forbiddenHeaders[strings.ToLower(field)]; forbidden || headerSensitiveRE.MatchString(field) {
				return ErrInvalid
			}
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validateHeaders(v map[string]any) bool { //nolint:gocyclo
	if !headerMap(v, "customRequestHeaders") || !headerMap(v, "customResponseHeaders") {
		return false
	}
	for _, key := range []string{"accessControlAllowCredentials", "addVaryHeader", "stsIncludeSubdomains", "stsPreload", "forceSTSHeader", "frameDeny", "contentTypeNosniff", "browserXssFilter", "isDevelopment"} {
		if !optionalBool(v, key) {
			return false
		}
	}
	if !integer(v, "accessControlMaxAge", 0, 86400, false) || !integer(v, "stsSeconds", 0, 63072000, false) {
		return false
	}
	if !optionalList(v, "accessControlAllowHeaders", func(s string) bool { return s == "*" || headerNameRE.MatchString(s) }) || !optionalList(v, "accessControlAllowMethods", func(s string) bool { return methodRE.MatchString(s) }) || !optionalList(v, "accessControlAllowOriginList", validOrigin) || !optionalList(v, "accessControlAllowOriginListRegex", validRE2) || !optionalList(v, "accessControlExposeHeaders", func(s string) bool { return s == "*" || headerNameRE.MatchString(s) }) || !optionalList(v, "allowedHosts", validHostname) {
		return false
	}
	for _, key := range []string{"customFrameOptionsValue", "customBrowserXSSValue", "referrerPolicy"} {
		if _, _, ok := optString(v, key, 2048); !ok {
			return false
		}
	}
	for _, key := range []string{"contentSecurityPolicy", "contentSecurityPolicyReportOnly", "permissionsPolicy"} {
		if _, _, ok := optString(v, key, 8192); !ok {
			return false
		}
	}
	credentials, _ := v["accessControlAllowCredentials"].(bool)
	origins, _ := stringList(v["accessControlAllowOriginList"], false)
	if credentials {
		for _, origin := range origins {
			if origin == "*" {
				return false
			}
		}
	}
	frameDeny, _ := v["frameDeny"].(bool)
	frameOptions, _, _ := optString(v, "customFrameOptionsValue", 2048)
	return !frameDeny || frameOptions == ""
}

func onlyKeys(value map[string]any, allowed ...string) bool {
	set := map[string]struct{}{}
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range value {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}
func validText(s string, maximum int) bool {
	if s == "" || len(s) > maximum || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= 31 || r == 127 || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}
func reqString(v map[string]any, key string, max int) (string, bool) {
	s, ok := v[key].(string)
	return s, ok && validText(s, max)
}
func optString(v map[string]any, key string, max int) (string, bool, bool) {
	raw, present := v[key]
	if !present {
		return "", false, true
	}
	s, ok := raw.(string)
	return s, true, ok && validText(s, max)
}
func optionalBool(v map[string]any, key string) bool {
	raw, present := v[key]
	if !present {
		return true
	}
	_, ok := raw.(bool)
	return ok
}
func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		x, e := strconv.ParseInt(n.String(), 10, 64)
		return x, e == nil && strconv.FormatInt(x, 10) == n.String()
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == float64(int64(n))
	default:
		return 0, false
	}
}
func integer(v map[string]any, key string, min, max int64, required bool) bool {
	raw, present := v[key]
	if !present {
		return !required
	}
	x, ok := intValue(raw)
	return ok && x >= min && x <= max
}
func validPort(s string) bool {
	x, e := strconv.Atoi(s)
	return e == nil && x >= 1 && x <= 65535 && strconv.Itoa(x) == s
}
func validPath(s string) bool { return validText(s, 2048) && strings.HasPrefix(s, "/") }
func validRE2(s string) bool {
	if !validText(s, 2048) {
		return false
	}
	_, e := regexp.Compile(s)
	return e == nil
}
func validDuration(s string) bool {
	if len(s) > 64 || !durationRE.MatchString(s) {
		return false
	}
	d, e := time.ParseDuration(s)
	return e == nil && d > 0
}
func optionalDuration(v map[string]any, key string) bool {
	s, present, ok := optString(v, key, 64)
	return ok && (!present || validDuration(s))
}
func stringList(v any, required bool) ([]string, bool) {
	if v == nil {
		return nil, !required
	}
	raw, ok := v.([]any)
	if !ok || required && len(raw) == 0 || len(raw) > 64 {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, x := range raw {
		s, ok := x.(string)
		if !ok || !validText(s, 2048) {
			return nil, false
		}
		if _, dup := seen[s]; dup {
			return nil, false
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, true
}
func optionalList(v map[string]any, key string, fn func(string) bool) bool {
	raw, present := v[key]
	if !present {
		return true
	}
	items, ok := stringList(raw, false)
	return ok && all(items, fn)
}
func all(items []string, fn func(string) bool) bool {
	for _, item := range items {
		if !fn(item) {
			return false
		}
	}
	return true
}
func validCIDR(s string) bool {
	ip, network, e := net.ParseCIDR(s)
	return e == nil && ip.String() == strings.Split(s, "/")[0] && network.String() == s
}
func validateIPStrategy(raw any) bool {
	if raw == nil {
		return true
	}
	v, ok := raw.(map[string]any)
	if !ok || !onlyKeys(v, "depth", "excludedIPs", "ipv6Subnet") || !integer(v, "depth", 0, 100, false) || !integer(v, "ipv6Subnet", 0, 128, false) {
		return false
	}
	return optionalList(v, "excludedIPs", func(s string) bool { return net.ParseIP(s) != nil || validCIDR(s) })
}
func headerMap(v map[string]any, key string) bool {
	raw, present := v[key]
	if !present {
		return true
	}
	values, ok := raw.(map[string]any)
	if !ok || len(values) > 64 {
		return false
	}
	seen := map[string]struct{}{}
	for name, rawValue := range values {
		lower := strings.ToLower(name)
		value, ok := rawValue.(string)
		if !ok || len(name) > 128 || !headerNameRE.MatchString(name) || !validText(value, 8192) || headerSensitiveRE.MatchString(lower) {
			return false
		}
		if _, forbidden := forbiddenHeaders[lower]; forbidden {
			return false
		}
		if _, dup := seen[lower]; dup {
			return false
		}
		seen[lower] = struct{}{}
	}
	return true
}
func validHostname(s string) bool {
	return len(s) <= 253 && hostnameRE.MatchString(strings.ToLower(s)) && !strings.Contains(s, "*")
}
func validOrigin(s string) bool {
	if s == "*" || s == "null" {
		return true
	}
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}
func validMediaType(s string) bool { return len(s) <= 255 && mediaTypeRE.MatchString(s) }
func optionalToken(v map[string]any, key string, pattern *regexp.Regexp) bool {
	s, present, ok := optString(v, key, 128)
	return ok && (!present || pattern.MatchString(s))
}
func cloneSpec(spec Spec) Spec {
	raw, _ := json.Marshal(spec)
	decoded, _ := DecodeSpecUnchecked(raw)
	return decoded
}
func DecodeSpecUnchecked(raw []byte) (Spec, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out Spec
	err := decoder.Decode(&out)
	return out, err
}

// SecretReferences returns metadata-only BasicAuth references. No secret value
// or caller-supplied Kubernetes Secret name crosses this boundary.
func SecretReferences(spec Spec) []domain.SecretBindingRef {
	raw, ok := spec["basicAuth"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := raw["secretBindingRef"].(map[string]any)
	if !ok {
		return nil
	}
	version, ok := intValue(value["version"])
	if !ok {
		return nil
	}
	return []domain.SecretBindingRef{{BindingID: stringValue(value["bindingId"]), Name: stringValue(value["name"]), Key: stringValue(value["key"]), Version: version}}
}

func AppConfigSecretReferences(parsed map[string]any) ([]domain.SecretBindingRef, error) {
	spec, ok := parsed["spec"].(map[string]any)
	if !ok || ValidateDefinitions(spec) != nil {
		return nil, ErrInvalid
	}
	raw, present := spec["middlewares"]
	if !present {
		return nil, nil
	}
	definitions, ok := raw.([]any)
	if !ok {
		return nil, ErrInvalid
	}
	refs := []domain.SecretBindingRef{}
	for _, item := range definitions {
		definition, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}
		middlewareSpec, ok := definition["spec"].(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}
		refs = append(refs, SecretReferences(Spec(middlewareSpec))...)
	}
	return refs, nil
}
func stringValue(v any) string { s, _ := v.(string); return s }
