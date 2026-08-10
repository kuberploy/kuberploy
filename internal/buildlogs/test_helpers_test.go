package buildlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/builds"
)

const (
	testActorID       = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testAttemptID     = "11111111-1111-4111-8111-111111111111"
	testProjectID     = "22222222-2222-4222-8222-222222222222"
	testApplicationID = "33333333-3333-4333-8333-333333333333"
)

func buildLogFixture(t *testing.T) (AuthorizedAttempt, map[string]any, map[string]any) {
	t.Helper()
	server, prefix := "registry.example.test", "kuberploy"
	definition := "sha256:" + strings.Repeat("a", 64)
	cacheRepository := server + "/" + prefix + "/projects/" + testProjectID + "/services/" + testApplicationID + "/cache/v1/trusted/amd64/" + strings.Repeat("a", 64)
	request := builder.BuildRequest{
		APIVersion: builder.ProtocolVersion, OperationID: testAttemptID, Generation: 2,
		ProjectID: testProjectID, ServiceID: testApplicationID, Commit: strings.Repeat("b", 40),
		ContextPath: ".", DockerfilePath: "Dockerfile", Platforms: []string{"linux/amd64"},
		Destination: builder.Destination{
			Repository: server + "/" + prefix + "/projects/" + testProjectID + "/services/" + testApplicationID + "/image",
			Reference:  "candidate-11111111111141118111111111111111-g2-bbbbbbbbbbbb",
		},
		Registry: builder.RegistryCredentials{Server: server, RepositoryPrefix: prefix, UsernameFile: builder.RegistryPushSecretRoot + "/username", PasswordFile: builder.RegistryPushSecretRoot + "/password"},
		Cache: builder.CachePolicy{Schema: "v1", TrustLane: "trusted", BuildDefinition: definition,
			Imports: []string{cacheRepository + ":generation-1"}, CandidateExport: cacheRepository + ":candidate-11111111111141118111111111111111-g2",
			UsernameFile: builder.RegistryCacheSecretRoot + "/username", PasswordFile: builder.RegistryCacheSecretRoot + "/password"},
		Profile: builder.BuildProfile{Resource: "standard", TimeoutSeconds: 900, Egress: "registry-and-source"},
	}
	resources := builder.ContainerResources{CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "256Mi", CPULimit: "1000m", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi"}
	planRequest := builder.JobPlanRequest{
		Build: request, Namespace: "kuberploy-build-dind", PodServiceAccount: "kuberploy-build-pod",
		RequestConfigMap: "build-request-111111111111411181111111", SourceCredentialSecret: "source-credentials-11111111111111111111",
		RegistryPushCredentialSecret: "registry-push", RegistryCacheCredentialSecret: "registry-cache",
		CheckoutImage:     "registry.example.test/system/builder-agent@sha256:" + strings.Repeat("1", 64),
		AgentImage:        "registry.example.test/system/builder-agent@sha256:" + strings.Repeat("1", 64),
		NodeSelector:      map[string]string{"kuberploy.io/node-class": "dind-builder"},
		Toleration:        builder.TaintToleration{Key: "kuberploy.io/dind-builder", Value: "true", Effect: "NoSchedule"},
		CheckoutResources: resources, DinDResources: resources, AgentResources: resources,
		WorkspaceSizeLimit: "10Gi", SocketSizeLimit: "16Mi", ResultSizeLimit: "1Mi", DockerDataSizeLimit: "20Gi",
		ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 3600,
		Egress: []builder.EgressEndpoint{{CIDR: "192.0.2.10/32", Ports: []int{443}}},
	}
	plan, err := builder.PlanJob(planRequest)
	if err != nil {
		t.Fatal(err)
	}
	jobName := plan.Job["metadata"].(map[string]any)["name"].(string)
	checkout := builder.CheckoutRequest{
		APIVersion: builder.ProtocolVersion, OperationID: testAttemptID, Generation: 2,
		RepositoryURL: "https://github.com/kuberploy/example.git", ApprovedHost: "github.com", Commit: request.Commit,
		UsernameFile: builder.SourceCredentialRoot + "/username", AccessTokenFile: builder.SourceCredentialRoot + "/token",
	}
	encoded, err := json.Marshal(struct {
		Plan     builder.JobPlanRequest  `json:"plan"`
		Checkout builder.CheckoutRequest `json:"checkout"`
	}{planRequest, checkout})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	attempt := builds.BuildAttempt{
		ID: testAttemptID, DefinitionID: "77777777-7777-4777-8777-777777777777", DeliveryClaimKey: strings.Repeat("d", 64),
		ProjectID: testProjectID, ServiceID: testApplicationID, CommitSHA: request.Commit, GitRef: "refs/heads/main", Generation: 2,
		DefinitionDigest: definition, PlanRequest: planRequest, CheckoutRequest: checkout, InputDigest: "sha256:" + hex.EncodeToString(digest[:]),
		RegistryMode: builds.RegistryManaged, State: builds.AttemptRunning, ExecutionAttempts: 1, MaxAttempts: 2, AvailableAt: now,
		JobNamespace: planRequest.Namespace, JobName: jobName, CacheCandidate: request.Cache.CandidateExport, CreatedAt: now, UpdatedAt: now,
		LogReference: "k8s://attacker/pods/attacker/containers/agent",
	}
	liveJob := cloneObject(plan.Job)
	jobMetadata := liveJob["metadata"].(map[string]any)
	jobMetadata["uid"] = "44444444-4444-4444-8444-444444444444"
	jobMetadata["resourceVersion"] = "1"
	jobMetadata["annotations"] = map[string]any{"kuberploy.io/build-input-digest": attempt.InputDigest}
	jobSpec := liveJob["spec"].(map[string]any)
	jobSpec["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": jobMetadata["uid"]}}
	template := jobSpec["template"].(map[string]any)
	templateLabels := template["metadata"].(map[string]any)["labels"].(map[string]any)
	templateLabels["batch.kubernetes.io/controller-uid"] = jobMetadata["uid"]
	templateLabels["batch.kubernetes.io/job-name"] = jobName
	podLabels := cloneObject(templateLabels)
	podLabels["controller-uid"] = jobMetadata["uid"]
	podLabels["job-name"] = jobName
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": "build-pod-aaaaaaaa", "namespace": planRequest.Namespace,
			"uid": "55555555-5555-4555-8555-555555555555", "labels": podLabels,
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": jobName, "uid": jobMetadata["uid"], "controller": true, "blockOwnerDeletion": true}},
		},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "agent"}}, "initContainers": []any{map[string]any{"name": "checkout"}, map[string]any{"name": "dind"}}},
		"status": map[string]any{
			"containerStatuses": []any{map[string]any{"name": "agent", "restartCount": int64(1)}},
			"conditions":        []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}
	return AuthorizedAttempt{Access: AccessRequest{ActorID: testActorID, AttemptID: testAttemptID}, Attempt: attempt, ApplicationID: testApplicationID, ProjectID: testProjectID}, liveJob, pod
}

func cloneObject[T any](input T) T {
	return cloneValue(input).(T)
}

func cloneValue(input any) any {
	switch value := input.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			result[key] = cloneValue(item)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = cloneValue(item)
		}
		return result
	case []map[string]any:
		result := make([]map[string]any, len(value))
		for index, item := range value {
			result[index] = cloneValue(item).(map[string]any)
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(value))
		for key, item := range value {
			result[key] = item
		}
		return result
	default:
		return value
	}
}

type fakeResolver struct {
	mu            sync.RWMutex
	authorized    AuthorizedAttempt
	resolveErr    error
	revalidateErr error
	resolveCalls  int
}

func (r *fakeResolver) Resolve(_ context.Context, _ AccessRequest) (AuthorizedAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveCalls++
	if r.resolveErr != nil {
		return AuthorizedAttempt{}, r.resolveErr
	}
	value := r.authorized
	value.Attempt = cloneAttempt(value.Attempt)
	return value, nil
}

func (r *fakeResolver) Revalidate(context.Context, AccessRequest) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revalidateErr
}

func (r *fakeResolver) setRevalidateError(err error) {
	r.mu.Lock()
	r.revalidateErr = err
	r.mu.Unlock()
}

func cloneAttempt(input builds.BuildAttempt) builds.BuildAttempt {
	result := input
	result.PlanRequest.NodeSelector = cloneObject(input.PlanRequest.NodeSelector)
	result.PlanRequest.Egress = append([]builder.EgressEndpoint(nil), input.PlanRequest.Egress...)
	result.PlanRequest.Build.Platforms = append([]string(nil), input.PlanRequest.Build.Platforms...)
	result.PlanRequest.Build.Cache.Imports = append([]string(nil), input.PlanRequest.Build.Cache.Imports...)
	return result
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
	err    error
}

func (a *fakeAuditor) AuditBuildLogAccess(_ context.Context, event AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, event)
	return nil
}

type fakeKubernetes struct {
	mu       sync.Mutex
	security ClientSecurity
	job      map[string]any
	pods     []map[string]any
	logs     []string
	readers  []io.ReadCloser
	queries  []JobPodQuery
	opens    []ExactPodRef
	options  []PodLogOptions
	getJobs  int
	getPods  int
	onList   func(*fakeKubernetes)
	openErr  error
}

func (f *fakeKubernetes) Security() ClientSecurity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.security
}

func (f *fakeKubernetes) GetBuildJob(_ context.Context, _, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getJobs++
	if f.job == nil {
		return nil, ErrNotFound
	}
	return cloneObject(f.job), nil
}

func (f *fakeKubernetes) ListBuildJobPods(_ context.Context, query JobPodQuery) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	result := cloneObject(f.pods)
	if f.onList != nil {
		callback := f.onList
		f.onList = nil
		callback(f)
	}
	return result, nil
}

func (f *fakeKubernetes) GetBuildPod(_ context.Context, _, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPods++
	if len(f.pods) == 0 {
		return nil, ErrNotFound
	}
	return cloneObject(f.pods[0]), nil
}

func (f *fakeKubernetes) OpenBuilderAgentLogs(_ context.Context, ref ExactPodRef, options PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, ref)
	f.options = append(f.options, options)
	if f.openErr != nil {
		return nil, f.openErr
	}
	if len(f.readers) > 0 {
		reader := f.readers[0]
		f.readers = f.readers[1:]
		return reader, nil
	}
	value := ""
	if len(f.logs) > 0 {
		value = f.logs[0]
		f.logs = f.logs[1:]
	}
	return io.NopCloser(strings.NewReader(value)), nil
}

func newTestService(t *testing.T) (*Service, *fakeResolver, *fakeAuditor, *fakeKubernetes) {
	t.Helper()
	return newTestServiceWithConfig(t, DefaultConfig())
}

func newTestServiceWithConfig(t *testing.T, config Config) (*Service, *fakeResolver, *fakeAuditor, *fakeKubernetes) {
	t.Helper()
	authorized, job, pod := buildLogFixture(t)
	resolver := &fakeResolver{authorized: authorized}
	auditor := &fakeAuditor{}
	client := &fakeKubernetes{security: ClientSecurity{TLSVerified: true}, job: job, pods: []map[string]any{pod}}
	service, err := NewService(resolver, auditor, client, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) }
	return service, resolver, auditor, client
}

func snapshotRequest() SnapshotRequest {
	return SnapshotRequest{Access: AccessRequest{ActorID: testActorID, AttemptID: testAttemptID}, RequestID: "req-123", Options: LogOptions{Timestamps: true}}
}

func waitStreamEvent(t *testing.T, stream *Stream, timeout time.Duration, predicate func(StreamEvent) bool) StreamEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-stream.Events:
			if !ok {
				t.Fatal("stream closed before expected event")
			}
			if predicate(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for stream event")
		}
	}
}

var errAudit = errors.New("audit unavailable")
