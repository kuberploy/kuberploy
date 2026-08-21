package argo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	productionPlatformBindingID    = "71111111-1111-4111-8111-111111111111"
	productionEnvironmentBindingID = "72111111-1111-4111-8111-111111111111"
	productionClusterID            = "73111111-1111-4111-8111-111111111111"
	productionProjectID            = "74111111-1111-4111-8111-111111111111"
	productionEnvironmentID        = "75111111-1111-4111-8111-111111111111"
)

type staticPrivateKeySource struct {
	value []byte
	err   error
}

func (s *staticPrivateKeySource) ReadGitHubAppPrivateKey(context.Context) ([]byte, error) {
	return s.value, s.err
}

type recordingCredentialKubernetes struct {
	applies      []RepositoryCredentialApply
	deletes      []string
	deleteAbsent bool
	err          error
}

func (k *recordingCredentialKubernetes) ApplyRepositoryCredential(_ context.Context, apply RepositoryCredentialApply, now time.Time) (RepositoryCredentialObservation, error) {
	if k.err != nil {
		return RepositoryCredentialObservation{}, k.err
	}
	copy := apply
	copy.PrivateKey = append([]byte(nil), apply.PrivateKey...)
	k.applies = append(k.applies, copy)
	return RepositoryCredentialObservation{BindingID: apply.BindingID, Namespace: apply.Namespace, Name: apply.Name,
		UID: "76111111-1111-4111-8111-111111111111", ResourceVersion: "10", SpecDigest: apply.SpecDigest, ObservedAt: now}, nil
}

func (k *recordingCredentialKubernetes) DeleteRepositoryCredential(_ context.Context, namespace, name, bindingID string, now time.Time) (RepositoryCredentialRevocationObservation, error) {
	if k.err != nil {
		return RepositoryCredentialRevocationObservation{}, k.err
	}
	k.deletes = append(k.deletes, namespace+"/"+name+"/"+bindingID)
	return RepositoryCredentialRevocationObservation{BindingID: bindingID, Namespace: namespace, Name: name,
		Absent: k.deleteAbsent, ObservedAt: now}, nil
}

type staticRuntimeBindingCatalog struct {
	values []RepositoryBindingAuthority
	err    error
}

type refreshingRuntimeBindingCatalog struct {
	staticRuntimeBindingCatalog
	calls    int
	bindings []gitprojection.Binding
	errMark  error
}

type staticFoundationProbe struct{ err error }

func (p staticFoundationProbe) Probe(context.Context) error { return p.err }

func (c staticRuntimeBindingCatalog) ArgoRepositoryBindings(context.Context, int64, string, string, time.Time, time.Duration) ([]RepositoryBindingAuthority, error) {
	return c.values, c.err
}

func (c *refreshingRuntimeBindingCatalog) MarkArgoRepositoryBindingsVerified(_ context.Context, _ int64, bindings []gitprojection.Binding, _ time.Time) error {
	c.calls++
	c.bindings = append([]gitprojection.Binding(nil), bindings...)
	return c.errMark
}

type staticHeadVerifier struct {
	head  gitprojection.VerifiedHead
	heads map[string]gitprojection.VerifiedHead
	err   error
}

type staticProtectionVerifier struct {
	mutate func(*PlatformRepositoryProtectionObservation)
	err    error
}

func (v staticProtectionVerifier) VerifyPlatformRepositoryProtection(_ context.Context, binding gitprojection.Binding, head gitprojection.VerifiedHead, now time.Time) (PlatformRepositoryProtectionObservation, error) {
	if v.err != nil {
		return PlatformRepositoryProtectionObservation{}, v.err
	}
	observation := PlatformRepositoryProtectionObservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Head: head.Commit,
		PolicyDigest: "sha256:" + strings.Repeat("9", 64), ObservedAt: now}
	if v.mutate != nil {
		v.mutate(&observation)
	}
	return observation, nil
}

func (v staticHeadVerifier) VerifyTargetHead(_ context.Context, binding gitprojection.Binding, _ gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	if head, ok := v.heads[binding.ID]; ok {
		return head, v.err
	}
	return v.head, v.err
}

type staticRootApplicationSource struct {
	mutate func(*PlatformRootApplicationObservation)
	err    error
}

func (s staticRootApplicationSource) ObservePlatformRootApplication(_ context.Context, expectation PlatformRootApplicationExpectation, now time.Time) (PlatformRootApplicationObservation, error) {
	if s.err != nil {
		return PlatformRootApplicationObservation{}, s.err
	}
	observation := PlatformRootApplicationObservation{Namespace: expectation.Namespace, Name: expectation.Name,
		UID: "77111111-1111-4111-8111-111111111111", ResourceVersion: "12", SpecDigest: expectation.SpecDigest,
		ObservedRevision: expectation.ExpectedGitRevision, SyncStatus: "Synced", HealthStatus: "Healthy", ObservedAt: now}
	if s.mutate != nil {
		s.mutate(&observation)
	}
	return observation, nil
}

type recoveringRootApplication struct {
	staleRevision string
	refreshes     int
}

func (s *recoveringRootApplication) ObservePlatformRootApplication(_ context.Context, expectation PlatformRootApplicationExpectation, now time.Time) (PlatformRootApplicationObservation, error) {
	revision := expectation.ExpectedGitRevision
	if s.refreshes == 0 {
		revision = s.staleRevision
	}
	return PlatformRootApplicationObservation{Namespace: expectation.Namespace, Name: expectation.Name,
		UID: "77111111-1111-4111-8111-111111111111", ResourceVersion: "12", SpecDigest: expectation.SpecDigest,
		ObservedRevision: revision, SyncStatus: "Synced", HealthStatus: "Healthy", ObservedAt: now}, nil
}

func (s *recoveringRootApplication) RefreshPlatformRootApplication(_ context.Context, expectation PlatformRootApplicationExpectation, now time.Time) error {
	if expectation.Name != PlatformRootApplicationName || !commitRE.MatchString(expectation.ExpectedGitRevision) || now.IsZero() {
		return ErrInvalid
	}
	s.refreshes++
	return nil
}

func productionBindings(t *testing.T, now time.Time) (gitprojection.Binding, gitprojection.Binding) {
	t.Helper()
	platform, err := gitprojection.NewGitHubPlatformBinding(productionPlatformBindingID, productionClusterID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 501, RepositoryID: 601, Owner: "kuberploy", Name: "platform"},
		"refs/heads/platform", now)
	if err != nil {
		t.Fatal(err)
	}
	platform.TargetHeadRevision, platform.TargetHeadObservedAt = strings.Repeat("a", 40), now
	platform.State, platform.UpdatedAt = gitprojection.BindingIndexing, now
	environment, err := gitprojection.NewGitHubEnvironmentBinding(productionEnvironmentBindingID, productionProjectID, productionEnvironmentID,
		gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 502, RepositoryID: 602, Owner: "kuberploy", Name: "environment"},
		"refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	environment.TargetHeadRevision, environment.IndexedRevision = strings.Repeat("b", 40), strings.Repeat("b", 40)
	environment.TargetHeadObservedAt, environment.IndexedAt = now, now
	environment.ProjectionGeneration, environment.State, environment.UpdatedAt = 1, gitprojection.BindingReady, now
	if platform.Validate() != nil || environment.Validate() != nil {
		t.Fatal("invalid production binding fixture")
	}
	return platform, environment
}

func productionIdentity(t *testing.T, platform gitprojection.Binding) DesiredStateRuntimeIdentity {
	t.Helper()
	credentialName, err := RepositoryCredentialName(platform.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := DesiredStateRuntimeIdentityForConfig(DesiredStateRuntimeConfig{Enabled: true, GitHubAppID: 1001,
		PlatformBindingID: platform.ID, ClusterID: platform.ClusterID, ArgoNamespace: "argocd",
		RootApplicationName: PlatformRootApplicationName, RepositorySecretName: credentialName,
		Runtime: RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime", ChartVersion: "1.2.3",
			ChartDigest: "sha256:" + strings.Repeat("c", 64), RendererImage: "ghcr.io/kuberploy/renderer@sha256:" + strings.Repeat("d", 64)},
		DigestEnforcement: ChartDigestNativeOCI})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestRepositoryCredentialControllerUsesOnlyExactBindingNamesAndRevokesClosedCatalog(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	platform, environment := productionBindings(t, now)
	key := []byte("-----BEGIN PRIVATE KEY-----\ntest-only\n-----END PRIVATE KEY-----\n")
	keySource := &staticPrivateKeySource{value: key}
	kubernetes := &recordingCredentialKubernetes{}
	controller := &RepositoryCredentialController{Namespace: "argocd", GitHubAppID: 1001, Keys: keySource, Kubernetes: kubernetes}
	authorities := []RepositoryBindingAuthority{
		{Binding: environment, Authorized: true, CatalogObservedAt: now},
		{Binding: platform, Authorized: true, CatalogObservedAt: now},
	}
	observation, err := controller.Reconcile(t.Context(), authorities, platform.ID, now, time.Minute)
	if err != nil || len(observation.Observed) != 2 || len(kubernetes.applies) != 2 {
		t.Fatalf("observation=%#v applies=%d err=%v", observation, len(kubernetes.applies), err)
	}
	for _, apply := range kubernetes.applies {
		wantName, nameErr := RepositoryCredentialName(apply.BindingID)
		if nameErr != nil || apply.Name != wantName || apply.Namespace != "argocd" || apply.GitHubAppID != 1001 ||
			!strings.HasPrefix(apply.RepositoryURL, "https://github.com/kuberploy/") || !digestRE.MatchString(apply.SpecDigest) {
			t.Fatalf("credential authority drifted: %#v err=%v", apply, nameErr)
		}
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("private key source buffer was not cleared")
	}

	keySource.value = []byte("another-test-key")
	kubernetes.applies, kubernetes.deletes = nil, nil
	authorities[0].Authorized, authorities[0].RevocationRequired, authorities[0].CatalogObservedAt = false, true, time.Time{}
	if _, err = controller.Reconcile(t.Context(), authorities, platform.ID, now, time.Minute); !errors.Is(err, ErrRepositoryCredentialNotReady) {
		t.Fatalf("revoked binding did not fail closed: %v", err)
	}
	wantDeletedName, _ := RepositoryCredentialName(environment.ID)
	if len(kubernetes.deletes) != 1 || !strings.Contains(kubernetes.deletes[0], wantDeletedName) || len(kubernetes.applies) != 1 ||
		kubernetes.applies[0].BindingID != platform.ID {
		t.Fatalf("revocation boundary was wrong: deletes=%v applies=%v", kubernetes.deletes, kubernetes.applies)
	}
	// A later exact NotFound acknowledgement completes revocation, so a
	// permanently retained deactivated catalog row cannot wedge readiness.
	keySource.value = []byte("post-revocation-test-key")
	kubernetes.applies, kubernetes.deletes, kubernetes.deleteAbsent = nil, nil, true
	observation, err = controller.Reconcile(t.Context(), authorities, platform.ID, now, time.Minute)
	if err != nil || len(observation.Observed) != 1 || observation.Observed[0].BindingID != platform.ID ||
		len(kubernetes.deletes) != 1 {
		t.Fatalf("absent revoked credential did not converge: observation=%#v deletes=%v err=%v", observation, kubernetes.deletes, err)
	}

	duplicate := []RepositoryBindingAuthority{authorities[1], authorities[1]}
	if _, err = controller.Reconcile(t.Context(), duplicate, platform.ID, now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate binding accepted: %v", err)
	}
	stale := []RepositoryBindingAuthority{{Binding: platform, Authorized: true, CatalogObservedAt: now.Add(-2 * time.Minute)}}
	if _, err = controller.Reconcile(t.Context(), stale, platform.ID, now, time.Minute); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
		t.Fatalf("stale catalog accepted: %v", err)
	}
	kubernetes.deletes = nil
	stale[0].Authorized = false
	if _, err = controller.Reconcile(t.Context(), stale, platform.ID, now, time.Minute); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) || len(kubernetes.deletes) != 0 {
		t.Fatalf("stale catalog destructively revoked a credential: deletes=%v err=%v", kubernetes.deletes, err)
	}
}

func TestProductionPrerequisitesRequireExactProviderHeadCredentialSetAndRootSpec(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	platform, environment := productionBindings(t, now)
	identity := productionIdentity(t, platform)
	authorities := []RepositoryBindingAuthority{{Binding: platform, Authorized: true, CatalogObservedAt: now},
		{Binding: environment, Authorized: true, CatalogObservedAt: now}}
	head := gitprojection.VerifiedHead{BindingID: platform.ID, Repository: platform.Repository, TargetRef: platform.TargetRef,
		Commit: platform.TargetHeadRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "runtime-provider-head", ObservedAt: now}
	credentials := &RepositoryCredentialController{Namespace: "argocd", GitHubAppID: identity.GitHubAppID,
		Keys: &staticPrivateKeySource{value: []byte("test-key")}, Kubernetes: &recordingCredentialKubernetes{}}
	prerequisites := &ProductionPrerequisites{Identity: identity, Catalog: staticRuntimeBindingCatalog{values: authorities},
		Credentials: credentials, Provider: staticHeadVerifier{head: head}, Protection: staticProtectionVerifier{}, RootApplications: staticRootApplicationSource{},
		RootRefresher: productionRuntimeRefresherStub{}, Foundation: staticFoundationProbe{}, MaximumCatalogAge: time.Minute}
	proof, err := prerequisites.ObserveProductionPrerequisites(t.Context(), now)
	if err != nil || proof.PlatformHead != head.Commit || proof.CredentialCount != 2 || proof.RootUID == "" {
		t.Fatalf("proof=%#v err=%v", proof, err)
	}

	staleAuthorities := []RepositoryBindingAuthority{{Binding: platform, CatalogObservedAt: now.Add(-2 * time.Minute)},
		{Binding: environment, CatalogObservedAt: now.Add(-2 * time.Minute)}}
	environmentHead := gitprojection.VerifiedHead{BindingID: environment.ID, Repository: environment.Repository,
		TargetRef: environment.TargetRef, Commit: environment.TargetHeadRevision, Source: gitprojection.ObservationPoll,
		ProviderRequest: "runtime-environment-head", ObservedAt: now}
	credentials.Keys.(*staticPrivateKeySource).value = []byte("refreshed-test-key")
	credentials.Kubernetes.(*recordingCredentialKubernetes).applies = nil
	refreshingCatalog := &refreshingRuntimeBindingCatalog{staticRuntimeBindingCatalog: staticRuntimeBindingCatalog{values: staleAuthorities}}
	prerequisites.Catalog = refreshingCatalog
	prerequisites.Provider = staticHeadVerifier{heads: map[string]gitprojection.VerifiedHead{
		platform.ID: head, environment.ID: environmentHead,
	}}
	proof, err = prerequisites.ObserveProductionPrerequisites(t.Context(), now)
	if err != nil || proof.CredentialCount != 2 || len(credentials.Kubernetes.(*recordingCredentialKubernetes).applies) != 2 {
		t.Fatalf("provider re-verification did not refresh stale matching authorities: proof=%#v err=%v", proof, err)
	}
	if refreshingCatalog.calls != 1 || len(refreshingCatalog.bindings) != 2 {
		t.Fatalf("provider verification did not renew the stale catalog: calls=%d bindings=%d", refreshingCatalog.calls, len(refreshingCatalog.bindings))
	}
	prerequisites.Catalog = staticRuntimeBindingCatalog{values: authorities}
	prerequisites.Provider = staticHeadVerifier{head: head}

	kubernetes := credentials.Kubernetes.(*recordingCredentialKubernetes)
	kubernetes.applies = nil
	prerequisites.Foundation = staticFoundationProbe{err: errors.New("foundation not root-synced")}
	if _, err = prerequisites.ObserveProductionPrerequisites(t.Context(), now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) || len(kubernetes.applies) != 0 {
		t.Fatalf("foundation gate was not fail-closed and first: applies=%d err=%v", len(kubernetes.applies), err)
	}
	prerequisites.Foundation = staticFoundationProbe{}

	badHead := head
	badHead.Commit = strings.Repeat("e", 40)
	prerequisites.Provider = staticHeadVerifier{head: badHead}
	if _, err = prerequisites.ObserveProductionPrerequisites(t.Context(), now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
		t.Fatalf("provider head mismatch accepted: %v", err)
	}
	prerequisites.Provider = staticHeadVerifier{head: head}
	prerequisites.Protection = staticProtectionVerifier{mutate: func(value *PlatformRepositoryProtectionObservation) {
		value.TargetRef = "refs/heads/unprotected"
	}}
	if _, err = prerequisites.ObserveProductionPrerequisites(t.Context(), now); !errors.Is(err, ErrArgoRuntimePrerequisiteNotReady) {
		t.Fatalf("branch protection mismatch accepted: %v", err)
	}
	prerequisites.Protection = staticProtectionVerifier{}
	prerequisites.RootApplications = staticRootApplicationSource{mutate: func(value *PlatformRootApplicationObservation) {
		value.SpecDigest = "sha256:" + strings.Repeat("f", 64)
	}}
	if _, err = prerequisites.ObserveProductionPrerequisites(t.Context(), now); !errors.Is(err, ErrPlatformRootNotReady) {
		t.Fatalf("root spec mismatch accepted: %v", err)
	}
}

func TestProductionPrerequisitesHardRefreshesExactStaleRootBeforeReadiness(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	platform, environment := productionBindings(t, now)
	identity := productionIdentity(t, platform)
	authorities := []RepositoryBindingAuthority{{Binding: platform, Authorized: true, CatalogObservedAt: now},
		{Binding: environment, Authorized: true, CatalogObservedAt: now}}
	head := gitprojection.VerifiedHead{BindingID: platform.ID, Repository: platform.Repository, TargetRef: platform.TargetRef,
		Commit: platform.TargetHeadRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "runtime-provider-head", ObservedAt: now}
	credentials := &RepositoryCredentialController{Namespace: "argocd", GitHubAppID: identity.GitHubAppID,
		Keys: &staticPrivateKeySource{value: []byte("test-key")}, Kubernetes: &recordingCredentialKubernetes{}}
	root := &recoveringRootApplication{staleRevision: strings.Repeat("f", 40)}
	prerequisites := &ProductionPrerequisites{Identity: identity, Catalog: staticRuntimeBindingCatalog{values: authorities},
		Credentials: credentials, Provider: staticHeadVerifier{head: head}, Protection: staticProtectionVerifier{},
		RootApplications: root, RootRefresher: root, Foundation: staticFoundationProbe{}, MaximumCatalogAge: time.Minute}

	proof, err := prerequisites.ObserveProductionPrerequisites(t.Context(), now)
	if err != nil || proof.PlatformHead != platform.TargetHeadRevision || proof.RootUID == "" {
		t.Fatalf("proof=%#v err=%v", proof, err)
	}
	if root.refreshes != 1 {
		t.Fatalf("stale exact root refreshes=%d, want 1", root.refreshes)
	}
}

func TestInClusterProductionClientUsesClosedSecretAndRootApplicationRequests(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	platform, _ := productionBindings(t, now)
	identity := productionIdentity(t, platform)
	head := gitprojection.VerifiedHead{BindingID: platform.ID, Repository: platform.Repository, TargetRef: platform.TargetRef,
		Commit: platform.TargetHeadRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "runtime-http-head", ObservedAt: now}
	expectation, err := NewPlatformRootApplicationExpectation(identity, platform, head)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("http-test-private-key")
	apply, err := newRepositoryCredentialApply("argocd", identity.GitHubAppID, platform, key)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := t.TempDir() + "/token"
	if err = os.WriteFile(tokenFile, []byte("service-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, deleteRequests, refreshRequests, rootReads := 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer service-account-token" {
			t.Errorf("missing bounded service token")
		}
		switch request.Method {
		case http.MethodPatch:
			if request.URL.Path == "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/"+PlatformRootApplicationName {
				refreshRequests++
				if request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/merge-patch+json" {
					t.Errorf("unsafe root refresh request: %s headers=%v", request.URL.String(), request.Header)
				}
				var body map[string]any
				if decodeErr := json.NewDecoder(io.LimitReader(request.Body, maximumArgoRuntimeResponseBytes)).Decode(&body); decodeErr != nil {
					t.Errorf("decode root refresh: %v", decodeErr)
				}
				encoded, _ := json.Marshal(body)
				if string(encoded) != `{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}` {
					t.Errorf("root refresh body drifted: %s", encoded)
				}
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata",
					"metadata": map[string]any{"name": expectation.Name, "namespace": expectation.Namespace,
						"uid": "79111111-1111-4111-8111-111111111111", "resourceVersion": "22",
						"annotations": map[string]string{argoHardRefreshAnnotation: "hard"}}})
				break
			}
			wantPath := "/api/v1/namespaces/argocd/secrets/" + apply.Name
			if request.URL.Path != wantPath || request.URL.Query().Get("fieldManager") != RepositoryCredentialFieldManager ||
				request.URL.Query().Get("force") != "true" || request.Header.Get("Content-Type") != "application/apply-patch+yaml" {
				t.Errorf("unsafe credential request: %s %s headers=%v", request.Method, request.URL.String(), request.Header)
			}
			var body repositorySecretApplyWire
			if decodeErr := json.NewDecoder(io.LimitReader(request.Body, maximumArgoRuntimeResponseBytes)).Decode(&body); decodeErr != nil {
				t.Errorf("decode apply: %v", decodeErr)
			}
			if body.Metadata.Name != apply.Name || body.Metadata.Namespace != "argocd" || body.Metadata.Labels["kuberploy.io/git-binding-id"] != platform.ID ||
				!bytes.Equal(body.Data["githubAppPrivateKey"], key) || string(body.Data["url"]) != apply.RepositoryURL || len(body.Data) != 5 {
				t.Errorf("credential apply body drifted: metadata=%#v keys=%v", body.Metadata, body.Data)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata",
				"metadata": map[string]any{"name": apply.Name, "namespace": "argocd", "uid": "78111111-1111-4111-8111-111111111111", "resourceVersion": "20",
					"labels": body.Metadata.Labels, "annotations": body.Metadata.Annotations}})
		case http.MethodGet:
			if request.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/"+PlatformRootApplicationName {
				t.Errorf("unsafe root path: %s", request.URL.Path)
			}
			rootReads++
			revision := expectation.ExpectedGitRevision
			if refreshRequests == 1 && rootReads == 2 {
				revision = strings.Repeat("f", 40)
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"metadata": map[string]any{"name": expectation.Name, "namespace": expectation.Namespace,
					"uid": "79111111-1111-4111-8111-111111111111", "resourceVersion": "21",
					"labels":      map[string]string{"app.kubernetes.io/part-of": "kuberploy"},
					"annotations": map[string]string{"kuberploy.io/repository-secret": expectation.RepositoryCredentialName}},
				"spec": platformRootApplicationSpec(expectation),
				"status": map[string]any{"sync": map[string]string{"status": "Synced", "revision": revision},
					"health": map[string]string{"status": "Healthy"}},
			})
		case http.MethodDelete:
			if request.URL.Path != "/api/v1/namespaces/argocd/secrets/"+apply.Name {
				t.Errorf("unsafe delete path: %s", request.URL.Path)
			}
			deleteRequests++
			if deleteRequests == 1 {
				writer.WriteHeader(http.StatusOK)
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
		default:
			t.Errorf("unexpected method %s", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	client := &InClusterProductionClient{baseURL: parsed.String(), http: server.Client(), tokenPath: tokenFile}
	credential, err := client.ApplyRepositoryCredential(t.Context(), apply, now)
	if err != nil || credential.SpecDigest != apply.SpecDigest {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	root, err := client.ObservePlatformRootApplication(t.Context(), expectation, now)
	if err != nil || root.ObservedRevision != expectation.ExpectedGitRevision || root.SpecDigest != expectation.SpecDigest {
		t.Fatalf("root=%#v err=%v", root, err)
	}
	if err = client.RefreshPlatformRootApplication(t.Context(), expectation, now); err != nil {
		t.Fatalf("refresh root through transient stale read: %v", err)
	}
	revocation, err := client.DeleteRepositoryCredential(t.Context(), "argocd", apply.Name, platform.ID, now)
	if err != nil || revocation.Absent {
		t.Fatalf("delete request was not pending: observation=%#v err=%v", revocation, err)
	}
	revocation, err = client.DeleteRepositoryCredential(t.Context(), "argocd", apply.Name, platform.ID, now)
	if err != nil || !revocation.Absent {
		t.Fatalf("NotFound did not acknowledge revocation: observation=%#v err=%v", revocation, err)
	}
	if requests != 7 || refreshRequests != 1 || rootReads != 3 {
		t.Fatalf("requests=%d refreshes=%d root reads=%d", requests, refreshRequests, rootReads)
	}
	if _, err = client.DeleteRepositoryCredential(t.Context(), "argocd", "attacker-secret", platform.ID, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary Secret delete accepted: %v", err)
	}
	invalidExpectation := expectation
	invalidExpectation.Name = "attacker-root"
	if err = client.RefreshPlatformRootApplication(t.Context(), invalidExpectation, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary Application refresh accepted: %v", err)
	}
}

func TestInClusterProductionClientRefreshesExactEnvironmentApplicationSet(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	expectation := EnvironmentApplicationSetExpectation{Namespace: "argocd", Name: ApplicationSetName(productionEnvironmentID),
		ProjectID: productionProjectID, EnvironmentID: productionEnvironmentID}
	if expectation.Validate() != nil {
		t.Fatal("valid ApplicationSet expectation rejected")
	}
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("service-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applicationsets/"+expectation.Name ||
			request.Header.Get("Authorization") != "Bearer service-account-token" {
			t.Errorf("unsafe ApplicationSet request: method=%s path=%s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		annotations := map[string]string{}
		// Annotation removal is the completion signal. The API server may expose
		// the consumed metadata with the same projected resourceVersion returned
		// by PATCH, so requiring another version advance can wedge the worker.
		resourceVersion := "10"
		switch request.Method {
		case http.MethodPatch:
			if request.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Errorf("unexpected patch content type: %s", request.Header.Get("Content-Type"))
			}
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil || !bytes.Contains(body, []byte(`"argocd.argoproj.io/application-set-refresh":"true"`)) {
				t.Errorf("invalid ApplicationSet refresh body: err=%v", readErr)
			}
			annotations[argoApplicationSetRefreshAnnotation] = "true"
		case http.MethodGet:
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata",
			"metadata": map[string]any{"name": expectation.Name, "namespace": expectation.Namespace,
				"uid": "75222222-2222-4222-8222-222222222222", "resourceVersion": resourceVersion,
				"labels": map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "kuberploy.io/project-id": expectation.ProjectID,
					"kuberploy.io/environment-id": expectation.EnvironmentID}, "annotations": annotations}})
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	client := &InClusterProductionClient{baseURL: parsed.String(), http: server.Client(), tokenPath: tokenFile}
	if err := client.RefreshEnvironmentApplicationSet(t.Context(), expectation, now); err != nil {
		t.Fatalf("refresh ApplicationSet: %v", err)
	}
	if requests != 2 {
		t.Fatalf("ApplicationSet requests=%d", requests)
	}
	invalid := expectation
	invalid.Name = "attacker-appset"
	if err := client.RefreshEnvironmentApplicationSet(t.Context(), invalid, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary ApplicationSet refresh accepted: %v", err)
	}
}

func TestInClusterProductionClientObservesExactProtectedApplication(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	expectation, err := NewProtectedApplicationExpectation("argocd", "kp-project",
		"https://github.com/kuberploy/gitops.git", strings.Repeat("a", 40), "app-ns",
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444",
		"55555555-5555-4555-8555-555555555555",
		"clusters/11111111-1111-4111-8111-111111111111/helm-manifests/environments/33333333-3333-4333-8333-333333333333/applications/44444444-4444-4444-8444-444444444444/revisions/55555555-5555-4555-8555-555555555555/release.yaml",
		"sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := t.TempDir() + "/token"
	if err = os.WriteFile(tokenFile, []byte("service-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.RawQuery != "" ||
			request.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/"+protectedApplicationName(expectation) {
			t.Errorf("unsafe protected Application request: %s %s", request.Method, request.URL.String())
		}
		labels := protectedApplicationLabels(expectation)
		if requests == 3 {
			labels["unexpected.example/label"] = "not-authorized"
		}
		finalizers := []string{ProtectedApplicationResourcesFinalizer}
		if requests == 2 {
			finalizers = nil
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"name": protectedApplicationName(expectation), "namespace": expectation.Namespace,
				"uid": "66666666-6666-4666-8666-666666666666", "resourceVersion": "101",
				"generation": 9, "managedFields": []any{}, "finalizers": finalizers,
				"labels": labels, "annotations": protectedApplicationAnnotations(expectation)},
			"spec": expectedProtectedApplicationSpec(expectation),
		})
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	client := &InClusterProductionClient{baseURL: parsed.String(), http: server.Client(), tokenPath: tokenFile}
	observation, err := client.ObserveProtectedApplication(t.Context(), expectation, now)
	if err != nil || observation.SpecDigest != expectation.SpecDigest ||
		observation.FinalizerDigest != expectation.FinalizerDigest {
		t.Fatalf("exact observation=%#v err=%v", observation, err)
	}
	if _, err = client.ObserveProtectedApplication(t.Context(), expectation, now); !errors.Is(err, ErrProtectedApplicationNotReady) {
		t.Fatalf("missing finalizer was accepted: %v", err)
	}
	if _, err = client.ObserveProtectedApplication(t.Context(), expectation, now); !errors.Is(err, ErrProtectedApplicationNotReady) {
		t.Fatalf("extra authority label was accepted: %v", err)
	}
}

func TestRootApplicationStrictDecoderRejectsUnknownOrMutableSourceFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	platform, _ := productionBindings(t, now)
	identity := productionIdentity(t, platform)
	head := gitprojection.VerifiedHead{BindingID: platform.ID, Repository: platform.Repository, TargetRef: platform.TargetRef,
		Commit: platform.TargetHeadRevision, Source: gitprojection.ObservationPoll, ProviderRequest: "runtime-strict-head", ObservedAt: now}
	expectation, err := NewPlatformRootApplicationExpectation(identity, platform, head)
	if err != nil {
		t.Fatal(err)
	}
	spec := platformRootApplicationSpec(expectation)
	encoded, _ := json.Marshal(spec)
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	object["sources"] = []any{map[string]any{"repoURL": "https://attacker.invalid/repo.git"}}
	mutated, _ := json.Marshal(object)
	var decoded rootApplicationSpecWire
	if err = decodeStrictJSON(mutated, &decoded); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown multi-source override accepted: %v", err)
	}
	object = map[string]any{}
	_ = json.Unmarshal(encoded, &object)
	source := object["source"].(map[string]any)
	source["targetRevision"] = "refs/heads/attacker"
	mutated, _ = json.Marshal(object)
	if err = decodeStrictJSON(mutated, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Source.TargetRevision == expectation.TargetRevision {
		t.Fatal("mutable source mutation was not represented")
	}
}
