package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/store"
)

var (
	ErrRegistryManagementInvalid   = errors.New("registry management input is invalid")
	ErrRegistryCleanupUnavailable  = errors.New("managed registry cleanup executor is unavailable")
	ErrRegistryConfirmationInvalid = errors.New("registry cleanup confirmation does not match the plan")
)

type ManagementStore interface {
	ListRegistryTargetsForActor(context.Context, string) ([]domain.RegistryTarget, error)
	CreateRegistryTargetForActor(context.Context, string, string, string, string, domain.RegistryTarget) (store.Result[domain.RegistryTarget], error)
	UpdateRegistryTargetForActor(context.Context, string, string, string, string, domain.RegistryTarget) (store.Result[domain.RegistryTarget], error)
	RegistryLifecycleSnapshotsForActor(context.Context, string, string, time.Time) ([]domain.RegistryLifecycleSnapshot, error)
	PutServiceRegistryPolicyForActor(context.Context, string, string, string, string, string, domain.ServiceRegistryPolicy) (store.Result[domain.ServiceRegistryPolicy], error)
	SaveRegistryCleanupPreviewForActor(context.Context, string, string, string, string, string, domain.RegistryCleanupPlan) (store.Result[domain.RegistryCleanupPlan], error)
	RegistryCleanupPlanForActor(context.Context, string, string) (domain.RegistryCleanupPlan, error)
	PrepareRegistryCleanupExecutionForActor(context.Context, string, string, string, string, string, string) (store.Result[domain.RegistryCleanupPlan], error)
}

type CleanupPlanExecutor interface {
	Execute(context.Context, string, string) error
}

type Management struct {
	store             ManagementStore
	executor          CleanupPlanExecutor
	protection        ProtectionRefresher
	now               func() time.Time
	newID             func() string
	managedTarget     *domain.RegistryTarget
	maxObservationAge time.Duration
}

type ManagementOption func(*Management)

func WithManagementClock(now func() time.Time) ManagementOption {
	return func(service *Management) { service.now = now }
}

func WithManagementIDGenerator(newID func() string) ManagementOption {
	return func(service *Management) { service.newID = newID }
}

// WithManagedTargetID binds the single operator-managed registry profile to
// the exact durable target identity supplied by runtime configuration. Other
// (external) targets continue to receive independently generated identities.
func WithManagedTarget(target domain.RegistryTarget) ManagementOption {
	return func(service *Management) {
		copy := target
		service.managedTarget = &copy
	}
}

func WithManagementObservationAge(max time.Duration) ManagementOption {
	return func(service *Management) { service.maxObservationAge = max }
}

func WithManagementProtectionRefresher(refresher ProtectionRefresher) ManagementOption {
	return func(service *Management) { service.protection = refresher }
}

func NewManagement(repository ManagementStore, executor CleanupPlanExecutor, options ...ManagementOption) *Management {
	service := &Management{
		store:             repository,
		executor:          executor,
		now:               func() time.Time { return time.Now().UTC() },
		newID:             id.New,
		maxObservationAge: 15 * time.Minute,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

type RegistryTargetInput struct {
	Name               string
	Mode               domain.RegistryTargetMode
	Endpoint           string
	RepositoryPrefix   string
	PullCredentialRef  string
	PushCredentialRef  string
	CacheCredentialRef string
}

func (s *Management) Targets(ctx context.Context, actor string) ([]domain.RegistryTarget, error) {
	if s == nil || s.store == nil {
		return nil, ErrRegistryManagementInvalid
	}
	return s.store.ListRegistryTargetsForActor(ctx, actor)
}

func (s *Management) CreateTarget(ctx context.Context, actor, key, fingerprint, requestID string, input RegistryTargetInput) (store.Result[domain.RegistryTarget], error) {
	if s == nil || s.store == nil || s.newID == nil {
		return store.Result[domain.RegistryTarget]{}, ErrRegistryManagementInvalid
	}
	targetID := s.newID()
	if input.Mode == domain.RegistryTargetManaged && s.managedTarget != nil {
		targetID = s.managedTarget.ID
	}
	target := registryTargetFromInput(targetID, input)
	if s.managedTarget != nil && target.Mode == domain.RegistryTargetManaged && target != *s.managedTarget {
		return store.Result[domain.RegistryTarget]{}, store.ErrConflict
	}
	if err := ValidateTarget(target); err != nil {
		return store.Result[domain.RegistryTarget]{}, err
	}
	return s.store.CreateRegistryTargetForActor(ctx, actor, key, fingerprint, requestID, target)
}

func (s *Management) UpdateTarget(ctx context.Context, actor, key, fingerprint, requestID, targetID string, input RegistryTargetInput) (store.Result[domain.RegistryTarget], error) {
	if s == nil || s.store == nil {
		return store.Result[domain.RegistryTarget]{}, ErrRegistryManagementInvalid
	}
	target := registryTargetFromInput(strings.TrimSpace(targetID), input)
	if s.managedTarget != nil && target.ID == s.managedTarget.ID && target != *s.managedTarget {
		return store.Result[domain.RegistryTarget]{}, store.ErrConflict
	}
	if err := ValidateTarget(target); err != nil {
		return store.Result[domain.RegistryTarget]{}, err
	}
	return s.store.UpdateRegistryTargetForActor(ctx, actor, key, fingerprint, requestID, target)
}

func registryTargetFromInput(targetID string, input RegistryTargetInput) domain.RegistryTarget {
	return domain.RegistryTarget{
		ID: targetID, Name: strings.TrimSpace(input.Name), Mode: input.Mode,
		Endpoint: strings.TrimSpace(input.Endpoint), RepositoryPrefix: strings.TrimSpace(input.RepositoryPrefix),
		PullCredentialRef: strings.TrimSpace(input.PullCredentialRef), PushCredentialRef: strings.TrimSpace(input.PushCredentialRef),
		CacheCredentialRef: strings.TrimSpace(input.CacheCredentialRef),
	}
}

func (s *Management) ApplicationInventory(ctx context.Context, actor, applicationID string) ([]domain.RegistryLifecycleSnapshot, error) {
	if s == nil || s.store == nil || s.now == nil || strings.TrimSpace(applicationID) == "" {
		return nil, ErrRegistryManagementInvalid
	}
	return s.store.RegistryLifecycleSnapshotsForActor(ctx, actor, strings.TrimSpace(applicationID), s.now().UTC())
}

type ServicePolicyInput struct {
	Repository               string
	KeepLastSuccessful       int
	MinimumSafetyAgeSeconds  int64
	CacheUnusedExpirySeconds int64
	CacheByteQuota           int64
}

func (s *Management) PutPolicy(ctx context.Context, actor, key, fingerprint, requestID, applicationID, targetID string, input ServicePolicyInput) (store.Result[domain.ServiceRegistryPolicy], error) {
	if s == nil || s.store == nil || strings.TrimSpace(applicationID) == "" || strings.TrimSpace(targetID) == "" {
		return store.Result[domain.ServiceRegistryPolicy]{}, ErrRegistryManagementInvalid
	}
	minimumSafetyAge, ok := secondsDuration(input.MinimumSafetyAgeSeconds)
	if !ok {
		return store.Result[domain.ServiceRegistryPolicy]{}, ErrRegistryManagementInvalid
	}
	cacheUnusedExpiry, ok := secondsDuration(input.CacheUnusedExpirySeconds)
	if !ok {
		return store.Result[domain.ServiceRegistryPolicy]{}, ErrRegistryManagementInvalid
	}
	applicationID = strings.TrimSpace(applicationID)
	policy := domain.ServiceRegistryPolicy{
		RegistryTargetID: strings.TrimSpace(targetID), ServiceID: applicationID,
		Repository: strings.TrimSpace(input.Repository), KeepLastSuccessful: input.KeepLastSuccessful,
		MinimumSafetyAge:  minimumSafetyAge,
		CacheUnusedExpiry: cacheUnusedExpiry, CacheByteQuota: input.CacheByteQuota,
	}
	return s.store.PutServiceRegistryPolicyForActor(ctx, actor, key, fingerprint, requestID, applicationID, policy)
}

func secondsDuration(seconds int64) (time.Duration, bool) {
	const maximumSeconds = int64(^uint64(0)>>1) / int64(time.Second)
	if seconds < 0 || seconds > maximumSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func (s *Management) PreviewCleanup(ctx context.Context, actor, key, fingerprint, requestID, applicationID, targetID string) (store.Result[domain.RegistryCleanupPlan], error) {
	if s == nil || s.store == nil || s.now == nil || s.newID == nil {
		return store.Result[domain.RegistryCleanupPlan]{}, ErrRegistryManagementInvalid
	}
	applicationID = strings.TrimSpace(applicationID)
	targetID = strings.TrimSpace(targetID)
	now := s.now().UTC()
	snapshots, err := s.store.RegistryLifecycleSnapshotsForActor(ctx, actor, applicationID, now)
	if err != nil {
		return store.Result[domain.RegistryCleanupPlan]{}, err
	}
	var selected *domain.RegistryLifecycleSnapshot
	for index := range snapshots {
		if snapshots[index].Target.ID == targetID {
			selected = &snapshots[index]
			break
		}
	}
	if selected == nil {
		return store.Result[domain.RegistryCleanupPlan]{}, store.ErrNotFound
	}
	if selected.Target.Mode != domain.RegistryTargetManaged {
		return store.Result[domain.RegistryCleanupPlan]{}, store.ErrRegistryExternalLifecycle
	}
	if s.protection != nil {
		if err = s.protection.RefreshRegistryProtection(ctx, targetID, applicationID, now, true); err != nil {
			return store.Result[domain.RegistryCleanupPlan]{}, err
		}
		// Reload after the exact source-derived checkpoint is durable. This also
		// reauthorizes the caller and prevents constructing a plan from the stale
		// pre-refresh view.
		snapshots, err = s.store.RegistryLifecycleSnapshotsForActor(ctx, actor, applicationID, now)
		if err != nil {
			return store.Result[domain.RegistryCleanupPlan]{}, err
		}
		selected = nil
		for index := range snapshots {
			if snapshots[index].Target.ID == targetID {
				selected = &snapshots[index]
				break
			}
		}
		if selected == nil || selected.Target.Mode != domain.RegistryTargetManaged {
			return store.Result[domain.RegistryCleanupPlan]{}, store.ErrNotFound
		}
	}
	plan, err := BuildCleanupPlan(*selected, now, s.maxObservationAge)
	if err != nil {
		return store.Result[domain.RegistryCleanupPlan]{}, err
	}
	plan.ID = s.newID()
	return s.store.SaveRegistryCleanupPreviewForActor(ctx, actor, key, fingerprint, requestID, applicationID, plan)
}

func (s *Management) CleanupPlan(ctx context.Context, actor, planID string) (domain.RegistryCleanupPlan, error) {
	if s == nil || s.store == nil || strings.TrimSpace(planID) == "" {
		return domain.RegistryCleanupPlan{}, ErrRegistryManagementInvalid
	}
	return s.store.RegistryCleanupPlanForActor(ctx, actor, strings.TrimSpace(planID))
}

func (s *Management) ExecuteCleanup(ctx context.Context, actor, key, fingerprint, requestID, applicationID, planID, confirmation string) (store.Result[domain.RegistryCleanupPlan], error) {
	if s == nil || s.store == nil {
		return store.Result[domain.RegistryCleanupPlan]{}, ErrRegistryManagementInvalid
	}
	if s.executor == nil {
		return store.Result[domain.RegistryCleanupPlan]{}, ErrRegistryCleanupUnavailable
	}
	applicationID = strings.TrimSpace(applicationID)
	planID = strings.TrimSpace(planID)
	if strings.TrimSpace(confirmation) != planID || planID == "" {
		return store.Result[domain.RegistryCleanupPlan]{}, ErrRegistryConfirmationInvalid
	}
	prepared, err := s.store.PrepareRegistryCleanupExecutionForActor(ctx, actor, key, fingerprint, requestID, applicationID, confirmation)
	if err != nil {
		return store.Result[domain.RegistryCleanupPlan]{}, err
	}
	if prepared.Value.ID != planID || prepared.Value.ServiceID != applicationID {
		return store.Result[domain.RegistryCleanupPlan]{}, ErrRegistryManagementInvalid
	}
	if prepared.Value.State == "preview" || prepared.Value.State == "executing" || store.RegistryCleanupPlanCanResumeOfflineSweep(prepared.Value) {
		if err = s.executor.Execute(ctx, planID, cleanupExecutionOwner(actor, key)); err != nil {
			current, loadErr := s.store.RegistryCleanupPlanForActor(ctx, actor, planID)
			if loadErr == nil {
				prepared.Value = current
			}
			return prepared, err
		}
	}
	current, err := s.store.RegistryCleanupPlanForActor(ctx, actor, planID)
	if err != nil {
		return store.Result[domain.RegistryCleanupPlan]{}, err
	}
	prepared.Value = current
	return prepared, nil
}

func cleanupExecutionOwner(actor, key string) string {
	sum := sha256.Sum256([]byte(actor + "\x00" + key))
	return "api-" + hex.EncodeToString(sum[:16])
}
