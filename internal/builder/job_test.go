package builder

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func validJobPlanRequest() JobPlanRequest {
	resources := ContainerResources{
		CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "256Mi",
		CPULimit: "1000m", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi",
	}
	return JobPlanRequest{
		Build:                         validBuildRequest(),
		Namespace:                     "kuberploy-build-dind",
		PodServiceAccount:             "kuberploy-build-pod",
		RequestConfigMap:              "build-request-abc",
		SourceCredentialSecret:        "source-credentials-abc",
		RegistryPushCredentialSecret:  "registry-push-abc",
		RegistryCacheCredentialSecret: "registry-cache-abc",
		BuildSecret:                   "build-secrets-abc",
		SSHSecret:                     "ssh-secrets-abc",
		CheckoutImage:                 "registry.example.test/system/builder-agent:0.1.0-rc.90",
		AgentImage:                    "registry.example.test/system/builder-agent:0.1.0-rc.90",
		NodeSelector:                  map[string]string{"kuberploy.io/node-class": "dind-builder", "kubernetes.io/arch": "amd64"},
		Toleration:                    TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"},
		CheckoutResources:             resources,
		DinDResources:                 resources,
		AgentResources:                resources,
		WorkspaceSizeLimit:            "10Gi",
		SocketSizeLimit:               "16Mi",
		ResultSizeLimit:               "1Mi",
		DockerDataSizeLimit:           "20Gi",
		ActiveDeadlineSeconds:         1800,
		TTLSecondsAfterFinished:       3600,
		Egress:                        []EgressEndpoint{{CIDR: "192.0.2.10/32", Ports: []int{443}}, {CIDR: "198.51.100.10/32", Ports: []int{5000}}},
	}
}

func TestJobPlanIsDeterministicAndHasNoForbiddenPodFields(t *testing.T) {
	request := validJobPlanRequest()
	one, err := PlanJob(request)
	if err != nil {
		t.Fatal(err)
	}
	two, err := PlanJob(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatal("same operation/generation did not produce the same plan")
	}
	encoded, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"hostPath", "/var/run/docker.sock", `"hostNetwork":true`, `"hostPID":true`, `"hostIPC":true`, `"automountServiceAccountToken":true`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("forbidden Pod capability rendered: %q", forbidden)
		}
	}
	if strings.Contains(text, "0.0.0.0/0") || strings.Contains(text, "::/0") {
		t.Fatal("world-open egress rendered")
	}
	policySpec := one.NetworkPolicy["spec"].(map[string]any)
	if _, present := policySpec["ingress"]; present {
		t.Fatal("empty ingress must be omitted because the Kubernetes API prunes it")
	}
	if !reflect.DeepEqual(policySpec["policyTypes"], []any{"Ingress", "Egress"}) {
		t.Fatal("omitted ingress did not retain the ingress deny policy type")
	}
	changed := validJobPlanRequest()
	changed.Build.Generation++
	changed.Build.Destination.Reference = strings.Replace(changed.Build.Destination.Reference, "-g2-", "-g3-", 1)
	changed.Build.Cache.Imports[0] = strings.Replace(changed.Build.Cache.Imports[0], "generation-1", "generation-2", 1)
	changed.Build.Cache.CandidateExport = strings.Replace(changed.Build.Cache.CandidateExport, "-g2", "-g3", 1)
	changedPlan, err := PlanJob(changed)
	if err != nil {
		t.Fatal(err)
	}
	oneName := one.Job["metadata"].(map[string]any)["name"]
	changedName := changedPlan.Job["metadata"].(map[string]any)["name"]
	if oneName == changedName {
		t.Fatal("generation did not change deterministic Job identity")
	}
	if !CanAdoptJob(one.Job, request) || CanAdoptJob(one.Job, changed) {
		t.Fatal("Job adoption did not require exact operation/generation/spec identity")
	}
	if !CanAdoptNetworkPolicy(one.NetworkPolicy, request) || CanAdoptNetworkPolicy(one.NetworkPolicy, changed) {
		t.Fatal("NetworkPolicy adoption did not require exact operation/generation/spec identity")
	}
}

func TestAdoptionRejectsPrivilegedShapeAndEgressMutations(t *testing.T) {
	request := validJobPlanRequest()
	tests := []struct {
		name   string
		mutate func(JobPlan)
	}{
		{name: "alternate checkout image", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["initContainers"].([]any)[0].(map[string]any)["image"] = "attacker.test/image:9.9.9"
		}},
		{name: "missing checkout", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["initContainers"] = pod["initContainers"].([]any)[1:]
		}},
		{name: "extra mount", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			agent := pod["containers"].([]any)[0].(map[string]any)
			agent["volumeMounts"] = append(agent["volumeMounts"].([]any), volumeMount("source-credentials", SourceCredentialRoot, true))
		}},
		{name: "extra container", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["containers"] = append(pod["containers"].([]any), map[string]any{"name": "attacker"})
		}},
		{name: "parallel privileged pods", mutate: func(p JobPlan) {
			p.Job["spec"].(map[string]any)["parallelism"] = int64(2)
		}},
		{name: "agent command override", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["containers"].([]any)[0].(map[string]any)["command"] = []any{"/attacker"}
		}},
		{name: "agent lifecycle command", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["containers"].([]any)[0].(map[string]any)["lifecycle"] = map[string]any{"postStart": map[string]any{"exec": map[string]any{"command": []any{"/attacker"}}}}
		}},
		{name: "dind liveness command", mutate: func(p JobPlan) {
			pod := p.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["initContainers"].([]any)[1].(map[string]any)["livenessProbe"] = map[string]any{"exec": map[string]any{"command": []any{"/attacker"}}}
		}},
		{name: "allow all egress", mutate: func(p JobPlan) {
			p.NetworkPolicy["spec"].(map[string]any)["egress"] = []any{
				map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanJob(request)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(plan)
			if CanAdoptJob(plan.Job, request) && CanAdoptNetworkPolicy(plan.NetworkPolicy, request) {
				t.Fatal("mutated privileged runtime or egress policy was accepted")
			}
		})
	}
}

func TestJobPlanPrivilegedOnlyDinDAndAgentWorkspaceReadOnly(t *testing.T) {
	plan, err := PlanJob(validJobPlanRequest())
	if err != nil {
		t.Fatal(err)
	}
	pod := plan.Job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	jobSpec := plan.Job["spec"].(map[string]any)
	if jobSpec["parallelism"] != int64(1) || jobSpec["completions"] != int64(1) || jobSpec["suspend"] != false || jobSpec["completionMode"] != "NonIndexed" || jobSpec["manualSelector"] != false || jobSpec["podReplacementPolicy"] != "Failed" || pod["restartPolicy"] != "Never" {
		t.Fatal("Job can multiply, overlap, index, suspend, or restart privileged builder Pods")
	}
	privileged := []string{}
	for _, raw := range append(pod["initContainers"].([]any), pod["containers"].([]any)...) {
		container := raw.(map[string]any)
		security := container["securityContext"].(map[string]any)
		if value, _ := security["privileged"].(bool); value {
			privileged = append(privileged, container["name"].(string))
		}
	}
	if !reflect.DeepEqual(privileged, []string{"dind"}) {
		t.Fatalf("unexpected privileged containers: %v", privileged)
	}
	init := pod["initContainers"].([]any)
	dind := init[1].(map[string]any)
	if dind["restartPolicy"] != "Always" || dind["image"] != DefaultDinDImage {
		t.Fatal("DinD is not the pinned restartable init-sidecar")
	}
	encodedDinD, _ := json.Marshal(dind)
	if !strings.Contains(string(encodedDinD), `"command":["/usr/local/bin/docker-init","--","/usr/local/bin/dockerd"]`) || strings.Contains(string(encodedDinD), "tcp://") {
		t.Fatal("DinD did not bypass the image entrypoint's implicit TCP listener")
	}
	if !strings.Contains(string(encodedDinD), `"--feature=cdi=false"`) {
		t.Fatal("DinD did not disable ambient CDI device discovery")
	}
	agent := pod["containers"].([]any)[0].(map[string]any)
	if agent["terminationMessagePath"] != DefaultBuildResult || agent["terminationMessagePolicy"] != "File" {
		t.Fatal("agent result is not bound to the exact Kubernetes termination message file")
	}
	mounts := agent["volumeMounts"].([]any)
	assertMount(t, mounts, "workspace", true)
	assertMount(t, mounts, "registry-push-credentials", true)
	assertMount(t, mounts, "registry-cache-credentials", true)
	assertNoMount(t, mounts, "source-credentials")
	checkout := init[0].(map[string]any)
	checkoutMounts := checkout["volumeMounts"].([]any)
	assertMount(t, checkoutMounts, "source-credentials", true)
	assertNoMount(t, checkoutMounts, "registry-push-credentials")
	assertNoMount(t, checkoutMounts, "registry-cache-credentials")
	assertMount(t, dind["volumeMounts"].([]any), "docker-data", false)
	podSecurity := pod["securityContext"].(map[string]any)
	if podSecurity["runAsUser"] != int64(65532) || podSecurity["fsGroup"] != int64(65532) || podSecurity["runAsGroup"] != int64(65532) || podSecurity["fsGroupChangePolicy"] != "OnRootMismatch" {
		t.Fatal("Pod does not make projected credentials group-readable to UID/GID 65532")
	}
	for _, raw := range pod["volumes"].([]any) {
		volume := raw.(map[string]any)
		for _, kind := range []string{"secret", "configMap"} {
			if projection, ok := volume[kind].(map[string]any); ok && projection["defaultMode"] != 0440 {
				t.Fatalf("%s projection %s is not group-readable 0440", kind, volume["name"])
			}
		}
	}
	assertRegistryCredentialProjection(t, pod["volumes"].([]any), "registry-push-credentials", "registry-push-abc")
	assertRegistryCredentialProjection(t, pod["volumes"].([]any), "registry-cache-credentials", "registry-cache-abc")
	for _, container := range []map[string]any{checkout, agent} {
		security := container["securityContext"].(map[string]any)
		if security["allowPrivilegeEscalation"] != false || security["runAsNonRoot"] != true {
			t.Fatalf("container %s is not restricted", container["name"])
		}
	}
}

func assertRegistryCredentialProjection(t *testing.T, volumes []any, name, secretName string) {
	t.Helper()
	for _, raw := range volumes {
		volume := raw.(map[string]any)
		if volume["name"] != name {
			continue
		}
		projection := volume["secret"].(map[string]any)
		want := []any{
			map[string]any{"key": "username", "path": "username"},
			map[string]any{"key": "password", "path": "password"},
		}
		if projection["secretName"] != secretName || projection["defaultMode"] != 0440 || !reflect.DeepEqual(projection["items"], want) {
			t.Fatalf("credential projection %s is not exact: %#v", name, projection)
		}
		return
	}
	t.Fatalf("credential projection %s absent", name)
}

func TestJobPlanRequiresIsolationResourcesAndBoundedEgress(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*JobPlanRequest)
	}{
		{name: "no taint", mutate: func(r *JobPlanRequest) { r.Toleration = TaintToleration{} }},
		{name: "general node", mutate: func(r *JobPlanRequest) { r.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"} }},
		{name: "floating agent", mutate: func(r *JobPlanRequest) { r.AgentImage = "registry.example.test/agent:latest" }},
		{name: "missing ephemeral limit", mutate: func(r *JobPlanRequest) { r.AgentResources.EphemeralStorageLimit = "" }},
		{name: "world egress", mutate: func(r *JobPlanRequest) { r.Egress = []EgressEndpoint{{CIDR: "0.0.0.0/0", Ports: []int{443}}} }},
		{name: "secret mismatch", mutate: func(r *JobPlanRequest) { r.BuildSecret = "" }},
		{name: "shared push and cache authority", mutate: func(r *JobPlanRequest) { r.RegistryCacheCredentialSecret = r.RegistryPushCredentialSecret }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validJobPlanRequest()
			test.mutate(&request)
			if _, err := PlanJob(request); err == nil {
				t.Fatal("unsafe Job plan was accepted")
			}
		})
	}
}

func assertMount(t *testing.T, mounts []any, name string, readOnly bool) {
	t.Helper()
	for _, raw := range mounts {
		mount := raw.(map[string]any)
		if mount["name"] == name {
			actual, _ := mount["readOnly"].(bool)
			if actual != readOnly {
				t.Fatalf("mount %s readOnly=%v", name, mount["readOnly"])
			}
			return
		}
	}
	t.Fatalf("mount %s absent", name)
}

func assertNoMount(t *testing.T, mounts []any, name string) {
	t.Helper()
	for _, raw := range mounts {
		if raw.(map[string]any)["name"] == name {
			t.Fatalf("credential mount %s crossed container trust boundary", name)
		}
	}
}
