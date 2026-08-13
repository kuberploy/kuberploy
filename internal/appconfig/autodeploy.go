package appconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var (
	autoDeployImageRE  = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	autoDeployDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

const autoDeployDependenciesKey = "x-kuberploy-auto-deploy-dependencies"

// AutoDeployDependencyIntent binds the exact projected parent inputs without
// copying inherited values into the application document.
type AutoDeployDependencyIntent struct {
	Path          string `json:"path"`
	Present       bool   `json:"present"`
	BlobID        string `json:"blobId,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
}

// BindAutoDeployDependencies extends a canonical application intent with the
// exact ordered project/environment VariableSet provenance. The result stays
// non-runnable and is safe to persist as immutable policy input.
func BindAutoDeployDependencies(intent []byte, dependencies []AutoDeployDependencyIntent) ([]byte, string, error) {
	if !ValidateAutoDeployIntentTemplate(intent, autoDeployIntentDigest(intent)) || !validAutoDeployDependencies(dependencies) {
		return nil, "", errors.New("invalid auto-deploy dependency intent")
	}
	var value map[string]any
	if err := json.Unmarshal(intent, &value); err != nil {
		return nil, "", err
	}
	dependencyBytes, err := json.Marshal(dependencies)
	if err != nil {
		return nil, "", err
	}
	var canonicalDependencies any
	if err = json.Unmarshal(dependencyBytes, &canonicalDependencies); err != nil {
		return nil, "", err
	}
	value[autoDeployDependenciesKey] = canonicalDependencies
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxDocument {
		return nil, "", errors.New("invalid auto-deploy dependency intent")
	}
	return encoded, autoDeployIntentDigest(encoded), nil
}

// AutoDeployIntentTemplate returns a canonical, non-runnable representation
// of an exact valid AppConfig. It preserves every caller-selected intent while
// removing only fields that the auto-deploy pipeline must freshly derive:
// image/release, registry pull metadata, effective scheduling placement,
// reusable middleware specs, configRevision, and sslip hostnames.
func AutoDeployIntentTemplate(raw []byte) ([]byte, string, []Diagnostic) {
	parsed, _, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		return nil, "", diagnostics
	}
	template, err := canonicalAutoDeployIntent(parsed)
	if err != nil {
		return nil, "", []Diagnostic{{Code: "AutoDeployTemplateInvalid", Detail: "The AppConfig could not be reduced to a safe image-only deployment template."}}
	}
	encoded, err := json.Marshal(template)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxDocument {
		return nil, "", []Diagnostic{{Code: "AutoDeployTemplateInvalid", Detail: "The AppConfig template exceeds the supported bound."}}
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ValidateAutoDeployIntentTemplate proves stored bytes are already in the
// unique canonical form. This prevents a database row from smuggling a second
// runnable AppConfig or derived runtime material into the worker.
func ValidateAutoDeployIntentTemplate(raw []byte, digest string) bool {
	if len(raw) == 0 || len(raw) > MaxDocument || !autoDeployDigestRE.MatchString(digest) {
		return false
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil || !bytes.Equal(encoded, raw) {
		return false
	}
	sum := sha256.Sum256(raw)
	return digest == "sha256:"+hex.EncodeToString(sum[:]) && validateCanonicalIntentShape(value)
}

// ApplyAutoDeployImage is a trusted image-only mutation primitive. It first
// proves the live AppConfig still matches the pinned policy intent after
// excluding server-derived fields, then changes only delivery.release. The
// caller must subsequently run the normal scheduling, middleware, secret,
// registry-pull, edge/TLS, preview/save, and Git publication policy pipeline.
func ApplyAutoDeployImage(current, pinnedIntent []byte, pinnedDigest, image string) Candidate {
	return ApplyAutoDeployImageWithDependencies(current, pinnedIntent, pinnedDigest, image, nil)
}

// ApplyAutoDeployImageWithDependencies additionally proves that the current
// projected parent inputs match the immutable policy snapshot. Prior image-only
// writes do not change this intent, while any parent VariableSet change does.
func ApplyAutoDeployImageWithDependencies(current, pinnedIntent []byte, pinnedDigest, image string, dependencies []AutoDeployDependencyIntent) Candidate {
	if !ValidateAutoDeployIntentTemplate(pinnedIntent, pinnedDigest) || !autoDeployImageRE.MatchString(image) {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployTemplateInvalid", Detail: "The pinned image-only template is invalid."}}}
	}
	currentParsed, _, diagnostics := ParseAndValidate(current)
	if len(diagnostics) != 0 {
		return Candidate{Diagnostics: []Diagnostic{{Code: "CurrentConfigInvalid", Detail: "The stored AppConfig is invalid and cannot be auto-deployed."}}}
	}
	liveTemplate, _, diagnostics := AutoDeployIntentTemplate(current)
	if len(diagnostics) == 0 && dependencies != nil {
		var bindErr error
		liveTemplate, _, bindErr = BindAutoDeployDependencies(liveTemplate, dependencies)
		if bindErr != nil {
			diagnostics = []Diagnostic{{Code: "AutoDeployDependencyInvalid", Detail: "The projected variable dependency identity is invalid."}}
		}
	}
	liveDigest := autoDeployIntentDigest(liveTemplate)
	if len(diagnostics) != 0 || liveDigest != pinnedDigest || !bytes.Equal(liveTemplate, pinnedIntent) {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployTemplateConflict", Detail: "The current AppConfig intent changed; revise the auto-deploy policy before deploying another build."}}}
	}
	repository, digest, ok := strings.Cut(image, "@")
	if !ok || repository == "" || digest == "" {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployImageInvalid", Detail: "The verified build image is not an immutable repository digest."}}}
	}
	clone, err := cloneJSONMap(currentParsed)
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployMutationFailed", Detail: "The current AppConfig could not be cloned."}}}
	}
	spec, _ := clone["spec"].(map[string]any)
	spec["delivery"] = map[string]any{"mode": "image", "release": map[string]any{"repository": repository, "digest": digest}}
	raw, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployMutationFailed", Detail: "The image-only AppConfig could not be encoded."}}}
	}
	raw = append(raw, '\n')
	parsed, runtime, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Diagnostics: diagnostics}
	}
	hash := sha256.Sum256(raw)
	return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Hash: hash[:], Changes: compare(currentParsed, parsed)}
}

// WithAutoDeployDerived installs only server-owned fields that must be
// re-resolved on every run. The pinned intent deliberately excludes these
// values, so refreshing them cannot broaden caller-selected configuration.
func WithAutoDeployDerived(current []byte, candidate Candidate, registryPull *domain.RegistryPullReference, sslipHostname string) Candidate {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return candidate
	}
	currentParsed, _, diagnostics := ParseAndValidate(current)
	if len(diagnostics) != 0 {
		return Candidate{Diagnostics: []Diagnostic{{Code: "CurrentConfigInvalid", Detail: "The stored AppConfig is invalid and cannot be auto-deployed."}}}
	}
	spec, ok := candidate.Parsed["spec"].(map[string]any)
	if !ok {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployMutationFailed", Detail: "The AppConfig spec is unavailable."}}}
	}
	delivery, ok := spec["delivery"].(map[string]any)
	if !ok {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployMutationFailed", Detail: "The AppConfig delivery is unavailable."}}}
	}
	if registryPull == nil {
		delete(delivery, "registryPull")
	} else if !registryPull.Valid() {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployRegistryPullInvalid", Detail: "The server-derived registry pull profile is invalid."}}}
	} else {
		delivery["registryPull"] = map[string]any{"targetId": registryPull.TargetID, "profileName": registryPull.ProfileName, "profileRevision": registryPull.ProfileRevision}
	}
	if routes, ok := spec["routes"].([]any); ok {
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			dns, _ := route["dns"].(map[string]any)
			if dns["mode"] == "sslip" {
				if sslipHostname == "" {
					return Candidate{Diagnostics: []Diagnostic{{Code: "SSLIPHostnameUnavailable", Detail: "The server-derived sslip.io hostname is unavailable."}}}
				}
				route["host"] = sslipHostname
			}
		}
	}
	raw, err := json.MarshalIndent(candidate.Parsed, "", "  ")
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "AutoDeployMutationFailed", Detail: "The server-derived AppConfig could not be encoded."}}}
	}
	raw = append(raw, '\n')
	parsed, runtime, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Diagnostics: diagnostics}
	}
	hash := sha256.Sum256(raw)
	return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Hash: hash[:], Changes: compare(currentParsed, parsed)}
}

func AutoDeployUsesSSLIP(candidate Candidate) bool {
	if candidate.Parsed == nil {
		return false
	}
	spec, _ := candidate.Parsed["spec"].(map[string]any)
	routes, _ := spec["routes"].([]any)
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		dns, _ := route["dns"].(map[string]any)
		if dns["mode"] == "sslip" {
			return true
		}
	}
	return false
}

func canonicalAutoDeployIntent(parsed map[string]any) (map[string]any, error) {
	value, err := cloneJSONMap(parsed)
	if err != nil {
		return nil, err
	}
	spec, ok := value["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("AppConfig spec is unavailable")
	}
	spec["delivery"] = map[string]any{"mode": "image"}
	if runtime, ok := spec["runtime"].(map[string]any); ok {
		delete(runtime, "configRevision")
	}
	if routes, ok := spec["routes"].([]any); ok {
		for _, item := range routes {
			route, _ := item.(map[string]any)
			dns, _ := route["dns"].(map[string]any)
			if dns["mode"] == "sslip" {
				delete(route, "host")
			}
		}
	}
	if definitions, ok := spec["middlewares"].([]any); ok {
		for _, item := range definitions {
			definition, _ := item.(map[string]any)
			if _, reusable := definition["profileRef"]; reusable {
				delete(definition, "spec")
			}
		}
	}
	return value, nil
}

func validateCanonicalIntentShape(value map[string]any) bool {
	if raw, exists := value[autoDeployDependenciesKey]; exists {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return false
		}
		var dependencies []AutoDeployDependencyIntent
		if json.Unmarshal(encoded, &dependencies) != nil || !validAutoDeployDependencies(dependencies) {
			return false
		}
	}
	spec, ok := value["spec"].(map[string]any)
	if !ok {
		return false
	}
	delivery, ok := spec["delivery"].(map[string]any)
	if !ok || len(delivery) != 1 || delivery["mode"] != "image" {
		return false
	}
	if runtime, ok := spec["runtime"].(map[string]any); ok {
		if _, exists := runtime["configRevision"]; exists {
			return false
		}
	}
	if routes, ok := spec["routes"].([]any); ok {
		for _, item := range routes {
			route, _ := item.(map[string]any)
			dns, _ := route["dns"].(map[string]any)
			if dns["mode"] == "sslip" {
				if _, exists := route["host"]; exists {
					return false
				}
			}
		}
	}
	if definitions, ok := spec["middlewares"].([]any); ok {
		for _, item := range definitions {
			definition, _ := item.(map[string]any)
			if _, reusable := definition["profileRef"]; reusable {
				if _, exists := definition["spec"]; exists {
					return false
				}
			}
		}
	}
	return true
}

func autoDeployIntentDigest(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validAutoDeployDependencies(dependencies []AutoDeployDependencyIntent) bool {
	if len(dependencies) != 2 || dependencies[0].Path == "" || dependencies[1].Path == "" || dependencies[0].Path == dependencies[1].Path {
		return false
	}
	for _, dependency := range dependencies {
		if dependency.Present {
			if dependency.BlobID == "" || !autoDeployDigestRE.MatchString(dependency.ContentSHA256) {
				return false
			}
		} else if dependency.BlobID != "" || dependency.ContentSHA256 != "" {
			return false
		}
	}
	return true
}

func cloneJSONMap(value map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var clone map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	err = decoder.Decode(&clone)
	return clone, err
}
