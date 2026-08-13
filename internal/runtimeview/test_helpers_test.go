package runtimeview

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	testNamespace      = "payments-production"
	testApplicationID  = "application-opaque-1"
	testDeploymentUID  = "deployment-uid-1"
	testStatefulSetUID = "statefulset-uid-1"
	testReplicaSetUID  = "replicaset-uid-1"
	testPodUID         = "pod-uid-1"
)

var testTargetRef = OpaqueTarget{Kind: TargetDeployment, ID: "deployment-opaque-1"}

type fakeResolver struct {
	mu            sync.Mutex
	target        AuthorizedTarget
	resolveErr    error
	revalidateErr error
	revalidations int
}

func (r *fakeResolver) Resolve(_ context.Context, reference OpaqueTarget) (AuthorizedTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolveErr != nil {
		return AuthorizedTarget{}, r.resolveErr
	}
	target := r.target
	target.Reference = reference
	target.Deployments = slices.Clone(target.Deployments)
	return target, nil
}

func (r *fakeResolver) Revalidate(_ context.Context, _ OpaqueTarget) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revalidations++
	return r.revalidateErr
}

func (r *fakeResolver) setRevalidateError(err error) {
	r.mu.Lock()
	r.revalidateErr = err
	r.mu.Unlock()
}

type fakeKubernetes struct {
	mu sync.Mutex

	security    ClientSecurity
	deployment  Deployment
	statefulSet StatefulSet
	replicaSets []ReplicaSet
	pods        []Pod
	currentPods map[string]Pod
	events      []KubernetesEvent

	getPodFn func(namespace, name string) (Pod, error)
	openFn   func(PodLogRequest, int) (io.ReadCloser, error)

	selectors    []LabelSelector
	openRequests []PodLogRequest
	eventQueries []EventQuery
	getPodCalls  int
}

func (f *fakeKubernetes) Security() ClientSecurity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.security
}

func (f *fakeKubernetes) GetDeployment(_ context.Context, namespace, name string) (Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deployment := f.deployment
	deployment.Selector = cloneSelector(deployment.Selector)
	return deployment, nil
}

func (f *fakeKubernetes) GetStatefulSet(_ context.Context, namespace, name string) (StatefulSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	statefulSet := f.statefulSet
	statefulSet.Selector = cloneSelector(statefulSet.Selector)
	return statefulSet, nil
}

func (f *fakeKubernetes) ListReplicaSets(_ context.Context, _ string, selector LabelSelector) ([]ReplicaSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectors = append(f.selectors, cloneSelector(selector))
	result := slices.Clone(f.replicaSets)
	for index := range result {
		result[index].Owners = slices.Clone(result[index].Owners)
	}
	return result, nil
}

func (f *fakeKubernetes) ListPods(_ context.Context, _ string, selector LabelSelector) ([]Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectors = append(f.selectors, cloneSelector(selector))
	result := make([]Pod, len(f.pods))
	for index, pod := range f.pods {
		result[index] = clonePod(pod)
	}
	return result, nil
}

func (f *fakeKubernetes) GetPod(_ context.Context, namespace, name string) (Pod, error) {
	f.mu.Lock()
	f.getPodCalls++
	custom := f.getPodFn
	pod, ok := f.currentPods[name]
	f.mu.Unlock()
	if custom != nil {
		return custom(namespace, name)
	}
	if !ok {
		return Pod{}, ErrNotFound
	}
	return clonePod(pod), nil
}

func (f *fakeKubernetes) OpenPodLogs(_ context.Context, request PodLogRequest) (io.ReadCloser, error) {
	f.mu.Lock()
	f.openRequests = append(f.openRequests, request)
	index := len(f.openRequests) - 1
	custom := f.openFn
	f.mu.Unlock()
	if custom != nil {
		return custom(request, index)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeKubernetes) ListEvents(_ context.Context, _ string, query EventQuery) ([]KubernetesEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	query.InvolvedUIDs = slices.Clone(query.InvolvedUIDs)
	f.eventQueries = append(f.eventQueries, query)
	return slices.Clone(f.events), nil
}

func (f *fakeKubernetes) requests() []PodLogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.openRequests)
}

func (f *fakeKubernetes) replacePods(pods []Pod) {
	f.mu.Lock()
	f.pods = make([]Pod, len(pods))
	for index, pod := range pods {
		f.pods[index] = clonePod(pod)
	}
	for _, pod := range pods {
		f.currentPods[pod.Name] = clonePod(pod)
	}
	f.mu.Unlock()
}

func baseFixture() (*fakeResolver, *fakeKubernetes) {
	selector := LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":   "kuberploy-runtime",
		"kuberploy.io/application": testApplicationID,
	}}
	deployment := Deployment{Namespace: testNamespace, Name: "payments-web", UID: testDeploymentUID, Selector: selector}
	replicaSet := ReplicaSet{Namespace: testNamespace, Name: "payments-web-7f9", UID: testReplicaSetUID, Owners: []OwnerReference{{UID: testDeploymentUID, Kind: "Deployment", Controller: true}}, Revision: "42", Ready: true}
	pod := Pod{Namespace: testNamespace, Name: "payments-web-7f9-abc", UID: testPodUID, Owners: []OwnerReference{{UID: testReplicaSetUID, Kind: "ReplicaSet", Controller: true}}, Containers: []Container{{Name: "application", Kind: ContainerRegular, RestartCount: 1}}, Ready: true}
	resolver := &fakeResolver{target: AuthorizedTarget{Reference: testTargetRef, ApplicationID: testApplicationID, Namespace: testNamespace, Deployments: []DeploymentRef{{Name: deployment.Name, UID: deployment.UID}}}}
	client := &fakeKubernetes{
		security:    ClientSecurity{TLSVerified: true},
		deployment:  deployment,
		replicaSets: []ReplicaSet{replicaSet},
		pods:        []Pod{pod},
		currentPods: map[string]Pod{pod.Name: pod},
	}
	return resolver, client
}

func statefulSetFixture() (*fakeResolver, *fakeKubernetes) {
	selector := LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name":   "kuberploy-runtime",
		"kuberploy.io/application": testApplicationID,
	}}
	statefulSet := StatefulSet{Namespace: testNamespace, Name: "payments-db", UID: testStatefulSetUID, Selector: selector}
	pod := Pod{Namespace: testNamespace, Name: "payments-db-0", UID: testPodUID, Owners: []OwnerReference{{UID: testStatefulSetUID, Kind: "StatefulSet", Controller: true}}, Containers: []Container{{Name: "application", Kind: ContainerRegular, RestartCount: 1}}, Ready: true}
	resolver := &fakeResolver{target: AuthorizedTarget{Reference: testTargetRef, ApplicationID: testApplicationID, Namespace: testNamespace, Deployments: []DeploymentRef{{Kind: "StatefulSet", Name: statefulSet.Name, UID: statefulSet.UID}}}}
	client := &fakeKubernetes{
		security:    ClientSecurity{TLSVerified: true},
		statefulSet: statefulSet,
		pods:        []Pod{pod},
		currentPods: map[string]Pod{pod.Name: pod},
	}
	return resolver, client
}

func testConfig() Config {
	config := DefaultConfig()
	config.RevalidateInterval = 20 * time.Millisecond
	config.HeartbeatInterval = 50 * time.Millisecond
	config.RediscoverInterval = 25 * time.Millisecond
	config.ReconnectDelay = 5 * time.Millisecond
	config.MaxFollowDuration = 2 * time.Second
	return config
}

func newTestService(resolver *fakeResolver, client *fakeKubernetes, config Config) *Service {
	service, err := NewService(resolver, client, nil, config)
	if err != nil {
		panic(err)
	}
	service.now = func() time.Time { return time.Now().UTC() }
	return service
}

type blockingReadCloser struct {
	mu     sync.Mutex
	data   []byte
	closed chan struct{}
	once   sync.Once
}

func newBlockingReader(data string) *blockingReadCloser {
	return &blockingReadCloser{data: []byte(data), closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	if len(r.data) > 0 {
		count := copy(buffer, r.data)
		r.data = r.data[count:]
		r.mu.Unlock()
		return count, nil
	}
	r.mu.Unlock()
	<-r.closed
	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (r *blockingReadCloser) wasClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}
