// Package variables parses Git-backed project and environment VariableSet
// documents and resolves them with application-level runtime variables.
// Ordinary values are intentionally readable; secret material is never a
// VariableSet value and must use the runtime-secret binding path.
package variables

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/domain"
	"go.yaml.in/yaml/v3"
)

const (
	APIVersion        = "variables.kuberploy.io/v1alpha1"
	Kind              = "VariableSet"
	MaxDocumentBytes  = 128 << 10
	MaxVariables      = 256
	MaxValueBytes     = 4096
	MaxAggregateBytes = 256 << 10
)

type Scope string

const (
	ScopeProject     Scope = "project"
	ScopeEnvironment Scope = "environment"
	ScopeApplication Scope = "application"
)

type Diagnostic struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Pointer string `json:"pointer,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

type Document struct {
	Values map[string]string
	Parsed map[string]any
}

type Override struct {
	Scope  Scope  `json:"scope"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

type Effective struct {
	Name      string                   `json:"name"`
	Value     *string                  `json:"value,omitempty"`
	SecretRef *domain.SecretBindingRef `json:"secretBindingRef,omitempty"`
	Source    Scope                    `json:"source"`
	Overrides []Override               `json:"overrides,omitempty"`
}

// ParseAndValidate accepts exactly one conservative YAML document. Values are
// string scalars only; YAML coercion, aliases, tags and duplicate keys fail
// closed so Git review and the effective runtime value cannot disagree.
func ParseAndValidate(raw []byte) (Document, []Diagnostic) {
	if len(raw) == 0 || len(raw) > MaxDocumentBytes {
		return Document{}, []Diagnostic{{Code: "InvalidDocument", Detail: fmt.Sprintf("VariableSet must be between 1 and %d bytes.", MaxDocumentBytes)}}
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return Document{}, []Diagnostic{{Code: "InvalidDocument", Detail: "VariableSet must be valid UTF-8 without NUL bytes."}}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return Document{}, []Diagnostic{{Code: "InvalidYAML", Detail: bounded(err.Error())}}
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return Document{}, []Diagnostic{{Code: "MultipleDocuments", Detail: "VariableSet must contain exactly one YAML document."}}
	} else if !errors.Is(err, io.EOF) {
		return Document{}, []Diagnostic{{Code: "InvalidYAML", Detail: bounded(err.Error())}}
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return Document{}, []Diagnostic{{Code: "InvalidDocument", Detail: "VariableSet must be a YAML mapping."}}
	}
	if diagnostic := validateYAMLNode(root.Content[0], ""); diagnostic != nil {
		return Document{}, []Diagnostic{*diagnostic}
	}

	top := root.Content[0]
	allowed := map[string]bool{"apiVersion": true, "kind": true, "values": true}
	fields := mappingFields(top)
	for key, node := range fields {
		if !allowed[key] {
			return Document{}, []Diagnostic{{Code: "UnknownField", Detail: "VariableSet contains an unsupported top-level field.", Pointer: "/" + escape(key), Line: node.Line, Column: node.Column}}
		}
	}
	if scalar(fields["apiVersion"]) != APIVersion {
		return Document{}, []Diagnostic{{Code: "InvalidAPIVersion", Detail: "apiVersion must be " + APIVersion + ".", Pointer: "/apiVersion"}}
	}
	if scalar(fields["kind"]) != Kind {
		return Document{}, []Diagnostic{{Code: "InvalidKind", Detail: "kind must be VariableSet.", Pointer: "/kind"}}
	}
	valuesNode := fields["values"]
	if valuesNode == nil || valuesNode.Kind != yaml.MappingNode {
		return Document{}, []Diagnostic{{Code: "InvalidValues", Detail: "values must be a mapping of variable names to strings.", Pointer: "/values"}}
	}
	if len(valuesNode.Content)/2 > MaxVariables {
		return Document{}, []Diagnostic{{Code: "TooManyVariables", Detail: fmt.Sprintf("VariableSet supports at most %d values.", MaxVariables), Pointer: "/values"}}
	}
	values := make(map[string]string, len(valuesNode.Content)/2)
	total := 0
	for index := 0; index < len(valuesNode.Content); index += 2 {
		keyNode, valueNode := valuesNode.Content[index], valuesNode.Content[index+1]
		name := keyNode.Value
		pointer := "/values/" + escape(name)
		if !validName(name) {
			return Document{}, []Diagnostic{{Code: "InvalidName", Detail: "Variable names must match [A-Za-z_][A-Za-z0-9_]* and contain at most 128 bytes.", Pointer: pointer, Line: keyNode.Line, Column: keyNode.Column}}
		}
		if valueNode.Kind != yaml.ScalarNode || valueNode.Tag != "!!str" {
			return Document{}, []Diagnostic{{Code: "InvalidValue", Detail: "Variable values must be explicit YAML strings; quote values that resemble numbers or booleans.", Pointer: pointer, Line: valueNode.Line, Column: valueNode.Column}}
		}
		if len(valueNode.Value) > MaxValueBytes || !utf8.ValidString(valueNode.Value) || strings.IndexByte(valueNode.Value, 0) >= 0 {
			return Document{}, []Diagnostic{{Code: "InvalidValue", Detail: fmt.Sprintf("Variable values must be valid UTF-8 without NUL bytes and at most %d bytes.", MaxValueBytes), Pointer: pointer, Line: valueNode.Line, Column: valueNode.Column}}
		}
		total += len(name) + len(valueNode.Value)
		if total > MaxAggregateBytes {
			return Document{}, []Diagnostic{{Code: "VariablesTooLarge", Detail: fmt.Sprintf("Variable names and values may total at most %d bytes.", MaxAggregateBytes), Pointer: "/values"}}
		}
		values[name] = valueNode.Value
	}
	parsedValues := make(map[string]any, len(values))
	for name, value := range values {
		parsedValues[name] = value
	}
	parsed := map[string]any{"apiVersion": APIVersion, "kind": Kind, "values": parsedValues}
	return Document{Values: values, Parsed: parsed}, nil
}

// Resolve applies project, environment and application precedence. The
// returned list is name-sorted for stable previews and chart rendering. An
// application secret reference can intentionally replace an inherited
// ordinary value, but parent VariableSets can never contain secret material.
func Resolve(project, environment Document, application []domain.WorkloadEnv) ([]Effective, []domain.WorkloadValidationError) {
	type candidate struct {
		env     domain.WorkloadEnv
		source  Scope
		history []Override
	}
	merged := map[string]candidate{}
	applyValues := func(scope Scope, values map[string]string) {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			value := values[name]
			current := merged[name]
			if current.env.Name != "" {
				current.history = append(current.history, overrideOf(current))
			}
			current.env = domain.WorkloadEnv{Name: name, Value: &value}
			current.source = scope
			merged[name] = current
		}
	}
	applyValues(ScopeProject, project.Values)
	applyValues(ScopeEnvironment, environment.Values)

	problems := domain.ValidateWorkloadRuntime(domain.WorkloadRuntime{Replicas: 1, Ports: []domain.WorkloadPort{{Name: "http", ContainerPort: 8080, Protocol: "TCP"}}, Env: application, Resources: domain.WorkloadResources{Requests: domain.ResourceList{CPU: domain.DefaultCPURequest, Memory: domain.DefaultMemoryRequest}}})
	if len(problems) != 0 {
		filtered := problems[:0]
		for _, problem := range problems {
			if strings.HasPrefix(problem.Pointer, "/runtime/env") {
				filtered = append(filtered, problem)
			}
		}
		if len(filtered) != 0 {
			return nil, filtered
		}
	}
	for _, value := range application {
		current := merged[value.Name]
		if current.env.Name != "" {
			current.history = append(current.history, overrideOf(current))
		}
		current.env = cloneEnv(value)
		current.source = ScopeApplication
		merged[value.Name] = current
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]Effective, 0, len(names))
	for _, name := range names {
		value := merged[name]
		effective := Effective{Name: name, Source: value.source, Overrides: slices.Clone(value.history)}
		if value.env.Value != nil {
			ordinary := *value.env.Value
			effective.Value = &ordinary
		} else if value.env.ValueFrom != nil {
			ref := value.env.ValueFrom.SecretBindingRef
			effective.SecretRef = &ref
		}
		result = append(result, effective)
	}
	return result, nil
}

func RuntimeEnv(effective []Effective) []domain.WorkloadEnv {
	result := make([]domain.WorkloadEnv, 0, len(effective))
	for _, value := range effective {
		env := domain.WorkloadEnv{Name: value.Name}
		if value.Value != nil {
			ordinary := *value.Value
			env.Value = &ordinary
		} else if value.SecretRef != nil {
			env.ValueFrom = &domain.WorkloadEnvValueFrom{SecretBindingRef: *value.SecretRef}
		}
		result = append(result, env)
	}
	return result
}

func overrideOf(value struct {
	env     domain.WorkloadEnv
	source  Scope
	history []Override
}) Override {
	result := Override{Scope: value.source}
	if value.env.Value != nil {
		result.Value = *value.env.Value
	} else {
		result.Secret = true
	}
	return result
}

func cloneEnv(value domain.WorkloadEnv) domain.WorkloadEnv {
	result := domain.WorkloadEnv{Name: value.Name}
	if value.Value != nil {
		ordinary := *value.Value
		result.Value = &ordinary
	}
	if value.ValueFrom != nil {
		from := *value.ValueFrom
		result.ValueFrom = &from
	}
	return result
}

func validateYAMLNode(node *yaml.Node, pointer string) *Diagnostic {
	if node.Anchor != "" || node.Alias != nil || (node.Tag != "" && !strings.HasPrefix(node.Tag, "!!")) {
		return &Diagnostic{Code: "UnsafeYAML", Detail: "YAML anchors, aliases, merge keys, and custom tags are not accepted.", Pointer: pointer, Line: node.Line, Column: node.Column}
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]struct{}{}
		for index := 0; index < len(node.Content); index += 2 {
			keyNode, valueNode := node.Content[index], node.Content[index+1]
			if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" || keyNode.Value == "<<" {
				return &Diagnostic{Code: "InvalidKey", Detail: "YAML mapping keys must be ordinary strings.", Pointer: pointer, Line: keyNode.Line, Column: keyNode.Column}
			}
			childPointer := pointer + "/" + escape(keyNode.Value)
			if _, exists := seen[keyNode.Value]; exists {
				return &Diagnostic{Code: "DuplicateKey", Detail: "YAML mapping keys must be unique.", Pointer: childPointer, Line: keyNode.Line, Column: keyNode.Column}
			}
			seen[keyNode.Value] = struct{}{}
			if diagnostic := validateYAMLNode(valueNode, childPointer); diagnostic != nil {
				return diagnostic
			}
		}
		return nil
	}
	if node.Kind == yaml.SequenceNode {
		for index, child := range node.Content {
			if diagnostic := validateYAMLNode(child, fmt.Sprintf("%s/%d", pointer, index)); diagnostic != nil {
				return diagnostic
			}
		}
	} else {
		for _, child := range node.Content {
			if diagnostic := validateYAMLNode(child, pointer); diagnostic != nil {
				return diagnostic
			}
		}
	}
	return nil
}

func mappingFields(node *yaml.Node) map[string]*yaml.Node {
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		result[node.Content[index].Value] = node.Content[index+1]
	}
	return result
}

func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return ""
	}
	return node.Value
}

func validName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, char := range []byte(value) {
		if index == 0 {
			if char != '_' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
				return false
			}
		} else if char != '_' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func escape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func bounded(value string) string {
	if len(value) <= 512 {
		return value
	}
	return value[:512]
}

// MarshalParsed exists to make projection tests assert that Parsed remains a
// plain JSON-compatible object rather than a yaml.Node graph.
func MarshalParsed(document Document) ([]byte, error) { return json.Marshal(document.Parsed) }
