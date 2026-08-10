package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/store"
)

const (
	defaultRegistryInventoryLimit = 50
	maximumRegistryInventoryLimit = 100
	maximumRegistryCleanupItems   = 100
)

type RegistryManagementService interface {
	Targets(context.Context, string) ([]domain.RegistryTarget, error)
	CreateTarget(context.Context, string, string, string, string, registry.RegistryTargetInput) (store.Result[domain.RegistryTarget], error)
	UpdateTarget(context.Context, string, string, string, string, string, registry.RegistryTargetInput) (store.Result[domain.RegistryTarget], error)
	ApplicationInventory(context.Context, string, string) ([]domain.RegistryLifecycleSnapshot, error)
	PutPolicy(context.Context, string, string, string, string, string, string, registry.ServicePolicyInput) (store.Result[domain.ServiceRegistryPolicy], error)
	PreviewCleanup(context.Context, string, string, string, string, string, string) (store.Result[domain.RegistryCleanupPlan], error)
	CleanupPlan(context.Context, string, string) (domain.RegistryCleanupPlan, error)
	ExecuteCleanup(context.Context, string, string, string, string, string, string, string) (store.Result[domain.RegistryCleanupPlan], error)
}

type registryHTTP struct {
	service   RegistryManagementService
	readiness ReadinessProbe
}

func newRegistryHTTP(service RegistryManagementService, readiness ReadinessProbe) *registryHTTP {
	return &registryHTTP{service: service, readiness: readiness}
}

func (h *registryHTTP) runtimeReady(ctx context.Context) bool {
	if h == nil || h.service == nil || h.readiness == nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.readiness.Probe(probeContext) == nil
}

type registryTargetRequest struct {
	Name               string                    `json:"name"`
	Mode               domain.RegistryTargetMode `json:"mode"`
	Endpoint           string                    `json:"endpoint"`
	RepositoryPrefix   string                    `json:"repositoryPrefix"`
	PullCredentialRef  string                    `json:"pullCredentialRef,omitempty"`
	PushCredentialRef  string                    `json:"pushCredentialRef,omitempty"`
	CacheCredentialRef string                    `json:"cacheCredentialRef,omitempty"`
}

type registryTargetView struct {
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	Mode               domain.RegistryTargetMode `json:"mode"`
	Endpoint           string                    `json:"endpoint"`
	RepositoryPrefix   string                    `json:"repositoryPrefix"`
	PullCredentialRef  string                    `json:"pullCredentialRef,omitempty"`
	PushCredentialRef  string                    `json:"pushCredentialRef,omitempty"`
	CacheCredentialRef string                    `json:"cacheCredentialRef,omitempty"`
	CreatedAt          time.Time                 `json:"createdAt"`
	UpdatedAt          time.Time                 `json:"updatedAt"`
}

func safeRegistryTarget(target domain.RegistryTarget) registryTargetView {
	return registryTargetView{
		ID: target.ID, Name: target.Name, Mode: target.Mode, Endpoint: target.Endpoint,
		RepositoryPrefix: target.RepositoryPrefix, PullCredentialRef: target.PullCredentialRef,
		PushCredentialRef: target.PushCredentialRef, CacheCredentialRef: target.CacheCredentialRef,
		CreatedAt: target.CreatedAt, UpdatedAt: target.UpdatedAt,
	}
}

func (input registryTargetRequest) serviceInput() registry.RegistryTargetInput {
	return registry.RegistryTargetInput{
		Name: input.Name, Mode: input.Mode, Endpoint: input.Endpoint, RepositoryPrefix: input.RepositoryPrefix,
		PullCredentialRef: input.PullCredentialRef, PushCredentialRef: input.PushCredentialRef,
		CacheCredentialRef: input.CacheCredentialRef,
	}
}

func registryResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Pragma", "no-cache")
}

func registryNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryResponseHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func (h *registryHTTP) targets(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if h == nil || h.service == nil {
		registryManagementUnavailable(w, r)
		return
	}
	actor := currentUser(r.Context()).ID
	if r.Method == http.MethodGet {
		limit, ok := registryInventoryLimit(w, r)
		if !ok {
			return
		}
		targets, err := h.service.Targets(r.Context(), actor)
		if err != nil {
			mappedRegistryError(w, r, err)
			return
		}
		truncated := len(targets) > limit
		if truncated {
			targets = targets[:limit]
		}
		items := make([]registryTargetView, 0, len(targets))
		for _, target := range targets {
			items = append(items, safeRegistryTarget(target))
		}
		writeJSON(w, http.StatusOK, struct {
			Items     []registryTargetView `json:"items"`
			Truncated bool                 `json:"truncated"`
		}{Items: items, Truncated: truncated})
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input registryTargetRequest
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.CreateTarget(r.Context(), actor, key, fingerprint(input), requestID(r.Context()), input.serviceInput())
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/registry-targets/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, safeRegistryTarget(result.Value))
}

func (h *registryHTTP) updateTarget(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if h == nil || h.service == nil {
		registryManagementUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input registryTargetRequest
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.UpdateTarget(r.Context(), currentUser(r.Context()).ID, key, fingerprint(input), requestID(r.Context()), strings.TrimSpace(r.PathValue("id")), input.serviceInput())
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeRegistryTarget(result.Value))
}

type registryPolicyRequest struct {
	Repository               string `json:"repository"`
	KeepLastSuccessful       int    `json:"keepLastSuccessful,omitempty"`
	MinimumSafetyAgeSeconds  int64  `json:"minimumSafetyAgeSeconds,omitempty"`
	CacheKeepGenerations     int    `json:"cacheKeepGenerations,omitempty"`
	CacheUnusedExpirySeconds int64  `json:"cacheUnusedExpirySeconds,omitempty"`
	CacheByteQuota           int64  `json:"cacheByteQuota,omitempty"`
}

type registryPolicyView struct {
	RegistryTargetID         string    `json:"registryTargetId"`
	ServiceID                string    `json:"serviceId"`
	Repository               string    `json:"repository"`
	KeepLastSuccessful       int       `json:"keepLastSuccessful"`
	MinimumSafetyAgeSeconds  int64     `json:"minimumSafetyAgeSeconds"`
	CacheKeepGenerations     int       `json:"cacheKeepGenerations"`
	CacheUnusedExpirySeconds int64     `json:"cacheUnusedExpirySeconds"`
	CacheByteQuota           int64     `json:"cacheByteQuota"`
	CreatedAt                time.Time `json:"createdAt"`
	UpdatedAt                time.Time `json:"updatedAt"`
}

func safeRegistryPolicy(policy domain.ServiceRegistryPolicy) registryPolicyView {
	return registryPolicyView{
		RegistryTargetID: policy.RegistryTargetID, ServiceID: policy.ServiceID,
		Repository: policy.Repository, KeepLastSuccessful: policy.KeepLastSuccessful,
		MinimumSafetyAgeSeconds:  int64(policy.MinimumSafetyAge / time.Second),
		CacheKeepGenerations:     policy.CacheKeepGenerations,
		CacheUnusedExpirySeconds: int64(policy.CacheUnusedExpiry / time.Second),
		CacheByteQuota:           policy.CacheByteQuota, CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
	}
}

func (input registryPolicyRequest) serviceInput() registry.ServicePolicyInput {
	return registry.ServicePolicyInput{
		Repository: input.Repository, KeepLastSuccessful: input.KeepLastSuccessful,
		MinimumSafetyAgeSeconds:  input.MinimumSafetyAgeSeconds,
		CacheKeepGenerations:     input.CacheKeepGenerations,
		CacheUnusedExpirySeconds: input.CacheUnusedExpirySeconds,
		CacheByteQuota:           input.CacheByteQuota,
	}
}

type registryInventoryObservationView struct {
	Revision              string    `json:"revision"`
	Complete              bool      `json:"complete"`
	Repositories          []string  `json:"repositories"`
	RepositoriesTruncated bool      `json:"repositoriesTruncated"`
	ObservedAt            time.Time `json:"observedAt"`
}

type registryCatalogObservationView struct {
	Repository    string    `json:"repository"`
	Revision      int64     `json:"revision"`
	Complete      bool      `json:"complete"`
	ObservedAt    time.Time `json:"observedAt"`
	ManifestCount int       `json:"manifestCount"`
	BlobCount     int       `json:"blobCount"`
}

type registryReleaseView struct {
	ID                     string                              `json:"id"`
	Repository             string                              `json:"repository"`
	RootDigest             string                              `json:"rootDigest"`
	CreatedAt              time.Time                           `json:"createdAt"`
	SucceededAt            *time.Time                          `json:"succeededAt,omitempty"`
	Availability           domain.RegistryArtifactAvailability `json:"availability"`
	AvailabilityObservedAt *time.Time                          `json:"availabilityObservedAt,omitempty"`
}

type registryCacheGenerationView struct {
	ID                  string     `json:"id"`
	Repository          string     `json:"repository"`
	PlatformSet         string     `json:"platformSet"`
	TrustLane           string     `json:"trustLane"`
	CacheSchema         string     `json:"cacheSchema"`
	BuildDefinitionHash string     `json:"buildDefinitionHash"`
	Generation          int64      `json:"generation"`
	RootDigest          string     `json:"rootDigest"`
	SizeBytes           int64      `json:"sizeBytes"`
	State               string     `json:"state"`
	ActiveImports       int        `json:"activeImports"`
	ActiveExports       int        `json:"activeExports"`
	CreatedAt           time.Time  `json:"createdAt"`
	CompletedAt         *time.Time `json:"completedAt,omitempty"`
	LastUsedAt          time.Time  `json:"lastUsedAt"`
}

type registryApplicationTargetView struct {
	Target                    registryTargetView                `json:"target"`
	Policy                    registryPolicyView                `json:"policy"`
	Inventory                 *registryInventoryObservationView `json:"inventory,omitempty"`
	CatalogObservations       []registryCatalogObservationView  `json:"catalogObservations"`
	CatalogTruncated          bool                              `json:"catalogTruncated"`
	Releases                  []registryReleaseView             `json:"releases"`
	ReleasesTruncated         bool                              `json:"releasesTruncated"`
	CacheGenerations          []registryCacheGenerationView     `json:"cacheGenerations"`
	CacheGenerationsTruncated bool                              `json:"cacheGenerationsTruncated"`
	ObservedAt                time.Time                         `json:"observedAt"`
}

func safeRegistryApplicationTarget(snapshot domain.RegistryLifecycleSnapshot, limit int) registryApplicationTargetView {
	view := registryApplicationTargetView{
		Target: safeRegistryTarget(snapshot.Target), Policy: safeRegistryPolicy(snapshot.Policy),
		CatalogObservations: make([]registryCatalogObservationView, 0), Releases: make([]registryReleaseView, 0),
		CacheGenerations: make([]registryCacheGenerationView, 0), ObservedAt: snapshot.AsOf,
	}
	if snapshot.Inventory.RegistryTargetID != "" {
		repositories := append([]string{}, snapshot.Inventory.Repositories...)
		sort.Strings(repositories)
		repositoriesTruncated := len(repositories) > limit
		if repositoriesTruncated {
			repositories = repositories[:limit]
		}
		view.Inventory = &registryInventoryObservationView{
			Revision: snapshot.Inventory.Revision, Complete: snapshot.Inventory.Complete,
			Repositories: repositories, RepositoriesTruncated: repositoriesTruncated,
			ObservedAt: snapshot.Inventory.ObservedAt,
		}
	}
	catalogs := append([]domain.RegistryCatalogObservation{}, snapshot.CatalogObservations...)
	sort.Slice(catalogs, func(i, j int) bool { return catalogs[i].Repository < catalogs[j].Repository })
	view.CatalogTruncated = len(catalogs) > limit
	if view.CatalogTruncated {
		catalogs = catalogs[:limit]
	}
	for _, observation := range catalogs {
		view.CatalogObservations = append(view.CatalogObservations, registryCatalogObservationView{
			Repository: observation.Repository, Revision: observation.Revision, Complete: observation.Complete,
			ObservedAt: observation.ObservedAt, ManifestCount: observation.ManifestCount, BlobCount: observation.BlobCount,
		})
	}
	releases := append([]domain.RegistryRelease{}, snapshot.Releases...)
	sort.Slice(releases, func(i, j int) bool {
		left, right := releases[i].CreatedAt, releases[j].CreatedAt
		if releases[i].SucceededAt != nil {
			left = *releases[i].SucceededAt
		}
		if releases[j].SucceededAt != nil {
			right = *releases[j].SucceededAt
		}
		if !left.Equal(right) {
			return left.After(right)
		}
		return releases[i].ID < releases[j].ID
	})
	view.ReleasesTruncated = len(releases) > limit
	if view.ReleasesTruncated {
		releases = releases[:limit]
	}
	for _, release := range releases {
		view.Releases = append(view.Releases, registryReleaseView{
			ID: release.ID, Repository: release.Repository, RootDigest: release.RootDigest,
			CreatedAt: release.CreatedAt, SucceededAt: release.SucceededAt, Availability: release.Availability,
			AvailabilityObservedAt: release.AvailabilityObservedAt,
		})
	}
	caches := append([]domain.RegistryCacheGeneration{}, snapshot.CacheGenerations...)
	sort.Slice(caches, func(i, j int) bool {
		if !caches[i].LastUsedAt.Equal(caches[j].LastUsedAt) {
			return caches[i].LastUsedAt.After(caches[j].LastUsedAt)
		}
		if caches[i].Generation != caches[j].Generation {
			return caches[i].Generation > caches[j].Generation
		}
		return caches[i].ID < caches[j].ID
	})
	view.CacheGenerationsTruncated = len(caches) > limit
	if view.CacheGenerationsTruncated {
		caches = caches[:limit]
	}
	for _, cache := range caches {
		view.CacheGenerations = append(view.CacheGenerations, registryCacheGenerationView{
			ID: cache.ID, Repository: cache.Repository, PlatformSet: cache.PlatformSet,
			TrustLane: cache.TrustLane, CacheSchema: cache.CacheSchema, BuildDefinitionHash: cache.BuildDefinitionHash,
			Generation: cache.Generation, RootDigest: cache.RootDigest, SizeBytes: cache.SizeBytes,
			State: cache.State, ActiveImports: cache.ActiveImports, ActiveExports: cache.ActiveExports,
			CreatedAt: cache.CreatedAt, CompletedAt: cache.CompletedAt, LastUsedAt: cache.LastUsedAt,
		})
	}
	return view
}

func (h *registryHTTP) applicationInventory(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if h == nil || h.service == nil {
		registryManagementUnavailable(w, r)
		return
	}
	limit, ok := registryInventoryLimit(w, r)
	if !ok {
		return
	}
	snapshots, err := h.service.ApplicationInventory(r.Context(), currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	// External targets are metadata and push/pull/cache authority only. They do
	// not depend on, and must not be hidden by, the managed Distribution
	// observer/GC worker. Conversely, never serve a managed snapshot while its
	// exact worker readiness is stale: callers could otherwise mistake old
	// inventory and protection observations for current cleanup evidence.
	if !h.runtimeReady(r.Context()) {
		for _, snapshot := range snapshots {
			if snapshot.Target.Mode == domain.RegistryTargetManaged {
				managedRegistryRuntimeUnavailable(w, r)
				return
			}
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Target.ID < snapshots[j].Target.ID })
	truncated := len(snapshots) > limit
	if truncated {
		snapshots = snapshots[:limit]
	}
	items := make([]registryApplicationTargetView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		items = append(items, safeRegistryApplicationTarget(snapshot, limit))
	}
	writeJSON(w, http.StatusOK, struct {
		Items     []registryApplicationTargetView `json:"items"`
		Truncated bool                            `json:"truncated"`
	}{Items: items, Truncated: truncated})
}

func (h *registryHTTP) putPolicy(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if h == nil || h.service == nil {
		registryManagementUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input registryPolicyRequest
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.PutPolicy(r.Context(), currentUser(r.Context()).ID, key, fingerprint(input), requestID(r.Context()), strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("targetId")), input.serviceInput())
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeRegistryPolicy(result.Value))
}

type registryCleanupPreviewRequest struct {
	TargetID string `json:"targetId"`
}

func (h *registryHTTP) previewCleanup(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if !h.runtimeReady(r.Context()) {
		managedRegistryRuntimeUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input registryCleanupPreviewRequest
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.PreviewCleanup(r.Context(), currentUser(r.Context()).ID, key, fingerprint(input), requestID(r.Context()), strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(input.TargetID))
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/registry-cleanup-plans/"+result.Value.ID)
	writeJSON(w, http.StatusCreated, safeRegistryCleanupPlan(result.Value))
}

type registryCleanupSummaryView struct {
	ProtectedManifests  int   `json:"protectedManifests"`
	DeletedManifests    int   `json:"deletedManifests"`
	GarbageCollectBlobs int   `json:"garbageCollectBlobs"`
	EstimatedBytes      int64 `json:"estimatedBytes"`
	CacheBytesBefore    int64 `json:"cacheBytesBefore"`
	CacheBytesAfter     int64 `json:"cacheBytesAfter"`
	CacheQuotaSatisfied bool  `json:"cacheQuotaSatisfied"`
}

type registryCleanupItemView struct {
	Ordinal         int                               `json:"ordinal"`
	Repository      string                            `json:"repository"`
	ResourceKind    string                            `json:"resourceKind"`
	Digest          string                            `json:"digest"`
	Disposition     domain.RegistryCleanupDisposition `json:"disposition"`
	Action          string                            `json:"action"`
	EstimatedBytes  int64                             `json:"estimatedBytes"`
	Reasons         []string                          `json:"reasons"`
	State           string                            `json:"state"`
	ProviderMessage string                            `json:"providerMessage,omitempty"`
	UpdatedAt       time.Time                         `json:"updatedAt"`
}

type registryCleanupPlanView struct {
	ID               string                     `json:"id"`
	RegistryTargetID string                     `json:"registryTargetId"`
	ServiceID        string                     `json:"serviceId"`
	PlanDigest       string                     `json:"planDigest"`
	State            string                     `json:"state"`
	Policy           registryPolicyView         `json:"policy"`
	Summary          registryCleanupSummaryView `json:"summary"`
	Items            []registryCleanupItemView  `json:"items"`
	ItemsTruncated   bool                       `json:"itemsTruncated"`
	CreatedAt        time.Time                  `json:"createdAt"`
	ClaimedAt        *time.Time                 `json:"claimedAt,omitempty"`
	CompletedAt      *time.Time                 `json:"completedAt,omitempty"`
	Failure          string                     `json:"failure,omitempty"`
}

func safeRegistryCleanupPlan(plan domain.RegistryCleanupPlan) registryCleanupPlanView {
	planItems := append([]domain.RegistryCleanupItem{}, plan.Items...)
	sort.Slice(planItems, func(i, j int) bool { return planItems[i].Ordinal < planItems[j].Ordinal })
	itemsTruncated := len(planItems) > maximumRegistryCleanupItems
	if itemsTruncated {
		planItems = planItems[:maximumRegistryCleanupItems]
	}
	items := make([]registryCleanupItemView, 0, len(planItems))
	for _, item := range planItems {
		items = append(items, registryCleanupItemView{
			Ordinal: item.Ordinal, Repository: item.Repository, ResourceKind: item.ResourceKind,
			Digest: item.Digest, Disposition: item.Disposition, Action: item.Action,
			EstimatedBytes: item.EstimatedBytes, Reasons: append([]string{}, item.Reasons...),
			State: item.State, ProviderMessage: item.ProviderMessage, UpdatedAt: item.UpdatedAt,
		})
	}
	return registryCleanupPlanView{
		ID: plan.ID, RegistryTargetID: plan.RegistryTargetID, ServiceID: plan.ServiceID,
		PlanDigest: plan.PlanDigest, State: plan.State, Policy: safeRegistryPolicy(plan.Policy),
		Summary: registryCleanupSummaryView{
			ProtectedManifests: plan.Summary.ProtectedManifests, DeletedManifests: plan.Summary.DeletedManifests,
			GarbageCollectBlobs: plan.Summary.GarbageCollectBlobs, EstimatedBytes: plan.Summary.EstimatedBytes,
			CacheBytesBefore: plan.Summary.CacheBytesBefore, CacheBytesAfter: plan.Summary.CacheBytesAfter,
			CacheQuotaSatisfied: plan.Summary.CacheQuotaSatisfied,
		},
		Items: items, ItemsTruncated: itemsTruncated, CreatedAt: plan.CreatedAt, ClaimedAt: plan.ClaimedAt,
		CompletedAt: plan.CompletedAt, Failure: plan.Failure,
	}
}

func (h *registryHTTP) cleanupPlan(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if h == nil || h.service == nil {
		registryManagementUnavailable(w, r)
		return
	}
	plan, err := h.service.CleanupPlan(r.Context(), currentUser(r.Context()).ID, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, safeRegistryCleanupPlan(plan))
}

type registryCleanupExecutionRequest struct {
	Confirmation string `json:"confirmation"`
}

func (h *registryHTTP) executeCleanup(w http.ResponseWriter, r *http.Request) {
	registryResponseHeaders(w)
	if !h.runtimeReady(r.Context()) {
		managedRegistryRuntimeUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input registryCleanupExecutionRequest
	if !decode(w, r, &input) {
		return
	}
	actor := currentUser(r.Context()).ID
	planID := strings.TrimSpace(r.PathValue("id"))
	plan, err := h.service.CleanupPlan(r.Context(), actor, planID)
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	result, err := h.service.ExecuteCleanup(r.Context(), actor, key, fingerprint(input), requestID(r.Context()), plan.ServiceID, planID, input.Confirmation)
	if err != nil {
		mappedRegistryError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, safeRegistryCleanupPlan(result.Value))
}

func registryInventoryLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultRegistryInventoryLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximumRegistryInventoryLimit {
		writeProblem(w, r, http.StatusBadRequest, "InvalidRegistryLimit", "Invalid registry limit", "limit must be an integer between 1 and 100.")
		return 0, false
	}
	return limit, true
}

func registryManagementUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "RegistryUnavailable", "Registry management unavailable", "Registry target and policy management is not configured on this API replica.")
}

func managedRegistryRuntimeUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "RegistryUnavailable", "Managed registry runtime unavailable", "No matching managed-registry worker has reported a fresh observer and cleanup-executor runtime observation.")
}

func mappedRegistryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrRegistryExternalLifecycle):
		writeProblem(w, r, http.StatusConflict, "RegistryExternalLifecycle", "Registry lifecycle is operator managed", "External registry targets expose metadata only. Kuberploy never previews or executes deletion or garbage collection for them.")
	case errors.Is(err, store.ErrRegistryObservationIncomplete):
		writeProblem(w, r, http.StatusConflict, "RegistryObservationIncomplete", "Registry observations are incomplete", "Cleanup remains disabled until inventory, catalog, Git, runtime, and operation observations are complete.")
	case errors.Is(err, store.ErrRegistrySnapshotStale):
		writeProblem(w, r, http.StatusConflict, "RegistrySnapshotStale", "Registry snapshot changed", "The cleanup preview is stale. Create a new preview before executing cleanup.")
	case errors.Is(err, store.ErrRegistryGraphInvalid):
		writeProblem(w, r, http.StatusConflict, "RegistryGraphInvalid", "Registry catalog is invalid", "Cleanup remains disabled because the observed OCI graph is not valid.")
	case errors.Is(err, store.ErrRegistryLeaseLost):
		writeProblem(w, r, http.StatusConflict, "RegistryCleanupLeaseLost", "Registry cleanup lease lost", "The cleanup execution no longer owns every required repository lease.")
	case errors.Is(err, store.ErrRegistryPolicyInvalid), errors.Is(err, registry.ErrRegistryManagementInvalid):
		writeProblem(w, r, http.StatusBadRequest, "RegistryPolicyInvalid", "Registry input is invalid", "Provide a valid target, repository, retention window, and cache policy.")
	case errors.Is(err, registry.ErrRegistryConfirmationInvalid):
		writeProblem(w, r, http.StatusBadRequest, "RegistryConfirmationInvalid", "Cleanup confirmation does not match", "Enter the exact cleanup plan ID to execute this managed-only cleanup.")
	case errors.Is(err, registry.ErrRegistryCleanupUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "RegistryCleanupUnavailable", "Managed registry cleanup unavailable", "No managed registry cleanup executor is configured.")
	case errors.Is(err, registry.ErrRegistryExecutionInvalid):
		writeProblem(w, r, http.StatusConflict, "RegistryExecutionInvalid", "Registry cleanup execution is invalid", "The managed cleanup executor rejected this plan.")
	default:
		mappedError(w, r, err)
	}
}
