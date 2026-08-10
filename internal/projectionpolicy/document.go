package projectionpolicy

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
)

var (
	policyUUIDRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	policyLabelRE  = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	policyDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// RegistryPullReference is the complete caller-visible registry pull metadata
// allowed in AppConfig. It deliberately has no Kubernetes Secret name,
// credential reference, projected source name, or registry authentication
// material. Those values remain operator-owned and are re-derived from the
// exact target and runtime profile during generation activation.
type RegistryPullReference struct {
	TargetID        string `json:"targetId" yaml:"targetId"`
	ProfileName     string `json:"profileName" yaml:"profileName"`
	ProfileRevision int64  `json:"profileRevision" yaml:"profileRevision"`
}

// AppConfigDeliveryDocument is the immutable, typed delivery view supplied to
// child policies. Presence flags preserve the distinction between an omitted
// optional field and its schema-valid zero value without exposing the parsed
// map to a policy implementation.
type AppConfigDeliveryDocument struct {
	Mode              string
	Repository        string
	Digest            string
	SourceRevision    string
	HasSourceRevision bool
	RegistryPull      RegistryPullReference
	HasRegistryPull   bool
}

// AppConfigPolicyDocument is constructed once, after schema validation and
// server-side scope resolution, and then shared with every child policy. Its
// mutable workload representation is stored as a private canonical copy;
// accessors always return values detached from the document.
type AppConfigPolicyDocument struct {
	scope                     DocumentScope
	delivery                  AppConfigDeliveryDocument
	runtimeJSON               []byte
	routesJSON                []byte
	middlewareNamesJSON       []byte
	middlewareSecretsJSON     []byte
	middlewareDefinitionsJSON []byte
}

// NewAppConfigPolicyDocument closes the conversion from a schema-validated
// AppConfig plus server-resolved scope into the typed child-policy view. The
// constructor still validates every copied field so callers cannot bypass the
// closed delivery contract with a forged parsed map.
func NewAppConfigPolicyDocument(scope DocumentScope, parsed map[string]any, runtime domain.WorkloadRuntime) (AppConfigPolicyDocument, error) {
	return newAppConfigPolicyDocument(scope, parsed, runtime)
}

func newAppConfigPolicyDocument(scope DocumentScope, parsed map[string]any, runtime domain.WorkloadRuntime) (AppConfigPolicyDocument, error) {
	if !validDocumentScope(scope) || len(domain.ValidateWorkloadRuntime(runtime)) != 0 || parsed == nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	spec, ok := parsed["spec"].(map[string]any)
	if !ok {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	deliveryValue, ok := spec["delivery"].(map[string]any)
	if !ok || !onlyKeys(deliveryValue, "mode", "release", "registryPull") {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	delivery, err := decodeDeliveryDocument(deliveryValue)
	if err != nil {
		return AppConfigPolicyDocument{}, err
	}
	routes, middlewareNames, middlewareSecrets, middlewareDefinitions, err := decodeRoutePolicyDocument(spec)
	if err != nil {
		return AppConfigPolicyDocument{}, err
	}
	runtimeJSON, err := json.Marshal(runtime)
	if err != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	var detached domain.WorkloadRuntime
	if err = json.Unmarshal(runtimeJSON, &detached); err != nil || len(domain.ValidateWorkloadRuntime(detached)) != 0 {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	routesJSON, err := json.Marshal(routes)
	if err != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	middlewareNamesJSON, err := json.Marshal(middlewareNames)
	if err != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	middlewareSecretsJSON, err := json.Marshal(middlewareSecrets)
	if err != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	middlewareDefinitionsJSON, err := json.Marshal(middlewareDefinitions)
	if err != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	document := AppConfigPolicyDocument{
		scope: scope, delivery: delivery,
		runtimeJSON:               append([]byte(nil), runtimeJSON...),
		routesJSON:                append([]byte(nil), routesJSON...),
		middlewareNamesJSON:       append([]byte(nil), middlewareNamesJSON...),
		middlewareSecretsJSON:     append([]byte(nil), middlewareSecretsJSON...),
		middlewareDefinitionsJSON: append([]byte(nil), middlewareDefinitionsJSON...),
	}
	if document.validate() != nil {
		return AppConfigPolicyDocument{}, gitprojection.ErrInvalid
	}
	return document, nil
}

func decodeDeliveryDocument(value map[string]any) (AppConfigDeliveryDocument, error) {
	mode, modeOK := value["mode"].(string)
	release, releaseOK := value["release"].(map[string]any)
	if !modeOK || (mode != "image" && mode != "build") || !releaseOK || !onlyKeys(release, "repository", "digest", "sourceRevision") {
		return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
	}
	repository, repositoryOK := release["repository"].(string)
	digest, digestOK := release["digest"].(string)
	if !repositoryOK || !validReleaseRepository(repository) || !digestOK || !policyDigestRE.MatchString(digest) {
		return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
	}
	delivery := AppConfigDeliveryDocument{Mode: mode, Repository: repository, Digest: digest}
	if raw, present := release["sourceRevision"]; present {
		sourceRevision, ok := raw.(string)
		if !ok || !utf8.ValidString(sourceRevision) || utf8.RuneCountInString(sourceRevision) > 128 {
			return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
		}
		delivery.SourceRevision, delivery.HasSourceRevision = sourceRevision, true
	}
	rawPull, hasPull := value["registryPull"]
	if !hasPull {
		return delivery, nil
	}
	pull, ok := rawPull.(map[string]any)
	if !ok || !onlyKeys(pull, "targetId", "profileName", "profileRevision") {
		return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
	}
	targetID, targetOK := pull["targetId"].(string)
	profileName, profileOK := pull["profileName"].(string)
	revisionNumber, revisionOK := pull["profileRevision"].(json.Number)
	if !targetOK || !policyUUIDRE.MatchString(targetID) || !profileOK || !policyLabelRE.MatchString(profileName) || !revisionOK {
		return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
	}
	revision, err := strconv.ParseInt(revisionNumber.String(), 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != revisionNumber.String() {
		return AppConfigDeliveryDocument{}, gitprojection.ErrInvalid
	}
	delivery.RegistryPull = RegistryPullReference{TargetID: targetID, ProfileName: profileName, ProfileRevision: revision}
	delivery.HasRegistryPull = true
	return delivery, nil
}

func validReleaseRepository(value string) bool {
	return value != "" && len(value) <= 255 && utf8.ValidString(value) && !strings.Contains(value, "@") &&
		strings.IndexFunc(value, unicode.IsSpace) == -1
}

func onlyKeys(value map[string]any, allowed ...string) bool {
	if len(value) > len(allowed) {
		return false
	}
	for key := range value {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validDocumentScope(scope DocumentScope) bool {
	if scope.Binding.Validate() != nil || scope.Binding.Kind != gitprojection.BindingEnvironment || !namespaceRE.MatchString(scope.Namespace) ||
		scope.ApplicationID == "" || scope.SourceRevision != scope.Binding.TargetHeadRevision || scope.ConfigRevision == "" {
		return false
	}
	expectedPath, err := gitprojection.ApplicationPath(scope.Binding, scope.ApplicationID)
	return err == nil && expectedPath == scope.Path
}

func (d AppConfigPolicyDocument) validate() error {
	if !validDocumentScope(d.scope) || (d.delivery.Mode != "image" && d.delivery.Mode != "build") ||
		!validReleaseRepository(d.delivery.Repository) || !policyDigestRE.MatchString(d.delivery.Digest) ||
		d.delivery.HasSourceRevision && (!utf8.ValidString(d.delivery.SourceRevision) || utf8.RuneCountInString(d.delivery.SourceRevision) > 128) {
		return gitprojection.ErrInvalid
	}
	if d.delivery.HasRegistryPull && (!policyUUIDRE.MatchString(d.delivery.RegistryPull.TargetID) ||
		!policyLabelRE.MatchString(d.delivery.RegistryPull.ProfileName) || d.delivery.RegistryPull.ProfileRevision <= 0) {
		return gitprojection.ErrInvalid
	}
	var runtime domain.WorkloadRuntime
	if len(d.runtimeJSON) == 0 || json.Unmarshal(d.runtimeJSON, &runtime) != nil || len(domain.ValidateWorkloadRuntime(runtime)) != 0 {
		return gitprojection.ErrInvalid
	}
	var routes []AppConfigRouteDocument
	var middlewareNames []string
	var middlewareSecrets []domain.SecretBindingRef
	var middlewareDefinitions []middlewareprofiles.MaterializedDefinition
	if len(d.routesJSON) == 0 || json.Unmarshal(d.routesJSON, &routes) != nil ||
		len(d.middlewareNamesJSON) == 0 || json.Unmarshal(d.middlewareNamesJSON, &middlewareNames) != nil ||
		len(d.middlewareSecretsJSON) == 0 || json.Unmarshal(d.middlewareSecretsJSON, &middlewareSecrets) != nil ||
		len(d.middlewareDefinitionsJSON) == 0 || json.Unmarshal(d.middlewareDefinitionsJSON, &middlewareDefinitions) != nil ||
		validateRoutePolicyDocument(routes, middlewareNames, runtime) != nil {
		return gitprojection.ErrInvalid
	}
	if len(middlewareDefinitions) != len(middlewareNames) {
		return gitprojection.ErrInvalid
	}
	for _, ref := range middlewareSecrets {
		if !ref.Valid() || ref.Key != "users" {
			return gitprojection.ErrInvalid
		}
	}
	return nil
}

// Scope returns the exact server-resolved identity by value.
func (d AppConfigPolicyDocument) Scope() DocumentScope { return d.scope }

// Delivery returns a detached value-only delivery view.
func (d AppConfigPolicyDocument) Delivery() AppConfigDeliveryDocument { return d.delivery }

// Runtime returns a deep clone; callers cannot mutate the document seen by a
// later child policy.
func (d AppConfigPolicyDocument) Runtime() domain.WorkloadRuntime {
	var runtime domain.WorkloadRuntime
	_ = json.Unmarshal(d.runtimeJSON, &runtime)
	return runtime
}

// Routes returns a deep clone of the schema-validated route policy view.
func (d AppConfigPolicyDocument) Routes() []AppConfigRouteDocument {
	var routes []AppConfigRouteDocument
	_ = json.Unmarshal(d.routesJSON, &routes)
	return routes
}

// MiddlewareNames returns the exact, unique names defined in this AppConfig.
// Middleware specifications stay in the schema/render boundary; child policy
// code receives only names needed to prove route references are closed.
func (d AppConfigPolicyDocument) MiddlewareNames() []string {
	var names []string
	_ = json.Unmarshal(d.middlewareNamesJSON, &names)
	return names
}

// MiddlewareSecretReferences returns metadata-only BasicAuth binding
// identities. Values and Kubernetes Secret names are never part of AppConfig.
func (d AppConfigPolicyDocument) MiddlewareSecretReferences() []domain.SecretBindingRef {
	var refs []domain.SecretBindingRef
	_ = json.Unmarshal(d.middlewareSecretsJSON, &refs)
	return refs
}

func (d AppConfigPolicyDocument) MiddlewareDefinitions() []middlewareprofiles.MaterializedDefinition {
	var definitions []middlewareprofiles.MaterializedDefinition
	_ = json.Unmarshal(d.middlewareDefinitionsJSON, &definitions)
	return definitions
}
