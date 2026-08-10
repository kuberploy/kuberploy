package variablecompiler

import (
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestResolveMissingParentsAndPrecedenceIncludingSecretOverride(t *testing.T) {
	states := []gitprojection.DependencyState{{Path: "project"}, {Path: "environment"}}
	value := "application"
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	runtime.Env = []domain.WorkloadEnv{{Name: "APP", Value: &value}}
	resolution, err := Resolve(states, nil, runtime)
	if err != nil || len(resolution.Effective) != 1 || *resolution.Effective[0].Value != value {
		t.Fatalf("missing parents did not resolve as empty: %#v err=%v", resolution, err)
	}

	binding := compilerBinding(t)
	paths, _ := gitprojection.DependencyPaths(binding)
	project := dependency(t, binding, paths[0], "PROJECT: project\nSHARED: project\nSECRET_OVERRIDE: ordinary\n", "b")
	environment := dependency(t, binding, paths[1], "ENVIRONMENT: environment\nSHARED: environment\n", "c")
	secret := domain.SecretBindingRef{BindingID: "55555555-5555-4555-8555-555555555555", Name: "credential", Key: "token", Version: 2}
	runtime.Env = []domain.WorkloadEnv{{Name: "SHARED", Value: &value}, {Name: "SECRET_OVERRIDE", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: secret}}}
	states, _ = States(paths, []gitprojection.Document{project, environment})
	resolution, err = Resolve(states, []gitprojection.Document{project, environment}, runtime)
	if err != nil || len(resolution.Effective) != 4 {
		t.Fatalf("resolve: %#v err=%v", resolution, err)
	}
	byName := map[string]domain.WorkloadEnv{}
	for _, env := range resolution.Runtime.Env {
		byName[env.Name] = env
	}
	if *byName["PROJECT"].Value != "project" || *byName["ENVIRONMENT"].Value != "environment" || *byName["SHARED"].Value != "application" || byName["SECRET_OVERRIDE"].ValueFrom == nil {
		t.Fatalf("precedence or secret override failed: %#v", resolution)
	}
}

func compilerBinding(t *testing.T) gitprojection.Binding {
	t.Helper()
	now := time.Now().UTC()
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "owner", Name: "repo"}
	binding, err := gitprojection.NewEnvironmentBinding("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", repository, "refs/heads/main", "credential", now)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func dependency(t *testing.T, binding gitprojection.Binding, path, values, blob string) gitprojection.Document {
	t.Helper()
	raw := []byte("apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  " + strings.ReplaceAll(strings.TrimSuffix(values, "\n"), "\n", "\n  ") + "\n")
	document, err := gitprojection.NewDependencyDocument(binding, 1, path, strings.Repeat("a", 40), strings.Repeat("a", 40), strings.Repeat(blob, 40), raw, map[string]any{"kind": "VariableSet"}, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return document
}
