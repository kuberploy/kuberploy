package argo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	observerApplicationID = "11111111-1111-4111-8111-111111111111"
	observerProjectID     = "22222222-2222-4222-8222-222222222222"
	observerEnvironmentID = "33333333-3333-4333-8333-333333333333"
	observerUID           = "44444444-4444-4444-8444-444444444444"
	observerDeploymentID  = "77777777-7777-4777-8777-777777777777"
)

func observerTarget() ObservationTarget {
	return ObservationTarget{DeploymentID: observerDeploymentID, ApplicationID: observerApplicationID, ProjectID: observerProjectID, EnvironmentID: observerEnvironmentID,
		ArgoProject: ProjectName(observerProjectID), DestinationNamespace: "kp-observer", DesiredRevision: strings.Repeat("a", 40)}
}

func observerApplication(now time.Time) KubernetesApplication {
	target := observerTarget()
	return KubernetesApplication{UID: observerUID, Namespace: "argocd", Name: ApplicationName(target.DeploymentID), ResourceVersion: "17",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "kuberploy.io/application-id": target.ApplicationID,
			"kuberploy.io/deployment-id": target.DeploymentID, "kuberploy.io/project-id": target.ProjectID, "kuberploy.io/environment-id": target.EnvironmentID},
		Project: target.ArgoProject, DestinationServer: InClusterServer, DestinationNamespace: target.DestinationNamespace,
		SyncStatus: "Synced", SyncRevisions: []string{"1.2.3", strings.Repeat("b", 40), strings.Repeat("b", 40)},
		HealthStatus: "Healthy", OperationPhase: "Succeeded", ReconciledAt: now.UTC()}
}

func TestObservationFromKubernetesApplicationBindsServerOwnedIdentity(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	observation, err := ObservationFromKubernetesApplication(observerApplication(now), observerTarget(), "argocd")
	if err != nil {
		t.Fatal(err)
	}
	if observation.DeploymentID != observerDeploymentID || observation.ApplicationID != observerApplicationID || observation.ProjectID != observerProjectID || observation.EnvironmentID != observerEnvironmentID ||
		observation.ObservedRevision != strings.Repeat("b", 40) || observation.DesiredRevision != strings.Repeat("a", 40) ||
		observation.Sync != SyncSynced || observation.Health != HealthHealthy || observation.OperationPhase != "succeeded" ||
		!observation.ObservedAt.Equal(now) || observation.Resources == nil || len(observation.Resources) != 0 {
		t.Fatalf("unexpected observation: %#v", observation)
	}

	mutations := []func(*KubernetesApplication){
		func(value *KubernetesApplication) { value.Labels["kuberploy.io/project-id"] = observerEnvironmentID },
		func(value *KubernetesApplication) { value.Namespace = "other" },
		func(value *KubernetesApplication) { value.DestinationServer = "https://attacker.example" },
		func(value *KubernetesApplication) { value.DestinationNamespace = "other" },
		func(value *KubernetesApplication) { value.Project = "default" },
		func(value *KubernetesApplication) { value.Name = "attacker" },
		func(value *KubernetesApplication) {
			value.SyncRevisions = append(value.SyncRevisions, strings.Repeat("c", 40))
		},
		func(value *KubernetesApplication) { value.ResourceVersion = "" },
	}
	for index, mutate := range mutations {
		candidate := observerApplication(now)
		mutate(&candidate)
		if _, err = ObservationFromKubernetesApplication(candidate, observerTarget(), "argocd"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("mutation %d: expected invalid, got %v", index, err)
		}
	}

	notReady := observerApplication(now)
	notReady.ReconciledAt = time.Time{}
	if _, err = ObservationFromKubernetesApplication(notReady, observerTarget(), "argocd"); !errors.Is(err, ErrObservationNotReady) {
		t.Fatalf("expected not ready, got %v", err)
	}
}

func TestDecodeKubernetesApplicationPageIsBoundedAndClosed(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	application := observerApplication(now)
	wire := map[string]any{
		"metadata": map[string]any{"resourceVersion": "99", "continue": "next"},
		"items": []any{map[string]any{
			"metadata": map[string]any{"uid": application.UID, "namespace": application.Namespace, "name": application.Name,
				"resourceVersion": application.ResourceVersion, "labels": application.Labels},
			"spec": map[string]any{"project": application.Project, "destination": map[string]any{"server": application.DestinationServer, "namespace": application.DestinationNamespace}},
			"status": map[string]any{"sync": map[string]any{"status": application.SyncStatus, "revisions": application.SyncRevisions},
				"health": map[string]any{"status": application.HealthStatus}, "operationState": map[string]any{"phase": application.OperationPhase}, "reconciledAt": now},
		}},
	}
	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	page, err := decodeKubernetesApplicationPage(body, "argocd")
	if err != nil || page.ResourceVersion != "99" || page.Continue != "next" || len(page.Applications) != 1 || len(page.Applications[0].SyncRevisions) < 2 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	body = append(body, []byte(` {}`)...)
	if _, err = decodeKubernetesApplicationPage(body, "argocd"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}

func TestDecodeKubernetesApplicationPagePrefersCurrentSyncRevisions(t *testing.T) {
	now := time.Date(2026, 8, 11, 2, 51, 55, 0, time.UTC)
	application := observerApplication(now)
	current := strings.Repeat("c", 40)
	previous := strings.Repeat("b", 40)
	wire := map[string]any{
		"metadata": map[string]any{"resourceVersion": "99"},
		"items": []any{map[string]any{
			"metadata": map[string]any{"uid": application.UID, "namespace": application.Namespace, "name": application.Name,
				"resourceVersion": application.ResourceVersion, "labels": application.Labels},
			"spec": map[string]any{"project": application.Project, "destination": map[string]any{"server": application.DestinationServer, "namespace": application.DestinationNamespace}},
			"status": map[string]any{
				"sync":           map[string]any{"status": "Synced", "revisions": []string{"sha256:" + strings.Repeat("d", 64), current}},
				"health":         map[string]any{"status": "Healthy"},
				"operationState": map[string]any{"phase": "Succeeded", "syncResult": map[string]any{"revisions": []string{"sha256:" + strings.Repeat("e", 64), previous}}},
				"reconciledAt":   now,
			},
		}},
	}
	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	page, err := decodeKubernetesApplicationPage(body, "argocd")
	if err != nil || len(page.Applications) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	observation, err := ObservationFromKubernetesApplication(page.Applications[0], observerTarget(), "argocd")
	if err != nil || observation.ObservedRevision != current {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

type observerSource struct {
	pages []KubernetesApplicationPage
	calls int
}

func (s *observerSource) ListKuberployApplications(_ context.Context, namespace, continuation string, limit int) (KubernetesApplicationPage, error) {
	if namespace != "argocd" || limit < 1 || limit > 500 || s.calls >= len(s.pages) {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	page := s.pages[s.calls]
	if s.calls == 0 && continuation != "" || s.calls > 0 && continuation != s.pages[s.calls-1].Continue {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	s.calls++
	return page, nil
}

type observerResolver struct{ targets map[string]ObservationTarget }

func (r observerResolver) ResolveArgoObservationTarget(_ context.Context, applicationID string) (ObservationTarget, error) {
	target, ok := r.targets[applicationID]
	if !ok {
		return ObservationTarget{}, ErrNotFound
	}
	return target, nil
}

func TestKubernetesObserverPaginatesIgnoresUnknownAndRejectsSnapshotDrift(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	unknown := observerApplication(now)
	unknown.Name = "kp-a-55555555555545558555555555555555"
	unknown.UID = "66666666-6666-4666-8666-666666666666"
	unknown.Labels = map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "kuberploy.io/deployment-id": "55555555-5555-4555-8555-555555555555", "kuberploy.io/application-id": "55555555-5555-4555-8555-555555555555"}
	helmApplication := observerApplication(now)
	helmApplication.Name = "kp-h-11111111111141118111111111111111"
	helmApplication.UID = "88888888-8888-4888-8888-888888888888"
	helmApplication.Labels = map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "app.kubernetes.io/component": "helm-application", "kuberploy.io/application-id": observerApplicationID,
		"kuberploy.io/project-id": observerProjectID, "kuberploy.io/environment-id": observerEnvironmentID}
	source := &observerSource{pages: []KubernetesApplicationPage{
		{ResourceVersion: "100", Continue: "second", Applications: []KubernetesApplication{observerApplication(now)}},
		{ResourceVersion: "100", Applications: []KubernetesApplication{unknown, helmApplication}},
	}}
	store := NewMemoryObservationStore()
	observer := KubernetesObserver{Source: source, Resolver: observerResolver{targets: map[string]ObservationTarget{observerDeploymentID: observerTarget()}}, Store: store, Namespace: "argocd", PageSize: 1}
	result, err := observer.PollOnce(t.Context())
	if err != nil || result.Observed != 1 || result.IgnoredUnknown != 2 || result.SnapshotVersion != "100" || source.calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, source.calls, err)
	}
	if _, err = store.Observation(t.Context(), observerDeploymentID); err != nil {
		t.Fatal(err)
	}

	drift := &observerSource{pages: []KubernetesApplicationPage{{ResourceVersion: "100", Continue: "second"}, {ResourceVersion: "101"}}}
	observer.Source = drift
	if _, err = observer.PollOnce(t.Context()); err == nil || !strings.Contains(err.Error(), "resourceVersion") {
		t.Fatalf("expected snapshot resourceVersion rejection, got %v", err)
	}
}

func TestKubernetesObserverRejectsMalformedMissingAndDuplicateDeploymentIdentity(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	malformed := observerApplication(now)
	delete(malformed.Labels, "kuberploy.io/deployment-id")
	observer := KubernetesObserver{Source: &observerSource{pages: []KubernetesApplicationPage{{ResourceVersion: "101", Applications: []KubernetesApplication{malformed}}}},
		Resolver: observerResolver{targets: map[string]ObservationTarget{observerDeploymentID: observerTarget()}}, Store: NewMemoryObservationStore(), Namespace: "argocd"}
	if _, err := observer.PollOnce(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed missing deployment identity was not rejected: %v", err)
	}

	duplicate := observerApplication(now)
	observer.Source = &observerSource{pages: []KubernetesApplicationPage{{ResourceVersion: "102", Applications: []KubernetesApplication{duplicate, duplicate}}}}
	if _, err := observer.PollOnce(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate deployment identity was not rejected: %v", err)
	}
}

func TestKubernetesObserverBoundsIgnoredHelmApplications(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 1, 0, 0, time.UTC)
	helmApplication := observerApplication(now)
	helmApplication.Name = "kp-h-11111111111141118111111111111111"
	helmApplication.Labels = map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "app.kubernetes.io/component": "helm-application",
		"kuberploy.io/application-id": observerApplicationID, "kuberploy.io/project-id": observerProjectID, "kuberploy.io/environment-id": observerEnvironmentID}
	applications := make([]KubernetesApplication, MaximumObservedApplications+1)
	for index := range applications {
		applications[index] = helmApplication
	}
	observer := KubernetesObserver{Source: &observerSource{pages: []KubernetesApplicationPage{{ResourceVersion: "103", Applications: applications}}},
		Resolver: observerResolver{targets: map[string]ObservationTarget{}}, Store: NewMemoryObservationStore(), Namespace: "argocd"}
	if _, err := observer.PollOnce(t.Context()); err == nil || !strings.Contains(err.Error(), "exceeded its bound") {
		t.Fatalf("ignored Helm Applications bypassed observation bound: %v", err)
	}
}

func TestManagedHelmApplicationAcceptsCurrentAndLegacyComponents(t *testing.T) {
	application := observerApplication(time.Date(2026, 8, 14, 8, 2, 0, 0, time.UTC))
	application.Name = "kp-h-11111111111141118111111111111111"
	application.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "kuberploy",
		"kuberploy.io/application-id":  observerApplicationID,
		"kuberploy.io/project-id":      observerProjectID,
		"kuberploy.io/environment-id":  observerEnvironmentID,
	}
	for _, component := range []string{"helm-application", "approved-helm-application"} {
		application.Labels["app.kubernetes.io/component"] = component
		if !isManagedHelmApplication(application) {
			t.Fatalf("component %q was not recognized as a managed Helm Application", component)
		}
	}
	application.Labels["app.kubernetes.io/component"] = "deployment"
	if isManagedHelmApplication(application) {
		t.Fatal("unrelated managed Application was accepted as Helm")
	}
}

func TestInClusterApplicationClientUsesOnlyBoundedOwnedList(t *testing.T) {
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-service-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continue") == "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "provider-secret-must-not-leak")
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications" ||
			r.URL.Query().Get("labelSelector") != KuberployApplicationSelector || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("continue") != "cursor" ||
			r.Header.Get("Authorization") != "Bearer test-service-token" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected Kubernetes request: %s %s headers=%v", r.Method, r.URL.String(), r.Header)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		application := observerApplication(now)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"resourceVersion": "200"},
			"items": []any{map[string]any{
				"metadata": map[string]any{"uid": application.UID, "namespace": application.Namespace, "name": application.Name,
					"resourceVersion": application.ResourceVersion, "labels": application.Labels},
				"spec": map[string]any{"project": application.Project, "destination": map[string]any{"server": application.DestinationServer, "namespace": application.DestinationNamespace}},
				"status": map[string]any{"sync": map[string]any{"status": application.SyncStatus, "revisions": application.SyncRevisions},
					"health": map[string]any{"status": application.HealthStatus}, "operationState": map[string]any{"phase": application.OperationPhase}, "reconciledAt": now},
			}},
		})
	}))
	defer server.Close()
	client := &InClusterApplicationClient{baseURL: server.URL, http: server.Client(), tokenPath: tokenPath}
	page, err := client.ListKuberployApplications(t.Context(), "argocd", "cursor", 1)
	if err != nil || page.ResourceVersion != "200" || len(page.Applications) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}

	leak := "provider-secret-must-not-leak"
	_, err = client.ListKuberployApplications(t.Context(), "argocd", "", 1)
	if err == nil || strings.Contains(err.Error(), leak) {
		t.Fatalf("Kubernetes error body leaked: %v", err)
	}
}
