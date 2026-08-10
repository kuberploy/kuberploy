package builds

import (
	"errors"
	"testing"
)

func TestBuildLogObservationVerifierReusesExactControllerIdentity(t *testing.T) {
	attempt, _ := kubernetesWorkloadFixture(t)
	adapter := newTestKubernetesAdapter(t, newFakeBuildResources())
	desired, err := adapter.desiredAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	liveJob, err := adapter.resources.Create(t.Context(), resourceJobs, desired.namespace, desired.job)
	if err != nil {
		t.Fatal(err)
	}
	job, err := VerifyObservedBuildJob(attempt, liveJob)
	if err != nil || job.UID == "" || job.Name != attempt.JobName || job.Namespace != attempt.JobNamespace {
		t.Fatalf("job=%#v err=%v", job, err)
	}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": "build-pod-aaaaaaaa", "namespace": attempt.JobNamespace, "uid": "55555555-5555-4555-8555-555555555555",
			"labels":          cloneMap(liveJob["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)),
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": attempt.JobName, "uid": job.UID, "controller": true, "blockOwnerDeletion": true}},
		},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "agent"}}, "initContainers": []any{map[string]any{"name": "checkout"}, map[string]any{"name": "dind"}}},
		"status": map[string]any{
			"containerStatuses": []any{map[string]any{"name": "agent", "restartCount": int64(2)}},
			"conditions":        []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}
	verifiedPod, err := VerifyObservedBuildPod(job, pod)
	if err != nil || verifiedPod.UID == "" || verifiedPod.AgentRestarts != 2 || !verifiedPod.Ready {
		t.Fatalf("pod=%#v err=%v", verifiedPod, err)
	}

	mutatedJob := cloneMap(liveJob)
	mutatedJob["spec"].(map[string]any)["parallelism"] = int64(2)
	if _, err = VerifyObservedBuildJob(attempt, mutatedJob); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("mutated Job accepted: %v", err)
	}
	wrongOwner := cloneMap(pod)
	wrongOwner["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "66666666-6666-4666-8666-666666666666"
	if _, err = VerifyObservedBuildPod(job, wrongOwner); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("foreign owner accepted: %v", err)
	}
	missingAgent := cloneMap(pod)
	missingAgent["spec"].(map[string]any)["containers"] = []any{map[string]any{"name": "other"}}
	if _, err = VerifyObservedBuildPod(job, missingAgent); !errors.Is(err, ErrInfrastructure) {
		t.Fatalf("Pod without exact agent accepted: %v", err)
	}
}
