package helmapps

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubPackages struct {
	artifact ChartArtifact
	err      error
}

func (s stubPackages) Fetch(context.Context, Approval) (ChartArtifact, error) {
	return s.artifact, s.err
}

type stubRenderer struct {
	outputs     [][]byte
	errors      []error
	invocations []RenderInvocation
	calls       int
}

func (s *stubRenderer) Render(_ context.Context, plan RenderPlan, invocation RenderInvocation) ([]byte, error) {
	if plan.Validate() != nil || plan.Sandbox != ExpectedSandboxContract() || invocation.Validate() != nil {
		return nil, ErrInvalid
	}
	s.invocations = append(s.invocations, invocation)
	index := s.calls
	s.calls++
	if index < len(s.errors) && s.errors[index] != nil {
		return nil, s.errors[index]
	}
	if index >= len(s.outputs) {
		return nil, errors.New("no stub output")
	}
	return append([]byte(nil), s.outputs[index]...), nil
}

func testWorker(t *testing.T, outputs [][]byte) (Worker, *MemoryStore, *stubRenderer) {
	t.Helper()
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	desired := testDesired(t, approval, []byte("replicas: 2\n"))
	store := NewMemoryStore()
	if _, _, err := store.PutApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(context.Background(), desired, testTime); err != nil {
		t.Fatal(err)
	}
	renderer := &stubRenderer{outputs: outputs}
	call := 0
	worker := Worker{Store: store,
		Packages: stubPackages{artifact: ChartArtifact{ManifestDigest: approval.ManifestDigest,
			PackageDigest: approval.PackageDigest, PackageBytes: packageBytes}},
		Renderer: renderer, LeaseDuration: time.Minute,
		OperatorConfigDigest: store.operatorConfigDigest,
		Now: func() time.Time {
			call++
			return testTime.Add(time.Duration(call-1) * time.Second)
		},
	}
	return worker, store, renderer
}

func TestWorkerDoubleRenderCompletesByteIdenticalOutput(t *testing.T) {
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	descriptor, err := NewDescriptor(approval, testDestination())
	if err != nil {
		t.Fatal(err)
	}
	manifest := validConfigMapManifest(descriptor)
	worker, store, renderer := testWorker(t, [][]byte{manifest, manifest})
	result, err := worker.ProcessOne(context.Background(), "helm-renderer-0001")
	if err != nil || result.CommandID != testCommandID || renderer.calls != 2 ||
		len(renderer.invocations) != 2 || renderer.invocations[0].Pass != 1 ||
		renderer.invocations[1].Pass != 2 || renderer.invocations[0].CommandID != testCommandID ||
		renderer.invocations[1].CommandID != testCommandID {
		t.Fatalf("process: %+v calls=%d err=%v", result, renderer.calls, err)
	}
	stored, err := store.Result(context.Background(), testCommandID)
	if err != nil || stored.ManifestDigest != result.ManifestDigest || !equalBytes(stored.RenderedManifests, result.RenderedManifests) {
		t.Fatalf("durable result mismatch: %+v %v", stored, err)
	}
}

func TestWorkerRejectsNonDeterminismPermanently(t *testing.T) {
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	descriptor, err := NewDescriptor(approval, testDestination())
	if err != nil {
		t.Fatal(err)
	}
	first := validConfigMapManifest(descriptor)
	second := append([]byte(nil), first...)
	second = append(second, []byte("# second render changed\n")...)
	worker, store, _ := testWorker(t, [][]byte{first, second})
	if _, err = worker.ProcessOne(context.Background(), "helm-renderer-0001"); !errors.Is(err, ErrNondeterministic) {
		t.Fatalf("non-determinism not rejected: %v", err)
	}
	command, err := store.Command(context.Background(), testCommandID)
	if err != nil || command.State != StateFailed || command.LastFailureCode != "nondeterministic-render" || command.CompletedAt == nil {
		t.Fatalf("non-determinism not terminal: %+v %v", command, err)
	}
}

func TestWorkerRetriesTransientRendererFailure(t *testing.T) {
	worker, store, renderer := testWorker(t, nil)
	renderer.errors = []error{errors.New("job temporarily unavailable")}
	if _, err := worker.ProcessOne(context.Background(), "helm-renderer-0001"); err == nil {
		t.Fatal("transient renderer error was hidden")
	}
	command, err := store.Command(context.Background(), testCommandID)
	if err != nil || command.State != StateQueued || command.Attempts != 1 ||
		command.LastFailureCode != "renderer-failed" || !command.AvailableAt.After(command.UpdatedAt) {
		t.Fatalf("transient renderer error not queued: %+v %v", command, err)
	}
}

func TestWorkerRejectsFetcherIdentityMismatch(t *testing.T) {
	worker, store, _ := testWorker(t, nil)
	packages := worker.Packages.(stubPackages)
	packages.artifact.ManifestDigest = digestBytes([]byte("different-manifest"))
	worker.Packages = packages
	if _, err := worker.ProcessOne(context.Background(), "helm-renderer-0001"); !errors.Is(err, ErrUnsafeChart) {
		t.Fatalf("fetch identity mismatch accepted: %v", err)
	}
	command, err := store.Command(context.Background(), testCommandID)
	if err != nil || command.State != StateFailed || command.LastFailureCode != "chart-identity-mismatch" {
		t.Fatalf("identity mismatch not terminal: %+v %v", command, err)
	}
}
