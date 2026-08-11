package edge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestKubernetesReaderSurfaceHasNoSecretMutationOrGenericPath(t *testing.T) {
	readerType := reflect.TypeOf((*KubernetesReader)(nil)).Elem()
	expected := []string{"ClusterIssuer", "ConfigMap", "CustomResourceDefinition", "Deployment", "IngressClass", "NetworkPolicy", "Service"}
	if readerType.NumMethod() != len(expected) {
		t.Fatalf("Kubernetes reader surface expanded: %d methods", readerType.NumMethod())
	}
	for index, name := range expected {
		if method := readerType.Method(index); method.Name != name {
			t.Fatalf("unexpected Kubernetes method %d: %s", index, method.Name)
		}
	}
}

func TestKubernetesPathAllowlistIsExact(t *testing.T) {
	allowed := []string{
		"/apis/apps/v1/namespaces/edge/deployments/traefik",
		"/api/v1/namespaces/edge/services/traefik",
		"/api/v1/namespaces/edge/configmaps/edge-profile",
		"/apis/networking.k8s.io/v1/ingressclasses/traefik",
		"/apis/networking.k8s.io/v1/namespaces/kuberploy-dns/networkpolicies/cloudflare-egress",
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/middlewares.traefik.io",
		"/apis/cert-manager.io/v1/clusterissuers/letsencrypt",
	}
	for _, path := range allowed {
		if !validEdgeKubernetesPath(path) {
			t.Fatalf("required fixed observation path rejected: %s", path)
		}
	}
	rejected := []string{
		"/api/v1/namespaces/edge/secrets/provider", "/api/v1/pods", "/apis/apps/v1/namespaces/edge/deployments",
		"/apis/apps/v1/namespaces/edge/deployments/traefik/status", "/api/v1/namespaces/edge/configmaps/profile?watch=true",
		"/api/v1/namespaces/edge/services/traefik/proxy", "https://attacker.invalid/api/v1/configmaps",
	}
	for _, path := range rejected {
		if validEdgeKubernetesPath(path) {
			t.Fatalf("unsafe Kubernetes path accepted: %s", path)
		}
	}
}

func TestInClusterReaderUsesBoundedExactGETAndRejectsRedirects(t *testing.T) {
	spec := []byte(`{"replicas":1,"template":{"spec":{"containers":[{"name":"traefik","image":"docker.io/traefik:v3.5.3","args":["--entrypoints.web.address=:80"]}]}}}`)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/apis/apps/v1/namespaces/traefik-system/deployments/traefik" ||
			request.Header.Get("Authorization") != "Bearer exact-test-token" || request.Header.Get("Accept") != "application/json" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"metadata":{"name":"traefik","namespace":"traefik-system","uid":"33333333-3333-4333-8333-333333333333","resourceVersion":"123","generation":4,"labels":{"app.kubernetes.io/version":"v3.5.3"}},"spec":` + string(spec) + `,"status":{"observedGeneration":4,"availableReplicas":1}}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("exact-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }
	reader := &InClusterKubernetesReader{baseURL: server.URL, http: client, tokenPath: tokenPath}
	deployment, err := reader.Deployment(context.Background(), "traefik-system", "traefik", "traefik")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.SpecDigest != canonicalJSONDigest(spec) || deployment.Version != "v3.5.3" || deployment.ObservedGeneration != 4 ||
		deployment.ContainerImage != "docker.io/traefik:v3.5.3" || deployment.AvailableReplicas != 1 {
		t.Fatalf("deployment observation drifted: %#v", deployment)
	}
	before := requests.Load()
	var destination map[string]any
	if err = reader.getJSON(context.Background(), "/api/v1/namespaces/traefik-system/secrets/provider-credentials", &destination); !errors.Is(err, ErrInvalid) {
		t.Fatalf("generic Secret path accepted: %v", err)
	}
	if err = reader.getJSON(context.Background(), "/api/v1/namespaces/traefik-system/configmaps/profile?watch=true", &destination); !errors.Is(err, ErrInvalid) {
		t.Fatalf("query/watch path accepted: %v", err)
	}
	if requests.Load() != before {
		t.Fatal("invalid query reached Kubernetes API")
	}

	redirectServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", server.URL+"/api/v1/namespaces/x/services/y")
		writer.WriteHeader(http.StatusFound)
	}))
	defer redirectServer.Close()
	redirectClient := redirectServer.Client()
	redirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }
	redirectReader := &InClusterKubernetesReader{baseURL: redirectServer.URL, http: redirectClient, tokenPath: tokenPath}
	if err = redirectReader.getJSON(context.Background(), "/api/v1/namespaces/x/services/y", &destination); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect was followed or exposed: %v", err)
	}
}

func TestObservedDeploymentVersionFallsBackOnlyToExplicitImageVersion(t *testing.T) {
	if got := observedDeploymentVersion(nil, "docker.io/library/traefik:v3.7.10"); got != "v3.7.10" {
		t.Fatalf("explicit image version was not observed: %q", got)
	}
	if got := observedDeploymentVersion(map[string]string{"app.kubernetes.io/version": "v3.7.9"}, "docker.io/library/traefik:v3.7.10"); got != "v3.7.9" {
		t.Fatalf("valid object label did not remain authoritative: %q", got)
	}
	for _, image := range []string{
		"docker.io/library/traefik:latest",
		"docker.io/library/traefik@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"registry.example.test:5000/library/traefik",
	} {
		if got := observedDeploymentVersion(map[string]string{"app.kubernetes.io/version": "latest"}, image); got != "" {
			t.Fatalf("non-version image produced runtime version %q for %q", got, image)
		}
	}
}

func TestClusterIssuerRejectsDuplicateReadyConditions(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"metadata":{"name":"letsencrypt","uid":"44444444-4444-4444-8444-444444444444","resourceVersion":"7","generation":1},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1},{"type":"Ready","status":"False","observedGeneration":1}]}}`))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("exact-test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }
	reader := &InClusterKubernetesReader{baseURL: server.URL, http: client, tokenPath: tokenPath}
	if _, err := reader.ClusterIssuer(context.Background(), "letsencrypt"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Ready conditions accepted: %v", err)
	}
}
