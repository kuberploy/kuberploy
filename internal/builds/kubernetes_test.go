package builds

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

type fakeBuildResources struct {
	mu              sync.Mutex
	objects         map[string]map[string]any
	pods            []map[string]any
	creates         []string
	gets            []string
	deletes         []string
	nextUID         int
	holdJobDeletion bool
	builderNodes    []map[string]any
}

func newFakeBuildResources() *fakeBuildResources {
	return &fakeBuildResources{objects: make(map[string]map[string]any), nextUID: 1, builderNodes: []map[string]any{readyBuilderNode("builder-1")}}
}

func (f *fakeBuildResources) key(resource kubernetesResource, namespace, name string) string {
	return string(resource) + "/" + namespace + "/" + name
}

func (f *fakeBuildResources) Get(_ context.Context, resource kubernetesResource, namespace, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(resource, namespace, name)
	f.gets = append(f.gets, key)
	object, found := f.objects[key]
	if !found {
		return nil, errKubernetesObjectNotFound
	}
	return cloneMap(object), nil
}

func (f *fakeBuildResources) Create(_ context.Context, resource kubernetesResource, namespace string, object map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(resource, namespace, objectName(object))
	if _, exists := f.objects[key]; exists {
		return nil, errKubernetesObjectConflict
	}
	stored := cloneMap(object)
	metadata := stored["metadata"].(map[string]any)
	metadata["uid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextUID)
	metadata["resourceVersion"] = fmt.Sprint(f.nextUID)
	if resource == resourceJobs {
		uid := metadata["uid"].(string)
		spec := stored["spec"].(map[string]any)
		spec["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": uid}}
		template := spec["template"].(map[string]any)
		labels := template["metadata"].(map[string]any)["labels"].(map[string]any)
		labels["batch.kubernetes.io/controller-uid"] = uid
		labels["batch.kubernetes.io/job-name"] = objectName(stored)
		labels["controller-uid"] = uid
		labels["job-name"] = objectName(stored)
	}
	f.nextUID++
	f.objects[key] = stored
	f.creates = append(f.creates, key)
	return cloneMap(stored), nil
}

func (f *fakeBuildResources) Delete(_ context.Context, resource kubernetesResource, namespace, name string, preconditions deletePreconditions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(resource, namespace, name)
	object, found := f.objects[key]
	if !found {
		return errKubernetesObjectNotFound
	}
	metadata := object["metadata"].(map[string]any)
	if metadata["uid"] != preconditions.UID || metadata["resourceVersion"] != preconditions.ResourceVersion {
		return errors.New("delete precondition mismatch")
	}
	if resource == resourceJobs && f.holdJobDeletion {
		metadata["deletionTimestamp"] = "2026-08-09T00:00:00Z"
		metadata["finalizers"] = []any{"foregroundDeletion"}
		f.deletes = append(f.deletes, key+"/"+preconditions.PropagationPolicy)
		return nil
	}
	delete(f.objects, key)
	f.deletes = append(f.deletes, key+"/"+preconditions.PropagationPolicy)
	return nil
}

func (f *fakeBuildResources) ListBuildPods(_ context.Context, namespace, operation, generation string, limit int64) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]map[string]any, 0)
	for _, pod := range f.pods {
		labels := metadataLabels(pod)
		if objectNamespace(pod) == namespace && labels["kuberploy.io/build-operation"] == operation && labels["kuberploy.io/build-generation"] == generation {
			result = append(result, cloneMap(pod))
		}
	}
	if len(result) > int(limit) {
		return nil, ErrInfrastructure
	}
	return result, nil
}

func (f *fakeBuildResources) ListBuilderNodes(_ context.Context, limit int64) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit != 100 || len(f.builderNodes) > int(limit) {
		return nil, ErrInfrastructure
	}
	result := make([]map[string]any, 0, len(f.builderNodes))
	for _, node := range f.builderNodes {
		result = append(result, cloneMap(node))
	}
	return result, nil
}

func readyBuilderNode(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": name, "labels": map[string]any{"kuberploy.io/node-class": "dind-builder"}},
		"spec":     map[string]any{"taints": []any{map[string]any{"key": "kuberploy.io/dind-builder", "value": "true", "effect": "NoSchedule"}}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
}

func kubernetesWorkloadFixture(t *testing.T) (BuildAttempt, BuildWorkload) {
	t.Helper()
	store, _ := seedMemory(t, RegistryManaged)
	clock := testNow
	attempt := createAttempt(t, store, RegistryManaged, &clock)
	attempt.State = AttemptPreparing
	attempt.ExecutionAttempts = 1
	plan, err := builder.PlanJob(attempt.PlanRequest)
	if err != nil {
		t.Fatal(err)
	}
	return attempt, BuildWorkload{
		Attempt: attempt, Plan: plan, CheckoutRequest: attempt.CheckoutRequest, InputDigest: attempt.InputDigest,
		SourceUsername: "x-access-token",
	}
}

func newTestKubernetesAdapter(t *testing.T, resources *fakeBuildResources) *KubernetesAdapter {
	t.Helper()
	adapter, err := newKubernetesAdapter(resources, "kuberploy-build-dind")
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func sourceTokenOne() string { return "ghs_" + strings.Repeat("A", 48) }
func sourceTokenTwo() string { return "ghs_" + strings.Repeat("B", 48) }

func TestKubernetesAdapterCreatesExactObjectsAndAdoptsWithoutCredentialLeakage(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	observation, err := adapter.ensure(context.Background(), workload, sourceTokenOne())
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != WorkloadPending || observation.InputDigest != attempt.InputDigest || !builder.CanAdoptJob(observation.Job, attempt.PlanRequest) || !builder.CanAdoptNetworkPolicy(observation.NetworkPolicy, attempt.PlanRequest) {
		t.Fatalf("observation=%#v", observation)
	}
	wantCreates := []string{
		"configmaps/kuberploy-build-dind/" + attempt.PlanRequest.RequestConfigMap,
		"networkpolicies/kuberploy-build-dind/" + attempt.JobName,
		"secrets/kuberploy-build-dind/" + attempt.PlanRequest.SourceCredentialSecret,
		"jobs/kuberploy-build-dind/" + attempt.JobName,
	}
	if !reflect.DeepEqual(resources.creates, wantCreates) {
		t.Fatalf("create order=%v", resources.creates)
	}
	secret := resources.objects[resources.key(resourceSecrets, attempt.JobNamespace, attempt.PlanRequest.SourceCredentialSecret)]
	data := secret["data"].(map[string]any)
	decoded, decodeErr := base64.StdEncoding.DecodeString(data["token"].(string))
	if decodeErr != nil || string(decoded) != sourceTokenOne() {
		t.Fatal("source token was not materialized exactly")
	}
	clear(decoded)
	for _, resource := range []kubernetesResource{resourceConfigMaps, resourceNetworkPolicies, resourceJobs} {
		for key, object := range resources.objects {
			if !strings.HasPrefix(key, string(resource)+"/") {
				continue
			}
			encoded, _ := json.Marshal(object)
			if strings.Contains(string(encoded), sourceTokenOne()) || strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte(sourceTokenOne()))) {
				t.Fatalf("credential leaked into %s", key)
			}
		}
	}
	for _, get := range resources.gets {
		if get == "secrets/kuberploy-build-dind/registry-credentials" {
			t.Fatal("adapter read an existing registry credential Secret")
		}
	}

	creates := len(resources.creates)
	if _, err = adapter.ensure(context.Background(), workload, sourceTokenTwo()); err != nil {
		t.Fatal(err)
	}
	if len(resources.creates) != creates {
		t.Fatal("adoption created a second workload")
	}
}

func TestKubernetesAdapterAdoptsOnlyAllowlistedAPIServerDefaults(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
	job := resources.objects[jobKey]
	metadata := job["metadata"].(map[string]any)
	metadata["annotations"].(map[string]any)["batch.kubernetes.io/job-tracking"] = ""
	uid := metadata["uid"].(string)
	spec := job["spec"].(map[string]any)
	spec["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": uid}}
	template := spec["template"].(map[string]any)
	templateLabels := template["metadata"].(map[string]any)["labels"].(map[string]any)
	templateLabels["batch.kubernetes.io/controller-uid"] = uid
	templateLabels["batch.kubernetes.io/job-name"] = attempt.JobName
	podSpec := template["spec"].(map[string]any)
	podSpec["dnsPolicy"] = "ClusterFirst"
	podSpec["schedulerName"] = "default-scheduler"
	podSpec["terminationGracePeriodSeconds"] = int64(30)
	podSpec["serviceAccount"] = attempt.PlanRequest.PodServiceAccount
	for _, field := range []string{"initContainers", "containers"} {
		for _, raw := range podSpec[field].([]any) {
			container := raw.(map[string]any)
			if container["name"] != "agent" {
				container["terminationMessagePath"] = "/dev/termination-log"
				container["terminationMessagePolicy"] = "File"
			}
			if probe, ok := container["startupProbe"].(map[string]any); ok {
				probe["successThreshold"] = int64(1)
			}
		}
	}
	checkout := podSpec["initContainers"].([]any)[0].(map[string]any)
	checkout["resources"].(map[string]any)["limits"].(map[string]any)["cpu"] = "1000m"
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenTwo()); err != nil {
		t.Fatalf("safe Kubernetes defaults were not adopted: %v", err)
	}
	podSpec["shareProcessNamespace"] = true
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenTwo()); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("unexpected API field was adopted: %v", err)
	}
}

func TestKubernetesAdapterFailsClosedOnEveryOwnedMutation(t *testing.T) {
	mutations := []struct {
		name     string
		resource kubernetesResource
		mutate   func(map[string]any)
	}{
		{name: "job privilege", resource: resourceJobs, mutate: func(object map[string]any) {
			pod := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
		}},
		{name: "termination result path", resource: resourceJobs, mutate: func(object map[string]any) {
			pod := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			pod["containers"].([]any)[0].(map[string]any)["terminationMessagePath"] = "/tmp/result"
		}},
		{name: "cache credential substituted with push authority", resource: resourceJobs, mutate: func(object map[string]any) {
			pod := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			for _, raw := range pod["volumes"].([]any) {
				volume := raw.(map[string]any)
				if volume["name"] == "registry-cache-credentials" {
					volume["secret"].(map[string]any)["secretName"] = "registry-push"
					return
				}
			}
			t.Fatal("cache credential volume not found")
		}},
		{name: "push credential projects extra key", resource: resourceJobs, mutate: func(object map[string]any) {
			pod := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			for _, raw := range pod["volumes"].([]any) {
				volume := raw.(map[string]any)
				if volume["name"] == "registry-push-credentials" {
					secret := volume["secret"].(map[string]any)
					secret["items"] = append(secret["items"].([]any), map[string]any{"key": "extra", "path": "extra"})
					return
				}
			}
			t.Fatal("push credential volume not found")
		}},
		{name: "request bytes", resource: resourceConfigMaps, mutate: func(object map[string]any) {
			object["data"].(map[string]any)["checkout.json"] = `{"commit":"attacker"}`
		}},
		{name: "request finalizer", resource: resourceConfigMaps, mutate: func(object map[string]any) {
			object["metadata"].(map[string]any)["finalizers"] = []any{"attacker.example/finalizer"}
		}},
		{name: "egress", resource: resourceNetworkPolicies, mutate: func(object map[string]any) {
			object["spec"].(map[string]any)["egress"] = []any{map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}}}
		}},
		{name: "input digest", resource: resourceJobs, mutate: func(object map[string]any) {
			object["metadata"].(map[string]any)["annotations"].(map[string]any)[buildInputDigestAnnotation] = "sha256:" + strings.Repeat("0", 64)
		}},
		{name: "source token shape", resource: resourceSecrets, mutate: func(object map[string]any) {
			object["data"].(map[string]any)["extra"] = "eA=="
		}},
		{name: "source token too short", resource: resourceSecrets, mutate: func(object map[string]any) {
			object["data"].(map[string]any)["token"] = base64.StdEncoding.EncodeToString([]byte("short"))
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			attempt, workload := kubernetesWorkloadFixture(t)
			resources := newFakeBuildResources()
			adapter := newTestKubernetesAdapter(t, resources)
			if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
				t.Fatal(err)
			}
			name := attempt.JobName
			if mutation.resource == resourceConfigMaps {
				name = attempt.PlanRequest.RequestConfigMap
			} else if mutation.resource == resourceSecrets {
				name = attempt.PlanRequest.SourceCredentialSecret
			}
			key := resources.key(mutation.resource, attempt.JobNamespace, name)
			mutation.mutate(resources.objects[key])
			deletes := len(resources.deletes)
			if _, err := adapter.ensure(context.Background(), workload, sourceTokenTwo()); !errors.Is(err, ErrInfrastructure) {
				t.Fatalf("mutation accepted: %v", err)
			}
			if len(resources.deletes) != deletes {
				t.Fatal("mismatched owned object was deleted")
			}
		})
	}
}

func TestCreatedSourceSecretMustContainExactMintedCredential(t *testing.T) {
	attempt, _ := kubernetesWorkloadFixture(t)
	desired, err := desiredSourceSecret(attempt.PlanRequest, attempt.InputDigest, []byte("x-access-token"), []byte(sourceTokenOne()))
	if err != nil {
		t.Fatal(err)
	}
	live := cloneMap(desired)
	live["data"].(map[string]any)["token"] = base64.StdEncoding.EncodeToString([]byte(sourceTokenTwo()))
	if err = validateCreatedSourceSecret(live, desired); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("mutated admission response was accepted: %v", err)
	}
}

func TestKubernetesAdapterDeletesSourceSecretAfterCheckoutAndOnCancel(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Running", true, nil)}
	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadRunning {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	secretKey := resources.key(resourceSecrets, attempt.JobNamespace, attempt.PlanRequest.SourceCredentialSecret)
	if _, found := resources.objects[secretKey]; found {
		t.Fatal("source Secret survived completed checkout")
	}

	if err = adapter.Cancel(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []kubernetesResource{resourceJobs, resourceConfigMaps, resourceNetworkPolicies, resourceSecrets} {
		for key := range resources.objects {
			if strings.HasPrefix(key, string(resource)+"/") {
				t.Fatalf("cancel left %s", key)
			}
		}
	}
	if err = adapter.Cancel(context.Background(), attempt); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
}

func TestKubernetesAdapterRecreatesMissingSourceSecretBeforeCheckout(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	secretKey := resources.key(resourceSecrets, attempt.JobNamespace, attempt.PlanRequest.SourceCredentialSecret)
	delete(resources.objects, secretKey)
	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending {
		t.Fatalf("recreated observation=%#v err=%v", observation, err)
	}
	data := resources.objects[secretKey]["data"].(map[string]any)
	decoded, decodeErr := base64.StdEncoding.DecodeString(data["token"].(string))
	defer clear(decoded)
	if decodeErr != nil || string(decoded) != sourceTokenTwo() {
		t.Fatal("missing pre-checkout source Secret was not recreated with the fresh scoped token")
	}
}

func TestKubernetesAdapterRejectsSpoofedBuildPods(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"extra-label": func(pod map[string]any) {
			pod["metadata"].(map[string]any)["labels"].(map[string]any)["attacker.example/spoof"] = "true"
		},
		"extra-owner": func(pod map[string]any) {
			metadata := pod["metadata"].(map[string]any)
			metadata["ownerReferences"] = append(metadata["ownerReferences"].([]any), map[string]any{"apiVersion": "v1", "kind": "Secret", "name": "spoof", "uid": "spoof"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			attempt, workload := kubernetesWorkloadFixture(t)
			resources := newFakeBuildResources()
			adapter := newTestKubernetesAdapter(t, resources)
			if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
				t.Fatal(err)
			}
			pod := buildPodFixture(resources, attempt, "Running", true, nil)
			mutate(pod)
			resources.pods = []map[string]any{pod}
			if _, err := adapter.ensure(context.Background(), workload, sourceTokenTwo()); !errors.Is(err, ErrInfrastructure) {
				t.Fatalf("spoofed Pod was accepted: %v", err)
			}
		})
	}
}

func TestKubernetesAdapterRetriesFailedJobWithSameImmutablePlan(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
	resources.objects[jobKey]["status"] = map[string]any{"failed": int64(1), "conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}}}
	resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Failed", true, nil)}
	workload.Attempt.State = AttemptRunning
	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadFailed || observation.FailureCode != "builder-backoff-exhausted" {
		t.Fatalf("failed observation=%#v err=%v", observation, err)
	}
	oldUID := resources.objects[jobKey]["metadata"].(map[string]any)["uid"]
	workload.Attempt.State = AttemptPreparing
	workload.Attempt.ExecutionAttempts = 2
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending {
		t.Fatalf("retry observation=%#v err=%v", observation, err)
	}
	if resources.objects[jobKey] != nil {
		t.Fatal("replacement Job was created in the same reconciliation as foreground deletion")
	}
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending || resources.objects[jobKey] != nil {
		t.Fatalf("stale controlled Pod did not fence replacement observation=%#v err=%v", observation, err)
	}
	resources.pods = nil
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending {
		t.Fatalf("clean retry observation=%#v err=%v", observation, err)
	}
	newUID := resources.objects[jobKey]["metadata"].(map[string]any)["uid"]
	if oldUID == newUID || !builder.CanAdoptJob(observation.Job, attempt.PlanRequest) {
		t.Fatal("retry did not create a clean Job with the immutable plan")
	}
}

func TestKubernetesAdapterRemovesTerminalFailureAuxiliaries(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
	resources.objects[jobKey]["status"] = map[string]any{
		"failed":     int64(1),
		"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}},
	}
	resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Failed", true, nil)}
	workload.Attempt.State = AttemptRunning

	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadFailed {
		t.Fatalf("failed observation=%#v err=%v", observation, err)
	}
	if _, found := resources.objects[jobKey]; !found {
		t.Fatal("terminal failure deleted the bounded Job/log authority")
	}
	for _, resource := range []kubernetesResource{resourceConfigMaps, resourceNetworkPolicies, resourceSecrets} {
		for key := range resources.objects {
			if strings.HasPrefix(key, string(resource)+"/") {
				t.Fatalf("terminal failure left auxiliary %s", key)
			}
		}
	}
}

func TestKubernetesAdapterWaitsForForegroundRetryCleanupWithoutBurningAttempt(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
	resources.objects[jobKey]["status"] = map[string]any{"failed": int64(1), "conditions": []any{map[string]any{"type": "Failed", "status": "True"}}}
	resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Failed", true, nil)}
	resources.holdJobDeletion = true
	workload.Attempt.State = AttemptPreparing
	workload.Attempt.ExecutionAttempts = 2

	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending || !jobDeletionInProgress(resources.objects[jobKey]) {
		t.Fatalf("foreground deletion observation=%#v err=%v", observation, err)
	}
	workload.Attempt.State = AttemptRunning
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending {
		t.Fatalf("in-progress foreground deletion observation=%#v err=%v", observation, err)
	}

	delete(resources.objects, jobKey)
	resources.pods = nil
	resources.holdJobDeletion = false
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadPending || jobDeletionInProgress(resources.objects[jobKey]) {
		t.Fatalf("replacement observation=%#v err=%v", observation, err)
	}
}

func TestKubernetesAdapterCollectsBoundedResultAndConfirmsAgentCachePromotion(t *testing.T) {
	attempt, workload := kubernetesWorkloadFixture(t)
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
		t.Fatal(err)
	}
	imageDigest := "sha256:" + strings.Repeat("c", 64)
	cacheDigest := "sha256:" + strings.Repeat("d", 64)
	result := builder.BuildResult{
		APIVersion: builder.ProtocolVersion, OperationID: attempt.ID, Generation: attempt.Generation, Status: "Succeeded",
		Image:      builder.Image{Reference: attempt.PlanRequest.Build.Destination.Repository + "@" + imageDigest, Digest: imageDigest, Platforms: attempt.PlanRequest.Build.Platforms},
		Cache:      &builder.Cache{Reference: cacheReference(attempt), Digest: cacheDigest},
		CacheReuse: builder.CacheReuseHit,
		Warnings:   []builder.Warning{}, StartedAt: testNow, CompletedAt: testNow.Add(time.Minute),
	}
	encoded, _ := json.Marshal(result)
	jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
	resources.objects[jobKey]["status"] = map[string]any{"succeeded": int64(1), "conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Succeeded", true, encoded)}
	observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadSucceeded || string(observation.Result) != string(encoded) || !logRefRE.MatchString(observation.LogReference) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	promoted, err := adapter.PromoteCache(context.Background(), attempt, cacheReference(attempt))
	if err != nil || !promoted {
		t.Fatalf("promoted=%v err=%v", promoted, err)
	}
	if _, found := resources.objects[jobKey]; !found {
		t.Fatal("result/log source Job was deleted before TTL")
	}
	for _, resource := range []kubernetesResource{resourceSecrets, resourceConfigMaps, resourceNetworkPolicies} {
		for key := range resources.objects {
			if strings.HasPrefix(key, string(resource)+"/") {
				t.Fatalf("terminal auxiliary survived: %s", key)
			}
		}
	}
	// Re-observation after exact auxiliary cleanup is durable through the
	// terminal Job's input digest and termination result.
	observation, err = adapter.ensure(context.Background(), workload, sourceTokenTwo())
	if err != nil || observation.State != WorkloadSucceeded {
		t.Fatalf("terminal re-observation=%#v err=%v", observation, err)
	}
}

func TestKubernetesAdapterNeverAcceptsTruncatedOrMalformedTerminationSuccess(t *testing.T) {
	for _, message := range [][]byte{[]byte(`{"status":`), []byte(strings.Repeat("x", builder.MaxTerminationResultBytes))} {
		attempt, workload := kubernetesWorkloadFixture(t)
		resources := newFakeBuildResources()
		adapter := newTestKubernetesAdapter(t, resources)
		if _, err := adapter.ensure(context.Background(), workload, sourceTokenOne()); err != nil {
			t.Fatal(err)
		}
		jobKey := resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)
		resources.objects[jobKey]["status"] = map[string]any{"succeeded": int64(1), "conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		resources.pods = []map[string]any{buildPodFixture(resources, attempt, "Succeeded", true, message)}
		observation, err := adapter.ensure(context.Background(), workload, sourceTokenTwo())
		if err != nil || observation.State != WorkloadFailed || observation.FailureCode != "builder-result-invalid" || len(observation.Result) != 0 {
			t.Fatalf("partial success accepted: %#v err=%v", observation, err)
		}
	}
}

func TestKubernetesRESTClientKeepsCredentialBodiesOutOfErrors(t *testing.T) {
	const serviceAccountToken = "service-account.header.signature"
	const sourceToken = "ghs_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+serviceAccountToken {
			t.Error("service account authorization missing")
		}
		body, _ := io.ReadAll(request.Body)
		defer clear(body)
		if !strings.Contains(string(body), base64.StdEncoding.EncodeToString([]byte(sourceToken))) {
			t.Error("source Secret body was not sent to the exact Kubernetes endpoint")
		}
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = response.Write([]byte(`{"message":"` + sourceToken + `"}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(serviceAccountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &inClusterBuildResources{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	attempt, _ := kubernetesWorkloadFixture(t)
	secret, err := desiredSourceSecret(attempt.PlanRequest, attempt.InputDigest, []byte("x-access-token"), []byte(sourceToken))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(context.Background(), resourceSecrets, attempt.JobNamespace, secret)
	if err == nil || strings.Contains(err.Error(), sourceToken) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(sourceToken))) {
		t.Fatalf("credential-bearing Kubernetes response escaped through error: %v", err)
	}
}

func TestKubernetesBuilderCapacityRequiresReadyDedicatedTaintedNode(t *testing.T) {
	resources := newFakeBuildResources()
	adapter := newTestKubernetesAdapter(t, resources)
	if err := adapter.BuilderCapacityReady(context.Background()); err != nil {
		t.Fatalf("ready builder rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing-taint": func(node map[string]any) { node["spec"].(map[string]any)["taints"] = []any{} },
		"cordoned":      func(node map[string]any) { node["spec"].(map[string]any)["unschedulable"] = true },
		"not-ready": func(node map[string]any) {
			node["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
		},
		"extra-hard-taint": func(node map[string]any) {
			node["spec"].(map[string]any)["taints"] = append(node["spec"].(map[string]any)["taints"].([]any), map[string]any{"key": "dedicated", "value": "other", "effect": "NoSchedule"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			node := readyBuilderNode("builder-1")
			mutate(node)
			resources.builderNodes = []map[string]any{node}
			if err := adapter.BuilderCapacityReady(context.Background()); !errors.Is(err, ErrBuilderCapacityUnavailable) {
				t.Fatalf("ineligible node accepted: %v", err)
			}
		})
	}
	resources.builderNodes = nil
	if err := adapter.BuilderCapacityReady(context.Background()); !errors.Is(err, ErrBuilderCapacityUnavailable) {
		t.Fatalf("empty capacity accepted: %v", err)
	}
	resources.builderNodes = []map[string]any{{"apiVersion": "v1", "kind": "Node"}}
	if err := adapter.BuilderCapacityReady(context.Background()); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("malformed node was not rejected: %v", err)
	}
}

func TestKubernetesRESTBuilderNodeListIsBoundedAndSelectorPinned(t *testing.T) {
	const serviceAccountToken = "service-account.header.signature"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/nodes" || request.URL.Query().Get("limit") != "100" || request.URL.Query().Get("labelSelector") != "kuberploy.io/node-class=dind-builder" {
			t.Errorf("unsafe node query: %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[{"metadata":{"name":"builder-1","labels":{"kuberploy.io/node-class":"dind-builder"}},"spec":{"taints":[{"key":"kuberploy.io/dind-builder","value":"true","effect":"NoSchedule"}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(serviceAccountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &inClusterBuildResources{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	nodes, err := client.ListBuilderNodes(context.Background(), 100)
	if err != nil || len(nodes) != 1 || nodes[0]["apiVersion"] != "v1" || nodes[0]["kind"] != "Node" {
		t.Fatalf("nodes=%#v err=%v", nodes, err)
	}
}

func TestKubernetesRESTResourcePathsAndNumbersAreClosed(t *testing.T) {
	path, err := kubernetesResourcePath(resourceJobs, "kuberploy-build-dind", "build-one")
	if err != nil || path != "/apis/batch/v1/namespaces/kuberploy-build-dind/jobs/build-one" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if _, err = kubernetesResourcePath(kubernetesResource("deployments"), "kuberploy-build-dind", "build-one"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broad resource accepted: %v", err)
	}
	object, err := decodeKubernetesObject([]byte(`{"apiVersion":"batch/v1","kind":"Job","status":{"succeeded":1}}`))
	if err != nil || jobCounter(object, "succeeded") != 1 {
		t.Fatalf("numeric Kubernetes status was not normalized: %#v err=%v", object, err)
	}
}

func TestKubernetesAdapterRejectsPersistedCacheCandidateMismatch(t *testing.T) {
	attempt, _ := kubernetesWorkloadFixture(t)
	attempt.CacheCandidate = "registry.example.test/attacker/cache:candidate-11111111111141118111111111111111-g1"
	adapter := newTestKubernetesAdapter(t, newFakeBuildResources())
	if _, err := adapter.desiredAttempt(attempt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched persisted candidate was accepted: %v", err)
	}
}

func TestKubernetesRESTPodListRejectsPagination(t *testing.T) {
	const serviceAccountToken = "service-account.header.signature"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("limit") != "2" || request.URL.Query().Get("labelSelector") != "kuberploy.io/build-operation=11111111111141118111111111111111,kuberploy.io/build-generation=1" {
			t.Errorf("unbounded pod query: %s", request.URL.RawQuery)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"continue":"opaque-next-page"},"items":[]}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(serviceAccountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &inClusterBuildResources{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	if _, err := client.ListBuildPods(context.Background(), "kuberploy-build-dind", "11111111111141118111111111111111", "1", 2); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("paginated pod list was accepted: %v", err)
	}
}

func TestKubernetesRESTPodListReconstructsOnlyOmittedFixedTypeMeta(t *testing.T) {
	const serviceAccountToken = "service-account.header.signature"
	for name, item := range map[string]struct {
		item     string
		accepted bool
	}{
		"omitted":     {item: `{"metadata":{"name":"build-pod","namespace":"kuberploy-build-dind"}}`, accepted: true},
		"exact":       {item: `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"build-pod","namespace":"kuberploy-build-dind"}}`, accepted: true},
		"partial":     {item: `{"kind":"Pod","metadata":{"name":"build-pod","namespace":"kuberploy-build-dind"}}`},
		"substituted": {item: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"build-pod","namespace":"kuberploy-build-dind"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` + item.item + `]}`))
			}))
			defer server.Close()
			tokenPath := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenPath, []byte(serviceAccountToken), 0o600); err != nil {
				t.Fatal(err)
			}
			client := &inClusterBuildResources{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
			pods, err := client.ListBuildPods(context.Background(), "kuberploy-build-dind", "11111111111141118111111111111111", "1", 2)
			if item.accepted {
				if err != nil || len(pods) != 1 || pods[0]["apiVersion"] != "v1" || pods[0]["kind"] != "Pod" {
					t.Fatalf("pods=%#v err=%v", pods, err)
				}
			} else if !errors.Is(err, ErrInfrastructure) {
				t.Fatalf("unsafe TypeMeta was accepted: pods=%#v err=%v", pods, err)
			}
		})
	}
}

func buildPodFixture(resources *fakeBuildResources, attempt BuildAttempt, phase string, checkoutDone bool, result []byte) map[string]any {
	job := resources.objects[resources.key(resourceJobs, attempt.JobNamespace, attempt.JobName)]
	jobMetadata := job["metadata"].(map[string]any)
	template := job["spec"].(map[string]any)["template"].(map[string]any)
	labels := cloneMap(template["metadata"].(map[string]any)["labels"].(map[string]any))
	labels["batch.kubernetes.io/controller-uid"] = jobMetadata["uid"]
	labels["batch.kubernetes.io/job-name"] = attempt.JobName
	labels["controller-uid"] = jobMetadata["uid"]
	labels["job-name"] = attempt.JobName
	status := map[string]any{"phase": phase}
	if checkoutDone {
		status["initContainerStatuses"] = []any{map[string]any{"name": "checkout", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(0)}}}}
	}
	if result != nil {
		status["containerStatuses"] = []any{map[string]any{"name": "agent", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(0), "message": string(result)}}}}
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": "build-pod-" + strings.Repeat("a", 8), "namespace": attempt.JobNamespace, "labels": labels,
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": attempt.JobName, "uid": jobMetadata["uid"], "controller": true, "blockOwnerDeletion": true}},
		},
		"status": status,
	}
}
