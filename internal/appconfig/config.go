// Package appconfig parses, validates and compares the single desired-state
// document edited through the deployment config API. It is deliberately pure:
// callers may run it before opening a database transaction.
package appconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	appschema "github.com/kuberploy/kuberploy/schema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const (
	DocumentID  = "app.yaml"
	MaxDocument = 256 << 10
	MaxDiff     = 64 << 10
)

var EditablePointers = []string{
	"/spec/runtime/replicas", "/spec/runtime/strategy", "/spec/runtime/ports", "/spec/runtime/env",
	"/spec/runtime/resources", "/spec/runtime/schedulingProfile",
	"/spec/runtime/probes",
	"/spec/runtime/command", "/spec/runtime/args", "/spec/runtime/terminationGracePeriodSeconds",
	"/spec/routes", "/spec/middlewares",
}

var LockedPointers = []string{
	"/apiVersion", "/kind", "/metadata/id", "/metadata/name",
	"/spec/projectId", "/spec/applicationId", "/spec/environmentId", "/spec/delivery",
	"/spec/runtime/configRevision", "/spec/runtime/nodeSelector", "/spec/runtime/affinity",
	"/spec/runtime/topologySpreadConstraints", "/spec/runtime/tolerations", "/spec/runtime/priorityClassName",
}

type Diagnostic struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Pointer string `json:"pointer,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

type DocumentChange struct {
	DocumentID string `json:"documentId"`
	RawYAML    string `json:"rawYaml"`
}

type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Change struct {
	Mode      string           `json:"mode"`
	Documents []DocumentChange `json:"documents,omitempty"`
	Patch     []PatchOperation `json:"patch,omitempty"`
}

type Candidate struct {
	Raw         []byte
	Parsed      map[string]any
	Runtime     domain.WorkloadRuntime
	Hash        []byte
	Changes     []SemanticChange
	Diagnostics []Diagnostic
}

type SemanticChange struct {
	Pointer string `json:"pointer"`
	Summary string `json:"summary"`
	Before  any    `json:"before,omitempty"`
	After   any    `json:"after,omitempty"`
}

var (
	compiledOnce sync.Once
	compiled     *jsonschema.Schema
	compileErr   error
)

func schemaValidator() (*jsonschema.Schema, error) {
	compiledOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		var document any
		decoder := json.NewDecoder(bytes.NewReader(appschema.AppConfig))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			compileErr = err
			return
		}
		if err := compiler.AddResource("appconfig.json", document); err != nil {
			compileErr = err
			return
		}
		compiled, compileErr = compiler.Compile("appconfig.json")
	})
	return compiled, compileErr
}

func normalize(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxDocument {
		return nil, fmt.Errorf("AppConfig must be between 1 and %d bytes", MaxDocument)
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte("\r"), []byte("\n"))
	if bytes.IndexByte(raw, 0) >= 0 {
		return nil, errors.New("AppConfig contains a NUL byte")
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		raw = append(append([]byte(nil), raw...), '\n')
	} else {
		raw = append([]byte(nil), raw...)
	}
	return raw, nil
}

func ParseAndValidate(raw []byte) (map[string]any, domain.WorkloadRuntime, []Diagnostic) {
	raw, err := normalize(raw)
	if err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidDocument", Detail: err.Error()}}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var node yaml.Node
	if err = decoder.Decode(&node); err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidYAML", Detail: bounded(err.Error(), 512)}}
	}
	var extra yaml.Node
	if err = decoder.Decode(&extra); err == nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "MultipleDocuments", Detail: "AppConfig must contain exactly one YAML document."}}
	}
	if !errors.Is(err, io.EOF) {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidYAML", Detail: bounded(err.Error(), 512)}}
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidDocument", Detail: "AppConfig must be a YAML mapping."}}
	}
	if diagnostic := forbiddenYAML(node.Content[0]); diagnostic != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{*diagnostic}
	}
	var decoded any
	if err = node.Content[0].Decode(&decoded); err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidYAML", Detail: bounded(err.Error(), 512)}}
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidDocument", Detail: bounded(err.Error(), 512)}}
	}
	var parsed map[string]any
	jsonDecoder := json.NewDecoder(bytes.NewReader(encoded))
	jsonDecoder.UseNumber()
	if err = jsonDecoder.Decode(&parsed); err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "InvalidDocument", Detail: bounded(err.Error(), 512)}}
	}
	validator, err := schemaValidator()
	if err != nil {
		return nil, domain.WorkloadRuntime{}, []Diagnostic{{Code: "SchemaUnavailable", Detail: "The AppConfig schema could not be compiled."}}
	}
	var diagnostics []Diagnostic
	if err = validator.Validate(parsed); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			diagnostics = append(diagnostics, flattenValidation(validation)...)
		} else {
			diagnostics = append(diagnostics, Diagnostic{Code: "SchemaViolation", Detail: bounded(err.Error(), 512)})
		}
	}
	spec, specOK := parsed["spec"].(map[string]any)
	if !specOK {
		return parsed, domain.WorkloadRuntime{}, diagnostics
	}
	if err = middlewareprofiles.ValidateDefinitions(spec); err != nil {
		diagnostics = append(diagnostics, Diagnostic{Code: "MiddlewarePolicyViolation", Detail: "Middleware definitions and ordered route references must use only the closed, bounded platform policy.", Pointer: "/spec/middlewares"})
	}
	runtimeValue, runtimeOK := spec["runtime"]
	if !runtimeOK {
		return parsed, domain.WorkloadRuntime{}, diagnostics
	}
	runtimeJSON, _ := json.Marshal(runtimeValue)
	var runtime domain.WorkloadRuntime
	if err = json.Unmarshal(runtimeJSON, &runtime); err != nil {
		if len(diagnostics) == 0 {
			diagnostics = append(diagnostics, Diagnostic{Code: "InvalidRuntime", Detail: bounded(err.Error(), 512), Pointer: "/spec/runtime"})
		}
		return parsed, runtime, diagnostics
	}
	for _, problem := range domain.ValidateWorkloadRuntime(runtime) {
		diagnostics = append(diagnostics, Diagnostic{Code: problem.Code, Detail: problem.Detail, Pointer: domainPointer(problem.Pointer)})
	}
	return parsed, runtime, diagnostics
}

// MaterializedImage returns the exact immutable image selected by a validated
// image-mode AppConfig. Callers must still reject any ParseAndValidate
// diagnostics before trusting this projection.
func MaterializedImage(parsed map[string]any) (string, bool) {
	mode, modeOK := stringAt(parsed, "/spec/delivery/mode")
	repository, repositoryOK := stringAt(parsed, "/spec/delivery/release/repository")
	digest, digestOK := stringAt(parsed, "/spec/delivery/release/digest")
	image := repository + "@" + digest
	return image, modeOK && mode == "image" && repositoryOK && digestOK && autoDeployImageRE.MatchString(image)
}

// ValidateBinding is the final execution-boundary defense for durable
// operation snapshots. Schema-valid YAML is still rejected unless all
// platform/release-owned identity and image fields match server-resolved data.
func ValidateBinding(raw []byte, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) []Diagnostic {
	parsed, _, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) > 0 {
		return diagnostics
	}
	expected := map[string]string{
		"/metadata/id":        application.ID,
		"/metadata/name":      application.Slug,
		"/spec/projectId":     project.ID,
		"/spec/applicationId": application.ID,
		"/spec/environmentId": environment.ID,
		"/spec/delivery/mode": "image",
	}
	parts := strings.SplitN(deployment.Image, "@", 2)
	if len(parts) != 2 {
		return []Diagnostic{{Code: "OperationBindingMismatch", Detail: "The operation image is not an immutable repository@digest reference.", Pointer: "/spec/delivery"}}
	}
	expected["/spec/delivery/release/repository"] = parts[0]
	expected["/spec/delivery/release/digest"] = parts[1]
	var out []Diagnostic
	for path, want := range expected {
		got, ok := stringAt(parsed, path)
		if !ok || got != want {
			out = append(out, Diagnostic{Code: "OperationBindingMismatch", Detail: "The operation snapshot does not match server-owned identity or release state.", Pointer: path})
		}
	}
	if _, ok := valueAt(parsed, "/spec/delivery/release/sourceRevision"); ok {
		out = append(out, Diagnostic{Code: "OperationBindingMismatch", Detail: "sourceRevision is release-owned and is not available in this deployment operation model.", Pointer: "/spec/delivery/release/sourceRevision"})
	}
	if len(out) > 1 {
		sortDiagnostics(out)
	}
	return out
}

func valueAt(root any, path string) (any, bool) {
	tokens, err := parsePointer(path)
	if err != nil {
		return nil, false
	}
	current := root
	for _, token := range tokens {
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[token]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func stringAt(root any, path string) (string, bool) {
	value, ok := valueAt(root, path)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}
func sortDiagnostics(values []Diagnostic) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].Pointer < values[j-1].Pointer; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func domainPointer(value string) string {
	value = "/spec" + value
	const selector = "/spec/runtime/nodeSelector/"
	if strings.HasPrefix(value, selector) {
		return selector + escape(strings.TrimPrefix(value, selector))
	}
	return value
}

func forbiddenYAML(node *yaml.Node) *Diagnostic {
	if node.Anchor != "" || node.Alias != nil || (node.Tag != "" && !strings.HasPrefix(node.Tag, "!!")) {
		return &Diagnostic{Code: "UnsafeYAML", Detail: "YAML anchors, aliases, and custom tags are not accepted.", Line: node.Line, Column: node.Column}
	}
	for _, child := range node.Content {
		if diagnostic := forbiddenYAML(child); diagnostic != nil {
			return diagnostic
		}
	}
	return nil
}

func flattenValidation(root *jsonschema.ValidationError) []Diagnostic {
	leaves := []*jsonschema.ValidationError{}
	var walk func(*jsonschema.ValidationError)
	walk = func(current *jsonschema.ValidationError) {
		if len(current.Causes) == 0 {
			leaves = append(leaves, current)
			return
		}
		for _, cause := range current.Causes {
			walk(cause)
		}
	}
	walk(root)
	if len(leaves) > 64 {
		leaves = leaves[:64]
	}
	out := make([]Diagnostic, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, Diagnostic{Code: "SchemaViolation", Pointer: pointer(leaf.InstanceLocation), Detail: bounded(leaf.Error(), 512)})
	}
	return out
}

func Apply(current []byte, change Change) Candidate {
	currentParsed, currentRuntime, currentDiagnostics := ParseAndValidate(current)
	if len(currentDiagnostics) > 0 {
		return Candidate{Diagnostics: []Diagnostic{{Code: "CurrentConfigInvalid", Detail: "The stored AppConfig is invalid and cannot be edited."}}}
	}
	var raw []byte
	switch change.Mode {
	case "yaml":
		if len(change.Documents) != 1 || change.Documents[0].DocumentID != DocumentID || len(change.Patch) != 0 {
			return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidChange", Detail: "YAML mode requires exactly one app.yaml document replacement."}}}
		}
		raw = []byte(change.Documents[0].RawYAML)
	case "jsonPatch":
		if len(change.Patch) == 0 || len(change.Patch) > 128 || len(change.Documents) != 0 {
			return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidChange", Detail: "JSON Patch mode requires between 1 and 128 operations."}}}
		}
		// RFC 6902 application mutates containers. Clone first so the immutable
		// baseline remains available for fail-closed locked-field comparison.
		baselineJSON, _ := json.Marshal(currentParsed)
		var value any
		cloneDecoder := json.NewDecoder(bytes.NewReader(baselineJSON))
		cloneDecoder.UseNumber()
		if err := cloneDecoder.Decode(&value); err != nil {
			return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidPatch", Detail: "The current AppConfig could not be cloned."}}}
		}
		var err error
		for index, operation := range change.Patch {
			value, err = applyPatch(value, operation)
			if err != nil {
				return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidPatch", Detail: fmt.Sprintf("patch[%d]: %s", index, bounded(err.Error(), 384)), Pointer: operation.Path}}}
			}
		}
		raw, err = json.MarshalIndent(value, "", "  ")
		if err != nil {
			return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidPatch", Detail: bounded(err.Error(), 512)}}}
		}
		raw = append(raw, '\n')
	default:
		return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidChange", Detail: "mode must be yaml or jsonPatch."}}}
	}
	normalized, err := normalize(raw)
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "InvalidDocument", Detail: err.Error()}}}
	}
	parsed, runtime, diagnostics := ParseAndValidate(normalized)
	schedulingDetached := false
	if len(diagnostics) > 0 {
		originalRaw, originalParsed, originalRuntime, originalDiagnostics := normalized, parsed, runtime, diagnostics
		normalized, parsed, runtime, diagnostics, schedulingDetached = dematerializeSchedulingDetach(currentParsed, currentRuntime, parsed, runtime, normalized)
		if !schedulingDetached {
			return Candidate{Raw: originalRaw, Parsed: originalParsed, Runtime: originalRuntime, Diagnostics: originalDiagnostics}
		}
		if len(diagnostics) > 0 {
			return Candidate{Raw: normalized, Parsed: parsed, Runtime: runtime, Diagnostics: diagnostics}
		}
	}
	changes := compare(currentParsed, parsed)
	for _, semantic := range changes {
		if !editable(semantic.Pointer) && !(schedulingDetached && effectiveSchedulingPointer(semantic.Pointer)) {
			diagnostics = append(diagnostics, Diagnostic{Code: "LockedField", Detail: "This field is owned by Kuberploy or the release pipeline.", Pointer: semantic.Pointer})
		}
	}
	if len(diagnostics) > 64 {
		diagnostics = diagnostics[:64]
	}
	hash := sha256.Sum256(normalized)
	return Candidate{Raw: normalized, Parsed: parsed, Runtime: runtime, Hash: hash[:], Changes: changes, Diagnostics: diagnostics}
}

// dematerializeSchedulingDetach recognizes only removal of a previously exact
// scheduling profile reference. Effective placement fields may be unchanged
// (as they are in the raw YAML editor) or removed with the reference, but a
// caller cannot substitute any of their values. The server then removes the
// whole profile-owned fragment atomically before the ordinary schema pass.
func dematerializeSchedulingDetach(currentParsed map[string]any, currentRuntime domain.WorkloadRuntime, parsed map[string]any, runtime domain.WorkloadRuntime, raw []byte) ([]byte, map[string]any, domain.WorkloadRuntime, []Diagnostic, bool) {
	if currentRuntime.SchedulingProfile == nil || parsed == nil {
		return raw, parsed, runtime, nil, false
	}
	spec, ok := parsed["spec"].(map[string]any)
	if !ok {
		return raw, parsed, runtime, nil, false
	}
	runtimeValue, ok := spec["runtime"].(map[string]any)
	if !ok {
		return raw, parsed, runtime, nil, false
	}
	if _, present := runtimeValue["schedulingProfile"]; present {
		return raw, parsed, runtime, nil, false
	}
	currentSpec, ok := currentParsed["spec"].(map[string]any)
	if !ok {
		return raw, parsed, runtime, nil, false
	}
	currentRuntimeValue, ok := currentSpec["runtime"].(map[string]any)
	if !ok {
		return raw, parsed, runtime, nil, false
	}
	for _, key := range []string{"nodeSelector", "affinity", "topologySpreadConstraints", "tolerations", "priorityClassName"} {
		candidateValue, candidatePresent := runtimeValue[key]
		currentValue, currentPresent := currentRuntimeValue[key]
		if candidatePresent && (!currentPresent || !reflect.DeepEqual(candidateValue, currentValue)) {
			pointer := "/spec/runtime/" + key
			return raw, parsed, runtime, []Diagnostic{{Code: "LockedField", Detail: "Effective scheduling fields cannot be changed while detaching their profile.", Pointer: pointer}}, true
		}
	}
	for _, semantic := range compare(currentParsed, parsed) {
		if !editable(semantic.Pointer) && !effectiveSchedulingPointer(semantic.Pointer) {
			return raw, parsed, runtime, []Diagnostic{{Code: "LockedField", Detail: "This field is owned by Kuberploy or the release pipeline.", Pointer: semantic.Pointer}}, true
		}
	}
	runtime.SchedulingProfile = nil
	runtime.NodeSelector = nil
	runtime.Affinity = nil
	runtime.TopologySpreadConstraints = nil
	runtime.Tolerations = nil
	runtime.PriorityClassName = ""
	encodedRuntime, err := json.Marshal(runtime)
	if err != nil {
		return raw, parsed, runtime, []Diagnostic{{Code: "SchedulingDematerializationFailed", Detail: "The scheduling profile could not be detached safely."}}, true
	}
	var cleanRuntime map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encodedRuntime))
	decoder.UseNumber()
	if err = decoder.Decode(&cleanRuntime); err != nil {
		return raw, parsed, runtime, []Diagnostic{{Code: "SchedulingDematerializationFailed", Detail: "The scheduling profile could not be detached safely."}}, true
	}
	spec["runtime"] = cleanRuntime
	cleanRaw, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return raw, parsed, runtime, []Diagnostic{{Code: "SchedulingDematerializationFailed", Detail: "The scheduling profile could not be detached safely."}}, true
	}
	cleanRaw = append(cleanRaw, '\n')
	cleanParsed, cleanRuntimeValue, diagnostics := ParseAndValidate(cleanRaw)
	return cleanRaw, cleanParsed, cleanRuntimeValue, diagnostics, true
}

func effectiveSchedulingPointer(pointer string) bool {
	for _, root := range []string{
		"/spec/runtime/nodeSelector", "/spec/runtime/affinity", "/spec/runtime/topologySpreadConstraints",
		"/spec/runtime/tolerations", "/spec/runtime/priorityClassName",
	} {
		if pointer == root || strings.HasPrefix(pointer, root+"/") {
			return true
		}
	}
	return false
}

// WithResolvedScheduling installs a server-resolved runtime after Apply has
// established that the caller changed only editable intent. It is deliberately
// separate from Apply: callers may select schedulingProfile, but only this
// trusted boundary may write effective placement fields.
func WithResolvedScheduling(current []byte, candidate Candidate, runtime domain.WorkloadRuntime) Candidate {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return candidate
	}
	currentParsed, _, currentDiagnostics := ParseAndValidate(current)
	if len(currentDiagnostics) != 0 {
		return Candidate{Diagnostics: []Diagnostic{{Code: "CurrentConfigInvalid", Detail: "The stored AppConfig is invalid and cannot be edited."}}}
	}
	encodedRuntime, err := json.Marshal(runtime)
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "SchedulingMaterializationFailed", Detail: "The effective scheduling profile could not be encoded."}}}
	}
	var runtimeValue map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encodedRuntime))
	decoder.UseNumber()
	if err = decoder.Decode(&runtimeValue); err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "SchedulingMaterializationFailed", Detail: "The effective scheduling profile could not be decoded."}}}
	}
	spec, ok := candidate.Parsed["spec"].(map[string]any)
	if !ok {
		return Candidate{Diagnostics: []Diagnostic{{Code: "SchedulingMaterializationFailed", Detail: "The AppConfig spec is unavailable."}}}
	}
	spec["runtime"] = runtimeValue
	raw, err := json.MarshalIndent(candidate.Parsed, "", "  ")
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "SchedulingMaterializationFailed", Detail: "The effective AppConfig could not be encoded."}}}
	}
	raw = append(raw, '\n')
	parsed, resolvedRuntime, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		return Candidate{Raw: raw, Parsed: parsed, Runtime: resolvedRuntime, Diagnostics: diagnostics}
	}
	hash := sha256.Sum256(raw)
	return Candidate{Raw: raw, Parsed: parsed, Runtime: resolvedRuntime, Hash: hash[:], Changes: compare(currentParsed, parsed)}
}

// WithResolvedMiddlewares replaces only the reusable-profile material with a
// server-resolved exact copy. Caller-selected logical ordering is preserved;
// Kubernetes names remain renderer-derived.
func WithResolvedMiddlewares(current []byte, candidate Candidate, definitions []middlewareprofiles.MaterializedDefinition) Candidate {
	if len(candidate.Diagnostics) != 0 || candidate.Parsed == nil {
		return candidate
	}
	currentParsed, _, currentDiagnostics := ParseAndValidate(current)
	if len(currentDiagnostics) != 0 {
		return Candidate{Diagnostics: []Diagnostic{{Code: "CurrentConfigInvalid", Detail: "The stored AppConfig is invalid and cannot be edited."}}}
	}
	spec, ok := candidate.Parsed["spec"].(map[string]any)
	if !ok {
		return Candidate{Diagnostics: []Diagnostic{{Code: "MiddlewareMaterializationFailed", Detail: "The AppConfig spec is unavailable."}}}
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "MiddlewareMaterializationFailed", Detail: "The reusable middleware profiles could not be encoded."}}}
	}
	var values []any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&values); err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "MiddlewareMaterializationFailed", Detail: "The reusable middleware profiles could not be decoded."}}}
	}
	spec["middlewares"] = values
	raw, err := json.MarshalIndent(candidate.Parsed, "", "  ")
	if err != nil {
		return Candidate{Diagnostics: []Diagnostic{{Code: "MiddlewareMaterializationFailed", Detail: "The effective AppConfig could not be encoded."}}}
	}
	raw = append(raw, '\n')
	parsed, runtime, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 {
		return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Diagnostics: diagnostics}
	}
	hash := sha256.Sum256(raw)
	return Candidate{Raw: raw, Parsed: parsed, Runtime: runtime, Hash: hash[:], Changes: compare(currentParsed, parsed)}
}

func editable(path string) bool {
	for _, root := range EditablePointers {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func compare(before, after any) []SemanticChange {
	changes := make([]SemanticChange, 0)
	var walk func(string, any, any)
	walk = func(path string, left, right any) {
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		if bytes.Equal(leftJSON, rightJSON) {
			return
		}
		lm, lok := left.(map[string]any)
		rm, rok := right.(map[string]any)
		if lok && rok {
			keys := map[string]struct{}{}
			for key := range lm {
				keys[key] = struct{}{}
			}
			for key := range rm {
				keys[key] = struct{}{}
			}
			ordered := make([]string, 0, len(keys))
			for key := range keys {
				ordered = append(ordered, key)
			}
			sortStrings(ordered)
			for _, key := range ordered {
				walk(path+"/"+escape(key), lm[key], rm[key])
			}
			return
		}
		if len(changes) < 256 {
			beforeValue, afterValue, summary := boundedSemanticValues(left, right)
			changes = append(changes, SemanticChange{Pointer: emptyRoot(path), Summary: summary, Before: beforeValue, After: afterValue})
		}
	}
	walk("", before, after)
	return changes
}

func boundedSemanticValues(before, after any) (any, any, string) {
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	if len(left)+len(right) > 4096 {
		return nil, nil, "value changed (details omitted by preview bound)"
	}
	return before, after, "value changed"
}

func GitDiff(path string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- a/")
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString("+++ b/")
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString("@@ bounded full-document diff @@\n")
	for _, line := range strings.Split(strings.TrimSuffix(string(before), "\n"), "\n") {
		b.WriteByte('-')
		b.WriteString(line)
		b.WriteByte('\n')
		if b.Len() >= MaxDiff {
			return boundedDiff(b.String())
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(after), "\n"), "\n") {
		b.WriteByte('+')
		b.WriteString(line)
		b.WriteByte('\n')
		if b.Len() >= MaxDiff {
			return boundedDiff(b.String())
		}
	}
	return b.String()
}

func boundedDiff(value string) string {
	if len(value) <= MaxDiff {
		return value
	}
	const marker = "\n... diff truncated by Kuberploy ...\n"
	return value[:MaxDiff-len(marker)] + marker
}

func applyPatch(root any, operation PatchOperation) (any, error) {
	if operation.Op != "add" && operation.Op != "replace" && operation.Op != "remove" {
		return nil, errors.New("op must be add, replace, or remove")
	}
	if operation.Path == "" {
		return nil, errors.New("the document root cannot be replaced")
	}
	tokens, err := parsePointer(operation.Path)
	if err != nil {
		return nil, err
	}
	return patchAt(root, tokens, operation)
}

func patchAt(current any, tokens []string, operation PatchOperation) (any, error) {
	if len(tokens) == 0 {
		return nil, errors.New("empty patch path")
	}
	key := tokens[0]
	if len(tokens) == 1 {
		switch typed := current.(type) {
		case map[string]any:
			_, exists := typed[key]
			if operation.Op != "add" && !exists {
				return nil, errors.New("path does not exist")
			}
			if operation.Op == "remove" {
				delete(typed, key)
			} else {
				typed[key] = operation.Value
			}
			return typed, nil
		case []any:
			index, err := patchIndex(key, len(typed), operation.Op == "add")
			if err != nil {
				return nil, err
			}
			switch operation.Op {
			case "add":
				typed = append(typed, nil)
				copy(typed[index+1:], typed[index:])
				typed[index] = operation.Value
			case "replace":
				typed[index] = operation.Value
			case "remove":
				typed = append(typed[:index], typed[index+1:]...)
			}
			return typed, nil
		default:
			return nil, errors.New("path parent is not an object or array")
		}
	}
	switch typed := current.(type) {
	case map[string]any:
		child, ok := typed[key]
		if !ok {
			return nil, errors.New("path does not exist")
		}
		updated, err := patchAt(child, tokens[1:], operation)
		if err != nil {
			return nil, err
		}
		typed[key] = updated
		return typed, nil
	case []any:
		index, err := patchIndex(key, len(typed), false)
		if err != nil {
			return nil, err
		}
		updated, err := patchAt(typed[index], tokens[1:], operation)
		if err != nil {
			return nil, err
		}
		typed[index] = updated
		return typed, nil
	default:
		return nil, errors.New("path parent is not an object or array")
	}
}

func patchIndex(raw string, length int, add bool) (int, error) {
	if raw == "-" && add {
		return length, nil
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 || index > length || (!add && index == length) {
		return 0, errors.New("array index is out of bounds")
	}
	return index, nil
}

func parsePointer(value string) ([]string, error) {
	if !strings.HasPrefix(value, "/") {
		return nil, errors.New("path must be an absolute JSON pointer")
	}
	parts := strings.Split(value[1:], "/")
	for index, part := range parts {
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(part, "~1", ""), "~0", ""), "~") {
			return nil, errors.New("invalid JSON pointer escape")
		}
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func pointer(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = escape(p)
	}
	return "/" + strings.Join(escaped, "/")
}
func escape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func emptyRoot(value string) string {
	if value == "" {
		return "/"
	}
	return value
}
func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
