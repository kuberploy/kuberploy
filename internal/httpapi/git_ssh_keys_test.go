package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitssh"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type gitSSHHTTPBackend struct {
	mu       sync.Mutex
	requests []gitssh.MutationRequest
	receipts map[string]gitssh.MutationResult
	records  map[string][]gitssh.KeyMetadata
}

func newGitSSHHTTPBackend() *gitSSHHTTPBackend {
	return &gitSSHHTTPBackend{receipts: map[string]gitssh.MutationResult{}, records: map[string][]gitssh.KeyMetadata{}}
}

func (b *gitSSHHTTPBackend) Mutate(_ context.Context, request gitssh.MutationRequest) (gitssh.MutationResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if result, ok := b.receipts[request.IdempotencyKey]; ok {
		result.Replay = true
		return result, nil
	}
	b.requests = append(b.requests, request)
	key := string(request.Scope) + "\x00" + request.OwnerID
	revision := uint64(len(b.records[key]) + 1)
	if request.Operation == gitssh.OperationRotate || request.Operation == gitssh.OperationRevoke {
		if len(b.records[key]) == 0 {
			return gitssh.MutationResult{}, gitssh.ErrActiveKeyNotFound
		}
		b.records[key][len(b.records[key])-1].Status = gitssh.StatusRevoked
	}
	value := gitssh.KeyMetadata{Scope: request.Scope, OwnerID: request.OwnerID, Revision: revision, Status: gitssh.StatusActive,
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPublicKey", Fingerprint: "SHA256:test-public-fingerprint"}
	if request.Operation == gitssh.OperationRevoke {
		value = b.records[key][len(b.records[key])-1]
	} else {
		b.records[key] = append(b.records[key], value)
	}
	result := gitssh.MutationResult{Value: value}
	b.receipts[request.IdempotencyKey] = result
	return result, nil
}

func (b *gitSSHHTTPBackend) List(_ context.Context, scope gitssh.Scope, ownerID string) ([]gitssh.KeyMetadata, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]gitssh.KeyMetadata(nil), b.records[string(scope)+"\x00"+ownerID]...), nil
}

func newGitSSHAPI(t *testing.T) (*apiFixture, *gitSSHHTTPBackend) {
	t.Helper()
	central := memory.New()
	backend := newGitSSHHTTPBackend()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: central, BootstrapToken: "one-time-secret", Version: "test",
		GitSSHKeys: backend, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: central}
	t.Cleanup(server.Close)
	return fixture, backend
}

func TestGitSSHKeyHTTPLifecycleIsAuthorizedIdempotentAndPublicOnly(t *testing.T) {
	fixture, backend := newGitSSHAPI(t)
	unauthenticated := fixture.request(http.MethodGet, "/v1/projects/11111111-1111-4111-8111-111111111111/git-ssh-keys", "", nil)
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.StatusCode)
	}
	unauthenticated.Body.Close()
	fixture.bootstrap()

	response := fixture.request(http.MethodPost, "/v1/projects", "git-ssh-http-project", map[string]string{"name": "Git SSH", "slug": "git-ssh"})
	project := decode[domain.Project](t, response)
	response = fixture.request(http.MethodPost, "/v1/applications", "git-ssh-http-app", map[string]string{
		"projectId": project.ID, "name": "Private source", "slug": "private-source",
	})
	application := decode[domain.Application](t, response)

	path := "/v1/applications/" + application.ID + "/git-ssh-keys"
	missingKey := fixture.request(http.MethodPost, path, "", nil)
	problem := decode[httpapi.Problem](t, missingKey)
	if missingKey.StatusCode != http.StatusBadRequest || problem.Code != "IdempotencyKeyRequired" {
		t.Fatalf("missing idempotency status=%d problem=%#v", missingKey.StatusCode, problem)
	}

	response = fixture.request(http.MethodPost, path, "git-ssh-http-create", nil)
	createdBody := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Location") != "" || createdBody["ownerId"] != application.ID {
		t.Fatalf("create status=%d location=%q body=%#v", response.StatusCode, response.Header.Get("Location"), createdBody)
	}
	assertGitSSHHTTPPublicOnly(t, createdBody)

	response = fixture.request(http.MethodPost, path, "git-ssh-http-create", nil)
	replayedBody := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotent-Replay") != "true" || replayedBody["publicKey"] != createdBody["publicKey"] || len(backend.requests) != 1 {
		t.Fatalf("replay status=%d header=%q calls=%d body=%#v", response.StatusCode, response.Header.Get("Idempotent-Replay"), len(backend.requests), replayedBody)
	}

	response = fixture.request(http.MethodPost, path+"/rotate", "git-ssh-http-rotate", nil)
	rotated := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || rotated["revision"] != float64(2) {
		t.Fatalf("rotate status=%d body=%#v", response.StatusCode, rotated)
	}
	assertGitSSHHTTPPublicOnly(t, rotated)

	response = fixture.request(http.MethodGet, path, "", nil)
	listed := decode[struct {
		Items []map[string]any `json:"items"`
	}](t, response)
	if response.StatusCode != http.StatusOK || len(listed.Items) != 2 || listed.Items[0]["status"] != string(gitssh.StatusRevoked) || listed.Items[1]["status"] != string(gitssh.StatusActive) {
		t.Fatalf("list status=%d items=%#v", response.StatusCode, listed.Items)
	}
	for _, item := range listed.Items {
		assertGitSSHHTTPPublicOnly(t, item)
	}

	response = fixture.request(http.MethodDelete, path+"/active", "git-ssh-http-revoke", nil)
	revoked := decode[map[string]any](t, response)
	if response.StatusCode != http.StatusOK || revoked["status"] != string(gitssh.StatusRevoked) {
		t.Fatalf("revoke status=%d body=%#v", response.StatusCode, revoked)
	}
}

func TestGitSSHKeyHTTPHidesMissingOwnersAndUnavailableBackend(t *testing.T) {
	fixture, _ := newGitSSHAPI(t)
	fixture.bootstrap()
	response := fixture.request(http.MethodGet, "/v1/projects/11111111-1111-4111-8111-111111111111/git-ssh-keys", "", nil)
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusNotFound || problem.Code != "NotFound" {
		t.Fatalf("missing owner status=%d problem=%#v", response.StatusCode, problem)
	}

	unavailable := newAPI(t)
	unavailable.bootstrap()
	response = unavailable.request(http.MethodGet, "/v1/projects/11111111-1111-4111-8111-111111111111/git-ssh-keys", "", nil)
	problem = decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "GitSSHUnavailable" {
		t.Fatalf("unavailable status=%d problem=%#v", response.StatusCode, problem)
	}
}

func assertGitSSHHTTPPublicOnly(t *testing.T, value map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"private", "ciphertext", "envelope", "keyversion"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("Git SSH HTTP response exposes forbidden field %q: %s", forbidden, encoded)
		}
	}
}
