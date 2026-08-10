package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
)

func TestEnvironmentDestinationFieldsAreLockedAndDerived(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	r := f.request("POST", "/v1/projects", "destination-project", map[string]string{"name": "Payments", "slug": "payments"})
	project := decode[domain.Project](t, r)

	r = f.request("POST", "/v1/environments", "locked-destination", map[string]string{
		"projectId":   project.ID,
		"name":        "Development",
		"namespace":   "attacker-chosen",
		"argoProject": "attacker-chosen",
	})
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusUnprocessableEntity || len(problem.Errors) != 2 || problem.Errors[0].Code != "LockedField" || problem.Errors[1].Code != "LockedField" {
		t.Fatalf("locked destination response=%d %#v", r.StatusCode, problem)
	}

	r = f.request("POST", "/v1/environments", "derived-destination", map[string]string{
		"projectId": project.ID,
		"name":      "Development",
		"slug":      "dev",
	})
	environment := decode[domain.Environment](t, r)
	wantNamespace, wantArgo := domain.DeriveEnvironmentDestination(project, "dev")
	if r.StatusCode != http.StatusCreated || environment.Namespace != wantNamespace || environment.ArgoProject != wantArgo || environment.ProtectionPolicy != domain.EnvironmentProtected {
		t.Fatalf("derived environment=%d %#v", r.StatusCode, environment)
	}
	r = f.request("POST", "/v1/environments", "development-policy", map[string]string{
		"projectId": project.ID, "name": "Local", "slug": "local", "protectionPolicy": "development",
	})
	development := decode[domain.Environment](t, r)
	if r.StatusCode != http.StatusCreated || development.ProtectionPolicy != domain.EnvironmentDevelopment {
		t.Fatalf("development policy=%d %#v", r.StatusCode, development)
	}
	r = f.request("POST", "/v1/environments", "invalid-policy", map[string]string{
		"projectId": project.ID, "name": "Invalid", "slug": "invalid", "protectionPolicy": "caller-selected-mode",
	})
	problem = decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusUnprocessableEntity || len(problem.Errors) != 1 || problem.Errors[0].Code != "InvalidEnvironmentProtectionPolicy" {
		t.Fatalf("invalid policy response=%d %#v", r.StatusCode, problem)
	}
}

func TestLongEnvironmentDestinationIsStableDNSLabel(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	projectSlug := strings.Repeat("p", 63)
	environmentSlug := strings.Repeat("e", 63)
	r := f.request("POST", "/v1/projects", "long-project", map[string]string{"name": "Long project", "slug": projectSlug})
	project := decode[domain.Project](t, r)
	r = f.request("POST", "/v1/environments", "long-environment", map[string]string{"projectId": project.ID, "name": "Long environment", "slug": environmentSlug})
	environment := decode[domain.Environment](t, r)
	wantNamespace, _ := domain.DeriveEnvironmentDestination(project, environmentSlug)
	if r.StatusCode != http.StatusCreated || environment.Namespace != wantNamespace || len(environment.Namespace) != 63 || strings.HasSuffix(environment.Namespace, "-") {
		t.Fatalf("long destination=%d %#v", r.StatusCode, environment)
	}
}

func TestCanonicalWorkloadRuntimeRoundTripsAndRejectsUnsafeValues(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	r := f.request("POST", "/v1/projects", "workload-project", map[string]string{"name": "Workloads"})
	project := decode[domain.Project](t, r)
	r = f.request("POST", "/v1/environments", "workload-environment", map[string]string{"projectId": project.ID, "name": "Development"})
	environment := decode[domain.Environment](t, r)
	r = f.request("POST", "/v1/applications", "workload-application", map[string]string{"projectId": project.ID, "name": "API"})
	application := decode[domain.Application](t, r)

	image := "registry.example.test/api@sha256:" + strings.Repeat("a", 64)
	runtime := map[string]any{
		"replicas": 2,
		"ports":    []any{map[string]any{"name": "http", "containerPort": 8080, "servicePort": 80}},
		"env": []any{
			map[string]any{"name": "LOG_LEVEL", "value": "info"},
			map[string]any{"name": "FEATURE_FLAG", "value": "enabled"},
		},
		"resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "100Mi"}, "limits": map[string]string{"cpu": "500m", "memory": "512Mi"}},
	}
	r = f.request("POST", "/v1/deployments", "canonical-runtime", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "runtime": runtime})
	op := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("create workload=%d %#v", r.StatusCode, op)
	}
	r = f.request("GET", "/v1/deployments/"+op.TargetID, "", nil)
	deployment := decode[domain.Deployment](t, r)
	if deployment.Runtime.Resources.Requests.CPU != "50m" || deployment.Runtime.Resources.Requests.Memory != "100Mi" || deployment.Runtime.Ports[0].Protocol != "TCP" || deployment.Runtime.Env[1].Value == nil || *deployment.Runtime.Env[1].Value != "enabled" {
		t.Fatalf("runtime did not round-trip: %#v", deployment.Runtime)
	}
	callerScheduling := map[string]any{
		"replicas": 1,
		"ports":    []any{map[string]any{"name": "http", "containerPort": 8080}},
		"resources": map[string]any{
			"requests": map[string]string{"cpu": "50m", "memory": "100Mi"},
		},
		"nodeSelector": map[string]string{"kubernetes.io/arch": "amd64"},
	}
	r = f.request("POST", "/v1/deployments", "caller-scheduling", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "runtime": callerScheduling})
	schedulingProblem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusUnprocessableEntity || schedulingProblem.Code != "SchedulingProfileInvalid" {
		t.Fatalf("caller scheduling material response=%d %#v", r.StatusCode, schedulingProblem)
	}
	cpuLimitOnly := map[string]any{
		"replicas": 1,
		"ports":    []any{map[string]any{"name": "web", "containerPort": 8080}},
		"resources": map[string]any{
			"requests": map[string]string{"cpu": "50m", "memory": "100Mi"},
			"limits":   map[string]string{"cpu": "500m"},
		},
	}
	r = f.request("POST", "/v1/deployments", "cpu-limit-only", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "runtime": cpuLimitOnly})
	cpuLimitOperation := decode[domain.Operation](t, r)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("CPU-only limit was rejected: %d %#v", r.StatusCode, cpuLimitOperation)
	}
	r = f.request("GET", "/v1/deployments/"+cpuLimitOperation.TargetID, "", nil)
	deployment = decode[domain.Deployment](t, r)
	if deployment.Runtime.Resources.Limits == nil || deployment.Runtime.Resources.Limits.CPU != "500m" || deployment.Runtime.Resources.Limits.Memory != "" {
		t.Fatalf("CPU-only limit did not round-trip: %#v", deployment.Runtime.Resources.Limits)
	}

	invalidRuntime := map[string]any{
		"replicas":     1,
		"ports":        []any{map[string]any{"name": "http", "containerPort": 8080}},
		"resources":    map[string]any{"requests": map[string]string{"cpu": "500m", "memory": "512Mi"}, "limits": map[string]string{"cpu": "50m", "memory": "100Mi"}},
		"tolerations":  []any{map[string]any{"key": "", "operator": "Exists", "effect": "NoSchedule"}},
		"nodeSelector": map[string]string{"kuberploy.io/node-class": "builder"},
	}
	r = f.request("POST", "/v1/deployments", "invalid-runtime", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": image, "runtime": invalidRuntime})
	problem := decode[httpapi.Problem](t, r)
	if r.StatusCode != http.StatusUnprocessableEntity || problem.Code != "SchedulingProfileInvalid" || !hasProblemPointer(problem, "/runtime/schedulingProfile") {
		t.Fatalf("invalid runtime response=%d %#v", r.StatusCode, problem)
	}
}

func hasProblemPointer(problem httpapi.Problem, pointer string) bool {
	for _, field := range problem.Errors {
		if field.Pointer == pointer {
			return true
		}
	}
	return false
}
