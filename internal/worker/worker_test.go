package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	base "github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type unavailableQueue struct{}

func (unavailableQueue) Receive(context.Context, string, int) ([]domain.WorkMessage, error) {
	return nil, errors.New("Valkey unavailable")
}
func (unavailableQueue) Ack(context.Context, domain.WorkMessage) error { return nil }

type oneDeliveryQueue struct {
	message domain.WorkMessage
	sent    bool
	acks    int
}

func (q *oneDeliveryQueue) Receive(context.Context, string, int) ([]domain.WorkMessage, error) {
	if q.sent {
		return nil, nil
	}
	q.sent = true
	return []domain.WorkMessage{q.message}, nil
}

func (q *oneDeliveryQueue) Ack(context.Context, domain.WorkMessage) error {
	q.acks++
	return nil
}

func TestProcessorFallsBackToDurableOperationAndCompletesGit(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Login: "Admin", Role: "platform-admin", Issuer: "test", Subject: "admin", GrantRevision: 1, CreatedAt: time.Now()}
	if err := st.BootstrapAdmin(ctx, admin, strings.Repeat("h", 64), []byte("session-hash"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, admin.ID, "p", "p", domain.CreateProject{Name: "Demo", Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.CreateEnvironment(ctx, admin.ID, "e", "e", domain.CreateEnvironment{ProjectID: p.Value.ID, Name: "Dev", Slug: "dev", Namespace: "kp-demo-dev", ArgoProject: "kp-demo"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApplication(ctx, admin.ID, "a", "a", domain.CreateApplication{ProjectID: p.Value.ID, Name: "Hello", Slug: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	d, op, err := st.CreateDeployment(ctx, admin.ID, "d", "d", "request-1", domain.CreateDeployment{EnvironmentID: e.Value.ID, ApplicationID: a.Value.ID, Image: "registry.test/hello@sha256:" + strings.Repeat("b", 64), Replicas: 1, Port: 8080}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	legacy := &gitops.Writer{Root: root}
	direct := gitWriterFunc(func(ctx context.Context, operation domain.Operation, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) (domain.GitPublicationResult, error) {
		revision, writeErr := legacy.Write(ctx, operation, project, environment, application, deployment)
		return domain.GitPublicationResult{Mode: "direct", Revision: revision}, writeErr
	})
	processor := &Processor{Store: st, Queue: unavailableQueue{}, Writer: direct, Name: "test-worker"}
	n, err := processor.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("processed=%d", n)
	}
	finished, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "succeeded" || finished.GitRevision == "" {
		t.Fatalf("operation %#v", finished)
	}
	status, err := st.DeploymentStatus(ctx, d.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "git-committed" || status.DesiredRevision != finished.GitRevision {
		t.Fatalf("status %#v", status)
	}
	path := filepath.Join(root, "environments", e.Value.ID, "apps", a.Value.ID, "app.yaml")
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	second, secondOp, err := st.CreateDeployment(ctx, admin.ID, "d-2", "d-2", "request-2", domain.CreateDeployment{EnvironmentID: e.Value.ID, ApplicationID: a.Value.ID, Image: "registry.test/hello@sha256:" + strings.Repeat("c", 64), Replicas: 2, Port: 8080}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Value.ID != d.Value.ID || secondOp.Generation != 2 {
		t.Fatalf("second release replaced stable binding: %#v %#v", second.Value, secondOp)
	}
	n, err = processor.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("second release processed=%d err=%v", n, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strings.Repeat("c", 64)) {
		t.Fatal("worker wrote stale first-release input")
	}
	n, err = processor.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("terminal operation reprocessed: n=%d err=%v", n, err)
	}
}

type gitWriterFunc func(context.Context, domain.Operation, domain.Project, domain.Environment, domain.Application, domain.Deployment) (domain.GitPublicationResult, error)

func (f gitWriterFunc) Write(ctx context.Context, op domain.Operation, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) (domain.GitPublicationResult, error) {
	return f(ctx, op, project, environment, application, deployment)
}

type pendingGitResultError struct{}

func (pendingGitResultError) Error() string { return "push response was lost after acceptance" }
func (pendingGitResultError) ReconcilePending() (string, string) {
	return "GitCommitResultPending", "The Git push result is uncertain and will be reconciled from authoritative history."
}

func deploymentOperationFixture(t *testing.T) (*memory.Store, domain.Operation) {
	t.Helper()
	ctx := t.Context()
	st := memory.New()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Login: "Admin", Role: "platform-admin", Issuer: "test", Subject: "admin", GrantRevision: 1, CreatedAt: time.Now()}
	if err := st.BootstrapAdmin(ctx, admin, strings.Repeat("h", 64), []byte("session-hash"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject(ctx, admin.ID, "p", "p", domain.CreateProject{Name: "Demo", Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, admin.ID, "e", "e", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev", Namespace: "kp-demo-dev", ArgoProject: "kp-demo"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := st.CreateApplication(ctx, admin.ID, "a", "a", domain.CreateApplication{ProjectID: project.Value.ID, Name: "Hello", Slug: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, operation, err := st.CreateDeployment(ctx, admin.ID, "d", "d", "request", domain.CreateDeployment{
		EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.test/hello@sha256:" + strings.Repeat("b", 64), Replicas: 1, Port: 8080,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st, operation
}

func operationMessage(operation domain.Operation) domain.WorkMessage {
	return domain.WorkMessage{OperationID: operation.ID, Kind: operation.Kind, ScopeID: operation.TargetID, Generation: operation.Generation, DeliveryID: "1-0"}
}

func TestProcessorRequeuesUncertainPostPushResultWithoutAck(t *testing.T) {
	st, operation := deploymentOperationFixture(t)
	queue := &oneDeliveryQueue{message: operationMessage(operation)}
	calls := 0
	writer := gitWriterFunc(func(context.Context, domain.Operation, domain.Project, domain.Environment, domain.Application, domain.Deployment) (domain.GitPublicationResult, error) {
		calls++
		if calls == 1 {
			return domain.GitPublicationResult{}, pendingGitResultError{}
		}
		return domain.GitPublicationResult{Mode: "direct", Revision: strings.Repeat("a", 40)}, nil
	})
	processor := &Processor{Store: st, Queue: queue, Writer: writer, Name: "projection-worker"}
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("uncertain run n=%d err=%v", n, err)
	}
	pending, err := st.GetOperation(t.Context(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "queued" || pending.Problem != nil || queue.acks != 0 {
		t.Fatalf("pending=%#v acks=%d", pending, queue.acks)
	}
	if len(pending.Progress) != 1 || pending.Progress[0].Name != "git-write" || pending.Progress[0].Status != "pending" {
		t.Fatalf("pending progress %#v", pending.Progress)
	}
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("recovery run n=%d err=%v", n, err)
	}
	finished, err := st.GetOperation(t.Context(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "succeeded" || finished.GitRevision != strings.Repeat("a", 40) || queue.acks != 0 {
		t.Fatalf("finished=%#v acks=%d", finished, queue.acks)
	}
	// The original pending stream entry is acknowledged only after a replay
	// observes the already-terminal durable operation.
	queue.sent = false
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 0 || queue.acks != 1 {
		t.Fatalf("terminal replay n=%d acks=%d err=%v", n, queue.acks, err)
	}
}

type failFirstCompletionStore struct {
	base.Store
	failed bool
}

func (s *failFirstCompletionStore) CompleteGitOperation(ctx context.Context, operationID string, generation int64, worker string, publication domain.GitPublicationResult) error {
	if !s.failed {
		s.failed = true
		return errors.New("database unavailable after projection finalization")
	}
	return s.Store.CompleteGitOperation(ctx, operationID, generation, worker, publication)
}

func TestProcessorRecoversFinalizedProjectionBeforeOperationCompletion(t *testing.T) {
	st, operation := deploymentOperationFixture(t)
	wrapped := &failFirstCompletionStore{Store: st}
	queue := &oneDeliveryQueue{message: operationMessage(operation)}
	writes := 0
	writer := gitWriterFunc(func(context.Context, domain.Operation, domain.Project, domain.Environment, domain.Application, domain.Deployment) (domain.GitPublicationResult, error) {
		writes++
		// A real ProjectionWriter returns this same durable command receipt on
		// retry once FinalizeVerifiedPath has committed it.
		return domain.GitPublicationResult{Mode: "direct", Revision: strings.Repeat("c", 40)}, nil
	})
	processor := &Processor{Store: wrapped, Queue: queue, Writer: writer, Name: "projection-worker"}
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("completion outage run n=%d err=%v", n, err)
	}
	pending, _ := st.GetOperation(t.Context(), operation.ID)
	if pending.Status != "queued" || queue.acks != 0 {
		t.Fatalf("pending=%#v acks=%d", pending, queue.acks)
	}
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("completion recovery n=%d err=%v", n, err)
	}
	finished, _ := st.GetOperation(t.Context(), operation.ID)
	if finished.Status != "succeeded" || writes != 2 || queue.acks != 0 {
		t.Fatalf("finished=%#v writes=%d acks=%d", finished, writes, queue.acks)
	}
}

type heartbeatTrackingStore struct {
	base.Store
	mu         sync.Mutex
	heartbeats int
	fail       bool
}

func (s *heartbeatTrackingStore) HeartbeatOperation(ctx context.Context, operationID string, generation int64, worker string, lease time.Duration) error {
	s.mu.Lock()
	s.heartbeats++
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return base.ErrOperationLeaseLost
	}
	return s.Store.HeartbeatOperation(ctx, operationID, generation, worker, lease)
}

func (s *heartbeatTrackingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeats
}

func TestProcessorRenewsLeaseAcrossLongGitIO(t *testing.T) {
	st, operation := deploymentOperationFixture(t)
	wrapped := &heartbeatTrackingStore{Store: st}
	writer := gitWriterFunc(func(ctx context.Context, _ domain.Operation, _ domain.Project, _ domain.Environment, _ domain.Application, _ domain.Deployment) (domain.GitPublicationResult, error) {
		select {
		case <-ctx.Done():
			return domain.GitPublicationResult{}, ctx.Err()
		case <-time.After(280 * time.Millisecond):
			return domain.GitPublicationResult{Mode: "direct", Revision: strings.Repeat("d", 40)}, nil
		}
	})
	processor := &Processor{Store: wrapped, Queue: unavailableQueue{}, Writer: writer, Name: "projection-worker", OperationLeaseDuration: 120 * time.Millisecond, OperationHeartbeatInterval: 20 * time.Millisecond}
	if n, err := processor.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("long Git I/O n=%d err=%v", n, err)
	}
	finished, _ := st.GetOperation(t.Context(), operation.ID)
	if finished.Status != "succeeded" || wrapped.count() < 3 {
		t.Fatalf("finished=%#v heartbeats=%d", finished, wrapped.count())
	}
}

func TestProcessorDoesNotFailOrAckAfterOperationLeaseLoss(t *testing.T) {
	st, operation := deploymentOperationFixture(t)
	wrapped := &heartbeatTrackingStore{Store: st, fail: true}
	queue := &oneDeliveryQueue{message: operationMessage(operation)}
	writer := gitWriterFunc(func(ctx context.Context, _ domain.Operation, _ domain.Project, _ domain.Environment, _ domain.Application, _ domain.Deployment) (domain.GitPublicationResult, error) {
		<-ctx.Done()
		return domain.GitPublicationResult{}, ctx.Err()
	})
	processor := &Processor{Store: wrapped, Queue: queue, Writer: writer, Name: "projection-worker", OperationLeaseDuration: 200 * time.Millisecond, OperationHeartbeatInterval: 20 * time.Millisecond}
	if n, err := processor.RunOnce(t.Context()); err == nil || n != 0 {
		t.Fatalf("lease-loss run n=%d err=%v", n, err)
	}
	current, _ := st.GetOperation(t.Context(), operation.ID)
	if current.Status != "running" || current.Problem != nil || queue.acks != 0 {
		t.Fatalf("current=%#v acks=%d", current, queue.acks)
	}
}
