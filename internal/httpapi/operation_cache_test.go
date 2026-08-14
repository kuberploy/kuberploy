package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/operationcache"
	"github.com/kuberploy/kuberploy/internal/ratelimit"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type cacheIdentityStore struct {
	store.Store
	grantRevision int64
	identities    int
}

func (s *cacheIdentityStore) AuthorizedImageSourcesForActor(ctx context.Context, actor, applicationID, environmentID string) ([]imageresolution.AuthorizedSource, error) {
	catalog, ok := s.Store.(imageresolution.Catalog)
	if !ok {
		return nil, imageresolution.ErrUnavailable
	}
	return catalog.AuthorizedImageSourcesForActor(ctx, actor, applicationID, environmentID)
}

func (s *cacheIdentityStore) OperationCacheIdentityForActor(ctx context.Context, actor, operationID string) (operationcache.Identity, error) {
	s.identities++
	operation, err := s.Store.GetOperationForActor(ctx, actor, operationID)
	if err != nil {
		return operationcache.Identity{}, err
	}
	return operationcache.NewIdentity(actor, s.grantRevision, operation.ID, operation.Generation, operation.UpdatedAt)
}

type fakeOperationCache struct {
	loaded domain.Operation
	hit    bool
	err    error
	stores int
}

func (c *fakeOperationCache) Load(context.Context, operationcache.Identity) (domain.Operation, bool, error) {
	return c.loaded, c.hit, c.err
}

func (c *fakeOperationCache) Store(_ context.Context, _ operationcache.Identity, operation domain.Operation) error {
	c.stores++
	c.loaded, c.hit = operation, true
	return c.err
}

func cachedOperationAPI(t *testing.T, cache operationcache.Cache) (*apiFixture, *cacheIdentityStore) {
	t.Helper()
	memoryStore := memory.New()
	wrapped := &cacheIdentityStore{Store: memoryStore, grantRevision: 1}
	server := httptest.NewServer(httpapi.New(httpapi.Options{Store: wrapped, BootstrapToken: "one-time-secret", Version: "test",
		OperationCache: cache, HighRiskLimiter: ratelimit.NewMemoryLimiter(10_000)}))
	jar, _ := cookiejar.New(nil)
	fixture := &apiFixture{t: t, server: server, client: &http.Client{Jar: jar}, store: memoryStore}
	t.Cleanup(server.Close)
	return fixture, wrapped
}

func createCacheableOperation(t *testing.T, fixture *apiFixture) domain.Operation {
	t.Helper()
	fixture.bootstrap()
	response := fixture.request(http.MethodPost, "/v1/projects", "cache-project", map[string]string{"name": "Cache project"})
	project := decode[domain.Project](t, response)
	response = fixture.request(http.MethodPost, "/v1/environments", "cache-environment", map[string]string{"projectId": project.ID, "name": "Cache environment"})
	environment := decode[domain.Environment](t, response)
	response = fixture.request(http.MethodPost, "/v1/applications", "cache-application", map[string]string{"projectId": project.ID, "name": "Cache application"})
	application := decode[domain.Application](t, response)
	response = fixture.request(http.MethodPost, "/v1/deployments", "cache-deployment", map[string]any{
		"environmentId": environment.ID, "applicationId": application.ID,
		"image": "registry.example/cache@sha256:" + strings.Repeat("a", 64), "replicas": 1, "port": 8080,
	})
	operation := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create deployment status=%d operation=%#v", response.StatusCode, operation)
	}
	return operation
}

func TestOperationCacheFailureAndPoisoningFallBackToAuthoritativeStore(t *testing.T) {
	cache := &fakeOperationCache{err: errors.New("Valkey unavailable")}
	fixture, identityStore := cachedOperationAPI(t, cache)
	operation := createCacheableOperation(t, fixture)

	response := fixture.request(http.MethodGet, "/v1/operations/"+operation.ID, "", nil)
	got := decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusOK || got.ID != operation.ID || identityStore.identities != 1 || cache.stores != 1 {
		t.Fatalf("fallback status=%d got=%#v identities=%d stores=%d", response.StatusCode, got, identityStore.identities, cache.stores)
	}

	cache.err = nil
	cache.hit = true
	cache.loaded = got
	cache.loaded.UpdatedAt = cache.loaded.UpdatedAt.Add(1)
	poisonedUpdatedAt := cache.loaded.UpdatedAt
	response = fixture.request(http.MethodGet, "/v1/operations/"+operation.ID, "", nil)
	got = decode[domain.Operation](t, response)
	if response.StatusCode != http.StatusOK || got.UpdatedAt.Equal(poisonedUpdatedAt) || cache.stores != 2 {
		t.Fatalf("poison fallback status=%d got=%#v poisoned=%#v stores=%d", response.StatusCode, got, cache.loaded, cache.stores)
	}
}
