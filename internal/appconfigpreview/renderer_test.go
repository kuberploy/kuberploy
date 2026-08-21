package appconfigpreview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/helmapps"
)

func testIdentity() Identity {
	return Identity{Contract: Contract, ChartName: "kuberploy-runtime", ChartVersion: "1.2.3",
		ChartDigest: "sha256:" + strings.Repeat("a", 64), RendererImage: RendererImage,
		RendererVersion: RendererVersion, PolicyVersion: helmapps.PolicyVersion}
}

func TestRendererProducesDeterministicBoundedRedactedDiff(t *testing.T) {
	request := Request{Namespace: "kp-project-development", ReleaseName: "kp-preview-123456789012",
		ProjectID: "11111111-1111-4111-8111-111111111111", EnvironmentID: "22222222-2222-4222-8222-222222222222",
		ApplicationID: "33333333-3333-4333-8333-333333333333",
		CurrentValues: []byte("replicas: 1\nsecret: current-secret\n"), CandidateValues: []byte("replicas: 2\nsecret: candidate-secret\n")}
	calls := 0
	renderer, err := NewTestService(testIdentity(), func(_ context.Context, binary string, arguments ...string) ([]byte, error) {
		calls++
		if binary != ProductionHelmPath || len(arguments) != 15 || arguments[0] != "template" || arguments[2] != ProductionChartPath ||
			arguments[3] != "--namespace" || arguments[4] != request.Namespace || arguments[5] != "--values" ||
			arguments[7] != "--kube-version" || arguments[8] != helmapps.RendererKubeVersion ||
			arguments[9] != "--set-string" || arguments[10] != "kuberployExpectedIdentity.projectId="+request.ProjectID ||
			arguments[11] != "--set-string" || arguments[12] != "kuberployExpectedIdentity.environmentId="+request.EnvironmentID ||
			arguments[13] != "--set-string" || arguments[14] != "kuberployExpectedIdentity.applicationId="+request.ApplicationID {
			t.Fatalf("renderer escaped closed arguments: %#v", arguments)
		}
		values, readErr := os.ReadFile(arguments[6])
		if readErr != nil {
			return nil, readErr
		}
		generation := "1"
		secret := "current-secret"
		if strings.Contains(string(values), "replicas: 2") {
			generation, secret = "2", "candidate-secret"
		}
		return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kp-a-preview\n  namespace: " + request.Namespace +
			"\n  labels:\n    kuberploy.io/application: " + request.ApplicationID + "\n    kuberploy.io/project: " + request.ProjectID +
			"\n    kuberploy.io/environment: " + request.EnvironmentID + "\n  annotations:\n    generation: \"" + generation + "\"\ndata:\n  API_TOKEN: " + secret + "\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := renderer.Render(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || result.RenderedDiff == "" || !strings.Contains(result.RenderedDiff, "generation") ||
		strings.Contains(result.RenderedDiff, "current-secret") || strings.Contains(result.RenderedDiff, "candidate-secret") ||
		result.IdentityDigest == "" || result.Identity != testIdentity() {
		t.Fatalf("unsafe or incomplete result: calls=%d result=%#v", calls, result)
	}
}

func TestRendererFailsClosedOnNondeterminismAndIdentityDrift(t *testing.T) {
	identity := testIdentity()
	identity.RendererVersion = "4.2.2"
	if _, err := NewTestService(identity, func(context.Context, string, ...string) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted identity drift: %v", err)
	}
	request := Request{Namespace: "kp-test", ReleaseName: "kp-preview-123456789012", ProjectID: "p", EnvironmentID: "e", ApplicationID: "a", CurrentValues: []byte("x: y\n"), CandidateValues: []byte("x: z\n")}
	calls := 0
	renderer, err := NewTestService(testIdentity(), func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte("different: " + string(rune('0'+calls)) + "\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = renderer.Render(t.Context(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nondeterministic render was accepted: %v", err)
	}
}

func TestPreviewTokenHashIsBoundToRenderIdentity(t *testing.T) {
	raw := []byte(strings.Repeat("x", 32))
	first, err := PreviewTokenHash(raw, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := PreviewTokenHash(raw, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("renderer identity did not participate in preview authority")
	}
	if _, err = PreviewTokenHash(raw[:31], "sha256:"+strings.Repeat("a", 64)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted malformed preview authority input: %v", err)
	}
}

func TestRepositoryRuntimeChartPassesProductionRenderBoundary(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is unavailable")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", "kuberploy-runtime"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values = []byte(strings.Replace(string(values), "kuberployExpectedIdentity:\n  projectId: 00000000-0000-4000-8000-000000000002\n  environmentId: 00000000-0000-4000-8000-000000000003\n  applicationId: 00000000-0000-4000-8000-000000000001\n", "", 1))
	candidate := []byte(strings.Replace(string(values), "replicas: 1", "replicas: 2", 1))
	renderer, err := newService(testIdentity(), helm, chart, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "kp-preview", ReleaseName: "kp-preview-123456789012",
		ProjectID: "00000000-0000-4000-8000-000000000002", EnvironmentID: "00000000-0000-4000-8000-000000000003",
		ApplicationID: "00000000-0000-4000-8000-000000000001", CurrentValues: values, CandidateValues: candidate}
	result, err := renderer.Render(t.Context(), request)
	if err != nil {
		t.Fatalf("runtime chart failed production preview boundary: %v", err)
	}
	if !strings.Contains(result.RenderedDiff, "replicas") {
		t.Fatalf("runtime diff omitted replica change: %s", result.RenderedDiff)
	}
}

func TestRepositoryRuntimeChartStatefulSetPassesProductionRenderBoundary(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is unavailable")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", "kuberploy-runtime"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values = []byte(strings.Replace(string(values), "kuberployExpectedIdentity:\n  projectId: 00000000-0000-4000-8000-000000000002\n  environmentId: 00000000-0000-4000-8000-000000000003\n  applicationId: 00000000-0000-4000-8000-000000000001\n", "", 1))
	candidate := []byte(strings.Replace(string(values), "  runtime:\n    replicas: 1", "  runtime:\n    workloadType: StatefulSet\n    strategy:\n      type: OnDelete\n    podManagementPolicy: Parallel\n    replicas: 1", 1))
	renderer, err := newService(testIdentity(), helm, chart, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "kp-preview", ReleaseName: "kp-preview-123456789012",
		ProjectID: "00000000-0000-4000-8000-000000000002", EnvironmentID: "00000000-0000-4000-8000-000000000003",
		ApplicationID: "00000000-0000-4000-8000-000000000001", CurrentValues: values, CandidateValues: candidate}
	result, err := renderer.Render(t.Context(), request)
	if err != nil {
		t.Fatalf("StatefulSet runtime chart failed production preview boundary: %v", err)
	}
	for _, expected := range []string{"kind: StatefulSet", "type: OnDelete", "podManagementPolicy: Parallel"} {
		if !strings.Contains(result.RenderedDiff, expected) {
			t.Fatalf("runtime diff omitted %q: %s", expected, result.RenderedDiff)
		}
	}
}
