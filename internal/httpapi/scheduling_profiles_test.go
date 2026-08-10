package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/scheduling"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func newSchedulingAPI(t *testing.T) (*apiFixture, *scheduling.MemoryStore) {
	t.Helper()
	central := memory.New()
	profiles := scheduling.NewMemoryStore()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, SchedulingProfiles: profiles, BootstrapToken: "one-time-secret", Version: "test", HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	return fixture, profiles
}

func TestSchedulingProfilesAreAdminOwnedAndEnvironmentCatalogIsExact(t *testing.T) {
	fixture, _ := newSchedulingAPI(t)
	fixture.bootstrap()
	projectResponse := fixture.request(http.MethodPost, "/v1/projects", "scheduling-project", map[string]any{"name": "Scheduling", "slug": "scheduling"})
	project := decode[struct {
		ID string `json:"id"`
	}](t, projectResponse)
	environmentResponse := fixture.request(http.MethodPost, "/v1/environments", "scheduling-environment", map[string]any{"projectId": project.ID, "name": "Production", "slug": "production"})
	environment := decode[struct {
		ID string `json:"id"`
	}](t, environmentResponse)

	input := map[string]any{
		"name": "on-demand",
		"spec": map[string]any{"description": "Stable capacity", "pod": map[string]any{
			"nodeSelector":          map[string]string{"karpenter.sh/capacity-type": "on-demand"},
			"preferredNodeAffinity": []any{map[string]any{"weight": 75, "requirements": []any{map[string]any{"key": "topology.kubernetes.io/zone", "operator": "In", "values": []string{"zone-a", "zone-b"}}}}},
			"sameApplicationPodAntiAffinity": []any{
				map[string]any{"enforcement": "required", "topologyKey": "kubernetes.io/hostname"},
				map[string]any{"enforcement": "preferred", "topologyKey": "topology.kubernetes.io/zone", "weight": 40},
			},
			"tolerations": []any{map[string]any{"key": "workload.example/class", "operator": "Equal", "value": "application", "effect": "NoSchedule"}},
		}},
		"assignments": []any{
			map[string]any{"scope": "environment", "id": environment.ID},
			map[string]any{"scope": "project", "id": "66666666-6666-4666-8666-666666666666"},
		},
	}
	response := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "create-profile", input)
	created := decode[scheduling.Entry](t, response)
	if response.StatusCode != http.StatusCreated || created.Profile.ID == "" || created.Revision.Revision != 1 || created.Revision.SpecDigest == "" || created.Revision.AssignmentsDigest == "" {
		t.Fatalf("created status=%d entry=%#v", response.StatusCode, created)
	}
	replay := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "create-profile", input)
	if replay.StatusCode != http.StatusCreated || replay.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay status=%d header=%q", replay.StatusCode, replay.Header.Get("Idempotent-Replay"))
	}
	replay.Body.Close()

	assignedResponse := fixture.request(http.MethodGet, "/v1/environments/"+environment.ID+"/scheduling-profiles", "", nil)
	assignedBody, err := io.ReadAll(assignedResponse.Body)
	assignedResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var assigned struct {
		Items []struct {
			ProfileID  string `json:"profileId"`
			SpecDigest string `json:"specDigest"`
		} `json:"items"`
	}
	if err = json.Unmarshal(assignedBody, &assigned); err != nil {
		t.Fatal(err)
	}
	if assignedResponse.StatusCode != http.StatusOK || len(assigned.Items) != 1 || assigned.Items[0].ProfileID != created.Profile.ID || assigned.Items[0].SpecDigest != created.Revision.SpecDigest {
		t.Fatalf("assigned status=%d result=%#v", assignedResponse.StatusCode, assigned)
	}
	for _, forbidden := range []string{`"assignments":`, `"createdBy":`, `"deactivatedBy":`, `"labelSelector":`, "66666666-6666-4666-8666-666666666666"} {
		if strings.Contains(string(assignedBody), forbidden) {
			t.Fatalf("tenant catalog exposed %q: %s", forbidden, assignedBody)
		}
	}
	runtime := map[string]any{
		"replicas":          1,
		"ports":             []any{map[string]any{"name": "http", "containerPort": 8080}},
		"resources":         map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "100Mi"}},
		"schedulingProfile": map[string]any{"profileId": created.Profile.ID, "revision": created.Revision.Revision, "specDigest": created.Revision.SpecDigest, "assignmentsDigest": created.Revision.AssignmentsDigest},
	}
	applicationResponse := fixture.request(http.MethodPost, "/v1/applications", "scheduled-app", map[string]any{"projectId": project.ID, "name": "Scheduled API", "slug": "scheduled-api"})
	application := decode[struct {
		ID string `json:"id"`
	}](t, applicationResponse)
	deployResponse := fixture.request(http.MethodPost, "/v1/deployments", "scheduled-deploy", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": "registry.example.test/api@sha256:" + strings.Repeat("a", 64), "runtime": runtime})
	operation := decode[domain.Operation](t, deployResponse)
	if deployResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("scheduled deploy status=%d operation=%#v", deployResponse.StatusCode, operation)
	}
	getResponse := fixture.request(http.MethodGet, "/v1/deployments/"+operation.TargetID, "", nil)
	deployment := decode[domain.Deployment](t, getResponse)
	if deployment.Runtime.SchedulingProfile == nil || deployment.Runtime.SchedulingProfile.ProfileID != created.Profile.ID || deployment.Runtime.NodeSelector["karpenter.sh/capacity-type"] != "on-demand" || len(deployment.Runtime.Tolerations) != 1 ||
		deployment.Runtime.Affinity == nil || deployment.Runtime.Affinity.NodeAffinity == nil || deployment.Runtime.Affinity.PodAntiAffinity == nil ||
		len(deployment.Runtime.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 || len(deployment.Runtime.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 || len(deployment.Runtime.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("server materialization did not round trip: %#v", deployment.Runtime)
	}
	selector := deployment.Runtime.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector
	if len(selector.MatchLabels) != 1 || selector.MatchLabels["kuberploy.io/application"] != application.ID {
		t.Fatalf("server did not derive the exact same-application selector: %#v", selector)
	}
	runtime["affinity"] = map[string]any{"podAntiAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{"topologyKey": "kubernetes.io/hostname", "labelSelector": map[string]any{"matchLabels": map[string]string{"kuberploy.io/application": "66666666-6666-4666-8666-666666666666"}}}}}}
	tamperedResponse := fixture.request(http.MethodPost, "/v1/deployments", "scheduled-deploy-tampered", map[string]any{"environmentId": environment.ID, "applicationId": application.ID, "image": "registry.example.test/api@sha256:" + strings.Repeat("a", 64), "runtime": runtime})
	tamperedProblem := decode[httpapi.Problem](t, tamperedResponse)
	if tamperedResponse.StatusCode != http.StatusUnprocessableEntity || tamperedProblem.Code != "SchedulingProfileInvalid" {
		t.Fatalf("cross-application caller affinity status=%d problem=%#v", tamperedResponse.StatusCode, tamperedProblem)
	}
}

func TestSchedulingProfileRejectsIdempotencySubstitutionAndBroadToleration(t *testing.T) {
	fixture, _ := newSchedulingAPI(t)
	fixture.bootstrap()
	assignmentID := "44444444-4444-4444-8444-444444444444"
	base := map[string]any{"name": "general", "spec": map[string]any{"pod": map[string]any{"nodeSelector": map[string]string{"kubernetes.io/arch": "amd64"}}}, "assignments": []any{map[string]any{"scope": "environment", "id": assignmentID}}}
	first := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "same-command", base)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status=%d problem=%#v", first.StatusCode, decode[httpapi.Problem](t, first))
	}
	first.Body.Close()
	base["spec"] = map[string]any{"pod": map[string]any{"nodeSelector": map[string]string{"kubernetes.io/arch": "arm64"}}}
	substitution := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "same-command", base)
	problem := decode[httpapi.Problem](t, substitution)
	if substitution.StatusCode != http.StatusConflict || problem.Code != "SchedulingProfileConflict" {
		t.Fatalf("substitution status=%d problem=%#v", substitution.StatusCode, problem)
	}

	broad := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "broad-toleration", map[string]any{"name": "unsafe", "spec": map[string]any{"pod": map[string]any{"tolerations": []any{map[string]any{"key": "", "operator": "Exists", "effect": "NoSchedule"}}}}, "assignments": []any{map[string]any{"scope": "environment", "id": assignmentID}}})
	broadProblem := decode[httpapi.Problem](t, broad)
	if broad.StatusCode != http.StatusUnprocessableEntity || broadProblem.Code != "SchedulingProfileInvalid" {
		t.Fatalf("broad status=%d problem=%#v", broad.StatusCode, broadProblem)
	}
}

func TestSchedulingProfileJSONCannotCarryRawPodSelectorOrApplicationIdentity(t *testing.T) {
	fixture, _ := newSchedulingAPI(t)
	fixture.bootstrap()
	assignmentID := "44444444-4444-4444-8444-444444444444"
	for name, injected := range map[string]map[string]any{
		"label selector":       {"enforcement": "required", "topologyKey": "kubernetes.io/hostname", "labelSelector": map[string]any{"matchLabels": map[string]string{"attacker.example/target": "other"}}},
		"application identity": {"enforcement": "preferred", "topologyKey": "kubernetes.io/hostname", "applicationId": "66666666-6666-4666-8666-666666666666"},
		"namespace selector":   {"enforcement": "required", "topologyKey": "kubernetes.io/hostname", "namespaceSelector": map[string]any{}},
		"mismatch label keys":  {"enforcement": "required", "topologyKey": "kubernetes.io/hostname", "mismatchLabelKeys": []string{"kuberploy.io/application"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := fixture.request(http.MethodPost, "/v1/platform/scheduling-profiles", "raw-selector-"+strings.ReplaceAll(name, " ", "-"), map[string]any{
				"name":        "raw-selector-" + strings.ReplaceAll(name, " ", "-"),
				"spec":        map[string]any{"pod": map[string]any{"sameApplicationPodAntiAffinity": []any{injected}}},
				"assignments": []any{map[string]any{"scope": "environment", "id": assignmentID}},
			})
			problem := decode[httpapi.Problem](t, response)
			if response.StatusCode != http.StatusBadRequest || problem.Code != "InvalidJSON" {
				t.Fatalf("raw selector field status=%d problem=%#v", response.StatusCode, problem)
			}
		})
	}
}
