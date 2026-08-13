package runtimeview

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStatefulSetSnapshotUsesDirectControllerOwnership(t *testing.T) {
	resolver, client := statefulSetFixture()
	attacker := clonePod(client.pods[0])
	attacker.Name = "payments-db-attacker"
	attacker.UID = "attacker-stateful-pod-uid"
	attacker.Owners = []OwnerReference{{UID: "unrelated-statefulset", Kind: "StatefulSet", Controller: true}}
	client.pods = append(client.pods, attacker)
	client.currentPods[attacker.Name] = attacker
	client.openFn = func(PodLogRequest, int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("ready\n")), nil
	}
	service := newTestService(resolver, client, testConfig())
	snapshot, err := service.Snapshot(t.Context(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	requests := client.requests()
	if len(requests) != 1 || requests[0].PodName != "payments-db-0" || requests[0].PodUID != testPodUID {
		t.Fatalf("unexpected StatefulSet log sources: %#v", requests)
	}
	if len(snapshot.Lines) != 1 || snapshot.Lines[0].Source.PodName != "payments-db-0" {
		t.Fatalf("unexpected StatefulSet snapshot: %#v", snapshot)
	}
}

func TestStatefulSetSnapshotRejectsDeploymentRevisionFilter(t *testing.T) {
	resolver, client := statefulSetFixture()
	service := newTestService(resolver, client, testConfig())
	_, err := service.Snapshot(t.Context(), SnapshotRequest{Target: testTargetRef, Options: LogOptions{Revision: "42"}})
	if err != ErrSourceNotFound {
		t.Fatalf("StatefulSet with no numeric Deployment revision matched revision filter: %v", err)
	}
	if len(client.requests()) != 0 {
		t.Fatal("unsupported StatefulSet revision filter reached the log subresource")
	}
}

func TestStatefulSetSnapshotRejectsAmbiguousControllerIdentity(t *testing.T) {
	resolver, client := statefulSetFixture()
	pod := client.pods[0]
	pod.Owners = append(pod.Owners, OwnerReference{UID: "attacker-controller-uid", Kind: "ReplicaSet", Controller: true})
	client.pods[0] = pod
	client.currentPods[pod.Name] = pod
	service := newTestService(resolver, client, testConfig())
	snapshot, err := service.Snapshot(t.Context(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sources) != 0 || len(client.requests()) != 0 {
		t.Fatalf("ambiguously controlled StatefulSet Pod reached logs: %#v", snapshot.Sources)
	}
}

func TestStatefulSetEventsIncludeOnlyOwnedWorkloadGraph(t *testing.T) {
	resolver, client := statefulSetFixture()
	now := time.Now().UTC()
	client.events = []KubernetesEvent{
		{Namespace: testNamespace, UID: "event-statefulset-uid", InvolvedUID: testStatefulSetUID, Type: "Normal", Reason: "Ready", Message: "ready", Count: 1, FirstSeen: now, LastSeen: now},
		{Namespace: testNamespace, UID: "event-stateful-pod-uid", InvolvedUID: testPodUID, Type: "Normal", Reason: "Started", Message: "started", Count: 1, FirstSeen: now, LastSeen: now},
	}
	service := newTestService(resolver, client, testConfig())
	snapshot, err := service.Events(t.Context(), EventRequest{Target: testTargetRef, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 2 || snapshot.Items[0].ObjectKind != "StatefulSet" && snapshot.Items[1].ObjectKind != "StatefulSet" {
		t.Fatalf("StatefulSet event graph missing: %#v", snapshot.Items)
	}
	client.mu.Lock()
	queries := append([]EventQuery(nil), client.eventQueries...)
	client.mu.Unlock()
	if len(queries) != 1 || len(queries[0].InvolvedUIDs) != 2 {
		t.Fatalf("unexpected StatefulSet event query: %#v", queries)
	}
}

func TestStatefulSetFollowOpensExactDirectOwnedPod(t *testing.T) {
	resolver, client := statefulSetFixture()
	reader := newBlockingReader("")
	client.openFn = func(PodLogRequest, int) (io.ReadCloser, error) { return reader, nil }
	service := newTestService(resolver, client, testConfig())
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := service.Follow(ctx, FollowRequest{Target: testTargetRef, Options: LogOptions{Follow: true}})
	if err != nil {
		t.Fatal(err)
	}
	waitForOpen(t, client)
	requests := client.requests()
	if len(requests) == 0 || requests[0].PodName != "payments-db-0" || requests[0].PodUID != testPodUID {
		t.Fatalf("unexpected StatefulSet follow request: %#v", requests)
	}
	cancel()
	waitDone(t, stream)
	if !reader.wasClosed() {
		t.Fatal("StatefulSet follow reader was not closed")
	}
}
