package certissuers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const observerTestUID = "55555555-5555-4555-8555-555555555555"
const observerTestBindingID = "66666666-6666-4666-8666-666666666666"

func observerLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func testObserverConfig() ObserverConfig {
	return ObserverConfig{Enabled: true, BindingID: observerTestBindingID, Namespace: "kuberploy-system", ServiceAccount: "kuberploy-issuer-observer", PollInterval: 30 * time.Second,
		RequestTimeout: 10 * time.Second, MaximumAge: 2 * time.Minute, ReadinessLease: 3 * time.Minute}
}

func TestObserverConfigurationIsStrictDefaultOffAndIdentityExact(t *testing.T) {
	config, err := ObserverConfigFromLookup(observerLookup(nil))
	if err != nil || !reflect.DeepEqual(config, ObserverConfig{}) {
		t.Fatalf("default config=%#v err=%v", config, err)
	}
	if dormant, dormantErr := ObserverConfigFromLookup(observerLookup(map[string]string{
		ObserverEnabledEnv: "false", ObserverNamespaceEnv: "kuberploy-system",
	})); dormantErr != nil || dormant != (ObserverConfig{}) {
		t.Fatalf("disabled observer did not ignore dormant companion: %#v %v", dormant, dormantErr)
	}
	if dormant, dormantErr := ObserverConfigFromLookup(observerLookup(map[string]string{
		ObserverEnabledEnv: "", ObserverPollSecondsEnv: "not-a-number",
	})); dormantErr != nil || dormant != (ObserverConfig{}) {
		t.Fatalf("empty enabled observer did not ignore dormant companion: %#v %v", dormant, dormantErr)
	}
	if _, err = ObserverConfigFromLookup(observerLookup(map[string]string{
		ObserverEnabledEnv: "true", ObserverBindingIDEnv: observerTestBindingID,
	})); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("partial identity accepted: %v", err)
	}
	if _, err = ObserverConfigFromLookup(observerLookup(map[string]string{
		ObserverEnabledEnv: "true", ObserverBindingIDEnv: observerTestBindingID,
		ObserverNamespaceEnv: "kuberploy-system", ObserverServiceAccountEnv: "INVALID SERVICE ACCOUNT",
	})); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("invalid service account accepted: %v", err)
	}
	config, err = ObserverConfigFromLookup(observerLookup(map[string]string{
		ObserverEnabledEnv: "true", ObserverNamespaceEnv: "kuberploy-system", ObserverServiceAccountEnv: "kuberploy-issuer-observer",
		ObserverBindingIDEnv:   observerTestBindingID,
		ObserverPollSecondsEnv: "20", ObserverRequestTimeoutSecondsEnv: "5", ObserverMaximumAgeSecondsEnv: "60",
	}))
	if err != nil || config.Validate() != nil {
		t.Fatalf("enabled config=%#v err=%v", config, err)
	}
	identity, err := ObserverIdentityForConfig(config)
	if err != nil || identity.Validate() != nil {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	changed := config
	changed.MaximumAge++
	if other, otherErr := ObserverIdentityForConfig(changed); !errors.Is(otherErr, ErrObservationUnavailable) || other == identity {
		t.Fatalf("invalid/substituted config produced identity=%#v err=%v", other, otherErr)
	}
	changed = config
	changed.MaximumAge += time.Second
	changed.PollInterval = 20 * time.Second
	if changed.Validate() != nil {
		t.Fatal("test substituted config invalid")
	}
	other, _ := ObserverIdentityForConfig(changed)
	if other == identity {
		t.Fatal("timing substitution did not change identity")
	}
}

func TestClusterIssuerReaderSurfaceAndPathAreExact(t *testing.T) {
	readerType := reflect.TypeOf((*ClusterIssuerReader)(nil)).Elem()
	if readerType.NumMethod() != 1 || readerType.Method(0).Name != "ClusterIssuer" {
		t.Fatalf("reader surface expanded: %#v", readerType)
	}
	valid := "/apis/cert-manager.io/v1/clusterissuers/letsencrypt-production-http"
	if !validClusterIssuerPath(valid) {
		t.Fatal("exact named path rejected")
	}
	for _, path := range []string{
		"/apis/cert-manager.io/v1/clusterissuers", valid + "/status", valid + "?watch=true",
		"/api/v1/secrets/cloudflare",
		"https://attacker.invalid/apis/cert-manager.io/v1/clusterissuers/letsencrypt-production-http",
	} {
		if validClusterIssuerPath(path) {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}

func TestInClusterClusterIssuerReaderUsesExactBoundedGETAndRecomputesHTTP01(t *testing.T) {
	clean, _, digest, err := normalizeSpec(httpSpec())
	if err != nil {
		t.Fatal(err)
	}
	name := "letsencrypt-production-http"
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/apis/cert-manager.io/v1/clusterissuers/"+name || request.URL.RawQuery != "" ||
			request.Header.Get("Authorization") != "Bearer exact-observer-token" || request.Header.Get("Accept") != "application/json" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(httpIssuerObject(name, digest, 1, 7, true)))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(tokenPath, []byte("exact-observer-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := newClusterIssuerReader(server.URL, server.Client(), tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.ClusterIssuer(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != name || snapshot.Generation != 7 || snapshot.ReadyObservedGeneration != 7 || !snapshot.Ready ||
		snapshot.SpecDigest != digest || snapshot.AnnotatedSpecDigest != digest || snapshot.AnnotatedRevision != 1 ||
		snapshot.Solver != HTTP01 || snapshot.Spec.ACME != clean.ACME || snapshot.Spec.HTTP01 == nil {
		t.Fatalf("snapshot drifted: %#v", snapshot)
	}
	before := requests.Load()
	if _, err = reader.ClusterIssuer(context.Background(), "INVALID/NAME"); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("invalid name accepted: %v", err)
	}
	if requests.Load() != before {
		t.Fatal("invalid name reached Kubernetes")
	}
}

func TestClusterIssuerReaderRejectsRedirectsUnknownFieldsAndDuplicateReady(t *testing.T) {
	name := "letsencrypt-production-http"
	_, _, digest, _ := normalizeSpec(httpSpec())
	targetRequests := atomic.Int32{}
	target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = writer.Write([]byte(httpIssuerObject(name, digest, 1, 1, true)))
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL+"/apis/cert-manager.io/v1/clusterissuers/"+name)
		writer.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := newClusterIssuerReader(redirect.URL, redirect.Client(), tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ClusterIssuer(context.Background(), name); !errors.Is(err, ErrObservationUnavailable) || targetRequests.Load() != 0 {
		t.Fatalf("redirect followed: target=%d err=%v", targetRequests.Load(), err)
	}

	for _, mutate := range []func(string) string{
		func(value string) string {
			return strings.Replace(value, `"solvers":[{`, `"solvers":[{"arbitrary":{"secret":"bytes"},`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"conditions":[`, `"conditions":[{"type":"Ready","status":"False","observedGeneration":1},`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"ingressClassName":"traefik"`, `"ingressClassName":"nginx"`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"annotation-only"`, `"true"`, 1)
		},
		func(value string) string {
			return strings.Replace(value, `"admin@example.com"`, `"Admin@Example.com"`, 1)
		},
	} {
		object := mutate(httpIssuerObject(name, digest, 1, 1, true))
		server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(object)) }))
		client, createErr := newClusterIssuerReader(server.URL, server.Client(), tokenPath)
		if createErr != nil {
			server.Close()
			t.Fatal(createErr)
		}
		_, observeErr := client.ClusterIssuer(context.Background(), name)
		server.Close()
		if !errors.Is(observeErr, ErrObservationUnavailable) {
			t.Fatalf("unsafe live object accepted: %v", observeErr)
		}
	}
}

func TestClusterIssuerReaderRecomputesCloudflareDNS01(t *testing.T) {
	desired := dnsSpec("example.com", "services.example.net")
	clean, solver, digest, err := normalizeSpec(desired)
	if err != nil {
		t.Fatal(err)
	}
	name := "letsencrypt-staging-cloudflare"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(dnsIssuerObject(name, digest, 8, 19, true)))
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := newClusterIssuerReader(server.URL, server.Client(), tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.ClusterIssuer(context.Background(), name)
	if err != nil || snapshot.SpecDigest != digest || snapshot.Solver != solver || !reflect.DeepEqual(snapshot.Spec, clean) {
		t.Fatalf("DNS snapshot=%#v err=%v", snapshot, err)
	}
	unsorted := strings.Replace(dnsIssuerObject(name, digest, 8, 19, true),
		`["example.com","services.example.net"]`, `["services.example.net","example.com"]`, 1)
	badServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(unsorted)) }))
	defer badServer.Close()
	badReader, err := newClusterIssuerReader(badServer.URL, badServer.Client(), tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = badReader.ClusterIssuer(context.Background(), name); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("non-canonical DNS zone order accepted: %v", err)
	}
}

type fixedClusterIssuerReader struct {
	snapshot ClusterIssuerSnapshot
	err      error
	calls    atomic.Int32
}

func (r *fixedClusterIssuerReader) ClusterIssuer(_ context.Context, _ string) (ClusterIssuerSnapshot, error) {
	r.calls.Add(1)
	return r.snapshot, r.err
}

func TestObserverRuntimeRecordsExactReadinessAndFailsClosedOnDrift(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	store := NewMemoryStore()
	created, err := store.Create(ctx, command("observer-runtime", now), "letsencrypt-production-http", httpSpec())
	if err != nil {
		t.Fatal(err)
	}
	config := testObserverConfig()
	identity, _ := ObserverIdentityForConfig(config)
	reader := &fixedClusterIssuerReader{snapshot: ClusterIssuerSnapshot{Name: created.Profile.Name, UID: observerTestUID, ResourceVersion: "7",
		AnnotatedSpecDigest: created.Revision.SpecDigest, AnnotatedRevision: 1, Generation: 2, ReadyObservedGeneration: 2,
		Ready: true, SpecDigest: created.Revision.SpecDigest, Solver: created.Revision.Solver, Spec: created.Revision.Spec}}
	current := now.Add(time.Second)
	runtime := &ObserverRuntime{Store: store, ReadinessStore: NewMemoryObserverReadinessStore(), Reader: reader, Config: config,
		Identity: identity, WorkerID: "issuer-observer:test", StartedAt: now, Now: func() time.Time { return current }}
	if err = runtime.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Probe(ctx); err != nil {
		t.Fatalf("fresh exact runtime not ready: %v", err)
	}
	observation, err := store.Observation(ctx, created.Profile.ID, 1)
	if err != nil || observation.State != Ready || observation.ObservedSpecDigest != created.Revision.SpecDigest || observation.ObservedGeneration != 2 {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}

	current = now.Add(config.MaximumAge + 2*time.Second)
	if err = runtime.Probe(ctx); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("stale readiness accepted: %v", err)
	}
	current = now
	if err = runtime.Probe(ctx); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("future readiness accepted: %v", err)
	}
	current = now.Add(2 * time.Second)
	revised, err := store.Revise(ctx, command("observer-revise", current), Ref{ProfileID: created.Profile.ID, Revision: 1}, httpSpec())
	if err != nil || revised.Revision.Revision != 2 {
		t.Fatalf("revise=%#v err=%v", revised, err)
	}
	if err = runtime.Probe(ctx); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("target revision substitution accepted: %v", err)
	}
}

func TestObserverRuntimeRecordsDegradedForLiveSubstitution(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	store := NewMemoryStore()
	created, err := store.Create(ctx, command("observer-degraded", now), "letsencrypt-production-http", httpSpec())
	if err != nil {
		t.Fatal(err)
	}
	config := testObserverConfig()
	identity, _ := ObserverIdentityForConfig(config)
	reader := &fixedClusterIssuerReader{snapshot: ClusterIssuerSnapshot{Name: created.Profile.Name, UID: observerTestUID, ResourceVersion: "7",
		AnnotatedSpecDigest: created.Revision.SpecDigest, AnnotatedRevision: 99, Generation: 2, ReadyObservedGeneration: 2,
		Ready: true, SpecDigest: created.Revision.SpecDigest, Solver: created.Revision.Solver, Spec: created.Revision.Spec}}
	runtime := &ObserverRuntime{Store: store, ReadinessStore: NewMemoryObserverReadinessStore(), Reader: reader, Config: config,
		Identity: identity, WorkerID: "issuer-observer:test", StartedAt: now, Now: func() time.Time { return now.Add(time.Second) }}
	if err = runtime.RunOnce(ctx); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("substitution accepted: %v", err)
	}
	observation, err := store.Observation(ctx, created.Profile.ID, 1)
	if err != nil || observation.State != Degraded || observation.Reason != "revision-annotation-mismatch" {
		t.Fatalf("degraded observation=%#v err=%v", observation, err)
	}
	if err = runtime.Probe(ctx); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("degraded runtime reported ready: %v", err)
	}
}

func TestObserverRuntimeDiscoversDynamicActiveCatalogName(t *testing.T) {
	store := NewMemoryStore()
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := store.Create(context.Background(), command("unknown-active", now), "not-configured", httpSpec()); err != nil {
		t.Fatal(err)
	}
	config := testObserverConfig()
	identity, _ := ObserverIdentityForConfig(config)
	entry, _ := store.List(context.Background(), 20)
	reader := &fixedClusterIssuerReader{snapshot: ClusterIssuerSnapshot{Name: "not-configured", UID: observerTestUID, ResourceVersion: "1",
		AnnotatedSpecDigest: entry[0].Revision.SpecDigest, AnnotatedRevision: 1, Generation: 1, ReadyObservedGeneration: 1,
		Ready: true, SpecDigest: entry[0].Revision.SpecDigest, Solver: entry[0].Revision.Solver, Spec: entry[0].Revision.Spec}}
	runtime := &ObserverRuntime{Store: store, ReadinessStore: NewMemoryObserverReadinessStore(), Reader: reader, Config: config,
		Identity: identity, WorkerID: "issuer-observer:test", StartedAt: now, Now: func() time.Time { return now }}
	if err := runtime.RunOnce(context.Background()); err != nil || reader.calls.Load() != 1 {
		t.Fatalf("dynamic active profile not observed: calls=%d err=%v", reader.calls.Load(), err)
	}
}

func TestMemoryObserverReadinessFencesActiveAndOlderWorkers(t *testing.T) {
	config := testObserverConfig()
	identity, _ := ObserverIdentityForConfig(config)
	now := time.Unix(1_700_000_000, 0).UTC()
	store := NewMemoryObserverReadinessStore()
	first := ObserverWorkerObservation{WorkerID: "issuer-observer:first", Identity: identity,
		TargetDigest: "sha256:" + strings.Repeat("1", 64), StartedAt: now, ObservedAt: now}
	lease, err := store.AcquireObserverReadiness(context.Background(), first, config.ReadinessLease)
	if err != nil || lease.Epoch != 1 {
		t.Fatalf("first lease=%#v err=%v", lease, err)
	}
	second := first
	second.WorkerID = "issuer-observer:second"
	second.StartedAt = now.Add(time.Second)
	second.ObservedAt = now.Add(time.Second)
	if _, err = store.AcquireObserverReadiness(context.Background(), second, config.ReadinessLease); !errors.Is(err, ErrObserverLeaseLost) {
		t.Fatalf("active lease stolen: %v", err)
	}
	second.ObservedAt = lease.Until
	second.StartedAt = now.Add(2 * time.Second)
	secondLease, err := store.AcquireObserverReadiness(context.Background(), second, config.ReadinessLease)
	if err != nil || secondLease.Epoch != 2 {
		t.Fatalf("expired lease not reclaimed: %#v err=%v", secondLease, err)
	}
	older := first
	older.ObservedAt = secondLease.Until
	if _, err = store.AcquireObserverReadiness(context.Background(), older, config.ReadinessLease); !errors.Is(err, ErrObserverLeaseLost) {
		t.Fatalf("older worker reclaimed lease: %v", err)
	}
	first.ObservedAt = secondLease.ObservedAt.Add(time.Second)
	if _, err = store.HeartbeatObserverReadiness(context.Background(), lease, first, config.ReadinessLease); !errors.Is(err, ErrObserverLeaseLost) {
		t.Fatalf("fenced epoch heartbeat accepted: %v", err)
	}
}

func httpIssuerObject(name, digest string, revision, generation int64, ready bool) string {
	status := "False"
	if ready {
		status = "True"
	}
	return fmt.Sprintf(`{"apiVersion":"cert-manager.io/v1","kind":"ClusterIssuer","metadata":{"name":%q,"uid":%q,"resourceVersion":"7","generation":%d,"labels":{"app.kubernetes.io/managed-by":"kuberploy","kuberploy.io/certificate-issuer-profile":%q},"annotations":{"kuberploy.io/certificate-issuer-spec-digest":%q,"kuberploy.io/certificate-issuer-revision":%q}},"spec":{"acme":{"email":"admin@example.com","server":"https://acme-v02.api.letsencrypt.org/directory","privateKeySecretRef":{"name":"letsencrypt-account"},"solvers":[{"http01":{"ingress":{"ingressClassName":"traefik","ingressTemplate":{"metadata":{"annotations":{"external-dns.alpha.kubernetes.io/exclude":"true","external-dns.alpha.kubernetes.io/ingress-hostname-source":"annotation-only"}}}}}}]}},"status":{"conditions":[{"type":"Ready","status":%q,"observedGeneration":%d,"reason":"ACMEAccountRegistered","message":"ready"}]}}`,
		name, observerTestUID, generation, name, digest, fmt.Sprintf("%d", revision), status, generation)
}

func dnsIssuerObject(name, digest string, revision, generation int64, ready bool) string {
	status := "False"
	if ready {
		status = "True"
	}
	return fmt.Sprintf(`{"apiVersion":"cert-manager.io/v1","kind":"ClusterIssuer","metadata":{"name":%q,"uid":%q,"resourceVersion":"9","generation":%d,"labels":{"app.kubernetes.io/managed-by":"kuberploy","kuberploy.io/certificate-issuer-profile":%q},"annotations":{"kuberploy.io/certificate-issuer-spec-digest":%q,"kuberploy.io/certificate-issuer-revision":%q}},"spec":{"acme":{"email":"admin@example.com","server":"https://acme-staging-v02.api.letsencrypt.org/directory","privateKeySecretRef":{"name":"letsencrypt-dns-account"},"solvers":[{"selector":{"dnsZones":["example.com","services.example.net"]},"dns01":{"cloudflare":{"apiTokenSecretRef":{"name":"cloudflare-token","key":"api-token"}}}}]}},"status":{"conditions":[{"type":"Ready","status":%q,"observedGeneration":%d}]}}`,
		name, observerTestUID, generation, name, digest, fmt.Sprintf("%d", revision), status, generation)
}
