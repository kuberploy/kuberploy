package runtimeview

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPublicRequestsExposeOnlyOpaqueTargetAndBoundedOptions(t *testing.T) {
	for _, value := range []any{SnapshotRequest{}, EventRequest{}, FollowRequest{}, LogOptions{}} {
		typeOf := reflect.TypeOf(value)
		for index := 0; index < typeOf.NumField(); index++ {
			name := strings.ToLower(typeOf.Field(index).Name)
			for _, forbidden := range []string{"namespace", "selector", "bearer", "kubeconfig", "tls", "token"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("API-facing %s unexpectedly exposes %q", typeOf.Name(), typeOf.Field(index).Name)
				}
			}
		}
	}
}

func TestServiceRejectsInsecureClientAndUnboundedOptions(t *testing.T) {
	resolver, client := baseFixture()
	client.security = ClientSecurity{TLSVerified: true, InsecureSkipTLSVerify: true}
	if _, err := NewService(resolver, client, nil, DefaultConfig()); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("expected insecure transport rejection, got %v", err)
	}
	client.security = ClientSecurity{TLSVerified: true}
	service := newTestService(resolver, client, testConfig())
	now := time.Now().UTC()
	for name, options := range map[string]LogOptions{
		"tail too large":   {TailLines: 2_001},
		"body too large":   {LimitBytes: 5<<20 + 1},
		"lookback too old": {SinceTime: timePointer(now.Add(-25 * time.Hour))},
		"future":           {SinceTime: timePointer(now.Add(2 * time.Minute))},
		"container syntax": {Container: "other/container"},
		"pod syntax":       {Pod: "Other_Namespace/Pod"},
		"revision syntax":  {Revision: "42/latest"},
		"snapshot follow":  {Follow: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef, Options: options}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("expected invalid request, got %v", err)
			}
		})
	}
	client.mu.Lock()
	client.security.InsecureSkipTLSVerify = true
	client.mu.Unlock()
	if _, err := service.Events(context.Background(), EventRequest{Target: testTargetRef}); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("runtime security drift was not rejected: %v", err)
	}
}

func TestSnapshotRejectsCrossNamespaceAndUnapprovedSelectors(t *testing.T) {
	resolver, client := baseFixture()
	service := newTestService(resolver, client, testConfig())
	client.pods[0].Namespace = "another-tenant"
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef}); !errors.Is(err, ErrScopeViolation) {
		t.Fatalf("expected namespace fail-closed, got %v", err)
	}
	if len(client.requests()) != 0 {
		t.Fatal("cross-namespace response reached the log subresource")
	}

	_, client = baseFixture()
	client.deployment.Selector.MatchLabels["attacker.example/selector"] = "all"
	service = newTestService(resolver, client, testConfig())
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef}); !errors.Is(err, ErrSelectorNotAllowed) {
		t.Fatalf("expected selector allowlist rejection, got %v", err)
	}

	_, client = baseFixture()
	client.replicaSets[0].Revision = "42\nattacker"
	service = newTestService(resolver, client, testConfig())
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef}); !errors.Is(err, ErrScopeViolation) {
		t.Fatalf("expected untrusted revision rejection, got %v", err)
	}
}

func TestSnapshotUsesControllerOwnershipAndReReadsExactPodUID(t *testing.T) {
	resolver, client := baseFixture()
	attacker := clonePod(client.pods[0])
	attacker.Name = "payments-web-attacker"
	attacker.UID = "attacker-pod-uid"
	attacker.Owners = []OwnerReference{{UID: "unrelated-replicaset", Kind: "ReplicaSet", Controller: true}}
	client.pods = append(client.pods, attacker)
	client.currentPods[attacker.Name] = attacker
	client.openFn = func(request PodLogRequest, _ int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("safe\n")), nil
	}
	service := newTestService(resolver, client, testConfig())
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	requests := client.requests()
	if len(requests) != 1 || requests[0].PodUID != testPodUID || requests[0].Namespace != testNamespace {
		t.Fatalf("unexpected log source requests: %#v", requests)
	}
	if len(snapshot.Lines) != 1 || snapshot.Lines[0].Source.PodName == attacker.Name {
		t.Fatalf("unowned similarly-labelled Pod leaked into snapshot: %#v", snapshot)
	}
	client.mu.Lock()
	if client.getPodCalls != 1 {
		t.Fatalf("expected an immediate Pod re-read, got %d", client.getPodCalls)
	}
	client.mu.Unlock()

	_, client = baseFixture()
	replacement := clonePod(client.pods[0])
	replacement.UID = "pod-uid-replacement"
	client.currentPods[replacement.Name] = replacement
	service = newTestService(resolver, client, testConfig())
	snapshot, err = service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.requests()) != 0 || len(snapshot.Statuses) != 1 || snapshot.Statuses[0].State != "expired" {
		t.Fatalf("replacement UID was not stopped before log open: %#v", snapshot)
	}
}

func TestSnapshotRejectsContainerConfusionAndSupportsExactPreviousInstance(t *testing.T) {
	resolver, client := baseFixture()
	pod := client.pods[0]
	pod.Containers = append(pod.Containers, Container{Name: "sidecar", Kind: ContainerRegular, RestartCount: 0})
	client.pods[0] = pod
	client.currentPods[pod.Name] = pod
	service := newTestService(resolver, client, testConfig())
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef}); !errors.Is(err, ErrContainerRequired) {
		t.Fatalf("expected exact container requirement, got %v", err)
	}
	client.openFn = func(request PodLogRequest, _ int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("previous\n")), nil
	}
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef, Options: LogOptions{Container: "application", Previous: true}})
	if err != nil {
		t.Fatal(err)
	}
	requests := client.requests()
	if len(requests) != 1 || requests[0].Options.Container != "application" || !requests[0].Options.Previous {
		t.Fatalf("previous exact-container options were not preserved: %#v", requests)
	}
	if len(snapshot.Lines) != 1 || !snapshot.Lines[0].Source.Previous {
		t.Fatalf("missing previous source identity: %#v", snapshot)
	}

	_, client = baseFixture()
	listed := clonePod(client.pods[0])
	listed.Containers = append(listed.Containers, Container{Name: "sidecar", Kind: ContainerRegular})
	client.pods[0] = listed
	// The re-read no longer contains sidecar. A stale list result must not let
	// the caller open a different/default container.
	service = newTestService(resolver, client, testConfig())
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef, Options: LogOptions{Container: "sidecar"}}); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("expected container re-read rejection, got %v", err)
	}
	if len(client.requests()) != 0 {
		t.Fatal("container confusion reached log open")
	}
}

func TestSnapshotBoundsLinesBodiesAndAppliesDefenseInDepthRedaction(t *testing.T) {
	resolver, client := baseFixture()
	config := testConfig()
	config.MaxLineBytes = 32
	config.DefaultLimitBytes = 64
	config.MaxSourceBytes = 64
	config.MaxSnapshotBytes = 96
	client.openFn = func(request PodLogRequest, _ int) (io.ReadCloser, error) {
		body := strings.Repeat("x", 80) + "\nsecond-line-that-must-not-grow\n"
		return io.NopCloser(strings.NewReader(body)), nil
	}
	service := newTestService(resolver, client, config)
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Truncated || len(snapshot.Lines) != 1 || !snapshot.Lines[0].Truncated || len(snapshot.Lines[0].Message) > config.MaxLineBytes || snapshot.Bytes > config.MaxSnapshotBytes {
		t.Fatalf("snapshot bounds failed: %#v", snapshot)
	}

	_, client = baseFixture()
	client.openFn = func(request PodLogRequest, _ int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("\x1b[31mAuthorization: Bearer top-secret\x1b[0m\x00\npassword=hunter2\n")), nil
	}
	service = newTestService(resolver, client, testConfig())
	snapshot, err = service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef})
	if err != nil {
		t.Fatal(err)
	}
	joined := snapshot.Lines[0].Message + snapshot.Lines[1].Message
	if strings.Contains(joined, "top-secret") || strings.Contains(joined, "hunter2") || strings.Contains(joined, "\x1b") || strings.ContainsRune(joined, '\x00') || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("unsafe log output: %q", joined)
	}
}

func TestEventsUseExactUIDScopeAndSanitizeUntrustedFields(t *testing.T) {
	resolver, client := baseFixture()
	client.events = []KubernetesEvent{{
		Namespace: testNamespace, UID: "event-uid-1", InvolvedUID: testPodUID,
		InvolvedKind: "Secret", InvolvedName: "attacker-name", Type: "Warning",
		Reason: "Failed\n<script>", Message: "token=super-secret \x1b[31mboom\x1b[0m" + strings.Repeat("z", 100),
		Count: 2, FirstSeen: time.Now().Add(-time.Minute), LastSeen: time.Now(),
	}}
	config := testConfig()
	config.MaxEventMessageBytes = 48
	service := newTestService(resolver, client, config)
	snapshot, err := service.Events(context.Background(), EventRequest{Target: testTargetRef, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 {
		t.Fatalf("unexpected events: %#v", snapshot)
	}
	event := snapshot.Items[0]
	if event.ObjectKind != "Pod" || event.ObjectName != client.pods[0].Name || event.Reason != "Failedscript" || !event.MessageTruncated || strings.Contains(event.Message, "super-secret") || strings.Contains(event.Message, "\x1b") {
		t.Fatalf("event was not safely projected: %#v", event)
	}
	client.mu.Lock()
	query := client.eventQueries[0]
	client.mu.Unlock()
	if !slices.Contains(query.InvolvedUIDs, testDeploymentUID) || !slices.Contains(query.InvolvedUIDs, testReplicaSetUID) || !slices.Contains(query.InvolvedUIDs, testPodUID) {
		t.Fatalf("event query did not contain the exact graph UIDs: %#v", query)
	}

	client.mu.Lock()
	client.events = []KubernetesEvent{{Namespace: testNamespace, UID: "event-uid-2", InvolvedUID: "other-tenant-pod", Type: "Normal"}}
	client.mu.Unlock()
	if _, err := service.Events(context.Background(), EventRequest{Target: testTargetRef, Limit: 10}); !errors.Is(err, ErrScopeViolation) {
		t.Fatalf("cross-scope event was not rejected: %v", err)
	}
}

func TestTooManySourcesReturnsExactCountWithoutSampling(t *testing.T) {
	resolver, client := baseFixture()
	for index := 2; index <= 4; index++ {
		pod := clonePod(client.pods[0])
		pod.Name = "payments-web-7f9-pod" + string(rune('a'+index))
		pod.UID = "pod-uid-" + string(rune('a'+index))
		client.pods = append(client.pods, pod)
		client.currentPods[pod.Name] = pod
	}
	config := testConfig()
	config.MaxSources = 2
	service := newTestService(resolver, client, config)
	_, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef})
	var tooMany *TooManySourcesError
	if !errors.As(err, &tooMany) || tooMany.Count != 4 || tooMany.Limit != 2 {
		t.Fatalf("expected exact source count, got %#v / %v", tooMany, err)
	}
	if len(client.requests()) != 0 {
		t.Fatal("source cap silently sampled Pods")
	}

	selectedPod := client.pods[2].Name
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef, Options: LogOptions{Pod: selectedPod, Revision: "42"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].PodName != selectedPod || snapshot.Sources[0].Revision != "42" || len(client.requests()) != 1 || client.requests()[0].PodName != selectedPod {
		t.Fatalf("exact Pod/revision filter escaped its source: snapshot=%#v requests=%#v", snapshot, client.requests())
	}
	if _, err = service.Snapshot(context.Background(), SnapshotRequest{Target: testTargetRef, Options: LogOptions{Pod: "payments-web-missing"}}); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("missing exact source filter did not fail closed: %v", err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
