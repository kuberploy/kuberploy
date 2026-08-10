package variables

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestParseVariableSetStrictly(t *testing.T) {
	raw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  LOG_LEVEL: info\n  FEATURE_FLAG: \"true\"\n")
	document, diagnostics := ParseAndValidate(raw)
	if len(diagnostics) != 0 || document.Values["LOG_LEVEL"] != "info" || document.Values["FEATURE_FLAG"] != "true" {
		t.Fatalf("document=%#v diagnostics=%#v", document, diagnostics)
	}
	encoded, err := MarshalParsed(document)
	if err != nil || !strings.Contains(string(encoded), `"LOG_LEVEL":"info"`) {
		t.Fatalf("parsed JSON=%s err=%v", encoded, err)
	}
}

func TestParseVariableSetRejectsAmbiguousOrUnsafeYAML(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		code string
	}{
		{"coerced boolean", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  ENABLED: true\n", "InvalidValue"},
		{"duplicate", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  A: first\n  A: second\n", "DuplicateKey"},
		{"alias", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  A: &value first\n  B: *value\n", "UnsafeYAML"},
		{"unknown field", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\nmetadata: {}\n", "UnknownField"},
		{"multiple documents", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues: {}\n---\nvalues: {}\n", "MultipleDocuments"},
		{"invalid name", "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  BAD-NAME: value\n", "InvalidName"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := ParseAndValidate([]byte(test.raw))
			if len(diagnostics) != 1 || diagnostics[0].Code != test.code {
				t.Fatalf("diagnostics=%#v", diagnostics)
			}
		})
	}
}

func TestResolveUsesProjectEnvironmentApplicationPrecedence(t *testing.T) {
	project := Document{Values: map[string]string{"A": "project-a", "PROJECT_ONLY": "project"}}
	environment := Document{Values: map[string]string{"A": "environment-a", "ENV_ONLY": "environment"}}
	ordinary := "application"
	application := []domain.WorkloadEnv{
		{Name: "A", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{BindingID: "44444444-4444-4444-8444-444444444444", Name: "app-a", Key: "value", Version: 1}}},
		{Name: "APP_ONLY", Value: &ordinary},
	}
	effective, problems := Resolve(project, environment, application)
	if len(problems) != 0 {
		t.Fatalf("problems=%#v", problems)
	}
	if len(effective) != 4 || effective[0].Name != "A" || effective[1].Name != "APP_ONLY" || effective[2].Name != "ENV_ONLY" || effective[3].Name != "PROJECT_ONLY" {
		t.Fatalf("effective order=%#v", effective)
	}
	if effective[0].Source != ScopeApplication || effective[0].SecretRef == nil || len(effective[0].Overrides) != 2 ||
		effective[0].Overrides[0].Scope != ScopeProject || effective[0].Overrides[0].Value != "project-a" ||
		effective[0].Overrides[1].Scope != ScopeEnvironment || effective[0].Overrides[1].Value != "environment-a" {
		t.Fatalf("override history=%#v", effective[0])
	}
	runtime := RuntimeEnv(effective)
	if runtime[0].Value != nil || runtime[0].ValueFrom == nil || runtime[1].Value == nil || *runtime[1].Value != ordinary {
		t.Fatalf("runtime=%#v", runtime)
	}
}

func TestResolveRejectsInvalidApplicationOverrides(t *testing.T) {
	value := "one"
	_, problems := Resolve(Document{}, Document{}, []domain.WorkloadEnv{{Name: "DUPLICATE", Value: &value}, {Name: "DUPLICATE", Value: &value}})
	if len(problems) == 0 || problems[0].Code != "Duplicate" {
		t.Fatalf("problems=%#v", problems)
	}
}

func TestParseVariableSetBounds(t *testing.T) {
	tooLarge := "apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  VALUE: \"" + strings.Repeat("x", MaxValueBytes+1) + "\"\n"
	_, diagnostics := ParseAndValidate([]byte(tooLarge))
	if len(diagnostics) != 1 || diagnostics[0].Code != "InvalidValue" {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	_, diagnostics = ParseAndValidate([]byte(strings.Repeat("x", MaxDocumentBytes+1)))
	if len(diagnostics) != 1 || diagnostics[0].Code != "InvalidDocument" {
		t.Fatalf("document bound diagnostics=%#v", diagnostics)
	}
}
