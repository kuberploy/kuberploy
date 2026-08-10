package operationcache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	testActor     = "018f3f7e-4f18-7c8d-8f5c-5d8f6a9b2c31"
	testOperation = "018f3f7e-5a29-7d9e-9a6d-6e9f7b0c3d42"
	testTarget    = "018f3f7e-6b3a-7eaf-aa7e-7f0a8c1d4e53"
)

func cacheFixture(t *testing.T) (Identity, domain.Operation, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 9, 8, 0, 0, 123, time.UTC)
	operation := domain.Operation{ID: testOperation, Kind: "deployment.git-write", Status: "running", TargetType: "deployment", TargetID: testTarget,
		RequestID: "018f3f7e-7c4b-7fb0-bb8f-8a1b9d2e5f64", Generation: 4, Progress: []domain.ProgressStep{{Name: "git-write", Status: "running"}}, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	identity, err := NewIdentity(testActor, 7, operation.ID, operation.Generation, operation.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	return identity, operation, now
}

func TestEnvelopeRejectsWrongScopeRevisionExtensionAndOversize(t *testing.T) {
	identity, operation, now := cacheFixture(t)
	encoded, err := encodeEnvelope(identity, operation, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if value, decodeErr := decodeEnvelope(encoded, identity, now.Add(time.Second), 30*time.Second); decodeErr != nil || value.ID != operation.ID {
		t.Fatalf("valid envelope value=%#v err=%v", value, decodeErr)
	}

	wrongScope := identity
	wrongScope.ScopeHash = strings.Repeat("a", 64)
	if _, err = decodeEnvelope(encoded, wrongScope, now.Add(time.Second), 30*time.Second); err == nil {
		t.Fatal("wrong-scope cache entry was accepted")
	}
	wrongRevision := identity
	wrongRevision.SourceRevision = "sha256:" + strings.Repeat("b", 64)
	if _, err = decodeEnvelope(encoded, wrongRevision, now.Add(time.Second), 30*time.Second); err == nil {
		t.Fatal("wrong-revision cache entry was accepted")
	}
	var extended map[string]any
	if err = json.Unmarshal(encoded, &extended); err != nil {
		t.Fatal(err)
	}
	extended["draftManifest"] = "kind: Secret"
	encoded, _ = json.Marshal(extended)
	if _, err = decodeEnvelope(encoded, identity, now.Add(time.Second), 30*time.Second); err == nil {
		t.Fatal("extended cache envelope was accepted")
	}
	if _, err = decodeEnvelope(bytes.Repeat([]byte("x"), maximumValue+1), identity, now, 30*time.Second); err == nil {
		t.Fatal("oversized cache envelope was accepted")
	}
	operation.Problem = &domain.ProblemData{Code: "Unsafe", Detail: strings.Repeat("manifest", maximumValue)}
	if _, err = encodeEnvelope(identity, operation, now, 30*time.Second); err == nil {
		t.Fatal("worker problem detail was accepted")
	}
	operation.Problem = nil
	operation.RequestID = "bearer-user-controlled-value"
	if _, err = encodeEnvelope(identity, operation, now, 30*time.Second); err == nil {
		t.Fatal("arbitrary request metadata was accepted")
	}
}

func TestEnvelopeRejectsExpiredFutureAndRacedBody(t *testing.T) {
	identity, operation, now := cacheFixture(t)
	encoded, err := encodeEnvelope(identity, operation, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeEnvelope(encoded, identity, now.Add(31*time.Second), 30*time.Second); err == nil {
		t.Fatal("expired cache envelope was accepted")
	}
	if _, err = decodeEnvelope(encoded, identity, now.Add(-2*time.Minute), 30*time.Second); err == nil {
		t.Fatal("future cache envelope was accepted")
	}
	operation.UpdatedAt = operation.UpdatedAt.Add(time.Nanosecond)
	if identity.MatchesOperation(operation) {
		t.Fatal("body from a different PostgreSQL row revision matched")
	}
}

func TestOptionsFailClosed(t *testing.T) {
	if _, err := NewValkeyCache(Options{}); err == nil {
		t.Fatal("empty addresses were accepted")
	}
	if _, err := NewValkeyCache(Options{Addresses: []string{"bad\naddress"}}); err == nil {
		t.Fatal("invalid address was accepted")
	}
	if _, err := NewValkeyCache(Options{Addresses: []string{"127.0.0.1:6379"}, TTL: 3 * time.Minute}); err == nil {
		t.Fatal("unbounded TTL was accepted")
	}
}

func TestValkeyCacheAndHintIntegration(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("KUBERPLOY_TEST_VALKEY_ADDRESS"))
	if address == "" {
		t.Skip("KUBERPLOY_TEST_VALKEY_ADDRESS is not set")
	}
	cache, err := NewValkeyCache(Options{Addresses: []string{address}, TTL: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	identity, operation, now := cacheFixture(t)
	cache.now = func() time.Time { return now }
	ctx := context.Background()
	t.Cleanup(func() { _ = cache.client.Do(ctx, cache.client.B().Del().Key(identity.key()).Build()).Error() })
	if err = cache.Store(ctx, identity, operation); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := cache.Load(ctx, identity)
	if err != nil || !ok || loaded.ID != operation.ID {
		t.Fatalf("loaded=%#v ok=%t err=%v", loaded, ok, err)
	}
	if err = cache.client.Do(ctx, cache.client.B().Set().Key(identity.key()).Value(`{"schema":"corrupt"}`).Ex(time.Minute).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = cache.Load(ctx, identity); err == nil || ok {
		t.Fatalf("corrupt entry ok=%t err=%v", ok, err)
	}
	if exists, existsErr := cache.client.Do(ctx, cache.client.B().Exists().Key(identity.key()).Build()).AsInt64(); existsErr != nil || exists != 0 {
		t.Fatalf("corrupt entry was not discarded exists=%d err=%v", exists, existsErr)
	}
}
